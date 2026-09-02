package indexer

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/httpx/v5"
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
		gotURL = r.URL.Clone()
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
	items, _, err := u.search(t.Context(), url.Values{"t": {"search"}, "q": {"Frieren"}})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("got %d items from an empty channel, want 0", len(items))
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
	items, _, err := u.search(t.Context(), url.Values{"t": {"search"}, "q": {"x"}})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(items) != 1 || items[0].Title != "ok" {
		t.Fatalf("items = %+v, want only the same-origin item", items)
	}
}

// mustFeedURL parses an upstream's configured Torznab endpoint the way search
// does, so a test calling filterDownloadURLs directly anchors the origin check
// on the same *url.URL production hands it.
func mustFeedURL(t *testing.T, u *upstream) *url.URL {
	t.Helper()
	parsed, err := url.Parse(u.feed)
	if err != nil {
		t.Fatalf("parse upstream feed %q: %v", u.feed, err)
	}
	return parsed
}

// TestSanitizeDisplayURL pins the display-URL gate on the passthrough
// InfoURL/GUID fields: only an absolute http(s) URL, free of userinfo, on the
// served upstream's own tracker host survives, and what survives is the gate's
// VOUCHED spelling rather than the original. Non-http schemes
// (javascript:/data:), relative forms, foreign hosts, userinfo tricks, and a
// cross-tracker host are all blanked; the tracker's exact host and a
// dot-delimited subdomain pass, and an unknown scope always blanks.
func TestSanitizeDisplayURL(t *testing.T) {
	tests := []struct {
		name, scope, raw, want string
		wantOK                 bool
	}{
		{name: "nyaa exact host kept", scope: upstreamNyaa, raw: "https://nyaa.si/view/1234567", want: "https://nyaa.si/view/1234567", wantOK: true},
		{name: "nyaa subdomain kept", scope: upstreamNyaa, raw: "https://sukebei.nyaa.si/view/7", want: "https://sukebei.nyaa.si/view/7", wantOK: true},
		{name: "ab exact host kept", scope: upstreamAB, raw: "https://animebytes.tv/torrent/1167293/group", want: "https://animebytes.tv/torrent/1167293/group", wantOK: true},
		// Edge padding is stripped by the WHATWG preprocessing the gate reads,
		// so the padded original must not reach the arr UI: url.Parse refuses
		// a space inside the host, which is what the weekly fuzz found.
		{name: "edge-padded host kept in its vouched spelling", scope: upstreamNyaa, raw: "http://nYAA.si  ", want: "http://nYAA.si", wantOK: true},
		{name: "javascript scheme blanked", scope: upstreamNyaa, raw: "javascript:alert(1)"},
		{name: "data scheme blanked", scope: upstreamNyaa, raw: "data:text/html,x"},
		{name: "relative path blanked", scope: upstreamNyaa, raw: "/view/1234567"},
		{name: "scheme-relative blanked", scope: upstreamNyaa, raw: "//nyaa.si/view/1234567"},
		{name: "foreign host blanked", scope: upstreamNyaa, raw: "https://evil.example/phish"},
		{name: "userinfo trick blanked", scope: upstreamNyaa, raw: "https://nyaa.si@evil.example/phish"},
		{name: "userinfo on canonical host blanked", scope: upstreamNyaa, raw: "https://trusted@nyaa.si/view/1"},
		{name: "cross-tracker host blanked under nyaa", scope: upstreamNyaa, raw: "https://animebytes.tv/torrent/1/group"},
		{name: "cross-tracker host blanked under ab", scope: upstreamAB, raw: "https://nyaa.si/view/1"},
		{name: "suffix-confusion host blanked", scope: upstreamNyaa, raw: "https://evilnyaa.si/view/1"},
		{name: "non-http scheme on the canonical host blanked", scope: upstreamNyaa, raw: "ftp://nyaa.si/view/1"},
		{name: "backslash-smuggled authority blanked", scope: upstreamNyaa, raw: "https://nyaa.si\\@evil.example/x"},
		{name: "tab-smuggled host blanked", scope: upstreamNyaa, raw: "https://nya\ta.si/view/1"},
		{name: "unknown scope blanks a canonical host", scope: "other", raw: "https://nyaa.si/view/1"},
		{name: "empty input blanked", scope: upstreamNyaa, raw: ""},
		{name: "unparseable blanked", scope: upstreamNyaa, raw: "http://[::1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := sanitizeDisplayURL(tc.scope, tc.raw)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("sanitizeDisplayURL(%q, %q) = (%q, %v), want (%q, %v)",
					tc.scope, tc.raw, got, ok, tc.want, tc.wantOK)
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
	items, _, err := u.search(t.Context(), url.Values{"t": {"search"}, "q": {"Frieren"}})
	if err != nil {
		t.Fatalf("search after one malformed response: %v (a parse failure must be retried)", err)
	}
	if len(items) != 1 {
		t.Errorf("got %d items, want 1", len(items))
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
			_, err := u.fetchAndParse(t.Context(), srv.URL)
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
		_, _, err := u.search(t.Context(), url.Values{"t": {"search"}, "q": {"Frieren"}})
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
		items, _, err := u.search(t.Context(), url.Values{"t": {"search"}, "q": {"Frieren"}})
		if err != nil {
			t.Fatalf("search after one code-900 error document: %v (a generic upstream error must be retried)", err)
		}
		if len(items) != 1 {
			t.Errorf("got %d items, want 1", len(items))
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
//
// "The key" is every credential the request TRANSMITS, not just the header
// one: config accepts (with a WARN) a credential embedded in the configured
// feed URL, and net/http turns userinfo into an Authorization: Basic header
// while a query value rides the request URL. In that configuration the header
// key is typically EMPTY, so a header-key-only redaction was inert on exactly
// the shape config permits - hence the two feed-URL cases below.
func TestUpstreamSearchRedactsAPIKeyInTorznabErrorDoc(t *testing.T) {
	const apiKey = "test-prowlarr-key"
	const (
		userinfoToken = "secret-userinfo-token"
		queryToken    = "secret-apikey-value-32-chars-long"
		// Shorter than minEmbeddedSecretLen: only its parameter NAME marks it
		// a credential.
		shortQueryToken = "secret-value"
		semicolonToken  = "secret-semicolon-value"
	)
	tests := map[string]struct {
		code    string
		padding string
		// embedded configures the credential in the feed URL (userinfo plus an
		// ?apikey= value) and leaves the header key empty; secret is the value
		// the upstream reflects back in its <error description>. rawQuery
		// overrides the default single ?apikey= pair.
		embedded bool
		rawQuery string
		secret   string
	}{
		"terminal request code 201":  {code: "201", secret: apiKey},
		"retryable generic code 900": {code: "900", secret: apiKey},
		// The reflected key straddles sanitizeUpstreamText's 200-byte cap:
		// redaction must run on the untruncated text (Error() sanitizes at
		// the emit boundary), or the exact-substring replacement misses the
		// cap-truncated key and leaks its prefix (CWE-532).
		"key straddling the sanitize cap": {code: "900", padding: strings.Repeat("x", 190), secret: apiKey},
		"feed-URL userinfo reflected":     {code: "900", embedded: true, secret: userinfoToken},
		"feed-URL apikey value reflected": {code: "900", embedded: true, secret: queryToken},
		// A credential-NAMED parameter is a secret at any length: config
		// accepts (and this package's own fixtures use) a short ?apikey=
		// value, which a value-length floor would leave unredacted.
		"feed-URL short apikey value reflected": {
			code: "900", embedded: true, rawQuery: "apikey=" + shortQueryToken, secret: shortQueryToken,
		},
		// net/url discards a semicolon-delimited pair wholesale, but the raw
		// query - credential and all - still rides the outgoing request, so
		// the collection must read the raw string.
		"feed-URL semicolon-delimited credential reflected": {
			code: "900", embedded: true, rawQuery: "apikey=" + semicolonToken + ";indexer=1", secret: semicolonToken,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/rss+xml")
				_, _ = io.WriteString(w,
					`<?xml version="1.0" encoding="UTF-8"?><error code="`+tc.code+`" description="`+tc.padding+tc.secret+`"/>`)
			}))
			defer srv.Close()

			feed, key := srv.URL, apiKey
			if tc.embedded {
				parsed, err := url.Parse(srv.URL)
				if err != nil {
					t.Fatalf("parse test server URL: %v", err)
				}
				parsed.User = url.User(userinfoToken)
				parsed.RawQuery = "apikey=" + queryToken
				if tc.rawQuery != "" {
					parsed.RawQuery = tc.rawQuery
				}
				feed, key = parsed.String(), ""
			}

			log, rec := capture.New()
			u := &upstream{http: srv.Client(), log: log, name: upstreamNyaa, feed: feed, apiKey: key}
			_, _, err := u.search(t.Context(), url.Values{"t": {"search"}, "q": {"x"}})
			if err == nil {
				t.Fatal("search against an error document returned nil error")
			}
			// A leaked PREFIX is as bad as the full key (the attacker picks
			// the truncation offset, so >=8 leaked chars are brute-forceable).
			if strings.Contains(err.Error(), tc.secret[:8]) {
				t.Errorf("returned error leaks the transmitted credential (or a prefix): %v", err)
			}
			if !strings.Contains(err.Error(), "REDACTED") {
				t.Errorf("returned error = %v, want REDACTED in place of the credential", err)
			}
			if rec.Contains(tc.secret[:8]) || rec.AttrContains("", "", tc.secret[:8]) {
				t.Errorf("log records leak the transmitted credential (or a prefix): %v", rec.Records())
			}
		})
	}
}

