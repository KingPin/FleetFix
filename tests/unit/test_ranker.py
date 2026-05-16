"""Tests for the /proc ranker — exercises parsing and ranking against a
fake /proc tree, plus a thin smoke test against real /proc."""

from __future__ import annotations

from pathlib import Path

from fleetfix.modules.procs.ranker import (
    ProcInfo,
    snapshot,
    top_by_cpu,
    top_by_rss,
)


def _write_fake_proc(
    root: Path, pid: int, *, comm: str, utime: int, stime: int, rss_pages: int, cmdline: str
) -> None:
    """Build the four files snapshot() reads.

    /proc/<pid>/stat needs fields 1..15 valid. The comm is the 2nd field
    in parens; utime is field 14 and stime is field 15.
    """
    pdir = root / str(pid)
    pdir.mkdir(parents=True)
    # Fields 1, (2), 3..15. We pad with zeros up to field 17 to be safe.
    # field index: 1=pid, 2=comm, 3=state, 4..13=zero, 14=utime, 15=stime, 16..=zero
    stat_fields = [
        str(pid),  # 1
        f"({comm})",  # 2
        "S",  # 3 state
    ]
    # fields 4..13 inclusive — that's 10 padding fields
    stat_fields.extend(["0"] * 10)
    stat_fields.append(str(utime))  # 14
    stat_fields.append(str(stime))  # 15
    stat_fields.extend(["0"] * 5)
    (pdir / "stat").write_text(" ".join(stat_fields))
    # statm: size resident shared text lib data dt
    (pdir / "statm").write_text(f"100 {rss_pages} 0 0 0 0 0\n")
    (pdir / "cmdline").write_text(cmdline.replace(" ", "\x00"))
    (pdir / "status").write_text(f"Name:\t{comm}\nUid:\t0\t0\t0\t0\n")


def test_snapshot_parses_fake_proc(tmp_path: Path) -> None:
    _write_fake_proc(
        tmp_path, 1, comm="init", utime=10, stime=5, rss_pages=1000, cmdline="/sbin/init"
    )
    _write_fake_proc(
        tmp_path,
        42,
        comm="postgres",
        utime=100,
        stime=20,
        rss_pages=50000,
        cmdline="postgres: writer process",
    )
    procs = snapshot(sample_interval_s=0.0, proc=tmp_path)
    by_pid = {p.pid: p for p in procs}
    assert by_pid[1].comm == "init"
    assert by_pid[42].comm == "postgres"
    assert by_pid[42].cmdline == "postgres: writer process"


def test_snapshot_computes_rss_bytes(tmp_path: Path) -> None:
    import os

    _write_fake_proc(tmp_path, 1, comm="x", utime=0, stime=0, rss_pages=1000, cmdline="x")
    procs = snapshot(sample_interval_s=0.0, proc=tmp_path)
    page = os.sysconf("SC_PAGE_SIZE") if hasattr(os, "sysconf") else 4096
    assert procs[0].rss_bytes == 1000 * page


def test_snapshot_handles_comm_with_spaces(tmp_path: Path) -> None:
    _write_fake_proc(
        tmp_path,
        7,
        comm="some (weird) thing",
        utime=1,
        stime=1,
        rss_pages=5,
        cmdline="x",
    )
    procs = snapshot(sample_interval_s=0.0, proc=tmp_path)
    assert procs[0].comm == "some (weird) thing"


def test_snapshot_skips_non_pid_dirs(tmp_path: Path) -> None:
    (tmp_path / "self").mkdir()  # symlink-like alias on real /proc
    (tmp_path / "cpuinfo").write_text("x")
    _write_fake_proc(tmp_path, 1, comm="real", utime=0, stime=0, rss_pages=1, cmdline="real")
    procs = snapshot(sample_interval_s=0.0, proc=tmp_path)
    assert len(procs) == 1
    assert procs[0].pid == 1


def test_top_by_rss_sorts_desc() -> None:
    procs = [
        ProcInfo(pid=1, comm="a", user=None, rss_bytes=100, cpu_pct=0, cmdline=""),
        ProcInfo(pid=2, comm="b", user=None, rss_bytes=500, cpu_pct=0, cmdline=""),
        ProcInfo(pid=3, comm="c", user=None, rss_bytes=200, cpu_pct=0, cmdline=""),
    ]
    top = top_by_rss(procs, n=2)
    assert [p.pid for p in top] == [2, 3]


def test_top_by_cpu_sorts_desc() -> None:
    procs = [
        ProcInfo(pid=1, comm="a", user=None, rss_bytes=0, cpu_pct=5.0, cmdline=""),
        ProcInfo(pid=2, comm="b", user=None, rss_bytes=0, cpu_pct=70.0, cmdline=""),
        ProcInfo(pid=3, comm="c", user=None, rss_bytes=0, cpu_pct=12.5, cmdline=""),
    ]
    top = top_by_cpu(procs, n=2)
    assert [p.pid for p in top] == [2, 3]


def test_snapshot_against_real_proc_returns_something() -> None:
    procs = snapshot(sample_interval_s=0.05)
    assert len(procs) > 1
    # The current python process should be in there.
    import os as _os

    assert any(p.pid == _os.getpid() for p in procs)
