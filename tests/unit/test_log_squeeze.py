"""Unit tests for modules.log_squeeze.gzip_inplace."""

from __future__ import annotations

import gzip
import json
import subprocess
from pathlib import Path

import pytest

from fleetfix.audit.logger import AuditLogger, Operator
from fleetfix.modules.disk.blacklist import BlacklistedPath
from fleetfix.modules.log_squeeze.gzip_inplace import (
    Candidate,
    find_squeezable_logs,
    is_open_for_write,
    squeeze_log,
)


@pytest.fixture
def audit(tmp_path: Path) -> AuditLogger:
    return AuditLogger(tmp_path / "audit.log", operator=Operator(unix_user="appuser"))


def _read_jsonl(path: Path) -> list[dict]:
    return [json.loads(line) for line in path.read_text().splitlines() if line.strip()]


def _completed(
    returncode: int = 0, stdout: str = "", stderr: str = ""
) -> subprocess.CompletedProcess[str]:
    return subprocess.CompletedProcess(
        args=["lsof"], returncode=returncode, stdout=stdout, stderr=stderr
    )


# --- find_squeezable_logs -------------------------------------------------


def test_find_returns_large_log_files(tmp_path: Path) -> None:
    big = tmp_path / "app.log"
    big.write_bytes(b"x" * 2048)
    small = tmp_path / "tiny.log"
    small.write_bytes(b"x" * 16)
    found = find_squeezable_logs([tmp_path], min_bytes=1024)
    assert [c.path for c in found] == [big]


def test_find_skips_already_compressed_logs(tmp_path: Path) -> None:
    (tmp_path / "rotated.log.gz").write_bytes(b"x" * 4096)
    (tmp_path / "rotated.log.1").write_bytes(b"x" * 4096)
    found = find_squeezable_logs([tmp_path], min_bytes=1024)
    assert {c.path.name for c in found} == {"rotated.log.1"}


def test_find_sorted_largest_first(tmp_path: Path) -> None:
    (tmp_path / "small.log").write_bytes(b"x" * 2048)
    (tmp_path / "huge.log").write_bytes(b"x" * 8192)
    (tmp_path / "medium.log").write_bytes(b"x" * 4096)
    sizes = [c.size for c in find_squeezable_logs([tmp_path], min_bytes=1024)]
    assert sizes == sorted(sizes, reverse=True)


def test_find_skips_symlinks(tmp_path: Path) -> None:
    real = tmp_path / "real.log"
    real.write_bytes(b"x" * 4096)
    link = tmp_path / "link.log"
    link.symlink_to(real)
    found = [c.path for c in find_squeezable_logs([tmp_path], min_bytes=1024)]
    assert real in found
    assert link not in found


def test_find_ignores_nonexistent_root(tmp_path: Path) -> None:
    assert find_squeezable_logs([tmp_path / "does-not-exist"], min_bytes=1) == []


def test_find_matches_rotated_log_with_date_suffix(tmp_path: Path) -> None:
    (tmp_path / "syslog.log.2026-05-16").write_bytes(b"x" * 2048)
    found = find_squeezable_logs([tmp_path], min_bytes=1024)
    assert len(found) == 1


# --- is_open_for_write ----------------------------------------------------


def test_is_open_returns_false_when_lsof_clean() -> None:
    proc = _completed(returncode=1, stdout="")
    assert is_open_for_write(Path("/var/log/x.log"), run=proc) is False


def test_is_open_returns_true_when_writer_present() -> None:
    output = "p123\ncrsyslogd\nf3\naw\nn/var/log/syslog\n"
    proc = _completed(returncode=0, stdout=output)
    assert is_open_for_write(Path("/var/log/syslog"), run=proc) is True


def test_is_open_treats_rw_mode_as_writer() -> None:
    output = "p99\nfsomething\nau\n"
    proc = _completed(returncode=0, stdout=output)
    assert is_open_for_write(Path("/var/log/x"), run=proc) is True


def test_is_open_read_only_is_not_a_writer() -> None:
    output = "p99\nfsomething\nar\n"
    proc = _completed(returncode=0, stdout=output)
    assert is_open_for_write(Path("/var/log/x"), run=proc) is False


# --- squeeze_log ----------------------------------------------------------


def test_squeeze_compresses_in_place(tmp_path: Path, audit: AuditLogger) -> None:
    target = tmp_path / "app.log"
    payload = b"hello world\n" * 4096
    target.write_bytes(payload)
    result = squeeze_log(target, audit, open_for_write=False)
    assert result.ok is True
    assert result.bytes_before == len(payload)
    assert result.bytes_after < result.bytes_before
    assert not target.exists()
    gz = target.with_name(target.name + ".gz")
    assert gz.exists()
    assert gzip.decompress(gz.read_bytes()) == payload


def test_squeeze_refuses_blacklisted_path(audit: AuditLogger) -> None:
    with pytest.raises(BlacklistedPath):
        squeeze_log(Path("/etc/passwd"), audit, open_for_write=False)


def test_squeeze_refuses_when_open_for_write(tmp_path: Path, audit: AuditLogger) -> None:
    target = tmp_path / "live.log"
    target.write_bytes(b"x" * 1024)
    result = squeeze_log(target, audit, open_for_write=True)
    assert result.ok is False
    assert "open for write" in (result.error or "")
    # Original must be untouched.
    assert target.exists()
    assert not target.with_name(target.name + ".gz").exists()


def test_squeeze_audits_intent_and_result(tmp_path: Path, audit: AuditLogger) -> None:
    target = tmp_path / "app.log"
    target.write_bytes(b"x" * 4096)
    squeeze_log(target, audit, open_for_write=False)
    records = _read_jsonl(audit.path)
    phases = [r["phase"] for r in records]
    assert phases == ["intent", "result"]
    assert records[1]["result"]["ok"] is True
    assert records[1]["result"]["bytes_before"] == 4096


def test_squeeze_audit_failure_when_held(tmp_path: Path, audit: AuditLogger) -> None:
    target = tmp_path / "held.log"
    target.write_bytes(b"x" * 1024)
    squeeze_log(target, audit, open_for_write=True)
    records = _read_jsonl(audit.path)
    assert records[-1]["result"]["ok"] is False
    assert "open for write" in records[-1]["result"]["error"]


def test_squeeze_refuses_symlinks(tmp_path: Path, audit: AuditLogger) -> None:
    real = tmp_path / "real.log"
    real.write_bytes(b"x" * 2048)
    link = tmp_path / "link.log"
    link.symlink_to(real)
    result = squeeze_log(link, audit, open_for_write=False)
    assert result.ok is False
    assert "not a regular file" in (result.error or "")
    assert real.exists()


def test_squeeze_refuses_missing_file(tmp_path: Path, audit: AuditLogger) -> None:
    result = squeeze_log(tmp_path / "ghost.log", audit, open_for_write=False)
    assert result.ok is False


def test_candidate_is_hashable_dataclass() -> None:
    c = Candidate(path=Path("/var/log/a.log"), size=1)
    assert hash(c) == hash((Path("/var/log/a.log"), 1))
