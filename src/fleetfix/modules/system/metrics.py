"""System metrics: uptime, load average, memory.

Pure Python — reads /proc directly so it works without psutil and stays
fast on hosts where importing heavy libs would be wasteful.
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

_PROC_UPTIME = Path("/proc/uptime")
_PROC_LOADAVG = Path("/proc/loadavg")
_PROC_MEMINFO = Path("/proc/meminfo")


@dataclass(frozen=True)
class LoadAverage:
    one: float
    five: float
    fifteen: float


@dataclass(frozen=True)
class MemoryInfo:
    total_kb: int
    available_kb: int
    used_kb: int
    swap_total_kb: int
    swap_used_kb: int

    @property
    def used_pct(self) -> float:
        if self.total_kb == 0:
            return 0.0
        return (self.used_kb / self.total_kb) * 100.0

    @property
    def swap_used_pct(self) -> float:
        if self.swap_total_kb == 0:
            return 0.0
        return (self.swap_used_kb / self.swap_total_kb) * 100.0


@dataclass(frozen=True)
class SystemMetrics:
    uptime_seconds: float
    load: LoadAverage
    memory: MemoryInfo


def read_uptime(source: Path = _PROC_UPTIME) -> float:
    """Return uptime in seconds. Format of /proc/uptime: '<uptime> <idle>'."""
    text = source.read_text()
    return float(text.split()[0])


def read_loadavg(source: Path = _PROC_LOADAVG) -> LoadAverage:
    """Return 1/5/15-minute load averages. Format: '0.12 0.34 0.56 1/123 4567'."""
    parts = source.read_text().split()
    return LoadAverage(one=float(parts[0]), five=float(parts[1]), fifteen=float(parts[2]))


def read_meminfo(source: Path = _PROC_MEMINFO) -> MemoryInfo:
    """Parse /proc/meminfo into total/available/used (RAM + swap), in KB."""
    fields: dict[str, int] = {}
    for line in source.read_text().splitlines():
        key, _, rest = line.partition(":")
        if not rest:
            continue
        value = rest.strip().split()
        if value and value[0].isdigit():
            fields[key.strip()] = int(value[0])

    total = fields.get("MemTotal", 0)
    available = fields.get("MemAvailable", fields.get("MemFree", 0))
    swap_total = fields.get("SwapTotal", 0)
    swap_free = fields.get("SwapFree", 0)

    return MemoryInfo(
        total_kb=total,
        available_kb=available,
        used_kb=max(total - available, 0),
        swap_total_kb=swap_total,
        swap_used_kb=max(swap_total - swap_free, 0),
    )


def read_all() -> SystemMetrics:
    return SystemMetrics(
        uptime_seconds=read_uptime(),
        load=read_loadavg(),
        memory=read_meminfo(),
    )


def format_uptime(seconds: float) -> str:
    """Render uptime as '3d 14h 22m' or '14h 22m' or '22m 8s'."""
    total = int(seconds)
    days, total = divmod(total, 86400)
    hours, total = divmod(total, 3600)
    minutes, secs = divmod(total, 60)
    if days:
        return f"{days}d {hours}h {minutes}m"
    if hours:
        return f"{hours}h {minutes}m"
    return f"{minutes}m {secs}s"