// TestUpstreamSearchRedactsReflectedBasicAuthorization pins the WIRE
// representation of a feed-URL userinfo credential (h-f7): net/http never
// transmits the plaintext username/password - it sends
// "Authorization: Basic base64(user:pass)" - so an upstream reflecting that
// header, or the bare token, back inside its <error> document would escape an
// exact-substring scrub that only knew the plaintext components. Both the
// username-only shape config permits and a full username:password pair must be
// unreadable in the returned error and in every log record (CWE-532).
func TestUpstreamSearchRedactsReflectedBasicAuthorization(t *testing.T) {
	tests := map[string]*url.Userinfo{
		"username only":         url.User("secret-userinfo-token"),
		"username and password": url.UserPassword("secret-user", "secret-password"),
	}
	for name, userinfo := range tests {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				auth := r.Header.Get("Authorization")
				w.Header().Set("Content-Type", "application/rss+xml")
				_, _ = io.WriteString(w,
					`<?xml version="1.0" encoding="UTF-8"?><error code="900" description="`+auth+` / `+strings.TrimPrefix(auth, "Basic ")+`"/>`)
			}))
			defer srv.Close()

			parsed, err := url.Parse(srv.URL)
			if err != nil {
				t.Fatalf("parse test server URL: %v", err)
			}
			parsed.User = userinfo
			password, _ := userinfo.Password()
			token := base64.StdEncoding.EncodeToString([]byte(userinfo.Username() + ":" + password))

			log, rec := capture.New()
			u := &upstream{http: srv.Client(), log: log, name: upstreamNyaa, feed: parsed.String()}
			if _, _, err := u.search(t.Context(), url.Values{"t": {"search"}, "q": {"x"}}); err == nil {
				t.Fatal("search against an error document returned nil error")
			} else if strings.Contains(err.Error(), token[:8]) {
				t.Errorf("returned error leaks the transmitted Basic token (or a prefix): %v", err)
			}
			if rec.Contains(token[:8]) || rec.AttrContains("", "", token[:8]) {
				t.Errorf("log records leak the transmitted Basic token (or a prefix): %v", rec.Records())
			}
		})
	}
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
//
// The second case covers the feed-URL credential shape config accepts with a
// WARN (userinfo plus an ?apikey= value, header key empty): it is transmitted
// on every request just as the header key is, so it must be scrubbed here too
// - a header-key-only redaction is a documented no-op on an empty secret and
// was therefore inert in exactly that configuration.
func TestUpstreamSearchRedactsAndBoundsGenericDecodeError(t *testing.T) {
	const apiKey = "test-prowlarr-key"
	const (
		userinfoToken = "secret-userinfo-token"
		queryToken    = "secret-apikey-value-32-chars-long"
	)
	tests := map[string]struct {
		embedded bool
		secret   string
	}{
		"header api key":               {secret: apiKey},
		"feed-URL embedded credential": {embedded: true, secret: queryToken},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			garbage := "GARBAGE-" + tc.secret + "-" + strings.Repeat("z", 4096)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/rss+xml")
				_, _ = io.WriteString(w,
					`<?xml version="1.0" encoding="UTF-8"?><rss xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel><item><title>x</title><size>`+garbage+`</size></item></channel></rss>`)
			}))
			defer srv.Close()

			feed, key := srv.URL, apiKey
			if tc.embedded {
				parsed, err := url.Parse(srv.URL)
				if err != nil {
					t.Fatalf("parse test server URL: %v", err)
				}
				parsed.User = url.User(userinfoToken)
				parsed.RawQuery = "apikey=" + queryToken
				feed, key = parsed.String(), ""
			}

			log, rec := capture.New()
			u := &upstream{http: srv.Client(), log: log, name: upstreamNyaa, feed: feed, apiKey: key}
			_, _, err := u.search(t.Context(), url.Values{"t": {"search"}, "q": {"x"}})
			if err == nil {
				t.Fatal("search against a garbled <size> body returned nil error")
			}
			if !malformedUpstreamBody(err) {
				t.Errorf("error = %T (%v), want the malformedBody marker preserved", err, err)
			}
			if got := len(err.Error()); got > 259 {
				t.Errorf("error text is %d bytes, want <= 259 (redacted then bounded at the parse boundary)", got)
			}
			if strings.Contains(err.Error(), tc.secret[:8]) {
				t.Errorf("returned error leaks the transmitted credential (or a prefix): %v", err)
			}
			if rec.Contains(tc.secret[:8]) || rec.AttrContains("", "", tc.secret[:8]) {
				t.Errorf("log records leak the transmitted credential (or a prefix): %v", rec.Records())
			}
		})
	}
}

