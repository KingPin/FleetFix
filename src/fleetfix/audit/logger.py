"""Local JSON-lines audit log.

Schema per line:
    {
      "ts": "2026-05-16T10:32:11.482Z",
      "host": "<hostname>",
      "session_id": "<uuid4-per-app-run>",
      "call_id": "<uuid4-per-action>",
      "seq": 1,
      "phase": "intent" | "result",
      "operator": {"unix_user": ..., "duo_principal": ..., "source_ip": ...},
      "action": "docker.truncate_log",
      "target": {...},          # action-specific payload
      "result": {"ok": true, "error": null, ...} | null,
      "fleetfix_version": "0.1.0"
    }

`intent` is written *before* the action runs, `result` after, sharing the
same call_id. If the process crashes mid-action, the intent line still
documents what was attempted.
"""

from __future__ import annotations

import json
import os
import platform
import threading
import uuid
from collections.abc import Iterator
from contextlib import contextmanager
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from fleetfix import __version__


def _utcnow_iso() -> str:
    """ISO 8601 UTC timestamp with millisecond precision."""
    now = datetime.now(tz=timezone.utc)
    return now.strftime("%Y-%m-%dT%H:%M:%S.") + f"{now.microsecond // 1000:03d}Z"


@dataclass(frozen=True)
class Operator:
    unix_user: str
    duo_principal: str | None = None
    source_ip: str | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "unix_user": self.unix_user,
            "duo_principal": self.duo_principal,
            "source_ip": self.source_ip,
        }

    @classmethod
    def from_environment(cls) -> Operator:
        unix_user = os.environ.get("SUDO_USER") or os.environ.get("USER") or "unknown"
        source_ip = _parse_ssh_source_ip(os.environ.get("SSH_CONNECTION"))
        return cls(unix_user=unix_user, duo_principal=None, source_ip=source_ip)


def _parse_ssh_source_ip(ssh_connection: str | None) -> str | None:
    """SSH_CONNECTION is '<client_ip> <client_port> <server_ip> <server_port>'."""
    if not ssh_connection:
        return None
    parts = ssh_connection.split()
    if not parts:
        return None
    return parts[0]


@dataclass
class AuditCall:
    """Handle for one in-flight action — emits the paired result on close."""

    logger: AuditLogger
    call_id: str
    action: str
    target: dict[str, Any]
    _result: dict[str, Any] = field(default_factory=dict)
    _closed: bool = False

    def set_result(self, **fields: Any) -> None:
        """Stash result fields to be flushed when the call ends."""
        self._result.update(fields)

    def _emit_result(self, *, ok: bool, error: str | None) -> None:
        if self._closed:
            return
        self._closed = True
        payload = {"ok": ok, "error": error, **self._result}
        self.logger._write(
            phase="result",
            action=self.action,
            target=self.target,
            result=payload,
            call_id=self.call_id,
        )


class AuditLogger:
    """Append-only JSON-lines writer for the local audit trail."""

    def __init__(
        self,
        path: Path,
        *,
        operator: Operator,
        host: str | None = None,
        session_id: str | None = None,
    ) -> None:
        self.path = path
        self.operator = operator
        self.host = host or platform.node()
        self.session_id = session_id or str(uuid.uuid4())
        self._seq = 0
        self._lock = threading.Lock()
        self.path.parent.mkdir(parents=True, exist_ok=True)
        # Touch the file so the first write doesn't fail on permissions check.
        self.path.touch(exist_ok=True)

    @contextmanager
    def action(
        self,
        action: str,
        *,
        target: dict[str, Any] | None = None,
    ) -> Iterator[AuditCall]:
        """Wrap a destructive action — writes intent now, result on exit.

        If the wrapped block raises, the result line records ok=False with
        the exception message before the exception propagates.
        """
        call_id = str(uuid.uuid4())
        target = target or {}
        self._write(
            phase="intent",
            action=action,
            target=target,
            result=None,
            call_id=call_id,
        )
        call = AuditCall(logger=self, call_id=call_id, action=action, target=target)
        try:
            yield call
        except BaseException as exc:
            call._emit_result(ok=False, error=f"{type(exc).__name__}: {exc}")
            raise
        else:
            call._emit_result(ok=True, error=None)

    def event(self, action: str, **target: Any) -> None:
        """One-shot audit record without intent/result pairing (launch/exit/etc.)."""
        call_id = str(uuid.uuid4())
        self._write(
            phase="event",
            action=action,
            target=target,
            result=None,
            call_id=call_id,
        )

    def _write(
        self,
        *,
        phase: str,
        action: str,
        target: dict[str, Any],
        result: dict[str, Any] | None,
        call_id: str,
    ) -> None:
        with self._lock:
            self._seq += 1
            record = {
                "ts": _utcnow_iso(),
                "host": self.host,
                "session_id": self.session_id,
                "call_id": call_id,
                "seq": self._seq,
                "phase": phase,
                "operator": self.operator.to_dict(),
                "action": action,
                "target": target,
                "result": result,
                "fleetfix_version": __version__,
            }
            with self.path.open("a", encoding="utf-8") as fh:
                fh.write(json.dumps(record, ensure_ascii=False) + "\n")


def read_recent(path: Path, *, limit: int = 200) -> list[dict[str, Any]]:
    """Tail the last `limit` records, oldest-first. Skips malformed lines."""
    if not path.exists():
        return []
    lines = path.read_text(encoding="utf-8").splitlines()
    out: list[dict[str, Any]] = []
    for line in lines[-limit:]:
        line = line.strip()
        if not line:
            continue
        try:
            out.append(json.loads(line))
        except json.JSONDecodeError:
            continue
    return out
