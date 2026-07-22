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

	"github.com/cplieger/httpx/v3"
	"github.com/cplieger/slogx/capture"
)

// TestUpstreamSearchPreservesExistingQuery pins the URL-join logic of the
// Prowlarr proxy: a configured Torznab URL that already carries a query string
// gets the forwarded params merged into the query component (not appended
// after a trailing fragment, which net/http would strip before sending), so
// both the original and forwarded params survive even on an endpoint carrying
// a fragment; the Prowlarr key rides the X-Api-Key header, never the URL.
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
		feed: srv.URL + "/api?indexer=1#client-fragment", apiKey: "prowlarr-key",
	}
	items, _, err := u.search(context.Background(), url.Values{"t": {"search"}, "q": {"Frieren"}})
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
	items, _, err := u.search(context.Background(), url.Values{"t": {"search"}, "q": {"x"}})
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
	items, _, err := u.search(context.Background(), url.Values{"t": {"search"}, "q": {"Frieren"}})
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

// TestFetchAndParseClassifiesTorznabErrorDoc pins the failure classification
// at the parse boundary: a syntactically valid Torznab <error> document is an
// upstream-scoped answer, so it NEVER carries the show-local malformedBody
// marker (after the search fails, the harvest latches the failed scope), and
// its retryability splits on the numeric code - a deterministic auth/account
// (100-199) or request/parameter (200-299) error is terminal because retrying
// cannot recover bad credentials or a bad request, while a generic/
// server-side (900) or unparseable code stays transient within the bounded
// budget. A truncated/garbled RSS body remains show-local (marker set) and
// transient.
func TestFetchAndParseClassifiesTorznabErrorDoc(t *testing.T) {
	tests := map[string]struct {
		body          string
		wantTransient bool
		wantMalformed bool
		wantDocErr    bool
	}{
		"auth error code 100 is terminal": {
			body:       `<?xml version="1.0" encoding="UTF-8"?><error code="100" description="Incorrect user credentials"/>`,
			wantDocErr: true,
		},
		"parameter error code 201 is terminal": {
			body:       `<?xml version="1.0" encoding="UTF-8"?><error code="201" description="Incorrect parameter"/>`,
			wantDocErr: true,
		},
		"generic error code 900 stays transient": {
			body:          `<?xml version="1.0" encoding="UTF-8"?><error code="900" description="Unknown error"/>`,
			wantTransient: true,
			wantDocErr:    true,
		},
		"unparseable error code stays transient": {
			body:          `<?xml version="1.0" encoding="UTF-8"?><error code="oops" description="weird upstream"/>`,
			wantTransient: true,
			wantDocErr:    true,
		},
		"truncated RSS stays show-local": {
			body:          `<?xml version="1.0" encoding="UTF-8"?><rss version="2.0"><channel><item><title>trunc`,
			wantTransient: true,
			wantMalformed: true,
		},
		"decode-limit overflow stays show-local": {
			body:          `<?xml version="1.0" encoding="UTF-8"?><rss><channel>` + strings.Repeat("<item/>", maxUpstreamItems+1) + `</channel></rss>`,
			wantTransient: true,
			wantMalformed: true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/rss+xml")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			u := &upstream{http: srv.Client(), log: slog.Default(), name: upstreamNyaa, feed: srv.URL}
			_, err := u.fetchAndParse(context.Background(), srv.URL)
			if err == nil {
				t.Fatal("fetchAndParse on an undecodable feed returned nil error")
			}
			var transient httpx.Transient
			gotTransient := errors.As(err, &transient) && transient.IsTransient()
			if gotTransient != tc.wantTransient {
				t.Errorf("transient = %v, want %v (err = %v)", gotTransient, tc.wantTransient, err)
			}
			if got := malformedUpstreamBody(err); got != tc.wantMalformed {
				t.Errorf("malformedUpstreamBody(err) = %v, want %v (err = %v)", got, tc.wantMalformed, err)
			}
			if _, ok := errors.AsType[*upstreamDocError](err); ok != tc.wantDocErr {
				t.Errorf("upstreamDocError in chain = %v, want %v (err = %v)", ok, tc.wantDocErr, err)
			}
		})
	}
}