// TestUpstreamSearchRedactsAcrossTheSanitizer pins the ORDER of the emit-boundary
// composition, which the two redaction sites in this file share (redactAndBound):
// redaction runs on both sides of the sanitizer, not only before it.
//
// The defect it guards is that runesafe's sanitizer is a normalizing transform, and
// the normal form it produces is a SPACE: every unsafe rune it rewrites becomes
// U+0020. Four of this upstream's needles carry a U+0020 - a userinfo username or
// password configured as user%20name (url.Userinfo hands back the percent-DECODED
// form) and a query credential whose literal '+' decodes to a space (url.QueryUnescape
// maps '+' to ' ', which base64 passkeys hit routinely). So an upstream that splits
// the credential with an unsafe rune defeats the byte-exact needle, and a redaction
// that ran only BEFORE the sanitizer leaves the sanitizer to REASSEMBLE the
// credential into the error text and the log stream (CWE-532). Every row below fails
// when the composition is reduced to redact-then-sanitize.
//
// Delivery is the <error>-document path, verified reachable end to end: an attribute
// value carries a raw DEL or LF, and &#13; carries a real CR, all the way into
// upstreamDocError's fields. Two shapes deliberately are NOT used because the real
// boundary cannot deliver them, so a row built on either would pass under both
// orders and prove nothing - encoding/xml rejects a C0 outright ("illegal character
// code U+0001", raw or as a character reference, in text and in attributes), and the
// generic <size> decode path reaches the error text through strconv.Quote, which
// escapes a control rune into a printable \x7f sequence the sanitizer never sees.
// The existing fixtures miss all of this because their credentials are space-free.
func TestUpstreamSearchRedactsAcrossTheSanitizer(t *testing.T) {
	tests := map[string]struct {
		// username and password are the RAW userinfo credential (encoded into the feed
		// URL by url.URL.String, decoded back by url.Userinfo); rawQuery is the feed
		// URL's raw query. code and description are the <error> document's attribute
		// values as they ride the wire, and secret is the credential that must not be
		// readable in any sink.
		username    string
		password    string
		rawQuery    string
		code        string
		description string
		secret      string
	}{
		// A DEL is the cheapest unsafe rune the XML boundary carries verbatim.
		"userinfo password reassembled from DEL": {
			username:    "alice",
			password:    "correct horse battery",
			code:        "100",
			description: "auth rejected: correct\x7fhorse\x7fbattery",
			secret:      "correct horse battery",
		},
		"userinfo username reassembled from DEL": {
			username:    "svc account",
			password:    "s3cret",
			code:        "100",
			description: "auth failed for user svc\x7faccount",
			secret:      "svc account",
		},
		// CR and LF are unsafe only under the single-line policy (keepCRLF=false),
		// which is the policy this site's sanitizer applies - so these two rows pin
		// that the composition reads the strict preset and not runesafe.Sanitize.
		"userinfo password reassembled from CR": {
			username:    "alice",
			password:    "correct horse battery",
			code:        "100",
			description: "auth rejected: correct&#13;horse&#13;battery",
			secret:      "correct horse battery",
		},
		// The needle here is the DECODED query value: QueryUnescape maps the '+' a
		// base64 credential routinely carries onto a space.
		"decoded query value reassembled from DEL": {
			username:    "alice",
			password:    "s3cret",
			rawQuery:    "passkey=Ab9+cd/efGH12345678==", // gitleaks:allow (synthetic fixture; the '+' is the needle)
			code:        "100",
			description: "decode: unexpected token in &quot;Ab9\x7fcd/efGH12345678==&quot;",
			secret:      "Ab9 cd/efGH12345678==",
		},
		// The code attribute is redacted at the same site as the description, so it
		// needs its own row; a non-numeric code is also the retryable arm, which
		// re-runs the composition on every attempt.
		"docErr code reassembled from LF": {
			username:    "alice",
			password:    "correct horse battery",
			code:        "correct\nhorse\nbattery",
			description: "Incorrect user credentials",
			secret:      "correct horse battery",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/rss+xml")
				_, _ = io.WriteString(w,
					`<?xml version="1.0" encoding="UTF-8"?><error code="`+tc.code+`" description="`+tc.description+`"/>`)
			}))
			defer srv.Close()

			parsed, err := url.Parse(srv.URL)
			if err != nil {
				t.Fatalf("parse test server URL: %v", err)
			}
			parsed.User = url.UserPassword(tc.username, tc.password)
			parsed.RawQuery = tc.rawQuery

			log, rec := capture.New()
			u := &upstream{http: srv.Client(), log: log, name: upstreamNyaa, feed: parsed.String()}
			_, _, err = u.search(t.Context(), url.Values{"t": {"search"}, "q": {"x"}})
			if err == nil {
				t.Fatal("search against an error document returned nil error")
			}
			if strings.Contains(err.Error(), tc.secret) {
				t.Errorf("returned error leaks the credential the sanitizer reassembled: %v", err)
			}
			if !strings.Contains(err.Error(), "REDACTED") {
				t.Errorf("returned error = %v, want REDACTED in place of the credential", err)
			}
			if rec.Contains(tc.secret) || rec.AttrContains("", "", tc.secret) {
				t.Errorf("log records leak the credential the sanitizer reassembled: %v", rec.Records())
			}
		})
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
	_, err := u.fetchAndParse(t.Context(), srv.URL)
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

