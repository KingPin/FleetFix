"""Boot forensics: parse `systemd-analyze blame`.

Output looks like:
    59.647s archlinux-keyring-wkd-sync.service
     5.569s NetworkManager-wait-online.service
      559ms NetworkManager.service
        1min 2.234s long-running.service

We normalize the time column into milliseconds so the UI can sort and
flag outliers (default >5s).
"""

from __future__ import annotations

import re
import subprocess
from dataclasses import dataclass

OUTLIER_MS = 5_000

_TIME_TOKEN = re.compile(r"(?:(\d+)min)?\s*(?:([\d.]+)s)?(?:([\d.]+)ms)?")


@dataclass(frozen=True)
class BlameEntry:
    unit: str
    duration_ms: int

    @property
    def is_outlier(self) -> bool:
        return self.duration_ms >= OUTLIER_MS


def parse_blame(text: str) -> list[BlameEntry]:
    out: list[BlameEntry] = []
    for line in text.splitlines():
        line = line.strip()
        if not line:
            continue
        # Time prefix may include multiple tokens (e.g. "1min 2.234s"). Split
        # on the LAST whitespace before the unit name, which is always the
        # service name with no spaces.
        try:
            time_part, unit = line.rsplit(maxsplit=1)
        except ValueError:
            continue
        ms = _parse_time(time_part)
        if ms is None:
            continue
        out.append(BlameEntry(unit=unit, duration_ms=ms))
    return out


def _parse_time(token: str) -> int | None:
    """Parse `1min 2.234s`, `59.647s`, `559ms` → milliseconds."""
    token = token.strip()
    if not token:
        return None
    total_ms = 0
    matched = False
    # min component
    m = re.search(r"(\d+)min", token)
    if m:
        total_ms += int(m.group(1)) * 60_000
        matched = True
    # seconds (must not be preceded by 'm' to avoid 'ms')
    m = re.search(r"(?<![\d.])([\d.]+)s\b", token)
    if m:
        total_ms += int(float(m.group(1)) * 1000)
        matched = True
    # milliseconds
    m = re.search(r"([\d.]+)ms\b", token)
    if m:
        total_ms += int(float(m.group(1)))
        matched = True
    return total_ms if matched else None


def blame() -> list[BlameEntry]:
    try:
        result = subprocess.run(
            ["systemd-analyze", "blame", "--no-pager"],
            capture_output=True,
            text=True,
            timeout=10,
            check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return []
    if result.returncode != 0:
        return []
    return parse_blame(result.stdout)
