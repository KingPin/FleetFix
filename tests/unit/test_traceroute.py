"""Tests for the traceroute / tracepath path tracer."""

from __future__ import annotations

import subprocess
from typing import Any

import pytest

from fleetfix.modules.network import traceroute as tr
from fleetfix.modules.network.traceroute import (
    MISSING_TOOLS_ERROR,
    parse_tracepath_output,
    parse_traceroute_output,
    select_tool,
    timeout_for,
    trace,
)

_TRACEROUTE_REACHED = """\
traceroute to 8.8.8.8 (8.8.8.8), 15 hops max, 60 byte packets
 1  192.168.1.1  0.687 ms
 2  10.240.178.229  6.968 ms
 3  8.8.8.8  11.029 ms
"""

_TRACEROUTE_STALLED = """\
traceroute to 192.0.2.1 (192.0.2.1), 6 hops max, 60 byte packets
 1  192.168.1.1  0.717 ms
 2  10.240.178.229  12.167 ms
 3  67.59.229.86  9.828 ms
 4  * * *
 5  * * *
 6  * * *
"""

# Hop 2 is load balanced: two responders, one probe lost.
_TRACEROUTE_MULTI_PROBE = """\
traceroute to 1.1.1.1 (1.1.1.1), 15 hops max, 60 byte packets
 1  192.168.1.1  0.687 ms  0.501 ms  0.442 ms
 2  64.15.5.142  11.248 ms  64.15.1.175  12.449 ms  *
 3  1.1.1.1  13.001 ms  13.100 ms  13.200 ms
"""

# A firewall answering *for* the destination — address matches, but nothing arrived.
_TRACEROUTE_ADMIN_PROHIBITED = """\
traceroute to 203.0.113.9 (203.0.113.9), 15 hops max, 60 byte packets
 1  192.168.1.1  0.687 ms
 2  10.240.178.229  6.968 ms
 3  203.0.113.9  11.029 ms !X
"""

_TRACEROUTE_UNKNOWN_HOST = "traceroute: unknown host nope.invalid\n"

# The tracepath blocks below are literal captures from a real host.
_TRACEPATH_REACHED = """\
 1?: [LOCALHOST]                      pmtu 1500
 1:  192.168.1.1                                           0.789ms reached
 1:  192.168.1.1                                           0.447ms reached
     Resume: pmtu 1500 hops 1 back 1
"""

_TRACEPATH_TOO_MANY_HOPS = """\
 1?: [LOCALHOST]                      pmtu 1500
 1:  192.168.1.1                                           0.727ms
 1:  192.168.1.1                                           0.418ms
 2:  10.240.178.229                                       11.988ms
 3:  67.59.229.86                                         10.429ms
 4:  67.83.221.166                                        12.940ms
 5:  no reply
 6:  no reply
 7:  no reply
 8:  no reply
     Too many hops: pmtu 1500
     Resume: pmtu 1500
"""

# "asymm N" annotations trail the RTT and are not responders.
_TRACEPATH_ASYMM = """\
 1?: [LOCALHOST]                      pmtu 1500
 1:  192.168.1.1                                           0.531ms
 2:  65.19.100.4                                          33.087ms asymm  7
 3:  162.158.61.101                                       26.459ms asymm  9
     Too many hops: pmtu 1500
     Resume: pmtu 1500
"""

_TRACEPATH_UNKNOWN_HOST = "tracepath: nope.invalid: Name or service not known\n"


def test_traceroute_reached() -> None:
    result = parse_traceroute_output("8.8.8.8", _TRACEROUTE_REACHED, max_hops=15)
    assert result.tool == "traceroute"
    assert len(result.hops) == 3
    assert result.hops[0].hosts == ("192.168.1.1",)
    assert result.hops[0].rtts_ms == (0.687,)
    assert result.reached is True
    assert result.stalled_at is None
    assert result.last_responding_hop == 3
    assert result.error is None
    assert result.raw == _TRACEROUTE_REACHED


