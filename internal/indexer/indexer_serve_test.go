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
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/slogx/capture"
)

// TestServeRejectsUnscopedRequest pins the no-combined-feed contract at the
// HTTP layer: a request that names no tracker by path or host (after passing
// the API-key gate) is 404 with a hint at the per-tracker paths, and no feed
// body is served.
func TestServeRejectsUnscopedRequest(t *testing.T) {
	ix := New(&Config{APIKey: "k"}, Deps{}, "")
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
	ix := New(&Config{APIKey: "k", UpstreamConfig: UpstreamConfig{ABPasskey: "pk"}}, Deps{}, "")
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
	err := New(&Config{UpstreamConfig: UpstreamConfig{ABPasskey: "pk"}}, Deps{}, "").Run(ctx)
	if err == nil {
		t.Fatal("Run with empty APIKey returned nil, want a configuration error")
	}
	if !strings.Contains(err.Error(), "feed_api_key") {
		t.Errorf("Run error = %v, want it to name feed_api_key", err)
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
// operator's feed secret) is NEVER forwarded upstream, and a missing t
// defaults to a basic search.
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
	if out.Get("t") != "tvsearch" || out.Get("q") != "Frieren" || out.Get("season") != "1" || out.Get("limit") != "50" {
		t.Errorf("forwarded params = %v, want t/q/season/limit passed through", out)
	}
	if got := upstreamParams(url.Values{"q": {"Frieren"}}); got.Get("t") != "search" {
		t.Errorf("default t = %q, want search", got.Get("t"))
	}
}

// TestQueryTotalUpstreamFailureSetsUpstreamFailed pins the failure contract of
// the search proxy: an upstream whose response cannot be parsed (Prowlarr down
// or misbehaving) yields an empty result flagged upstreamFailed - so serve
// renders a Torznab <error>, never a fake-empty 200 feed that would read as a
// clean no-match to the arr - plus one warning. With per-tracker scoping a
// request queries exactly one upstream, so this single-upstream failure IS the
// total upstream failure (there is no partial case). Also exercises the AB
// upstream wiring in New.
func TestQueryTotalUpstreamFailureSetsUpstreamFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "not xml at all")
	}))
	defer srv.Close()

	log, rec := capture.New()
	ix := New(&Config{UpstreamConfig: UpstreamConfig{ABTorznabURL: srv.URL, ProwlarrAPIKey: "k"}}, Deps{HTTP: srv.Client(), Logger: log}, "")

	items, stats := ix.query(context.Background(), url.Values{"t": {"tvsearch"}, "q": {"Frieren"}}, "ab")
	if len(items) != 0 {
		t.Fatalf("got %d items from a failed upstream, want 0", len(items))
	}
	if !stats.answered || stats.feed || stats.upstream != 0 || stats.curated != 0 {
		t.Errorf("stats = %+v, want answered search with 0 upstream/curated", stats)
	}
	if !stats.upstreamFailed {
		t.Errorf("stats.upstreamFailed = false, want true (a total upstream failure must render a Torznab <error>, not an empty feed)")
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
		Deps{HTTP: srv.Client()}, "")
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
	ix := New(&Config{APIKey: "k", UpstreamConfig: UpstreamConfig{NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "pk"}},
		Deps{HTTP: srv.Client(), Logger: log}, path)

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
	// the feed normally again.
	writeSnapshotFile(t, path, &snapshot{ByHash: map[string]bool{}, ByKey: map[string]bool{}, Seen: map[string]bool{}})
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
	ix := New(&Config{}, Deps{}, "")
	items, stats := ix.query(context.Background(), url.Values{"t": {"search"}, "q": {"Frieren 01"}}, "nyaa")
	if len(items) != 0 {
		t.Fatalf("skipped query returned %d items, want 0", len(items))
	}
	if stats.answered || stats.feed || stats.upstream != 0 || stats.curated != 0 {
		t.Errorf("stats = %+v, want the zero queryStats (deliberate skip)", stats)
	}
}

