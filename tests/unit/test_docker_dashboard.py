"""Unit tests for docker.dashboard — parser + restart-loop logic."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone

from fleetfix.modules.docker.dashboard import (
    RESTART_LOOP_THRESHOLD,
    Container,
    _parse_iso,
    parse_inspect_fields,
    parse_ps_json_lines,
)


def test_parse_ps_json_lines_handles_blank_and_garbage_lines() -> None:
    text = (
        '{"ID":"abc","Names":"web","Image":"nginx","State":"running",'
        '"Status":"Up 2 hours","Ports":"80/tcp"}\n'
        "\n"
        "not json\n"
        '{"ID":"def","Names":"db","Image":"pg","State":"exited",'
        '"Status":"Exited (0) 1 hour ago","Ports":""}\n'
    )
    rows = parse_ps_json_lines(text)
    assert len(rows) == 2
    assert rows[0]["ID"] == "abc"
    assert rows[1]["State"] == "exited"


def test_parse_inspect_fields_extracts_all_four() -> None:
    text = "5|/var/lib/docker/containers/abc/abc-json.log|2026-05-16T10:00:00Z|running"
    out = parse_inspect_fields(text)
    assert out["restart_count"] == 5
    assert out["log_path"].endswith("abc-json.log")
    assert out["started_at"] == "2026-05-16T10:00:00Z"
    assert out["status"] == "running"


def test_parse_inspect_fields_handles_malformed() -> None:
    out = parse_inspect_fields("garbage")
    assert out["restart_count"] == 0
    assert out["log_path"] == ""


def test_parse_iso_handles_zero_value() -> None:
    # Docker writes 0001-01-01T00:00:00Z for "never finished"; treat as None.
    assert _parse_iso("0001-01-01T00:00:00Z") is None
    assert _parse_iso(None) is None
    assert _parse_iso("") is None


def test_parse_iso_handles_z_suffix() -> None:
    parsed = _parse_iso("2026-05-16T10:00:00Z")
    assert parsed is not None
    assert parsed.tzinfo is not None


def test_restart_loop_requires_threshold_and_window() -> None:
    now = datetime.now(timezone.utc)
    looping = Container(
        id="a",
        name="flappy",
        image="x",
        state="running",
        status="Up 3 seconds",
        ports="",
        restart_count=RESTART_LOOP_THRESHOLD + 2,
        started_at=now - timedelta(seconds=10),
        log_path="",
    )
    assert looping.is_restart_loop is True

    stable = Container(
        id="b",
        name="stable",
        image="x",
        state="running",
        status="Up 12 hours",
        ports="",
        restart_count=RESTART_LOOP_THRESHOLD + 5,
        started_at=now - timedelta(hours=12),
        log_path="",
    )
    # Started long ago → outside window even though count is high.
    assert stable.is_restart_loop is False

    low_count = Container(
        id="c",
        name="ok",
        image="x",
        state="running",
        status="Up 3 seconds",
        ports="",
        restart_count=RESTART_LOOP_THRESHOLD,
        started_at=now - timedelta(seconds=5),
        log_path="",
    )
    # Equal to threshold is NOT > threshold.
    assert low_count.is_restart_loop is False


def test_restart_loop_needs_started_at() -> None:
    c = Container(
        id="x",
        name="x",
        image="x",
        state="created",
        status="Created",
        ports="",
        restart_count=99,
        started_at=None,
        log_path="",
    )
    assert c.is_restart_loop is False
