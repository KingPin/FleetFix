"""Tier 1 view of pending apt upgrades.

Two strategies, tried in order:

1. Read `/var/lib/update-notifier/updates-available` — populated by the
   unattended-upgrades / update-notifier package on Ubuntu. Cheap, no
   subprocess, returns the same numbers the MOTD shows on login.
2. Fall back to `apt list --upgradable 2>/dev/null` and count the lines.
   This works on Debian 12 (where update-notifier isn't installed by
   default) and on Ubuntu boxes where the notifier hasn't run yet.

Neither approach requires sudo, so this stays in Tier 1.
"""

from __future__ import annotations

import re
import subprocess
from dataclasses import dataclass
from pathlib import Path

NOTIFIER_PATH = Path("/var/lib/update-notifier/updates-available")

_NOTIFIER_REGULAR = re.compile(r"(\d+)\s+package(?:s)?\s+can\s+be\s+updated", re.IGNORECASE)
# Security wording varies wildly between releases:
#   "8 updates are security updates."
#   "8 of these updates are security updates."
#   "8 are security updates."
# Match the leading int on any line that ends with "security update[s]".
_NOTIFIER_SECURITY = re.compile(r"^\s*(\d+)\b.*security\s+update", re.IGNORECASE | re.MULTILINE)


@dataclass(frozen=True)
class UpdateStatus:
    upgradable: int
    security: int
    source: str  # "update-notifier" | "apt" | "unavailable"
    error: str | None = None


def parse_notifier_text(text: str) -> tuple[int, int] | None:
    """Pull `(upgradable, security)` out of the update-notifier blurb.

    The file is human-readable and varies between Ubuntu releases, so we
    grep for the two numbers rather than enforce a strict schema.
    """
    upgradable_match = _NOTIFIER_REGULAR.search(text)
    if upgradable_match is None:
        return None
    upgradable = int(upgradable_match.group(1))
    security_match = _NOTIFIER_SECURITY.search(text)
    security = int(security_match.group(1)) if security_match else 0
    return upgradable, security


def from_notifier(path: Path | None = None) -> UpdateStatus | None:
    """Read the update-notifier file. Returns None if not present / unparseable.

    Path is looked up at call time so tests can monkeypatch
    `updates.NOTIFIER_PATH` and have the change take effect.
    """
    actual = path if path is not None else NOTIFIER_PATH
    try:
        text = actual.read_text(encoding="utf-8", errors="replace")
    except OSError:
        return None
    parsed = parse_notifier_text(text)
    if parsed is None:
        return None
    upgradable, security = parsed
    return UpdateStatus(
        upgradable=upgradable,
        security=security,
        source="update-notifier",
    )


def from_apt(timeout_s: int = 10) -> UpdateStatus | None:
    """Run `apt list --upgradable` and count the upgradable lines.

    Lines starting with "Listing..." or empty lines are skipped. Lines
    that contain `-security/` are counted as security updates (works on
    both Ubuntu and Debian's security pocket naming).
    """
    try:
        result = subprocess.run(
            ["apt", "list", "--upgradable"],
            check=False,
            capture_output=True,
            text=True,
            timeout=timeout_s,
        )
    except (subprocess.TimeoutExpired, OSError):
        return None
    if result.returncode != 0:
        return None

    upgradable = 0
    security = 0
    for raw in result.stdout.splitlines():
        line = raw.strip()
        if not line or line.startswith("Listing"):
            continue
        upgradable += 1
        # apt list --upgradable rows look like
        #   pkg/jammy-security 1.2.3 amd64 [upgradable from: 1.2.2]
        # i.e. the pocket is in the second slash-separated field and ends in
        # "-security" before the next whitespace.
        suite = line.split(" ", 1)[0]
        if suite.endswith("-security") or "-security/" in line:
            security += 1
    return UpdateStatus(upgradable=upgradable, security=security, source="apt")


def get_update_status() -> UpdateStatus:
    """Best-effort update count. Always returns a status, even on total failure."""
    primary = from_notifier()
    if primary is not None:
        return primary
    fallback = from_apt()
    if fallback is not None:
        return fallback
    return UpdateStatus(
        upgradable=0,
        security=0,
        source="unavailable",
        error="neither update-notifier nor apt produced parseable output",
    )
