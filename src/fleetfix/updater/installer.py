"""In-app updater — downloads, verifies, and atomically swaps the binary.

Flow:
  1. Stream the new binary to ``/tmp/fleetfix.<ver>.new``.
  2. Fetch the matching ``.sha256`` line and verify the digest.
  3. Swap it over the *running* binary (``resolve_install_target()``):
       * if its directory is user-writable (e.g. ``~/bin/fleetfix``),
         copy + atomic ``os.replace`` with NO sudo;
       * otherwise (e.g. root-owned ``/usr/local/bin``), fall back to
         ``sudo -n install`` + ``sudo -n mv``.
  4. The user restarts FleetFix manually — we never auto-relaunch mid-triage.

Every step that touches the filesystem or runs sudo is audit-wrapped.
"""

from __future__ import annotations

import hashlib
import logging
import os
import shutil
import subprocess
import sys
import tempfile
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path

import httpx

from fleetfix.audit.logger import AuditLogger
from fleetfix.config import DEFAULT_BINARY_PATH
from fleetfix.updater.checker import ReleaseInfo

_log = logging.getLogger(__name__)

HTTP_TIMEOUT_S = 30.0
DOWNLOAD_CHUNK = 64 * 1024


@dataclass(frozen=True)
class InstallResult:
    ok: bool
    version: str
    target: Path
    error: str | None = None


Downloader = Callable[[str, Path], None]


def _default_download(url: str, dest: Path) -> None:
    dest.parent.mkdir(parents=True, exist_ok=True)
    with (
        httpx.Client(timeout=HTTP_TIMEOUT_S, follow_redirects=True) as client,
        client.stream("GET", url) as resp,
    ):
        resp.raise_for_status()
        with dest.open("wb") as fh:
            for chunk in resp.iter_bytes(DOWNLOAD_CHUNK):
                fh.write(chunk)


def _default_fetch_text(url: str) -> str:
    with httpx.Client(timeout=HTTP_TIMEOUT_S, follow_redirects=True) as client:
        resp = client.get(url)
    resp.raise_for_status()
    return resp.text


def parse_sha256_line(text: str, *, asset_name: str) -> str | None:
    """Pull the digest for ``asset_name`` from a ``sha256sum``-style file."""
    for line in text.splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        parts = line.split(maxsplit=1)
        if len(parts) != 2:
            continue
        digest, name = parts[0], parts[1].lstrip("*")
        if name == asset_name:
            return digest.lower()
    return None


def sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as fh:
        for chunk in iter(lambda: fh.read(DOWNLOAD_CHUNK), b""):
            h.update(chunk)
    return h.hexdigest()


def resolve_install_target() -> Path:
    """Path of the binary to replace.

    When running as a frozen PyInstaller binary, that's the binary the
    operator actually launched — ``~/bin/fleetfix``, ``/usr/local/bin/fleetfix``,
    wherever it lives. Running from source (``python -m fleetfix``),
    ``sys.executable`` is the interpreter, so fall back to the conventional
    system install path.
    """
    if getattr(sys, "frozen", False):
        return Path(sys.executable).resolve()
    return DEFAULT_BINARY_PATH


def can_write_directly(target: Path) -> bool:
    """True if the current user can replace ``target`` without sudo.

    Atomic replacement creates a sibling temp file and renames it over the
    target, so what matters is write access to the *containing directory* —
    not the (possibly root-owned) target file itself.
    """
    parent = target.parent
    return parent.is_dir() and os.access(parent, os.W_OK)


def _swap_in_place(staged: Path, target: Path) -> str | None:
    """Install without privilege escalation: copy into the target dir, then
    atomically rename over the target. Returns an error str or None."""
    staging_path = target.with_name(target.name + ".new")
    try:
        shutil.copy2(staged, staging_path)
        # 0o755: a shipped binary must be world-executable, same as the
        # `install -m 0755` the sudo path uses.
        os.chmod(staging_path, 0o755)  # noqa: S103
        os.replace(staging_path, target)  # atomic within the same directory
    except OSError as exc:
        _safe_unlink(staging_path)
        return f"in-place install failed: {exc}"
    return None


def _run_install_swap(staged: Path, target: Path) -> str | None:
    """Swap the new binary into place, escalating with sudo only if needed."""
    if can_write_directly(target):
        return _swap_in_place(staged, target)
    return _sudo_install_swap(staged, target)


