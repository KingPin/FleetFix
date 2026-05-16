"""Unit tests for docker.hygiene — size parser, df rows, prune flow."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from fleetfix.audit.logger import AuditLogger, Operator
from fleetfix.modules.docker import hygiene
from fleetfix.modules.docker.hygiene import (
    DfRow,
    parse_reclaimed_total,
    parse_size,
    parse_system_df_json_lines,
    prune_images,
    prune_volumes,
)


@pytest.fixture
def audit(tmp_path: Path) -> AuditLogger:
    return AuditLogger(tmp_path / "audit.log", operator=Operator(unix_user="tester"))


def _read_audit(audit: AuditLogger) -> list[dict]:
    return [json.loads(line) for line in audit.path.read_text().splitlines() if line.strip()]


class TestParseSize:
    def test_handles_decimal_gigabytes(self) -> None:
        assert parse_size("45.68GB") == int(45.68 * 1000**3)

    def test_handles_plain_bytes(self) -> None:
        assert parse_size("136B") == 136
        assert parse_size("0 B") == 0

    def test_handles_binary_units(self) -> None:
        assert parse_size("1KiB") == 1024
        assert parse_size("2MiB") == 2 * 1024 * 1024

    def test_handles_empty(self) -> None:
        assert parse_size("") == 0
        assert parse_size("garbage") == 0


def test_parse_system_df_json_lines() -> None:
    text = (
        '{"Type":"Images","TotalCount":"114","Active":"6","Size":"53.5GB",'
        '"Reclaimable":"45.68GB (85%)"}\n'
        '{"Type":"Containers","TotalCount":"7","Active":"7","Size":"136B",'
        '"Reclaimable":"0B (0%)"}\n'
        '{"Type":"Local Volumes","TotalCount":"26","Active":"2","Size":"17.71GB",'
        '"Reclaimable":"444.8MB (2%)"}\n'
        '{"Type":"Build Cache","TotalCount":"202","Active":"0","Size":"7.817GB",'
        '"Reclaimable":"7.817GB"}\n'
    )
    rows = parse_system_df_json_lines(text)
    assert len(rows) == 4
    images: DfRow = rows[0]
    assert images.type == "Images"
    assert images.total_count == 114
    assert images.reclaimable_pct == 85
    assert images.reclaimable_bytes == int(45.68 * 1000**3)
    build_cache = rows[3]
    # No "(X%)" suffix is fine — percentage defaults to 0.
    assert build_cache.reclaimable_pct == 0
    assert build_cache.reclaimable_bytes == int(7.817 * 1000**3)


def test_parse_reclaimed_total_finds_trailing_line() -> None:
    output = "Deleted Images:\ndeleted: sha256:abc\n\nTotal reclaimed space: 123.4MB\n"
    assert parse_reclaimed_total(output) == int(123.4 * 1000**2)


def test_parse_reclaimed_total_missing_returns_zero() -> None:
    assert parse_reclaimed_total("nothing happened") == 0


def test_prune_images_records_bytes(audit: AuditLogger, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(
        hygiene,
        "_invoke_prune",
        lambda argv: (None, 500 * 1000**2),
    )
    result = prune_images(audit=audit)
    assert result.ok is True
    assert result.bytes_reclaimed == 500 * 1000**2
    records = _read_audit(audit)
    intent = next(r for r in records if r["phase"] == "intent")
    res = next(r for r in records if r["phase"] == "result")
    assert intent["action"] == "docker.prune_images"
    assert res["result"]["ok"] is True
    assert res["result"]["bytes_reclaimed"] == 500 * 1000**2


def test_prune_volumes_records_failure(
    audit: AuditLogger,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr(hygiene, "_invoke_prune", lambda argv: ("permission denied", 0))
    result = prune_volumes(audit=audit)
    assert result.ok is False
    assert result.error == "permission denied"
    records = _read_audit(audit)
    res = next(r for r in records if r["phase"] == "result")
    assert res["result"]["ok"] is False
    assert res["action"] == "docker.prune_volumes"


def test_prune_images_passes_include_all_flag(
    audit: AuditLogger,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    captured: list[list[str]] = []

    def fake_invoke(argv: list[str]) -> tuple[str | None, int]:
        captured.append(argv)
        return None, 0

    monkeypatch.setattr(hygiene, "_invoke_prune", fake_invoke)
    prune_images(audit=audit, include_all=True)
    assert captured == [["docker", "image", "prune", "-f", "-a"]]
