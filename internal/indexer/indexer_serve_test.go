package indexer

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cplieger/slogx/capture"
)

// TestServeRejectsUnscopedRequest pins the no-combined-feed contract at the
// HTTP layer: a request that names no tracker by path or host (after passing
// the API-key gate) is 404 with a hint at the per-tracker paths, and no feed
// body is served.
func TestServeRejectsUnscopedRequest(t *testing.T) {
	ix := New(&Config{APIKey: "k"}, nil, nil)
	rec := httptest.NewRecorder()
	ix.serve(rec, httptest.NewRequest(http.MethodGet, "/?t=caps&apikey=k", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unscoped request status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if body := rec.Body.String(); !strings.Contains(body, "/nyaa or /ab") {
		t.Errorf("404 body = %q, want the per-tracker hint", body)
	}
	if strings.Contains(rec.Body.String(), "<caps>") {
		t.Errorf("unscoped request served a caps document: %q", rec.Body.String())
	}
}

// TestServeMarksResponsesNonCacheable pins the sensitive-data cache contract:
// an authenticated /ab RSS response (whose download links embed ab_passkey)
// carries Cache-Control/Pragma headers forbidding any cache from retaining the
// credential-bearing body beyond the request.
func TestServeMarksResponsesNonCacheable(t *testing.T) {
	ix := New(&Config{APIKey: "k", UpstreamConfig: UpstreamConfig{ABPasskey: "pk"}}, nil, nil)
	rec := httptest.NewRecorder()
	ix.serve(rec, httptest.NewRequest(http.MethodGet, "/ab?apikey=k", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated /ab RSS status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got, want := rec.Header().Get("Cache-Control"), "private, no-store, max-age=0"; got != want {
		t.Errorf("Cache-Control = %q, want %q", got, want)
	}
	if got := rec.Header().Get("Pragma"); got != "no-cache" {
		t.Errorf("Pragma = %q, want no-cache", got)
	}
}

// TestRunRefusesEmptyAPIKey pins the fail-closed network boundary: Run with no
// configured API key returns a configuration error before binding a listener,
// so an unauthenticated Torznab feed (whose AnimeBytes RSS links embed
// ab_passkey) can never be served by any construction path. The cancelled
// context guarantees the test cannot hang even if the guard regressed: a bound
// server would fail with a listen/shutdown error that does not name
// feed_api_key.
func TestRunRefusesEmptyAPIKey(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := New(&Config{UpstreamConfig: UpstreamConfig{ABPasskey: "pk"}}, nil, nil).Run(ctx)
	if err == nil {
		t.Fatal("Run with empty APIKey returned nil, want a configuration error")
	}
	if !strings.Contains(err.Error(), "feed_api_key") {
		t.Errorf("Run error = %v, want it to name feed_api_key", err)
	}
}

// TestRunRefusesUnresolvedAPIKeyPlaceholder pins the other half of that
// boundary: a feed_api_key left as a literal ${VAR} reference (the variable
// unset or outside the expansion allowlist) is a GUESSABLE credential - the
// placeholder spelling ships in the public config.example - on the only gate
// protecting the /ab RSS body, whose download links embed ab_passkey. Run must
// refuse to bind on it exactly as it does on an empty key, and the error must
// name only the field, never the value.
func TestRunRefusesUnresolvedAPIKeyPlaceholder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := New(&Config{APIKey: "${SEADEX_SCOUT_FEED_API_KEY}", UpstreamConfig: UpstreamConfig{ABPasskey: "pk"}}, nil, nil).Run(ctx)
	if err == nil {
		t.Fatal("Run with an unresolved ${VAR} APIKey returned nil, want a configuration error")
	}
	if !strings.Contains(err.Error(), "feed_api_key") {
		t.Errorf("Run error = %v, want it to name feed_api_key", err)
	}
	if strings.Contains(err.Error(), "SEADEX_SCOUT_FEED_API_KEY") {
		t.Errorf("Run error echoes the configured value: %v", err)
	}
}

// TestServeFailsClosedWithUnresolvedAPIKey pins the handler's twin guard: a
// placeholder key must answer 503 (auth not configured) rather than
// authenticate a caller who guessed the placeholder.
func TestServeFailsClosedWithUnresolvedAPIKey(t *testing.T) {
	ix := New(&Config{APIKey: "${FEED_KEY}"}, nil, nil)
	rec := httptest.NewRecorder()
	ix.serve(rec, httptest.NewRequest(http.MethodGet, "/nyaa?t=caps&apikey=${FEED_KEY}", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("serve with a placeholder feed_api_key = %d, want 503", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "<caps>") {
		t.Error("serve authenticated a caller who guessed the ${VAR} placeholder")
	}
}

// TestTorznabErrorResponder pins the panic-recovery wire shape: the responder
// webhttp's Recoverer calls must render the status plus a Torznab <error>
// document (code 900, XML-escaped message) on the XML content type - not
// webhttp's default JSON envelope - so a recovered panic still reads as a
// Torznab error to the arrs.
func TestTorznabErrorResponder(t *testing.T) {
	rec := httptest.NewRecorder()
	torznabErrorResponder(rec, nil, http.StatusInternalServerError, "", `boom & <panic> "quoted"`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/xml; charset=utf-8" {
		t.Errorf("content type = %q, want application/xml; charset=utf-8", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<error code="900"`) {
		t.Errorf("body = %q, want a Torznab <error> with the unknown-error code 900", body)
	}
	if !strings.Contains(body, "boom &amp; &lt;panic&gt;") {
		t.Errorf("body = %q, want the XML-escaped panic message", body)
	}
}

// TestUpstreamParams pins the search-proxy parameter gate: only the known
// Torznab params are forwarded to Prowlarr, the feed's own apikey (the
// operator's feed secret) is NEVER forwarded upstream, a missing t defaults to
// a basic search, and the forwarded limit is always the decoder's own window
// (maxItems) rather than the client's - the client's limit counts CURATED
// items, so forwarding it truncated the upstream page before curation ran and
// hid a curated release sitting past the arr's page size (h-f12).
func TestUpstreamParams(t *testing.T) {
	in := url.Values{
		"t": {"tvsearch"}, "q": {"Frieren"}, "season": {"1"}, "limit": {"50"},
		"apikey": {"feed-secret"}, "extended": {"1"},
	}
	out := upstreamParams(in)
	if got := out.Get("apikey"); got != "" {
		t.Errorf("apikey forwarded upstream = %q, want it stripped (feed secret must not reach Prowlarr)", got)
	}
	if got := out.Get("extended"); got != "" {
		t.Errorf("unknown param forwarded upstream = %q, want it dropped", got)
	}
	if out.Get("t") != "tvsearch" || out.Get("q") != "Frieren" || out.Get("season") != "1" {
		t.Errorf("forwarded params = %v, want t/q/season passed through", out)
	}
	if got := out.Get("limit"); got != strconv.Itoa(maxItems) {
		t.Errorf("forwarded limit = %q, want the full decoder window %d", got, maxItems)
	}
	if got := upstreamParams(url.Values{"q": {"Frieren"}}); got.Get("t") != "search" {
		t.Errorf("default t = %q, want search", got.Get("t"))
	}
	// offset still rides through: it names a position in the upstream's own
	// result list, which local curation filtering does not reinterpret.
	if got := upstreamParams(url.Values{"q": {"x"}, "offset": {"100"}}); got.Get("offset") != "100" {
		t.Errorf("forwarded offset = %q, want 100", got.Get("offset"))
	}
}

// TestQueryTotalUpstreamFailureReturnsFault pins the failure contract of
// the search proxy: an upstream whose response cannot be parsed (Prowlarr down
// or misbehaving) yields an empty result plus a torznabFault - so serve
// renders a Torznab <error>, never a fake-empty 200 feed that would read as a
// clean no-match to the arr - plus one warning. With per-tracker scoping a
// request queries exactly one upstream, so this single-upstream failure IS the
// total upstream failure (there is no partial case). Also exercises the AB
// upstream wiring in New.
func TestQueryTotalUpstreamFailureReturnsFault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "not xml at all")
	}))
	defer srv.Close()

	log, rec := capture.New()
	ix := New(&Config{UpstreamConfig: UpstreamConfig{ABTorznabURL: srv.URL, ProwlarrAPIKey: "k"}}, log, srv.Client())

	items, stats, fault := ix.query(context.Background(), url.Values{"t": {"tvsearch"}, "q": {"Frieren"}}, "ab")
	if len(items) != 0 {
		t.Fatalf("got %d items from a failed upstream, want 0", len(items))
	}
	if !stats.answered || stats.feed || stats.upstream != 0 || stats.curated != 0 {
		t.Errorf("stats = %+v, want answered search with 0 upstream/curated", stats)
	}
	if fault == nil {
		t.Errorf("fault = nil, want a torznabFault (a total upstream failure must render a Torznab <error>, not an empty feed)")
	} else if fault.summary != "upstream query failed" {
		t.Errorf("fault.summary = %q, want %q", fault.summary, "upstream query failed")
	}
	if !rec.Contains("upstream query failed") {
		t.Errorf("upstream failure not warned; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestServeTotalUpstreamFailureRendersTorznabError pins the wire shape of a
// total Prowlarr upstream failure end to end: the search response the arr
// receives is a Torznab <error> document (code 900, XML content type, no <rss>
// feed), matching the endpoint's other <error> responses, so a Prowlarr outage
// surfaces as a failed search rather than being recorded as a successful
// no-results one.
func TestServeTotalUpstreamFailureRendersTorznabError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "not xml at all")
	}))
	defer srv.Close()

	ix := New(&Config{APIKey: "k", UpstreamConfig: UpstreamConfig{NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "pk"}},
		nil, srv.Client())
	rec := httptest.NewRecorder()
	ix.serve(rec, httptest.NewRequest(http.MethodGet, "/nyaa?t=tvsearch&q=Frieren&apikey=k", nil))

	if ct := rec.Header().Get("Content-Type"); ct != "application/xml; charset=utf-8" {
		t.Errorf("content type = %q, want application/xml; charset=utf-8", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<error code="900"`) || !strings.Contains(body, "upstream Prowlarr query failed") {
		t.Errorf("body = %q, want a Torznab <error code=\"900\"> naming the upstream failure", body)
	}
	if strings.Contains(body, "<rss") {
		t.Errorf("body = %q, want no RSS feed on a total upstream failure", body)
	}
}

// TestServeStartupSnapshotFailureRendersTorznabError pins the startup
// false-empty gate: a daemon starting over a malformed feed snapshot (before
// any snapshot has ever loaded) holds a zero-value in-memory snapshot that is
// a local fault, not a fresh install - so a search must NOT contact Prowlarr
// (it would filter every result against nil curation maps) and both request
// kinds must answer a Torznab <error> (code 900) rather than an empty 200
// feed the arr would record as a clean no-match. The WARN is bounded to one
// per onset, and a subsequently written valid snapshot restores normal
// serving.
func TestServeStartupSnapshotFailureRendersTorznabError(t *testing.T) {
	upstreamCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		_, _ = io.WriteString(w, `<rss><channel></channel></rss>`)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "feed.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write malformed snapshot: %v", err)
	}
	log, logRec := capture.New()
	ix := warmedIndexer(&Config{APIKey: "k", SnapshotPath: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "pk"}},
		log, srv.Client())

	rec := httptest.NewRecorder()
	ix.serve(rec, httptest.NewRequest(http.MethodGet, "/nyaa?t=tvsearch&q=Frieren&apikey=k", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `<error code="900"`) || !strings.Contains(body, "feed snapshot unavailable") {
		t.Errorf("search body = %q, want a Torznab <error code=\"900\"> naming the unavailable snapshot", body)
	}
	if strings.Contains(body, "<rss") {
		t.Errorf("search body = %q, want no RSS feed while the snapshot is unavailable", body)
	}
	if upstreamCalls != 0 {
		t.Errorf("Prowlarr queried %d times during a snapshot-unavailable search, want 0", upstreamCalls)
	}

	rec = httptest.NewRecorder()
	ix.serve(rec, httptest.NewRequest(http.MethodGet, "/nyaa?apikey=k", nil))
	if body := rec.Body.String(); !strings.Contains(body, `<error code="900"`) {
		t.Errorf("RSS body = %q, want a Torznab <error code=\"900\"> instead of a false-empty feed", body)
	}

	const warnMsg = "indexer feed snapshot unavailable; answering Torznab requests with an error until a snapshot loads"
	if got := logRec.CountExact(warnMsg); got != 1 {
		t.Errorf("snapshot-unavailable WARN count = %d, want 1 (bounded per onset); log output:\n%s",
			got, strings.Join(logRec.Messages(), "\n"))
	}

	// A cycle writes a valid snapshot: the state clears and requests serve
	// the feed normally again. The in-place rewrite lands on the memoized
	// malformed file's inode within the filesystem's mtime granularity, so
	// bump the mtime or matchesFailedFile would skip the reread (production
	// writes are atomic renames, which install a new inode instead).
	writeSnapshotFile(t, path, &snapshot{Owners: owns(), Published: map[string]bool{}})
	bumpMtime(t, path)
	tick(ix)
	rec = httptest.NewRecorder()
	ix.serve(rec, httptest.NewRequest(http.MethodGet, "/nyaa?apikey=k", nil))
	if body := rec.Body.String(); !strings.Contains(body, "<rss") || strings.Contains(body, "<error") {
		t.Errorf("body after a valid snapshot = %q, want a normal RSS feed", body)
	}
}

// TestQuerySkipsPerEpisodeQuery pins the skip path through query itself: a
// per-episode basic search returns nothing WITHOUT being marked answered, so
// the request log reads as a deliberate skip rather than a no-match.
func TestQuerySkipsPerEpisodeQuery(t *testing.T) {
	ix := New(&Config{}, nil, nil)
	items, stats, _ := ix.query(context.Background(), url.Values{"t": {"search"}, "q": {"Frieren 01"}}, "nyaa")
	if len(items) != 0 {
		t.Fatalf("skipped query returned %d items, want 0", len(items))
	}
	if stats.answered || stats.feed || stats.upstream != 0 || stats.curated != 0 {
		t.Errorf("stats = %+v, want the zero queryStats (deliberate skip)", stats)
	}
}

// seedNyaaFeed installs n synthesized Nyaa journal items (GUIDs "0".."n-1", newest-first order)
// straight into ix's snapshot cache, the shape the paging tests need without a writer round-trip.
// It is the one place a test names the cache's lock.
func seedNyaaFeed(t *testing.T, ix *Indexer, n int) {
	t.Helper()
	feed := make([]journalItem, n)
	for i := range feed {
		feed[i] = journalItem{item: item{Title: "t", GUID: strconv.Itoa(i)}}
	}
	ix.cache.mu.Lock()
	defer ix.cache.mu.Unlock()
	ix.cache.snap.NyaaFeed = feed
}

// TestQueryCapsResults pins the maxItems safety bound: a synthesized feed
// larger than the cap is truncated - even when the request's explicit limit
// exceeds it - so a rendered response can never grow unboundedly. (A
// limit-less request is trimmed to defaultCapsLimit before this cap can bite;
// see TestQueryFeedDefaultLimit.)
func TestQueryCapsResults(t *testing.T) {
	ix := New(&Config{UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, nil, nil)
	seedNyaaFeed(t, ix, maxItems+5)
	items, _, _ := ix.query(context.Background(), url.Values{"t": {"search"}, "limit": {strconv.Itoa(maxItems + 5)}}, "nyaa")
	if len(items) != maxItems {
		t.Fatalf("got %d items, want the maxItems cap %d", len(items), maxItems)
	}
}

// TestQueryFeedDefaultLimit pins the advertised caps default (t=caps declares
// limits default=defaultCapsLimit) on the synthesized-feed path: an empty-q
// request with NO explicit limit returns exactly defaultCapsLimit newest items
// when the feed holds more - never the whole window - so the caps document is
// honest. The window stays anchored at the newest item (the feed is sorted
// newest-first), and an explicit limit still wins over the default.
func TestQueryFeedDefaultLimit(t *testing.T) {
	ix := New(&Config{UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, nil, nil)
	seedNyaaFeed(t, ix, defaultCapsLimit+50)

	items, stats, _ := ix.query(context.Background(), url.Values{"t": {"search"}}, "nyaa")
	if !stats.feed {
		t.Fatal("empty-q query not served from the synthesized feed")
	}
	if len(items) != defaultCapsLimit {
		t.Fatalf("limit-less feed request returned %d items, want the advertised default %d", len(items), defaultCapsLimit)
	}
	if items[0].GUID != "0" || items[defaultCapsLimit-1].GUID != strconv.Itoa(defaultCapsLimit-1) {
		t.Errorf("default window = GUIDs %s..%s, want 0..%d (anchored at the newest item)",
			items[0].GUID, items[defaultCapsLimit-1].GUID, defaultCapsLimit-1)
	}

	explicit, _, _ := ix.query(context.Background(), url.Values{"t": {"search"}, "limit": {"7"}}, "nyaa")
	if len(explicit) != 7 {
		t.Errorf("explicit limit=7 returned %d items, want 7 (an explicit limit wins over the default)", len(explicit))
	}
}

// TestReloadKeepsFeedOnUnreadableSnapshot pins the read-failure leg of reload's
// resilience contract (the sibling of the malformed-JSON case): once a good
// feed is loaded, a snapshot that stats as a regular file but cannot be read
// (here one past the bounded-read limit - a root-safe injection, unlike a
// chmod, which a root test process reads anyway) is warned about and ignored,
// never blanking the live feed.
func TestReloadKeepsFeedOnUnreadableSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	if err := seedRebuild(path, nyaaTestEntries(1)); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	log, rec := capture.New()
	ix := warmedIndexer(&Config{SnapshotPath: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, log, nil)
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 {
		t.Fatalf("initial feed = %d items, want 1", len(got))
	}

	// Overwrite the snapshot with a regular file past maxFeedBytes at a newer
	// mtime: os.Stat and the regular-file gate both pass, the bounded read
	// fails, and the served feed must survive.
	if err := os.WriteFile(path, make([]byte, maxFeedBytes+1), 0o600); err != nil {
		t.Fatalf("write oversized snapshot: %v", err)
	}
	bumpMtime(t, path)
	ix.cache.refresh(context.Background())
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 {
		t.Errorf("feed after unreadable snapshot = %d items, want 1 (a bad read must not blank a live feed)", len(got))
	}
	if !rec.Contains("indexer feed snapshot unreadable") {
		t.Errorf("unreadable snapshot not warned; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestReloadKeepsFeedOnNonRegularSnapshotPath pins openSnapshot's regular-file
// gate: a snapshot path replaced by anything that is not a regular file (here a
// directory - the root-safe stand-in for the FIFO, socket, and device forms) is
// refused BEFORE the bounded read decodes it, warned about once, and leaves the
// live feed serving. The gate is what rejects a FIFO at the path (whose open
// returns immediately thanks to O_NONBLOCK - the arm
// TestReloadRefusesFifoSnapshotPathWithoutBlocking pins) instead of blocking
// past the warm-load timeout, which would leave the daemon binding neither the
// Torznab listener nor the compare loop.
func TestReloadKeepsFeedOnNonRegularSnapshotPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	if err := seedRebuild(path, nyaaTestEntries(1)); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	log, rec := capture.New()
	ix := warmedIndexer(&Config{SnapshotPath: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, log, nil)
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 {
		t.Fatalf("initial feed = %d items, want 1", len(got))
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove snapshot: %v", err)
	}
	if err := os.Mkdir(path, 0o750); err != nil {
		t.Fatalf("mkdir over snapshot: %v", err)
	}
	bumpMtime(t, path)
	ix.cache.refresh(context.Background())
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 {
		t.Errorf("feed after non-regular snapshot path = %d items, want 1 (a refused path must not blank a live feed)", len(got))
	}
	if !rec.Contains("indexer feed snapshot path is not a regular file; refusing to load it") {
		t.Errorf("non-regular snapshot path not warned; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestReloadRefusesSymlinkedSnapshotPath pins openSnapshot's O_NOFOLLOW arm:
// the reader must refuse a symlink at the snapshot path (ELOOP, the "open
// failed" arm), matching the writer's ErrSymlinkTarget contract, so a link
// planted at /config/feed.json can never make the served feed come from an
// arbitrary file.
func TestReloadRefusesSymlinkedSnapshotPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "feed.json")
	if err := seedRebuild(path, nyaaTestEntries(1)); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	log, rec := capture.New()
	ix := warmedIndexer(&Config{SnapshotPath: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, log, nil)
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 {
		t.Fatalf("initial feed = %d items, want 1", len(got))
	}

	// Replace the snapshot with a symlink to a DIFFERENT valid snapshot: the
	// bytes are loadable, so only the O_NOFOLLOW gate can refuse them.
	other := filepath.Join(dir, "other.json")
	if err := seedRebuild(other, nyaaTestEntries(3)); err != nil {
		t.Fatalf("Rebuild other: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove snapshot: %v", err)
	}
	if err := os.Symlink(other, path); err != nil {
		t.Fatalf("symlink snapshot: %v", err)
	}
	ix.cache.refresh(context.Background())
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 {
		t.Errorf("feed after symlinked snapshot path = %d items, want 1 (the link target must never be loaded)", len(got))
	}
	if !rec.Contains("indexer feed snapshot open failed; keeping current feed") {
		t.Errorf("symlinked snapshot path not warned; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestReloadRefusesFifoSnapshotPathWithoutBlocking pins openSnapshot's
// O_NONBLOCK arm: a FIFO left at the snapshot path must be rejected by the
// regular-file gate rather than blocking the open. A blocking open cannot be
// interrupted, so it would wedge the cache's loader permanently - the served feed
// would freeze on whatever was loaded and never reload again - which makes the
// test asserting this returns at all the regression guard.
func TestReloadRefusesFifoSnapshotPathWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	if err := seedRebuild(path, nyaaTestEntries(1)); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	log, rec := capture.New()
	ix := warmedIndexer(&Config{SnapshotPath: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, log, nil)
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 {
		t.Fatalf("initial feed = %d items, want 1", len(got))
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove snapshot: %v", err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ix.cache.refresh(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh blocked on a FIFO snapshot path; the open must not wait for a writer")
	}
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 {
		t.Errorf("feed after FIFO snapshot path = %d items, want 1", len(got))
	}
	if !rec.Contains("indexer feed snapshot path is not a regular file; refusing to load it") {
		t.Errorf("FIFO snapshot path not warned; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestQueryCallerCancellationIsNotWarnedAsUpstreamFault pins fetchRaw's
// error classification: when the caller (the arr) cancels its request context,
// the failed upstream search returns empty WITHOUT the "upstream query failed"
// WARN, so a client disconnect never reads as a Prowlarr fault in the Loki
// stream. A genuine upstream failure
// (TestQueryTotalUpstreamFailureSetsUpstreamFailed) still warns.
func TestQueryCallerCancellationIsNotWarnedAsUpstreamFault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, `<rss><channel></channel></rss>`)
	}))
	defer srv.Close()

	log, rec := capture.New()
	ix := New(&Config{UpstreamConfig: UpstreamConfig{NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k"}}, log, srv.Client())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	items, stats, fault := ix.query(ctx, url.Values{"t": {"tvsearch"}, "q": {"Frieren"}}, "nyaa")
	if len(items) != 0 {
		t.Fatalf("cancelled search returned %d items, want 0", len(items))
	}
	if !stats.answered || stats.feed || stats.upstream != 0 || stats.curated != 0 {
		t.Errorf("stats = %+v, want an answered search with 0 upstream/curated", stats)
	}
	if fault != nil {
		t.Errorf("fault = %+v on caller cancellation, want nil (a client disconnect must not render a Torznab <error>)", fault)
	}
	if rec.Contains("upstream query failed") {
		t.Errorf("caller cancellation warned as an upstream fault; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestReloadWarnsOnStatFailure pins the open-error visibility leg of reload:
// an open failure other than fs.ErrNotExist (here ENOTDIR via a
// regular-file parent, root-safe in the same way as the bounded-read overflow
// TestReloadKeepsFeedOnUnreadableSnapshot uses for reads) must be warned
// about - a silent stat failure would invisibly freeze the served feed - while
// the current (empty) feed is kept.
func TestReloadWarnsOnStatFailure(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	log, rec := capture.New()
	ix := warmedIndexer(&Config{SnapshotPath: filepath.Join(blocker, "feed.json")}, log, nil)
	if !rec.Contains("indexer feed snapshot open failed") {
		t.Errorf("stat failure (ENOTDIR) not warned; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
	if got := ix.feedFor(upstreamNyaa); len(got) != 0 {
		t.Errorf("feed = %d items, want 0 (current feed kept on stat failure)", len(got))
	}
}

// TestHandlerRoutesTorznabEndpoint pins the mux wiring Run actually serves:
// the catch-all "/" route hands every path to serve, so a scoped Torznab path
// like /nyaa reaches serve (200 caps) and an unscoped path 404s at serve, not
// at the mux.
func TestHandlerRoutesTorznabEndpoint(t *testing.T) {
	h := New(&Config{APIKey: "k"}, nil, nil).handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nyaa?t=caps&apikey=k", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<caps>") {
		t.Fatalf("handler /nyaa caps = %d %q, want 200 with a caps document", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/other?apikey=k", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("handler /other = %d, want 404 (no tracker scope)", rec.Code)
	}
}

// TestServeThrottlesFailedAuth pins the failed-auth throttle at the served
// middleware chain: past the burst of 10, rapid bad-apikey requests get 429
// rejected OUTSIDE the access logger - no access line, no domain line, so a
// wire-speed flood cannot fill the slog/Loki stream - while a correct key is
// never throttled, so the arrs' happy path is untouched even mid-flood.
func TestServeThrottlesFailedAuth(t *testing.T) {
	log, rec := capture.New()
	ix := New(&Config{APIKey: "k"}, log, nil)
	h := ix.chain()
	for i := 1; i <= 10; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/nyaa?apikey=wrong", nil))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("bad-key request %d status = %d, want %d (inside burst)", i, w.Code, http.StatusUnauthorized)
		}
	}
	accessBefore := rec.CountExact("http")
	domainBefore := rec.CountExact("indexer request rejected")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/nyaa?apikey=wrong", nil))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("bad-key request past burst status = %d, want %d", w.Code, http.StatusTooManyRequests)
	}
	if got := w.Header().Get("Retry-After"); got != "6" {
		t.Errorf("throttled Retry-After = %q, want %q (one token accrued per 6s)", got, "6")
	}
	if got := rec.CountExact("http"); got != accessBefore {
		t.Errorf("throttled 429 emitted an access line (%d -> %d records); the limiter must sit outside the logger", accessBefore, got)
	}
	if got := rec.CountExact("indexer request rejected"); got != domainBefore {
		t.Errorf("throttled 429 emitted a domain rejection line (%d -> %d records)", domainBefore, got)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/nyaa?t=caps&apikey=k", nil))
	if w.Code != http.StatusOK {
		t.Errorf("good-key request during throttle status = %d, want %d (correct callers are never throttled)", w.Code, http.StatusOK)
	}
}

// TestLogParamBoundsAndCleansRequestValues pins the emit-boundary policy on
// request-controlled log values (URL path, Host, Torznab query params):
// control characters are flattened to spaces (a newline cannot spoof a log
// line) and output past 256 bytes is capped on a rune boundary with a "..."
// marker, so a caller holding the feed key cannot flood a Loki record with a
// near-megabyte query value; a value at exactly the cap is untouched.
func TestLogParamBoundsAndCleansRequestValues(t *testing.T) {
	if got, want := logParam("a\nb"), "a b"; got != want {
		t.Errorf("logParam(control char) = %q, want %q", got, want)
	}
	if got, want := logParam(strings.Repeat("x", 300)), strings.Repeat("x", 256)+"..."; got != want {
		t.Errorf("logParam(300 bytes) = %d bytes %q..., want 256 bytes plus the truncation marker", len(got), got[:16])
	}
	if got := logParam(strings.Repeat("x", 256)); got != strings.Repeat("x", 256) {
		t.Errorf("logParam(exactly 256 bytes) = %d bytes, want the input unchanged", len(got))
	}
}

// TestLogParamCapsAtRuneBoundary pins the rune-boundary guarantee of the
// 256-byte cap: a multibyte rune straddling the cap is dropped whole (never
// split into invalid UTF-8), which a raw byte-slice cap would violate while
// the ASCII-only cases above still passed.
func TestLogParamCapsAtRuneBoundary(t *testing.T) {
	input := strings.Repeat("x", 255) + "é"
	want := strings.Repeat("x", 255) + "..."
	if got := logParam(input); got != want {
		t.Errorf("logParam(multibyte boundary) = %q, want %q", got, want)
	}
}

// TestRunSurfacesBindFailureSynchronously pins Run's documented bind
// contract: the listener is bound up front, so a port already in use fails
// Run synchronously with an error naming the address (startIndexer logs it),
// never a silently dead feed goroutine.
func TestRunSurfacesBindFailureSynchronously(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	defer ln.Close()
	orig := listenAddr
	listenAddr = ln.Addr().String()
	defer func() { listenAddr = orig }()
	err = New(&Config{APIKey: "k"}, nil, nil).Run(context.Background())
	if err == nil {
		t.Fatal("Run on an occupied port returned nil, want a bind error")
	}
	if !strings.Contains(err.Error(), "indexer listen on") {
		t.Errorf("Run error = %v, want it wrapped as a listen failure naming the address", err)
	}
}

// TestRunServesAndShutsDownGracefully pins Run's lifecycle: it binds, logs
// the listening line, blocks until the shared daemon context is cancelled,
// then shuts down gracefully returning nil and logging shutdown-complete -
// the contract startIndexer's goroutine and the daemon's shutdown wait rely
// on. The capture recorder is mutex-guarded, so polling it while Run's
// goroutine logs is race-safe; each lifecycle phase carries its own deadline.
func TestRunServesAndShutsDownGracefully(t *testing.T) {
	orig := listenAddr
	listenAddr = "127.0.0.1:0"
	defer func() { listenAddr = orig }()
	log, rec := capture.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- New(&Config{APIKey: "k"}, log, nil).Run(ctx) }()
	startupDeadline := time.After(10 * time.Second)
	for !rec.Contains("seadex-scout indexer listening") {
		select {
		case err := <-done:
			t.Fatalf("Run exited before serving: %v", err)
		case <-startupDeadline:
			t.Fatal("indexer never logged the listening line")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	shutdownDeadline := time.After(10 * time.Second)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on graceful shutdown, want nil", err)
		}
	case <-shutdownDeadline:
		t.Fatal("Run did not return after context cancellation")
	}
	if !rec.Contains("indexer shutdown complete") {
		t.Errorf("shutdown-complete line not logged; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestServeQueryWarnsOnRenderTruncation pins serveQuery's render-budget
// degradation contract end to end: a feed whose rendered document blows
// maxRenderedFeedBytes still answers 200 with a truncated-but-parseable
// Torznab document, and the truncation WARN fires exactly once so the
// request log never falsely reports the full result count while the arr
// silently received a partial feed.
func TestServeQueryWarnsOnRenderTruncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	// Escape-amplified titles: each at-cap "&" title renders ~5x larger
	// (&amp;), so 500 items overshoot the 8 MiB render budget while every
	// item passes the persisted-item limits.
	title := strings.Repeat("&", maxPersistedFieldBytes)
	feed := make([]journalItem, 500)
	now := time.Now().UTC()
	for i := range feed {
		id := strconv.Itoa(i + 1)
		feed[i] = journalItem{
			item:      item{Title: title, GUID: "https://nyaa.si/view/" + id, PubDate: now},
			Key:       "nyaa:" + id,
			FirstSeen: now,
		}
	}
	writeSnapshotFile(t, path, &snapshot{
		Owners: owns(), Published: map[string]bool{},
		NyaaFeed: feed,
	})
	log, rec := capture.New()
	ix := warmedIndexer(&Config{APIKey: "k", SnapshotPath: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, log, nil)

	rr := httptest.NewRecorder()
	ix.serve(rr, httptest.NewRequest(http.MethodGet, "/nyaa?apikey=k&limit=1000", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a truncated feed is still a valid document)", rr.Code)
	}
	if got := rec.CountExact("indexer feed truncated by the render byte budget"); got != 1 {
		t.Fatalf("truncation WARN count = %d, want 1; log output:\n%s", got, strings.Join(rec.Messages(), "\n"))
	}
	parsed, err := parseTorznab(rr.Body.Bytes())
	if err != nil {
		t.Fatalf("truncated response is not parseable Torznab: %v", err)
	}
	if len(parsed) == 0 || len(parsed) >= 500 {
		t.Errorf("truncated response items = %d, want 0 < n < 500", len(parsed))
	}
}

// TestServeNeverLogsTheFeedAPIKey pins the log-hygiene contract server.go
// states in three places (logParam's doc, and chain's notes on the access
// logger and on serve's own domain line): feed_api_key arrives as a QUERY
// parameter, so a line that logged the query string - or a future domain attr
// carrying it - would persist the operator's feed secret into Loki, where it
// is durable and readable by anyone with log access while the endpoint itself
// is only apikey-gated. Every request kind that logs is exercised (authorized
// caps, authorized RSS, unscoped 404, bad-key 401) through the SERVED
// middleware chain, so the access line is covered alongside the domain lines;
// the wrong-key value embeds the real key, so leaking either value fails.
// The property rests on webhttp's RequestLogger recording r.URL.Path only and
// on serve's whitelist of logged params; webhttp is Renovate-bumped, so a
// library change could start recording the query string - which is why this is
// asserted here rather than only stated in a comment.
func TestServeNeverLogsTheFeedAPIKey(t *testing.T) {
	const feedKey = "feed-key-not-a-secret"
	log, rec := capture.New()
	h := New(&Config{APIKey: feedKey}, log, nil).chain()
	for _, target := range []string{
		"/nyaa?t=caps&apikey=" + feedKey,
		"/nyaa?t=search&apikey=" + feedKey,
		"/other?t=caps&apikey=" + feedKey,
		"/nyaa?t=caps&apikey=" + feedKey + "-wrong",
	} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, target, nil))
	}
	if rec.Len() == 0 {
		t.Fatal("no log records captured; the guard is vacuous unless the requests logged")
	}
	if rec.Contains(feedKey) || rec.AttrContains("", "", feedKey) {
		t.Errorf("log records leak the feed API key: %v", rec.Records())
	}
}

// TestServeAppliesLogParamToRequestControlledValues pins that the
// emit-boundary policy is APPLIED at serve's rejection lines, not merely
// implemented: TestLogParamBoundsAndCleansRequestValues exercises logParam in
// isolation, so dropping either call site would leave every unit test green
// while an unauthenticated caller pushed an unbounded path (NewServer permits
// up to 1 MiB of request head) or a control-character-laced Host into a Loki
// record.
func TestServeAppliesLogParamToRequestControlledValues(t *testing.T) {
	log, rec := capture.New()
	ix := New(&Config{APIKey: "k"}, log, nil)
	long := "/nyaa" + strings.Repeat("y", 4096)
	ix.serve(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, long+"?t=caps&apikey=wrong", nil))
	got, ok := rec.AttrValue("indexer request rejected", "path")
	if !ok {
		t.Fatalf("bad-key rejection logged no path attr; records: %v", rec.Messages())
	}
	if want := 256 + len("..."); len(got) != want {
		t.Errorf("logged path = %d bytes, want %d (the 256-byte cap plus the truncation marker)", len(got), want)
	}

	hostLog, hostRec := capture.New()
	hostIx := New(&Config{APIKey: "k"}, hostLog, nil)
	req := httptest.NewRequest(http.MethodGet, "/nope?t=caps&apikey=k", nil)
	req.Host = "host\nwith.control"
	hostIx.serve(httptest.NewRecorder(), req)
	host, ok := hostRec.AttrValue("indexer request rejected", "host")
	if !ok {
		t.Fatalf("unscoped rejection logged no host attr; records: %v", hostRec.Messages())
	}
	if strings.ContainsAny(host, "\n\r") {
		t.Errorf("logged host = %q, want control characters flattened", host)
	}
}

// TestServeSummaryLineReportsTheUpstreamFilterLadder pins the ATTRIBUTE
// semantics of serve's one INFO line per request - the feed's only per-request
// diagnostic - on the search path, the one path where the three counts differ.
// A mock Prowlarr returns two items, one of which carries an off-origin
// download URL, against an empty curation set: upstream_fetched is the raw page
// count, upstream the origin-filter survivors, curated the post-curation
// result, and returned what was actually rendered. The gap between the first
// two is the only standing signal that the origin filter is dropping items
// (its own WARN fires once per onset), so an operator reads this line to tell
// "the tracker returned nothing" from "everything was dropped".
func TestServeSummaryLineReportsTheUpstreamFilterLadder(t *testing.T) {
	const feed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed">
  <channel>
    <title>Nyaa.si</title>
    <item>
      <title>[G] Kept S01 [BD 1080p]</title>
      <guid>https://nyaa.si/view/42</guid>
      <comments>https://nyaa.si/view/42</comments>
      <size>1</size>
      <enclosure url="ORIGIN/1/download?link=kept" length="1" type="application/x-bittorrent"/>
      <torznab:attr name="seeders" value="3"/>
    </item>
    <item>
      <title>[G] Dropped S01 [BD 1080p]</title>
      <guid>https://nyaa.si/view/43</guid>
      <comments>https://nyaa.si/view/43</comments>
      <size>1</size>
      <enclosure url="http://elsewhere.example/evil.torrent" length="1" type="application/x-bittorrent"/>
      <torznab:attr name="seeders" value="3"/>
    </item>
  </channel>
</rss>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		// A healthy Prowlarr hands out download links on the queried
		// endpoint's own origin; the second item deliberately does not.
		_, _ = io.WriteString(w, strings.ReplaceAll(feed, "ORIGIN", "http://"+r.Host))
	}))
	defer srv.Close()

	log, rec := capture.New()
	ix := New(&Config{APIKey: "k", UpstreamConfig: UpstreamConfig{NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "pk"}},
		log, srv.Client())

	w := httptest.NewRecorder()
	ix.serve(w, httptest.NewRequest(http.MethodGet, "/nyaa?t=tvsearch&q=Kept&apikey=k", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("search status = %d, want 200", w.Code)
	}

	for _, want := range []struct{ key, value string }{
		{"scope", "nyaa"},
		{"answered", "true"},
		{"feed", "false"},
		{"upstream_fetched", "2"},
		{"upstream", "1"},
		{"curated", "0"},
		{"identity_conflicts", "0"},
		{"returned", "0"},
	} {
		if !rec.HasAttr("indexer request", want.key, want.value) {
			got, _ := rec.AttrValue("indexer request", want.key)
			t.Errorf("request summary %s = %q, want %q", want.key, got, want.value)
		}
	}
}

// TestRunWarnsOnUnexpandedABPasskeyWithoutLoggingIt pins both halves of Run's
// ab_passkey startup diagnostic. An unexpanded ${VAR} passkey cannot build a
// grabbable AnimeBytes link, so a CONFIGURED AB tracker says so once at
// startup - field-name-only, because the rejected value is a credential
// (CWE-532), which is why the WARN carries no attributes at all. And the gate
// stays scoped to an ENABLED tracker: with ab_torznab_url blank (the README's
// off switch) nothing is served for /ab, so warning there would be exactly the
// parked-credential noise l-f13 removed.
func TestRunWarnsOnUnexpandedABPasskeyWithoutLoggingIt(t *testing.T) {
	orig := listenAddr
	listenAddr = "127.0.0.1:0"
	t.Cleanup(func() { listenAddr = orig })
	const ref = "${SEADEX_SCOUT_AB_PASSKEY}"
	const warnMsg = "indexer.ab_passkey still holds an unexpanded environment-variable reference"
	// A cancelled context guarantees the test cannot serve: the guards run
	// before the bind, and the ephemeral listenAddr keeps a bind that does
	// happen off any real deployment's port.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	log, rec := capture.New()
	_ = New(&Config{APIKey: "k", UpstreamConfig: UpstreamConfig{
		ABTorznabURL: "http://prowlarr:9696/2/api",
		ABPasskey:    ref,
	}}, log, nil).Run(ctx)

	if !rec.Contains(warnMsg) {
		t.Fatalf("configured AB tracker with an unexpanded passkey did not warn: %v", rec.Messages())
	}
	for _, r := range rec.Records() {
		if strings.Contains(r.Message, ref) {
			t.Errorf("the unexpanded passkey reference reached a log message: %q", r.Message)
		}
		if strings.Contains(r.Message, warnMsg) && r.NumAttrs() != 0 {
			t.Errorf("the passkey WARN carries %d attributes, want none (field-name-only: the value is a credential)", r.NumAttrs())
		}
	}

	offLog, offRec := capture.New()
	_ = New(&Config{APIKey: "k", UpstreamConfig: UpstreamConfig{ABPasskey: ref}}, offLog, nil).Run(ctx)
	if offRec.Contains(warnMsg) {
		t.Errorf("warned about a parked passkey for a tracker with no ab_torznab_url: %v", offRec.Messages())
	}
}
