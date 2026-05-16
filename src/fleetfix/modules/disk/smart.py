"""SMART health summary for block devices.

Drives smartctl out-of-band via sudo; surfaces the handful of attributes
that actually correlate with imminent failure:

- SATA/SCSI: attr 5 (Reallocated_Sector_Ct), 187 (Reported_Uncorrect),
  197 (Current_Pending_Sector), 9 (Power_On_Hours), 233 (SSD wear)
- NVMe: percentage_used, available_spare, available_spare_threshold,
  media_and_data_integrity_errors

Parsing is robust against the wildly different output formats smartctl
emits — we never trust a header to be in column N, we look up rows by
the attribute name / id pair.
"""

from __future__ import annotations

import re
import subprocess
from dataclasses import dataclass, field
from pathlib import Path

# Attributes we care about, by id. Keyed by id rather than name because
# vendor renames break name-based lookups (Seagate vs WD reallocations).
_SATA_INTERESTING_IDS: dict[int, str] = {
    5: "reallocated_sectors",
    9: "power_on_hours",
    187: "reported_uncorrect",
    197: "current_pending_sector",
    233: "ssd_wear_indicator",
}

_HEALTH_RE = re.compile(
    r"SMART overall-health self-assessment test result:\s*(\S+)",
    re.IGNORECASE,
)
# SATA attribute row example:
#   "  5 Reallocated_Sector_Ct   0x0033   100   100   010    Pre-fail  Always       -       0"
_SATA_ROW_RE = re.compile(
    r"^\s*(?P<id>\d+)\s+\S+\s+\S+\s+\d+\s+\d+\s+\d+\s+\S+\s+\S+\s+\S+\s+(?P<raw>\S+)",
)
# NVMe lines look like "Percentage Used:                    3%" — colon-separated.
_NVME_PCT_USED = re.compile(r"^\s*Percentage Used:\s+(\d+)\s*%", re.MULTILINE)
_NVME_AVAIL_SPARE = re.compile(r"^\s*Available Spare:\s+(\d+)\s*%", re.MULTILINE)
_NVME_AVAIL_SPARE_THRESH = re.compile(r"^\s*Available Spare Threshold:\s+(\d+)\s*%", re.MULTILINE)
_NVME_INTEGRITY = re.compile(r"^\s*Media and Data Integrity Errors:\s+([\d,]+)", re.MULTILINE)


@dataclass(frozen=True)
class SmartReport:
    device: str  # e.g. "/dev/sda" or "/dev/nvme0n1"
    kind: str  # "sata" | "nvme" | "unknown"
    health: str | None  # "PASSED" / "FAILED" / None if not in output
    attributes: dict[str, int] = field(default_factory=dict)
    error: str | None = None

    @property
    def ok(self) -> bool:
        return self.health is not None and self.health.upper() == "PASSED" and self.error is None


def parse_sata_attributes(text: str) -> dict[str, int]:
    """Pull the few SATA attributes we care about out of `smartctl -A` output."""
    out: dict[str, int] = {}
    for line in text.splitlines():
        m = _SATA_ROW_RE.match(line)
        if not m:
            continue
        attr_id = int(m.group("id"))
        if attr_id not in _SATA_INTERESTING_IDS:
            continue
        raw = m.group("raw")
        # raw is sometimes "0" or "12345" or "12345h+0m+0.000s" (Power_On_Hours).
        # Take the leading integer.
        digit_run = re.match(r"\d+", raw)
        if digit_run is None:
            continue
        out[_SATA_INTERESTING_IDS[attr_id]] = int(digit_run.group(0))
    return out


def parse_nvme_attributes(text: str) -> dict[str, int]:
    """Pull NVMe SMART/health attributes out of `smartctl -A` output."""
    out: dict[str, int] = {}
    if (m := _NVME_PCT_USED.search(text)) is not None:
        out["percentage_used"] = int(m.group(1))
    if (m := _NVME_AVAIL_SPARE.search(text)) is not None:
        out["available_spare"] = int(m.group(1))
    if (m := _NVME_AVAIL_SPARE_THRESH.search(text)) is not None:
        out["available_spare_threshold"] = int(m.group(1))
    if (m := _NVME_INTEGRITY.search(text)) is not None:
        out["media_and_data_integrity_errors"] = int(m.group(1).replace(",", ""))
    return out


def parse_health(text: str) -> str | None:
    """Pull the PASSED/FAILED verdict out of `smartctl -H` output."""
    m = _HEALTH_RE.search(text)
    return m.group(1) if m else None


def _kind_for_device(device: str) -> str:
    name = Path(device).name
    if name.startswith("nvme"):
        return "nvme"
    if name.startswith("sd") or name.startswith("hd"):
        return "sata"
    return "unknown"


def enumerate_block_devices(sys_block: Path = Path("/sys/block")) -> list[str]:
    """List `/dev/<name>` for every real disk under /sys/block.

    Filters out loop, ram, dm-* and partition-only entries. NVMe namespaces
    (`nvme0n1`) are included; partitions (`nvme0n1p1`) are not because they
    don't appear at the top of /sys/block.
    """
    try:
        names = sorted(p.name for p in sys_block.iterdir())
    except OSError:
        return []
    keep: list[str] = []
    for name in names:
        if name.startswith(("loop", "ram", "dm-", "sr", "fd", "zram")):
            continue
        keep.append(f"/dev/{name}")
    return keep


def read_device(device: str, *, timeout_s: int = 15) -> SmartReport:
    """Shell out to `sudo smartctl -H -A <device>` and parse the result."""
    kind = _kind_for_device(device)
    try:
        result = subprocess.run(
            ["sudo", "-n", "smartctl", "-H", "-A", device],
            check=False,
            capture_output=True,
            text=True,
            timeout=timeout_s,
        )
    except (subprocess.TimeoutExpired, OSError) as exc:
        return SmartReport(device=device, kind=kind, health=None, error=str(exc))
    # smartctl exit codes are bit-flags: 0=ok, anything nonzero means
    # something is off. We still try to parse — partial info is useful.
    text = result.stdout
    health = parse_health(text)
    if kind == "nvme":
        attrs = parse_nvme_attributes(text)
    else:
        attrs = parse_sata_attributes(text)
    error: str | None = None
    if result.returncode != 0 and not attrs and health is None:
        error = (result.stderr or "smartctl exited nonzero with no parseable output").strip()
    return SmartReport(
        device=device,
        kind=kind,
        health=health,
        attributes=attrs,
        error=error,
    )


def report_all(devices: list[str] | None = None) -> list[SmartReport]:
    """SMART summary for every disk on the box (or an explicit list)."""
    targets = devices if devices is not None else enumerate_block_devices()
    return [read_device(d) for d in targets]
