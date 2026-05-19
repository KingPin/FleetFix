"""Failed-units reader: `systemctl list-units --state=failed`.

The default `systemctl` output is multi-column space-padded text with an
optional legend; we use `--plain --no-legend --no-pager` to get only the
data rows and parse them by splitting on whitespace.

Schema per row (whitespace-separated):
  UNIT  LOAD  ACTIVE  SUB  DESCRIPTION

DESCRIPTION can contain spaces, so it's everything after the 4th column.

Optional target-user filtering
-------------------------------
When `target_user` is supplied to `list_failed_units`, a single bulk
`systemctl show -p User <unit1> <unit2> ...` call retrieves the owning user
for every failed unit in one round-trip.  Units with no explicit `User=` are
normalised to `"root"` (systemd's default) so that `target_user="root"`
returns both explicitly-root and unspecified units.  Any error from the bulk
show call (non-zero exit, FileNotFoundError, timeout, or a block-count
mismatch) is treated conservatively: the function returns `[]` rather than
returning a silently-unfiltered list.
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


def parse_show_user(text: str) -> list[str]:
    """Parse `systemctl show -p User unit1 unit2 ...` multi-block output.

    Each block contains a `User=...` line; blocks are separated by blank
    lines.  Empty `User=` (the default when no override is set) is normalised
    to `"root"` since systemd runs unspecified units as root.
    """
    users: list[str] = []
    for block in text.split("\n\n"):
        for line in block.splitlines():
            if line.startswith("User="):
                value = line[len("User=") :].strip()
                users.append(value if value else "root")
                break
    return users


def list_failed_units(target_user: str | None = None) -> list[FailedUnit]:
    """List failed systemd units, optionally filtered by `User=` matching `target_user`.

    A unit with no explicit `User=` is treated as `root`; it is therefore only
    kept when `target_user == "root"`.  The filter is implemented with a single
    bulk `systemctl show -p User <unit1> <unit2> ...` subprocess call.
    """
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
    units = parse_failed_units(result.stdout)
    if target_user is None or not units:
        return units
    try:
        show = subprocess.run(
            ["systemctl", "show", "-p", "User", *[u.name for u in units]],
            capture_output=True,
            text=True,
            timeout=10,
            check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return []
    if show.returncode != 0:
        return []
    users = parse_show_user(show.stdout)
    if len(users) != len(units):
        return []
    return [u for u, owner in zip(units, users, strict=True) if owner == target_user]
