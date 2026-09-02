package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/config"
)

const asset = "fleetfix-linux-x86_64"

// payloadFor is a releases response carrying both assets for one tag.
func payloadFor(tag string) string {
	return fmt.Sprintf(`{
		"tag_name": %q,
		"html_url": "https://github.com/KingPin/FleetFix/releases/tag/%s",
		"body": "notes",
		"assets": [
			{"name": %q, "browser_download_url": "https://example.invalid/bin"},
			{"name": %q, "browser_download_url": "https://example.invalid/sum"}
		]
	}`, tag, tag, asset, asset+".sha256")
}

// checker is a Checker with no network and a stopped clock. Every test starts here,
// so a missing seam shows up as a compile error rather than as a real request.
func checker(t *testing.T, body string) (*Checker, *int) {
	t.Helper()
	calls := 0
	return &Checker{
		URL:       "https://example.invalid/releases/latest",
		AssetName: asset,
		CachePath: filepath.Join(t.TempDir(), "release_check.json"),
		CacheTTL:  CacheTTL,
		Now:       func() time.Time { return time.Unix(1_760_000_000, 0) },
		Fetch: func(context.Context, string) ([]byte, error) {
			calls++
			return []byte(body), nil
		},
	}, &calls
}

func TestANewerReleaseIsReported(t *testing.T) {
	c, _ := checker(t, payloadFor("v2.1.0"))

	rel, ok, err := c.Check(t.Context(), "2.0.0")
	if err != nil || !ok {
		t.Fatalf("Check = %v, %v, want a release", ok, err)
	}
	if rel.Tag != "v2.1.0" || rel.Version != "2.1.0" {
		t.Errorf("got tag %q version %q, want v2.1.0 and 2.1.0", rel.Tag, rel.Version)
	}
	if rel.AssetURL != "https://example.invalid/bin" || rel.ChecksumURL != "https://example.invalid/sum" {
		t.Errorf("got %q and %q, want both asset URLs", rel.AssetURL, rel.ChecksumURL)
	}
}

func TestTheRunningVersionIsNotAnUpdate(t *testing.T) {
	c, _ := checker(t, payloadFor("v2.0.0"))

	if _, ok, err := c.Check(t.Context(), "2.0.0"); ok || err != nil {
		t.Errorf("Check = %v, %v, want no update and no error", ok, err)
	}
}

// An older tag than the running binary. Reachable for real: a release can be
// deleted, which makes the one before it "latest".
func TestAnOlderReleaseIsNotAnUpdate(t *testing.T) {
	c, _ := checker(t, payloadFor("v1.9.0"))

	if _, ok, err := c.Check(t.Context(), "2.0.0"); ok || err != nil {
		t.Errorf("Check = %v, %v, want no update and no error", ok, err)
	}
}

// The distinction v1 could not make. A banner has nothing to say about a rate
// limit, but an operator who typed `fleetfix update` does.
func TestAFailedFetchIsReportedRatherThanSwallowed(t *testing.T) {
	c, _ := checker(t, "")
	c.Fetch = func(context.Context, string) ([]byte, error) {
		return nil, errors.New("the releases API answered 403 rate limit exceeded")
	}

	_, ok, err := c.Check(t.Context(), "2.0.0")
	if ok {
		t.Error("a failed check is not an update")
	}
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("got %v, want the API's answer", err)
	}
}

func TestAReleaseWithNoMatchingAssetIsReported(t *testing.T) {
	c, _ := checker(t, `{"tag_name": "v9.0.0", "assets": []}`)

	_, ok, err := c.Check(t.Context(), "2.0.0")
	if ok {
		t.Error("a release with no binary is not an update")
	}
	if !errors.Is(err, ErrNoAsset) {
		t.Errorf("got %v, want ErrNoAsset", err)
	}
}

// A body that is not a releases response at all -- what a captive portal returns.
func TestAnUndecodableBodyIsAnError(t *testing.T) {
	c, _ := checker(t, "<html>sign in to continue</html>")

	if _, _, err := c.Check(t.Context(), "2.0.0"); err == nil {
		t.Error("an HTML body is not a releases response")
	}
}