def _sudo_install_swap(staged: Path, target: Path) -> str | None:
    """Run the privileged install + atomic rename. Returns error str or None."""
    staging_path = target.with_name(target.name + ".new")
    install_cmd = ["sudo", "-n", "install", "-m", "0755", str(staged), str(staging_path)]
    try:
        proc = subprocess.run(install_cmd, capture_output=True, text=True, timeout=15, check=False)
    except FileNotFoundError:
        return "sudo not found"
    except subprocess.TimeoutExpired:
        return "sudo install timed out"
    if proc.returncode != 0:
        return (proc.stderr or "").strip() or f"sudo install exited {proc.returncode}"

    swap_cmd = ["sudo", "-n", "mv", "-f", str(staging_path), str(target)]
    try:
        proc = subprocess.run(swap_cmd, capture_output=True, text=True, timeout=15, check=False)
    except subprocess.TimeoutExpired:
        return "sudo mv timed out"
    if proc.returncode != 0:
        return (proc.stderr or "").strip() or f"sudo mv exited {proc.returncode}"
    return None


def apply_update(
    release: ReleaseInfo,
    *,
    audit: AuditLogger,
    target: Path | None = None,
    asset_name: str = "fleetfix-linux-x86_64",
    staging_dir: Path | None = None,
    download: Downloader | None = None,
    fetch_text: Callable[[str], str] | None = None,
    install_swap: Callable[[Path, Path], str | None] | None = None,
) -> InstallResult:
    """Download, verify, and swap the local binary. Audit-wrapped.

    ``target`` defaults to the running binary (``resolve_install_target()``) so
    an update lands on whatever path FleetFix was launched from, not a
    hardcoded system path.
    """
    target = target if target is not None else resolve_install_target()
    do_download = download or _default_download
    do_fetch_text = fetch_text or _default_fetch_text
    do_swap = install_swap or _run_install_swap
    stage_root = staging_dir if staging_dir is not None else Path(tempfile.gettempdir())

    staged = stage_root / f"fleetfix.{release.version}.new"
    target_dict = {
        "version_from": "unknown",
        "version_to": release.version,
        "asset_url": release.asset_url,
        "target": str(target),
    }

    with audit.action("updater.apply", target=target_dict) as call:
        try:
            do_download(release.asset_url, staged)
        except Exception as exc:
            err = f"download failed: {exc}"
            call.set_result(ok=False, error=err)
            return InstallResult(ok=False, version=release.version, target=target, error=err)

        try:
            checksum_text = do_fetch_text(release.checksum_url)
        except Exception as exc:
            err = f"checksum fetch failed: {exc}"
            call.set_result(ok=False, error=err)
            _safe_unlink(staged)
            return InstallResult(ok=False, version=release.version, target=target, error=err)

        expected = parse_sha256_line(checksum_text, asset_name=asset_name)
        if expected is None:
            err = f"no digest for {asset_name} in checksum file"
            call.set_result(ok=False, error=err)
            _safe_unlink(staged)
            return InstallResult(ok=False, version=release.version, target=target, error=err)

        actual = sha256_file(staged)
        if actual != expected:
            err = f"sha256 mismatch (expected {expected}, got {actual})"
            call.set_result(ok=False, error=err)
            _safe_unlink(staged)
            return InstallResult(ok=False, version=release.version, target=target, error=err)

        swap_err = do_swap(staged, target)
        if swap_err is not None:
            call.set_result(ok=False, error=swap_err)
            _safe_unlink(staged)
            return InstallResult(ok=False, version=release.version, target=target, error=swap_err)

        call.set_result(bytes_installed=staged.stat().st_size if staged.exists() else None)
        _safe_unlink(staged)
        return InstallResult(ok=True, version=release.version, target=target)


def _safe_unlink(path: Path) -> None:
    try:
        path.unlink(missing_ok=True)
    except OSError:
        _log.debug("could not remove staged file %s", path, exc_info=True)


def have_writable_target(target: Path | None = None) -> bool:
    """Cheap pre-check: can we install to ``target`` at all?

    True if we can write the directory directly, or sudo is available to
    escalate for a root-owned path. ``target`` defaults to the running binary.
    """
    target = target if target is not None else resolve_install_target()
    if can_write_directly(target):
        return True
    return target.parent.is_dir() and shutil.which("sudo") is not None
