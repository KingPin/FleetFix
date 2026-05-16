"""Unit tests for system metrics parsing — uses fixture files, no real /proc."""

from __future__ import annotations

from pathlib import Path

import pytest

from fleetfix.modules.system import metrics, thermal

SAMPLE_UPTIME = "12345.67 98765.43\n"
SAMPLE_LOADAVG = "0.12 0.34 0.56 2/345 6789\n"
SAMPLE_MEMINFO = """\
MemTotal:       16384000 kB
MemFree:         2048000 kB
MemAvailable:    8192000 kB
Buffers:          128000 kB
Cached:          4096000 kB
SwapTotal:       4194304 kB
SwapFree:        3145728 kB
Dirty:              1234 kB
"""


@pytest.fixture
def proc_files(tmp_path: Path) -> dict[str, Path]:
    uptime = tmp_path / "uptime"
    uptime.write_text(SAMPLE_UPTIME)
    loadavg = tmp_path / "loadavg"
    loadavg.write_text(SAMPLE_LOADAVG)
    meminfo = tmp_path / "meminfo"
    meminfo.write_text(SAMPLE_MEMINFO)
    return {"uptime": uptime, "loadavg": loadavg, "meminfo": meminfo}


def test_read_uptime(proc_files: dict[str, Path]) -> None:
    assert metrics.read_uptime(proc_files["uptime"]) == pytest.approx(12345.67)


def test_read_loadavg(proc_files: dict[str, Path]) -> None:
    load = metrics.read_loadavg(proc_files["loadavg"])
    assert load.one == pytest.approx(0.12)
    assert load.five == pytest.approx(0.34)
    assert load.fifteen == pytest.approx(0.56)


def test_read_meminfo(proc_files: dict[str, Path]) -> None:
    mem = metrics.read_meminfo(proc_files["meminfo"])
    assert mem.total_kb == 16384000
    assert mem.available_kb == 8192000
    assert mem.used_kb == 16384000 - 8192000
    assert mem.swap_total_kb == 4194304
    assert mem.swap_used_kb == 4194304 - 3145728
    assert mem.used_pct == pytest.approx(50.0)


def test_meminfo_handles_missing_swap(tmp_path: Path) -> None:
    file = tmp_path / "meminfo"
    file.write_text("MemTotal: 1000 kB\nMemAvailable: 500 kB\n")
    mem = metrics.read_meminfo(file)
    assert mem.swap_total_kb == 0
    assert mem.swap_used_pct == 0.0


def test_meminfo_falls_back_to_memfree_when_available_missing(tmp_path: Path) -> None:
    file = tmp_path / "meminfo"
    file.write_text("MemTotal: 1000 kB\nMemFree: 250 kB\n")
    mem = metrics.read_meminfo(file)
    assert mem.available_kb == 250


@pytest.mark.parametrize(
    "seconds,expected",
    [
        (45, "0m 45s"),
        (90, "1m 30s"),
        (3700, "1h 1m"),
        (90000, "1d 1h 0m"),
        (3 * 86400 + 14 * 3600 + 22 * 60, "3d 14h 22m"),
    ],
)
def test_format_uptime(seconds: int, expected: str) -> None:
    assert metrics.format_uptime(seconds) == expected


def test_thermal_absent_returns_empty(tmp_path: Path) -> None:
    nowhere = tmp_path / "no-such-dir"
    assert thermal.read_zones(nowhere) == []
    assert thermal.hottest([]) is None


def test_thermal_parses_zones(tmp_path: Path) -> None:
    zone0 = tmp_path / "thermal_zone0"
    zone0.mkdir()
    (zone0 / "temp").write_text("42000\n")
    (zone0 / "type").write_text("x86_pkg_temp\n")
    zone1 = tmp_path / "thermal_zone1"
    zone1.mkdir()
    (zone1 / "temp").write_text("55500\n")
    (zone1 / "type").write_text("acpitz\n")

    zones = thermal.read_zones(tmp_path)
    assert len(zones) == 2
    hottest = thermal.hottest(zones)
    assert hottest is not None
    assert hottest.type == "acpitz"
    assert hottest.temp_c == pytest.approx(55.5)


def test_thermal_skips_unparseable_temp(tmp_path: Path) -> None:
    zone = tmp_path / "thermal_zone0"
    zone.mkdir()
    (zone / "temp").write_text("not-a-number\n")
    (zone / "type").write_text("x86_pkg_temp\n")
    assert thermal.read_zones(tmp_path) == []
