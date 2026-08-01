package updater

import (
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"testing"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
)

const releaseBase = "https://github.com/KingPin/FleetFix/releases/download/v2.0.0"

// parse decodes a fixture and runs ParseRelease over it, which is what the shipping
// checker will do with a response body.
func parse(t *testing.T, rel, asset string) (Release, bool) {
	t.Helper()
	payload, err := DecodePayload(fixture.Bytes(t, rel))
	if err != nil {
		t.Fatalf("DecodePayload(%s): %v", rel, err)
	}
	return ParseRelease(payload, asset)
}

// The real v1.6.0 payload, captured from the API. It is the one that matters most:
// it is what a v1.6.0 host fetches, and the release it names is the binary the whole
// auto-update continuity story runs through.
func TestParseReleaseFixture(t *testing.T) {
	got, ok := parse(t, "release/latest.json", assetName)
	if !ok {
		t.Fatal("ParseRelease(latest) reported no release")
	}
	want := Release{
		Tag:         "v1.6.0",
		Version:     "1.6.0",
		AssetURL:    "https://github.com/KingPin/FleetFix/releases/download/v1.6.0/fleetfix-linux-x86_64",
		ChecksumURL: "https://github.com/KingPin/FleetFix/releases/download/v1.6.0/fleetfix-linux-x86_64.sha256",
		HTMLURL:     "https://github.com/KingPin/FleetFix/releases/tag/v1.6.0",
		Body:        got.Body, // 3.9 KB of release notes; length is what is worth asserting.
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ParseRelease(latest) mismatch (-want +got):\n%s", diff)
	}
	// Runes, not bytes: 3924 is what Python's len() reports, and the notes hold
	// em-dashes. Counting bytes here would pass and would be measuring something the
	// oracle never said.
	if n := utf8.RuneCountInString(got.Body); n != 3924 {
		t.Errorf("body is %d characters, want the captured 3924", n)
	}
}

// The asset name is what selects a build, so a release that published only the other
// architecture must report nothing rather than the wrong binary.
func TestParseReleaseFixtureWithNoMatchingAsset(t *testing.T) {
	if got, ok := parse(t, "release/latest.json", "fleetfix-linux-aarch64"); ok {
		t.Errorf("ParseRelease(latest, aarch64) = %+v, want no release", got)
	}
}

// The adversarial corpus, one fixture per branch. Every expectation is what CPython
// 3.14.6's parse_release returned for the same bytes, with a Python None written as
// ok=false.
func TestParseReleaseFixtures(t *testing.T) {
	full := Release{
		Tag:         "v2.0.0",
		Version:     "2.0.0",
		AssetURL:    releaseBase + "/" + assetName,
		ChecksumURL: releaseBase + "/" + assetName + ".sha256",
		HTMLURL:     "https://example.invalid/r/v2.0.0",
		Body:        "notes",
	}
	tests := []struct {
		fixture string
		why     string
		want    Release
		ok      bool
	}{
		{fixture: "no_checksum", why: "nothing to verify the binary against"},
		{fixture: "no_binary", why: "a checksum for a binary that was never uploaded"},
		{fixture: "empty_download_url", why: "a blank URL is as good as a missing asset"},
		{fixture: "tag_missing", why: "no tag_name at all"},
		{fixture: "tag_not_a_string", why: "a numeric tag is refused, not rendered"},
		{fixture: "tag_empty", why: "an empty tag is falsy"},
		{fixture: "assets_not_a_list", why: "a truthy assets that is not a list"},
		{
			fixture: "junk_assets",
			why:     "entries that are not assets are skipped, and the last of two same-named assets wins",
			ok:      true,
			want: func() Release {
				r := full
				r.AssetURL = releaseBase + "/second/" + assetName
				return r
			}(),
		},
		{
			fixture: "odd_scalars",
			why:     "html_url and body are the only fields str() renders rather than type-checks",
			ok:      true,
			want: func() Release {
				r := full
				r.HTMLURL, r.Body = "42", "True"
				return r
			}(),
		},
		{
			fixture: "falsy_scalars",
			why:     "`or \"\"` takes a falsy value before str() sees it, so a null body is not \"None\"",
			ok:      true,
			want: func() Release {
				r := full
				r.HTMLURL, r.Body = "", ""
				return r
			}(),
		},
		{
			fixture: "wide_scalars",
			why:     "a float renders as Python's repr and an integer past int64 keeps every digit",
			ok:      true,
			want: func() Release {
				r := full
				r.HTMLURL, r.Body = "1.5", "12345678901234567890123456789"
				return r
			}(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			got, ok := parse(t, "release/"+tt.fixture+".json", assetName)
			if ok != tt.ok {
				t.Fatalf("ParseRelease = %+v, %v, want ok=%v (%s)", got, ok, tt.ok, tt.why)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("%s mismatch (-want +got):\n%s", tt.why, diff)
			}
		})
	}
}

