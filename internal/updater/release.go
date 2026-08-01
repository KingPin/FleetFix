package updater

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"strconv"
	"strings"

	"github.com/KingPin/FleetFix/v2/internal/config"
	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// ErrNotAnObject is what a releases API response that decodes to something other
// than an object gets, mirroring v1's "releases API returned non-object".
//
// v1 raises it in the fetcher, before parse_release is reached, and check_for_update
// catches everything and reports no update. Reproduced here as an error rather than
// as a silent absence so the M4 checker can log which of the two happened -- a rate
// limit body and a genuine "no releases yet" both end in no update, and only one of
// them is worth telling the operator about.
var ErrNotAnObject = errors.New("releases API returned non-object")

// ErrTrailingData is a response body holding more than one JSON value, which is an
// "Extra data" JSONDecodeError to json.loads.
var ErrTrailingData = errors.New("releases API returned trailing data")

// Release is a GitHub release reduced to what the updater acts on.
//
// Field names match v1's ReleaseInfo, which is what the differential compares.
type Release struct {
	Tag         string `json:"tag"`
	Version     string `json:"version"`
	AssetURL    string `json:"asset_url"`
	ChecksumURL string `json:"checksum_url"`
	HTMLURL     string `json:"html_url"`
	Body        string `json:"body"`
}

// DecodePayload decodes a releases API response body into the value vocabulary
// ParseRelease reads.
//
// Numbers come out as Python's json.loads spells them -- an integer literal is an
// integer, anything else is a float -- because ParseRelease coerces two of its
// fields with str() and 42 and 42.0 do not render the same way. encoding/json's
// default float64 for every number would turn a release id into "3.59792237e+08".
//
// Integers deliberately do not go through pytext.Int: that saturates past int64,
// where CPython's int does not, and the digits of an out-of-range value are exactly
// what str() would have shown.
func DecodePayload(body []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	// json.loads reads the whole string, so anything after the first value is an
	// error there and must not be silently accepted here -- a Decoder stops at the
	// end of one value and would take {"tag_name": "v1"} out of a truncated response
	// that had been concatenated with the next one.
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, ErrTrailingData
	}
	obj, ok := pythonize(raw).(map[string]any)
	if !ok {
		return nil, ErrNotAnObject
	}
	return obj, nil
}

// pythonize rewrites every json.Number in a decoded value.
func pythonize(v any) any {
	switch t := v.(type) {
	case json.Number:
		return pythonNumber(t)
	case []any:
		for i := range t {
			t[i] = pythonize(t[i])
		}
		return t
	case map[string]any:
		for k := range t {
			t[k] = pythonize(t[k])
		}
		return t
	default:
		return v
	}
}

// pythonNumber is int(literal) for an integer literal and float(literal) otherwise,
// which is the split CPython's JSON scanner makes.
func pythonNumber(n json.Number) any {
	s := n.String()
	if !strings.ContainsAny(s, ".eE") {
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i
		}
		if b, ok := new(big.Int).SetString(s, 10); ok {
			return b
		}
	}
	// pytext.Float is float(), including the +Inf a literal past float64 saturates
	// to rather than the error strconv.ParseFloat reports.
	f, err := pytext.Float(s)
	if err != nil {
		// Unreachable: the decoder has already validated the literal. Keeping the
		// number rather than substituting a zero means a future caller that hands
		// this a hand-built value sees the value it passed in.
		return n
	}
	return f
}

// ParseRelease pulls the binary and checksum asset URLs out of a releases API
// payload, reporting false when the release does not carry both.
//
// Takes the decoded payload rather than the bytes, and a map rather than a struct,
// because v1's tolerance is the behaviour: every field is fetched with .get and
// type-checked in place, so a payload with an unexpected shape produces no release
// instead of an error. Unmarshalling into a struct would reject a numeric html_url
// that v1 renders, and would accept a numeric tag_name that v1 refuses -- both
// silent, and in opposite directions.
//
// The false covers all of: no tag, an empty tag, a tag that is not a string, assets
// that are not a list, and either asset missing. v1 collapses them the same way,
// and the caller does the same thing with each -- there is no update.
func ParseRelease(payload map[string]any, assetName string) (Release, bool) {
	tag, ok := payload["tag_name"].(string)
	if !ok || tag == "" {
		return Release{}, false
	}

	// `payload.get("assets") or []` and then a list check: a falsy assets -- absent,
	// null, [] -- becomes an empty list and walks no entries, while a truthy non-list
	// is refused outright. Both end with no asset URL, so the two paths are only
	// distinguishable by reading the source, and are reproduced for that reason
	// rather than because a caller can tell.
	assets, isList := payload["assets"].([]any)
	if !isList && config.PyTruthy(payload["assets"]) {
		return Release{}, false
	}

	var assetURL, checksumURL string
	for _, entry := range assets {
		asset, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		// A name or URL of the wrong type skips the asset rather than failing the
		// release: GitHub has never sent one, and a release whose other asset is
		// well-formed is still installable.
		name, nameOK := asset["name"].(string)
		url, urlOK := asset["browser_download_url"].(string)
		if !nameOK || !urlOK {
			continue
		}
		// No break, so the last matching asset wins. Nothing publishes a release with
		// two assets of one name, but the loop is what decides which one would be
		// downloaded, and a silent first-wins here would be a different answer.
		switch name {
		case assetName:
			assetURL = url
		case assetName + ".sha256":
			checksumURL = url
		}
	}
	// An empty URL is as good as a missing one, because v1 tests both with `not`. It
	// matters: an asset published with a blank browser_download_url would otherwise
	// be handed to the downloader, which would fail with something less obvious than
	// "this release has no binary".
	if assetURL == "" || checksumURL == "" {
		return Release{}, false
	}

	return Release{
		Tag:         tag,
		Version:     stripV(tag),
		AssetURL:    assetURL,
		ChecksumURL: checksumURL,
		HTMLURL:     pyStrOrEmpty(payload["html_url"]),
		Body:        pyStrOrEmpty(payload["body"]),
	}, true
}

// pyStrOrEmpty is v1's `str(payload.get(key) or "")`.
//
// Two coercions in one expression, and neither is incidental. The `or` makes every
// falsy value the empty string, which is how a null body -- what GitHub sends for a
// release with no notes -- becomes "" rather than "None". The str() then renders
// whatever is left, so these two fields are the only ones a non-string survives.
func pyStrOrEmpty(v any) string {
	if !config.PyTruthy(v) {
		return ""
	}
	return config.PyStr(v)
}

// stripV drops the "v" from a tag so it can be compared against a bare version.
//
// One "v", and only at the front: "vv1.0.0" keeps its second one, which is a tag
// nobody pushes but is what v1's tag[1:] does.
func stripV(tag string) string {
	return strings.TrimPrefix(tag, "v")
}
