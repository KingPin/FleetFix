"""Headless update flow for ``fleetfix --update`` — upgrade without the TUI.

Mirrors the in-app updater (fresh release check, sha256-verified swap,
audit-wrapped) but runs from the shell. ``--force``/``-y`` skips the
confirmation prompt so ansible/CI can upgrade unattended; without it, a
non-interactive run (no TTY) safely declines rather than hanging.
"""

from __future__ import annotations

from collections.abc import Callable

from fleetfix import __version__
from fleetfix.audit.logger import AuditLogger, Operator
from fleetfix.config import resolve_audit_path
from fleetfix.updater.checker import ReleaseInfo, check_for_update
from fleetfix.updater.installer import (
    InstallResult,
    apply_update,
    can_write_directly,
    resolve_install_target,
)

Printer = Callable[[str], None]
Confirmer = Callable[[ReleaseInfo], bool]
Checker = Callable[..., ReleaseInfo | None]
Applier = Callable[..., InstallResult]


def _render_notes(release: ReleaseInfo, current: str, target: object, needs_sudo: bool) -> str:
    sudo_note = (
        f"sudo required to write {target}" if needs_sudo else f"{target} is writable (no sudo)"
    )
    body = release.body.strip() or "(no release notes provided)"
    return (
        f"FleetFix update available: {current} -> {release.version}\n"
        f"  Target: {target} ({sudo_note})\n"
        f"  Release: {release.html_url}\n\n"
        f"Release notes:\n{body}\n"
    )


def _prompt_yes(release: ReleaseInfo) -> bool:
    try:
        answer = input(f"Apply update to {release.tag}? [y/N] ").strip().lower()
    except EOFError:
        # No TTY and no --force: decline rather than block an unattended run.
        return False
    return answer in {"y", "yes"}


def run_update(
    *,
    force: bool = False,
    out: Printer = print,
    confirm: Confirmer = _prompt_yes,
    check: Checker = check_for_update,
    apply: Applier = apply_update,
    current_version: str = __version__,
) -> int:
    """Run the headless update. Returns a process exit code.

    0 = up to date or applied successfully; 1 = declined or failed.
    """
    # Explicit --update always checks fresh — never trust the 1h launch cache.
    release = check(current_version, cache_ttl_s=0)
    if release is None:
        out(f"fleetfix {current_version} is already up to date.")
        return 0

    target = resolve_install_target()
    out(_render_notes(release, current_version, target, needs_sudo=not can_write_directly(target)))

    if not force and not confirm(release):
        out("Update cancelled.")
        return 1

    audit = AuditLogger(resolve_audit_path(), operator=Operator.from_environment())
    result = apply(release, audit=audit, target=target)
    if result.ok:
        out(f"Updated to {release.tag}. Restart FleetFix to use the new version.")
        return 0
    out(f"Update failed: {result.error}")
    return 1
