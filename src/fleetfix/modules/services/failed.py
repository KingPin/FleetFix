"""Failed-units reader: `systemctl list-units --state=failed`.

The default `systemctl` output is multi-column space-padded text with an
optional legend; we use `--plain --no-legend --no-pager` to get only the
data rows and parse them by splitting on whitespace.

Schema per row (whitespace-separated):
  UNIT  LOAD  ACTIVE  SUB  DESCRIPTION

DESCRIPTION can contain spaces, so it's everything after the 4th column.
"""

from __future__ import annotations

import subprocess
from dataclasses import dataclass


@dataclass(frozen=True)
class FailedUnit:
    name: str
    load: str
    active: str
    sub: str
    description: str


def parse_failed_units(text: str) -> list[FailedUnit]:
    out: list[FailedUnit] = []
    for line in text.splitlines():
        line = line.rstrip()
        if not line or line.startswith(("UNIT ", "  ")):
            continue
        parts = line.split(maxsplit=4)
        if len(parts) < 4:
            continue
        name, load, active, sub = parts[0], parts[1], parts[2], parts[3]
        description = parts[4] if len(parts) > 4 else ""
        out.append(
            FailedUnit(
                name=name,
                load=load,
                active=active,
                sub=sub,
                description=description,
            )
        )
    return out


def list_failed_units() -> list[FailedUnit]:
    try:
        result = subprocess.run(
            [
                "systemctl",
                "list-units",
                "--state=failed",
                "--no-legend",
                "--no-pager",
                "--plain",
            ],
            capture_output=True,
            text=True,
            timeout=10,
            check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return []
    if result.returncode != 0:
        return []
    return parse_failed_units(result.stdout)
