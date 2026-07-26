package network

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
)

// Expectations measured against the v1 parser, as elsewhere in this package.

// A complete template, the base every synthetic case below edits.
const curlFull = CurlProbeMarker + "\n" +
	"http_code=200\ntime_namelookup=0.1\ntime_connect=0.2\n" +
	"time_appconnect=0.3\ntime_starttransfer=0.4\ntime_total=0.5\nsize_download=9\n"

// curlWith replaces one field's line in the template.
func curlWith(field, value string) string {
	for _, line := range strings.Split(curlFull, "\n") {
		if strings.HasPrefix(line, field+"=") {
			return strings.Replace(curlFull, line, field+"="+value, 1)
		}
	}
	panic("no such field: " + field)
}

// curlWithout drops one field's line from the template.
func curlWithout(field string) string {
	for _, line := range strings.Split(curlFull, "\n") {
		if strings.HasPrefix(line, field+"=") {
			return strings.Replace(curlFull, line+"\n", "", 1)
		}
	}
	panic("no such field: " + field)
}

// Raw is the whole input; the dedicated case below covers it.
func ignoreRawCurl() cmp.Option { return cmpopts.IgnoreFields(CurlProbe{}, "Raw") }

// The complete template's parse, for cases that only vary one field.
func curlProbe() CurlProbe {
	return CurlProbe{
		URL: "u", OK: true, HTTPCode: 200,
		TimeNamelookupS: 0.1, TimeConnectS: 0.2, TimeAppconnectS: 0.3,
		TimeStarttransferS: 0.4, TimeTotalS: 0.5, SizeDownloadBytes: 9,
	}
}

