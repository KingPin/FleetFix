"""Walk the network stack one layer at a time and name the layer that broke.

"curl failed" is not a diagnosis. This runs five rungs bottom-up — link, gateway,
internet, DNS, HTTPS — so the answer is "DNS is the problem", which is the
difference between fixing it and guessing.

**Every rung runs, even after one fails.** Halting at the first failure would
confidently report "gateway down" on any cloud host whose gateway drops ICMP —
a box that is in fact perfectly healthy. The verdict still names the *first*
failure, because that is where to start looking; the rest of the rungs are what
tell you whether that failure actually mattered.

Every sub-probe is injected, so the tests exercise the whole ladder without
touching the network. `should_continue` is a plain callable so the Textual
`get_current_worker()` call stays in the screen and this module stays Textual-free.
"""

from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass

from .curl_probe import CurlProbe
from .curl_probe import probe as curl_probe
from .dns import DnsResult, resolve_one
from .interfaces import NetworkInfo, read_network
from .ping import PingSummary, run_ping
from .probes import Probes

LINK = "link"
GATEWAY = "gateway"
INTERNET = "internet"
DNS = "dns"
HTTPS = "https"

# Ladder pings are deliberately shorter than the standalone ping probe: this asks
# "does anything come back", not "how stable is this link over time".
_PING_COUNT = 3
_PING_TIMEOUT_S = 6


@dataclass(frozen=True)
class LadderRung:
    name: str
    label: str
    ok: bool
    detail: str
    skipped: bool = False


@dataclass(frozen=True)
class LadderResult:
    rungs: tuple[LadderRung, ...]

    @property
    def first_failure(self) -> LadderRung | None:
        """Lowest rung that failed — where to start looking."""
        for rung in self.rungs:
            if not rung.ok and not rung.skipped:
                return rung
        return None

    @property
    def ok(self) -> bool:
        return all(rung.ok for rung in self.rungs if not rung.skipped)


def run_ladder(
    *,
    probes: Probes,
    on_rung: Callable[[LadderRung], None] | None = None,
    should_continue: Callable[[], bool] | None = None,
    net_fn: Callable[[], NetworkInfo | None] = read_network,
    ping_fn: Callable[..., PingSummary | None] = run_ping,
    dns_fn: Callable[..., DnsResult] = resolve_one,
    curl_fn: Callable[..., CurlProbe] = curl_probe,
) -> LadderResult:
    """Run all five rungs bottom-up. `on_rung` fires as each one finishes.

    `should_continue` is checked before each rung; returning False marks the
    remainder `skipped` and returns early, so an abandoned ladder stops burning
    subprocess time instead of finishing a run nobody will see.
    """
    rungs: list[LadderRung] = []
    net: NetworkInfo | None = None

    def emit(rung: LadderRung) -> None:
        rungs.append(rung)
        if on_rung is not None:
            on_rung(rung)

    def cancelled() -> bool:
        return should_continue is not None and not should_continue()

    def link_step(label: str) -> LadderRung:
        nonlocal net
        net = net_fn()
        return _link_rung(label, net)

    # Lazy by construction: the gateway step reads `net` at call time, which is
    # after link_step has set it. Nothing runs until the loop reaches it, so a
    # cancelled ladder never spawns the probes it skipped.
    steps: tuple[tuple[str, str, Callable[[str], LadderRung]], ...] = (
        (LINK, "link state", link_step),
        (GATEWAY, "default gateway", lambda label: _gateway_rung(label, net, ping_fn)),
        (
            INTERNET,
            f"internet ({probes.ladder.internet_target})",
            lambda label: _ping_rung(INTERNET, label, probes.ladder.internet_target, ping_fn),
        ),
        (DNS, f"dns ({probes.ladder.dns_name})", lambda label: _dns_rung(label, probes, dns_fn)),
        (
            HTTPS,
            f"https ({probes.ladder.https_url})",
            lambda label: _https_rung(label, probes, curl_fn),
        ),
    )

    for index, (_name, label, step) in enumerate(steps):
        if cancelled():
            rungs.extend(
                LadderRung(name=name, label=text, ok=False, detail="not run", skipped=True)
                for name, text, _fn in steps[index:]
            )
            break
        emit(step(label))

    return LadderResult(rungs=tuple(rungs))


def _link_rung(label: str, net: NetworkInfo | None) -> LadderRung:
    if net is None:
        return LadderRung(
            name=LINK,
            label=label,
            ok=False,
            detail="no default route — this box has no path off itself",
        )
    detail = f"{net.iface} {net.ipv4 or 'no address'} via {net.gateway} ({net.operstate})"
    return LadderRung(name=LINK, label=label, ok=net.operstate == "up", detail=detail)


def _gateway_rung(
    label: str, net: NetworkInfo | None, ping_fn: Callable[..., PingSummary | None]
) -> LadderRung:
    if net is None or not net.gateway:
        # Without a gateway address there is nothing to ping — reporting a failed
        # ping here would be inventing a result.
        return LadderRung(
            name=GATEWAY, label=label, ok=False, detail="no gateway to test", skipped=True
        )
    return _ping_rung(GATEWAY, f"{label} ({net.gateway})", net.gateway, ping_fn)


def _ping_rung(
    name: str, label: str, target: str, ping_fn: Callable[..., PingSummary | None]
) -> LadderRung:
    summary = ping_fn(target, count=_PING_COUNT, timeout_s=_PING_TIMEOUT_S)
    if summary is None:
        return LadderRung(name=name, label=label, ok=False, detail="ping produced no summary")
    # ok on partial loss: 2/3 back means this rung is up. Quantifying flakiness
    # is the standalone ping probe's job, not the ladder's.
    return LadderRung(
        name=name,
        label=label,
        ok=summary.loss_pct < 100.0,
        detail=(
            f"{summary.received}/{summary.sent} back, "
            f"{summary.loss_pct:g}% loss, {summary.rtt_avg_ms:.1f}ms avg"
        ),
    )


def _dns_rung(label: str, probes: Probes, dns_fn: Callable[..., DnsResult]) -> LadderRung:
    result = dns_fn(probes.ladder.dns_name, timeout_s=probes.dns.timeout_s)
    if not result.ok:
        return LadderRung(name=DNS, label=label, ok=False, detail=result.error or "lookup failed")
    addresses = ", ".join(result.addresses) or "no addresses"
    return LadderRung(
        name=DNS,
        label=label,
        ok=True,
        detail=f"{addresses} in {result.latency_ms:.0f}ms",
    )


def _https_rung(label: str, probes: Probes, curl_fn: Callable[..., CurlProbe]) -> LadderRung:
    result = curl_fn(
        probes.ladder.https_url,
        timeout_s=probes.http.timeout_s,
        max_redirects=probes.http.max_redirects,
    )
    if result.error is not None:
        return LadderRung(name=HTTPS, label=label, ok=False, detail=result.error)
    return LadderRung(
        name=HTTPS,
        label=label,
        ok=result.ok,
        detail=f"HTTP {result.http_code} in {result.time_total_s * 1000:.0f}ms",
    )
