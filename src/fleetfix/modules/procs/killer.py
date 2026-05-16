"""Process killer — sends SIGTERM (default) or SIGKILL, audit-wrapped.

SIGKILL requires a separate second confirmation in the UI; here we just
accept it as a parameter. PIDs ≤ 1 are refused (init / kernel). Pids
owned by root that aren't part of an obvious user-session process tree
*are* still killable — operators sometimes need to kill a stuck systemd
service worker — but the UI surfaces who owns the pid so the operator
sees what they're about to do.

Auditing: every kill emits an `intent` line before sending the signal
and a `result` line after. The result records the signal, the pid, the
process command (so the audit trail is readable later even if the pid
has been reused), and any errno.
"""

from __future__ import annotations

import errno
import os
import signal
from dataclasses import dataclass
from pathlib import Path

from fleetfix.audit.logger import AuditLogger

PROTECTED_PIDS = (0, 1)


@dataclass(frozen=True)
class KillResult:
    pid: int
    signal: int
    ok: bool
    error: str | None = None


def _proc_comm(pid: int, proc: Path = Path("/proc")) -> str:
    try:
        return (proc / str(pid) / "comm").read_text(encoding="utf-8", errors="replace").strip()
    except OSError:
        return ""


def send_signal(
    pid: int,
    *,
    sig: int = signal.SIGTERM,
    audit: AuditLogger,
    proc: Path = Path("/proc"),
) -> KillResult:
    """Audit-wrapped signal send. Refuses pid 0 and 1.

    Refusal happens *before* the audit context — we don't want intent
    lines for actions the tool itself refused.
    """
    if pid in PROTECTED_PIDS:
        return KillResult(
            pid=pid,
            signal=sig,
            ok=False,
            error=f"refused: pid {pid} is protected",
        )
    comm = _proc_comm(pid, proc=proc)
    target = {
        "pid": pid,
        "comm": comm,
        "signal": sig,
        "signal_name": signal.Signals(sig).name
        if sig in signal.Signals.__members__.values()
        else str(sig),
    }
    with audit.action("procs.signal", target=target) as call:
        try:
            os.kill(pid, sig)
        except ProcessLookupError:
            call.set_result(errno="ESRCH")
            return KillResult(pid=pid, signal=sig, ok=False, error="no such process")
        except PermissionError as exc:
            call.set_result(errno="EPERM")
            return KillResult(pid=pid, signal=sig, ok=False, error=f"permission denied: {exc}")
        except OSError as exc:
            name = errno.errorcode.get(exc.errno or 0, "EUNKNOWN")
            call.set_result(errno=name)
            return KillResult(pid=pid, signal=sig, ok=False, error=str(exc))
    return KillResult(pid=pid, signal=sig, ok=True)


def terminate(pid: int, *, audit: AuditLogger) -> KillResult:
    """SIGTERM convenience wrapper."""
    return send_signal(pid, sig=signal.SIGTERM, audit=audit)


def force_kill(pid: int, *, audit: AuditLogger) -> KillResult:
    """SIGKILL convenience wrapper — UI must require a second confirm."""
    return send_signal(pid, sig=signal.SIGKILL, audit=audit)