func TestParseCurlOutputFixtures(t *testing.T) {
	tests := []struct {
		name string
		file string
		want CurlProbe
		ok   bool
	}{
		{
			name: "200", file: "curl/ok_200.txt", ok: true,
			want: CurlProbe{
				URL: "https://x", OK: true, HTTPCode: 200,
				TimeNamelookupS: 0.001234, TimeConnectS: 0.012345, TimeAppconnectS: 0.045678,
				TimeStarttransferS: 0.098765, TimeTotalS: 0.123456, SizeDownloadBytes: 4096,
			},
		},
		{
			// A completed exchange, so every timing is real -- but not ok.
			name: "404", file: "curl/not_found_404.txt", ok: true,
			want: CurlProbe{
				URL: "https://x", OK: false, HTTPCode: 404,
				TimeNamelookupS: 0.0001, TimeConnectS: 0.001, TimeAppconnectS: 0,
				TimeStarttransferS: 0.005, TimeTotalS: 0.006, SizeDownloadBytes: 120,
			},
		},
		// curl died before writing the template: all that is left is its stderr.
		{name: "resolve failed", file: "curl/resolve_failed.txt"},
		{name: "certificate verify skipped", file: "curl/cert_verify_skipped.txt"},
		{name: "no marker", file: "curl/no_marker.txt"},
		// The marker is there but the template is cut short.
		{name: "partial fields", file: "curl/partial_fields.txt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseCurlOutput("https://x", fixture.Text(t, tt.file))
			if ok != tt.ok {
				t.Fatalf("ParseCurlOutput() ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if diff := cmp.Diff(tt.want, got, ignoreRawCurl()); diff != "" {
				t.Errorf("ParseCurlOutput() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestParseCurlOutputKeepsRawVerbatim(t *testing.T) {
	text := fixture.Text(t, "curl/ok_200.txt")
	got, ok := ParseCurlOutput("https://x", text)
	if !ok {
		t.Fatal("ParseCurlOutput() reported no probe")
	}
	if got.Raw != text {
		t.Errorf("Raw = %q, want the input verbatim", got.Raw)
	}
}

func TestParseCurlOutputMarker(t *testing.T) {
	tests := []struct {
		name string
		text string
		want CurlProbe
		ok   bool
	}{
		{name: "no marker", text: "nothing here"},
		{
			// splitlines drops the marker's own line, and there is nothing after
			// it.
			name: "marker alone", text: CurlProbeMarker,
		},
		{
			// The line the marker index lands on is dropped whatever else is on
			// it, so a template curl ran into gets read anyway.
			name: "trailing text on the marker line",
			text: CurlProbeMarker + "junk\n" + strings.SplitN(curlFull, "\n", 2)[1],
			want: curlProbe(), ok: true,
		},
		{
			name: "marker mid-line",
			text: "xx" + CurlProbeMarker + "\n" + strings.SplitN(curlFull, "\n", 2)[1],
			want: curlProbe(), ok: true,
		},
		{
			name: "noise before the marker", text: "warning: blah\n" + curlFull,
			want: curlProbe(), ok: true,
		},
		{
			// rfind, so a redirect chain reports the request that finished.
			name: "two templates, the last one wins",
			text: curlFull + strings.Replace(curlFull, "http_code=200", "http_code=503", 1),
			want: func() CurlProbe { p := curlProbe(); p.OK, p.HTTPCode = false, 503; return p }(),
			ok:   true,
		},
		{
			// ...even when the last one is truncated, which is then the whole
			// answer rather than a fallback to the complete earlier block.
			name: "two templates, the last one truncated",
			text: curlFull + CurlProbeMarker + "\nhttp_code=503\n",
		},
		{name: "CRLF", text: strings.ReplaceAll(curlFull, "\n", "\r\n"), want: curlProbe(), ok: true},
		{
			// Python's splitlines boundaries, not just \n.
			name: "field separator", text: strings.ReplaceAll(curlFull, "\n", "\x1c"),
			want: curlProbe(), ok: true,
		},
		{
			name: "no trailing newline", text: strings.TrimSuffix(curlFull, "\n"),
			want: curlProbe(), ok: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseCurlOutput("u", tt.text)
			if ok != tt.ok {
				t.Fatalf("ParseCurlOutput() ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if diff := cmp.Diff(tt.want, got, ignoreRawCurl()); diff != "" {
				t.Errorf("ParseCurlOutput() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestParseCurlOutputFields(t *testing.T) {
	probe := curlProbe
	tests := []struct {
		name string
		text string
		want CurlProbe
		ok   bool
	}{
		{name: "line without a separator", text: curlFull + "junk\n", want: probe(), ok: true},
		{
			// Both halves are stripped.
			name: "padding around key and value",
			text: strings.Replace(curlFull, "http_code=200", "  http_code  =  201  ", 1),
			want: func() CurlProbe { p := probe(); p.HTTPCode = 201; return p }(), ok: true,
		},
		{
			// partition splits on the first '=', so the rest stays in the value
			// and then fails to convert.
			name: "separator inside the value", text: curlWith("http_code", "2=00"),
		},
		{name: "empty key", text: curlFull + "=5\n", want: probe(), ok: true},
		{
			name: "repeated key", text: curlFull + "http_code=503\n",
			want: func() CurlProbe { p := probe(); p.OK, p.HTTPCode = false, 503; return p }(), ok: true,
		},
		{
			// Only the text from the marker on is scanned.
			name: "field before the marker", text: "http_code=503\n" + curlFull,
			want: probe(), ok: true,
		},
		{name: "missing http_code", text: curlWithout("http_code")},
		{name: "missing size_download", text: curlWithout("size_download")},
		{name: "missing time_namelookup", text: curlWithout("time_namelookup")},
		{name: "missing time_connect", text: curlWithout("time_connect")},
		{name: "missing time_appconnect", text: curlWithout("time_appconnect")},
		{name: "missing time_starttransfer", text: curlWithout("time_starttransfer")},
		{name: "missing time_total", text: curlWithout("time_total")},
		{name: "empty http_code", text: curlWith("http_code", "")},
		{name: "http_code written as a float", text: curlWith("http_code", "200.0")},
		{name: "size_download written as a float", text: curlWith("size_download", "9.0")},
		{name: "hexadecimal timing", text: curlWith("time_total", "0x10")},
		// int() and float(), not field validators: everything Python reads, this
		// reads.
		{
			name: "underscored code", text: curlWith("http_code", "2_00"),
			want: probe(), ok: true,
		},
		{
			name: "code in Arabic-Indic digits", text: curlWith("http_code", "٤٢"),
			want: func() CurlProbe { p := probe(); p.OK, p.HTTPCode = false, 42; return p }(), ok: true,
		},
		{
			name: "signed code", text: curlWith("http_code", "+200"),
			want: probe(), ok: true,
		},
		{
			name: "negative code", text: curlWith("http_code", "-1"),
			want: func() CurlProbe { p := probe(); p.OK, p.HTTPCode = false, -1; return p }(), ok: true,
		},
		{
			name: "underscored timing", text: curlWith("time_total", "1_0.5"),
			want: func() CurlProbe { p := probe(); p.TimeTotalS = 10.5; return p }(), ok: true,
		},
		{
			name: "exponent timing", text: curlWith("time_total", "5e-1"),
			want: probe(), ok: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseCurlOutput("u", tt.text)
			if ok != tt.ok {
				t.Fatalf("ParseCurlOutput() ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if diff := cmp.Diff(tt.want, got, ignoreRawCurl()); diff != "" {
				t.Errorf("ParseCurlOutput() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// The OK window, at both edges. 3xx counts because curl was asked to follow
// redirects, so a 3xx that reached the parser is a server that answered.
func TestCurlProbeOKWindow(t *testing.T) {
	for _, tt := range []struct {
		code string
		want bool
	}{
		{"0", false}, {"199", false}, {"200", true}, {"399", true}, {"400", false}, {"503", false},
	} {
		t.Run(tt.code, func(t *testing.T) {
			got, ok := ParseCurlOutput("u", curlWith("http_code", tt.code))
			if !ok {
				t.Fatal("ParseCurlOutput() reported no probe")
			}
			if got.OK != tt.want {
				t.Errorf("OK for code %s = %v, want %v", tt.code, got.OK, tt.want)
			}
		})
	}
}

// The documented departure: v1 reports the arbitrary-precision integer.
func TestParseCurlOutputHugeIntegersReportNoProbe(t *testing.T) {
	for _, field := range []string{"http_code", "size_download"} {
		t.Run(field, func(t *testing.T) {
			if _, ok := ParseCurlOutput("u", curlWith(field, "99999999999999999999999")); ok {
				t.Error("ParseCurlOutput() reported a probe, want none")
			}
		})
	}
}

func TestCurlProbeMarshalsAbsentErrorAsNull(t *testing.T) {
	got, ok := ParseCurlOutput("u", curlFull)
	if !ok {
		t.Fatal("ParseCurlOutput() reported no probe")
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if !strings.Contains(string(b), `"error":null`) {
		t.Errorf("json.Marshal() = %s, want a null error", b)
	}
}

func FuzzParseCurlOutput(f *testing.F) {
	for _, name := range []string{
		"curl/ok_200.txt", "curl/not_found_404.txt", "curl/partial_fields.txt",
		"curl/no_marker.txt", "curl/resolve_failed.txt", "curl/cert_verify_skipped.txt",
	} {
		f.Add(fixture.Text(f, name))
	}
	f.Add(curlFull)

	f.Fuzz(func(t *testing.T, output string) {
		got, ok := ParseCurlOutput("u", output)
		if !ok {
			if got != (CurlProbe{}) {
				t.Errorf("ParseCurlOutput() returned %+v with ok=false, want the zero probe", got)
			}
			return
		}
		if got.Raw != output {
			t.Errorf("Raw = %q, want the input verbatim", got.Raw)
		}
		if got.OK != (got.HTTPCode >= 200 && got.HTTPCode < 400) {
			t.Errorf("probe %+v disagrees with its own status code", got)
		}
		if got.Error != nil {
			t.Errorf("probe %+v has an error set, which this path never does", got)
		}
	})
}