func TestAnArchitectureWithNoBinaryIsNotAnError(t *testing.T) {
	c, calls := checker(t, payloadFor("v9.0.0"))
	c.AssetName = ""

	if _, ok, err := c.Check(t.Context(), "2.0.0"); ok || err != nil {
		t.Errorf("Check = %v, %v, want no update and no error", ok, err)
	}
	// And it must not have asked. There is nothing the answer could change.
	if *calls != 0 {
		t.Errorf("fetched %d times; a host with no asset has no question", *calls)
	}
}

// The cache's whole purpose: a relaunch loop must not spend an unauthenticated
// client's sixty requests an hour.
func TestAFreshCacheIsReusedRatherThanRefetched(t *testing.T) {
	c, calls := checker(t, payloadFor("v2.1.0"))

	for i := range 3 {
		if _, ok, err := c.Check(t.Context(), "2.0.0"); !ok || err != nil {
			t.Fatalf("check %d = %v, %v", i, ok, err)
		}
	}
	if *calls != 1 {
		t.Errorf("fetched %d times, want 1", *calls)
	}
}

func TestAStaleCacheIsRefetched(t *testing.T) {
	c, calls := checker(t, payloadFor("v2.1.0"))
	if _, _, err := c.Check(t.Context(), "2.0.0"); err != nil {
		t.Fatalf("first check: %v", err)
	}

	at := c.Now()
	c.Now = func() time.Time { return at.Add(CacheTTL + time.Second) }
	if _, _, err := c.Check(t.Context(), "2.0.0"); err != nil {
		t.Fatalf("second check: %v", err)
	}

	if *calls != 2 {
		t.Errorf("fetched %d times, want 2", *calls)
	}
}

// What `fleetfix update` sets. An operator who asked is owed a fresh answer, not
// one from fifty minutes ago.
func TestAZeroTTLAlwaysRefetches(t *testing.T) {
	c, calls := checker(t, payloadFor("v2.1.0"))
	c.CacheTTL = 0

	for range 2 {
		if _, _, err := c.Check(t.Context(), "2.0.0"); err != nil {
			t.Fatalf("check: %v", err)
		}
	}
	if *calls != 2 {
		t.Errorf("fetched %d times, want 2", *calls)
	}
}

func TestNoCachePathAlwaysRefetchesAndWritesNothing(t *testing.T) {
	c, calls := checker(t, payloadFor("v2.1.0"))
	where := c.CachePath
	c.CachePath = ""

	for range 2 {
		if _, _, err := c.Check(t.Context(), "2.0.0"); err != nil {
			t.Fatalf("check: %v", err)
		}
	}
	if *calls != 2 {
		t.Errorf("fetched %d times, want 2", *calls)
	}
	if _, err := os.Stat(where); !os.IsNotExist(err) {
		t.Errorf("%s exists (%v); a checker with no cache path writes nothing", where, err)
	}
}

// The shape has to survive the version hop in both directions: a host that ran
// v1.6.0 already has one of these files.
func TestTheCacheKeepsV1sShapeAndTheBytesGitHubSent(t *testing.T) {
	body := payloadFor("v2.1.0")
	c, _ := checker(t, body)
	if _, _, err := c.Check(t.Context(), "2.0.0"); err != nil {
		t.Fatalf("check: %v", err)
	}

	data, err := os.ReadFile(c.CachePath)
	if err != nil {
		t.Fatalf("reading the cache: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decoding the cache: %v", err)
	}
	for _, key := range []string{"checked_at", "payload"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("the cache has no %q; v1 reads that key", key)
		}
	}
	// A float of seconds, which is what time.time() writes and what v1 type-checks.
	var checkedAt float64
	if err := json.Unmarshal(raw["checked_at"], &checkedAt); err != nil {
		t.Fatalf("checked_at is not a number: %v", err)
	}
	if want := float64(c.Now().Unix()); checkedAt != want {
		t.Errorf("checked_at = %v, want %v", checkedAt, want)
	}
	// The payload is the response, not a re-encoding of it.
	var stored, sent any
	if err := json.Unmarshal(raw["payload"], &stored); err != nil {
		t.Fatalf("decoding the cached payload: %v", err)
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("decoding the body: %v", err)
	}
	if fmt.Sprint(stored) != fmt.Sprint(sent) {
		t.Error("the cached payload is not the response that was received")
	}
}

