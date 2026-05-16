"""Tier 1 Network screen — listening sockets + curl/DNS/ping probes.

Renders three tables: the box's listening TCP ports (always), and on
demand the result of a curl/DNS/ping run against an operator-supplied
target. Probe input is a single text field — the operator types a URL,
hostname, or IP, and we pick the right tool based on what was typed.
"""

from __future__ import annotations

from textual.app import ComposeResult
from textual.containers import Horizontal, Vertical
from textual.widget import Widget
from textual.widgets import Button, DataTable, Input, Static

from fleetfix.modules.network.curl_probe import probe as run_curl
from fleetfix.modules.network.dns import resolve_one
from fleetfix.modules.network.ping import run_ping
from fleetfix.modules.network.sockets import list_listening_sockets


class NetworkView(Widget):
    DEFAULT_CSS = """
    NetworkView {
        layout: vertical;
        height: 1fr;
        padding: 1 1 0 1;
    }
    NetworkView .panel-title {
        text-style: bold;
        color: $accent;
        margin-bottom: 1;
    }
    NetworkView #probe-controls {
        height: 3;
        margin-bottom: 1;
    }
    NetworkView #probe-controls Input {
        width: 50;
        margin-right: 1;
    }
    NetworkView #probe-controls Button {
        margin-right: 1;
    }
    NetworkView #probe-result {
        height: auto;
        max-height: 10;
        margin-bottom: 1;
    }
    NetworkView #sockets-table {
        height: 1fr;
    }
    """

    def __init__(self, *, id: str | None = None) -> None:
        super().__init__(id=id)

    def compose(self) -> ComposeResult:
        with Vertical():
            yield Static("Probe a target (URL, hostname, or IP)", classes="panel-title")
            with Horizontal(id="probe-controls"):
                yield Input(
                    placeholder="https://api.internal/health  or  db.internal", id="probe-target"
                )
                yield Button("Curl", id="probe-curl", variant="primary")
                yield Button("DNS", id="probe-dns")
                yield Button("Ping", id="probe-ping")
            yield Static("Pick a tool above to run a probe.", id="probe-result")

            yield Static("Listening TCP sockets", classes="panel-title")
            sockets_table = DataTable(id="sockets-table", zebra_stripes=True, cursor_type="row")
            sockets_table.add_columns("Port", "Address", "Process", "PID")
            yield sockets_table

    def on_mount(self) -> None:
        self._refresh_sockets()

    def on_button_pressed(self, event: Button.Pressed) -> None:
        target = self.query_one("#probe-target", Input).value.strip()
        if not target:
            self.query_one("#probe-result", Static).update("Enter a target above.")
            return
        if event.button.id == "probe-curl":
            self._run_curl(target)
        elif event.button.id == "probe-dns":
            self._run_dns(target)
        elif event.button.id == "probe-ping":
            self._run_ping(target)

    def _run_curl(self, target: str) -> None:
        if not target.startswith(("http://", "https://")):
            target = "https://" + target
        result = run_curl(target)
        out = self.query_one("#probe-result", Static)
        if result.error:
            out.update(f"[red]✗[/] curl {target}: {result.error}")
            return
        marker = "[green]✓[/]" if result.ok else "[red]✗[/]"
        out.update(
            f"{marker} HTTP {result.http_code}  total {result.time_total_s * 1000:.1f}ms  "
            f"(dns {result.time_namelookup_s * 1000:.1f}ms · "
            f"connect {result.time_connect_s * 1000:.1f}ms · "
            f"tls {result.time_appconnect_s * 1000:.1f}ms · "
            f"ttfb {result.time_starttransfer_s * 1000:.1f}ms)  "
            f"{result.size_download_bytes}B"
        )

    def _run_dns(self, target: str) -> None:
        # Strip a scheme if the operator pasted a URL.
        host = target.split("://", 1)[-1].split("/", 1)[0].split(":", 1)[0]
        result = resolve_one(host)
        out = self.query_one("#probe-result", Static)
        if not result.ok:
            out.update(f"[red]✗[/] DNS {host}: {result.error}  ({result.latency_ms:.1f}ms)")
            return
        addrs = ", ".join(result.addresses)
        out.update(f"[green]✓[/] DNS {host} → {addrs}  ({result.latency_ms:.1f}ms)")

    def _run_ping(self, target: str) -> None:
        host = target.split("://", 1)[-1].split("/", 1)[0].split(":", 1)[0]
        summary = run_ping(host, count=10, interval_s=0.2)
        out = self.query_one("#probe-result", Static)
        if summary is None:
            out.update(f"[red]✗[/] ping {host}: no usable output (binary missing or timed out)")
            return
        marker = "[green]✓[/]" if summary.loss_pct == 0 else "[yellow]![/]"
        out.update(
            f"{marker} ping {host}  {summary.received}/{summary.sent}  "
            f"loss {summary.loss_pct:.0f}%  avg {summary.rtt_avg_ms:.1f}ms  "
            f"jitter {summary.jitter_ms:.1f}ms"
        )

    def _refresh_sockets(self) -> None:
        table = self.query_one("#sockets-table", DataTable)
        table.clear()
        for sock in sorted(list_listening_sockets(), key=lambda s: s.local_port):
            table.add_row(
                str(sock.local_port),
                sock.local_address,
                sock.process_name or "—",
                str(sock.pid) if sock.pid is not None else "—",
            )
