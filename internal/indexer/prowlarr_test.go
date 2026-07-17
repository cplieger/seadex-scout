package indexer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/httpx/v2"
)

// TestUpstreamSearchPreservesExistingQuery pins the URL-join logic of the
// Prowlarr proxy: a configured Torznab URL that already carries a query string
// gets the forwarded params appended with "&" (not a second "?"), so both the
// original and forwarded params survive; the Prowlarr key rides the X-Api-Key
// header, never the URL.
func TestUpstreamSearchPreservesExistingQuery(t *testing.T) {
	var (
		mu     sync.Mutex
		gotURL *url.URL
		gotKey string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		u := *r.URL
		gotURL = &u
		gotKey = r.Header.Get("X-Api-Key")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, `<rss><channel></channel></rss>`)
	}))
	defer srv.Close()

	u := &upstream{
		http: srv.Client(), log: slog.Default(), name: upstreamNyaa,
		feed: srv.URL + "/api?indexer=1", apiKey: "prowlarr-key",
	}
	items, err := u.search(context.Background(), url.Values{"t": {"search"}, "q": {"Frieren"}})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("got %d items from an empty channel, want 0", len(items))
	}

	mu.Lock()
	defer mu.Unlock()
	if gotURL == nil {
		t.Fatal("upstream never queried")
	}
	q := gotURL.Query()
	if q.Get("indexer") != "1" {
		t.Errorf("original query param lost: url = %q", gotURL)
	}
	if q.Get("t") != "search" || q.Get("q") != "Frieren" {
		t.Errorf("forwarded params missing: url = %q", gotURL)
	}
	if q.Get("apikey") != "" {
		t.Errorf("an apikey landed in the upstream URL: %q", gotURL)
	}
	if gotKey != "prowlarr-key" {
		t.Errorf("X-Api-Key = %q, want prowlarr-key", gotKey)
	}
}