// The shapes the corpus cannot hold, because a fixture is one payload and these are
// variations on a single field.
func TestParseRelease(t *testing.T) {
	assets := []any{
		map[string]any{"name": assetName, "browser_download_url": "u"},
		map[string]any{"name": assetName + ".sha256", "browser_download_url": "c"},
	}
	tests := []struct {
		name string
		// asset defaults to the amd64 name; only the concatenation case needs another.
		asset   string
		payload map[string]any
		want    Release
		ok      bool
	}{
		{
			name:    "a bare tag needs no v to strip",
			payload: map[string]any{"tag_name": "2.0.0", "assets": assets},
			ok:      true,
			want:    Release{Tag: "2.0.0", Version: "2.0.0", AssetURL: "u", ChecksumURL: "c"},
		},
		{
			// tag[1:] drops one character, and only when the first is a "v".
			name:    "only the first v is stripped",
			payload: map[string]any{"tag_name": "vv2.0.0", "assets": assets},
			ok:      true,
			want:    Release{Tag: "vv2.0.0", Version: "v2.0.0", AssetURL: "u", ChecksumURL: "c"},
		},
		{
			// A tag that is nothing but the prefix leaves an empty version, which the
			// comparison then refuses. Refusing it here instead would be a different
			// answer, and the tag is still what gets recorded.
			name:    "a tag of just v",
			payload: map[string]any{"tag_name": "v", "assets": assets},
			ok:      true,
			want:    Release{Tag: "v", Version: "", AssetURL: "u", ChecksumURL: "c"},
		},
		{
			name:    "a null tag",
			payload: map[string]any{"tag_name": nil, "assets": assets},
		},
		{
			name:    "no assets key at all",
			payload: map[string]any{"tag_name": "v2.0.0"},
		},
		{
			// `.get("assets") or []` makes every falsy assets an empty list, so these
			// three walk nothing rather than being refused for their type.
			name:    "a null assets",
			payload: map[string]any{"tag_name": "v2.0.0", "assets": nil},
		},
		{
			name:    "an empty assets",
			payload: map[string]any{"tag_name": "v2.0.0", "assets": []any{}},
		},
		{
			name:    "a falsy assets of the wrong type",
			payload: map[string]any{"tag_name": "v2.0.0", "assets": ""},
		},
		{
			// A truthy non-list is the one shape refused for its type. The answer is
			// the same either way, which is why the branch is easy to lose.
			name:    "a truthy assets of the wrong type",
			payload: map[string]any{"tag_name": "v2.0.0", "assets": "fleetfix-linux-x86_64"},
		},
		{
			name: "an asset with no name",
			payload: map[string]any{"tag_name": "v2.0.0", "assets": []any{
				map[string]any{"browser_download_url": "u"},
				assets[1],
			}},
		},
		{
			// The checksum name is built by concatenation, so an asset name that
			// already ends in .sha256 asks for <name>.sha256.sha256.
			name:  "an asset name that is itself a checksum name",
			asset: "x.sha256",
			payload: map[string]any{"tag_name": "v2.0.0", "assets": []any{
				map[string]any{"name": "x.sha256", "browser_download_url": "u"},
				map[string]any{"name": "x.sha256.sha256", "browser_download_url": "c"},
			}},
			ok:   true,
			want: Release{Tag: "v2.0.0", Version: "2.0.0", AssetURL: "u", ChecksumURL: "c"},
		},
		{
			name: "an empty checksum URL",
			payload: map[string]any{"tag_name": "v2.0.0", "assets": []any{
				assets[0],
				map[string]any{"name": assetName + ".sha256", "browser_download_url": ""},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			asset := tt.asset
			if asset == "" {
				asset = assetName
			}
			got, ok := ParseRelease(tt.payload, asset)
			if ok != tt.ok {
				t.Fatalf("ParseRelease = %+v, %v, want ok=%v", got, ok, tt.ok)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// ParseRelease must not rewrite the payload it was handed: the checker caches the
// decoded response and parses it again on the next launch.
func TestParseReleaseLeavesThePayloadAlone(t *testing.T) {
	payload, err := DecodePayload(fixture.Bytes(t, "release/junk_assets.json"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ParseRelease(payload, assetName); !ok {
		t.Fatal("ParseRelease(junk_assets) reported no release")
	}
	after, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("payload changed:\nbefore %s\nafter  %s", before, after)
	}
}

// Numbers are where a JSON decode can quietly disagree with Python's, and html_url
// and body are where that disagreement becomes visible.
func TestDecodePayloadNumbers(t *testing.T) {
	body := []byte(`{
		"int": 42,
		"negative_zero": -0,
		"float": 1.5,
		"exponent": 1e3,
		"float_zero": 0.0,
		"past_int64": 12345678901234567890123456789,
		"past_float64": 1e400,
		"nested": {"list": [1, 2.5, null, true, "s"]}
	}`)
	got, err := DecodePayload(body)
	if err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	big29, _ := new(big.Int).SetString("12345678901234567890123456789", 10)
	want := map[string]any{
		// An integer literal is a Python int, so str() shows its digits and not a
		// float's. -0 is an integer literal too: int("-0") is 0.
		"int":           int64(42),
		"negative_zero": int64(0),
		// Anything with a "." or an exponent is a float, including 1e3 -- which
		// str()s to "1000.0" rather than keeping the literal's spelling.
		"float":        1.5,
		"exponent":     1000.0,
		"float_zero":   0.0,
		"past_int64":   big29,
		"past_float64": math.Inf(1),
		"nested":       map[string]any{"list": []any{int64(1), 2.5, nil, true, "s"}},
	}
	// big.Int has unexported fields, which cmp panics on rather than reaching for
	// Cmp itself.
	bigInts := cmp.Comparer(func(a, b *big.Int) bool { return a.Cmp(b) == 0 })
	if diff := cmp.Diff(want, got, bigInts); diff != "" {
		t.Errorf("DecodePayload mismatch (-want +got):\n%s", diff)
	}
}

func TestDecodePayloadRejections(t *testing.T) {
	tests := []struct {
		name string
		body string
		want error
	}{
		// json.loads succeeds here and payload.get is what fails, so v1's answer is an
		// AttributeError rather than a decode error.
		{"a list", `[]`, ErrNotAnObject},
		{"a string", `"v2.0.0"`, ErrNotAnObject},
		{"a number", `42`, ErrNotAnObject},
		{"null", `null`, ErrNotAnObject},
		// json.loads reads the whole string, so a second value is "Extra data".
		{"two objects", `{"tag_name": "v1"} {"tag_name": "v2"}`, ErrTrailingData},
		{"an object and a scalar", `{} 1`, ErrTrailingData},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := DecodePayload([]byte(tt.body)); !errors.Is(err, tt.want) {
				t.Errorf("DecodePayload(%s) = %v, want %v", tt.body, err, tt.want)
			}
		})
	}
	for _, body := range []string{``, `{`, `{"tag_name": }`, `{"a": 1,}`} {
		if _, err := DecodePayload([]byte(body)); err == nil {
			t.Errorf("DecodePayload(%q) accepted a malformed body", body)
		}
	}
}

// A rate-limit body is valid JSON with none of the keys, which is a no-release rather
// than an error -- the commonest thing the checker actually receives after the happy
// path.
func TestParseReleaseReadsARateLimitBody(t *testing.T) {
	body := []byte(`{"message": "API rate limit exceeded", "documentation_url": "https://docs.github.com"}`)
	payload, err := DecodePayload(body)
	if err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if got, ok := ParseRelease(payload, assetName); ok {
		t.Errorf("ParseRelease(rate limit body) = %+v, want no release", got)
	}
}

// The wire names are what the differential compares against v1's ReleaseInfo fields.
func TestReleaseWireShape(t *testing.T) {
	got, err := json.Marshal(Release{
		Tag: "v2.0.0", Version: "2.0.0",
		AssetURL: "a", ChecksumURL: "c", HTMLURL: "h", Body: "b",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"tag":"v2.0.0","version":"2.0.0","asset_url":"a","checksum_url":"c","html_url":"h","body":"b"}`
	if string(got) != want {
		t.Errorf("Release marshals as\n%s\nwant\n%s", got, want)
	}
}
