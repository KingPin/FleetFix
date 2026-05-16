"""Unit tests for updater.installer — download, verify, swap."""

from __future__ import annotations

import hashlib
import json
from pathlib import Path

import pytest

from fleetfix.audit.logger import AuditLogger, Operator
from fleetfix.updater.checker import ReleaseInfo
from fleetfix.updater.installer import (
    apply_update,
    parse_sha256_line,
    sha256_file,
)

ASSET = "fleetfix-linux-x86_64"


@pytest.fixture
def audit(tmp_path: Path) -> AuditLogger:
    return AuditLogger(
        tmp_path / "audit.log",
        operator=Operator(unix_user="appuser"),
    )


def _release(asset_url: str, checksum_url: str) -> ReleaseInfo:
    return ReleaseInfo(
        tag="v0.2.0",
        version="0.2.0",
        asset_url=asset_url,
        checksum_url=checksum_url,
        html_url="https://example.com/release",
        body="",
    )


def _read_jsonl(path: Path) -> list[dict]:
    return [json.loads(line) for line in path.read_text().splitlines() if line.strip()]


def test_parse_sha256_line_simple() -> None:
    text = f"abc123  {ASSET}\n"
    assert parse_sha256_line(text, asset_name=ASSET) == "abc123"


def test_parse_sha256_line_with_binary_marker() -> None:
    text = f"abc123 *{ASSET}\n"
    assert parse_sha256_line(text, asset_name=ASSET) == "abc123"


def test_parse_sha256_line_returns_none_when_missing() -> None:
    text = "abc123  some-other-file\n"
    assert parse_sha256_line(text, asset_name=ASSET) is None


def test_parse_sha256_line_skips_comments_and_blanks() -> None:
    text = f"# header\n\nabc123  {ASSET}\n"
    assert parse_sha256_line(text, asset_name=ASSET) == "abc123"


def test_sha256_file_matches_hashlib(tmp_path: Path) -> None:
    payload = b"hello world"
    f = tmp_path / "blob"
    f.write_bytes(payload)
    assert sha256_file(f) == hashlib.sha256(payload).hexdigest()


def test_apply_update_happy_path(tmp_path: Path, audit: AuditLogger) -> None:
    payload = b"fake-binary-data"
    digest = hashlib.sha256(payload).hexdigest()

    def fake_download(url: str, dest: Path) -> None:
        dest.write_bytes(payload)

    def fake_fetch_text(url: str) -> str:
        return f"{digest}  {ASSET}\n"

    swap_calls: list[tuple[Path, Path]] = []

    def fake_swap(staged: Path, target: Path) -> str | None:
        swap_calls.append((staged, target))
        target.write_bytes(payload)
        return None

    target = tmp_path / "fleetfix"
    result = apply_update(
        _release("https://x/asset", "https://x/sums"),
        audit=audit,
        target=target,
        staging_dir=tmp_path,
        download=fake_download,
        fetch_text=fake_fetch_text,
        install_swap=fake_swap,
    )
    assert result.ok is True
    assert result.version == "0.2.0"
    assert target.read_bytes() == payload
    assert len(swap_calls) == 1
    # Audit records intent + result with ok=True.
    records = _read_jsonl(audit.path)
    assert [r["phase"] for r in records] == ["intent", "result"]
    assert records[1]["result"]["ok"] is True


def test_apply_update_sha_mismatch_aborts(tmp_path: Path, audit: AuditLogger) -> None:
    def fake_download(url: str, dest: Path) -> None:
        dest.write_bytes(b"real-bytes")

    def fake_fetch_text(url: str) -> str:
        return f"0000000000000000000000000000000000000000000000000000000000000000  {ASSET}\n"

    target = tmp_path / "fleetfix"
    result = apply_update(
        _release("https://x/asset", "https://x/sums"),
        audit=audit,
        target=target,
        staging_dir=tmp_path,
        download=fake_download,
        fetch_text=fake_fetch_text,
        install_swap=lambda staged, target: pytest.fail("swap must not run on bad sha"),
    )
    assert result.ok is False
    assert result.error is not None
    assert "sha256 mismatch" in result.error
    assert not target.exists()
    records = _read_jsonl(audit.path)
    assert records[-1]["result"]["ok"] is False
    assert "sha256 mismatch" in records[-1]["result"]["error"]


def test_apply_update_download_failure(tmp_path: Path, audit: AuditLogger) -> None:
    def fake_download(url: str, dest: Path) -> None:
        raise RuntimeError("connection reset")

    result = apply_update(
        _release("https://x/asset", "https://x/sums"),
        audit=audit,
        target=tmp_path / "fleetfix",
        staging_dir=tmp_path,
        download=fake_download,
        fetch_text=lambda url: "",
        install_swap=lambda staged, target: None,
    )
    assert result.ok is False
    assert result.error is not None
    assert "download failed" in result.error


def test_apply_update_swap_failure_propagates(tmp_path: Path, audit: AuditLogger) -> None:
    payload = b"x"
    digest = hashlib.sha256(payload).hexdigest()

    def fake_download(url: str, dest: Path) -> None:
        dest.write_bytes(payload)

    def fake_fetch_text(url: str) -> str:
        return f"{digest}  {ASSET}\n"

    result = apply_update(
        _release("https://x/asset", "https://x/sums"),
        audit=audit,
        target=tmp_path / "fleetfix",
        staging_dir=tmp_path,
        download=fake_download,
        fetch_text=fake_fetch_text,
        install_swap=lambda staged, target: "sudo: password required",
    )
    assert result.ok is False
    assert result.error == "sudo: password required"


def test_apply_update_missing_digest_for_asset(tmp_path: Path, audit: AuditLogger) -> None:
    def fake_download(url: str, dest: Path) -> None:
        dest.write_bytes(b"data")

    def fake_fetch_text(url: str) -> str:
        return "deadbeef  some-other-file\n"

    result = apply_update(
        _release("https://x/asset", "https://x/sums"),
        audit=audit,
        target=tmp_path / "fleetfix",
        staging_dir=tmp_path,
        download=fake_download,
        fetch_text=fake_fetch_text,
        install_swap=lambda staged, target: None,
    )
    assert result.ok is False
    assert result.error is not None
    assert "no digest" in result.error
