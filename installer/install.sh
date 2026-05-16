#!/usr/bin/env bash
#
# FleetFix bootstrap installer.
#
# Usage (intended):
#   curl -fsSL https://raw.githubusercontent.com/KingPin/FleetFix/main/installer/install.sh \
#     | sudo bash
#
# Side effects:
#   - Installs /usr/local/bin/fleetfix (mode 0755)
#   - Creates /var/log/fleetfix-audit.log (root:adm 0640) if it doesn't exist
#   - Installs /etc/logrotate.d/fleetfix to rotate the audit log
#
# Re-running this script upgrades the binary in place via an atomic rename.

set -euo pipefail

REPO="${FLEETFIX_REPO:-KingPin/FleetFix}"
ASSET="${FLEETFIX_ASSET:-fleetfix-linux-x86_64}"
TARGET="${FLEETFIX_TARGET:-/usr/local/bin/fleetfix}"
AUDIT_LOG="${FLEETFIX_AUDIT_LOG:-/var/log/fleetfix-audit.log}"
LOGROTATE_CONF="${FLEETFIX_LOGROTATE_CONF:-/etc/logrotate.d/fleetfix}"
RELEASE_URL="${FLEETFIX_RELEASE_URL:-https://api.github.com/repos/${REPO}/releases/latest}"

err() { printf "error: %s\n" "$*" >&2; exit 1; }
info() { printf ">> %s\n" "$*"; }

[[ $EUID -eq 0 ]] || err "must run as root (try: curl ... | sudo bash)"

for cmd in curl sha256sum install mv mktemp; do
  command -v "$cmd" >/dev/null 2>&1 || err "required command not found: $cmd"
done

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

info "querying ${RELEASE_URL}"
release_json="$(curl -fsSL -H "Accept: application/vnd.github+json" "$RELEASE_URL")"

# Pull the asset + checksum URLs out of the JSON using grep/sed — no jq dep.
asset_url="$(printf '%s' "$release_json" \
  | grep -oE "\"browser_download_url\"[[:space:]]*:[[:space:]]*\"[^\"]*${ASSET}\"" \
  | head -n1 \
  | sed -E 's/.*"(https[^"]+)"/\1/')"
checksum_url="$(printf '%s' "$release_json" \
  | grep -oE "\"browser_download_url\"[[:space:]]*:[[:space:]]*\"[^\"]*${ASSET}\.sha256\"" \
  | head -n1 \
  | sed -E 's/.*"(https[^"]+)"/\1/')"

[[ -n "$asset_url" ]] || err "could not find ${ASSET} asset in latest release"
[[ -n "$checksum_url" ]] || err "could not find ${ASSET}.sha256 in latest release"

info "downloading binary: $asset_url"
curl -fsSL -o "$work_dir/$ASSET" "$asset_url"

info "downloading checksum: $checksum_url"
curl -fsSL -o "$work_dir/${ASSET}.sha256" "$checksum_url"

info "verifying sha256"
(cd "$work_dir" && sha256sum -c "${ASSET}.sha256")

info "installing to ${TARGET}"
install -m 0755 "$work_dir/$ASSET" "${TARGET}.new"
mv -f "${TARGET}.new" "$TARGET"

if [[ ! -f "$AUDIT_LOG" ]]; then
  info "creating audit log at ${AUDIT_LOG}"
  install -m 0640 -o root -g adm /dev/null "$AUDIT_LOG" 2>/dev/null \
    || install -m 0640 /dev/null "$AUDIT_LOG"
fi

if [[ ! -f "$LOGROTATE_CONF" ]]; then
  info "installing logrotate config at ${LOGROTATE_CONF}"
  cat >"$LOGROTATE_CONF" <<'EOF'
/var/log/fleetfix-audit.log {
    weekly
    rotate 12
    compress
    missingok
    notifempty
    create 0640 root adm
}
EOF
fi

info "fleetfix $(${TARGET} --version 2>/dev/null || echo 'installed') ready — run \`fleetfix\` to start."
