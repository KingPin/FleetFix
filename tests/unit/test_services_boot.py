"""Unit tests for services.boot — blame parser + outlier flag."""

from __future__ import annotations

from fleetfix.modules.services.boot import OUTLIER_MS, _parse_time, parse_blame


def test_parse_time_seconds_only() -> None:
    assert _parse_time("59.647s") == int(59.647 * 1000)


def test_parse_time_milliseconds_only() -> None:
    assert _parse_time("559ms") == 559


def test_parse_time_minutes_and_seconds() -> None:
    assert _parse_time("1min 2.234s") == 60_000 + int(2.234 * 1000)


def test_parse_time_minutes_only() -> None:
    assert _parse_time("2min") == 120_000


def test_parse_time_empty_returns_none() -> None:
    assert _parse_time("") is None
    assert _parse_time("nonsense") is None


def test_parse_blame_classic_output() -> None:
    text = (
        "59.647s archlinux-keyring-wkd-sync.service\n"
        " 5.569s NetworkManager-wait-online.service\n"
        "  559ms NetworkManager.service\n"
        "  1min 2.234s long-thing.service\n"
    )
    entries = parse_blame(text)
    assert len(entries) == 4
    assert entries[0].unit == "archlinux-keyring-wkd-sync.service"
    assert entries[0].is_outlier is True
    assert entries[1].duration_ms >= OUTLIER_MS  # 5.569s
    assert entries[2].is_outlier is False
    assert entries[3].duration_ms == 60_000 + 2234


def test_parse_blame_skips_blank_lines() -> None:
    text = "\n\n5s foo.service\n\n"
    entries = parse_blame(text)
    assert len(entries) == 1
    assert entries[0].unit == "foo.service"
