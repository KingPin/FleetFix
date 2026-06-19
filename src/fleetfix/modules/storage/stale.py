"""Find stale database dumps, archives, and rotated logs.

Pure-logic scanner: takes a root directory + age threshold, returns a list
of candidates sorted largest-first. No deletion happens here — the result
is fed into the storage UI, which routes any actual delete through the
confirm modal in `screens/confirm.py`.
"""

from __future__ import annotations

import fnmatch
import os
from collections.abc import Iterable
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path

DEFAULT_STALE_AGE_DAYS = 30

# Database dump / archive artifacts that often pile up in user homes
# during debugging and rarely get cleaned afterwards.
STALE_ARTIFACT_GLOBS: tuple[str, ...] = (
    "*.sql",
    "*.sql.gz",
    "*.sql.xz",
    "*.dump",
    "*.dump.gz",
    "*.bak",
    "*.tar.gz",
    "*.tgz",
    "*.zip",
)

# Logrotate / journald leftovers a user can write under their own home
# (and elsewhere). Production system logs live under /var/log and
# aren't scanned by Tier 1.
LEGACY_LOG_GLOBS: tuple[str, ...] = (
    "*.log.[0-9]",
    "*.log.[0-9][0-9]",
    "*.log.gz",
    "*.log.[0-9]*.gz",
    "*.log.old",
)

# Directory names we never descend into: package caches, VCS internals, and
# virtualenvs. They hold high-churn machinery, not the DB dumps / archives
# this scanner targets, and walking them is what makes a full-home scan crawl
# (tens of thousands of files under ~/.cache and node_modules alone).
PRUNE_DIR_NAMES: frozenset[str] = frozenset(
    {
        ".git",
        ".hg",
        ".svn",
        "__pycache__",
        ".venv",
        "venv",
        ".tox",
        ".mypy_cache",
        ".pytest_cache",
        ".ruff_cache",
        "node_modules",
        ".cache",
        ".npm",
        ".cargo",
        ".rustup",
        ".gradle",
        ".m2",
    }
)


@dataclass(frozen=True)
class StaleCandidate:
    path: Path
    size_bytes: int
    mtime_epoch: float
    category: str  # "artifact" | "log"

    @property
    def age_days(self) -> float:
        return (datetime.now(tz=timezone.utc).timestamp() - self.mtime_epoch) / 86400.0


def find_stale(
    root: Path,
    *,
    older_than_days: int = DEFAULT_STALE_AGE_DAYS,
    artifact_globs: Iterable[str] = STALE_ARTIFACT_GLOBS,
    log_globs: Iterable[str] = LEGACY_LOG_GLOBS,
    follow_symlinks: bool = False,
    prune_dirs: Iterable[str] = PRUNE_DIR_NAMES,
) -> list[StaleCandidate]:
    """Walk `root` and collect files matching the glob lists older than the cutoff.

    Returns candidates sorted by size descending — biggest wins are first.
    Permission errors mid-walk are swallowed (we'd rather show partial
    results than abort the scan). Directory names in `prune_dirs` are skipped
    entirely so the walk doesn't disappear into package caches and virtualenvs.
    """
    if not root.exists():
        return []

    cutoff = datetime.now(tz=timezone.utc).timestamp() - older_than_days * 86400
    artifact_set = tuple(artifact_globs)
    log_set = tuple(log_globs)
    prune_set = frozenset(prune_dirs)
    out: list[StaleCandidate] = []

    for dirpath, dirnames, filenames in os.walk(
        root, followlinks=follow_symlinks, onerror=lambda _e: None
    ):
        # Prune in place so os.walk never descends into the skipped dirs.
        dirnames[:] = [d for d in dirnames if d not in prune_set]
        for name in filenames:
            category = _classify(name, artifact_set, log_set)
            if category is None:
                continue
            full = Path(dirpath) / name
            try:
                stat = full.stat(follow_symlinks=False)
            except OSError:
                continue
            if stat.st_mtime > cutoff:
                continue
            out.append(
                StaleCandidate(
                    path=full,
                    size_bytes=stat.st_size,
                    mtime_epoch=stat.st_mtime,
                    category=category,
                )
            )

    out.sort(key=lambda c: c.size_bytes, reverse=True)
    return out


def _classify(name: str, artifact_globs: tuple[str, ...], log_globs: tuple[str, ...]) -> str | None:
    """Return 'artifact', 'log', or None based on which glob set matches."""
    if any(fnmatch.fnmatchcase(name, pattern) for pattern in artifact_globs):
        return "artifact"
    if any(fnmatch.fnmatchcase(name, pattern) for pattern in log_globs):
        return "log"
    return None
