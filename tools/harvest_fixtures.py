#!/usr/bin/env python3
"""Lift inline CLI-output fixtures out of the test suite into ``testdata/``.

The Go port verifies itself against this implementation by running both over the
same captured tool output (see ``tools/oracle/py_oracle.py``). Today those
captures live as string literals inside ``tests/``, where only Python can reach
them, so they have to land on disk before either side can share them.

Two phases, deliberately split:

``--report``
    Walk the test suite's AST, collect every multi-line string literal, and
    write a proposal to ``tools/harvest_proposal.json``. Nothing is modified.

``--apply PROPOSAL``
    For each entry marked ``extract``: write the literal to ``testdata/<path>``
    and replace it in the source with a ``fixture("<path>")`` call.

The naming pass in between is manual on purpose. A machine can tell that a
string looks like ``df`` output; only a human can name it after the edge case it
exists to pin down, and those names become shared vocabulary across two test
suites, so they are worth choosing rather than generating.

Byte fidelity is the whole point of the exercise: several tests assert on
``result.raw``, and the differential harness compares parser output across
languages, so a stray newline or stripped trailing space is a silent corruption
of the corpus. Every write is verified by reading the file back and comparing to
the literal it came from, and sources are spliced as bytes rather than text.
"""

from __future__ import annotations

import argparse
import ast
import dataclasses
import hashlib
import json
import re
import subprocess
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
TESTDATA = REPO_ROOT / "testdata"
DEFAULT_PROPOSAL = REPO_ROOT / "tools" / "harvest_proposal.json"
LOADER_IMPORT = "from tests.support.fixtures import fixture"

# Content sniffers, first match wins. These only seed the proposal -- the point
# is to save typing during the manual pass, not to be authoritative.
_SNIFFERS: tuple[tuple[str, re.Pattern[str]], ...] = (
    ("df", re.compile(r"^Filesystem\s+1024-blocks|^Filesystem\s+Inodes")),
    ("traceroute", re.compile(r"^traceroute[ :]")),
    ("tracepath", re.compile(r"^\s*\d+\?*:\s|^tracepath:")),
    ("ping", re.compile(r"^PING |--- .* ping statistics ---")),
    ("ss", re.compile(r"^(Netid|State)\s+")),
    ("smartctl", re.compile(r"smartctl \d|ID# ATTRIBUTE_NAME|SMART overall-health")),
    ("systemctl", re.compile(r"^\s*UNIT\s+LOAD\s+ACTIVE|\.service\s+loaded\s+")),
    ("systemd-analyze", re.compile(r"^Startup finished in |graphical\.target reached")),
    ("journalctl", re.compile(r"^-- (Logs begin|Journal begins)|^\w{3} \d\d \d\d:\d\d:\d\d ")),
    ("curl", re.compile(r"^HTTP/[\d.]+ \d{3}|^\d{3} \d+\.\d+ |^curl: \(\d+\)|FLEETFIX_CURL")),
    ("dig", re.compile(r"^;; |^;.*ANSWER SECTION")),
    ("apt", re.compile(r"^Listing\.\.\.|^Inst \S+ \[")),
    ("docker", re.compile(r"^(CONTAINER ID|REPOSITORY|TYPE)\s+|^Deleted (Images|Containers):")),
    ("proc/meminfo", re.compile(r"^MemTotal:\s+\d+ kB")),
    ("proc/stat", re.compile(r"^cpu\d*\s+\d+ \d+ \d+")),
    ("proc/cpuinfo", re.compile(r"^processor\s*:")),
    ("proc/net", re.compile(r"^\s*sl\s+local_address|^Inter-\|\s+Receive|^Iface\tDestination")),
    ("env", re.compile(r"^[A-Z][A-Z0-9_]*=")),
    ("json", re.compile(r"^\s*[{\[]")),
    # Last, and deliberately narrow: a bare ``key: value`` line is also what half
    # the CLI tools on earth print, so anything above this wins first.
    ("yaml", re.compile(r"^[a-z_][\w-]*:\s*$|^[a-z_][\w-]*:\s+[^(]\S*$")),
)


