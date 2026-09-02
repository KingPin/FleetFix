"""Pin packaging.version's ordering so the Go port can be checked against it.

v1's ``updater.checker.is_newer`` compares with ``packaging.version.Version``, which
is PEP 440 and not semver whatever the module docstring says. The Go side reimplements
that ordering, and the only trustworthy statement of what it should produce is the
library itself -- not this file's author's memory of PEP 440, and not a hand-written
table that would be wrong in exactly the places the port is.

Run it by hand after changing the corpus::

    python3 tools/oracle/pin_pep440.py

It writes ``testdata/pep440/corpus.json`` and ``testdata/pep440/pin.json``, both
checked in, and ``TestIsNewerMatchesPackaging`` replays them. This is not the live
differential harness: the harness dies with the Python at M7, and this pin does not.

``pin.json`` holds one row per corpus entry, each a string of "0"/"1" flags: row i,
column j is ``is_newer(corpus[i], corpus[j])``. A full matrix rather than a sorted
order because it also captures ties (1.0, 1.0.0 and 1 are one version) and the
InvalidVersion branch, where every comparison is false in both directions.

Deliberately absent: release components past a 64-bit integer, such as
``99999999999999999999.0``. Python's int is unbounded and orders them; the port
refuses them, answering "not newer" the way it answers for anything else it cannot
order. That is a documented limit rather than a case the pin should assert on, and no
git tag carries a twenty-digit component.
"""

from __future__ import annotations

import json
from pathlib import Path

from packaging.version import InvalidVersion, Version

# Every group is a reason, not a bucket of examples. A version here should be one the
# port could get wrong on its own: a normalization, an implicit number, a separator
# spelling, a rank boundary, or a parse that must fail.
CASES: list[str] = [
    # Release segments, including the trailing zeros that compare equal.
    "1",
    "1.0",
    "1.0.0",
    "1.0.0.0",
    "0.9",
    "1.9.0",
    "1.10.0",
    "2.0.0",
    "10.0.0",
    "1.0.1",
    # The leading v is stripped once by is_newer and again by the pattern, so a
    # doubled one still parses.
    "v1.0.0",
    "vv1.0.0",
    "V1.0.0",
    # Epochs outrank everything to their right.
    "0!1.0",
    "1!1.0",
    "1!0.1",
    "2!0.1",
    # Pre-release spellings that normalize onto three letters, and the implicit 0.
    "1.0a1",
    "1.0a",
    "1.0a0",
    "1.0alpha1",
    "1.0.a1",
    "1.0-a1",
    "1.0_a_1",
    "1.0b2",
    "1.0beta2",
    "1.0c1",
    "1.0rc1",
    "1.0pre1",
    "1.0preview1",
    "1.0A1",
    "1.0RC1",
    # Post releases, including the letter-less `1.0-1` form and rev/r spellings.
    "1.0.post1",
    "1.0post1",
    "1.0-post1",
    "1.0.post",
    "1.0.post0",
    "1.0-1",
    "1.0rev1",
    "1.0r1",
    "1.0.POST1",
    # Dev releases. One with no pre or post sorts before every alpha; one attached to
    # a pre-release sorts just under it.
    "1.0.dev1",
    "1.0.dev",
    "1.0.dev0",
    "1.0dev1",
    "1.0a1.dev1",
    "1.0.post1.dev1",
    "1.0rc1.post2.dev3",
    # Local versions: absent sorts before present, words before numbers, and the
    # three separators are one separator.
    "1.0+ubuntu1",
    "1.0+ubuntu.1",
    "1.0+ubuntu-1",
    "1.0+ubuntu_1",
    "1.0+abc",
    "1.0+abd",
    "1.0+1",
    "1.0+2",
    "1.0+0",
    "1.0+1.abc",
    "1.0+abc.1",
    "1.0+UBUNTU1",
    # Whitespace is stripped by the \s* the pattern is compiled with, and \s on a str
    # pattern is str.isspace() -- so a non-breaking space goes too.
    " 1.0",
    "1.0 ",
    "\t1.0\n",
    "\u00a01.0",
    # Parses that must fail. Every one of these is false against everything, in both
    # directions, which is the branch is_newer takes on InvalidVersion.
    "",
    " ",
    "1..2",
    ".1",
    "1.",
    "abc",
    "v",
    "1.0.0-alpha.1",
    "1.0.0+",
    "1.0.0+ubuntu!",
    "1.0.0rc1rc2",
    "-1.0",
    "1.0.0 1.0.0",
]


def strip_v(tag: str) -> str:
    return tag[1:] if tag.startswith("v") else tag


def is_newer(remote: str, local: str) -> bool:
    """v1's updater.checker.is_newer, copied so the pin does not import the TUI."""
    try:
        return Version(strip_v(remote)) > Version(strip_v(local))
    except InvalidVersion:
        return False


def main() -> None:
    out = Path(__file__).resolve().parents[2] / "testdata" / "pep440"
    out.mkdir(parents=True, exist_ok=True)

    matrix = ["".join("1" if is_newer(a, b) else "0" for b in CASES) for a in CASES]

    (out / "corpus.json").write_text(
        json.dumps(CASES, indent=2, sort_keys=True, ensure_ascii=True) + "\n",
        encoding="utf-8",
    )
    (out / "pin.json").write_text(
        json.dumps({"is_newer": matrix}, indent=2, sort_keys=True, ensure_ascii=True) + "\n",
        encoding="utf-8",
    )
    print(f"wrote {len(CASES)} cases to {out}")


if __name__ == "__main__":
    main()
