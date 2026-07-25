"""Tests for the layered connectivity ladder.

Every sub-probe is injected, so nothing here touches the network.
"""

from __future__ import annotations

from typing import Any

from fleetfix.modules.network.curl_probe import CurlProbe
from fleetfix.modules.network.dns import DnsResult
from fleetfix.modules.network.interfaces import NetworkInfo
from fleetfix.modules.network.ladder import (
    DNS,
    GATEWAY,
    HTTPS,
    INTERNET,
    LINK,
    LadderRung,
    run_ladder,
)
from fleetfix.modules.network.ping import PingSummary
from fleetfix.modules.network.probes import DEFAULT_PROBES

_UP = NetworkInfo(
    iface="eth0",
    ipv4="10.0.0.9",
    gateway="10.0.0.1",
    operstate="up",
    rx_bytes=1,
    tx_bytes=1,
)


def _ping(*, loss: float, sent: int = 3) -> PingSummary:
    received = round(sent * (1 - loss / 100.0))
    return PingSummary(
        target="x",
        sent=sent,
        received=received,
        loss_pct=loss,
        rtt_min_ms=1.0,
        rtt_avg_ms=2.0,
        rtt_max_ms=3.0,
        rtt_mdev_ms=0.5,
    )


def _curl(*, ok: bool = True, code: int = 200, error: str | None = None) -> CurlProbe:
    return CurlProbe(
        url="https://github.com",
        ok=ok,
        http_code=code,
        time_total_s=0.2,
        time_namelookup_s=0.01,
        time_connect_s=0.02,
        time_appconnect_s=0.05,
        time_starttransfer_s=0.1,
        size_download_bytes=1234,
        error=error,
    )


def _dns(*, ok: bool = True) -> DnsResult:
    if ok:
        return DnsResult(name="github.com", ok=True, addresses=("140.82.121.4",), latency_ms=12.0)
    return DnsResult(
        name="github.com", ok=False, addresses=(), latency_ms=3000.0, error="Name not known"
    )


def _all_good(**overrides: Any) -> dict[str, Any]:
    fns: dict[str, Any] = {
        "net_fn": lambda: _UP,
        "ping_fn": lambda *a, **k: _ping(loss=0.0),
        "dns_fn": lambda *a, **k: _dns(),
        "curl_fn": lambda *a, **k: _curl(),
    }
    fns.update(overrides)
    return fns


def test_healthy_host_passes_all_five_rungs() -> None:
    result = run_ladder(probes=DEFAULT_PROBES, **_all_good())
    assert [rung.name for rung in result.rungs] == [LINK, GATEWAY, INTERNET, DNS, HTTPS]
    assert result.ok is True
    assert result.first_failure is None


def test_on_rung_fires_bottom_up_as_each_rung_finishes() -> None:
    seen: list[str] = []
    run_ladder(probes=DEFAULT_PROBES, on_rung=lambda r: seen.append(r.name), **_all_good())
    assert seen == [LINK, GATEWAY, INTERNET, DNS, HTTPS]


def test_no_default_route_fails_link_and_skips_gateway() -> None:
    result = run_ladder(probes=DEFAULT_PROBES, **_all_good(net_fn=lambda: None))
    by_name = {rung.name: rung for rung in result.rungs}
    assert by_name[LINK].ok is False
    assert "no path off itself" in by_name[LINK].detail
    # No gateway address means there is nothing to ping; a failed ping here would
    # be an invented result.
    assert by_name[GATEWAY].skipped is True
    assert result.first_failure is not None
    assert result.first_failure.name == LINK


def test_down_interface_fails_the_link_rung() -> None:
    down = NetworkInfo(
        iface="eth0", ipv4=None, gateway="10.0.0.1", operstate="down", rx_bytes=0, tx_bytes=0
    )
    result = run_ladder(probes=DEFAULT_PROBES, **_all_good(net_fn=lambda: down))
    assert result.rungs[0].ok is False
    assert result.ok is False


def test_unknown_operstate_does_not_fail_the_link_rung() -> None:
    # "unknown" is what wireguard/tun-tap and carrier-less virtio links report while
    # passing traffic, and it is also `interfaces.operstate()`'s fallback when the sysfs
    # read raises. Neither is evidence of a down link, so rung 1 must not take the
    # verdict away from the rungs that actually measured something.
    unknown = NetworkInfo(
        iface="wg0",
        ipv4="10.9.0.2",
        gateway="10.9.0.1",
        operstate="unknown",
        rx_bytes=1,
        tx_bytes=1,
    )
    result = run_ladder(probes=DEFAULT_PROBES, **_all_good(net_fn=lambda: unknown))
    assert result.rungs[0].ok is True
    assert "unknown" in result.rungs[0].detail
    assert result.ok is True
    assert result.first_failure is None


def test_gateway_failure_does_not_halt_the_ladder() -> None:
    # This pins the do-not-halt decision: an ICMP-filtered cloud gateway must not
    # produce a confident "gateway down" verdict on a box whose internet works.
    def ping(target: str, **_kwargs: Any) -> PingSummary | None:
        return None if target == "10.0.0.1" else _ping(loss=0.0)

    result = run_ladder(probes=DEFAULT_PROBES, **_all_good(ping_fn=ping))
    by_name = {rung.name: rung for rung in result.rungs}
    assert by_name[GATEWAY].ok is False
    assert by_name[GATEWAY].skipped is False
    # The rungs above it still ran, and they are what say the failure didn't matter.
    assert by_name[INTERNET].ok is True
    assert by_name[DNS].ok is True
    assert by_name[HTTPS].ok is True
    assert result.first_failure is not None
    assert result.first_failure.name == GATEWAY


