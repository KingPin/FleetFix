package network

import (
	"strings"

	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// CurlProbeMarker is the first line of the -w template FleetFix asks curl for.
// It is what lets the timing block be found in output that also carries curl's
// own warnings.
const CurlProbeMarker = "FLEETFIX_CURL_PROBE"

// CurlProbe is one URL probe, as curl's -w template reports it.
type CurlProbe struct {
	URL string `json:"url"`
	// OK is the 2xx/3xx range, not "the request worked": a 404 is a successful
	// exchange and an unsuccessful probe.
	OK                 bool    `json:"ok"`
	HTTPCode           int64   `json:"http_code"`
	TimeTotalS         float64 `json:"time_total_s"`
	TimeNamelookupS    float64 `json:"time_namelookup_s"`
	TimeConnectS       float64 `json:"time_connect_s"`
	TimeAppconnectS    float64 `json:"time_appconnect_s"`
	TimeStarttransferS float64 `json:"time_starttransfer_s"`
	SizeDownloadBytes  int64   `json:"size_download_bytes"`
	Error              *string `json:"error"`
	Raw                string  `json:"raw"`
}

// ParseCurlOutput reads the -w template out of curl's stdout. The second return
// is false when there is no usable timing block -- no marker at all (curl failed
// before it could write one), or a field that is missing or will not convert.
// The caller reports that as a failed probe with curl's stderr as the message,
// which is where the reason lives.
//
// Raw is set to output. The probe caller records stderr+stdout instead, since a
// warning curl emitted on the way to a 200 is worth showing; that is an
// assignment on the returned probe, not a parsing concern.
//
// One departure from v1: an http_code or size_download past int64 yields no
// probe, where Python reports the arbitrary-precision integer. curl writes both
// with printf from a counter, so neither is reachable by output curl produces.
func ParseCurlOutput(url, output string) (CurlProbe, bool) {
	// rfind: with two templates in one capture -- a redirect chain curl was
	// asked to follow -- the last one is the request that actually finished.
	// The index may land mid-line, and then the marker's own line is whatever
	// followed it; either way that line is dropped and the fields start after.
	start := strings.LastIndex(output, CurlProbeMarker)
	if start == -1 {
		return CurlProbe{}, false
	}
	lines := pytext.SplitLines(output[start:])

	fields := map[string]string{}
	for _, line := range lines[1:] {
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		// The value keeps any further '=' -- and then fails to convert, which is
		// the same answer as a corrupt field.
		fields[strings.TrimFunc(key, pytext.IsSpace)] = strings.TrimFunc(value, pytext.IsSpace)
	}

	code, ok := intField(fields, "http_code")
	if !ok {
		return CurlProbe{}, false
	}
	size, ok := intField(fields, "size_download")
	if !ok {
		return CurlProbe{}, false
	}
	out := CurlProbe{
		URL:               url,
		OK:                code >= 200 && code < 400,
		HTTPCode:          code,
		SizeDownloadBytes: size,
		Raw:               output,
	}
	for _, f := range []struct {
		key string
		dst *float64
	}{
		{"time_namelookup", &out.TimeNamelookupS},
		{"time_connect", &out.TimeConnectS},
		{"time_appconnect", &out.TimeAppconnectS},
		{"time_starttransfer", &out.TimeStarttransferS},
		{"time_total", &out.TimeTotalS},
	} {
		v, ok := floatField(fields, f.key)
		if !ok {
			return CurlProbe{}, false
		}
		*f.dst = v
	}
	return out, true
}

func intField(fields map[string]string, key string) (int64, bool) {
	v, present := fields[key]
	if !present {
		return 0, false
	}
	n, err := pytext.Int(v)
	return n, err == nil
}

func floatField(fields map[string]string, key string) (float64, bool) {
	v, present := fields[key]
	if !present {
		return 0, false
	}
	f, err := pytext.Float(v)
	return f, err == nil
}
