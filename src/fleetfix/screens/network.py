"""Tier 1 Network screen — connectivity checks + listening sockets.

Two tabs. **Checks** is where an operator lands: link/resolver facts at the top,
one-click quick checks, then a typed-target probe row. Every action writes to the
same two widgets — a one-line styled verdict and a scrollable pane holding the
detail plus the tool's verbatim output — so "did it work, and what did the tool
actually say" is one mental model for all of them. **Sockets** answers a different
question (what listens *on* this box) and is the biggest vertical consumer, so it
gets its own pane rather than squeezing the raw output pane.

One output surface means one worker group governs it: see `_probe_worker`.
"""

from __future__ import annotations

from collections.abc import Callable
from typing import NamedTuple

from textual import work
from textual.app import ComposeResult
from textual.containers import Grid, Horizontal, VerticalScroll
from textual.markup import escape
from textual.widgets import Button, DataTable, Input, Static, TabbedContent, TabPane

from fleetfix.modules.network.curl_probe import probe as run_curl
from fleetfix.modules.network.dns import resolve_one
from fleetfix.modules.network.ping import run_ping
from fleetfix.modules.network.sockets import ListeningSocket, list_listening_sockets
from fleetfix.screens.base import LazyScanView

# Verdict tiers. `warn` exists because partial packet loss and a 4xx are
# genuinely neither pass nor fail — flattening them into one or the other is how
# a screen starts lying to the operator.
OK = "ok"
WARN = "warn"
BAD = "bad"

# Buttons that consume whatever is typed in #probe-target. Everything else on the
# screen runs against a configured or discovered target, so the empty-target
# guard must not fire for them.
_TARGET_BUTTONS = frozenset({"probe-curl", "probe-dns", "probe-ping"})


class ProbeOutput(NamedTuple):
    """One check's result, in the shape both output widgets need.

    `verdict` is a single markup line; `status` is one of OK/WARN/BAD (or "" for
    the running state); `body` is the raw pane's full content — our parsed detail
    followed by the tool's verbatim output.
    """

    verdict: str
    status: str
    body: str