def test_dns_failure_still_runs_the_https_rung() -> None:
    result = run_ladder(probes=DEFAULT_PROBES, **_all_good(dns_fn=lambda *a, **k: _dns(ok=False)))
    by_name = {rung.name: rung for rung in result.rungs}
    assert by_name[DNS].ok is False
    assert by_name[DNS].detail == "Name not known"
    assert HTTPS in by_name
    assert by_name[HTTPS].skipped is False
    assert result.first_failure is not None
    assert result.first_failure.name == DNS


def test_partial_packet_loss_still_passes_the_rung() -> None:
    # 1/3 back means this rung is up. Quantifying flakiness is the standalone
    # ping probe's job.
    result = run_ladder(
        probes=DEFAULT_PROBES, **_all_good(ping_fn=lambda *a, **k: _ping(loss=66.0))
    )
    by_name = {rung.name: rung for rung in result.rungs}
    assert by_name[INTERNET].ok is True
    assert "66% loss" in by_name[INTERNET].detail
    assert result.ok is True


def test_total_packet_loss_fails_the_rung() -> None:
    result = run_ladder(
        probes=DEFAULT_PROBES, **_all_good(ping_fn=lambda *a, **k: _ping(loss=100.0))
    )
    assert result.rungs[1].ok is False


def test_curl_error_fails_the_https_rung_with_its_message() -> None:
    result = run_ladder(
        probes=DEFAULT_PROBES,
        **_all_good(curl_fn=lambda *a, **k: _curl(ok=False, code=0, error="curl: (35) TLS")),
    )
    assert result.rungs[-1].ok is False
    assert result.rungs[-1].detail == "curl: (35) TLS"


def test_authenticated_endpoint_4xx_still_passes_the_https_rung() -> None:
    # A 401 means the server answered, so every layer beneath it works. `CurlProbe.ok`
    # is False here because the standalone probe grades the service; the ladder grades
    # the path, and conflating them reports "https is the lowest thing broken" on a
    # healthy box whose configured `ladder.https_url` sits behind auth.
    result = run_ladder(
        probes=DEFAULT_PROBES, **_all_good(curl_fn=lambda *a, **k: _curl(ok=False, code=401))
    )
    assert result.rungs[-1].ok is True
    assert "HTTP 401" in result.rungs[-1].detail
    assert result.ok is True
    assert result.first_failure is None


def test_server_error_still_passes_the_https_rung() -> None:
    # Pins the same decision for 5xx: a broken app behind a working network is the
    # standalone curl probe's finding to report, not a layer attribution.
    result = run_ladder(
        probes=DEFAULT_PROBES, **_all_good(curl_fn=lambda *a, **k: _curl(ok=False, code=502))
    )
    assert result.rungs[-1].ok is True
    assert result.ok is True


def test_cancelling_skips_the_remaining_rungs_without_running_them() -> None:
    calls = {"ping": 0, "dns": 0, "curl": 0}

    def ping(*_a: Any, **_k: Any) -> PingSummary:
        calls["ping"] += 1
        return _ping(loss=0.0)

    def dns(*_a: Any, **_k: Any) -> DnsResult:
        calls["dns"] += 1
        return _dns()

    def curl(*_a: Any, **_k: Any) -> CurlProbe:
        calls["curl"] += 1
        return _curl()

    done: list[LadderRung] = []

    result = run_ladder(
        probes=DEFAULT_PROBES,
        on_rung=done.append,
        # Cancel once link and gateway are behind us.
        should_continue=lambda: len(done) < 2,
        **_all_good(ping_fn=ping, dns_fn=dns, curl_fn=curl),
    )

    assert [rung.name for rung in result.rungs] == [LINK, GATEWAY, INTERNET, DNS, HTTPS]
    skipped = [rung.name for rung in result.rungs if rung.skipped]
    assert skipped == [INTERNET, DNS, HTTPS]
    # The point of cancelling is to stop burning subprocess time.
    assert calls == {"ping": 1, "dns": 0, "curl": 0}
    # on_rung never fires for a skipped rung.
    assert [rung.name for rung in done] == [LINK, GATEWAY]


def test_skipped_rungs_do_not_count_against_ok_or_first_failure() -> None:
    result = run_ladder(
        probes=DEFAULT_PROBES,
        should_continue=lambda: False,
        **_all_good(),
    )
    assert all(rung.skipped for rung in result.rungs)
    assert result.ok is True
    assert result.first_failure is None


def test_ladder_targets_come_from_probes() -> None:
    seen: list[str] = []

    def ping(target: str, **_kwargs: Any) -> PingSummary:
        seen.append(target)
        return _ping(loss=0.0)

    def dns(name: str, **_kwargs: Any) -> DnsResult:
        seen.append(name)
        return _dns()

    def curl(url: str, **_kwargs: Any) -> CurlProbe:
        seen.append(url)
        return _curl()

    run_ladder(probes=DEFAULT_PROBES, **_all_good(ping_fn=ping, dns_fn=dns, curl_fn=curl))
    assert seen == [
        "10.0.0.1",  # gateway, discovered from net_fn
        DEFAULT_PROBES.ladder.internet_target,
        DEFAULT_PROBES.ladder.dns_name,
        DEFAULT_PROBES.ladder.https_url,
    ]
