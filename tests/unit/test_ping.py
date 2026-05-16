"""Unit tests for the ping output parser."""

from __future__ import annotations

import subprocess
from typing import Any

import pytest

from fleetfix.modules.network import ping
from fleetfix.modules.network.ping import parse_ping_output, run_ping

_HEALTHY_UBUNTU = """\
PING 8.8.8.8 (8.8.8.8) 56(84) bytes of data.
64 bytes from 8.8.8.8: icmp_seq=1 ttl=115 time=12.4 ms
64 bytes from 8.8.8.8: icmp_seq=2 ttl=115 time=11.9 ms
64 bytes from 8.8.8.8: icmp_seq=3 ttl=115 time=12.1 ms

--- 8.8.8.8 ping statistics ---
3 packets transmitted, 3 received, 0% packet loss, time 412ms
rtt min/avg/max/mdev = 11.913/12.144/12.396/0.198 ms
"""

_PARTIAL_LOSS = """\
PING flaky.internal (10.0.0.5) 56(84) bytes of data.
64 bytes from 10.0.0.5: icmp_seq=1 ttl=64 time=2.40 ms

--- flaky.internal ping statistics ---
5 packets transmitted, 2 received, 60% packet loss, time 4096ms
rtt min/avg/max/mdev = 2.401/3.118/3.835/0.717 ms
"""

_TOTAL_LOSS = """\
PING unreachable (10.99.0.1) 56(84) bytes of data.

--- unreachable ping statistics ---
4 packets transmitted, 0 received, 100% packet loss, time 3076ms
"""

_DEBIAN_WITH_ERRORS = """\
PING router (10.0.0.1) 56(84) bytes of data.
From 10.0.0.7 icmp_seq=1 Destination Host Unreachable
64 bytes from 10.0.0.1: icmp_seq=2 ttl=64 time=0.901 ms

--- router ping statistics ---
4 packets transmitted, 3 received, +1 errors, 25% packet loss, time 3050ms
rtt min/avg/max/mdev = 0.901/1.500/2.250/0.580 ms
"""


def test_parse_healthy_summary() -> None:
    summary = parse_ping_output("8.8.8.8", _HEALTHY_UBUNTU)
    assert summary is not None
    assert summary.sent == 3
    assert summary.received == 3
    assert summary.loss_pct == 0.0
    assert summary.rtt_avg_ms == pytest.approx(12.144)
    assert summary.jitter_ms == pytest.approx(0.198)


def test_parse_partial_loss() -> None:
    summary = parse_ping_output("flaky.internal", _PARTIAL_LOSS)
    assert summary is not None
    assert summary.sent == 5
    assert summary.received == 2
    assert summary.loss_pct == 60.0
    assert summary.rtt_min_ms == pytest.approx(2.401)
    assert summary.rtt_max_ms == pytest.approx(3.835)


def test_parse_total_loss_has_zero_rtt() -> None:
    summary = parse_ping_output("unreachable", _TOTAL_LOSS)
    assert summary is not None
    assert summary.loss_pct == 100.0
    assert summary.received == 0
    assert summary.rtt_avg_ms == 0.0
    assert summary.rtt_mdev_ms == 0.0


def test_parse_debian_with_errors_field() -> None:
    summary = parse_ping_output("router", _DEBIAN_WITH_ERRORS)
    assert summary is not None
    assert summary.sent == 4
    assert summary.received == 3
    assert summary.loss_pct == 25.0


def test_parse_unrecognised_output_returns_none() -> None:
    assert parse_ping_output("x", "nothing useful here\n") is None


def test_run_ping_returns_summary_on_success(monkeypatch: pytest.MonkeyPatch) -> None:
    def fake_run(*args: Any, **kwargs: Any) -> subprocess.CompletedProcess:
        return subprocess.CompletedProcess(args=args, returncode=0, stdout=_HEALTHY_UBUNTU)

    monkeypatch.setattr(ping.subprocess, "run", fake_run)
    summary = run_ping("8.8.8.8", count=3, interval_s=0.2)
    assert summary is not None
    assert summary.target == "8.8.8.8"
    assert summary.sent == 3


def test_run_ping_returns_none_on_timeout(monkeypatch: pytest.MonkeyPatch) -> None:
    def boom(*args: Any, **kwargs: Any) -> subprocess.CompletedProcess:
        raise subprocess.TimeoutExpired(cmd=args[0], timeout=kwargs.get("timeout", 0))

    monkeypatch.setattr(ping.subprocess, "run", boom)
    assert run_ping("slow.example.com") is None


def test_run_ping_returns_none_when_binary_missing(monkeypatch: pytest.MonkeyPatch) -> None:
    def boom(*args: Any, **kwargs: Any) -> subprocess.CompletedProcess:
        raise FileNotFoundError("ping not installed")

    monkeypatch.setattr(ping.subprocess, "run", boom)
    assert run_ping("8.8.8.8") is None