class NetworkView(LazyScanView):
    DEFAULT_CSS = """
    NetworkView {
        layout: vertical;
        height: 1fr;
        padding: 1 1 0 1;
    }
    NetworkView #net-tabs {
        height: 1fr;
    }
    NetworkView #net-tabs ContentSwitcher {
        height: 1fr;
    }
    NetworkView TabPane {
        height: 1fr;
        padding: 0;
    }
    NetworkView #link-summary, NetworkView #resolver-summary {
        height: 1;
        color: $text-muted;
    }
    /* Grid, not Horizontal: a 5x1 grid gives every button a deterministic 1/5
       share, so nothing clips at 80 columns and nothing balloons at 160. */
    NetworkView Grid#quick-checks {
        grid-size: 5 1;
        grid-gutter: 0 1;
        height: 3;
        max-width: 80;
    }
    NetworkView Grid#quick-checks Button {
        width: 1fr;
        min-width: 0;
    }
    NetworkView #probe-controls {
        height: 3;
        margin-bottom: 1;
    }
    NetworkView #probe-controls Input {
        width: 1fr;
        min-width: 12;
        max-width: 60;
        margin-right: 1;
    }
    NetworkView #probe-controls Button {
        width: auto;
        min-width: 7;
        margin-right: 1;
    }
    NetworkView #probe-verdict {
        height: 1;
        text-style: bold;
    }
    NetworkView .verdict-ok { color: $success; }
    NetworkView .verdict-warn { color: $warning; }
    NetworkView .verdict-bad { color: $error; }
    NetworkView #raw-pane {
        height: 1fr;
        min-height: 4;
        border: round $primary-darken-2;
        background: $panel;
    }
    NetworkView #sockets-table {
        height: 1fr;
    }
    """

    def __init__(self, *, id: str | None = None) -> None:
        super().__init__(id=id)

    def compose(self) -> ComposeResult:
        with TabbedContent(id="net-tabs"):
            with TabPane("Checks", id="tab-checks"):
                # Dense and self-labelling — at 24 rows a heading per line is a
                # row we'd rather give to the raw output pane.
                yield Static("link  —", id="link-summary")
                yield Static("dns   —", id="resolver-summary")
                with Grid(id="quick-checks"):
                    yield Button("Run all", id="quick-all", variant="primary")
                    yield Button("Gateway", id="quick-gateway")
                    yield Button("Internet", id="quick-internet")
                    yield Button("Probe set", id="quick-set")
                    yield Button("Refresh", id="net-refresh")
                with Horizontal(id="probe-controls"):
                    yield Input(placeholder="host, url, or host:port", id="probe-target")
                    yield Button("Curl", id="probe-curl", variant="primary")
                    yield Button("DNS", id="probe-dns")
                    yield Button("Ping", id="probe-ping")
                    yield Button("Trace", id="probe-trace")
                    yield Button("Port", id="probe-port")
                yield Static("Pick a check above.", id="probe-verdict")
                # VerticalScroll + Static rather than RichLog: update() is the
                # right *replace* semantic for a probe result (RichLog is
                # append-only, so a forgotten clear() is invisible stale output).
                with VerticalScroll(id="raw-pane"):
                    yield Static("", id="probe-result")
            with TabPane("Sockets", id="tab-sockets"):
                sockets_table = DataTable(id="sockets-table", zebra_stripes=True, cursor_type="row")
                sockets_table.add_columns("Port", "Address", "Process", "PID")
                yield sockets_table

    def start_initial_scan(self) -> None:
        # Eager even though the Sockets pane is inactive: query_one resolves
        # through an inactive TabPane, and the table is then already populated
        # when the operator switches to it.
        self._refresh_sockets()

    def on_button_pressed(self, event: Button.Pressed) -> None:
        # The repo uses no @on decorators; one id ladder keeps every route to the
        # output surface visible in one place.
        button_id = event.button.id
        if button_id not in _TARGET_BUTTONS:
            return
        target = self.query_one("#probe-target", Input).value.strip()
        if not target:
            self._show_verdict(
                "! no target",
                WARN,
                "Enter a target above — a hostname, IP, URL, or host:port.",
            )
            return
        if button_id == "probe-curl":
            self._run_curl(target)
        elif button_id == "probe-dns":
            self._run_dns(target)
        elif button_id == "probe-ping":
            self._run_ping(target)

    # Probes shell out (curl, ping) or block on DNS; ping in particular runs
    # ~2s. The formatting helpers below are pure (no DOM access) so they are safe
    # to call from the worker thread; only the final ProbeOutput is marshalled
    # back to the UI thread.

    def _run_curl(self, target: str) -> None:
        if not target.startswith(("http://", "https://")):
            target = "https://" + target
        self._start_probe(f"curl {target}")
        self._probe_worker(lambda: _format_curl(target))

    def _run_dns(self, target: str) -> None:
        host = _host_of(target)
        self._start_probe(f"resolving {host}")
        self._probe_worker(lambda: _format_dns(host))

    def _run_ping(self, target: str) -> None:
        host = _host_of(target)
        self._start_probe(f"ping {host} (10 packets, ~2s)")
        self._probe_worker(lambda: _format_ping(host))

    def _start_probe(self, label: str) -> None:
        # No `loading = True`: on a height:auto Static that collapses the widget
        # to 0x0, so the spinner is invisible. A named running line is better
        # anyway — it can say which tool and roughly how long, which a spinner
        # cannot.
        self._show_verdict(f"[dim]⋯ {label}[/]", "", "")

    @work(thread=True, exclusive=True, group="net-probe")
    def _probe_worker(self, compute: Callable[[], ProbeOutput]) -> None:
        out = compute()
        self.app.call_from_thread(self._apply_probe, out)

    def _apply_probe(self, out: ProbeOutput) -> None:
        self._show_verdict(out.verdict, out.status, out.body)

    def _show_verdict(self, verdict: str, status: str, body: str) -> None:
        line = self.query_one("#probe-verdict", Static)
        # set_classes replaces wholesale, so the previous tier can't linger.
        line.set_classes([f"verdict-{status}"] if status else [])
        line.update(verdict)
        self.query_one("#probe-result", Static).update(body)

    def _refresh_sockets(self) -> None:
        self.query_one("#sockets-table", DataTable).loading = True
        self._load_sockets()

    @work(thread=True, exclusive=True, group="net-sockets")
    def _load_sockets(self) -> None:
        socks = list_listening_sockets()
        self.app.call_from_thread(self._apply_sockets, socks)

    def _apply_sockets(self, socks: list[ListeningSocket]) -> None:
        table = self.query_one("#sockets-table", DataTable)
        table.loading = False
        table.clear()
        for sock in sorted(socks, key=lambda s: s.local_port):
            table.add_row(
                str(sock.local_port),
                sock.local_address,
                sock.process_name or "—",
                str(sock.pid) if sock.pid is not None else "—",
            )


