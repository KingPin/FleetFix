# FleetFix

A terminal-UI triage toolbox for technicians supporting KingPin's Ubuntu 22.04+ / Debian 12+ server fleet.

## Status

**v0.1.0 — scaffolding.** Repo is initialized and CI is wired up; module work begins at milestone 2.
See [the implementation plan](../../../../home/kingpin/.claude/plans/check-out-init-txt-there-parsed-anchor.md) (local) for the full roadmap.

## What it does (when complete)

- One interactive TUI per host: storage explorer, network probes, Docker triage, process / service management, audit log
- Hard safety rails on every destructive action (path blacklist, typed-phrase confirm gate, system-path refusal)
- Tier 1 (read-only + safe deletes in `/home/appuser`) runs as the `appuser` user; Tier 2 (SMART, docker prune, kill, journal triage) requires sudo
- Every privileged action is recorded locally to `/var/log/fleetfix-audit.log` (JSON lines) and exported to SigNoz via OTLP
- Self-update via GitHub Releases — checks on launch, prompts the operator, never auto-applies

## Install (when released)

```sh
curl -fsSL https://raw.githubusercontent.com/KingPin/FleetFix/main/installer/install.sh | sudo bash
```

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

## License

TBD (see open questions in the implementation plan).
