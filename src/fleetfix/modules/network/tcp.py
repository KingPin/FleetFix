"""Outbound TCP reachability — "can this box reach that host:port?"

`sockets.py` answers the inbound question (what is listening *on* me). This
answers the outbound one, which is what an operator actually asks when an app
cannot reach its database: is the port open, actively refused, silently dropped,
or is the *name* the problem?

Pure Python — `socket.connect_ex` with a timeout. No nc, no nmap, no binary to
be missing on a minimal host.
"""

from __future__ import annotations

import errno
import os
import socket
import time
from dataclasses import dataclass

OPEN = "open"
REFUSED = "refused"
TIMEOUT = "timeout"
UNREACHABLE = "unreachable"
DNS_ERROR = "dns-error"
ERROR = "error"

# connect_ex returns an errno rather than raising. EAGAIN is the surprising one:
# it is what a non-blocking connect reports when settimeout() elapses, *not*
# ETIMEDOUT (which is the kernel's own SYN timeout, reachable when our timeout is
# generous enough to let the kernel give up first). Both mean "nothing came back".
_STATES = {
    0: OPEN,
    errno.ECONNREFUSED: REFUSED,
    errno.EAGAIN: TIMEOUT,
    errno.EINPROGRESS: TIMEOUT,
    errno.ETIMEDOUT: TIMEOUT,
    errno.EHOSTUNREACH: UNREACHABLE,
    errno.ENETUNREACH: UNREACHABLE,
}

# Enough to make a pasted URL or a well-known service name work without a port.
_SCHEME_PORTS = {
    "https": 443,
    "http": 80,
    "ssh": 22,
    "postgres": 5432,
    "postgresql": 5432,
    "redis": 6379,
    "mysql": 3306,
}

_MAX_PORT = 65535


@dataclass(frozen=True)
class TcpTarget:
    host: str
    port: int

    def __str__(self) -> str:
        # Bracket IPv6 literals so the result round-trips through parse_host_port.
        return f"[{self.host}]:{self.port}" if ":" in self.host else f"{self.host}:{self.port}"


@dataclass(frozen=True)
class TcpCheck:
    target: TcpTarget
    state: str
    latency_ms: float
    error: str | None = None

    @property
    def ok(self) -> bool:
        return self.state == OPEN


def parse_host_port(raw: str, *, default_port: int | None = None) -> TcpTarget | None:
    """Parse ``host:port``, ``[::1]:port``, or a URL. None when no port can be determined.

    We deliberately never guess a port for a bare hostname unless the caller
    supplies `default_port` — silently probing 80 when the operator meant 5432
    would produce a confidently wrong answer.
    """
    text = raw.strip()
    if not text:
        return None

    port: int | None = None

    # URL form: pull the scheme's default port, then reduce to the authority.
    if "://" in text:
        scheme, _, rest = text.partition("://")
        port = _SCHEME_PORTS.get(scheme.lower())
        text = rest.split("/", 1)[0]

    # Bracketed IPv6 literal, optionally with a port: [::1] or [::1]:5432
    if text.startswith("["):
        host, sep, tail = text[1:].partition("]")
        if not sep or not host:
            return None
        if tail.startswith(":"):
            port = _port_or_none(tail[1:])
            if port is None:
                return None
    elif text.count(":") == 1:
        host, _, port_text = text.partition(":")
        port = _port_or_none(port_text)
        if port is None:
            return None
    elif ":" in text:
        # Bare IPv6 literal with no brackets — no way to tell host from port.
        host = text
    else:
        host = text

    if not host:
        return None
    if port is None:
        port = default_port
    if port is None:
        return None
    return TcpTarget(host=host, port=port)


def _port_or_none(text: str) -> int | None:
    try:
        port = int(text)
    except ValueError:
        return None
    return port if 1 <= port <= _MAX_PORT else None


def check_port(target: TcpTarget, *, timeout_s: float = 3.0) -> TcpCheck:
    """Attempt a TCP connect and classify the outcome.

    Resolution happens first and reports `dns-error` on its own, because the whole
    point of this check is layer attribution: "the name does not resolve" and "the
    port is filtered" are different problems with different fixes, and a single
    `timeout` verdict would conflate them.
    """
    started = time.monotonic()
    try:
        infos = socket.getaddrinfo(target.host, target.port, type=socket.SOCK_STREAM)
    except socket.gaierror as exc:
        return TcpCheck(
            target=target,
            state=DNS_ERROR,
            latency_ms=_elapsed_ms(started),
            error=str(exc),
        )
    except OSError as exc:
        return TcpCheck(target=target, state=ERROR, latency_ms=_elapsed_ms(started), error=str(exc))
    if not infos:
        return TcpCheck(
            target=target,
            state=DNS_ERROR,
            latency_ms=_elapsed_ms(started),
            error="name resolved to no addresses",
        )

    # Take the family from getaddrinfo rather than assuming AF_INET, so v6-only
    # names and bare v6 literals work.
    family, socktype, proto, _canonname, sockaddr = infos[0]
    sock = socket.socket(family, socktype, proto)
    connect_started = time.monotonic()
    try:
        sock.settimeout(timeout_s)
        code = sock.connect_ex(sockaddr)
    except OSError as exc:
        return TcpCheck(
            target=target, state=ERROR, latency_ms=_elapsed_ms(connect_started), error=str(exc)
        )
    finally:
        sock.close()

    latency_ms = _elapsed_ms(connect_started)
    state = _STATES.get(code, ERROR)
    error = None if state in (OPEN, REFUSED, TIMEOUT, UNREACHABLE) else _errno_text(code)
    return TcpCheck(target=target, state=state, latency_ms=latency_ms, error=error)


def _elapsed_ms(started: float) -> float:
    return (time.monotonic() - started) * 1000.0


def _errno_text(code: int) -> str:
    return f"{errno.errorcode.get(code, code)}: {os.strerror(code)}"