// TestUpstreamSearchDropsForeignDownloadURLs pins the SSRF guard on the
// Prowlarr hop: an item survives search only when its download URL is an
// absolute http(s) URL, free of userinfo, on the configured Torznab endpoint's
// origin. A file URL, a userinfo trick, a sibling/internal host, and a
// link-less item are all dropped; the same-origin Prowlarr proxy link passes.
func TestUpstreamSearchDropsForeignDownloadURLs(t *testing.T) {
	const feedTmpl = `<rss xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel>
<item><title>ok</title><enclosure url="http://HOST/1/download?link=abc" length="1" type="application/x-bittorrent"/></item>
<item><title>file scheme</title><enclosure url="file:///etc/passwd" length="1" type="application/x-bittorrent"/></item>
<item><title>userinfo trick</title><enclosure url="http://HOST@evil.internal/steal" length="1" type="application/x-bittorrent"/></item>
<item><title>sibling host</title><enclosure url="http://sonarr:8989/api/internal" length="1" type="application/x-bittorrent"/></item>
<item><title>no link</title></item>
</channel></rss>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, strings.ReplaceAll(feedTmpl, "HOST", r.Host))
	}))
	defer srv.Close()

	u := &upstream{http: srv.Client(), log: slog.Default(), name: upstreamNyaa, feed: srv.URL + "/api"}
	items, err := u.search(context.Background(), url.Values{"t": {"search"}, "q": {"x"}})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(items) != 1 || items[0].Title != "ok" {
		t.Fatalf("items = %+v, want only the same-origin item", items)
	}
}

// TestFilterDownloadURLsFailsClosedOnUnparseableFeedURL pins the fail-closed
// arm of the SSRF guard: when the configured Torznab endpoint URL cannot be
// parsed, no origin can anchor the check, so every item is dropped rather than
// passed through unvalidated.
func TestFilterDownloadURLsFailsClosedOnUnparseableFeedURL(t *testing.T) {
	u := &upstream{log: slog.Default(), name: upstreamNyaa, feed: "http://prowlarr:9696/api%zz"}
	items := []item{{Title: "x", DownloadURL: "http://prowlarr:9696/1/download"}}
	if got := u.filterDownloadURLs(items); len(got) != 0 {
		t.Fatalf("unparseable feed URL passed %d items, want 0 (fail closed)", len(got))
	}
}

// TestSanitizeDisplayURL pins the display-URL gate on the passthrough
// InfoURL/GUID fields: only an absolute http(s) URL, free of userinfo, on the
// served upstream's own tracker host survives. Non-http schemes
// (javascript:/data:), relative forms, foreign hosts, userinfo tricks, and a
// cross-tracker host are all blanked; the tracker's exact host and a
// dot-delimited subdomain pass, and an unknown scope always blanks.
func TestSanitizeDisplayURL(t *testing.T) {
	tests := []struct {
		name, scope, raw, want string
	}{
		{"nyaa exact host kept", upstreamNyaa, "https://nyaa.si/view/1234567", "https://nyaa.si/view/1234567"},
		{"nyaa subdomain kept", upstreamNyaa, "https://sukebei.nyaa.si/view/7", "https://sukebei.nyaa.si/view/7"},
		{"ab exact host kept", upstreamAB, "https://animebytes.tv/torrent/1167293/group", "https://animebytes.tv/torrent/1167293/group"},
		{"javascript scheme blanked", upstreamNyaa, "javascript:alert(1)", ""},
		{"data scheme blanked", upstreamNyaa, "data:text/html,x", ""},
		{"relative path blanked", upstreamNyaa, "/view/1234567", ""},
		{"scheme-relative blanked", upstreamNyaa, "//nyaa.si/view/1234567", ""},
		{"foreign host blanked", upstreamNyaa, "https://evil.example/phish", ""},
		{"userinfo trick blanked", upstreamNyaa, "https://nyaa.si@evil.example/phish", ""},
		{"userinfo on canonical host blanked", upstreamNyaa, "https://trusted@nyaa.si/view/1", ""},
		{"cross-tracker host blanked under nyaa", upstreamNyaa, "https://animebytes.tv/torrent/1/group", ""},
		{"cross-tracker host blanked under ab", upstreamAB, "https://nyaa.si/view/1", ""},
		{"suffix-confusion host blanked", upstreamNyaa, "https://evilnyaa.si/view/1", ""},
		{"unknown scope blanks a canonical host", "other", "https://nyaa.si/view/1", ""},
		{"empty input blanked", upstreamNyaa, "", ""},
		{"unparseable blanked", upstreamNyaa, "http://[::1", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeDisplayURL(tc.scope, tc.raw); got != tc.want {
				t.Errorf("sanitizeDisplayURL(%q, %q) = %q, want %q", tc.scope, tc.raw, got, tc.want)
			}
		})
	}
}

// TestUpstreamSearchRetriesMalformedResponse pins the retry boundary around
// the WHOLE search attempt: a transient malformed 200 body (truncated/garbled
// Torznab XML) participates in the same bounded attempt budget as a failed
// request - the query is an idempotent GET - so one bad response followed by a
// healthy one succeeds instead of failing the search with two attempts unused.
func TestUpstreamSearchRetriesMalformedResponse(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		w.Header().Set("Content-Type", "application/rss+xml")
		if n == 1 {
			// A truncated response: 200 status, undecodable body.
			_, _ = io.WriteString(w, "<rss><channel><item><title>trunc")
			return
		}
		_, _ = io.WriteString(w, strings.ReplaceAll(sampleFeed, "http://prowlarr:9696", "http://"+r.Host))
	}))
	defer srv.Close()

	u := &upstream{http: srv.Client(), log: slog.Default(), name: upstreamNyaa, feed: srv.URL}
	items, err := u.search(context.Background(), url.Values{"t": {"search"}, "q": {"Frieren"}})
	if err != nil {
		t.Fatalf("search after one malformed response: %v (a parse failure must be retried)", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Errorf("upstream called %d times, want 2 (one malformed attempt + one retry)", calls)
	}
}


// TestFetchAndParseRateLimitCarriesRetryAfterHint pins the status path of the
// single-attempt fetch: a 429 response's Retry-After survives as a positive
// RetryAfterHint on the returned transient error (asserted directly, no
// sleeping), so the enclosing RetryWithBackoff honors the upstream-requested
// delay instead of its jittered backoff. The httpx sentinel chain is
// preserved for the caller's errors.Is classification.
func TestFetchAndParseRateLimitCarriesRetryAfterHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	u := &upstream{http: srv.Client(), log: slog.Default(), name: upstreamNyaa, feed: srv.URL}
	_, err := u.fetchAndParse(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("fetchAndParse on a 429 returned nil error")
	}
	if !errors.Is(err, httpx.ErrRateLimited) {
		t.Errorf("errors.Is(err, httpx.ErrRateLimited) = false (err = %v), want the sentinel preserved", err)
	}
	var transient httpx.Transient
	if !errors.As(err, &transient) || !transient.IsTransient() {
		t.Errorf("429 error is not transient (err = %v), want retryable", err)
	}
	var hint httpx.RetryAfterHint
	if !errors.As(err, &hint) {
		t.Fatalf("429 error carries no RetryAfterHint (err = %v)", err)
	}
	if got := hint.RetryAfterHint(); got != 7*time.Second {
		t.Errorf("RetryAfterHint() = %v, want 7s from the upstream Retry-After header", got)
	}
}
