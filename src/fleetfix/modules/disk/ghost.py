"""Ghost-space scanner — files deleted from the FS but still held open.

`lsof +L1` lists files whose link count is 0 (i.e. unlinked) but which
some process still has open. Their disk blocks stay allocated until the
last open fd closes. On long-running boxes this is the most common
"df says full, du says fine" cause — usually a daemon that was rotated
without a HUP / restart.

Output: per-file rows with PID, command, fd, size, and the path the file
had at unlink time. Aggregated total = bytes you'd reclaim by restarting
the owning services.
"""

from __future__ import annotations

import re
import subprocess
from dataclasses import dataclass

_FIELD_RE = re.compile(r"^(.)(.*)$")


@dataclass(frozen=True)
class GhostFile:
    pid: int
    command: str
    user: str
    fd: str
    size_bytes: int
    path: str


def parse_lsof_field_output(text: str) -> list[GhostFile]:
    """Parse `lsof -F pcuLfsn +L1` output.

    -F format: one field per line, prefixed by a single-char tag.
    A 'p<pid>' line starts a process group; subsequent 'c<cmd>', 'u<user>'
    apply to every file in that group until the next 'p'. Each file starts
    with 'f<fd>' and contains 's<size>', 'L<links>', 'n<name>'.
    We only emit files where L=0 (defensive — `+L1` already filters,
    but format is the same with or without that arg).
    """
    files: list[GhostFile] = []
    pid = 0
    command = ""
    user = ""
    cur: dict[str, str] = {}

    def flush() -> None:
        nonlocal cur
        if not cur:
            return
        # Only emit when the link count is 0 (deleted-but-open).
        try:
            links = int(cur.get("L", "1"))
        except ValueError:
            links = 1
        if links == 0:
            try:
                size = int(cur.get("s", "0"))
            except ValueError:
                size = 0
            files.append(
                GhostFile(
                    pid=pid,
                    command=command,
                    user=user,
                    fd=cur.get("f", ""),
                    size_bytes=size,
                    path=cur.get("n", ""),
                )
            )
        cur = {}

    for raw in text.splitlines():
        if not raw:
            continue
        m = _FIELD_RE.match(raw)
        if not m:
            continue
        tag, value = m.group(1), m.group(2)
        if tag == "p":
            flush()
            try:
                pid = int(value)
            except ValueError:
                pid = 0
            command = ""
            user = ""
        elif tag == "c":
            command = value
        elif tag == "u":
            user = value
        elif tag == "f":
            # New file record — flush prior one.
            flush()
            cur["f"] = value
        else:
            cur[tag] = value
    flush()
    return files


def total_bytes(files: list[GhostFile]) -> int:
    return sum(f.size_bytes for f in files)


def list_ghost_files(*, timeout_s: int = 20) -> list[GhostFile]:
    """Run `sudo -n lsof -F pcuLfsn +L1` and return parsed ghost files.

    Returns [] if lsof isn't installed, sudo refuses, or the command times
    out — never raises. Errors are an empty result, not an exception, so
    the UI doesn't have to special-case "we don't know".
    """
    try:
        result = subprocess.run(
            ["sudo", "-n", "lsof", "-F", "pcuLfsn", "+L1"],
            check=False,
            capture_output=True,
            text=True,
            timeout=timeout_s,
        )
    except (subprocess.TimeoutExpired, OSError):
        return []
    # lsof exits 1 when nothing matches `+L1`. That's not an error.
    return parse_lsof_field_output(result.stdout)
