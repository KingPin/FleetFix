"""Emergency log squeeze — gzip oversized rotated logs in place.

When `/var/log` is the reason a disk is full, the tech needs to reclaim
space without waiting for the next logrotate run. This module finds large
`*.log.N` / `*.log` files, checks that no process has them open for write
(so we don't corrupt an active write stream), and gzips them in place.

We refuse if `lsof` reports any writer — better to leave space on the floor
than truncate a journal mid-flush.
"""

from __future__ import annotations

import gzip
import logging
import os
import shutil
import subprocess
from collections.abc import Iterable, Iterator
from dataclasses import dataclass
from pathlib import Path

from fleetfix.audit.logger import AuditLogger
from fleetfix.modules.disk.blacklist import refuse_if_blacklisted

_log = logging.getLogger(__name__)

DEFAULT_ROOTS: tuple[Path, ...] = (Path("/var/log"),)
DEFAULT_MIN_BYTES = 10 * 1024 * 1024  # 10 MiB
LSOF_TIMEOUT_S = 5.0


@dataclass(frozen=True)
class Candidate:
    path: Path
    size: int


@dataclass(frozen=True)
class SqueezeResult:
    path: Path
    bytes_before: int
    bytes_after: int
    ok: bool
    error: str | None = None

    @property
    def bytes_freed(self) -> int:
        return max(0, self.bytes_before - self.bytes_after) if self.ok else 0


def find_squeezable_logs(
    roots: Iterable[Path] = DEFAULT_ROOTS,
    *,
    min_bytes: int = DEFAULT_MIN_BYTES,
) -> list[Candidate]:
    """Walk ``roots`` and return uncompressed log files >= ``min_bytes``.

    Skips already-gzipped files, symlinks, and anything we can't stat.
    Returned list is sorted largest-first so the operator triages the
    worst offender first.
    """
    found: list[Candidate] = []
    for root in roots:
        for path in _walk_logs(root):
            try:
                st = path.stat(follow_symlinks=False)
            except OSError:
                continue
            if st.st_size < min_bytes:
                continue
            found.append(Candidate(path=path, size=st.st_size))
    found.sort(key=lambda c: c.size, reverse=True)
    return found


def is_open_for_write(
    path: Path,
    *,
    run: subprocess.CompletedProcess | None = None,
    lsof_cmd: tuple[str, ...] = ("lsof", "-Fan", "--"),
) -> bool:
    """True if any process has ``path`` open with a writable file descriptor.

    Parses ``lsof -Fan`` output: each FD line starts with ``f`` and the
    access mode follows on an ``a`` line. ``w`` and ``u`` (read+write)
    both count as "open for write".

    If ``lsof`` is missing or errors we conservatively return True — the
    safer default is to refuse than to gzip an actively-written log.
    """
    if run is not None:
        proc = run  # injected for tests
    else:
        try:
            proc = subprocess.run(
                [*lsof_cmd, str(path)],
                capture_output=True,
                text=True,
                timeout=LSOF_TIMEOUT_S,
                check=False,
            )
        except FileNotFoundError:
            _log.warning("lsof not available; refusing to squeeze %s", path)
            return True
        except subprocess.TimeoutExpired:
            _log.warning("lsof timed out for %s; refusing to squeeze", path)
            return True

    # lsof exits 1 when no process has the file open — that's a clean "no writer".
    if proc.returncode != 0 and not (proc.stdout or "").strip():
        return False
    return _lsof_has_writer(proc.stdout or "")


def squeeze_log(
    path: Path,
    audit: AuditLogger,
    *,
    chunk_bytes: int = 64 * 1024,
    open_for_write: bool | None = None,
) -> SqueezeResult:
    """Gzip ``path`` in place. Refuses if blacklisted or open for write.

    The write goes to a sibling ``<name>.gz.partial`` then renames atomically
    over the final ``<name>.gz``. The original is unlinked only after the
    rename succeeds, so a crash mid-compression leaves the input intact.
    """
    refuse_if_blacklisted(path)

    target_info = {"path": str(path)}
    with audit.action("log_squeeze.gzip_inplace", target=target_info) as call:
        if not path.is_file() or path.is_symlink():
            err = f"not a regular file: {path}"
            call.set_result(ok=False, error=err)
            return SqueezeResult(path=path, bytes_before=0, bytes_after=0, ok=False, error=err)

        bytes_before = path.stat(follow_symlinks=False).st_size

        held = open_for_write if open_for_write is not None else is_open_for_write(path)
        if held:
            err = "file is open for write by another process"
            call.set_result(ok=False, error=err, bytes_before=bytes_before)
            return SqueezeResult(
                path=path, bytes_before=bytes_before, bytes_after=bytes_before, ok=False, error=err
            )

        final = path.with_name(path.name + ".gz")
        partial = path.with_name(path.name + ".gz.partial")

        try:
            with path.open("rb") as src, gzip.open(partial, "wb") as dst:
                shutil.copyfileobj(src, dst, length=chunk_bytes)
            os.replace(partial, final)
            path.unlink()
        except OSError as exc:
            _safe_unlink(partial)
            err = f"gzip failed: {exc}"
            call.set_result(ok=False, error=err, bytes_before=bytes_before)
            return SqueezeResult(
                path=path, bytes_before=bytes_before, bytes_after=bytes_before, ok=False, error=err
            )

        bytes_after = final.stat().st_size
        call.set_result(
            ok=True,
            bytes_before=bytes_before,
            bytes_after=bytes_after,
            bytes_freed=bytes_before - bytes_after,
            output_path=str(final),
        )
        return SqueezeResult(path=path, bytes_before=bytes_before, bytes_after=bytes_after, ok=True)


def _walk_logs(root: Path) -> Iterator[Path]:
    if not root.exists():
        return
    try:
        entries = list(root.rglob("*"))
    except OSError:
        return
    for entry in entries:
        if entry.is_symlink() or not entry.is_file():
            continue
        name = entry.name
        if name.endswith((".gz", ".xz", ".zst", ".bz2")):
            continue
        # Match active and rotated logs: foo.log, foo.log.1, foo.log.2026-05-16
        if name.endswith(".log"):
            yield entry
            continue
        if ".log." in name and not name.endswith(".partial"):
            yield entry


def _lsof_has_writer(output: str) -> bool:
    for line in output.splitlines():
        if not line:
            continue
        kind, _, value = line[0], line[1:2], line[1:]
        if kind == "a" and any(ch in value for ch in ("w", "u")):
            return True
    return False


def _safe_unlink(path: Path) -> None:
    try:
        path.unlink(missing_ok=True)
    except OSError:
        _log.debug("could not remove partial file %s", path, exc_info=True)


__all__ = (
    "DEFAULT_MIN_BYTES",
    "DEFAULT_ROOTS",
    "Candidate",
    "SqueezeResult",
    "find_squeezable_logs",
    "is_open_for_write",
    "squeeze_log",
)
