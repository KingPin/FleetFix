// Package updater finds and applies a newer release.
//
// Nothing here updates anything on its own. v1 checked on launch and lit a banner,
// and the operator pressed a key; the Go port keeps that shape -- Check reports what
// is available and the caller decides. A fleet tool that replaced its own binary
// because it noticed a tag would be replacing it during someone's incident.
package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/config"
	"github.com/KingPin/FleetFix/v2/internal/release"
	"github.com/KingPin/FleetFix/v2/internal/version"
)

// CacheTTL is how long a releases-API answer is reused.
//
// One hour, from v1. The cache exists to stop a relaunch loop from hammering the
// API: an operator restarting a TUI twenty times during triage would otherwise spend
// twenty of an unauthenticated client's sixty requests per hour, and be rate-limited
// into a silent updater for the rest of it.
const CacheTTL = time.Hour

// HTTPTimeout bounds the whole request. v1's httpx timeout, and the reason the
// launch check cannot wedge a start-up: five seconds is longer than the API needs
// and shorter than an operator will wait before deciding the tool is hung.
const HTTPTimeout = 5 * time.Second

// maxResponse caps what is read from the API.
//
// A releases payload is a few kilobytes and its body is bounded by what a human
// typed into a release note. The cap is not about GitHub -- it is about a proxy or a
// captive portal answering this request with something that never ends, which an
// unbounded ReadAll would spend the host's memory on.
const maxResponse = 8 << 20

// ErrNoAsset is what Check reports when the latest release carries no binary for
// this architecture, or no checksum beside it.
//
// Distinct from "no update", which is the ordinary answer. A release that published
// only some of its assets is a mistake in the release, and an operator who ran
// `fleetfix update` expecting one deserves to hear which of the two happened.
var ErrNoAsset = errors.New("the latest release publishes no matching asset")

// Fetcher retrieves a URL's body. The seam every test uses instead of the network.
type Fetcher func(ctx context.Context, url string) ([]byte, error)

// Checker asks GitHub what the latest release is, through a cache.
//
// The zero value is not usable; use NewChecker. Every field is a seam, and the ones
// that are not obviously seams -- URL, AssetName -- are the ones a test would
// otherwise have to reach the real API to exercise.
type Checker struct {
	// URL is the releases endpoint. Overridable so a test can point at an httptest
	// server, and for a fork that publishes its own builds.
	URL string

	// AssetName is the binary this host would install, e.g. fleetfix-linux-x86_64.
	// Empty means this architecture publishes no binary, and Check reports no update
	// rather than offering one that cannot run here.
	AssetName string

	// CachePath is the file the last answer is kept in. Empty disables the cache
	// entirely, which is what `fleetfix update` wants: an operator who asked is
	// owed a fresh answer, not one from fifty minutes ago.
	CachePath string

	// CacheTTL is how long that file is reused. Zero with a CachePath set means
	// every read is stale, so the file is written and never read -- v1's cache_ttl_s=0.
	CacheTTL time.Duration

	// Fetch retrieves the payload. Nil means an HTTP client with HTTPTimeout.
	Fetch Fetcher

	// Now is the clock the cache's freshness is measured against.
	Now func() time.Time
}

// goarch is the architecture NewChecker asks for an asset name for. A variable so a
// test can be a machine this project publishes no binary for, which is a state no
// amd64 or arm64 runner can otherwise reach.
var goarch = runtime.GOARCH

// NewChecker points a checker at the real releases API and this host's asset.
//
// An architecture with no published binary yields an empty AssetName rather than an
// error. The release for it may exist and simply not carry something this machine
// can run, and that is a "no update" rather than a failure to report.
func NewChecker(paths config.Paths) *Checker {
	asset, err := release.AssetName(goarch)
	if err != nil {
		asset = ""
	}
	return &Checker{
		URL:       config.GitHubReleasesURL,
		AssetName: asset,
		CachePath: paths.ReleaseCachePath(),
		CacheTTL:  CacheTTL,
	}
}

// Check reports the release that should replace currentVersion, if there is one.
//
// The three answers are distinct on purpose, where v1 collapsed them into None:
//   - a release and no error: newer, and installable here
//   - no release and no error: up to date, or nothing to install here
//   - an error: the question could not be answered
//
// v1 returned None for all three because its only caller lit a banner, and a banner
// has nothing to say about a rate limit. `fleetfix update` does: an operator who
// typed the command is owed "GitHub said 403" rather than a silent no. The launch
// check keeps v1's behaviour by ignoring the error.
func (c *Checker) Check(ctx context.Context, currentVersion string) (Release, bool, error) {
	if c.AssetName == "" {
		// Nothing published for this architecture. Not an error: the release exists,
		// it just has nothing this host could run.
		return Release{}, false, nil
	}

	payload, err := c.payload(ctx)
	if err != nil {
		return Release{}, false, err
	}

	rel, ok := ParseRelease(payload, c.AssetName)
	if !ok {
		return Release{}, false, fmt.Errorf("%w: %s", ErrNoAsset, c.AssetName)
	}
	if !IsNewer(rel.Version, currentVersion) {
		return Release{}, false, nil
	}
	return rel, true, nil
}

