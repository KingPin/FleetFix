package netprobe

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	"github.com/KingPin/FleetFix/v2/internal/core/network"
)

const curlTimings = `FLEETFIX_CURL_PROBE
http_code=200
time_namelookup=0.010
time_connect=0.020
time_appconnect=0.060
time_starttransfer=0.100
time_total=0.120
size_download=4096
`

// curlArgv is the argv the probe must build. Spelled once here so a test asserting
// on the response and a test asserting on the command cannot drift apart.
func curlArgv(url, timeoutS, redirects string) []string {
	return []string{
		"-sS", "-o", "/dev/null",
		"--max-time", timeoutS,
		"--max-redirs", redirects,
		"-L",
		"-w", curlFormat,
		url,
	}
}

func TestCurlParsesTheTimingTemplate(t *testing.T) {
	t.Parallel()
	p, fake := newTestProber(t)
	fake.Stdout(curlTimings, "curl", curlArgv("https://github.com", "15", "5")...)

	probe := p.Curl(context.Background(), "https://github.com", 15, 5)
	if probe.Error != nil {
		t.Fatalf("error = %q", *probe.Error)
	}
	if !probe.OK || probe.HTTPCode != 200 {
		t.Errorf("ok=%v code=%d", probe.OK, probe.HTTPCode)
	}
	if probe.TimeTotalS != 0.12 || probe.TimeAppconnectS != 0.06 {
		t.Errorf("timings: total=%v tls=%v", probe.TimeTotalS, probe.TimeAppconnectS)
	}
	if probe.SizeDownloadBytes != 4096 {
		t.Errorf("size = %d", probe.SizeDownloadBytes)
	}
	if probe.Raw != curlTimings {
		t.Error("raw did not carry what curl printed")
	}
}

// The -w template and the parser are one contract split across two packages. If
// the marker ever drifts, everything still compiles and every probe silently
// reports "no usable output".
func TestCurlTemplateCarriesTheMarkerTheParserLooksFor(t *testing.T) {
	t.Parallel()
	if !strings.HasPrefix(curlFormat, network.CurlProbeMarker+"\n") {
		t.Fatalf("the -w template does not start with %q", network.CurlProbeMarker)
	}
}

// A 404 is a successful probe reporting an unsuccessful request: the network path
// worked. OK is narrower than "we got an answer" on purpose, and the collector
// grades the two differently.
func TestCurlReportsA404AsAnAnsweredProbe(t *testing.T) {
	t.Parallel()
	p, fake := newTestProber(t)
	body := strings.Replace(curlTimings, "http_code=200", "http_code=404", 1)
	fake.Stdout(body, "curl", curlArgv("https://github.com/nope", "15", "5")...)

	probe := p.Curl(context.Background(), "https://github.com/nope", 15, 5)
	if probe.Error != nil {
		t.Fatalf("error = %q, want none: the server answered", *probe.Error)
	}
	if probe.OK {
		t.Error("a 404 was reported as ok")
	}
	if probe.HTTPCode != 404 {
		t.Errorf("code = %d", probe.HTTPCode)
	}
}

// curl's stderr is the diagnosis. The exit code alone says only that something
// went wrong, which is what the operator already knew.
func TestCurlReportsStderrWhenThereIsNoTimingBlock(t *testing.T) {
	t.Parallel()
	p, fake := newTestProber(t)
	fake.Exit(
		60,
		"",
		"curl: (60) SSL certificate problem: self-signed certificate\nMore details here: ...",
		"curl", curlArgv("https://internal.example", "15", "5")...,
	)

	probe := p.Curl(context.Background(), "https://internal.example", 15, 5)
	if probe.Error == nil {
		t.Fatal("no error from a failed handshake")
	}
	if *probe.Error != "curl: (60) SSL certificate problem: self-signed certificate" {
		t.Errorf("error = %q, want curl's first stderr line", *probe.Error)
	}
	if !strings.Contains(probe.Raw, "More details here") {
		t.Error("raw dropped the rest of curl's message")
	}
}

// A curl that failed silently -- no template, no stderr -- still has to say
// something, and the exit code is the only thing left.
func TestCurlFallsBackToTheExitCode(t *testing.T) {
	t.Parallel()
	p, fake := newTestProber(t)
	fake.Exit(7, "", "", "curl", curlArgv("https://down.example", "15", "5")...)

	probe := p.Curl(context.Background(), "https://down.example", 15, 5)
	if probe.Error == nil || *probe.Error != "curl exited 7" {
		t.Fatalf("error = %v", probe.Error)
	}
}

func TestCurlReportsAnAbsentBinary(t *testing.T) {
	t.Parallel()
	p, fake := newTestProber(t)
	fake.Missing("curl", curlArgv("https://github.com", "15", "5")...)

	probe := p.Curl(context.Background(), "https://github.com", 15, 5)
	if probe.Error == nil || !strings.Contains(*probe.Error, "not found") {
		t.Fatalf("error = %v, want the missing-executable reason", probe.Error)
	}
	if probe.URL != "https://github.com" {
		t.Errorf("url = %q, want it echoed even on the failure path", probe.URL)
	}
}

// --max-time is curl's own budget; the process gets longer. Equal budgets race,
// and losing that race kills curl the moment before it writes the block that
// explains what went wrong.
func TestCurlGivesTheProcessMoreTimeThanItGivesCurl(t *testing.T) {
	t.Parallel()
	p, fake := newTestProber(t)
	fake.Stdout(curlTimings, "curl", curlArgv("https://github.com", "15", "5")...)

	deadlineSeen := make(chan time.Duration, 1)
	p.Run = runnerFunc(func(ctx context.Context, name string, args ...string) (cmdrun.Result, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Error("no deadline on the curl context")
			deadlineSeen <- 0
			return cmdrun.Result{Stdout: curlTimings}, nil
		}
		deadlineSeen <- time.Until(deadline)
		return cmdrun.Result{Stdout: curlTimings}, nil
	})

	p.Curl(context.Background(), "https://github.com", 15, 5)
	budget := <-deadlineSeen
	if budget <= 15*time.Second || budget > 17*time.Second {
		t.Fatalf("process budget %v, want curl's 15s plus the grace", budget)
	}
}
