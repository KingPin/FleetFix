"""Paths, constants, and host-environment detection."""

from __future__ import annotations

import logging
import os
import platform
import pwd
import shutil
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import yaml

_log = logging.getLogger(__name__)

AUDIT_LOG_PATH = Path("/var/log/fleetfix-audit.log")
USER_CONFIG_DIR = Path(os.environ.get("XDG_CONFIG_HOME", Path.home() / ".config")) / "fleetfix"
USER_CACHE_DIR = Path(os.environ.get("XDG_CACHE_HOME", Path.home() / ".cache")) / "fleetfix"
USER_STATE_DIR = (
    Path(os.environ.get("XDG_STATE_HOME", Path.home() / ".local" / "state")) / "fleetfix"
)

PATHS_CONFIG_PATH = USER_CONFIG_DIR / "paths.yml"


def resolve_audit_path() -> Path:
    """Pick a writable audit log path.

    Production: /var/log/fleetfix-audit.log (writable as root, group adm
    readable). Development / non-privileged: fall back to the XDG state
    dir so the app still produces an audit trail under the invoking user.
    """
    primary = AUDIT_LOG_PATH
    try:
        primary.parent.mkdir(parents=True, exist_ok=True)
        primary.touch(exist_ok=True)
        return primary
    except (OSError, PermissionError):
        fallback = USER_STATE_DIR / "audit.log"
        fallback.parent.mkdir(parents=True, exist_ok=True)
        return fallback


GITHUB_RELEASES_URL = "https://api.github.com/repos/KingPin/FleetFix/releases/latest"
DEFAULT_BINARY_PATH = Path("/usr/local/bin/fleetfix")

SYSTEM_BLACKLIST = (
    "/",
    "/boot",
    "/etc",
    "/usr",
    "/lib",
    "/lib32",
    "/lib64",
    "/libx32",
    "/sbin",
    "/bin",
    "/var/lib/dpkg",
    "/var/lib/apt",
    "/proc",
    "/sys",
    "/dev",
)


@dataclass(frozen=True)
class HostInfo:
    hostname: str
    os_pretty: str
    kernel: str
    arch: str
    has_systemd: bool
    has_docker: bool


def detect_host() -> HostInfo:
    os_pretty = "Unknown Linux"
    try:
        for line in Path("/etc/os-release").read_text().splitlines():
            if line.startswith("PRETTY_NAME="):
                os_pretty = line.split("=", 1)[1].strip().strip('"')
                break
    except OSError:
        pass

    return HostInfo(
        hostname=platform.node(),
        os_pretty=os_pretty,
        kernel=platform.release(),
        arch=platform.machine(),
        has_systemd=Path("/run/systemd/system").exists(),
        has_docker=shutil.which("docker") is not None,
    )


@dataclass(frozen=True)
class InspectTarget:
    """User whose footprint is being inspected (decoupled from the operator)."""

    user: str
    home: Path
    uid: int


def read_paths_yaml(path: Path) -> dict[str, Any]:
    try:
        text = path.read_text(encoding="utf-8")
    except OSError:
        return {}
    try:
        data = yaml.safe_load(text)
    except yaml.YAMLError as exc:
        _log.warning("failed to parse %s: %s", path, exc)
        return {}
    return data if isinstance(data, dict) else {}


def resolve_inspect_target(
    *, cli_user: str | None, paths_cfg: dict[str, Any]
) -> InspectTarget | None:
    """Pick the inspect-target user from CLI flag, paths.yml, or leave unset.

    An empty CLI string ("") is an explicit clear that overrides paths.yml.
    Unknown users log a warning and return None — we never silently substitute.
    """
    if cli_user == "":
        return None
    name = cli_user if cli_user else paths_cfg.get("target_user")
    if not name:
        return None
    try:
        entry = pwd.getpwnam(name)
    except KeyError:
        _log.warning("inspect target %r does not exist on this host", name)
        return None
    return InspectTarget(user=name, home=Path(entry.pw_dir), uid=entry.pw_uid)
