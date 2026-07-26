"""Unit tests for the ping output parser."""

from __future__ import annotations

import subprocess
from typing import Any

import pytest

from fleetfix.modules.network import ping
from fleetfix.modules.network.ping import parse_ping_output, run_ping
from tests.support.fixtures import fixture

_HEALTHY_UBUNTU = fixture("ping/ubuntu_no_loss.txt")

_PARTIAL_LOSS = fixture("ping/partial_loss.txt")

_TOTAL_LOSS = fixture("ping/total_loss.txt")

_DEBIAN_WITH_ERRORS = fixture("ping/debian_plus_errors.txt")


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
    assert parse_ping_output("x", fixture("ping/unrecognised.txt")) is None


def test_parse_keeps_the_raw_output() -> None:
    summary = parse_ping_output("8.8.8.8", _HEALTHY_UBUNTU)
    assert summary is not None
    assert summary.raw == _HEALTHY_UBUNTU


def test_parse_keeps_the_raw_output_on_total_loss() -> None:
    # The 100%-loss path synthesizes its rtt block, so it needs its own assert.
    summary = parse_ping_output("192.0.2.1", _TOTAL_LOSS)
    assert summary is not None
    assert summary.raw == _TOTAL_LOSS


def test_run_ping_returns_summary_on_success(monkeypatch: pytest.MonkeyPatch) -> None:
    def fake_run(*args: Any, **kwargs: Any) -> subprocess.CompletedProcess:
        return subprocess.CompletedProcess(args=args, returncode=0, stdout=_HEALTHY_UBUNTU)

    monkeypatch.setattr(ping.subprocess, "run", fake_run)
    summary = run_ping("8.8.8.8", count=3, interval_s=0.2)
    assert summary is not None
    assert summary.target == "8.8.8.8"
    assert summary.sent == 3
    assert summary.raw == _HEALTHY_UBUNTU


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
