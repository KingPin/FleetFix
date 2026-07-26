"""Tests for `df -P -i` parser."""

from __future__ import annotations

from fleetfix.modules.disk.inodes import (
    CRITICAL_PCT,
    WARN_PCT,
    alerts,
    parse_df_inodes,
)
from tests.support.fixtures import fixture

_FIXTURE = fixture("df/inodes_mixed.txt")


def test_parse_skips_pseudo_filesystems() -> None:
    rows = parse_df_inodes(_FIXTURE)
    fss = {r.filesystem for r in rows}
    assert "udev" not in fss
    assert "tmpfs" not in fss
    assert "overlay" not in fss


def test_parse_skips_dynamic_inode_filesystems() -> None:
    rows = parse_df_inodes(_FIXTURE)
    # /boot/efi reports 0 inodes — should be skipped.
    assert all(r.mount != "/boot/efi" for r in rows)


def test_parse_extracts_real_rows() -> None:
    rows = parse_df_inodes(_FIXTURE)
    mounts = {r.mount: r for r in rows}
    assert set(mounts) == {"/", "/var", "/var/lib/docker"}
    assert mounts["/var"].used_pct == 90
    assert mounts["/var/lib/docker"].used == 15000000


def test_alerts_filter_above_threshold() -> None:
    rows = parse_df_inodes(_FIXTURE)
    warn = alerts(rows, threshold=WARN_PCT)
    assert {r.mount for r in warn} == {"/var", "/var/lib/docker"}


def test_alerts_critical_subset() -> None:
    rows = parse_df_inodes(_FIXTURE)
    crit = alerts(rows, threshold=CRITICAL_PCT)
    assert {r.mount for r in crit} == {"/var/lib/docker"}


def test_is_warn_and_is_critical_flags() -> None:
    rows = parse_df_inodes(_FIXTURE)
    by_mount = {r.mount: r for r in rows}
    assert by_mount["/var/lib/docker"].is_critical
    assert by_mount["/var/lib/docker"].is_warn
    assert by_mount["/var"].is_warn
    assert not by_mount["/var"].is_critical
    assert not by_mount["/"].is_warn


def test_parse_handles_mount_with_spaces() -> None:
    # `df -P` keeps the mount point on one line; we split(None, 5).
    text = fixture("df/inodes_mount_with_spaces.txt")
    rows = parse_df_inodes(text)
    assert len(rows) == 1
    assert rows[0].mount == "/mnt/with space"


def test_parse_handles_missing_iuse_percent() -> None:
    # Some df builds emit "-" for the percentage on dynamic filesystems.
    text = fixture("df/inodes_missing_iuse.txt")
    rows = parse_df_inodes(text)
    assert rows[0].used_pct == 30  # computed from used/total
