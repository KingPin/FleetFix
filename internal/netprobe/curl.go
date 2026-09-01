package netprobe

import (
	"context"
	"strconv"

	"github.com/KingPin/FleetFix/v2/internal/core/network"
)

// curlFormat is the -w template, byte-for-byte v1's. The marker is what lets the
// timing block be found in output that also carries curl's own warnings, and the
// field names are what ParseCurlOutput looks for -- so this string and the parser
// are one contract split across two packages.
const curlFormat = network.CurlProbeMarker + "\n" +
	"http_code=%{http_code}\n" +
	"time_namelookup=%{time_namelookup}\n" +
	"time_connect=%{time_connect}\n" +
	"time_appconnect=%{time_appconnect}\n" +
	"time_starttransfer=%{time_starttransfer}\n" +
	"time_total=%{time_total}\n" +
	"size_download=%{size_download}\n"

// curlGraceS is how much longer we wait than we told curl to.
//
// --max-time is curl's own budget; this is the wall clock on the process. Making
// them equal would race, and losing the race means killing curl a moment before it
// writes the timing block that explains what went wrong -- turning a clean "timed
// out after 15s" into "no usable output".
const curlGraceS = 2

// Curl probes a URL and reports what curl's timing template said.
//
// Always a CurlProbe, never an error: a TLS handshake that failed is a finding
// about the host's network, and Error carries curl's own message because that is
// the sentence an operator can act on.
func (p *Prober) Curl(ctx context.Context, url string, timeoutS, maxRedirects int64) network.CurlProbe {
	ctx, cancel := withTimeout(ctx, float64(timeoutS+curlGraceS))
	defer cancel()

	res, err := p.Run.Run(
		ctx, "curl",
		// -sS: no progress meter, but keep the error message. -o /dev/null: the
		// body is not the subject, the timings are.
		"-sS", "-o", "/dev/null",
		"--max-time", strconv.FormatInt(timeoutS, 10),
		"--max-redirs", strconv.FormatInt(maxRedirects, 10),
		"-L",
		"-w", curlFormat,
		url,
	)
	if err != nil {
		return failedCurl(url, err.Error(), res.Combined())
	}

	// Combined for the same reason ping uses it: the template lands on stdout and
	// curl's complaint lands on stderr, and a probe that reported only one of them
	// would either lose the timings or lose the reason.
	raw := res.Combined()
	probe, ok := network.ParseCurlOutput(url, raw)
	if !ok {
		// curl exited before it could write a timing block. Its stderr is the
		// diagnosis -- "SSL certificate problem", "Could not resolve host" -- and
		// the exit code alone is not.
		return failedCurl(url, firstLine(res.Stderr, "curl exited "+strconv.Itoa(res.ExitCode)), raw)
	}
	probe.Raw = raw
	return probe
}

// failedCurl is a probe that never produced timings, carrying the reason.
func failedCurl(url, reason, raw string) network.CurlProbe {
	return network.CurlProbe{URL: url, Error: errText(reason), Raw: raw}
}
