"""Safe single-file delete with blacklist guard + audit logging.

Single function: `safe_delete(path, audit, ...)`. It:

1. Resolves and rejects the target if it's under the system blacklist.
2. Refuses to delete directories (Tier 1 is single-file only — bulk
   cleanup is a future feature with its own confirm flow).
3. Records pre-delete size for the audit result.
4. Wraps the actual unlink in the audit logger's `action()` context so
   intent is durable even on crash.

The confirm modal runs upstream of this — by the time `safe_delete()` is
called, the operator has already typed the consent phrase.
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

from fleetfix.audit.logger import AuditLogger
from fleetfix.modules.disk.blacklist import BlacklistedPath, refuse_if_blacklisted


@dataclass(frozen=True)
class DeleteResult:
    path: Path
    bytes_freed: int


class UnsafeDelete(Exception):
    """Raised when the target is a directory, symlink, or otherwise unsafe."""


def safe_delete(path: Path, audit: AuditLogger) -> DeleteResult:
    """Delete a single regular file. Refuses directories and blacklisted paths.

    Returns the bytes freed on success. On failure (blacklist, missing,
    directory, permission, etc.) raises an exception — the audit log
    will already have captured the intent and the failure reason.
    """
    refuse_if_blacklisted(path)  # raises BlacklistedPath before any audit write

    target_info = {"path": str(path)}
    with audit.action("storage.delete_file", target=target_info) as call:
        if not path.exists():
            raise FileNotFoundError(f"target does not exist: {path}")
        if path.is_dir():
            raise UnsafeDelete(f"refusing to delete directory: {path}")
        if path.is_symlink():
            raise UnsafeDelete(f"refusing to delete symlink: {path}")
        size = path.stat(follow_symlinks=False).st_size
        path.unlink()
        call.set_result(bytes_freed=size)
        return DeleteResult(path=path, bytes_freed=size)


__all__ = ("BlacklistedPath", "DeleteResult", "UnsafeDelete", "safe_delete")
