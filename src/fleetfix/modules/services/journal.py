"""Fetch the last N lines from `journalctl -u <unit>`.

Used by the services screen to show a tail of a failed unit's recent log
without leaving the TUI. The shell-out is read-only, so we never need a
sudo wrap here (the systemd journal is readable by the `adm` group on
Debian/Ubuntu and by the operator on most distros).
"""

from __future__ import annotations

import subprocess

DEFAULT_LINES = 100


def journal_tail(unit: str, *, lines: int = DEFAULT_LINES) -> str:
    """Return the last `lines` of `journalctl -u <unit>` output, or an error string."""
    if not unit:
        return "(no unit selected)"
    try:
        result = subprocess.run(
            [
                "journalctl",
                "-u",
                unit,
                "-n",
                str(lines),
                "--no-pager",
                "--output=short-iso",
            ],
            capture_output=True,
            text=True,
            timeout=10,
            check=False,
        )
    except FileNotFoundError:
        return "(journalctl not found)"
    except subprocess.TimeoutExpired:
        return "(journalctl timed out)"
    if result.returncode != 0:
        return result.stderr.strip() or f"(journalctl exited {result.returncode})"
    return result.stdout