@dataclasses.dataclass(frozen=True)
class Candidate:
    """One multi-line string literal found in the test suite."""

    file: str
    lineno: int
    col: int
    end_lineno: int
    end_col: int
    #: Assignment target name, if the literal is the value of a simple assignment.
    name: str | None
    #: Enclosing function, if any. ``None`` means module level.
    func: str | None
    text: str

    @property
    def sha256(self) -> str:
        return hashlib.sha256(self.text.encode()).hexdigest()

    @property
    def line_count(self) -> int:
        return self.text.count("\n") + (0 if self.text.endswith("\n") else 1)

    @property
    def preview(self) -> str:
        first = self.text.split("\n", 1)[0]
        return first[:90]


def _enclosing_functions(tree: ast.Module) -> dict[int, str]:
    """Map ``id(node)`` of every descendant to its innermost enclosing function."""
    owner: dict[int, str] = {}

    def walk(node: ast.AST, current: str | None) -> None:
        for child in ast.iter_child_nodes(node):
            name = current
            if isinstance(child, ast.FunctionDef | ast.AsyncFunctionDef):
                name = child.name
            if name is not None:
                owner[id(child)] = name
            walk(child, name)

    walk(tree, None)
    return owner


def _assignment_names(tree: ast.Module) -> dict[int, str]:
    """Map ``id(value_node)`` to the single ``Name`` target it is assigned to."""
    names: dict[int, str] = {}
    for node in ast.walk(tree):
        if isinstance(node, ast.Assign) and len(node.targets) == 1:
            target = node.targets[0]
            if isinstance(target, ast.Name):
                names[id(node.value)] = target.id
        elif isinstance(node, ast.AnnAssign) and isinstance(node.target, ast.Name):
            if node.value is not None:
                names[id(node.value)] = node.target.id
    return names


def _docstring_ids(tree: ast.Module) -> set[int]:
    """``id()`` of every docstring literal -- module, class, and function.

    Function docstrings are indented, so a column check alone misses them and
    they end up proposed as fixtures.
    """
    ids: set[int] = set()
    for node in ast.walk(tree):
        if not isinstance(node, ast.Module | ast.ClassDef | ast.FunctionDef | ast.AsyncFunctionDef):
            continue
        if not node.body:
            continue
        first = node.body[0]
        if isinstance(first, ast.Expr) and isinstance(first.value, ast.Constant):
            if isinstance(first.value.value, str):
                ids.add(id(first.value))
    return ids


def collect(path: Path) -> list[Candidate]:
    """Every multi-line string literal in ``path``, outermost node per literal.

    Adjacent string literals are folded into a single ``Constant`` by the parser,
    so implicit concatenation across lines arrives here as one node spanning the
    whole group -- which is exactly the span we want to replace.
    """
    tree = ast.parse(path.read_bytes().decode())
    funcs = _enclosing_functions(tree)
    names = _assignment_names(tree)
    docstrings = _docstring_ids(tree)
    rel = path.relative_to(REPO_ROOT).as_posix()

    found: list[Candidate] = []
    for node in ast.walk(tree):
        if not isinstance(node, ast.Constant) or not isinstance(node.value, str):
            continue
        if "\n" not in node.value or id(node) in docstrings:
            continue
        if node.end_lineno is None or node.end_col_offset is None:
            continue  # pragma: no cover - always populated for parsed source
        found.append(
            Candidate(
                file=rel,
                lineno=node.lineno,
                col=node.col_offset,
                end_lineno=node.end_lineno,
                end_col=node.end_col_offset,
                name=names.get(id(node)),
                func=funcs.get(id(node)),
                text=node.value,
            )
        )
    found.sort(key=lambda c: (c.lineno, c.col))
    return found


def sniff_tool(text: str) -> str | None:
    """Guess which tool produced ``text``, for the proposed fixture directory."""
    head = text.lstrip("\n")
    for tool, pattern in _SNIFFERS:
        for line in head.split("\n", 8)[:8]:
            if pattern.search(line):
                return tool
    return None


def _case_name(cand: Candidate) -> str:
    raw = cand.name or cand.func or "case"
    raw = raw.lstrip("_").lower()
    raw = re.sub(r"^test_", "", raw)
    return re.sub(r"[^a-z0-9]+", "_", raw).strip("_") or "case"


