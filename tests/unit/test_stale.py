"""Unit tests for the stale artifact + legacy log finder."""

from __future__ import annotations

import os
import time
from pathlib import Path

from fleetfix.modules.storage.stale import (
    LEGACY_LOG_GLOBS,
    STALE_ARTIFACT_GLOBS,
    find_stale,
)


def _touch(path: Path, *, size: int = 0, age_days: float = 0.0) -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(b"x" * size)
    if age_days:
        past = time.time() - age_days * 86400
        os.utime(path, (past, past))
    return path


def test_find_stale_returns_empty_when_root_missing(tmp_path: Path) -> None:
    assert find_stale(tmp_path / "does-not-exist") == []


def test_find_stale_picks_up_old_sql_dump(tmp_path: Path) -> None:
    target = _touch(tmp_path / "db" / "backup.sql.gz", size=1024, age_days=45)
    results = find_stale(tmp_path, older_than_days=30)
    assert len(results) == 1
    assert results[0].path == target
    assert results[0].category == "artifact"
    assert results[0].size_bytes == 1024
    assert results[0].age_days > 30


def test_find_stale_skips_recent_artifacts(tmp_path: Path) -> None:
    _touch(tmp_path / "recent.sql", size=10, age_days=5)
    assert find_stale(tmp_path, older_than_days=30) == []


def test_find_stale_skips_unmatched_extensions(tmp_path: Path) -> None:
    _touch(tmp_path / "notes.txt", size=10, age_days=45)
    _touch(tmp_path / "video.mp4", size=10, age_days=45)
    assert find_stale(tmp_path, older_than_days=30) == []


def test_find_stale_classifies_rotated_logs(tmp_path: Path) -> None:
    _touch(tmp_path / "app.log.1", size=200, age_days=60)
    _touch(tmp_path / "app.log.2.gz", size=100, age_days=60)
    _touch(tmp_path / "app.log.old", size=50, age_days=60)
    results = find_stale(tmp_path, older_than_days=30)
    assert {c.category for c in results} == {"log"}
    assert len(results) == 3


def test_find_stale_orders_results_by_size_desc(tmp_path: Path) -> None:
    _touch(tmp_path / "small.sql", size=10, age_days=45)
    _touch(tmp_path / "big.sql", size=10_000, age_days=45)
    _touch(tmp_path / "mid.sql", size=500, age_days=45)
    sizes = [c.size_bytes for c in find_stale(tmp_path, older_than_days=30)]
    assert sizes == [10_000, 500, 10]


def test_find_stale_walks_subdirectories(tmp_path: Path) -> None:
    _touch(tmp_path / "a" / "b" / "c" / "old.dump", size=42, age_days=90)
    results = find_stale(tmp_path, older_than_days=30)
    assert len(results) == 1
    assert results[0].path.name == "old.dump"


def test_find_stale_ignores_symlinks_by_default(tmp_path: Path) -> None:
    real = _touch(tmp_path / "real" / "dump.sql", size=10, age_days=45)
    link_dir = tmp_path / "linked"
    link_dir.symlink_to(real.parent)
    # symlinks not followed → file is still discovered via the real path only
    results = find_stale(tmp_path, older_than_days=30)
    assert len(results) == 1


def test_find_stale_accepts_custom_globs(tmp_path: Path) -> None:
    _touch(tmp_path / "core.1234", size=10, age_days=45)
    results = find_stale(
        tmp_path,
        older_than_days=30,
        artifact_globs=("core.*",),
        log_globs=(),
    )
    assert len(results) == 1
    assert results[0].category == "artifact"


def test_default_globs_include_sql_and_rotated_logs() -> None:
    assert "*.sql" in STALE_ARTIFACT_GLOBS
    assert "*.sql.gz" in STALE_ARTIFACT_GLOBS
    assert "*.log.gz" in LEGACY_LOG_GLOBS