def _host_of(target: str) -> str:
    """Strip a scheme, path, and port so ping/DNS get a bare hostname."""
    return target.split("://", 1)[-1].split("/", 1)[0].split(":", 1)[0]


def _body(detail: str, raw: str = "") -> str:
    """One raw-pane block: our parsed detail, then the tool's verbatim output.

    The tool's output is escaped. It is not markup, and a stray `[/]` in it
    raises MarkupError while a stray `[foo]` silently eats the rest of the line.
    """
    blocks = [detail.strip("\n")]
    if raw.strip():
        blocks.append(escape(raw.strip("\n")))
    return "\n\n".join(blocks)


def _format_curl(target: str) -> ProbeOutput:
    result = run_curl(target)
    if result.error:
        return ProbeOutput(
            f"✗ curl {target}: {result.error}", BAD, _body(f"curl {target}", result.raw)
        )
    detail = (
        f"HTTP {result.http_code}  total {result.time_total_s * 1000:.1f}ms  "
        f"(dns {result.time_namelookup_s * 1000:.1f}ms · "
        f"connect {result.time_connect_s * 1000:.1f}ms · "
        f"tls {result.time_appconnect_s * 1000:.1f}ms · "
        f"ttfb {result.time_starttransfer_s * 1000:.1f}ms)  "
        f"{result.size_download_bytes}B"
    )
    # A 4xx/5xx is a warn, not a fail: the server answered, which means the whole
    # network path underneath it works.
    status = OK if result.ok else WARN
    marker = "✓" if result.ok else "!"
    return ProbeOutput(
        f"{marker} curl {target} — HTTP {result.http_code} in {result.time_total_s * 1000:.1f}ms",
        status,
        _body(detail, result.raw),
    )


def _format_dns(host: str) -> ProbeOutput:
    # No command output to show — resolve_one uses getaddrinfo, so the "raw"
    # block here is a synthesized report. There is no `dig` invocation to find.
    result = resolve_one(host)
    if not result.ok:
        detail = f"DNS {host}: {result.error}  ({result.latency_ms:.1f}ms)"
        return ProbeOutput(f"✗ DNS {host} failed", BAD, _body(detail))
    addrs = "\n".join(f"  {addr}" for addr in result.addresses)
    detail = (
        f"DNS {host} → {len(result.addresses)} address(es) in {result.latency_ms:.1f}ms\n{addrs}"
    )
    return ProbeOutput(
        f"✓ DNS {host} → {', '.join(result.addresses)}  ({result.latency_ms:.1f}ms)",
        OK,
        _body(detail),
    )


def _format_ping(host: str) -> ProbeOutput:
    summary = run_ping(host, count=10, interval_s=0.2)
    if summary is None:
        return ProbeOutput(
            f"✗ ping {host}: no usable output (binary missing or timed out)",
            BAD,
            _body(f"ping {host} produced nothing we could parse."),
        )
    detail = (
        f"ping {host}  {summary.received}/{summary.sent}  "
        f"loss {summary.loss_pct:.0f}%  avg {summary.rtt_avg_ms:.1f}ms  "
        f"jitter {summary.jitter_ms:.1f}ms"
    )
    if summary.loss_pct >= 100.0:
        status = BAD
    elif summary.loss_pct > 0:
        status = WARN
    else:
        status = OK
    marker = {OK: "✓", WARN: "!", BAD: "✗"}[status]
    return ProbeOutput(f"{marker} {detail}", status, _body(detail, summary.raw))
