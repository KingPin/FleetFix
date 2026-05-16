"""DNS resolution check via getaddrinfo.

Quick triage check: feed in a list of hostnames the box is expected to
reach (database, auth service, S3 endpoint), get back which ones resolve
and which fail, with latency for the lookups that succeed.

This is intentionally lower-fidelity than `dig +trace` — the medium-value
"DNS deep check" suggested addition can layer that on later.
"""

from __future__ import annotations

import socket
import time
from collections.abc import Iterable
from dataclasses import dataclass


@dataclass(frozen=True)
class DnsResult:
    name: str
    ok: bool
    addresses: tuple[str, ...]
    latency_ms: float
    error: str | None = None


def resolve_one(name: str, *, timeout_s: float = 3.0) -> DnsResult:
    """Resolve `name` and time the lookup. Wraps getaddrinfo with timing."""
    start = time.perf_counter()
    prev_timeout = socket.getdefaulttimeout()
    socket.setdefaulttimeout(timeout_s)
    try:
        infos = socket.getaddrinfo(name, None, type=socket.SOCK_STREAM)
    except socket.gaierror as exc:
        return DnsResult(
            name=name,
            ok=False,
            addresses=(),
            latency_ms=(time.perf_counter() - start) * 1000.0,
            error=str(exc),
        )
    except OSError as exc:
        return DnsResult(
            name=name,
            ok=False,
            addresses=(),
            latency_ms=(time.perf_counter() - start) * 1000.0,
            error=str(exc),
        )
    finally:
        socket.setdefaulttimeout(prev_timeout)

    addrs = tuple(sorted({info[4][0] for info in infos}))
    return DnsResult(
        name=name,
        ok=True,
        addresses=addrs,
        latency_ms=(time.perf_counter() - start) * 1000.0,
    )


def resolve_many(names: Iterable[str], *, timeout_s: float = 3.0) -> list[DnsResult]:
    """Resolve a list of names sequentially. Order matches input."""
    return [resolve_one(name, timeout_s=timeout_s) for name in names]