// TestUpstreamSearchTorznabErrorDocAttempts pins the retry traffic the error
// document classification governs: a deterministic auth failure (code 100)
// fails the search on the FIRST attempt - no retry backoff, no extra upstream
// load for credentials that stay wrong until a config change - while a
// generic upstream failure (code 900) stays inside the bounded retry budget,
// so a healthy response on the next attempt still succeeds the search.
func TestUpstreamSearchTorznabErrorDocAttempts(t *testing.T) {
	t.Run("auth error code 100 fails after one attempt", func(t *testing.T) {
		var (
			mu    sync.Mutex
			calls int
		)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			calls++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/rss+xml")
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><error code="100" description="Incorrect user credentials"/>`)
		}))
		defer srv.Close()

		u := &upstream{http: srv.Client(), log: slog.Default(), name: upstreamNyaa, feed: srv.URL}
		_, _, err := u.search(context.Background(), url.Values{"t": {"search"}, "q": {"Frieren"}})
		if err == nil {
			t.Fatal("search against a code-100 error document returned nil error")
		}
		if _, ok := errors.AsType[*upstreamDocError](err); !ok {
			t.Errorf("error = %T (%v), want *upstreamDocError in the chain", err, err)
		}
		mu.Lock()
		defer mu.Unlock()
		if calls != 1 {
			t.Errorf("upstream called %d times, want 1 (a credentials error is deterministic, not a transient to retry)", calls)
		}
	})

	t.Run("generic error code 900 is retried", func(t *testing.T) {
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
				_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><error code="900" description="Unknown error"/>`)
				return
			}
			_, _ = io.WriteString(w, strings.ReplaceAll(sampleFeed, "http://prowlarr:9696", "http://"+r.Host))
		}))
		defer srv.Close()

		u := &upstream{http: srv.Client(), log: slog.Default(), name: upstreamNyaa, feed: srv.URL}
		items, _, err := u.search(context.Background(), url.Values{"t": {"search"}, "q": {"Frieren"}})
		if err != nil {
			t.Fatalf("search after one code-900 error document: %v (a generic upstream error must be retried)", err)
		}
		if len(items) != 1 {
			t.Fatalf("got %d items, want 1", len(items))
		}
		mu.Lock()
		defer mu.Unlock()
		if calls != 2 {
			t.Errorf("upstream called %d times, want 2 (one code-900 attempt + one retry)", calls)
		}
	})
}

// TestUpstreamSearchRedactsAPIKeyInTorznabErrorDoc pins the credential
// redaction on the parse boundary: a syntactically valid Torznab <error>
// document's code/description are attacker-influenced text, and the request
// that produced them carried the Prowlarr API key - a compromised upstream
// can reflect that key back in the error description. Both the terminal
// (request/parameter) and retryable (generic) document paths must scrub the
// key before the error reaches httpx.Do's retry logger or any caller WARN,
// so the credential never expands into the log stream (CWE-532).
func TestUpstreamSearchRedactsAPIKeyInTorznabErrorDoc(t *testing.T) {
	const apiKey = "test-prowlarr-key"
	tests := map[string]struct {
		code    string
		padding string
	}{
		"terminal request code 201":  {code: "201"},
		"retryable generic code 900": {code: "900"},
		// The reflected key straddles sanitizeUpstreamText's 200-byte cap:
		// redaction must run on the untruncated text (Error() sanitizes at
		// the emit boundary), or the exact-substring replacement misses the
		// cap-truncated key and leaks its prefix (CWE-532).
		"key straddling the sanitize cap": {code: "900", padding: strings.Repeat("x", 190)},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/rss+xml")
				_, _ = io.WriteString(w,
					`<?xml version="1.0" encoding="UTF-8"?><error code="`+tc.code+`" description="`+tc.padding+apiKey+`"/>`)
			}))
			defer srv.Close()

			log, rec := capture.New()
			u := &upstream{http: srv.Client(), log: log, name: upstreamNyaa, feed: srv.URL, apiKey: apiKey}
			_, _, err := u.search(context.Background(), url.Values{"t": {"search"}, "q": {"x"}})
			if err == nil {
				t.Fatal("search against an error document returned nil error")
			}
			// A leaked PREFIX is as bad as the full key (the attacker picks
			// the truncation offset, so >=8 leaked chars are brute-forceable).
			if strings.Contains(err.Error(), apiKey[:8]) {
				t.Errorf("returned error leaks the API key (or a prefix): %v", err)
			}
			if !strings.Contains(err.Error(), "REDACTED") {
				t.Errorf("returned error = %v, want REDACTED in place of the API key", err)
			}
			for _, line := range renderedLogRecords(rec) {
				if strings.Contains(line, apiKey[:8]) {
					t.Errorf("log record leaks the API key (or a prefix): %q", line)
				}
			}
		})
	}
}

// renderedLogRecords flattens each captured slog record (message + top-level
// attrs) into one string, so a test can assert a secret never reached ANY
// part of a log line - the error text rides the "error" attr, which
// Recorder.Contains (messages only) would miss.
func renderedLogRecords(rec *capture.Recorder) []string {
	var out []string
	for _, r := range rec.Records() {
		var b strings.Builder
		b.WriteString(r.Message)
		r.Attrs(func(a slog.Attr) bool {
			b.WriteString(" ")
			b.WriteString(a.String())
			return true
		})
		out = append(out, b.String())
	}
	return out
}

