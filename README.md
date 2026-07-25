# FleetFix

A terminal-UI triage toolbox for technicians supporting Ubuntu 22.04+ / Debian 12+ server fleets.

## Status

**v1.0.0 — production-ready.** All Tier 1 / Tier 2 features are implemented, 300+ tests pass, and the release ships as a single PyInstaller binary.

## What it does

- One interactive TUI per host: storage explorer, layered network diagnostics, disk health, Docker triage, process / service management, audit log
- Hard safety rails on every destructive action: hardcoded system-path blacklist, typed-phrase confirm gate, sudo gating on Tier 2
- Tier 1 (read-only + safe deletes under the invoking user's home) runs unprivileged; Tier 2 (SMART, docker prune, kill, journal triage, log squeeze) wraps individual commands in `sudo` per-call so the operator's identity is preserved in the audit log
- Every privileged action is recorded locally to `/var/log/fleetfix-audit.log` (JSON lines) and optionally exported to any OTLP-compatible collector (SigNoz, Tempo, Honeycomb, Grafana Cloud, etc.)
- Self-update via GitHub Releases — checks on launch with a 1h cache, prompts the operator, never auto-applies

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/KingPin/FleetFix/main/installer/install.sh | sudo bash
```

This installs `/usr/local/bin/fleetfix`, creates `/var/log/fleetfix-audit.log` (`root:adm 0640`), and drops a logrotate config at `/etc/logrotate.d/fleetfix`.

## Develop

```sh
python3 -m venv .venv
source .venv/bin/activate
pip install -e ".[dev]"

ruff check src tests
ruff format --check src tests
pytest

python -m fleetfix --version
```

## Inspecting another user's footprint

By default FleetFix scans the invoking user's home and lists failed units owned by anyone. Pass `--target-user <name>` to point the storage screen at that user's home and filter the services screen to units whose `User=` matches:

```sh
fleetfix --target-user appuser
```

The resolved target is stamped on every audit record (top-level `inspect_target` field) and promoted to the `fleetfix.inspect_target` OTEL attribute, so the trail keeps operator and target unambiguous. A "Inspecting: \<user\>" chip in the top bar surfaces the active target. The flag also accepts an empty string (`--target-user ""`) to force-clear a `target_user` set in `paths.yml`.

## Network screen

The **Checks** tab opens with the facts you need before running anything: interface, address, default gateway and link state, plus the nameservers from `/etc/resolv.conf` (it flags a `127.0.0.53` systemd-resolved stub and points you at `resolvectl status`, since the real upstreams sit behind it).

One-click checks, no typing:

| Button | What it does |
| --- | --- |
| **Run all** | Walks the stack bottom-up — link, gateway, internet, DNS, HTTPS — streaming a line per rung, then names the *lowest* thing that broke. Every rung runs even after one fails: halting early would confidently report "gateway down" on any cloud host whose gateway just drops ICMP. |
| **Gateway** | Pings whatever the routing table says the first hop is. |
| **Internet** | Pings the configured `ladder.internet_target`. |
| **Probe set** | Runs every target in `probes.yml` — one line each, in layer order. |

Or type a hostname, URL, or `host:port` and pick **Curl**, **DNS**, **Ping**, **Trace**, or **Port**. Every check writes to the same place: a styled pass/warn/fail verdict line with the tool's verbatim output in the scrollable pane underneath, so "did it work, and what did the tool actually say" is one mental model throughout.

`warn` is a real tier, not a softened failure. A 4xx means the server answered — the whole network path under it works. A refused TCP port means the host answered and nothing is listening — a service problem, not a path problem. A traceroute whose path goes dark mid-way is usually ICMP-rate-limited transit and says nothing about the destination.

**Trace** needs `traceroute` or `tracepath`; it prefers `traceroute`, falls back to `tracepath`, and tells you the `apt` line if neither is present:

```sh
sudo apt install traceroute      # or: sudo apt install iputils-tracepath
```

The **Sockets** tab lists what is listening *on* this box (port, address, process, PID).

## Configuration

Per-host overrides live under `~/.config/fleetfix/`:

- `probes.yml` — targets and timing for the Network screen (see below)
- `otel.yml` — OTLP endpoint, headers, service name (or use `FLEETFIX_OTLP_ENDPOINT` / `FLEETFIX_OTLP_HEADERS` / `FLEETFIX_OTLP_SERVICE` env vars)
- `paths.yml` — `target_user: <name>` sets the default inspect target (overridden by `--target-user`)

All are optional. Sensible defaults are baked into the binary.

### probes.yml

Every key is optional. A host with no `probes.yml` at all is fully functional — it runs against a deliberately boring set of public targets.

```yaml
ping:
  targets: [10.0.0.1, 8.8.8.8]     # default: [8.8.8.8, 1.1.1.1]
  count: 5                         # default: 10       (1–900)
  interval_s: 0.3                  # default: 0.2      (0.05–5.0)
  timeout_s: 15                    # default: 15       (1–300)
dns:
  names: [db.corp.internal, github.com]   # default: [github.com, archive.ubuntu.com]
  timeout_s: 3.0                   # default: 3.0      (0.1–30)
http:
  urls: [https://api.corp.internal/health]  # default: [https://github.com]
  timeout_s: 15                    # default: 15       (1–120)
  max_redirects: 5                 # default: 5        (0–20)
tcp:
  targets:                         # default: [github.com:443]
    - "db.corp.internal:5432"      # host:port, [::1]:5432, or a URL
    - "https://api.corp.internal"  # scheme implies the port
  timeout_s: 3.0                   # default: 3.0      (0.1–30)
traceroute:
  max_hops: 20                     # default: 15       (1–30)
  wait_s: 1                        # default: 1        (1–10)
  queries: 1                       # default: 1        (1–3)
ladder:                            # the single target per rung for "Run all"
  internet_target: 9.9.9.9         # default: 8.8.8.8
  dns_name: db.corp.internal       # default: github.com
  https_url: https://api.corp.internal/health   # default: https://github.com
```

**Target lists replace; scalar knobs merge.** Setting `ping.targets` drops the built-in ping targets entirely, but setting `ping.count` leaves `interval_s` and `timeout_s` at their defaults. The asymmetry is deliberate: a target list is a fleet inventory, not a suggestion — appending our `github.com` to an egress-filtered host's `[api.internal, db.internal]` would add a permanently-red row, and a check that is always red trains operators to ignore red. Scalars are independent knobs, so `count: 3` means "shorter ping", not "also zero the interval".

An explicit empty list (`targets: []`) means "no presets on this box" and is honoured as empty — **Probe set** then tells you where to add them rather than silently doing nothing.

Numeric values are clamped to the ranges above, because they become subprocess arguments: `count: 100000` with `interval_s: 0` is a self-inflicted DoS on the box you are trying to triage.

Nothing in this file can stop the Network screen from opening. A missing file, invalid YAML, a top-level list, a junk scalar, an out-of-range number, or a `tcp` target with no port each degrade to the default for that one field and log a warning.

## License

[MIT](LICENSE)
