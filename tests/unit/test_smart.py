"""Tests for SMART parsing — no smartctl required."""

from __future__ import annotations

from fleetfix.modules.disk.smart import (
    enumerate_block_devices,
    parse_health,
    parse_nvme_attributes,
    parse_sata_attributes,
)

_SATA_FIXTURE = """\
smartctl 7.3 2022-02-28 r5338 [x86_64-linux-5.15.0-1-generic] (local build)
Copyright (C) 2002-22, Bruce Allen, Christian Franke, www.smartmontools.org

=== START OF READ SMART DATA SECTION ===
SMART overall-health self-assessment test result: PASSED

SMART Attributes Data Structure revision number: 16
Vendor Specific SMART Attributes with Thresholds:
ID# ATTRIBUTE_NAME          FLAG     VALUE WORST THRESH TYPE      UPDATED  WHEN_FAILED RAW_VALUE
  5 Reallocated_Sector_Ct   0x0033   100   100   010    Pre-fail  Always       -       3
  9 Power_On_Hours          0x0032   099   099   000    Old_age   Always       -       1234
187 Reported_Uncorrect      0x0032   100   100   000    Old_age   Always       -       0
197 Current_Pending_Sector  0x0032   100   100   000    Old_age   Always       -       0
233 Media_Wearout_Indicator 0x0032   088   088   000    Old_age   Always       -       12
"""

_NVME_FIXTURE = """\
smartctl 7.3 2022-02-28 r5338 [x86_64-linux-5.15.0-1-generic] (local build)
Copyright (C) 2002-22, Bruce Allen, Christian Franke, www.smartmontools.org

=== START OF SMART DATA SECTION ===
SMART overall-health self-assessment test result: PASSED

SMART/Health Information (NVMe Log 0x02)
Critical Warning:                   0x00
Temperature:                        38 Celsius
Available Spare:                    100%
Available Spare Threshold:          10%
Percentage Used:                    3%
Data Units Read:                    1,234,567 [632 GB]
Data Units Written:                 234,567 [120 GB]
Host Read Commands:                 12,345,678
Host Write Commands:                2,345,678
Media and Data Integrity Errors:    0
Error Information Log Entries:      0
"""

_FAILED_FIXTURE = """\
=== START OF READ SMART DATA SECTION ===
SMART overall-health self-assessment test result: FAILED!
"""


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
    text = "Media and Data Integrity Errors:    12,345\n"
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