// TestUpstreamSearchRedactsAndBoundsGenericDecodeError pins the emit-boundary
// policy on the GENERIC 2xx decode-failure path (the sibling of the <error>-
// document path above): encoding/xml returns the raw strconv error quoting
// the FULL unparsed <size> value, so a hostile 2xx body can pack
// attacker-controlled text - including a reflection of the Prowlarr API key
// the request carried - into the search error that httpx.Do's retry logger
// and fetchRaw's WARN expand into the log stream. The returned error must be
// redacted FIRST and then bounded (sanitizeUpstreamText's 200-byte cap plus
// the truncation marker, well under the 259-byte ceiling), must never contain
// the key, and must keep the malformedBody marker the harvest classifies on.
func TestUpstreamSearchRedactsAndBoundsGenericDecodeError(t *testing.T) {
	const apiKey = "test-prowlarr-key"
	garbage := "GARBAGE-" + apiKey + "-" + strings.Repeat("z", 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w,
			`<?xml version="1.0" encoding="UTF-8"?><rss xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel><item><title>x</title><size>`+garbage+`</size></item></channel></rss>`)
	}))
	defer srv.Close()

	log, rec := capture.New()
	u := &upstream{http: srv.Client(), log: log, name: upstreamNyaa, feed: srv.URL, apiKey: apiKey}
	_, _, err := u.search(context.Background(), url.Values{"t": {"search"}, "q": {"x"}})
	if err == nil {
		t.Fatal("search against a garbled <size> body returned nil error")
	}
	if !malformedUpstreamBody(err) {
		t.Errorf("error = %T (%v), want the malformedBody marker preserved", err, err)
	}
	if got := len(err.Error()); got > 259 {
		t.Errorf("error text is %d bytes, want <= 259 (redacted then bounded at the parse boundary)", got)
	}
	if strings.Contains(err.Error(), apiKey[:8]) {
		t.Errorf("returned error leaks the API key (or a prefix): %v", err)
	}
	for _, line := range renderedLogRecords(rec) {
		if strings.Contains(line, apiKey[:8]) {
			t.Errorf("log record leaks the API key (or a prefix): %q", line)
		}
	}
}

// TestFetchAndParseRateLimitCarriesRetryAfterHint pins the status path of the
// single-attempt fetch: a 429 response's Retry-After survives as a positive
// RetryAfterHint on the returned transient error (asserted directly, no
// sleeping), so the enclosing Do honors the upstream-requested
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

// TestUpstreamSearchRejectsOversizedResponse pins the bounded-read guard on the
// untrusted Torznab response: a 200 body past upstreamMaxBytes fails the search
// with httpx's *ResponseTooLargeError naming the cap, and the deterministic
// failure is terminal - it must not burn the remaining retry attempts.
func TestUpstreamSearchRejectsOversizedResponse(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
	)
	chunk := make([]byte, 1<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/rss+xml")
		for range 17 { // 17 MiB > the 16 MiB upstreamMaxBytes cap
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	u := &upstream{http: srv.Client(), log: slog.Default(), name: upstreamNyaa, feed: srv.URL}
	_, _, err := u.search(context.Background(), url.Values{"t": {"search"}, "q": {"x"}})
	if err == nil {
		t.Fatal("search with an oversized upstream body returned nil, want *httpx.ResponseTooLargeError")
	}
	var tooLarge *httpx.ResponseTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("error = %v, want *httpx.ResponseTooLargeError", err)
	}
	if tooLarge.Limit != upstreamMaxBytes {
		t.Errorf("ResponseTooLargeError.Limit = %d, want %d", tooLarge.Limit, int64(upstreamMaxBytes))
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("upstream called %d times, want 1 (an oversized body is deterministic, not a transient to retry)", calls)
	}
}

// TestSearchRejectsUnparseableUpstreamURLs pins the two defensive parse
// guards of the Prowlarr proxy: a configured Torznab feed URL that does not
// parse fails the search with the invalid-feed-URL error BEFORE any HTTP call
// (no request can be built against it), and fetchAndParse surfaces a
// request-build failure for a URL http.NewRequestWithContext cannot accept.
func TestSearchRejectsUnparseableUpstreamURLs(t *testing.T) {
	t.Run("unparseable configured feed URL", func(t *testing.T) {
		u := &upstream{log: slog.Default(), name: upstreamNyaa, feed: "http://prowlarr:9696/api%zz"}
		_, _, err := u.search(context.Background(), url.Values{"t": {"search"}, "q": {"x"}})
		if err == nil || !strings.Contains(err.Error(), "invalid upstream feed URL") {
			t.Errorf("search error = %v, want the invalid-feed-URL error before any HTTP call", err)
		}
	})
	t.Run("unbuildable request URL", func(t *testing.T) {
		u := &upstream{http: &http.Client{}, log: slog.Default(), name: upstreamNyaa}
		if _, err := u.fetchAndParse(context.Background(), ":"); err == nil {
			t.Error("fetchAndParse(\":\") = nil error, want a request-build failure")
		}
	})
}
