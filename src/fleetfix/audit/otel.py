"""OTLP log exporter for the audit trail.

Each audit record (one dict per local-log line) is forwarded as an OTEL
log record via a BatchLogRecordProcessor. The local JSON-lines file
stays authoritative — this sink is best-effort and swallows errors.

Configuration order (later wins):
  1. ``~/.config/fleetfix/otel.yml`` — keys: endpoint, headers, insecure,
     service_name.
  2. Environment variables:
       FLEETFIX_OTLP_ENDPOINT
       FLEETFIX_OTLP_HEADERS   (``k1=v1,k2=v2``)
       FLEETFIX_OTLP_INSECURE  (any non-empty value)
       FLEETFIX_OTLP_SERVICE   (resource service.name override)

If no endpoint is configured after both layers, ``make_sink()`` returns
``None`` and FleetFix runs with local-only auditing.
"""

from __future__ import annotations

import logging
import os
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import yaml

from fleetfix import __version__
from fleetfix.config import USER_CONFIG_DIR

_log = logging.getLogger(__name__)

OTEL_CONFIG_PATH = USER_CONFIG_DIR / "otel.yml"
DEFAULT_SERVICE_NAME = "fleetfix"
SHUTDOWN_TIMEOUT_MS = 2_000


@dataclass(frozen=True)
class OtelConfig:
    endpoint: str
    headers: dict[str, str] = field(default_factory=dict)
    insecure: bool = False
    service_name: str = DEFAULT_SERVICE_NAME


def _parse_headers_env(raw: str) -> dict[str, str]:
    out: dict[str, str] = {}
    for pair in raw.split(","):
        pair = pair.strip()
        if not pair or "=" not in pair:
            continue
        k, v = pair.split("=", 1)
        out[k.strip()] = v.strip()
    return out


def _read_yaml(path: Path) -> dict[str, Any]:
    try:
        text = path.read_text(encoding="utf-8")
    except OSError:
        return {}
    try:
        data = yaml.safe_load(text)
    except yaml.YAMLError as exc:
        _log.warning("failed to parse %s: %s", path, exc)
        return {}
    return data if isinstance(data, dict) else {}


def load_otel_config(
    *,
    path: Path | None = None,
    env: dict[str, str] | None = None,
) -> OtelConfig | None:
    """Resolve config from YAML + env; return None if endpoint is missing."""
    env = env if env is not None else dict(os.environ)
    cfg_path = path if path is not None else OTEL_CONFIG_PATH
    data = _read_yaml(cfg_path)

    endpoint = data.get("endpoint") or ""
    headers_raw = data.get("headers") or {}
    headers: dict[str, str] = (
        {str(k): str(v) for k, v in headers_raw.items()} if isinstance(headers_raw, dict) else {}
    )
    insecure = bool(data.get("insecure", False))
    service_name = str(data.get("service_name") or DEFAULT_SERVICE_NAME)

    env_endpoint = env.get("FLEETFIX_OTLP_ENDPOINT")
    if env_endpoint:
        endpoint = env_endpoint
    env_headers = env.get("FLEETFIX_OTLP_HEADERS")
    if env_headers:
        headers = {**headers, **_parse_headers_env(env_headers)}
    if env.get("FLEETFIX_OTLP_INSECURE"):
        insecure = True
    env_service = env.get("FLEETFIX_OTLP_SERVICE")
    if env_service:
        service_name = env_service

    if not endpoint:
        return None
    return OtelConfig(
        endpoint=endpoint,
        headers=headers,
        insecure=insecure,
        service_name=service_name,
    )


def _flatten_attributes(record: dict[str, Any]) -> dict[str, Any]:
    """Promote audit fields to OTEL attributes; flat string/scalar values only."""
    out: dict[str, Any] = {
        "fleetfix.action": record.get("action"),
        "fleetfix.phase": record.get("phase"),
        "fleetfix.session_id": record.get("session_id"),
        "fleetfix.call_id": record.get("call_id"),
        "fleetfix.seq": record.get("seq"),
        "fleetfix.host": record.get("host"),
        "fleetfix.version": record.get("fleetfix_version"),
    }
    operator = record.get("operator") or {}
    if operator.get("unix_user") is not None:
        out["fleetfix.operator.unix_user"] = operator["unix_user"]
    if operator.get("duo_principal") is not None:
        out["fleetfix.operator.duo_principal"] = operator["duo_principal"]
    if operator.get("source_ip") is not None:
        out["fleetfix.operator.source_ip"] = operator["source_ip"]
    result = record.get("result")
    if isinstance(result, dict):
        if "ok" in result:
            out["fleetfix.result.ok"] = bool(result["ok"])
        if result.get("error"):
            out["fleetfix.result.error"] = str(result["error"])
    return {k: v for k, v in out.items() if v is not None}


def _severity_for(record: dict[str, Any]) -> tuple[int, str]:
    """Map audit record to OTEL severity (number, text)."""
    from opentelemetry._logs import SeverityNumber

    result = record.get("result")
    if isinstance(result, dict) and result.get("ok") is False:
        return int(SeverityNumber.ERROR.value), "ERROR"
    return int(SeverityNumber.INFO.value), "INFO"


class OtelSink:
    """Best-effort OTEL log forwarder for audit records.

    The provider/exporter live on this instance only — we do NOT call
    ``set_logger_provider`` so we don't fight with other OTEL users that
    a host might have installed.
    """

    def __init__(self, config: OtelConfig, *, _provider: Any = None) -> None:
        self.config = config
        if _provider is not None:
            self._provider = _provider
        else:
            self._provider = self._build_provider(config)
        self._logger = self._provider.get_logger("fleetfix.audit", __version__)

    @staticmethod
    def _build_provider(config: OtelConfig) -> Any:
        from opentelemetry.exporter.otlp.proto.grpc._log_exporter import OTLPLogExporter
        from opentelemetry.sdk._logs import LoggerProvider
        from opentelemetry.sdk._logs.export import BatchLogRecordProcessor
        from opentelemetry.sdk.resources import Resource

        resource = Resource.create({"service.name": config.service_name})
        exporter = OTLPLogExporter(
            endpoint=config.endpoint,
            headers=config.headers or None,
            insecure=config.insecure,
        )
        provider = LoggerProvider(resource=resource)
        provider.add_log_record_processor(BatchLogRecordProcessor(exporter))
        return provider

    def emit(self, record: dict[str, Any]) -> None:
        try:
            severity_number, severity_text = _severity_for(record)
            self._logger.emit(
                timestamp=time.time_ns(),
                severity_number=severity_number,  # type: ignore[arg-type]
                severity_text=severity_text,
                body=record,
                attributes=_flatten_attributes(record),
            )
        except Exception:
            _log.exception("failed to emit audit record to OTEL")

    def shutdown(self, timeout_ms: int = SHUTDOWN_TIMEOUT_MS) -> None:
        try:
            self._provider.force_flush(timeout_millis=timeout_ms)
        except Exception:
            _log.exception("OTEL provider flush failed")
        try:
            self._provider.shutdown()
        except Exception:
            _log.exception("OTEL provider shutdown failed")


def make_sink(config: OtelConfig | None) -> OtelSink | None:
    """Build a sink from config, or return None when no endpoint is set."""
    if config is None:
        return None
    try:
        return OtelSink(config)
    except Exception:
        _log.exception("failed to construct OTEL sink; running local-only")
        return None
