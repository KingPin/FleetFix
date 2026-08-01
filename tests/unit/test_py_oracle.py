"""Guards on the reference oracle, ``tools/oracle/py_oracle.py``.

The oracle is what the Go port is measured against, so a fault in it does not
show up as a failure -- it shows up as the harness reporting equivalence on a
comparison it never actually made. Three things have to hold: every function the
manifest names is dispatchable, every case produces a record, and two runs of the
same corpus produce the same bytes.
"""

from __future__ import annotations

import importlib.util
import json
from pathlib import Path
from types import ModuleType

import pytest
import yaml

REPO_ROOT = Path(__file__).resolve().parents[2]
ORACLE_PATH = REPO_ROOT / "tools" / "oracle" / "py_oracle.py"
KNOWN_DIVERGENCES = yaml.safe_load(
    (REPO_ROOT / "differential" / "known_divergences.yaml").read_text(encoding="utf-8")
)


def load_oracle() -> ModuleType:
    """Import the oracle by path -- ``tools/`` is deliberately not a package.

    It is a repo script, not part of the shipped library, and making it importable
    the normal way would put it on the same footing as ``src/fleetfix``.
    """
    spec = importlib.util.spec_from_file_location("py_oracle", ORACLE_PATH)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


ORACLE = load_oracle()
CASES = ORACLE.load_cases()


def test_every_manifest_function_is_dispatchable() -> None:
    """A case naming an unknown function is silently skipped by the comparison.

    The oracle records it as an UnknownFunction error rather than crashing, so
    without this test a typo in ``fn`` costs a fixture its coverage and nothing
    goes red.
    """
    named = {str(c["fn"]) for c in CASES}
    assert not (named - set(ORACLE.DISPATCH)), (
        f"undispatchable: {sorted(named - set(ORACLE.DISPATCH))}"
    )


def test_no_dispatch_entry_is_unused() -> None:
    """The other direction: an adapter no case reaches is untested plumbing."""
    named = {str(c["fn"]) for c in CASES}
    assert not (set(ORACLE.DISPATCH) - named), f"unused: {sorted(set(ORACLE.DISPATCH) - named)}"


def test_run_produces_one_record_per_case_and_only_documented_errors(tmp_path: Path) -> None:
    """An error record is a claim about v1, so it has to be one somebody made.

    The oracle turns an exception into ``{"error": {"code": ...}}`` rather than
    crashing, which is what lets a case where v1 raises and Go answers compare as a
    divergence instead of vanishing. That same swallow would hide a broken adapter,
    so the allowance is exactly the ids ``known_divergences.yaml`` justifies -- a
    new error either has an argued entry beside it or turns this red.
    """
    out = tmp_path / "py.jsonl"
    assert ORACLE.main(["--out", str(out)]) == 0
    records = [json.loads(line) for line in out.read_text(encoding="utf-8").splitlines()]
    assert [r["id"] for r in records] == [c["id"] for c in CASES]
    failed = {r["id"]: r["error"] for r in records if "error" in r}
    known = {str(d["id"]) for d in (KNOWN_DIVERGENCES.get("divergences") or [])}
    assert not (set(failed) - known), (
        f"cases the oracle could not run, with no divergence entry: "
        f"{ {k: v for k, v in failed.items() if k not in known} }"
    )


def test_two_runs_of_the_same_corpus_are_byte_identical(tmp_path: Path) -> None:
    first, second = tmp_path / "a.jsonl", tmp_path / "b.jsonl"
    ORACLE.main(["--out", str(first)])
    ORACLE.main(["--out", str(second)])
    assert first.read_bytes() == second.read_bytes()


def test_an_input_path_never_leaks_into_a_result(tmp_path: Path) -> None:
    """``EnvCheckResult`` echoes the path it was given, and that path is a temp dir.

    Left alone it would make every run of those cases differ from the last, which
    reads as a divergence in the parser rather than in the harness.
    """
    out = tmp_path / "py.jsonl"
    ORACLE.main(["--out", str(out)])
    body = out.read_text(encoding="utf-8")
    assert "fleetfix-oracle-" not in body
    assert "<input-path>" in body


def test_unknown_case_id_is_an_error_not_an_empty_run() -> None:
    assert ORACLE.main(["--case", "no.such.case", "--out", "/dev/null"]) == 1


@pytest.mark.parametrize(
    ("value", "expected"),
    [
        ((1, 2), [1, 2]),
        ({"b", "a"}, ["a", "b"]),
        (Path("/tmp/x"), "/tmp/x"),
    ],
)
def test_plain_normalises_types_json_cannot_carry(value: object, expected: object) -> None:
    assert ORACLE.plain(value) == expected


def test_plain_refuses_a_type_it_has_no_defined_form_for() -> None:
    """Better a loud failure than a stringified repr that differs across languages."""
    with pytest.raises(TypeError):
        ORACLE.plain(object())
