"""Unit tests for the curl probe module."""

from __future__ import annotations

import subprocess
from typing import Any

import pytest

from fleetfix.modules.network import curl_probe
from fleetfix.modules.network.curl_probe import parse_curl_output, probe

_HEALTHY = """\
FLEETFIX_CURL_PROBE
http_code=200
time_namelookup=0.001234
time_connect=0.012345
time_appconnect=0.045678
time_starttransfer=0.098765
time_total=0.123456
size_download=4096
"""

_NOT_FOUND = """\
FLEETFIX_CURL_PROBE
http_code=404
time_namelookup=0.000100
time_connect=0.001000
time_appconnect=0.000000
time_starttransfer=0.005000
time_total=0.006000
size_download=120
"""


def test_parse_healthy() -> None:
    result = parse_curl_output("https://x", _HEALTHY)
    assert result is not None
    assert result.ok is True
    assert result.http_code == 200
    assert result.time_total_s == pytest.approx(0.123456)
    assert result.time_namelookup_s == pytest.approx(0.001234)
    assert result.size_download_bytes == 4096


def test_parse_4xx_marked_not_ok() -> None:
    result = parse_curl_output("https://x", _NOT_FOUND)
    assert result is not None
    assert result.ok is False
    assert result.http_code == 404


def test_parse_missing_marker_returns_none() -> None:
    assert parse_curl_output("https://x", "stderr output, no marker\n") is None


def test_parse_partial_fields_returns_none() -> None:
    bad = "FLEETFIX_CURL_PROBE\nhttp_code=200\n"
    assert parse_curl_output("https://x", bad) is None


def test_probe_returns_parsed_on_success(monkeypatch: pytest.MonkeyPatch) -> None:
    def fake_run(*args: Any, **kwargs: Any) -> subprocess.CompletedProcess:
        return subprocess.CompletedProcess(args=args, returncode=0, stdout=_HEALTHY, stderr="")

    monkeypatch.setattr(curl_probe.subprocess, "run", fake_run)
    result = probe("https://api.internal/health")
    assert result.ok is True
    assert result.url == "https://api.internal/health"


def test_probe_handles_timeout(monkeypatch: pytest.MonkeyPatch) -> None:
    def boom(*args: Any, **kwargs: Any) -> subprocess.CompletedProcess:
        raise subprocess.TimeoutExpired(cmd=args[0], timeout=kwargs.get("timeout", 0))

    monkeypatch.setattr(curl_probe.subprocess, "run", boom)
    result = probe("https://slow.example")
    assert result.ok is False
    assert "timeout" in (result.error or "").lower()


def test_probe_handles_missing_binary(monkeypatch: pytest.MonkeyPatch) -> None:
    def boom(*args: Any, **kwargs: Any) -> subprocess.CompletedProcess:
        raise FileNotFoundError("no curl")

    monkeypatch.setattr(curl_probe.subprocess, "run", boom)
    result = probe("https://x")
    assert result.ok is False
    assert "unavailable" in (result.error or "")


def test_probe_returns_failure_when_no_marker_present(monkeypatch: pytest.MonkeyPatch) -> None:
    def fake_run(*args: Any, **kwargs: Any) -> subprocess.CompletedProcess:
        return subprocess.CompletedProcess(
            args=args,
            returncode=6,
            stdout="",
            stderr="curl: (6) Could not resolve host: nope.example\n",
        )

    monkeypatch.setattr(curl_probe.subprocess, "run", fake_run)
    result = probe("https://nope.example")
    assert result.ok is False
    assert "Could not resolve host" in (result.error or "")
