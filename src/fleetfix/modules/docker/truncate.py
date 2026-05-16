"""Truncate a container's json-log file in place.

Docker writes per-container logs to `/var/lib/docker/containers/<id>/<id>-json.log`.
Truncating that file with `truncate -s 0` reclaims the space *without*
restarting the container — the next log line continues at offset 0. Doing
this requires root, so we shell out to `sudo truncate`.

The flow is:
  1. resolve the log path via `docker inspect` (defensive: never trust a
     caller-supplied path)
  2. stat the file to capture the byte count we're about to free
  3. wrap the truncate call in an audit-logged action so intent + result
     pair up in the audit log
"""

from __future__ import annotations

import os
import subprocess
from dataclasses import dataclass
from pathlib import Path

from fleetfix.audit.logger import AuditLogger


@dataclass(frozen=True)
class TruncateResult:
    container_id: str
    log_path: str
    bytes_freed: int
    ok: bool
    error: str | None = None


def resolve_log_path(container_id: str) -> str | None:
    """Ask `docker inspect` where the container's json-log lives."""
    try:
        result = subprocess.run(
            ["docker", "inspect", "--format", "{{.LogPath}}", container_id],
            capture_output=True,
            text=True,
            timeout=5,
            check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return None
    if result.returncode != 0:
        return None
    path = result.stdout.strip()
    return path or None


def truncate_log(
    container_id: str,
    *,
    audit: AuditLogger,
    container_name: str = "",
) -> TruncateResult:
    """Truncate the json-log for `container_id`, recording intent + result."""
    log_path = resolve_log_path(container_id)
    if not log_path:
        return TruncateResult(
            container_id=container_id,
            log_path="",
            bytes_freed=0,
            ok=False,
            error="could not resolve container log path",
        )

    path = Path(log_path)
    size_before = _stat_size(path)

    target = {
        "container_id": container_id,
        "container_name": container_name,
        "log_path": log_path,
        "size_before": size_before,
    }

    with audit.action("docker.truncate_log", target=target) as call:
        error = _run_truncate(log_path)
        if error is not None:
            call.set_result(ok=False, error=error)
            return TruncateResult(
                container_id=container_id,
                log_path=log_path,
                bytes_freed=0,
                ok=False,
                error=error,
            )
        size_after = _stat_size(path)
        bytes_freed = max(0, size_before - size_after)
        call.set_result(bytes_freed=bytes_freed)

    return TruncateResult(
        container_id=container_id,
        log_path=log_path,
        bytes_freed=bytes_freed,
        ok=True,
    )


def _run_truncate(log_path: str) -> str | None:
    """Run `sudo truncate -s 0 <log_path>`. Return error string or None on success."""
    try:
        subprocess.run(
            ["sudo", "-n", "truncate", "-s", "0", log_path],
            capture_output=True,
            text=True,
            timeout=5,
            check=True,
        )
    except FileNotFoundError:
        return "truncate not found"
    except subprocess.TimeoutExpired:
        return "truncate timed out"
    except subprocess.CalledProcessError as exc:
        return (exc.stderr or "").strip() or "truncate failed"
    return None


def _stat_size(path: Path) -> int:
    try:
        return os.stat(path).st_size
    except OSError:
        return 0