// payload returns the releases response, from the cache when it is fresh.
//
// A failed fetch does not fall back to a stale cache. v1 does not either, and the
// reason holds: a cache entry an hour past its TTL is evidence about a release that
// may since have been deleted or replaced, and the installer would go on to download
// an asset URL from it. "I could not ask" is the honest answer.
func (c *Checker) payload(ctx context.Context) (map[string]any, error) {
	if cached, ok := c.fresh(); ok {
		return cached, nil
	}

	fetch := c.Fetch
	if fetch == nil {
		fetch = fetchHTTP
	}
	body, err := fetch(ctx, c.URL)
	if err != nil {
		return nil, err
	}
	payload, err := DecodePayload(body)
	if err != nil {
		return nil, err
	}

	// Written after decoding, so a body that is not a releases response never
	// becomes an hour of cached nonsense.
	c.writeCache(body)
	return payload, nil
}

// cacheFile is v1's release_check.json, and the shape is load-bearing in one
// direction: a host that ran v1.6.0 has one of these already, and the two versions
// have to be able to read each other's during the hop.
//
// The payload is kept as the bytes GitHub sent rather than as a re-encoded value.
// Round-tripping it through a decode would mean re-marshalling numbers, and a Go
// encoder's spelling of a release id is not Python's -- the file would still parse
// and would no longer be the response.
type cacheFile struct {
	CheckedAt float64         `json:"checked_at"`
	Payload   json.RawMessage `json:"payload"`
}

// fresh returns the cached payload when there is one and it is inside the TTL.
//
// Every failure is a miss. An unreadable, truncated, or hand-edited cache is not
// worth an error: the fetch below it answers the same question from the source.
func (c *Checker) fresh() (map[string]any, bool) {
	if c.CachePath == "" {
		return nil, false
	}
	data, err := os.ReadFile(c.CachePath) //nolint:gosec // the path is configuration, not input
	if err != nil {
		return nil, false
	}
	var cached cacheFile
	if err := json.Unmarshal(data, &cached); err != nil {
		return nil, false
	}
	if c.now().Sub(floatSeconds(cached.CheckedAt)) >= c.CacheTTL {
		return nil, false
	}
	payload, err := DecodePayload(cached.Payload)
	if err != nil {
		return nil, false
	}
	return payload, true
}

// writeCache stores the response, and says nothing when it cannot.
//
// A read-only or absent cache directory means every check goes to the network,
// which is slower and completely correct. Reporting it would put an error in front
// of an operator about a file that exists to save GitHub some requests.
func (c *Checker) writeCache(body []byte) {
	if c.CachePath == "" {
		return
	}
	data, err := json.Marshal(cacheFile{
		CheckedAt: seconds(c.now()),
		Payload:   body,
	})
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(c.CachePath), 0o700); err != nil {
		return
	}
	_ = os.WriteFile(c.CachePath, data, 0o600)
}

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// seconds and floatSeconds are Python's time.time(): a float of seconds since the
// epoch. Kept as a float rather than an integer because v1 writes one, and a v1
// process reading an integer back would still work -- but only by accident, and the
// accident is not worth relying on in the direction that matters less.
func seconds(t time.Time) float64 {
	return float64(t.UnixNano()) / float64(time.Second)
}

func floatSeconds(s float64) time.Time {
	return time.Unix(0, int64(s*float64(time.Second)))
}

// fetchHTTP is the real request.
//
// The User-Agent is new. v1 sent none, which the API tolerates for now and
// documents as required; more usefully, an operator reading a proxy log sees which
// binary on which host is calling out rather than a bare Go-http-client.
func fetchHTTP(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, HTTPTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", version.UserAgent())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // the body is fully read or abandoned; closing is best-effort

	if resp.StatusCode != http.StatusOK {
		// The status alone, not the body. A rate-limited response carries a JSON
		// explanation that is useful, and a captive portal carries an HTML login
		// page that is not -- and the caller prints this to a terminal.
		return nil, fmt.Errorf("the releases API answered %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxResponse))
}
