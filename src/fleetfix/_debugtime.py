"""Env-gated timing instrumentation for the slow-module-switch investigation.

Kept in-tree as an opt-in diagnostic (issue #2). Entirely gated behind the
``FLEETFIX_TIMING`` env var, so a normal run pays nothing (every hook returns
immediately); enable it only when reproducing a module-switch latency report.

Usage on an affected (anemic-CPU) host:

    FLEETFIX_TIMING=1 \
    FLEETFIX_TIMING_LOG=/tmp/fleetfix-timing.log \
    fleetfix

Then: switch to a module you have NOT visited yet (reproduce the 10-13s
stall), let it finish, switch to a second fresh module, quit, and send back
the log file.

Reading the log (all timestamps are ms since process start, one clock across
every thread, so you subtract two lines to get a span):

  * ``switch.request`` -> ``switch.painted`` for the same key
        = time the event loop spent laying out + painting the new view.
        A big gap here means the cost is Textual render, not the scan.
  * ``worker … state=PENDING`` for a view's group
        = when that view's scan was triggered (i.e. when on_mount fired).
        If every view's worker is PENDING right after ``app.on_mount``,
        scans run at STARTUP, not on first switch.
  * ``state=RUNNING`` -> ``state=SUCCESS`` for the same group
        = wall-clock the scan itself took. Cold-vs-warm shows up here.
"""

from __future__ import annotations

import os
import tempfile
import threading
import time
from pathlib import Path

ENABLED: bool = os.environ.get("FLEETFIX_TIMING") == "1"
_DEFAULT_LOG = Path(tempfile.gettempdir()) / "fleetfix-timing.log"
_LOG_PATH = Path(os.environ.get("FLEETFIX_TIMING_LOG") or _DEFAULT_LOG)
_lock = threading.Lock()
_t0 = time.perf_counter()


def stamp(label: str, **fields: object) -> None:
    """Append one timestamped line to the timing log. No-op unless enabled."""
    if not ENABLED:
        return
    elapsed_ms = (time.perf_counter() - _t0) * 1000.0
    thread = threading.current_thread().name
    extra = " ".join(f"{k}={v}" for k, v in fields.items())
    line = f"{elapsed_ms:11.1f}ms  [{thread:<20}]  {label:<16} {extra}\n"
    with _lock, _LOG_PATH.open("a") as fh:
        fh.write(line)


def install() -> None:
    """Wrap ``Worker.state`` so every worker transition is timed centrally.

    ``Worker.StateChanged`` is declared ``bubble=False``, so an app-level
    message handler never sees it. Patching the one state setter that every
    worker funnels through captures all of them (PENDING/RUNNING/SUCCESS)
    without touching the individual view screens. No-op unless enabled.
    """
    if not ENABLED:
        return
    from typing import Any, cast

    from textual.worker import Worker, WorkerState

    prop = cast(property, Worker.__dict__["state"])
    original_fset = prop.fset
    assert original_fset is not None

    def _timed_setter(worker: Any, state: WorkerState) -> None:
        if state != worker._state:
            stamp("worker", name=worker.name, group=worker.group, state=state.name)
        original_fset(worker, state)

    setattr(Worker, "state", property(prop.fget, _timed_setter))  # noqa: B010
