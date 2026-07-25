"""Read /etc/resolv.conf so the Network screen can say *which* resolver is in play.

A failed DNS lookup is only half a diagnosis — the other half is which nameserver
was asked. On a systemd-resolved box that answer is `127.0.0.53`, a local stub
that hides the real upstreams, which is worth flagging explicitly so the operator
knows to reach for `resolvectl status` rather than assuming the box has a broken
loopback resolver.

Plain file read, no subprocess, and the path is injectable so tests can feed
canned files from tmp_path (same idiom as `interfaces.operstate(iface, root=...)`).
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

_RESOLV_CONF = Path("/etc/resolv.conf")
SYSTEMD_STUB = "127.0.0.53"


@dataclass(frozen=True)
class ResolverConfig:
    nameservers: tuple[str, ...]
    search: tuple[str, ...]
    options: tuple[str, ...]
    source: str

    @property
    def stub_resolver(self) -> bool:
        """True when the only nameserver is systemd-resolved's local stub.

        The addresses that actually answer queries sit behind the stub, so
        `nameserver 127.0.0.53` on its own tells the operator almost nothing.
        """
        return self.nameservers == (SYSTEMD_STUB,)


def parse_resolv_conf(text: str, *, source: str = "") -> ResolverConfig:
    """Parse resolv.conf text.

    Nameserver order is preserved — the resolver tries them in file order, so
    reordering them would misrepresent which server actually gets asked first.
    `domain` and `search` are mutually exclusive and the last instance wins
    (resolv.conf(5)), so both directives simply overwrite the search list.
    """
    nameservers: list[str] = []
    search: tuple[str, ...] = ()
    options: list[str] = []

    for raw_line in text.splitlines():
        # Comments start with '#' or ';' and may follow a directive on the same line.
        line = raw_line.split("#", 1)[0].split(";", 1)[0].strip()
        if not line:
            continue
        parts = line.split()
        keyword, args = parts[0], parts[1:]
        if keyword == "nameserver" and args:
            nameservers.append(args[0])
        elif keyword == "search":
            search = tuple(args)
        elif keyword == "domain" and args:
            # Legacy single-domain form — normalise it into a one-element search list.
            search = (args[0],)
        elif keyword == "options":
            options.extend(args)

    return ResolverConfig(
        nameservers=tuple(nameservers),
        search=search,
        options=tuple(options),
        source=source,
    )


def read_resolver(source: Path = _RESOLV_CONF) -> ResolverConfig:
    """Read and parse resolv.conf. A missing or unreadable file yields an empty config."""
    try:
        text = source.read_text(encoding="utf-8")
    except OSError:
        return ResolverConfig(nameservers=(), search=(), options=(), source=str(source))
    return parse_resolv_conf(text, source=str(source))