// A cache written by a v1 host, at the seconds-with-a-fraction time.time() produces.
func TestACacheWrittenByV1IsReadable(t *testing.T) {
	c, calls := checker(t, payloadFor("v9.9.9"))
	written := fmt.Sprintf(`{"checked_at": %.6f, "payload": %s}`,
		float64(c.Now().Unix())-60.5, payloadFor("v2.1.0"))
	if err := os.WriteFile(c.CachePath, []byte(written), 0o600); err != nil {
		t.Fatalf("writing the cache: %v", err)
	}

	rel, ok, err := c.Check(t.Context(), "2.0.0")
	if err != nil || !ok {
		t.Fatalf("Check = %v, %v, want the cached release", ok, err)
	}
	if rel.Tag != "v2.1.0" {
		t.Errorf("got %q, want the cached v2.1.0 rather than a refetch", rel.Tag)
	}
	if *calls != 0 {
		t.Errorf("fetched %d times; the cache was fresh", *calls)
	}
}

// Every unusable cache is a miss, not an error: the fetch below it answers the same
// question from the source.
func TestAnUnusableCacheIsAMiss(t *testing.T) {
	for _, tt := range []struct {
		name, contents string
	}{
		{"truncated", `{"checked_at": 17600000`},
		{"not an object", `[1, 2, 3]`},
		{"no timestamp", `{"payload": {"tag_name": "v2.1.0"}}`},
		{"a payload that is not an object", `{"checked_at": 1760000000, "payload": 5}`},
		{"an empty file", ``},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, calls := checker(t, payloadFor("v2.1.0"))
			if err := os.WriteFile(c.CachePath, []byte(tt.contents), 0o600); err != nil {
				t.Fatalf("writing the cache: %v", err)
			}

			if _, ok, err := c.Check(t.Context(), "2.0.0"); !ok || err != nil {
				t.Fatalf("Check = %v, %v, want the refetched release", ok, err)
			}
			if *calls != 1 {
				t.Errorf("fetched %d times, want 1", *calls)
			}
		})
	}
}

// A read-only cache directory means every check goes to the network, which is
// slower and completely correct.
func TestAnUnwritableCacheIsNotAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory regardless of its mode")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	c, _ := checker(t, payloadFor("v2.1.0"))
	c.CachePath = filepath.Join(dir, "sub", "release_check.json")

	if _, ok, err := c.Check(t.Context(), "2.0.0"); !ok || err != nil {
		t.Errorf("Check = %v, %v, want the release despite the unwritable cache", ok, err)
	}
}

// A body that will not decode must not become an hour of cached nonsense.
func TestAnUndecodableBodyIsNotCached(t *testing.T) {
	c, _ := checker(t, "<html>sign in</html>")

	if _, _, err := c.Check(t.Context(), "2.0.0"); err == nil {
		t.Fatal("an HTML body is not a releases response")
	}
	if _, err := os.Stat(c.CachePath); !os.IsNotExist(err) {
		t.Errorf("the cache exists (%v); a body that would not decode was cached", err)
	}
}

func TestNewCheckerPointsAtTheRealEndpoint(t *testing.T) {
	dir := t.TempDir()
	c := NewChecker(config.Paths{CacheDir: dir})

	if c.URL != config.GitHubReleasesURL {
		t.Errorf("got %q, want the releases API", c.URL)
	}
	if c.CachePath != filepath.Join(dir, config.ReleaseCacheFile) {
		t.Errorf("got %q, want the cache under %q", c.CachePath, dir)
	}
	if c.CacheTTL != CacheTTL {
		t.Errorf("got %v, want %v", c.CacheTTL, CacheTTL)
	}
	// Both architectures this project publishes for, and nothing else.
	if c.AssetName != "fleetfix-linux-x86_64" && c.AssetName != "fleetfix-linux-aarch64" {
		t.Errorf("got %q, want a published asset name", c.AssetName)
	}
}

