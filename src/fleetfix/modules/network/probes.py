"""Per-host network probe targets, from `~/.config/fleetfix/probes.yml`.

The file is entirely optional: `DEFAULT_PROBES` is a deliberately boring set of
public targets, so a freshly-imaged host with no config is fully functional and
`probes.yml` is only for saying "on *this* box, check these instead".

Nothing here raises. A missing file, invalid YAML, a top-level list, a junk
scalar, or an out-of-range number all degrade to the default for that one field —
a broken config file must never be the reason the Network screen won't open.

The loader lives here rather than in `config.py` because the schema is richer than
anything there and it needs `tcp.parse_host_port` (same self-contained-loader shape
as `audit/otel.py`). `modules/` importing `fleetfix.config` is already precedented
by `modules/disk/blacklist.py`; the boundary rule is "no Textual", not "no config".
"""

from __future__ import annotations

import logging
from dataclasses import dataclass, replace
from pathlib import Path
from typing import Any

from fleetfix.config import PROBES_CONFIG_PATH, read_probes_yaml

from .tcp import TcpTarget, parse_host_port

_log = logging.getLogger(__name__)


@dataclass(frozen=True)
class PingProbes:
    targets: tuple[str, ...]
    count: int
    interval_s: float
    timeout_s: int


@dataclass(frozen=True)
class DnsProbes:
    names: tuple[str, ...]
    timeout_s: float


@dataclass(frozen=True)
class HttpProbes:
    urls: tuple[str, ...]
    timeout_s: int
    max_redirects: int


@dataclass(frozen=True)
class TcpProbes:
    targets: tuple[TcpTarget, ...]
    timeout_s: float


@dataclass(frozen=True)
class TracerouteProbes:
    max_hops: int
    wait_s: int
    queries: int


@dataclass(frozen=True)
class LadderProbes:
    """The single target per layer that the connectivity ladder walks."""

    internet_target: str
    dns_name: str
    https_url: str


@dataclass(frozen=True)
class Probes:
    ping: PingProbes
    dns: DnsProbes
    http: HttpProbes
    tcp: TcpProbes
    traceroute: TracerouteProbes
    ladder: LadderProbes


DEFAULT_PROBES = Probes(
    ping=PingProbes(targets=("8.8.8.8", "1.1.1.1"), count=10, interval_s=0.2, timeout_s=15),
    dns=DnsProbes(names=("github.com", "archive.ubuntu.com"), timeout_s=3.0),
    http=HttpProbes(urls=("https://github.com",), timeout_s=15, max_redirects=5),
    tcp=TcpProbes(targets=(TcpTarget("github.com", 443),), timeout_s=3.0),
    traceroute=TracerouteProbes(max_hops=15, wait_s=1, queries=1),
    ladder=LadderProbes(
        internet_target="8.8.8.8",
        dns_name="github.com",
        https_url="https://github.com",
    ),
)

# Clamps exist because these numbers become subprocess arguments. `ping.count:
# 100000` with `interval_s: 0` is a self-inflicted DoS on the operator's own box,
# and a fat-fingered config should not be able to cause one.
_COUNT_RANGE = (1, 900)
_PING_INTERVAL_RANGE = (0.05, 5.0)
_PING_TIMEOUT_RANGE = (1, 300)
_DNS_TIMEOUT_RANGE = (0.1, 30.0)
_HTTP_TIMEOUT_RANGE = (1, 120)
_REDIRECT_RANGE = (0, 20)
_TCP_TIMEOUT_RANGE = (0.1, 30.0)
_MAX_HOPS_RANGE = (1, 30)
_TRACE_WAIT_RANGE = (1, 10)
_TRACE_QUERIES_RANGE = (1, 3)


