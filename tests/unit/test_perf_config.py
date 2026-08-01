"""Low-spec / animation config resolution (issue #2 responsiveness knobs)."""

from __future__ import annotations

from pathlib import Path

from fleetfix.config import read_perf_yaml, resolve_reduce_animations
from tests.support.fixtures import fixture


def test_explicit_true_wins_on_multicore() -> None:
    assert resolve_reduce_animations(perf_cfg={"reduce_animations": True}, cpu_count=16) is True


def test_explicit_false_wins_on_single_core() -> None:
    # An operator who explicitly opts in to animations keeps them even on 1 core.
    assert resolve_reduce_animations(perf_cfg={"reduce_animations": False}, cpu_count=1) is False


def test_auto_disables_on_single_core() -> None:
    assert resolve_reduce_animations(perf_cfg={}, cpu_count=1) is True
    assert resolve_reduce_animations(perf_cfg={"reduce_animations": "auto"}, cpu_count=1) is True


def test_auto_keeps_animations_on_multicore() -> None:
    assert resolve_reduce_animations(perf_cfg={}, cpu_count=8) is False


def test_junk_value_falls_back_to_heuristic() -> None:
    assert (
        resolve_reduce_animations(perf_cfg={"reduce_animations": "yes-please"}, cpu_count=1) is True
    )
    assert (
        resolve_reduce_animations(perf_cfg={"reduce_animations": "yes-please"}, cpu_count=4)
        is False
    )


def test_read_perf_yaml_missing_file_is_empty(tmp_path: Path) -> None:
    assert read_perf_yaml(tmp_path / "nope.yml") == {}


def test_read_perf_yaml_parses_mapping(tmp_path: Path) -> None:
    p = tmp_path / "perf.yml"
    p.write_text(fixture("perf/reduce_animations_true.yml"), encoding="utf-8")
    assert read_perf_yaml(p) == {"reduce_animations": True}


def test_read_perf_yaml_malformed_is_empty(tmp_path: Path) -> None:
    p = tmp_path / "perf.yml"
    p.write_text(fixture("perf/malformed.yml"), encoding="utf-8")
    assert read_perf_yaml(p) == {}