// TestUpstreamSearchRetriesRetryableStatuses pins the retry taxonomy the app
// owns now that httpx.GetBytes performs the single fetch under
// WithMaxAttempts(1). GetBytes deliberately does NOT mark its exhaustion error
// Transient - after a one-attempt budget the retry decision belongs to the
// caller - so without attemptError re-classifying the *StatusError, every
// self-healing upstream status would fail the search on its first attempt:
// the documented three-attempt budget silently collapses to one, an
// interactive search fails immediately, and the title harvest latches the
// tracker scope for a whole rebuild. A 408/429/5xx must therefore be retried
// and recover on a healthy next attempt; any other non-2xx must still fail
// fast, spending exactly one attempt.
func TestUpstreamSearchRetriesRetryableStatuses(t *testing.T) {
	newUpstream := func(t *testing.T, first int) (*upstream, func() int) {
		t.Helper()
		var (
			mu    sync.Mutex
			calls int
		)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n == 1 {
				// A short Retry-After keeps the retried subtests fast; the
				// hint path itself is pinned by
				// TestFetchAndParseRateLimitCarriesRetryAfterHint.
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(first)
				return
			}
			w.Header().Set("Content-Type", "application/rss+xml")
			_, _ = io.WriteString(w, strings.ReplaceAll(sampleFeed, "http://prowlarr:9696", "http://"+r.Host))
		}))
		t.Cleanup(srv.Close)
		return &upstream{http: srv.Client(), log: slog.Default(), name: upstreamNyaa, feed: srv.URL},
			func() int {
				mu.Lock()
				defer mu.Unlock()
				return calls
			}
	}

	// 408 is the server reporting that IT gave up waiting, 429 a rate limit,
	// 500/503 a server fault: all self-heal without operator action.
	for _, status := range []int{
		http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
	} {
		t.Run("retried/"+http.StatusText(status), func(t *testing.T) {
			u, calls := newUpstream(t, status)
			items, _, err := u.search(t.Context(), url.Values{"t": {"search"}, "q": {"Frieren"}})
			if err != nil {
				t.Fatalf("search after one HTTP %d: %v (a self-healing status must be retried)", status, err)
			}
			if len(items) != 1 {
				t.Errorf("got %d items, want 1 from the healthy retry", len(items))
			}
			if got := calls(); got != 2 {
				t.Errorf("upstream called %d times, want 2 (one %d + one retry)", got, status)
			}
		})
	}

	// A 401/403 is a wrong Prowlarr API key and a 404 a wrong Torznab path:
	// both stay wrong on every attempt until the operator fixes config.
	for _, status := range []int{
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
	} {
		t.Run("terminal/"+http.StatusText(status), func(t *testing.T) {
			u, calls := newUpstream(t, status)
			_, _, err := u.search(t.Context(), url.Values{"t": {"search"}, "q": {"Frieren"}})
			if err == nil {
				t.Fatalf("search against an HTTP %d returned nil error", status)
			}
			statusErr, ok := errors.AsType[*httpx.StatusError](err)
			if !ok {
				t.Fatalf("error = %T (%v), want *httpx.StatusError", err, err)
			}
			if statusErr.Code != status {
				t.Errorf("StatusError.Code = %d, want %d", statusErr.Code, status)
			}
			var transient httpx.Transient
			if errors.As(err, &transient) && transient.IsTransient() {
				t.Errorf("HTTP %d classified transient; a config fault must not be retried", status)
			}
			if got := calls(); got != 1 {
				t.Errorf("upstream called %d times, want 1 (a config fault is deterministic)", got)
			}
		})
	}
}

// TestUpstreamSearchRedactsUserinfoAcrossRetryLogging is the retry-path half of
// TestUpstreamSearchStatusErrorOmitsUserinfoAndQuery: a retryable status is
// logged once per attempt by BOTH httpx.GetBytes (which sees the raw request
// URL) and the enclosing httpx.Do, so an endpoint carrying a username-only
// userinfo token and an apikey query value gets three chances per search to
// leak them into the log stream (CWE-532). The app passes the unscrubbed URL to
// GetBytes deliberately - only the actual request needs the credentials - which
// makes httpx's redaction the sole guard, and this test the acceptance test for
// it across the whole retry tree.
func TestUpstreamSearchRedactsUserinfoAcrossRetryLogging(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	feed := strings.Replace(srv.URL, "http://", "http://secret-token@", 1) + "/1/api?apikey=secret-value"
	log, rec := capture.New()
	u := &upstream{http: srv.Client(), log: log, name: upstreamNyaa, feed: feed}
	_, _, err := u.search(t.Context(), url.Values{"t": {"search"}, "q": {"Frieren"}})
	if err == nil {
		t.Fatal("search against a permanently 503 endpoint returned nil error")
	}
	if rec.Len() == 0 {
		t.Fatal("no log records captured; the retry tree must have logged the exhaustion")
	}
	for _, secret := range []string{"secret-token", "secret-value"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("returned error leaks %q: %v", secret, err)
		}
		if rec.Contains(secret) || rec.AttrContains("", "", secret) {
			t.Errorf("log records leak %q: %v", secret, rec.Records())
		}
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
		for range 9 { // 9 MiB > the new 8 MiB cap but < the former 16 MiB cap
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	u := &upstream{http: srv.Client(), log: slog.Default(), name: upstreamNyaa, feed: srv.URL}
	_, _, err := u.search(t.Context(), url.Values{"t": {"search"}, "q": {"x"}})
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
	// GetBytes checks status BEFORE reading the body, so an over-cap read is by
	// construction the read failure of a SUCCESSFUL (2xx) response: the harvest
	// must scope it to this one show's result set (malformedUpstreamBody) rather
	// than latching the whole tracker scope as failed and skipping every
	// remaining show on it.
	if !malformedUpstreamBody(err) {
		t.Errorf("malformedUpstreamBody(%T) = false, want true: an over-cap 2xx body is show-scoped evidence, not upstream-down evidence", err)
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
		_, _, err := u.search(t.Context(), url.Values{"t": {"search"}, "q": {"x"}})
		if err == nil || !strings.Contains(err.Error(), "invalid upstream feed URL") {
			t.Errorf("search error = %v, want the invalid-feed-URL error before any HTTP call", err)
		}
	})
	t.Run("unbuildable request URL", func(t *testing.T) {
		u := &upstream{http: &http.Client{}, log: slog.Default(), name: upstreamNyaa}
		if _, err := u.fetchAndParse(t.Context(), ":"); err == nil {
			t.Error("fetchAndParse(\":\") = nil error, want a request-build failure")
		}
	})
}

// TestUpstreamSearchStatusErrorOmitsUserinfoAndQuery pins the status-error
// sanitization of the Prowlarr proxy: a configured Torznab endpoint may carry
// a username-only userinfo token (which validateHTTPURL accepts) and an
// apikey query value, and both must be absent from the error the search
// returns and from every line the retry logger emits (CWE-532). The scrub is
// httpx's since v4 - redactURL drops the whole userinfo component and REDACTs
// every query value, so *StatusError renders safely on its own and the app
// carries no pre-scrubbed URL clone. This is the cross-library acceptance test
// for that guarantee: it fails if httpx ever regresses to url.Redacted()
// semantics, which mask only the password and preserve the username verbatim.
func TestUpstreamSearchStatusErrorOmitsUserinfoAndQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	feed := strings.Replace(srv.URL, "http://", "http://secret-token@", 1) + "/1/api?apikey=secret-value"
	log, rec := capture.New()
	u := &upstream{http: srv.Client(), log: log, name: upstreamNyaa, feed: feed}
	_, _, err := u.search(t.Context(), url.Values{"t": {"search"}, "q": {"x"}})
	if err == nil {
		t.Fatal("search against a 404 endpoint returned nil error")
	}
	for _, secret := range []string{"secret-token", "secret-value"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("returned status error leaks %q: %v", secret, err)
		}
		if rec.Contains(secret) || rec.AttrContains("", "", secret) {
			t.Errorf("log records leak %q: %v", secret, rec.Records())
		}
	}
	var statusErr *httpx.StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("error = %T (%v), want *httpx.StatusError", err, err)
	}
	if statusErr.Code != http.StatusNotFound {
		t.Errorf("StatusError.Code = %d, want 404", statusErr.Code)
	}
}

