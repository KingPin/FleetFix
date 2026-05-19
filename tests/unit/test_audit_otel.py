"""Unit tests for the OTEL audit sink — config loading + record translation."""

from __future__ import annotations

from pathlib import Path
from typing import Any

import pytest

from fleetfix.audit.otel import (
    OtelConfig,
    OtelSink,
    _flatten_attributes,
    _parse_headers_env,
    _severity_for,
    load_otel_config,
    make_sink,
)


def test_parse_headers_env_basic() -> None:
    out = _parse_headers_env("k1=v1, k2= v2 ,bad,")
    assert out == {"k1": "v1", "k2": "v2"}


def test_parse_headers_env_empty() -> None:
    assert _parse_headers_env("") == {}


def test_load_otel_config_returns_none_when_unset(tmp_path: Path) -> None:
    cfg = load_otel_config(path=tmp_path / "missing.yml", env={})
    assert cfg is None


def test_load_otel_config_reads_yaml(tmp_path: Path) -> None:
    p = tmp_path / "otel.yml"
    p.write_text(
        "endpoint: https://ingest.example.com:443\n"
        "service_name: fleetfix-test\n"
        "headers:\n"
        "  x-otlp-token: abc\n"
        "insecure: false\n",
        encoding="utf-8",
    )
    cfg = load_otel_config(path=p, env={})
    assert cfg is not None
    assert cfg.endpoint == "https://ingest.example.com:443"
    assert cfg.headers == {"x-otlp-token": "abc"}
    assert cfg.insecure is False
    assert cfg.service_name == "fleetfix-test"


def test_load_otel_config_env_overrides_yaml(tmp_path: Path) -> None:
    p = tmp_path / "otel.yml"
    p.write_text(
        "endpoint: https://ingest.example.com:443\nheaders:\n  a: '1'\n",
        encoding="utf-8",
    )
    cfg = load_otel_config(
        path=p,
        env={
            "FLEETFIX_OTLP_ENDPOINT": "https://override.local:4317",
            "FLEETFIX_OTLP_HEADERS": "b=2,c=3",
            "FLEETFIX_OTLP_INSECURE": "1",
            "FLEETFIX_OTLP_SERVICE": "fleetfix-prod",
        },
    )
    assert cfg is not None
    assert cfg.endpoint == "https://override.local:4317"
    # Env headers merge on top of YAML headers.
    assert cfg.headers == {"a": "1", "b": "2", "c": "3"}
    assert cfg.insecure is True
    assert cfg.service_name == "fleetfix-prod"


def test_load_otel_config_skips_malformed_yaml(tmp_path: Path) -> None:
    p = tmp_path / "otel.yml"
    p.write_text("endpoint: [unterminated\n", encoding="utf-8")
    cfg = load_otel_config(path=p, env={"FLEETFIX_OTLP_ENDPOINT": "https://x:443"})
    assert cfg is not None
    assert cfg.endpoint == "https://x:443"


def test_load_otel_config_yaml_not_a_mapping(tmp_path: Path) -> None:
    p = tmp_path / "otel.yml"
    p.write_text("- just\n- a\n- list\n", encoding="utf-8")
    assert load_otel_config(path=p, env={}) is None


def test_flatten_attributes_promotes_known_fields() -> None:
    rec = {
        "action": "docker.truncate_log",
        "phase": "result",
        "session_id": "s1",
        "call_id": "c1",
        "seq": 7,
        "host": "web-01",
        "fleetfix_version": "0.1.0",
        "operator": {"unix_user": "operator", "auth_principal": None, "source_ip": "10.0.0.1"},
        "result": {"ok": True, "error": None, "bytes_freed": 1234},
    }
    attrs = _flatten_attributes(rec)
    assert attrs["fleetfix.action"] == "docker.truncate_log"
    assert attrs["fleetfix.phase"] == "result"
    assert attrs["fleetfix.session_id"] == "s1"
    assert attrs["fleetfix.seq"] == 7
    assert attrs["fleetfix.host"] == "web-01"
    assert attrs["fleetfix.version"] == "0.1.0"
    assert attrs["fleetfix.operator.unix_user"] == "operator"
    assert attrs["fleetfix.operator.source_ip"] == "10.0.0.1"
    # auth_principal=None should be dropped, not emitted as a null attr.
    assert "fleetfix.operator.auth_principal" not in attrs
    assert attrs["fleetfix.result.ok"] is True


