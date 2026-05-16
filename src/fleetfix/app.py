"""Textual App root.

Stub for milestone 1 — full UI lands in milestone 2. Importing this module
must not raise on a headless system.
"""

from __future__ import annotations

from dataclasses import dataclass

from fleetfix import __version__
from fleetfix.config import detect_host
from fleetfix.privilege import detect as detect_privilege


@dataclass(frozen=True)
class AppContext:
    version: str
    read_only: bool


class FleetFixApp:
    """Placeholder app shell — replaced by a real Textual `App` in M2."""

    def __init__(self, *, read_only: bool = False) -> None:
        self.ctx = AppContext(version=__version__, read_only=read_only)
        self.host = detect_host()
        self.privilege = detect_privilege()

    def run(self) -> None:
        # M2 will replace this with `textual.app.App.run`.
        banner = (
            f"FleetFix {self.ctx.version} on {self.host.hostname} "
            f"({self.host.os_pretty} / {self.host.kernel})"
        )
        privilege = "root" if self.privilege.is_root else (
            "sudo-ready" if self.privilege.can_tier2 else "tier1-only"
        )
        mode = "read-only" if self.ctx.read_only else "normal"
        print(banner)
        print(f"  privilege: {privilege}  mode: {mode}")
        print("  (Textual UI lands in milestone 2)")
