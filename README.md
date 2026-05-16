# FleetFix

A terminal-UI triage toolbox for technicians supporting Ubuntu 22.04+ / Debian 12+ server fleets.

## Status

**v1.0.0 — production-ready.** All Tier 1 / Tier 2 features are implemented, 300+ tests pass, and the release ships as a single PyInstaller binary.

## What it does

- One interactive TUI per host: storage explorer, network probes, disk health, Docker triage, process / service management, audit log
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

## Configuration

Per-host overrides live under `~/.config/fleetfix/`:

- `probes.yml` — curl/DNS targets for the Network screen
- `otel.yml` — OTLP endpoint, headers, service name (or use `FLEETFIX_OTLP_ENDPOINT` / `FLEETFIX_OTLP_HEADERS` / `FLEETFIX_OTLP_SERVICE` env vars)
- `paths.yml` — overrides for stale-file scan roots and age thresholds

All are optional. Sensible defaults are baked into the binary.

## License

[MIT](LICENSE)