def test_traceroute_stalled_reports_first_dark_hop() -> None:
    result = parse_traceroute_output("192.0.2.1", _TRACEROUTE_STALLED, max_hops=6)
    assert result.reached is False
    assert result.last_responding_hop == 3
    assert result.stalled_at == 4
    assert result.hops[3].timeouts == 3
    assert result.hops[3].responded is False


def test_traceroute_load_balanced_hop_keeps_every_responder() -> None:
    result = parse_traceroute_output("1.1.1.1", _TRACEROUTE_MULTI_PROBE, max_hops=15)
    hop = result.hops[1]
    assert hop.hosts == ("64.15.5.142", "64.15.1.175")
    assert hop.rtts_ms == (11.248, 12.449)
    assert hop.timeouts == 1
    assert result.hops[0].rtts_ms == (0.687, 0.501, 0.442)
    assert result.reached is True


def test_traceroute_admin_prohibited_is_not_reached() -> None:
    result = parse_traceroute_output("203.0.113.9", _TRACEROUTE_ADMIN_PROHIBITED, max_hops=15)
    # The destination address appears, but a router sent it — !X says so.
    assert result.hops[-1].hosts == ("203.0.113.9",)
    assert result.hops[-1].flags == ("!X",)
    assert result.reached is False


def test_traceroute_unknown_host_has_no_hops_and_an_error() -> None:
    result = parse_traceroute_output("nope.invalid", _TRACEROUTE_UNKNOWN_HOST, max_hops=15)
    assert result.hops == ()
    assert result.reached is False
    assert result.error == "unknown host nope.invalid"
    assert result.stalled_at is None


def test_tracepath_reached_merges_repeated_probes() -> None:
    result = parse_tracepath_output("192.168.1.1", _TRACEPATH_REACHED, max_hops=15)
    assert result.tool == "tracepath"
    # Two probe lines for hop 1, one responder.
    assert len(result.hops) == 1
    assert result.hops[0].hosts == ("192.168.1.1",)
    assert result.hops[0].rtts_ms == (0.789, 0.447)
    assert result.reached is True
    assert result.error is None


def test_tracepath_too_many_hops_is_not_reached() -> None:
    # A maxed-out trace still prints a Resume line, so "Resume:" alone is not enough.
    result = parse_tracepath_output("192.0.2.1", _TRACEPATH_TOO_MANY_HOPS, max_hops=8)
    assert result.reached is False
    assert result.last_responding_hop == 4
    assert result.stalled_at == 5
    assert result.hops[4].timeouts == 1
    assert result.hops[4].responded is False


def test_tracepath_skips_the_localhost_mtu_probe() -> None:
    result = parse_tracepath_output("192.0.2.1", _TRACEPATH_TOO_MANY_HOPS, max_hops=8)
    assert all("[LOCALHOST]" not in host for hop in result.hops for host in hop.hosts)
    assert result.hops[0].number == 1


def test_tracepath_ignores_asymm_annotations() -> None:
    result = parse_tracepath_output("1.1.1.1", _TRACEPATH_ASYMM, max_hops=15)
    assert [hop.hosts for hop in result.hops] == [
        ("192.168.1.1",),
        ("65.19.100.4",),
        ("162.158.61.101",),
    ]
    assert result.hops[1].rtts_ms == (33.087,)


def test_tracepath_unknown_host_has_no_hops_and_an_error() -> None:
    result = parse_tracepath_output("nope.invalid", _TRACEPATH_UNKNOWN_HOST, max_hops=15)
    assert result.hops == ()
    assert result.error == "nope.invalid: Name or service not known"


def test_timeout_for_reflects_the_two_tools_probing_models() -> None:
    # traceroute -N 16 probes in batches, so a dark path costs one batch of waiting.
    assert timeout_for("traceroute") == 9
    assert timeout_for("traceroute", max_hops=30, queries=3) == 29
    # tracepath probes serially with no wait knob, so it scales with hop count.
    assert timeout_for("tracepath") == 27.5
    assert timeout_for("tracepath", max_hops=8) == 17.0


