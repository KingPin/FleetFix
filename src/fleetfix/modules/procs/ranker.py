"""Process ranker — top-N by RSS or CPU directly from /proc.

Avoids shelling out to `ps` so the snapshot is consistent (one open()
per process) and survives in restricted-PATH environments. RSS comes
from `/proc/<pid>/statm` (pages) times page size. CPU time is derived from
`/proc/<pid>/stat` utime+stime in clock ticks — we sample twice with a
short sleep to compute a usage percentage.
"""

from __future__ import annotations

import os
import time
from dataclasses import dataclass
from pathlib import Path

PAGE_SIZE = os.sysconf("SC_PAGE_SIZE") if hasattr(os, "sysconf") else 4096
CLOCK_HZ = os.sysconf("SC_CLK_TCK") if hasattr(os, "sysconf") else 100


@dataclass(frozen=True)
class ProcInfo:
    pid: int
    comm: str
    user: str | None
    rss_bytes: int
    cpu_pct: float
    cmdline: str


def _read_text(path: Path) -> str | None:
    try:
        return path.read_text(encoding="utf-8", errors="replace")
    except (OSError, PermissionError):
        return None


def _parse_stat_comm_and_ticks(text: str) -> tuple[str, int] | None:
    """`/proc/<pid>/stat` is space-separated EXCEPT comm, which is
    parenthesised and may contain spaces. Find the last ')' and split from
    there, then index utime=field 14 (0-based 13) and stime=field 15.
    """
    end = text.rfind(")")
    if end < 0:
        return None
    comm_start = text.find("(")
    if comm_start < 0 or comm_start > end:
        return None
    comm = text[comm_start + 1 : end]
    rest = text[end + 1 :].split()
    # rest[0] is field 3 (state). utime is field 14, stime is field 15,
    # so rest[14 - 3] = rest[11] and rest[12].
    if len(rest) < 13:
        return None
    try:
        utime = int(rest[11])
        stime = int(rest[12])
    except ValueError:
        return None
    return comm, utime + stime


def _parse_statm_rss_pages(text: str) -> int | None:
    # statm format: size resident shared text lib data dt
    parts = text.split()
    if len(parts) < 2:
        return None
    try:
        return int(parts[1])
    except ValueError:
        return None


def _read_user(uid: int) -> str | None:
    try:
        import pwd

        return pwd.getpwuid(uid).pw_name
    except (KeyError, ImportError):
        return None


def _list_pids(proc: Path) -> list[int]:
    pids: list[int] = []
    try:
        entries = list(proc.iterdir())
    except OSError:
        return []
    for entry in entries:
        if not entry.name.isdigit():
            continue
        pids.append(int(entry.name))
    return pids


def snapshot(*, sample_interval_s: float = 0.2, proc: Path = Path("/proc")) -> list[ProcInfo]:
    """Take two stat samples `sample_interval_s` apart to compute CPU%.

    Returns one ProcInfo per process visible at the second sample. CPU%
    is 100 * (delta ticks for this pid) / (interval * CLOCK_HZ * cpu_count).
    Total across all processes will not sum to 100 — that's per-core
    normalisation.
    """
    first: dict[int, int] = {}
    for pid in _list_pids(proc):
        text = _read_text(proc / str(pid) / "stat")
        if text is None:
            continue
        parsed = _parse_stat_comm_and_ticks(text)
        if parsed is None:
            continue
        first[pid] = parsed[1]

    time.sleep(sample_interval_s)

    cpu_count = os.cpu_count() or 1
    interval_ticks = max(sample_interval_s * CLOCK_HZ * cpu_count, 1.0)

    out: list[ProcInfo] = []
    for pid in _list_pids(proc):
        pid_dir = proc / str(pid)
        stat_text = _read_text(pid_dir / "stat")
        if stat_text is None:
            continue
        parsed = _parse_stat_comm_and_ticks(stat_text)
        if parsed is None:
            continue
        comm, total_ticks = parsed
        prev = first.get(pid)
        cpu_pct = 0.0 if prev is None else max(0.0, (total_ticks - prev) * 100.0 / interval_ticks)

        statm_text = _read_text(pid_dir / "statm")
        rss_pages = _parse_statm_rss_pages(statm_text) if statm_text else 0

        cmdline_text = _read_text(pid_dir / "cmdline") or ""
        cmdline = cmdline_text.replace("\x00", " ").strip()

        user: str | None = None
        try:
            uid = (pid_dir / "status").stat().st_uid
            user = _read_user(uid)
        except OSError:
            pass

        out.append(
            ProcInfo(
                pid=pid,
                comm=comm,
                user=user,
                rss_bytes=(rss_pages or 0) * PAGE_SIZE,
                cpu_pct=cpu_pct,
                cmdline=cmdline,
            )
        )
    return out


def top_by_rss(procs: list[ProcInfo], n: int = 10) -> list[ProcInfo]:
    return sorted(procs, key=lambda p: p.rss_bytes, reverse=True)[:n]


def top_by_cpu(procs: list[ProcInfo], n: int = 10) -> list[ProcInfo]:
    return sorted(procs, key=lambda p: p.cpu_pct, reverse=True)[:n]