def test_flatten_attributes_records_error_field() -> None:
    rec = {
        "action": "x.y",
        "result": {"ok": False, "error": "boom"},
    }
    attrs = _flatten_attributes(rec)
    assert attrs["fleetfix.result.ok"] is False
    assert attrs["fleetfix.result.error"] == "boom"


def test_flatten_attributes_drops_none_top_level() -> None:
    rec = {"action": "x", "phase": "intent"}
    attrs = _flatten_attributes(rec)
    assert "fleetfix.host" not in attrs
    assert attrs["fleetfix.action"] == "x"


def test_flatten_attributes_promotes_inspect_target() -> None:
    rec = {"action": "x", "inspect_target": "appuser"}
    attrs = _flatten_attributes(rec)
    assert attrs["fleetfix.inspect_target"] == "appuser"


def test_flatten_attributes_drops_null_inspect_target() -> None:
    rec = {"action": "x", "inspect_target": None}
    attrs = _flatten_attributes(rec)
    assert "fleetfix.inspect_target" not in attrs


def test_flatten_attributes_no_inspect_target_key_at_all() -> None:
    rec = {"action": "x", "phase": "intent"}
    attrs = _flatten_attributes(rec)
    assert "fleetfix.inspect_target" not in attrs


def test_severity_for_default_info() -> None:
    sev_num, sev_text = _severity_for({"action": "x"})
    assert sev_text == "INFO"
    assert sev_num > 0


def test_severity_for_failure_error() -> None:
    _, sev_text = _severity_for({"action": "x", "result": {"ok": False}})
    assert sev_text == "ERROR"


class _FakeLogger:
    def __init__(self) -> None:
        self.emitted: list[dict[str, Any]] = []

    def emit(self, **kwargs: Any) -> None:
        self.emitted.append(kwargs)


class _FakeProvider:
    def __init__(self) -> None:
        self.fake_logger = _FakeLogger()
        self.flushed = False
        self.shut = False

    def get_logger(self, name: str, version: str | None = None) -> _FakeLogger:
        return self.fake_logger

    def force_flush(self, timeout_millis: int) -> None:
        self.flushed = True

    def shutdown(self) -> None:
        self.shut = True


def test_otel_sink_emits_record_with_attributes() -> None:
    provider = _FakeProvider()
    sink = OtelSink(
        OtelConfig(endpoint="x", headers={}, insecure=True),
        _provider=provider,
    )
    rec = {
        "action": "docker.prune",
        "phase": "result",
        "session_id": "s1",
        "call_id": "c1",
        "seq": 1,
        "host": "h",
        "fleetfix_version": "0.1.0",
        "operator": {"unix_user": "appuser"},
        "result": {"ok": True},
    }
    sink.emit(rec)
    assert len(provider.fake_logger.emitted) == 1
    call = provider.fake_logger.emitted[0]
    assert call["body"] == rec
    assert call["severity_text"] == "INFO"
    assert call["attributes"]["fleetfix.action"] == "docker.prune"
    assert call["attributes"]["fleetfix.result.ok"] is True


def test_otel_sink_swallows_emit_errors() -> None:
    class _ExplodingLogger:
        def emit(self, **kwargs: Any) -> None:
            raise RuntimeError("boom")

    class _ExplodingProvider:
        def get_logger(self, name: str, version: str | None = None) -> _ExplodingLogger:
            return _ExplodingLogger()

        def force_flush(self, timeout_millis: int) -> None:
            pass

        def shutdown(self) -> None:
            pass

    sink = OtelSink(
        OtelConfig(endpoint="x", headers={}, insecure=True),
        _provider=_ExplodingProvider(),
    )
    # Must not raise — local file remains authoritative when remote fails.
    sink.emit({"action": "x"})


def test_otel_sink_shutdown_calls_flush_and_shutdown() -> None:
    provider = _FakeProvider()
    sink = OtelSink(
        OtelConfig(endpoint="x", headers={}, insecure=True),
        _provider=provider,
    )
    sink.shutdown(timeout_ms=500)
    assert provider.flushed is True
    assert provider.shut is True


def test_make_sink_returns_none_when_no_config() -> None:
    assert make_sink(None) is None


def test_make_sink_failure_returns_none(monkeypatch: pytest.MonkeyPatch) -> None:
    def explode(self: OtelSink, config: OtelConfig) -> None:
        raise RuntimeError("no exporter available")

    monkeypatch.setattr(OtelSink, "__init__", explode)
    cfg = OtelConfig(endpoint="https://x:443", headers={}, insecure=False)
    assert make_sink(cfg) is None
