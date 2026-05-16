"""Privilege detection and Tier 2 gating.

We never re-exec the whole app as root — that would lose operator identity
in the audit log. Instead, individual destructive actions wrap the
underlying command with `sudo` per-call.
"""

from __future__ import annotations

import os
import shutil
import subprocess
from dataclasses import dataclass


@dataclass(frozen=True)
class PrivilegeState:
    """Snapshot of the operator's current escalation status."""

    is_root: bool
    sudo_available: bool
    passwordless_sudo: bool

    @property
    def can_tier2(self) -> bool:
        """True if Tier 2 actions are reachable without further setup."""
        return self.is_root or self.sudo_available


def detect() -> PrivilegeState:
    is_root = os.geteuid() == 0
    sudo_bin = shutil.which("sudo")

    if is_root:
        return PrivilegeState(is_root=True, sudo_available=True, passwordless_sudo=True)

    if sudo_bin is None:
        return PrivilegeState(is_root=False, sudo_available=False, passwordless_sudo=False)

    passwordless = _check_passwordless_sudo()
    return PrivilegeState(
        is_root=False,
        sudo_available=True,
        passwordless_sudo=passwordless,
    )


def _check_passwordless_sudo() -> bool:
    try:
        result = subprocess.run(
            ["sudo", "-n", "true"],
            check=False,
            capture_output=True,
            timeout=2,
        )
        return result.returncode == 0
    except (subprocess.TimeoutExpired, OSError):
        return False


def refresh_sudo_credential() -> bool:
    """Prompt for sudo password (or refresh the cached credential).

    Returns True on success. Callers should run this once at the start of a
    Tier 2 session, then periodically (every ~4 minutes) to keep the
    credential alive without re-prompting mid-workflow.
    """
    try:
        result = subprocess.run(["sudo", "-v"], check=False, timeout=60)
        return result.returncode == 0
    except (subprocess.TimeoutExpired, OSError):
        return False


def refresh_sudo_credential_quietly() -> bool:
    """Non-interactive sudo keepalive — `sudo -nv`. Never prompts.

    Use for the background keepalive timer. If the cached credential has
    expired, returns False; the caller should escalate by switching to the
    interactive refresh_sudo_credential() flow.
    """
    try:
        result = subprocess.run(
            ["sudo", "-n", "-v"],
            check=False,
            capture_output=True,
            timeout=2,
        )
        return result.returncode == 0
    except (subprocess.TimeoutExpired, OSError):
        return False


# How often to refresh the cached sudo credential, in seconds. Sudo's default
# timestamp_timeout is 5 minutes, so 4 keeps us comfortably under the wire.
SUDO_KEEPALIVE_INTERVAL = 4 * 60
