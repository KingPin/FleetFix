"""Parse `ss -tlnp` output into a structured list of listening TCP sockets.

We invoke `ss` rather than walking /proc/net/tcp because ss already
handles IPv4/IPv6, name resolution suppression, and PID/program lookup
in one tool that's present on every modern Debian/Ubuntu image.
"""

from __future__ import annotations

import re
import subprocess
from dataclasses import dataclass

# Example line we want to parse (column positions are not fixed, so we
# split on whitespace and pull values by index):
#   LISTEN 0  4096  127.0.0.1:5432  0.0.0.0:*  users:(("postgres",pid=1234,fd=8))
_USERS_RE = re.compile(r'\("([^"]+)",pid=(\d+),fd=\d+\)')


@dataclass(frozen=True)
class ListeningSocket:
    local_address: str
    local_port: int
    process_name: str | None
    pid: int | None


def parse_ss_output(output: str) -> list[ListeningSocket]:
    """Parse `ss -tlnp` (or `-tlnpH`) into a list of listening sockets."""
    sockets: list[ListeningSocket] = []
    for raw in output.splitlines():
        line = raw.strip()
        if not line or line.startswith("State") or line.startswith("Netid"):
            continue
        parts = line.split()
        # Expected form: STATE recv-q send-q local-addr peer-addr [users:...]
        if len(parts) < 4:
            continue
        if parts[0] != "LISTEN":
            continue
        local = parts[3]
        addr, port = _split_addr_port(local)
        if port is None:
            continue
        proc_name, pid = _extract_users(line)
        sockets.append(
            ListeningSocket(
                local_address=addr,
                local_port=port,
                process_name=proc_name,
                pid=pid,
            )
        )
    return sockets


def _split_addr_port(local: str) -> tuple[str, int | None]:
    """Split 'addr:port' (IPv4) or '[::1]:port' / '*:port' (IPv6 / wildcard)."""
    if local.startswith("["):
        end = local.find("]")
        if end == -1:
            return local, None
        addr = local[1:end]
        port_part = local[end + 2 :]
    else:
        addr, _, port_part = local.rpartition(":")
        if not addr:
            return local, None
    try:
        return addr, int(port_part)
    except ValueError:
        return addr, None


def _extract_users(line: str) -> tuple[str | None, int | None]:
    """Pull program name + pid out of the users:(("name",pid=N,fd=K)) suffix."""
    match = _USERS_RE.search(line)
    if match is None:
        return None, None
    return match.group(1), int(match.group(2))


def list_listening_sockets() -> list[ListeningSocket]:
    """Run `ss -tlnpH` and parse its output. Returns [] on failure."""
    try:
        result = subprocess.run(
            ["ss", "-tlnpH"],
            check=False,
            capture_output=True,
            text=True,
            timeout=5,
        )
    except (subprocess.TimeoutExpired, OSError):
        return []
    if result.returncode != 0:
        return []
    return parse_ss_output(result.stdout)