def resolve_probes(*, probes_cfg: dict[str, Any] | None = None) -> Probes:
    """Merge a probes.yml mapping over DEFAULT_PROBES. Never raises.

    **Scalar knobs merge per-key; target lists replace wholesale.** The asymmetry
    is deliberate. A list is a fleet inventory, not a suggestion: appending our
    `github.com` to an operator's `[api.internal, db.internal]` would put a
    permanently-red row on an egress-filtered host, and a check that is always
    red trains operators to ignore red. Scalars are independent knobs, so
    `ping: {count: 3}` means "shorter ping", not "also zero the interval".

    The escape hatch is presence, not truthiness: `targets: []` is an explicit
    "no presets on this box" and is honoured as empty.
    """
    cfg = probes_cfg or {}
    default = DEFAULT_PROBES

    ping_cfg = _section(cfg, "ping")
    ping = replace(
        default.ping,
        targets=_strs(ping_cfg, "targets", default.ping.targets),
        count=_int(ping_cfg, "count", default.ping.count, _COUNT_RANGE),
        interval_s=_float(ping_cfg, "interval_s", default.ping.interval_s, _PING_INTERVAL_RANGE),
        timeout_s=_int(ping_cfg, "timeout_s", default.ping.timeout_s, _PING_TIMEOUT_RANGE),
    )

    dns_cfg = _section(cfg, "dns")
    dns = replace(
        default.dns,
        names=_strs(dns_cfg, "names", default.dns.names),
        timeout_s=_float(dns_cfg, "timeout_s", default.dns.timeout_s, _DNS_TIMEOUT_RANGE),
    )

    http_cfg = _section(cfg, "http")
    http = replace(
        default.http,
        urls=_strs(http_cfg, "urls", default.http.urls),
        timeout_s=_int(http_cfg, "timeout_s", default.http.timeout_s, _HTTP_TIMEOUT_RANGE),
        max_redirects=_int(http_cfg, "max_redirects", default.http.max_redirects, _REDIRECT_RANGE),
    )

    tcp_cfg = _section(cfg, "tcp")
    tcp = replace(
        default.tcp,
        targets=_tcp_targets(tcp_cfg, default.tcp.targets),
        timeout_s=_float(tcp_cfg, "timeout_s", default.tcp.timeout_s, _TCP_TIMEOUT_RANGE),
    )

    trace_cfg = _section(cfg, "traceroute")
    traceroute = replace(
        default.traceroute,
        max_hops=_int(trace_cfg, "max_hops", default.traceroute.max_hops, _MAX_HOPS_RANGE),
        wait_s=_int(trace_cfg, "wait_s", default.traceroute.wait_s, _TRACE_WAIT_RANGE),
        queries=_int(trace_cfg, "queries", default.traceroute.queries, _TRACE_QUERIES_RANGE),
    )

    ladder_cfg = _section(cfg, "ladder")
    ladder = replace(
        default.ladder,
        internet_target=_str(ladder_cfg, "internet_target", default.ladder.internet_target),
        dns_name=_str(ladder_cfg, "dns_name", default.ladder.dns_name),
        https_url=_str(ladder_cfg, "https_url", default.ladder.https_url),
    )

    return Probes(ping=ping, dns=dns, http=http, tcp=tcp, traceroute=traceroute, ladder=ladder)


def load_probes(*, path: Path | None = None) -> Probes:
    """Read and resolve probes.yml. What app.py calls once at startup."""
    return resolve_probes(probes_cfg=read_probes_yaml(PROBES_CONFIG_PATH if path is None else path))


def _section(cfg: dict[str, Any], key: str) -> dict[str, Any]:
    """One section of the config, or {} if it isn't a mapping."""
    value = cfg.get(key)
    if value is None:
        return {}
    if not isinstance(value, dict):
        _log.warning("probes.yml: %s must be a mapping, ignoring it", key)
        return {}
    return value


def _strs(section: dict[str, Any], key: str, fallback: tuple[str, ...]) -> tuple[str, ...]:
    # Presence, not truthiness: an empty list means "no presets", not "use ours".
    if key not in section:
        return fallback
    value = section[key]
    if not isinstance(value, list):
        _log.warning("probes.yml: %s must be a list, using defaults", key)
        return fallback
    return tuple(str(item).strip() for item in value if str(item).strip())


def _tcp_targets(section: dict[str, Any], fallback: tuple[TcpTarget, ...]) -> tuple[TcpTarget, ...]:
    if "targets" not in section:
        return fallback
    raw = section["targets"]
    if not isinstance(raw, list):
        _log.warning("probes.yml: tcp.targets must be a list, using defaults")
        return fallback
    targets: list[TcpTarget] = []
    for item in raw:
        parsed = parse_host_port(str(item))
        if parsed is None:
            # Dropped rather than guessed: probing port 80 when the operator
            # meant 5432 would be confidently wrong.
            _log.warning("probes.yml: tcp target %r needs a host:port, skipping", item)
            continue
        targets.append(parsed)
    return tuple(targets)


def _str(section: dict[str, Any], key: str, fallback: str) -> str:
    value = section.get(key)
    if not isinstance(value, str) or not value.strip():
        return fallback
    return value.strip()


def _int(section: dict[str, Any], key: str, fallback: int, bounds: tuple[int, int]) -> int:
    value = section.get(key)
    # bool is an int subclass, and `count: true` is junk, not 1.
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        if value is not None:
            _log.warning("probes.yml: %s must be a number, using %r", key, fallback)
        return fallback
    return _clamp_int(int(value), bounds, key)


def _float(
    section: dict[str, Any], key: str, fallback: float, bounds: tuple[float, float]
) -> float:
    value = section.get(key)
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        if value is not None:
            _log.warning("probes.yml: %s must be a number, using %r", key, fallback)
        return fallback
    return _clamp_float(float(value), bounds, key)


def _clamp_int(value: int, bounds: tuple[int, int], key: str) -> int:
    low, high = bounds
    clamped = max(low, min(high, value))
    if clamped != value:
        _log.warning("probes.yml: %s=%s clamped to %s", key, value, clamped)
    return clamped


def _clamp_float(value: float, bounds: tuple[float, float], key: str) -> float:
    low, high = bounds
    clamped = max(low, min(high, value))
    if clamped != value:
        _log.warning("probes.yml: %s=%s clamped to %s", key, value, clamped)
    return clamped
