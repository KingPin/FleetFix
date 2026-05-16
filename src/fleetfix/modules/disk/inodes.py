"""Inode-pressure scan via `df -i`.

A box can have 90% free *space* and still be unable to create files because
its filesystem ran out of inodes — usually millions of tiny session files,
mail spool entries, or untruncated docker container logs. We surface every
mount over a threshold (default 85%) plus the raw per-fs breakdown.

Pseudo-filesystems (tmpfs, devtmpfs, squashfs, overlay) are filtered out
because they either have no inode limit reported or aren't actionable from
this tool.
"""

from __future__ import annotations

import subprocess
from dataclasses import dataclass

WARN_PCT = 85
CRITICAL_PCT = 95

# `df -P -i` is POSIX-mode + inode-mode. POSIX mode means a fixed
# 6-column layout: Filesystem  Inodes  IUsed  IFree  IUse%  Mounted on
_HEADER_PREFIX = "Filesystem"
_SKIP_FS_PREFIXES = ("tmpfs", "devtmpfs", "squashfs", "overlay", "udev", "/dev/loop")


@dataclass(frozen=True)
class InodeUsage:
    filesystem: str
    mount: str
    total: int
    used: int
    free: int
    used_pct: int

    @property
    def is_critical(self) -> bool:
        return self.used_pct >= CRITICAL_PCT

    @property
    def is_warn(self) -> bool:
        return self.used_pct >= WARN_PCT


def parse_df_inodes(text: str) -> list[InodeUsage]:
    """Parse `df -P -i` output. Skips pseudo filesystems with no usable count."""
    out: list[InodeUsage] = []
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
        fs, total_s, used_s, free_s, used_pct_s, mount = parts
        try:
            total = int(total_s)
            used = int(used_s)
            free = int(free_s)
        except ValueError:
            # `df` emits '-' for filesystems without inode accounting
            # (e.g. some overlayfs). Skip those rows.
            continue
        if total == 0:
            # btrfs / zfs report 0 inodes — they're dynamic. No usable signal.
            continue
        try:
            used_pct = int(used_pct_s.rstrip("%"))
        except ValueError:
            # Fall back to computing it from used/total.
            used_pct = round(used * 100 / total) if total else 0
        out.append(
            InodeUsage(
                filesystem=fs,
                mount=mount,
                total=total,
                used=used,
                free=free,
                used_pct=used_pct,
            )
        )
    return out


def alerts(rows: list[InodeUsage], *, threshold: int = WARN_PCT) -> list[InodeUsage]:
    """Subset of `rows` that breach the warning threshold."""
    return [r for r in rows if r.used_pct >= threshold]


def run_df_inodes(*, timeout_s: int = 5) -> list[InodeUsage]:
    """Shell out to `df -P -i` and parse the result. Returns [] on failure."""
    try:
        result = subprocess.run(
            ["df", "-P", "-i"],
            check=False,
            capture_output=True,
            text=True,
            timeout=timeout_s,
        )
    except (subprocess.TimeoutExpired, OSError):
        return []
    if result.returncode != 0:
        return []
    return parse_df_inodes(result.stdout)
