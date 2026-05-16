"""System-path blacklist for destructive actions.

Every delete in FleetFix runs the target through `is_blacklisted()` before
the confirm modal even renders. The blacklist is hard-coded — not config
file driven — because the worst-case mistake is a tech accidentally
including `/etc` in a config-driven allowlist.

Resolves the path (follows symlinks) so `~/oops -> /etc/passwd` can't slip
through. `..` segments are handled by `Path.resolve(strict=False)`.
"""

from __future__ import annotations

from collections.abc import Iterable
from pathlib import Path

from fleetfix.config import SYSTEM_BLACKLIST


class BlacklistedPath(ValueError):
    """Raised when a destructive action targets a protected path."""

    def __init__(self, target: Path, matched: str) -> None:
        super().__init__(f"refusing to touch {target}: under blacklisted path {matched}")
        self.target = target
        self.matched = matched


def is_blacklisted(
    target: Path,
    *,
    extra: Iterable[str] = (),
) -> str | None:
    """Return the matched blacklist prefix if `target` is protected, else None.

    Compares against the resolved absolute path so symlinks pointing into
    `/etc` or similar are rejected. Membership test is done as path
    components, so `/etc-prod` does NOT match `/etc`.
    """
    resolved = _resolve(target)
    candidates = tuple(SYSTEM_BLACKLIST) + tuple(extra)
    for prefix in candidates:
        if _under(resolved, Path(prefix)):
            return prefix
    return None


def refuse_if_blacklisted(target: Path, *, extra: Iterable[str] = ()) -> None:
    """Raise `BlacklistedPath` if the target is protected. Otherwise no-op."""
    matched = is_blacklisted(target, extra=extra)
    if matched is not None:
        raise BlacklistedPath(target, matched)


def _resolve(target: Path) -> Path:
    try:
        return target.resolve(strict=False)
    except (OSError, RuntimeError):
        # RuntimeError happens on infinite symlink loops; treat as the
        # un-resolved absolute form, which is still safe to compare.
        return target.absolute()


def _under(target: Path, prefix: Path) -> bool:
    """True if `target` is equal to or contained inside `prefix`, by path parts."""
    target_parts = target.parts
    prefix_parts = prefix.parts
    # Filesystem root is its own special case: only a literal "/" target counts —
    # otherwise the "/" entry would match every absolute path and shadow the
    # finer-grained entries below it.
    if prefix_parts == ("/",):
        return target_parts == ("/",)
    if len(target_parts) < len(prefix_parts):
        return False
    return target_parts[: len(prefix_parts)] == prefix_parts