func TestAnArchitectureWithNoPublishedBinaryGetsNoAssetName(t *testing.T) {
	original := goarch
	t.Cleanup(func() { goarch = original })
	goarch = "riscv64"

	if got := NewChecker(config.Paths{CacheDir: t.TempDir()}).AssetName; got != "" {
		t.Errorf("got %q, want no asset for an architecture nothing is published for", got)
	}
}

// The real request, against a server that is not GitHub. Everything above this
// replaces Fetch, so without it the headers and the status handling ship untested.
func TestTheHTTPFetcherSendsGitHubsHeadersAndReadsTheBody(t *testing.T) {
	var gotAccept, gotAgent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept, gotAgent = r.Header.Get("Accept"), r.Header.Get("User-Agent")
		fmt.Fprint(w, payloadFor("v2.1.0"))
	}))
	defer srv.Close()

	body, err := fetchHTTP(t.Context(), srv.URL)
	if err != nil {
		t.Fatalf("fetching: %v", err)
	}
	if !strings.Contains(string(body), "v2.1.0") {
		t.Errorf("got %q, want the release body", body)
	}
	if gotAccept != "application/vnd.github+json" {
		t.Errorf("Accept = %q, want GitHub's media type", gotAccept)
	}
	// v1 sent none. An operator reading a proxy log should see which binary called.
	if !strings.HasPrefix(gotAgent, "fleetfix/") {
		t.Errorf("User-Agent = %q, want it to name this binary", gotAgent)
	}
}

// The status alone. A rate-limited response carries a useful JSON explanation and a
// captive portal carries an HTML login page, and this string reaches a terminal.
func TestAnErrorStatusNamesTheStatusAndNotTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "<html>a login page nobody wants in their terminal</html>")
	}))
	defer srv.Close()

	_, err := fetchHTTP(t.Context(), srv.URL)
	if err == nil {
		t.Fatal("a 403 is not a releases response")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("got %v, want the status", err)
	}
	if strings.Contains(err.Error(), "login page") {
		t.Errorf("got %v, want no body in the message", err)
	}
}

func TestTheHTTPFetcherReportsAnUnreachableHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	if _, err := fetchHTTP(t.Context(), url); err == nil {
		t.Error("a closed server is not a releases response")
	}
}

func TestAnInvalidURLIsReported(t *testing.T) {
	if _, err := fetchHTTP(t.Context(), "://not a url"); err == nil {
		t.Error("a malformed URL is not a request")
	}
}

// Everything above replaces Fetch and Now, so the defaults -- the wiring that
// actually ships -- would otherwise never run. One check with both seams left nil,
// against a server standing in for GitHub.
func TestACheckerWithNoSeamsUsesTheNetworkAndTheWallClock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, payloadFor("v2.1.0"))
	}))
	defer srv.Close()

	c := &Checker{
		URL:       srv.URL,
		AssetName: asset,
		CachePath: filepath.Join(t.TempDir(), "release_check.json"),
		CacheTTL:  CacheTTL,
	}

	rel, ok, err := c.Check(t.Context(), "2.0.0")
	if err != nil || !ok {
		t.Fatalf("Check = %v, %v, want a release", ok, err)
	}
	if rel.Tag != "v2.1.0" {
		t.Errorf("got %q, want v2.1.0", rel.Tag)
	}
	// And the wall clock stamped a cache entry the next check will find fresh.
	before := time.Now()
	srv.Close()
	if _, ok, err := c.Check(t.Context(), "2.0.0"); !ok || err != nil {
		t.Fatalf("Check = %v, %v, want the cached release with the server gone", ok, err)
	}
	data, err := os.ReadFile(c.CachePath)
	if err != nil {
		t.Fatalf("reading the cache: %v", err)
	}
	var cached cacheFile
	if err := json.Unmarshal(data, &cached); err != nil {
		t.Fatalf("decoding the cache: %v", err)
	}
	if at := floatSeconds(cached.CheckedAt); at.After(before) {
		t.Errorf("checked_at is %v, after the check at %v", at, before)
	}
}