// TestSameHTTPOrigin pins the accept side of the SSRF origin gate directly,
// including the https leg no other test reaches: every existing consumer test
// runs against an http httptest server, so a mutant that rejects the https
// scheme survives the whole suite while breaking every TLS-terminated
// Prowlarr deployment.
func TestSameHTTPOrigin(t *testing.T) {
	mustParse := func(s string) *url.URL {
		u, err := url.Parse(s)
		if err != nil {
			t.Fatalf("parse origin %q: %v", s, err)
		}
		return u
	}
	tests := []struct {
		name   string
		raw    string
		origin string
		want   bool
	}{
		{"http URL on http origin accepted", "http://prowlarr:9696/1/download?link=abc", "http://prowlarr:9696/1/api", true},
		{"https URL on https origin accepted", "https://prowlarr:9696/1/download?link=abc", "https://prowlarr:9696/1/api", true},
		{"uppercase scheme and host fold to the origin", "HTTPS://PROWLARR:9696/1/download", "https://prowlarr:9696/1/api", true},
		{"explicit https default port matches an omitted one", "https://prowlarr:443/1/download", "https://prowlarr/1/api", true},
		{"omitted https default port matches an explicit one", "https://prowlarr/1/download", "https://prowlarr:443/1/api", true},
		{"explicit http default port matches an omitted one", "http://prowlarr:80/1/download", "http://prowlarr/1/api", true},
		{"cross-scheme default ports still rejected", "http://prowlarr:80/1/download", "https://prowlarr:443/1/api", false},
		{"non-default port against an omitted default rejected", "https://prowlarr:9696/1/download", "https://prowlarr/1/api", false},
		{"http URL on https origin rejected", "http://prowlarr:9696/1/download", "https://prowlarr:9696/1/api", false},
		{"https URL on http origin rejected", "https://prowlarr:9696/1/download", "http://prowlarr:9696/1/api", false},
		{"port mismatch rejected", "https://prowlarr:9697/1/download", "https://prowlarr:9696/1/api", false},
		{"host mismatch rejected", "https://sonarr:9696/1/download", "https://prowlarr:9696/1/api", false},
		{"userinfo rejected even on the origin host", "https://user@prowlarr:9696/1/download", "https://prowlarr:9696/1/api", false},
		{"non-http scheme rejected", "ftp://prowlarr:9696/1/download", "https://prowlarr:9696/1/api", false},
		// The fold is ASCII-only, so a host that differs from the origin by a
		// non-ASCII rune is a DIFFERENT origin however the toolchain's Unicode
		// simple-fold table happens to relate the two. All three pairs below
		// are the ones Unicode 17 (Go 1.27) newly folds together, and
		// strings.EqualFold answers "same origin" for every one of them - which
		// is exactly why this gate must not use it. U+017F is the long-s case
		// the app's other host gates were hardened against.
		{"U+0390 host is not the U+1FD3 origin", "https://pro\u0390larr:9696/1/download", "https://pro\u1FD3larr:9696/1/api", false},
		{"U+03B0 host is not the U+1FE3 origin", "https://pro\u03B0larr:9696/1/download", "https://pro\u1FE3larr:9696/1/api", false},
		{"U+FB05 host is not the U+FB06 origin", "https://prowlarr\uFB05:9696/1/download", "https://prowlarr\uFB06:9696/1/api", false},
		{"long-s host is not the ASCII origin", "https://\u017Fonarr:9696/1/download", "https://sonarr:9696/1/api", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameHTTPOrigin(tc.raw, mustParse(tc.origin)); got != tc.want {
				t.Errorf("sameHTTPOrigin(%q, %q) = %v, want %v", tc.raw, tc.origin, got, tc.want)
			}
		})
	}
}

// TestFilterDownloadURLsWarnsOnDroppedItems pins the operational contract of
// the SSRF origin filter's warning: the "upstream items dropped" WARN fires
// exactly once per search that dropped items, carrying the true dropped/kept
// counts, and never fires for a clean all-same-origin response. This is the
// only observable that distinguishes a healthy passthrough from a filter
// silently eating items, and no existing test asserts it.
func TestFilterDownloadURLsWarnsOnDroppedItems(t *testing.T) {
	const feedTmpl = `<rss xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel>
<item><title>ok</title><enclosure url="http://HOST/1/download?link=abc" length="1" type="application/x-bittorrent"/></item>
<item><title>foreign</title><enclosure url="http://evil.internal/steal" length="1" type="application/x-bittorrent"/></item>
<item><title>no link</title></item>
</channel></rss>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, strings.ReplaceAll(feedTmpl, "HOST", r.Host))
	}))
	defer srv.Close()

	log, rec := capture.New()
	u := &upstream{http: srv.Client(), log: log, name: upstreamNyaa, feed: srv.URL + "/api"}
	items, _, err := u.search(t.Context(), url.Values{"t": {"search"}, "q": {"x"}})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1 (only the same-origin item)", len(items))
	}

	const msg = "upstream items dropped: download URL not on the Prowlarr endpoint origin"
	if got := rec.CountExact(msg); got != 1 {
		t.Fatalf("dropped-items WARN count = %d, want exactly 1; log output:\n%s", got, strings.Join(rec.Messages(), "\n"))
	}
	var dropped, kept int64 = -1, -1
	for _, r := range rec.Records() {
		if r.Message != msg {
			continue
		}
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "dropped":
				dropped = a.Value.Int64()
			case "kept":
				kept = a.Value.Int64()
			}
			return true
		})
	}
	if dropped != 2 || kept != 1 {
		t.Errorf("WARN attrs dropped=%d kept=%d, want dropped=2 kept=1", dropped, kept)
	}

	// A clean response (every item on the endpoint origin) must not warn.
	cleanLog, cleanRec := capture.New()
	const cleanTmpl = `<rss xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel>
<item><title>ok</title><enclosure url="http://HOST/1/download?link=abc" length="1" type="application/x-bittorrent"/></item>
</channel></rss>`
	cleanSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, strings.ReplaceAll(cleanTmpl, "HOST", r.Host))
	}))
	defer cleanSrv.Close()
	cu := &upstream{http: cleanSrv.Client(), log: cleanLog, name: upstreamNyaa, feed: cleanSrv.URL + "/api"}
	if _, _, err := cu.search(t.Context(), url.Values{"t": {"search"}, "q": {"x"}}); err != nil {
		t.Fatalf("clean search: %v", err)
	}
	if got := cleanRec.CountExact(msg); got != 0 {
		t.Errorf("clean response WARN count = %d, want 0; log output:\n%s", got, strings.Join(cleanRec.Messages(), "\n"))
	}
}

// TestUpstreamSearchReportsRawPageCount pins search's second return value: the
// RAW parsed-item count of the response, taken BEFORE the download-URL origin
// filter. It is what the request log line's upstream_fetched reports, so an
// operator can tell a small tracker answer from a large one whose
// foreign-origin items were all dropped; reporting the filtered count instead
// would silently merge the two.
func TestUpstreamSearchReportsRawPageCount(t *testing.T) {
	const feedTmpl = `<rss xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel>
<item><title>same origin</title><enclosure url="http://HOST/1/download?link=abc" length="1" type="application/x-bittorrent"/></item>
<item><title>foreign host</title><enclosure url="http://evil.internal/steal" length="1" type="application/x-bittorrent"/></item>
<item><title>no link</title></item>
</channel></rss>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, strings.ReplaceAll(feedTmpl, "HOST", r.Host))
	}))
	defer srv.Close()

	u := &upstream{http: srv.Client(), log: slog.Default(), name: upstreamNyaa, feed: srv.URL + "/api"}
	items, raw, err := u.search(t.Context(), url.Values{"t": {"search"}, "q": {"x"}})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("filtered items = %d, want 1 (only the same-origin item survives)", len(items))
	}
	if raw != 3 {
		t.Errorf("raw page count = %d, want 3 (the parsed-item count BEFORE the origin filter)", raw)
	}
}