def _fake_which(present: set[str]) -> Any:
    return lambda tool: f"/usr/bin/{tool}" if tool in present else None


def test_select_tool_prefers_traceroute(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(tr.shutil, "which", _fake_which({"traceroute", "tracepath"}))
    assert select_tool() == "traceroute"


def test_select_tool_falls_back_to_tracepath(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(tr.shutil, "which", _fake_which({"tracepath"}))
    assert select_tool() == "tracepath"


def test_select_tool_reports_nothing_installed(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(tr.shutil, "which", _fake_which(set()))
    assert select_tool() == ""


def test_trace_without_either_binary_says_how_to_install_one(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr(tr.shutil, "which", _fake_which(set()))
    result = trace("8.8.8.8")
    assert result.tool == ""
    assert result.hops == ()
    assert result.error == MISSING_TOOLS_ERROR


class _FakeProc:
    def __init__(self, output: str, *, hang: bool = False) -> None:
        self._output = output
        self._hang = hang
        self.killed = False

    def communicate(self, timeout: float | None = None) -> tuple[str, None]:
        # The post-kill drain passes no timeout, so a hanging proc still yields
        # whatever the tool had already written.
        if self._hang and timeout is not None:
            raise subprocess.TimeoutExpired(cmd="trace", timeout=timeout)
        return self._output, None

    def kill(self) -> None:
        self.killed = True


def _fake_popen(
    monkeypatch: pytest.MonkeyPatch, proc: _FakeProc
) -> tuple[list[list[str]], _FakeProc]:
    seen: list[list[str]] = []

    def popen(argv: list[str], **_kwargs: Any) -> _FakeProc:
        seen.append(argv)
        return proc

    monkeypatch.setattr(tr.subprocess, "Popen", popen)
    return seen, proc


def test_trace_traceroute_argv(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(tr.shutil, "which", _fake_which({"traceroute", "tracepath"}))
    seen, _ = _fake_popen(monkeypatch, _FakeProc(_TRACEROUTE_REACHED))
    result = trace("8.8.8.8", max_hops=15, wait_s=1, queries=1)
    assert seen == [
        ["traceroute", "-n", "-q", "1", "-w", "1", "-N", "16", "-m", "15", "8.8.8.8"],
    ]
    assert result.tool == "traceroute"
    assert result.reached is True


def test_trace_tracepath_argv(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(tr.shutil, "which", _fake_which({"tracepath"}))
    seen, _ = _fake_popen(monkeypatch, _FakeProc(_TRACEPATH_REACHED))
    result = trace("192.168.1.1", max_hops=15)
    # tracepath takes no -q/-w equivalents.
    assert seen == [["tracepath", "-4", "-n", "-m", "15", "192.168.1.1"]]
    assert result.tool == "tracepath"
    assert result.reached is True


def test_trace_timeout_kills_the_child_and_keeps_partial_hops(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr(tr.shutil, "which", _fake_which({"traceroute"}))
    _, proc = _fake_popen(monkeypatch, _FakeProc(_TRACEROUTE_STALLED, hang=True))
    result = trace("192.0.2.1", max_hops=6)
    assert proc.killed is True
    # The hops collected before the wall clock are the diagnostic.
    assert result.last_responding_hop == 3
    assert result.stalled_at == 4
    assert result.error is not None
    assert "wall clock (partial)" in result.error


def test_trace_survives_a_binary_that_vanished_after_which(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr(tr.shutil, "which", _fake_which({"traceroute"}))

    def boom(*_args: Any, **_kwargs: Any) -> _FakeProc:
        raise OSError("No such file or directory")

    monkeypatch.setattr(tr.subprocess, "Popen", boom)
    result = trace("8.8.8.8")
    assert result.tool == "traceroute"
    assert result.hops == ()
    assert result.error is not None
    assert "traceroute unavailable" in result.error
