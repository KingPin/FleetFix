"""Unit tests for the headless `fleetfix --update` flow."""

from __future__ import annotations

from pathlib import Path

import pytest

from fleetfix.updater.checker import ReleaseInfo
from fleetfix.updater.cli import run_update
from fleetfix.updater.installer import InstallResult


@pytest.fixture(autouse=True)
def _audit_in_tmp(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr("fleetfix.updater.cli.resolve_audit_path", lambda: tmp_path / "audit.log")


def _release() -> ReleaseInfo:
    return ReleaseInfo(
        tag="v2.0.0",
        version="2.0.0",
        asset_url="https://x/asset",
        checksum_url="https://x/sums",
        html_url="https://x/release",
        body="- shiny new thing",
    )


def test_up_to_date_returns_zero_and_does_not_apply() -> None:
    out: list[str] = []
    applied: list[object] = []
    code = run_update(
        force=False,
        out=out.append,
        check=lambda v, **kw: None,
        apply=lambda *a, **kw: (
            applied.append(a) or InstallResult(ok=True, version="", target=Path())
        ),
        current_version="2.0.0",
    )
    assert code == 0
    assert applied == []
    assert any("up to date" in line for line in out)


def test_force_applies_without_prompting() -> None:
    out: list[str] = []
    seen: dict[str, object] = {}

    def fake_apply(release: ReleaseInfo, **kw: object) -> InstallResult:
        seen["target"] = kw.get("target")
        return InstallResult(ok=True, version=release.version, target=Path("/x"))

    code = run_update(
        force=True,
        out=out.append,
        confirm=lambda r: pytest.fail("must not prompt with --force"),
        check=lambda v, **kw: _release(),
        apply=fake_apply,
        current_version="1.0.0",
    )
    assert code == 0
    assert seen["target"] is not None  # explicit target threaded through
    assert any("shiny new thing" in line for line in out)  # notes shown
    assert any("Updated to v2.0.0" in line for line in out)


def test_prompt_decline_does_not_apply_and_returns_nonzero() -> None:
    out: list[str] = []
    code = run_update(
        force=False,
        out=out.append,
        confirm=lambda r: False,
        check=lambda v, **kw: _release(),
        apply=lambda *a, **kw: pytest.fail("declined update must not apply"),
        current_version="1.0.0",
    )
    assert code == 1
    assert any("cancel" in line.lower() for line in out)


def test_failure_propagates_nonzero() -> None:
    out: list[str] = []
    code = run_update(
        force=True,
        out=out.append,
        check=lambda v, **kw: _release(),
        apply=lambda *a, **kw: InstallResult(
            ok=False, version="2.0.0", target=Path("/x"), error="sha256 mismatch"
        ),
        current_version="1.0.0",
    )
    assert code == 1
    assert any("sha256 mismatch" in line for line in out)


def test_check_is_forced_fresh() -> None:
    """An explicit --update must bypass the 1h cache (cache_ttl_s=0)."""
    seen: dict[str, object] = {}

    def fake_check(v: str, **kw: object) -> None:
        seen.update(kw)
        return None

    run_update(force=False, out=lambda s: None, check=fake_check, current_version="2.0.0")
    assert seen.get("cache_ttl_s") == 0