// TestQueryCapsResults pins the maxItems safety bound: a synthesized feed
// larger than the cap is truncated - even when the request's explicit limit
// exceeds it - so a rendered response can never grow unboundedly. (A
// limit-less request is trimmed to defaultCapsLimit before this cap can bite;
// see TestQueryFeedDefaultLimit.)
func TestQueryCapsResults(t *testing.T) {
	ix := New(&Config{UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{}, "")
	feed := make([]item, maxItems+5)
	for i := range feed {
		feed[i] = item{Title: "t", GUID: strconv.Itoa(i)}
	}
	ix.mu.Lock()
	ix.snap.NyaaFeed = feed
	ix.mu.Unlock()
	items, _ := ix.query(context.Background(), url.Values{"t": {"search"}, "limit": {strconv.Itoa(maxItems + 5)}}, "nyaa")
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
	ix := New(&Config{UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{}, "")
	feed := make([]item, defaultCapsLimit+50)
	for i := range feed {
		feed[i] = item{Title: "t", GUID: strconv.Itoa(i)}
	}
	ix.mu.Lock()
	ix.snap.NyaaFeed = feed
	ix.mu.Unlock()

	items, stats := ix.query(context.Background(), url.Values{"t": {"search"}}, "nyaa")
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

	explicit, _ := ix.query(context.Background(), url.Values{"t": {"search"}, "limit": {"7"}}, "nyaa")
	if len(explicit) != 7 {
		t.Errorf("explicit limit=7 returned %d items, want 7 (an explicit limit wins over the default)", len(explicit))
	}
}

// TestReloadKeepsFeedOnUnreadableSnapshot pins the read-failure leg of reload's
// resilience contract (the sibling of the malformed-JSON case): once a good
// feed is loaded, a snapshot path that stats fine but cannot be read (here a
// directory - a root-safe EISDIR injection) is warned about and ignored, never
// blanking the live feed.
func TestReloadKeepsFeedOnUnreadableSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	entries := []seadex.Entry{{
		AniListID: 7,
		Torrents: []seadex.Torrent{{
			Tracker: "Nyaa", URL: "https://nyaa.si/view/42", IsBest: true,
			Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
		}},
	}}
	if err := seedRebuild(path, entries); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	log, rec := capture.New()
	ix := New(&Config{UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{Logger: log}, path)
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 {
		t.Fatalf("initial feed = %d items, want 1", len(got))
	}

	// Replace the snapshot with a directory at a newer mtime: os.Stat succeeds,
	// the bounded read fails (EISDIR), and the served feed must survive.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove snapshot: %v", err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir over snapshot: %v", err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	ix.reload(context.Background())
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 {
		t.Errorf("feed after unreadable snapshot = %d items, want 1 (a bad read must not blank a live feed)", len(got))
	}
	if !rec.Contains("indexer feed snapshot unreadable") {
		t.Errorf("unreadable snapshot not warned; log output:\n%s", strings.Join(rec.Messages(), "\n"))
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
	ix := New(&Config{UpstreamConfig: UpstreamConfig{NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k"}}, Deps{HTTP: srv.Client(), Logger: log}, "")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	items, stats := ix.query(ctx, url.Values{"t": {"tvsearch"}, "q": {"Frieren"}}, "nyaa")
	if len(items) != 0 {
		t.Fatalf("cancelled search returned %d items, want 0", len(items))
	}
	if !stats.answered || stats.feed || stats.upstream != 0 || stats.curated != 0 {
		t.Errorf("stats = %+v, want an answered search with 0 upstream/curated", stats)
	}
	if stats.upstreamFailed {
		t.Errorf("stats.upstreamFailed = true on caller cancellation, want false (a client disconnect must not render a Torznab <error>)")
	}
	if rec.Contains("upstream query failed") {
		t.Errorf("caller cancellation warned as an upstream fault; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestReloadWarnsOnStatFailure pins the stat-error visibility leg of reload:
// an os.Stat failure other than fs.ErrNotExist (here ENOTDIR via a
// regular-file parent, the same root-safe injection
// TestReloadKeepsFeedOnUnreadableSnapshot uses for reads) must be warned
// about - a silent stat failure would invisibly freeze the served feed - while
// the current (empty) feed is kept.
func TestReloadWarnsOnStatFailure(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	log, rec := capture.New()
	ix := New(&Config{}, Deps{Logger: log}, filepath.Join(blocker, "feed.json"))
	if !rec.Contains("indexer feed snapshot stat failed") {
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
	h := New(&Config{APIKey: "k"}, Deps{}, "").handler()
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
	ix := New(&Config{APIKey: "k"}, Deps{Logger: log}, "")
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
	err = New(&Config{APIKey: "k"}, Deps{}, "").Run(context.Background())
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
// goroutine logs is race-safe; both waits are deadline-bounded.
func TestRunServesAndShutsDownGracefully(t *testing.T) {
	orig := listenAddr
	listenAddr = "127.0.0.1:0"
	defer func() { listenAddr = orig }()
	log, rec := capture.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- New(&Config{APIKey: "k"}, Deps{Logger: log}, "").Run(ctx) }()
	deadline := time.After(10 * time.Second)
	for !rec.Contains("seadex-scout indexer listening") {
		select {
		case err := <-done:
			t.Fatalf("Run exited before serving: %v", err)
		case <-deadline:
			t.Fatal("indexer never logged the listening line")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on graceful shutdown, want nil", err)
		}
	case <-deadline:
		t.Fatal("Run did not return after context cancellation")
	}
	if !rec.Contains("indexer shutdown complete") {
		t.Errorf("shutdown-complete line not logged; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}
