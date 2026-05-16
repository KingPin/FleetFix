"""Lazy directory tree with per-entry size and mtime.

Used by the storage screen to render an interactive `/home/appuser` browser
without `du`-walking the whole tree up front. The TUI calls `list_dir()`
each time the operator expands a node; we compute size and mtime for the
*immediate* children only and let the user drill in.

For directories, `size_bytes` is the size of the directory inode itself
(typically 4 KiB) — not the recursive total. Recursive sizes are computed
lazily by `summarize_subtree()` on demand, because aggregating /home can
be slow on a busy box.
"""

from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path


@dataclass(frozen=True)
class TreeEntry:
    path: Path
    name: str
    is_dir: bool
    size_bytes: int
    mtime_epoch: float
    is_symlink: bool

    @classmethod
    def from_path(cls, path: Path) -> TreeEntry:
        stat = path.stat(follow_symlinks=False)
        return cls(
            path=path,
            name=path.name,
            is_dir=path.is_dir() and not path.is_symlink(),
            size_bytes=stat.st_size,
            mtime_epoch=stat.st_mtime,
            is_symlink=path.is_symlink(),
        )


def list_dir(path: Path) -> list[TreeEntry]:
    """List the immediate children of `path`, dirs first then files, name-sorted.

    Permission errors on individual entries are skipped silently — better
    to render a partial tree than to abort the screen.
    """
    if not path.is_dir():
        return []
    entries: list[TreeEntry] = []
    try:
        children = sorted(path.iterdir(), key=lambda p: (not p.is_dir(), p.name.lower()))
    except OSError:
        return []
    for child in children:
        try:
            entries.append(TreeEntry.from_path(child))
        except OSError:
            continue
    return entries


def summarize_subtree(root: Path) -> int:
    """Recursive byte total under `root`. Used on-demand from the UI.

    Symlinks are not followed; permission errors and disappearing files
    are skipped. The intent is "rough sense of how big this is", not
    inode-perfect accounting.
    """
    total = 0
    for dirpath, _dirnames, filenames in os.walk(root, followlinks=False, onerror=lambda _e: None):
        for name in filenames:
            try:
                total += (Path(dirpath) / name).stat(follow_symlinks=False).st_size
            except OSError:
                continue
    return total
