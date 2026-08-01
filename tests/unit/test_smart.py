"""Tests for SMART parsing — no smartctl required."""

from __future__ import annotations

from fleetfix.modules.disk.smart import (
    enumerate_block_devices,
    parse_health,
    parse_nvme_attributes,
    parse_sata_attributes,
)
from tests.support.fixtures import fixture

_SATA_FIXTURE = fixture("smartctl/sata_passed.txt")

_NVME_FIXTURE = fixture("smartctl/nvme_passed.txt")

_FAILED_FIXTURE = fixture("smartctl/health_failed.txt")


def test_parse_health_passed() -> None:
    assert parse_health(_SATA_FIXTURE) == "PASSED"


def test_parse_health_failed() -> None:
    # "FAILED!" is the literal smartctl emits — regex strips the bang.
    assert parse_health(_FAILED_FIXTURE) == "FAILED!"


def test_parse_health_missing() -> None:
    assert parse_health("no health line in here") is None


def test_parse_sata_attributes_picks_interesting_ids() -> None:
    attrs = parse_sata_attributes(_SATA_FIXTURE)
    assert attrs["reallocated_sectors"] == 3
    assert attrs["power_on_hours"] == 1234
    assert attrs["reported_uncorrect"] == 0
    assert attrs["current_pending_sector"] == 0
    assert attrs["ssd_wear_indicator"] == 12


def test_parse_sata_attributes_ignores_other_rows() -> None:
    attrs = parse_sata_attributes(_SATA_FIXTURE)
    # 16 isn't in the interesting set
    assert "16" not in attrs
    # vendor-named columns like "Vendor Specific" don't pollute output
    assert all(isinstance(v, int) for v in attrs.values())


def test_parse_nvme_attributes() -> None:
    attrs = parse_nvme_attributes(_NVME_FIXTURE)
    assert attrs["percentage_used"] == 3
    assert attrs["available_spare"] == 100
    assert attrs["available_spare_threshold"] == 10
    assert attrs["media_and_data_integrity_errors"] == 0


def test_parse_nvme_attributes_handles_comma_separated_ints() -> None:
    text = fixture("smartctl/nvme_comma_separated_ints.txt")
    attrs = parse_nvme_attributes(text)
    assert attrs["media_and_data_integrity_errors"] == 12345


def test_enumerate_filters_pseudo_devices(tmp_path) -> None:  # type: ignore[no-untyped-def]
    sys_block = tmp_path / "sys" / "block"
    sys_block.mkdir(parents=True)
    for name in ["sda", "sdb", "nvme0n1", "loop0", "ram0", "dm-0", "zram0", "sr0"]:
        (sys_block / name).mkdir()
    devs = enumerate_block_devices(sys_block)
    assert devs == ["/dev/nvme0n1", "/dev/sda", "/dev/sdb"]


def test_enumerate_missing_sys_block(tmp_path) -> None:  # type: ignore[no-untyped-def]
    assert enumerate_block_devices(tmp_path / "does-not-exist") == []