def propose(cand: Candidate) -> dict[str, object]:
    """Seed one proposal entry: a guessed path and a default action."""
    module = Path(cand.file).stem
    module = re.sub(r"^test_", "", module)
    tool = sniff_tool(cand.text) or module
    case = _case_name(cand)
    # Don't repeat the directory in the basename: ``traceroute/traceroute_reached``
    # reads worse than ``traceroute/reached``.
    leaf = tool.rsplit("/", 1)[-1]
    if case.startswith(f"{leaf}_"):
        case = case[len(leaf) + 1 :]
    # Module-level constants are nearly always captured input. In-function
    # literals are a mix of input and expected values, so they need a look.
    action = "extract" if cand.func is None else "review"
    return {
        "action": action,
        "path": f"{tool}/{case}.txt",
        "file": cand.file,
        "lineno": cand.lineno,
        "col": cand.col,
        "end_lineno": cand.end_lineno,
        "end_col": cand.end_col,
        "name": cand.name,
        "func": cand.func,
        "lines": cand.line_count,
        "sha256": cand.sha256,
        "preview": cand.preview,
    }


def cmd_report(test_root: Path, out: Path) -> int:
    entries: list[dict[str, object]] = []
    for path in sorted(test_root.rglob("test_*.py")):
        entries.extend(propose(cand) for cand in collect(path))

    # Flag collisions rather than silently letting one fixture clobber another.
    seen: dict[str, list[dict[str, object]]] = {}
    for entry in entries:
        seen.setdefault(str(entry["path"]), []).append(entry)
    for path_str, group in seen.items():
        if len(group) > 1:
            for i, entry in enumerate(group, start=1):
                stem, _, ext = path_str.rpartition(".")
                entry["path"] = f"{stem}_{i}.{ext}"
                entry["collision"] = True

    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps({"fixtures": entries}, indent=2) + "\n")

    extract = sum(1 for e in entries if e["action"] == "extract")
    review = len(entries) - extract
    print(f"{len(entries)} candidates ({extract} extract, {review} review) -> {out}")
    print()
    for entry in entries:
        mark = "+" if entry["action"] == "extract" else "?"
        loc = f"{entry['file']}:{entry['lineno']}"
        print(f"{mark} {entry['lines']:>3}L  {entry['path']!s:<46} {loc}")
        print(f"          {entry['preview']!r}")
    return 0


def _splice(src: bytes, spans: list[tuple[int, int, bytes]]) -> bytes:
    """Apply ``(start, end, replacement)`` byte spans, last one first."""
    out = src
    for start, end, replacement in sorted(spans, key=lambda s: s[0], reverse=True):
        out = out[:start] + replacement + out[end:]
    return out


def _git_ignored(paths: list[Path]) -> list[str]:
    """Which of ``paths`` git refuses to track.

    A fixture that lands on disk but is ignored still passes every test locally
    and then simply is not there in anyone else's checkout, which makes the
    corpus non-reproducible in the one way nothing else notices. Generic venv and
    dotfile rules are the usual culprit -- a bare ``env/`` in .gitignore matches
    ``testdata/env/`` too.
    """
    proc = subprocess.run(
        ["git", "check-ignore", "--", *(str(p) for p in paths)],
        cwd=REPO_ROOT,
        capture_output=True,
        text=True,
        check=False,  # exit 1 simply means nothing was ignored
    )
    return [line for line in proc.stdout.splitlines() if line.strip()]


def _constants_by_pos(tree: ast.Module) -> dict[tuple[int, int], ast.Constant]:
    """String literals keyed by ``(lineno, col)`` -- the proposal's identity for them.

    Take the value from the node, never by re-parsing the source slice. A slice of
    a parenthesised implicit concatenation loses its enclosing parentheses, so the
    continuation lines read as an unexpected indent and ``literal_eval`` rejects
    text the file itself parses fine. Reading ``node.value`` also guarantees the
    write path and ``collect`` agree about quoting and escapes, because both are
    looking at the same parse.
    """
    out: dict[tuple[int, int], ast.Constant] = {}
    for node in ast.walk(tree):
        if isinstance(node, ast.Constant) and isinstance(node.value, str):
            out[(node.lineno, node.col_offset)] = node
    return out


def _line_starts(src: bytes) -> list[int]:
    starts = [0]
    for line in src.splitlines(keepends=True):
        starts.append(starts[-1] + len(line))
    return starts


def _offset(starts: list[int], lineno: int, col: int) -> int:
    """AST positions are 1-based lines and *byte* column offsets."""
    return starts[lineno - 1] + col


