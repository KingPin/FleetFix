"""Shared base for views whose scan is deferred until they are first shown."""

from __future__ import annotations

from textual import events
from textual.widget import Widget


class LazyScanView(Widget):
    """A view that defers its first scan until it actually becomes visible.

    Every view is composed eagerly into the app's ``ContentSwitcher`` up front,
    so mounting one mounts them all. Scanning from ``on_mount`` therefore fired
    *every* view's workers at launch — a thundering herd that saturated a single
    core (and the GIL) and froze the UI for seconds on anemic hosts (issue #2).

    Instead, a view scans once, the first time it becomes the current pane, via
    :meth:`ensure_initial_scan`. That runs from whichever comes first — the
    ``ContentSwitcher`` posting :class:`~textual.events.Show` when this becomes
    current, or :meth:`fleetfix.app.FleetFixApp.action_switch` calling it
    directly on switch. Both are guarded by one flag, so the scan happens exactly
    once; revisits stay instant (each view already has a manual Refresh control).

    Subclasses implement :meth:`start_initial_scan`. Views with periodic
    refreshes additionally override ``on_show``/``on_hide`` (calling ``super()``)
    to pause that work while hidden — a hidden pane must never keep spending the
    event loop.
    """

    _initial_scan_done: bool = False

    def start_initial_scan(self) -> None:
        """Run this view's first data load. Subclasses override."""
        raise NotImplementedError

    def ensure_initial_scan(self) -> None:
        """Trigger the first scan exactly once; a no-op on every later call."""
        if self._initial_scan_done:
            return
        self._initial_scan_done = True
        self.start_initial_scan()

    def on_show(self, event: events.Show) -> None:
        self.ensure_initial_scan()
