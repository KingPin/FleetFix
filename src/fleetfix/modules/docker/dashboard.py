"""Container dashboard — `docker ps -a` plus restart-loop detection.

`list_containers()` is the entry point. It runs `docker ps -a` in JSON-lines
form to get one row per container, then runs `docker inspect` per container
to recover the RestartCount, StartedAt, and LogPath fields that `ps` doesn't
expose. Containers whose RestartCount exceeds `RESTART_LOOP_THRESHOLD` and
whose most recent StartedAt is within `RESTART_LOOP_WINDOW_S` are flagged
as a restart loop.

All subprocess calls go through small wrappers so tests can replace them
with fixtures without monkeypatching `subprocess` itself.
"""

from __future__ import annotations

import json
import subprocess
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone

RESTART_LOOP_THRESHOLD = 3
RESTART_LOOP_WINDOW_S = 10 * 60


@dataclass(frozen=True)
class Container:
    id: str
    name: str
    image: str
    state: str
    status: str
    ports: str
    restart_count: int
    started_at: datetime | None
    log_path: str

    @property
    def is_restart_loop(self) -> bool:
        if self.restart_count <= RESTART_LOOP_THRESHOLD:
            return False
        if self.started_at is None:
            return False
        now = datetime.now(timezone.utc)
        return now - self.started_at <= timedelta(seconds=RESTART_LOOP_WINDOW_S)


def list_containers() -> list[Container]:
    rows = _run_ps()
    out: list[Container] = []
    for row in rows:
        details = _run_inspect(row["ID"])
        out.append(
            Container(
                id=row["ID"],
                name=row.get("Names", ""),
                image=row.get("Image", ""),
                state=row.get("State", ""),
                status=row.get("Status", ""),
                ports=row.get("Ports", ""),
                restart_count=details.get("restart_count", 0),
                started_at=_parse_iso(details.get("started_at")),
                log_path=details.get("log_path", ""),
            )
        )
    return out


def parse_ps_json_lines(text: str) -> list[dict]:
    """Parse `docker ps -a --format '{{json .}}'` output (one JSON object per line)."""
    rows: list[dict] = []
    for line in text.splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            rows.append(json.loads(line))
        except json.JSONDecodeError:
            continue
    return rows


def parse_inspect_fields(text: str) -> dict:
    """Parse the pipe-delimited inspect format we emit.

    Format: `<RestartCount>|<LogPath>|<StartedAt>|<State.Status>`
    """
    parts = text.strip().split("|")
    if len(parts) < 4:
        return {"restart_count": 0, "log_path": "", "started_at": None, "status": ""}
    try:
        restart_count = int(parts[0])
    except ValueError:
        restart_count = 0
    return {
        "restart_count": restart_count,
        "log_path": parts[1],
        "started_at": parts[2],
        "status": parts[3],
    }


def _parse_iso(value: str | None) -> datetime | None:
    if not value:
        return None
    # Docker uses ".StartedAt" with sub-second precision and a "Z" suffix.
    # Treat the zero value as "never started".
    if value.startswith("0001-01-01"):
        return None
    try:
        # Python 3.11+ handles trailing 'Z' natively.
        return datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None


def _run_ps() -> list[dict]:
    try:
        result = subprocess.run(
            ["docker", "ps", "-a", "--format", "{{json .}}"],
            capture_output=True,
            text=True,
            timeout=10,
            check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return []
    if result.returncode != 0:
        return []
    return parse_ps_json_lines(result.stdout)


def _run_inspect(container_id: str) -> dict:
    fmt = "{{.RestartCount}}|{{.LogPath}}|{{.State.StartedAt}}|{{.State.Status}}"
    try:
        result = subprocess.run(
            ["docker", "inspect", "--format", fmt, container_id],
            capture_output=True,
            text=True,
            timeout=5,
            check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return {"restart_count": 0, "log_path": "", "started_at": None, "status": ""}
    if result.returncode != 0:
        return {"restart_count": 0, "log_path": "", "started_at": None, "status": ""}
    return parse_inspect_fields(result.stdout)