def _import_insert_line(tree: ast.Module) -> int:
    """1-based line number to insert the loader import *before*."""
    last = 0
    for node in tree.body:
        if isinstance(node, ast.Import | ast.ImportFrom):
            last = max(last, node.end_lineno or node.lineno)
    if last:
        return last + 1
    # No imports at all: land after the module docstring, if there is one.
    if tree.body and isinstance(tree.body[0], ast.Expr):
        first = tree.body[0]
        if isinstance(first.value, ast.Constant) and isinstance(first.value.value, str):
            return (first.end_lineno or first.lineno) + 1
    return 1


def cmd_apply(proposal: Path) -> int:
    data = json.loads(proposal.read_text())
    entries = [e for e in data["fixtures"] if e["action"] == "extract"]
    if not entries:
        print("nothing marked extract; edit the proposal first", file=sys.stderr)
        return 1

    # Two entries may deliberately share a path when the same capture is inlined
    # twice — they dedupe onto one file. Sharing a path with *different* bytes is
    # a naming-pass slip: one silently clobbers the other and a call site ends up
    # reading someone else's fixture.
    by_path: dict[str, str] = {}
    for entry in entries:
        path, sha = str(entry["path"]), str(entry["sha256"])
        if by_path.setdefault(path, sha) != sha:
            print(
                f"{path}: two entries claim this path with different content; "
                "give one of them its own name",
                file=sys.stderr,
            )
            return 1

    by_file: dict[str, list[dict[str, object]]] = {}
    for entry in entries:
        by_file.setdefault(str(entry["file"]), []).append(entry)

    if ignored := _git_ignored([TESTDATA / p for p in by_path]):
        print("git ignores these fixture paths, so they would never be committed:", file=sys.stderr)
        for path in ignored:
            print(f"  {path}", file=sys.stderr)
        print("rename the directory, or add a negation to .gitignore", file=sys.stderr)
        return 1

    written = 0
    for rel, group in sorted(by_file.items()):
        path = REPO_ROOT / rel
        src = path.read_bytes()
        tree = ast.parse(src.decode())
        starts = _line_starts(src)
        spans: list[tuple[int, int, bytes]] = []

        consts = _constants_by_pos(tree)

        for entry in group:
            pos = (int(entry["lineno"]), int(entry["col"]))
            node = consts.get(pos)
            if node is None:
                raise ValueError(f"{rel}:{pos[0]} has no string literal at column {pos[1]}")
            literal = node.value
            start = _offset(starts, node.lineno, node.col_offset)
            end = _offset(starts, node.end_lineno or node.lineno, node.end_col_offset or 0)
            if (node.end_lineno, node.end_col_offset) != (entry["end_lineno"], entry["end_col"]):
                raise ValueError(
                    f"{rel}:{pos[0]} ends somewhere new since the proposal was written; "
                    "re-run report"
                )
            if hashlib.sha256(literal.encode()).hexdigest() != entry["sha256"]:
                raise ValueError(
                    f"{rel}:{entry['lineno']} changed since the proposal was written; "
                    "re-run --report"
                )

            target = TESTDATA / str(entry["path"])
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(literal.encode())
            if target.read_bytes().decode() != literal:
                raise AssertionError(f"round-trip mismatch writing {target}")
            written += 1

            call = f'fixture("{entry["path"]}")'.encode()
            spans.append((start, end, call))

        if LOADER_IMPORT not in src.decode():
            line = _import_insert_line(tree)
            at = starts[line - 1] if line - 1 < len(starts) else len(src)
            spans.append((at, at, f"{LOADER_IMPORT}\n".encode()))

        path.write_bytes(_splice(src, spans))
        print(f"rewrote {rel} ({len(group)} fixture(s))")

    print(f"\n{written} fixture(s) written under {TESTDATA.relative_to(REPO_ROOT)}")
    print("now run: ruff check --fix tests && ruff format tests && pytest")
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n", 1)[0])
    sub = parser.add_subparsers(dest="cmd", required=True)

    rep = sub.add_parser("report", help="collect candidates, write a proposal")
    rep.add_argument("--tests", type=Path, default=REPO_ROOT / "tests")
    rep.add_argument("--out", type=Path, default=DEFAULT_PROPOSAL)

    app = sub.add_parser("apply", help="write fixtures and rewrite sources")
    app.add_argument("proposal", type=Path, nargs="?", default=DEFAULT_PROPOSAL)

    args = parser.parse_args(argv)
    if args.cmd == "report":
        return cmd_report(args.tests, args.out)
    return cmd_apply(args.proposal)


if __name__ == "__main__":
    raise SystemExit(main())
