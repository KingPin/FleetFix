package resolve

import (
	"strings"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/config"
	corenet "github.com/KingPin/FleetFix/v2/internal/core/network"
)

// The same property Thresholds has, for the same reason: the network checks probe
// what this says, and doctor prints this, so there is one reading of the file.
func TestProbesYamlReachesTheResolvedProbes(t *testing.T) {
	opts := bare(t)
	writeYAML(t, opts.Paths.SystemDir, config.ProbesFile,
		"ping:\n  targets: [10.0.0.1]\n  count: 4\n")
	writeYAML(t, opts.Paths.UserDir, config.ProbesFile,
		"ladder:\n  internet_target: 9.9.9.9\n")

	r := New(opts)

	// A list replaces wholesale -- it is a fleet inventory, not a suggestion --
	// and the scalars beside it merge per-key.
	if got := r.Probes.Ping.Targets; len(got) != 1 || got[0] != "10.0.0.1" {
		t.Errorf("ping targets = %v", got)
	}
	if r.Probes.Ping.Count != 4 {
		t.Errorf("ping count = %d", r.Probes.Ping.Count)
	}
	// Both layers, merged: the user file amended a section the system file never
	// mentioned.
	if r.Probes.Ladder.InternetTarget != "9.9.9.9" {
		t.Errorf("internet target = %q", r.Probes.Ladder.InternetTarget)
	}
	// And what the file did not say still stands.
	if r.Probes.DNS.TimeoutS != corenet.DefaultProbes().DNS.TimeoutS {
		t.Errorf("dns timeout = %v, want the shipped default", r.Probes.DNS.TimeoutS)
	}
}

func TestTheShippedTargetsStandWithNoProbesFile(t *testing.T) {
	r := New(bare(t))

	if len(r.Probes.Ping.Targets) == 0 || r.Probes.Ladder.HTTPSURL == "" {
		t.Fatalf("probes = %+v on a host with no probes.yml", r.Probes)
	}
	if len(r.Warnings) != 0 {
		t.Errorf("warnings = %v; an absent file is the ordinary case", r.Warnings)
	}
}

// The clamps exist because these numbers become subprocess arguments, and a clamp
// nobody is told about is an operator whose ping runs 900 packets when they asked
// for 100000 and never finds out why the check takes three minutes.
func TestWhatTheProbeResolverAdjustedIsReported(t *testing.T) {
	opts := bare(t)
	writeYAML(t, opts.Paths.UserDir, config.ProbesFile, "ping:\n  count: 100000\n")

	r := New(opts)

	if len(r.Warnings) != 1 {
		t.Fatalf("warnings = %v, want the clamp reported", r.Warnings)
	}
	want := config.ProbesFile + ": ping.count 100000 is out of range, using 900"
	if r.Warnings[0] != want {
		t.Errorf("warning = %q, want %q", r.Warnings[0], want)
	}
	if r.Probes.Ping.Count != 900 {
		t.Errorf("count = %d, want the clamped value the warning named", r.Probes.Ping.Count)
	}
}

// probes.yml sits between perf.yml and thresholds.yml in Files, and its warnings
// have to arrive there rather than at the end -- config_warnings[] is compared
// byte-for-byte between consecutive runs.
func TestProbeWarningsSitInFileOrderWithTheRest(t *testing.T) {
	opts := bare(t)
	writeYAML(t, opts.Paths.UserDir, config.ProbesFile, "ping:\n  count: 100000\n")
	writeYAML(t, opts.Paths.UserDir, config.ThresholdsFile, "disk.used_pct:\n  warn: high\n")
	writeYAML(t, opts.Paths.UserDir, config.IdentityFile, "principals: not-a-mapping\n")

	got := New(opts).Warnings
	if len(got) != 3 {
		t.Fatalf("warnings = %v, want one per file", got)
	}
	for i, name := range []string{config.IdentityFile, config.ProbesFile, config.ThresholdsFile} {
		if !strings.Contains(got[i], name) {
			t.Errorf("warning %d is %q, want the one for %s", i, got[i], name)
		}
	}
}

// Every kind gets a sentence, and every sentence ends with what the tool did --
// the failure this guards against is an operator who set a target list, never
// found out it was refused, and read a report about the defaults.
func TestEveryProbeWarningSaysWhatHappenedInstead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		warning corenet.Warning
		want    string
	}{
		{
			"section",
			corenet.Warning{Section: "ping", Kind: corenet.WarnNotAMapping, Got: "3"},
			"ping is not a mapping (got 3), ignoring the section",
		},
		{
			"list",
			corenet.Warning{
				Section: "ping", Key: "targets", Kind: corenet.WarnNotAList,
				Got: "'8.8.8.8'", Used: "['1.1.1.1']",
			},
			"ping.targets is not a list (got '8.8.8.8'), using ['1.1.1.1']",
		},
		{
			// The quotes are the point: they are why the number was refused.
			"number",
			corenet.Warning{
				Section: "ping", Key: "count", Kind: corenet.WarnNotANumber, Got: "'5'", Used: "10",
			},
			"ping.count is not a number (got '5'), using 10",
		},
		{
			"clamp",
			corenet.Warning{
				Section: "traceroute", Key: "max_hops", Kind: corenet.WarnClamped,
				Got: "64", Used: "30",
			},
			"traceroute.max_hops 64 is out of range, using 30",
		},
		{
			// Dropped rather than replaced, so the sentence cannot promise a value.
			"target",
			corenet.Warning{
				Section: "tcp", Key: "targets", Kind: corenet.WarnBadTarget, Got: "'github.com'",
			},
			"tcp.targets has no port (got 'github.com'), dropping it",
		},
		{
			// A kind the resolver grew without a sentence here. Still says where
			// and what, because saying nothing is how the setting vanishes.
			"unknown kind",
			corenet.Warning{Section: "dns", Key: "names", Kind: "something_new", Got: "42"},
			"dns.names: something_new (got 42)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeProbeWarning(tc.warning); got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}
