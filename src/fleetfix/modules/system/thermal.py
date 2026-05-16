"""CPU thermal readout from /sys/class/thermal.

Gracefully absent on VMs and containers — return None and let the UI hide
the widget rather than show a fake zero.
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

_THERMAL_ROOT = Path("/sys/class/thermal")


@dataclass(frozen=True)
class ThermalZone:
    name: str
    type: str
    temp_c: float


def read_zones(root: Path = _THERMAL_ROOT) -> list[ThermalZone]:
    """List every thermal_zone with a parseable temperature."""
    if not root.exists():
        return []

    zones: list[ThermalZone] = []
    for entry in sorted(root.glob("thermal_zone*")):
        temp_file = entry / "temp"
        type_file = entry / "type"
        if not temp_file.exists():
            continue
        try:
            raw = int(temp_file.read_text().strip())
        except (OSError, ValueError):
            continue
        type_name = type_file.read_text().strip() if type_file.exists() else entry.name
        zones.append(ThermalZone(name=entry.name, type=type_name, temp_c=raw / 1000.0))
    return zones


def hottest(zones: list[ThermalZone] | None = None) -> ThermalZone | None:
    """Return the warmest zone, or None on a host without thermal sensors."""
    zones = zones if zones is not None else read_zones()
    if not zones:
        return None
    return max(zones, key=lambda z: z.temp_c)
