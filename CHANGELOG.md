# Changelog

All notable changes to FleetFix are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.0.0] — 2026-05-16

First production-ready release. Covers every Tier 1 and Tier 2 feature from
the KingPin triage spec and ships as a single, signed-and-checksummed
PyInstaller binary at `/usr/local/bin/fleetfix`.

### Added

#### Core shell
- Textual TUI with top bar (host, OS, kernel, uptime, load, RAM, version,
  update banner) and left nav (Dashboard / Storage / Network / Disk / Docker
  / Processes / Services / Audit).
- `Tier 2 🔒` annotations on nav items when sudo is unavailable.
- Dashboard with live `/proc`-backed metrics, thermal readout, and pending
  apt update count.

#### Storage (Tier 1)
- Interactive `/home/appuser` tree explorer with lazy directory loading.
- Stale artifact finder for `*.sql`, `*.sql.gz`, `*.dump`, and rotated logs
  under user home, configurable age threshold.
- `.env` / config validator with basic key=value parse + required-key checks.
- Safe single-file delete routed through the path blacklist + confirm gate.

#### Network (Tier 1)
- Ping quality test with jitter + loss reporting.
- Curl probes with timing breakdown and configurable probe list.
- DNS resolution checks against a configured name set.
- Listening TCP socket inventory parsed from `ss`.

#### System metrics (Tier 1)
- `/proc/loadavg`, `/proc/meminfo`, `/proc/uptime` parsers with 2s refresh.
- Thermal zone readout that degrades gracefully on VMs.
- Pending apt upgrade count, surfaced as a dashboard card.

#### Disk (Tier 2)
- SMART health summary for SATA (`smartctl -H -A`) and NVMe.
- Ghost-space scan via `lsof +L1` with restart-owner-service follow-up.
- Inode-pressure scan from `df -P -i` with warn/critical thresholds.
- Hardcoded path blacklist (`/`, `/boot`, `/etc`, `/usr`, `/lib*`, `/sbin`,
  `/bin`, `/var/lib/{dpkg,apt}`, `/proc`, `/sys`, `/dev`) with symlink-chain,
  `..` traversal, broken-symlink, and relative-cwd resolution.

#### Docker (Tier 2)
- Container dashboard with restart-loop detection (>3 restarts in 10 min).
- Per-container json-log truncate via sudo with size-freed reporting.
- `docker system df` + dry-run prune flows for images and volumes with
  reclaim estimates shown before apply.

#### Processes (Tier 2)
- Top-N RSS / CPU ranker scraped from `/proc`.
- Audit-wrapped signal sender. SIGTERM is the default; SIGKILL requires a
  second typed confirmation. Refuses pid 0 and pid 1.

#### Services (Tier 2)
- `systemctl list-units --state=failed` table with inline journal tail.
- `systemd-analyze blame` table that flags outliers >5s.

#### Audit logging
- Append-only JSON-lines audit log at `/var/log/fleetfix-audit.log`.
- Intent / result pairing — intent is written *before* the action runs so
  a crash leaves a record of what was attempted.
- Optional OTLP-gRPC log export to SigNoz (or any OTEL collector). The
  local file remains the authoritative record; the OTEL sink is best-effort
  and never blocks local writes.
- In-TUI audit log viewer.

#### Safety
- Single shared destructive-action confirm modal. Requires typing a phrase
  (`DELETE`, `KILL`, `PRUNE`, `TRUNCATE`, `UPDATE`) — no checkboxes, no
  enter-to-confirm.
- Emergency log-squeeze module: gzips oversized `*.log` / `*.log.N` files
  in place after an `lsof` check confirms no process holds them open for
  write. Conservative-refuse on lsof errors.

#### Updater
- Startup release check against GitHub Releases with a 1h cache at
  `~/.cache/fleetfix/release_check.json`. Silent on network failures.
- In-app installer flow: download → sha256-verify → `sudo install` →
  atomic rename. Tech is asked to restart manually; FleetFix never
  auto-relaunches mid-triage.
- `installer/install.sh` one-shot bootstrap: installs the binary to
  `/usr/local/bin/fleetfix`, creates `/var/log/fleetfix-audit.log`
  (root:adm 0640), and installs a logrotate config.

#### CI / release
- `lint-test.yml` runs ruff and pytest on every PR.
- `build.yml` builds the PyInstaller `fleetfix-linux-x86_64` artifact on
  ubuntu-22.04, generates the matching `.sha256`, and attaches both to a
  GitHub release on tag push.

### Test coverage

317 passing tests at release: unit tests for every module, Textual snapshot
tests for the top-level screens, and integration tests for the audit log
and updater flows. Overall line coverage 86%.

### Known limitations

- ARM64 builds are not produced in CI yet. Add the arm64 runner when fleet
  hosts on that architecture appear.
- Duo principal mapping (`identity/duo_map.py`) is stubbed — the audit
  record currently records `unix_user` and `source_ip` but leaves
  `duo_principal` as null. Will land in v1.1 once the auth.log format is
  confirmed on production hosts.