// TestFilterDownloadURLsWarnsOnBlankedDisplayURLs pins the operational
// contract of the display-URL gate's warning, the sibling of the origin
// filter's "items dropped" WARN: an item whose passthrough InfoURL/GUID are not
// the tracker's own canonical http(s) page URLs survives with those fields
// blanked, and the blanking is reported exactly once per search carrying the
// true per-FIELD count (two bad fields on one item count 2) and the kept-item
// count, while a clean response never warns. It is the only observable
// distinguishing a healthy passthrough from a tampered upstream whose
// clickable links are being stripped.
func TestFilterDownloadURLsWarnsOnBlankedDisplayURLs(t *testing.T) {
	log, rec := capture.New()
	u := &upstream{log: log, name: upstreamNyaa, feed: "http://prowlarr:9696/1/api"}
	items := []item{
		{
			Title: "both display fields hostile", DownloadURL: "http://prowlarr:9696/1/download?link=a",
			InfoURL: "https://evil.example/phish", GUID: "javascript:alert(1)",
		},
		{
			Title: "clean", DownloadURL: "http://prowlarr:9696/1/download?link=b",
			InfoURL: "https://nyaa.si/view/1", GUID: "https://nyaa.si/view/1",
		},
	}
	got := u.filterDownloadURLs(items, mustFeedURL(t, u))
	if len(got) != 2 {
		t.Fatalf("kept items = %d, want 2 (a bad display URL blanks the field, never drops the item)", len(got))
	}
	if got[0].InfoURL != "" || got[0].GUID != "" {
		t.Errorf("hostile display fields = (%q, %q), want both blanked", got[0].InfoURL, got[0].GUID)
	}
	if got[1].InfoURL != "https://nyaa.si/view/1" || got[1].GUID != "https://nyaa.si/view/1" {
		t.Errorf("canonical display fields = (%q, %q), want both preserved", got[1].InfoURL, got[1].GUID)
	}

	const msg = "upstream display URLs blanked: not the tracker's own canonical http(s) page URL"
	if n := rec.CountExact(msg); n != 1 {
		t.Fatalf("blanked-display WARN count = %d, want exactly 1; log output:\n%s", n, strings.Join(rec.Messages(), "\n"))
	}
	if v, ok := rec.AttrValue(msg, "blanked"); !ok || v != "2" {
		t.Errorf("WARN blanked = %q (found=%v), want 2 (counted per blanked FIELD, not per item)", v, ok)
	}
	if v, ok := rec.AttrValue(msg, "kept_items"); !ok || v != "2" {
		t.Errorf("WARN kept_items = %q (found=%v), want 2", v, ok)
	}

	// A clean response must not warn at all.
	cleanLog, cleanRec := capture.New()
	cu := &upstream{log: cleanLog, name: upstreamNyaa, feed: "http://prowlarr:9696/1/api"}
	cu.filterDownloadURLs([]item{{
		Title: "clean", DownloadURL: "http://prowlarr:9696/1/download?link=b",
		InfoURL: "https://nyaa.si/view/1", GUID: "https://nyaa.si/view/1",
	}}, mustFeedURL(t, cu))
	if n := cleanRec.CountExact(msg); n != 0 {
		t.Errorf("clean response blanked-display WARN count = %d, want 0", n)
	}
}

