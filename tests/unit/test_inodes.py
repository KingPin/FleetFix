"""Tests for `df -P -i` parser."""

from __future__ import annotations

from fleetfix.modules.disk.inodes import (
    CRITICAL_PCT,
    WARN_PCT,
    alerts,
    parse_df_inodes,
)

_FIXTURE = """\
Filesystem       Inodes   IUsed   IFree IUse% Mounted on
udev            2034567     500 2034067    1% /dev
tmpfs           2046789     800 2045989    1% /run
/dev/sda1      62500000 5800000 56700000   10% /
/dev/sdb1      31250000 28000000 3250000   90% /var
/dev/sdc1      15625000 15000000  625000   96% /var/lib/docker
tmpfs           2046789       1 2046788    1% /dev/shm
/dev/sda2             0       0       0    -  /boot/efi
overlay          123456    1000  122456    1% /var/lib/docker/overlay2/abc
"""


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
    text = (
        "Filesystem       Inodes   IUsed   IFree IUse% Mounted on\n"
        "/dev/sdd1       1000000  500000  500000   50% /mnt/with space\n"
    )
    rows = parse_df_inodes(text)
    assert len(rows) == 1
    assert rows[0].mount == "/mnt/with space"


def test_parse_handles_missing_iuse_percent() -> None:
    # Some df builds emit "-" for the percentage on dynamic filesystems.
    text = (
        "Filesystem       Inodes   IUsed   IFree IUse% Mounted on\n"
        "/dev/sde1       1000000  300000  700000   - /mnt/x\n"
    )
    rows = parse_df_inodes(text)
    assert rows[0].used_pct == 30  # computed from used/total
