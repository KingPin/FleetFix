"""Unit tests for the DNS resolver — getaddrinfo is fully stubbed."""

from __future__ import annotations

import socket
from typing import Any

import pytest

from fleetfix.modules.network import dns
from fleetfix.modules.network.dns import resolve_many, resolve_one


def test_resolve_one_success(monkeypatch: pytest.MonkeyPatch) -> None:
    def fake(*args: Any, **kwargs: Any) -> list[Any]:
        return [
            (socket.AF_INET, socket.SOCK_STREAM, 0, "", ("10.0.0.5", 0)),
            (socket.AF_INET, socket.SOCK_STREAM, 0, "", ("10.0.0.6", 0)),
        ]

    monkeypatch.setattr(dns.socket, "getaddrinfo", fake)
    result = resolve_one("svc.internal")
    assert result.ok is True
    assert result.addresses == ("10.0.0.5", "10.0.0.6")
    assert result.latency_ms >= 0


def test_resolve_one_dedupes_addresses(monkeypatch: pytest.MonkeyPatch) -> None:
    def fake(*args: Any, **kwargs: Any) -> list[Any]:
        return [
            (socket.AF_INET, socket.SOCK_STREAM, 0, "", ("10.0.0.5", 0)),
            (socket.AF_INET, socket.SOCK_DGRAM, 0, "", ("10.0.0.5", 0)),
        ]

    monkeypatch.setattr(dns.socket, "getaddrinfo", fake)
    result = resolve_one("svc.internal")
    assert result.addresses == ("10.0.0.5",)


def test_resolve_one_handles_nxdomain(monkeypatch: pytest.MonkeyPatch) -> None:
    def fake(*args: Any, **kwargs: Any) -> list[Any]:
        raise socket.gaierror("Name or service not known")

    monkeypatch.setattr(dns.socket, "getaddrinfo", fake)
    result = resolve_one("nope.example")
    assert result.ok is False
    assert result.addresses == ()
    assert "not known" in (result.error or "")


def test_resolve_one_handles_os_error(monkeypatch: pytest.MonkeyPatch) -> None:
    def fake(*args: Any, **kwargs: Any) -> list[Any]:
        raise OSError("resolver unreachable")

    monkeypatch.setattr(dns.socket, "getaddrinfo", fake)
    result = resolve_one("x")
    assert result.ok is False


def test_resolve_many_preserves_order(monkeypatch: pytest.MonkeyPatch) -> None:
    seen: list[str] = []

    def fake(host: str, *args: Any, **kwargs: Any) -> list[Any]:
        seen.append(host)
        if host == "fail.example":
            raise socket.gaierror("nope")
        return [(socket.AF_INET, socket.SOCK_STREAM, 0, "", ("1.1.1.1", 0))]

    monkeypatch.setattr(dns.socket, "getaddrinfo", fake)
    results = resolve_many(["ok.example", "fail.example", "other.example"])
    assert [r.name for r in results] == ["ok.example", "fail.example", "other.example"]
    assert seen == ["ok.example", "fail.example", "other.example"]
    assert results[1].ok is False
