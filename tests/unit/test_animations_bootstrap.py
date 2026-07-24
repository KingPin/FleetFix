"""`_configure_animations` sets TEXTUAL_ANIMATIONS before Textual is imported."""

from __future__ import annotations

import os
from pathlib import Path

import pytest

from fleetfix import __main__, config


@pytest.fixture(autouse=True)
def _isolate_env(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    # Start from a clean slate: no operator override, no perf.yml.
    monkeypatch.delenv("TEXTUAL_ANIMATIONS", raising=False)
    monkeypatch.setattr(config, "PERF_CONFIG_PATH", tmp_path / "perf.yml")


def test_explicit_env_is_never_overwritten(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("TEXTUAL_ANIMATIONS", "full")
    __main__._configure_animations(low_spec=True)  # low_spec would otherwise force none
    assert os.environ["TEXTUAL_ANIMATIONS"] == "full"


def test_low_spec_flag_disables(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(os, "cpu_count", lambda: 8)  # multicore: only the flag forces it
    __main__._configure_animations(low_spec=True)
    assert os.environ["TEXTUAL_ANIMATIONS"] == "none"


def test_auto_single_core_disables(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(os, "cpu_count", lambda: 1)
    __main__._configure_animations(low_spec=False)
    assert os.environ["TEXTUAL_ANIMATIONS"] == "none"


def test_multicore_leaves_default(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(os, "cpu_count", lambda: 8)
    __main__._configure_animations(low_spec=False)
    assert "TEXTUAL_ANIMATIONS" not in os.environ


def test_perf_yaml_opt_in_disables(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    monkeypatch.setattr(os, "cpu_count", lambda: 8)  # multicore
    perf = tmp_path / "perf.yml"
    perf.write_text("reduce_animations: true\n", encoding="utf-8")
    monkeypatch.setattr(config, "PERF_CONFIG_PATH", perf)
    __main__._configure_animations(low_spec=False)
    assert os.environ["TEXTUAL_ANIMATIONS"] == "none"
