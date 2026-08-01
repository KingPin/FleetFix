"""Tests for `df -P -k` (byte-level usage) parser."""

from __future__ import annotations

from fleetfix.modules.disk.usage import (
    CRITICAL_PCT,
    WARN_PCT,
    alerts,
    fullest,
    parse_df,
)
from tests.support.fixtures import fixture

_FIXTURE = fixture("df/usage_mixed.txt")


def test_parse_skips_pseudo_filesystems() -> None:
    rows = parse_df(_FIXTURE)
    fss = {r.filesystem for r in rows}
    assert "udev" not in fss
    assert "tmpfs" not in fss
    assert "overlay" not in fss


def test_parse_skips_zero_capacity_mounts() -> None:
    rows = parse_df(_FIXTURE)
    # /boot/efi reports 0 blocks — should be skipped.
    assert all(r.mount != "/boot/efi" for r in rows)


def test_parse_extracts_real_rows() -> None:
    rows = parse_df(_FIXTURE)
    mounts = {r.mount: r for r in rows}
    assert set(mounts) == {"/", "/var", "/var/lib/docker"}
    assert mounts["/var"].used_pct == 90
    assert mounts["/"].total_kb == 102400000
    assert mounts["/"].used_kb == 51200000
    assert mounts["/"].avail_kb == 51200000


def test_alerts_filter_above_threshold() -> None:
    rows = parse_df(_FIXTURE)
    warn = alerts(rows, threshold=WARN_PCT)
    assert {r.mount for r in warn} == {"/var", "/var/lib/docker"}


def test_alerts_critical_subset() -> None:
    rows = parse_df(_FIXTURE)
    crit = alerts(rows, threshold=CRITICAL_PCT)
    assert {r.mount for r in crit} == {"/var/lib/docker"}


def test_is_warn_and_is_critical_flags() -> None:
    rows = parse_df(_FIXTURE)
    by_mount = {r.mount: r for r in rows}
    assert by_mount["/var/lib/docker"].is_critical
    assert by_mount["/var/lib/docker"].is_warn
    assert by_mount["/var"].is_warn
    assert not by_mount["/var"].is_critical
    assert not by_mount["/"].is_warn


def test_fullest_returns_highest_pct() -> None:
    rows = parse_df(_FIXTURE)
    top = fullest(rows)
    assert top is not None
    assert top.mount == "/var/lib/docker"
    assert top.used_pct == 96


def test_fullest_empty_is_none() -> None:
    assert fullest([]) is None


def test_parse_handles_mount_with_spaces() -> None:
    text = fixture("df/usage_mount_with_spaces.txt")
    rows = parse_df(text)
    assert len(rows) == 1
    assert rows[0].mount == "/mnt/with space"


def test_parse_handles_missing_capacity_percent() -> None:
    # Some df builds emit "-" for the percentage on dynamic filesystems.
    text = fixture("df/usage_missing_capacity.txt")
    rows = parse_df(text)
    assert rows[0].used_pct == 30  # computed from used/total
