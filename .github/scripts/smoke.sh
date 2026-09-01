#!/bin/sh
# Runs one fleetfix binary through its whole public contract on whatever distro,
# libc and user it finds itself as.
#
# A script rather than inline YAML for one reason: a gate you can only run by
# pushing is a gate you debug by pushing. This is the file that caught the stale
# `test "$code" = 3` assertion the smoke job carried for four commits after the
# registry started registering checks -- found by running it against a real
# debian:11 container on a laptop, which the inline version made impossible.
#
# POSIX sh throughout. alpine:3.20 has no bash, and the whole point of the musl
# leg is that nothing about it is assumed.
#
# Usage: smoke.sh /path/to/fleetfix
set -eu

BIN="${1:?usage: smoke.sh /path/to/fleetfix}"
WHO="$(id -un 2>/dev/null || echo unknown)"
fail=0

note() { printf '  %s\n' "$*"; }

bad() {
	printf '::error::[%s] %s\n' "$WHO" "$1"
	fail=1
}

# capture runs BIN and leaves the exit code in $code, stdout in $out and stderr
# in $err. `set -e` would abort on the first non-zero exit, and half these cases
# are about a non-zero exit being correct.
capture() {
	set +e
	out="$("$BIN" "$@" 2>/tmp/smoke-err)"
	code=$?
	set -e
	err="$(cat /tmp/smoke-err)"
}

printf '=== %s as %s\n' "$BIN" "$WHO"

# --- version ------------------------------------------------------------------
capture --version
[ "$code" = "0" ] || bad "--version exited $code"
case "$out" in
fleetfix\ ?*) note "version: $out" ;;
*) bad "--version printed '$out'" ;;
esac
[ -z "$err" ] || bad "--version wrote to stderr: $err"

# --- check --json -------------------------------------------------------------
# The exit code is the host's verdict, so it is asserted against the document
# rather than against a constant. A container's disk fills, a runner's load
# spikes, and a hardcoded expectation would either be wrong or would have to be
# so loose it asserted nothing. What must hold everywhere is that the process and
# the document agree: a consumer trusting $? and one reading exit_code out of a
# stored report have to reach the same answer.
capture check --json
[ -z "$err" ] || bad "check wrote to stderr: $err"
case "$out" in
*'"schema": "fleetfix.check/v1"'*) ;;
*) bad "check did not emit a v1 document" ;;
esac

# Top-level keys are at exactly two spaces of indent, so this cannot pick up a
# nested one. No jq: debian:11 and alpine:3.20 both ship without it, and
# installing it would put a package mirror between this gate and its verdict.
claimed="$(printf '%s\n' "$out" | sed -n 's/^  "exit_code": \([0-9]*\),$/\1/p')"
if [ -z "$claimed" ]; then
	bad "no top-level exit_code in the document"
elif [ "$claimed" != "$code" ]; then
	bad "check exited $code and the document says exit_code $claimed"
else
	note "check exited $code, agreeing with the document"
fi

# An empty checks array out of a build that registers collectors is the failure
# worth catching here: it is what a front door that forgot to build the registry
# would print, and it would otherwise sail through as a valid document.
case "$out" in
*'"checks": []'*) bad "check ran nothing; the front door built no registry" ;;
esac

# --- check --list -------------------------------------------------------------
capture check --list
[ "$code" = "0" ] || bad "--list exited $code; it says nothing about the host"
[ -z "$err" ] || bad "--list wrote to stderr: $err"
case "$out" in
*'"checks": ['*'"id"'*) ;;
*) bad "--list named no checks" ;;
esac

# --- doctor -------------------------------------------------------------------
# Zero on every distro and both users. Doctor describes rather than grades, so a
# container with no sudo, no systemd and no docker must still exit 0 -- and that
# is precisely the host this leg runs on.
capture doctor
[ "$code" = "0" ] || bad "doctor exited $code; it describes rather than grades"
[ -z "$err" ] || bad "doctor wrote to stderr: $err"
for heading in Host Operator Privilege 'Container runtime' Configuration Thresholds 'External programs' Checks; do
	case "$out" in
	*"
$heading
"*) ;;
	*) bad "doctor printed no '$heading' section" ;;
	esac
done
note "doctor exited 0 with every section"

# --- a usage error ------------------------------------------------------------
# The hard half of the stream contract: a consumer's parser sees an empty stdout
# and a non-zero code, never a half-written document.
capture --nope
[ "$code" != "0" ] || bad "an unknown flag exited 0"
[ -z "$out" ] || bad "a usage error wrote to stdout: $out"
[ -n "$err" ] || bad "a usage error explained nothing on stderr"

rm -f /tmp/smoke-err
if [ "$fail" -ne 0 ]; then
	printf '=== FAILED as %s\n' "$WHO"
	exit 1
fi
printf '=== ok as %s\n' "$WHO"
