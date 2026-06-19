"""Byte-level filesystem usage via `df -P -k`.

The companion `inodes.py` answers "can this box still create files?"; this
module answers the more familiar "how full is the disk in bytes?". We surface
every real mount plus its used percentage so the dashboard can headline the
fullest one.

Pseudo-filesystems (tmpfs, devtmpfs, squashfs, overlay) are filtered out for
the same reason as in `inodes.py`: they either report no meaningful capacity
or aren't actionable from this tool.
"""

from __future__ import annotations

import subprocess
from dataclasses import dataclass

WARN_PCT = 85
CRITICAL_PCT = 95

# `df -P -k` is POSIX-mode + 1024-byte blocks. POSIX mode means a fixed
# 6-column layout: Filesystem  1024-blocks  Used  Available  Capacity  Mounted on
_HEADER_PREFIX = "Filesystem"
_SKIP_FS_PREFIXES = ("tmpfs", "devtmpfs", "squashfs", "overlay", "udev", "/dev/loop")


@dataclass(frozen=True)
class DiskUsage:
    filesystem: str
    mount: str
    total_kb: int
    used_kb: int
    avail_kb: int
    used_pct: int

    @property
    def is_critical(self) -> bool:
        return self.used_pct >= CRITICAL_PCT

    @property
    def is_warn(self) -> bool:
        return self.used_pct >= WARN_PCT


def parse_df(text: str) -> list[DiskUsage]:
    """Parse `df -P -k` output. Skips pseudo filesystems and zero-size mounts."""
    out: list[DiskUsage] = []
    for line in text.splitlines():
        if not line or line.startswith(_HEADER_PREFIX):
            continue
        if line.startswith(_SKIP_FS_PREFIXES):
            continue
        # POSIX mode keeps everything but the mount point on one line —
        # split(maxsplit=5) gives us 6 fields even if the mount has a space.
        parts = line.split(None, 5)
        if len(parts) < 6:
            continue
        fs, total_s, used_s, avail_s, used_pct_s, mount = parts
        try:
            total = int(total_s)
            used = int(used_s)
            avail = int(avail_s)
        except ValueError:
            # `df` emits '-' for filesystems without usable accounting. Skip.
            continue
        if total == 0:
            # Zero-capacity mounts carry no usable signal.
            continue
        try:
            used_pct = int(used_pct_s.rstrip("%"))
        except ValueError:
            # Fall back to computing it from used/total.
            used_pct = round(used * 100 / total) if total else 0
        out.append(
            DiskUsage(
                filesystem=fs,
                mount=mount,
                total_kb=total,
                used_kb=used,
                avail_kb=avail,
                used_pct=used_pct,
            )
        )
    return out


def fullest(rows: list[DiskUsage]) -> DiskUsage | None:
    """The mount with the highest used percentage — the dashboard headline."""
    if not rows:
        return None
    return max(rows, key=lambda r: r.used_pct)


def alerts(rows: list[DiskUsage], *, threshold: int = WARN_PCT) -> list[DiskUsage]:
    """Subset of `rows` that breach the warning threshold."""
    return [r for r in rows if r.used_pct >= threshold]


def run_df(*, timeout_s: int = 5) -> list[DiskUsage]:
    """Shell out to `df -P -k` and parse the result. Returns [] on failure."""
    try:
        result = subprocess.run(
            ["df", "-P", "-k"],
            check=False,
            capture_output=True,
            text=True,
            timeout=timeout_s,
        )
    except (subprocess.TimeoutExpired, OSError):
        return []
    if result.returncode != 0:
        return []
    return parse_df(result.stdout)