// timeoutOnFirstCall returns a Torznab server that stalls past the caller's
// per-attempt client timeout on its FIRST request and answers with a valid
// empty Torznab document afterwards, plus a counter of requests it received.
// stallBody selects WHERE it stalls: before writing anything (a header-phase
// timeout) or after flushing a 200 header and a partial body (a body-read
// timeout).
func timeoutOnFirstCall(t *testing.T, stallBody bool) (*httptest.Server, func() int) {
	t.Helper()
	var (
		mu    sync.Mutex
		calls int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			if stallBody {
				w.Header().Set("Content-Type", "application/rss+xml")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><rss><channel>`)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
			// Hold the response open until the client's attempt timer fires
			// (the client aborts the request, cancelling this context).
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w,
			`<?xml version="1.0" encoding="UTF-8"?><rss xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel></channel></rss>`)
	}))
	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
}

// TestSearchBoundsAttemptWithoutClientTimeout pins that the per-attempt bound
// is this package's own, not an obligation on whoever built the client: with a
// client carrying NO http.Client.Timeout at all, a stalled upstream still ends
// the attempt (fetchAndParse's attempt context) and the search still spends its
// remaining budget and succeeds. Before the deadline moved in here, the number
// lived only in an exported constant the composition root had to wire into
// http.Client.Timeout, so a client built without it made the retry tree - and
// server.go's writeTimeout, which is sized against it - unbounded in practice.
func TestSearchBoundsAttemptWithoutClientTimeout(t *testing.T) {
	srv, calls := timeoutOnFirstCall(t, false)
	defer srv.Close()

	restore := upstreamAttemptTimeout
	upstreamAttemptTimeout = 150 * time.Millisecond
	t.Cleanup(func() { upstreamAttemptTimeout = restore })

	client := srv.Client()
	client.Timeout = 0 // no transport backstop: the package must bound the attempt
	u := &upstream{http: client, log: slog.Default(), name: upstreamNyaa, feed: srv.URL}
	if _, _, err := u.search(t.Context(), url.Values{"t": {"search"}, "q": {"x"}}); err != nil {
		t.Fatalf("search with a package-bounded attempt = %v, want the retry to succeed", err)
	}
	if got := calls(); got != 2 {
		t.Errorf("upstream requests = %d, want 2 (the package's own attempt deadline fired and was retried)", got)
	}
}

// TestSearchRetriesAttemptTimeoutAwaitingHeaders pins that a per-attempt
// timeout (the package's own attempt deadline, or a client-level
// http.Client.Timeout backstop - this test drives the latter, which is the
// cheaper timer to shorten)
// participates in the bounded retry budget instead of ending the search after
// one attempt. The timer's error matches context.DeadlineExceeded, which
// httpx.IsTransient treats as terminal, so an unnormalized attempt timeout
// would fail an interactive search immediately and latch the harvest's tracker
// scope for the whole rebuild.
func TestSearchRetriesAttemptTimeoutAwaitingHeaders(t *testing.T) {
	srv, calls := timeoutOnFirstCall(t, false)
	defer srv.Close()

	client := srv.Client()
	client.Timeout = 150 * time.Millisecond
	u := &upstream{http: client, log: slog.Default(), name: upstreamNyaa, feed: srv.URL}
	items, _, err := u.search(t.Context(), url.Values{"t": {"search"}, "q": {"x"}})
	if err != nil {
		t.Fatalf("search after one header-phase attempt timeout = %v, want the retry to succeed", err)
	}
	if len(items) != 0 {
		t.Errorf("items = %d, want 0 (the second attempt returned an empty feed)", len(items))
	}
	if got := calls(); got != 2 {
		t.Errorf("upstream requests = %d, want 2 (attempt timeout retried within the budget)", got)
	}
}

// TestSearchRetriesAttemptTimeoutReadingBody pins the sibling exit: the same
// per-attempt timer can fire while the body of an already-200 response stalls,
// which surfaces from httpx.ReadLimitedBody rather than from client.Do. That
// exit must be normalized identically, or a stalled body ends the search after
// one attempt.
func TestSearchRetriesAttemptTimeoutReadingBody(t *testing.T) {
	srv, calls := timeoutOnFirstCall(t, true)
	defer srv.Close()

	client := srv.Client()
	client.Timeout = 150 * time.Millisecond
	u := &upstream{http: client, log: slog.Default(), name: upstreamNyaa, feed: srv.URL}
	if _, _, err := u.search(t.Context(), url.Values{"t": {"search"}, "q": {"x"}}); err != nil {
		t.Fatalf("search after one body-read attempt timeout = %v, want the retry to succeed", err)
	}
	if got := calls(); got != 2 {
		t.Errorf("upstream requests = %d, want 2 (body-read timeout retried within the budget)", got)
	}
}

// TestSearchDoesNotRetryExpiredCallerContext pins the other half of the split:
// the CALLER's expired context stays terminal. Only the client-owned attempt
// timer is retryable, so a cancelled cycle or a shed request must not spend
// further attempts, and the returned error keeps its context identity.
func TestSearchDoesNotRetryExpiredCallerContext(t *testing.T) {
	srv, calls := timeoutOnFirstCall(t, false)
	defer srv.Close()

	client := srv.Client()
	client.Timeout = time.Minute
	u := &upstream{http: client, log: slog.Default(), name: upstreamNyaa, feed: srv.URL}
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	_, _, err := u.search(ctx, url.Values{"t": {"search"}, "q": {"x"}})
	if err == nil {
		t.Fatal("search with an expired caller context = nil error, want the deadline surfaced")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want the caller's context deadline preserved", err)
	}
	if got := calls(); got > 1 {
		t.Errorf("upstream requests = %d, want at most 1 (an expired caller context is terminal)", got)
	}
}

// TestFilterDownloadURLsKeepsDisplayOnsetWhenPageFullyDropped pins that the
// display-URL diagnostic only reports on pages it actually observed: when every
// item of a non-empty page is dropped for an off-origin download URL, no
// InfoURL/GUID was inspected, so a zero blanked count must NOT clear the onset
// state and announce a recovery. Otherwise a simultaneous download-origin fault
// masquerades as the display fault recovering, and the next surviving hostile
// item re-arms another WARN.
func TestFilterDownloadURLsKeepsDisplayOnsetWhenPageFullyDropped(t *testing.T) {
	const (
		blankedMsg  = "upstream display URLs blanked: not the tracker's own canonical http(s) page URL"
		recoveryMsg = "upstream display URLs back on the tracker's canonical host"
	)
	log, rec := capture.New()
	u := &upstream{log: log, name: upstreamNyaa, feed: "http://prowlarr:9696/1/api"}

	// Onset: a kept item carrying a hostile display URL arms displayWarned.
	u.filterDownloadURLs([]item{{
		Title: "hostile display", DownloadURL: "http://prowlarr:9696/1/download?link=a",
		InfoURL: "https://evil.example/phish", GUID: "https://nyaa.si/view/1",
	}}, mustFeedURL(t, u))
	if n := rec.CountExact(blankedMsg); n != 1 {
		t.Fatalf("blanked-display WARN count = %d, want 1 (the onset)", n)
	}

	// A non-empty page whose every download URL is off-origin observes no
	// display field at all.
	if got := u.filterDownloadURLs([]item{{
		Title: "off-origin", DownloadURL: "https://attacker.example/poison.torrent",
		InfoURL: "https://nyaa.si/view/2", GUID: "https://nyaa.si/view/2",
	}}, mustFeedURL(t, u)); len(got) != 0 {
		t.Fatalf("kept items = %d, want 0 (the off-origin download URL is dropped)", len(got))
	}
	if n := rec.CountExact(recoveryMsg); n != 0 {
		t.Errorf("display-recovery INFO count = %d, want 0 (nothing was observed on a fully dropped page); log output:\n%s",
			n, strings.Join(rec.Messages(), "\n"))
	}
	if !u.displayWarned.Load() {
		t.Error("displayWarned = false, want the onset state retained across a fully dropped page")
	}

	// The genuine recovery still fires once the fault clears on an observed item.
	u.filterDownloadURLs([]item{{
		Title: "clean", DownloadURL: "http://prowlarr:9696/1/download?link=b",
		InfoURL: "https://nyaa.si/view/3", GUID: "https://nyaa.si/view/3",
	}}, mustFeedURL(t, u))
	if n := rec.CountExact(recoveryMsg); n != 1 {
		t.Errorf("display-recovery INFO count = %d, want 1 once a clean page is observed", n)
	}
}

// TestFilterDownloadURLsKeepsDisplayOnsetWhenSurvivorsCarryNoDisplayURL pins the
// third unobserved-evidence case of the display gate: a page whose SURVIVING
// items carry neither an InfoURL nor a GUID inspects no display field at all, so
// it must not clear displayWarned. The len(out) > 0 guard reported a zero blanked
// count here and announced a false recovery; the observedDisplay count is what
// closes it, and nothing else in the suite fails if that count is removed.
func TestFilterDownloadURLsKeepsDisplayOnsetWhenSurvivorsCarryNoDisplayURL(t *testing.T) {
	const (
		blankedMsg  = "upstream display URLs blanked: not the tracker's own canonical http(s) page URL"
		recoveryMsg = "upstream display URLs back on the tracker's canonical host"
	)
	log, rec := capture.New()
	u := &upstream{log: log, name: upstreamNyaa, feed: "http://prowlarr:9696/1/api"}

	// Onset: a kept item carrying a hostile display URL arms displayWarned.
	u.filterDownloadURLs([]item{{
		Title: "hostile display", DownloadURL: "http://prowlarr:9696/1/download?link=a",
		InfoURL: "https://evil.example/phish", GUID: "https://nyaa.si/view/1",
	}}, mustFeedURL(t, u))
	if n := rec.CountExact(blankedMsg); n != 1 {
		t.Fatalf("blanked-display WARN count = %d, want 1 (the onset)", n)
	}

	// A surviving item with neither display field observes nothing about the gate.
	if got := u.filterDownloadURLs([]item{{
		Title: "no display fields", DownloadURL: "http://prowlarr:9696/1/download?link=b",
	}}, mustFeedURL(t, u)); len(got) != 1 {
		t.Fatalf("kept items = %d, want 1 (the on-origin download URL survives)", len(got))
	}
	if n := rec.CountExact(recoveryMsg); n != 0 {
		t.Errorf("display-recovery INFO count = %d, want 0 (no display field was inspected); log output:\n%s",
			n, strings.Join(rec.Messages(), "\n"))
	}
	if !u.displayWarned.Load() {
		t.Error("displayWarned = false, want the onset state retained across a page carrying no display URLs")
	}
}

// TestTerminalTorznabCode pins both numeric boundaries of the retry-vs-fail-fast
// split on a Torznab <error> document's code. Only 100-199 (credentials/account)
// and 200-299 (request/parameter) are deterministic enough to fail the search on
// its first attempt; a content/server-side code (300 "no such item", 500, 900
// "unknown error") and a non-numeric code (-1) must stay inside the bounded retry
// budget. TestFetchAndParseClassifiesTorznabErrorDoc exercises the classification
// end-to-end but only at 100/201/900, so neither edge of the window is pinned.
func TestTerminalTorznabCode(t *testing.T) {
	tests := map[int]bool{
		-1: false, 0: false, 99: false,
		100: true, 101: true, 199: true, 200: true, 201: true, 299: true,
		300: false, 301: false, 500: false, 900: false, 910: false,
	}
	for code, want := range tests {
		if got := terminalTorznabCode(code); got != want {
			t.Errorf("terminalTorznabCode(%d) = %v, want %v", code, got, want)
		}
	}
}

// TestUpstreamSecretsRedactionScope pins WHICH values upstreamSecrets collects
// as transmitted credentials, the set every log line and error message on the
// upstream failure path is scrubbed against (CWE-532). Four rules decide it and
// none of them is reachable from the existing redaction tests, which all use a
// credential-NAMED query parameter: a value on a parameter whose NAME says
// nothing is a secret from minEmbeddedSecretLen bytes up (an operator-embedded
// token), a credential-named parameter is a secret at any length even when its
// NAME is percent-encoded, both the raw and the percent-decoded form of a value
// are registered because a hostile upstream can reflect either, and a raw-query
// pair with no '=' at all has no name to judge, so the whole token is graded as
// an unlabelled VALUE against the same length floor.
func TestUpstreamSecretsRedactionScope(t *testing.T) {
	const (
		unlabelledAtFloor = "abcdefghijklmnop"    // exactly minEmbeddedSecretLen bytes
		unlabelledBelow   = "abcdefghijklmno"     // one byte short of the floor
		escapedParamValue = "abc%2Fdefghijklmnop" // percent-encoded form of abc/defghijklmnop
	)
	has := func(secrets []string, want string) bool {
		return slices.Contains(secrets, want)
	}
	tests := map[string]struct {
		rawQuery string
		want     []string
		unwanted []string
	}{
		"non-credential-named value at the length floor is a secret": {
			rawQuery: "indexer=" + unlabelledAtFloor,
			want:     []string{unlabelledAtFloor},
		},
		"non-credential-named value below the floor stays readable in a diagnostic": {
			rawQuery: "indexer=" + unlabelledBelow,
			unwanted: []string{unlabelledBelow},
		},
		"percent-encoded credential name is a secret at any length": {
			rawQuery: "%6Bey=short",
			want:     []string{"short"},
		},
		"raw and decoded forms of a credential value are both secrets": {
			rawQuery: "apikey=" + escapedParamValue,
			want:     []string{escapedParamValue, "abc/defghijklmnop"},
		},
		// A pair with no '=' has no name to judge, so the length floor alone
		// decides: the whole token is what rides the outgoing request.
		"bare token with no '=' at the length floor is a secret": {
			rawQuery: unlabelledAtFloor,
			want:     []string{unlabelledAtFloor},
		},
		"bare structural flag with no '=' stays readable in a diagnostic": {
			rawQuery: "rss",
			unwanted: []string{"rss"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			u := &upstream{name: upstreamNyaa, feed: "http://prowlarr:9696/1/api?" + tc.rawQuery}
			got := u.upstreamSecrets()
			for _, want := range tc.want {
				if !has(got, want) {
					t.Errorf("upstreamSecrets() = %q, want it to carry the transmitted credential %q", got, want)
				}
			}
			for _, unwanted := range tc.unwanted {
				if has(got, unwanted) {
					t.Errorf("upstreamSecrets() = %q, want it NOT to carry the structural value %q", got, unwanted)
				}
			}
		})
	}
}

// TestSearchCredentialRejectionLogsAtErrorNamingTheRemedy pins the SEARCH path's
// half of the credential classification, which is the half whose consequence
// escalates: every rejected search answers the arr a Torznab <error>, and an arr
// counts those toward disabling the indexer - RSS included - so a wrong
// indexer.prowlarr_api_key takes the whole feed down while the container stays
// healthy. fetchRaw's permanentUpstreamCredentialError arm is what turns that
// into the one ERROR line naming the remedy, and nothing exercised it in either
// of its two shapes: a 401/403 status, or Prowlarr's 200 + Torznab <error>
// document in the auth band (codes 100-199).
//
// The LEVEL is the assertion that matters. alerts/logql.yaml keys SeadexScoutCycleError
// on level=ERROR and on no message, so a re-level to WARN - or deleting the arm,
// which drops the rejection into the generic "upstream query failed" WARN one
// line below - leaves the dead feed un-alerted with the suite green.
func TestSearchCredentialRejectionLogsAtErrorNamingTheRemedy(t *testing.T) {
	const credentialsMsg = "upstream rejected the credentials"
	tests := map[string]http.HandlerFunc{
		"401 status": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		},
		"torznab auth error document": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/rss+xml")
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><error code="100" description="Incorrect user credentials"/>`)
		},
	}
	for name, handler := range tests {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(handler)
			defer srv.Close()

			log, rec := capture.New()
			ix := New(&Config{
				NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "k",
			}, log, srv.Client())

			items, stats, fault := ix.query(t.Context(),
				url.Values{"t": {"tvsearch"}, "q": {"Frieren"}}, upstreamNyaa)

			if len(items) != 0 {
				t.Errorf("got %d items from a rejected search, want 0", len(items))
			}
			if !stats.answered {
				t.Errorf("stats = %+v, want an answered search", stats)
			}
			if fault == nil {
				t.Error("fault = nil, want a torznabFault: an empty 200 feed reads as a clean no-match, which records a credential failure as a successful search")
			}
			if got := rec.CountLevel(slog.LevelError, credentialsMsg); got != 1 {
				t.Errorf("credential rejection logged at ERROR %d times, want 1 (alerts/logql.yaml keys on the level, not the message); log output:\n%s",
					got, strings.Join(rec.Messages(), "\n"))
			}
			if rec.Contains("upstream query failed") {
				t.Errorf("a credential rejection also logged the generic upstream WARN; the classes must stay distinct; log output:\n%s",
					strings.Join(rec.Messages(), "\n"))
			}
		})
	}
}
