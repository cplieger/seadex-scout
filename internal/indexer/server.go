package indexer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/cplieger/webhttp"
)

const (
	shutdownGrace     = 10 * time.Second
	readHeaderTimeout = 15 * time.Second
	readTimeout       = 30 * time.Second
	idleTimeout       = 120 * time.Second
	// writeTimeout bounds a stalled response consumer. It is sized above the
	// complete bounded Prowlarr retry budget - three 60-second HTTP attempts
	// plus at most two 60-second Retry-After waits - with render margin, so
	// no legitimate search can hit it while a client that stops reading
	// cannot hold the connection, handler goroutine, and rendered response
	// indefinitely.
	writeTimeout = 6 * time.Minute
)

// listenAddr is the fixed LAN bind address for the Torznab feed server. The
// port is an internal detail (the container/compose port mapping publishes
// it), not an operator-tuned setting, so it is hardcoded rather than a key.
// A var rather than a const purely as a test seam: the server lifecycle
// tests point it at an ephemeral 127.0.0.1 port so they never collide with a
// real deployment's :9118.
var listenAddr = ":9118"

// Run serves the Torznab endpoint from the persisted feed snapshot until ctx is
// cancelled. The endpoint listens immediately (so an arr's caps Test succeeds
// right away); it serves whatever feed the last compare cycle persisted (empty
// until the first cycle on a fresh install) and reloads the snapshot when a
// cycle rewrites it. It owns no health marker - the daemon that runs it does -
// so a feed failure never flips container health.
func (ix *Indexer) Run(ctx context.Context) error {
	// Fail closed at the network boundary: config.Validate (validateIndexer)
	// already rejects a configured feed with an empty feed_api_key on the
	// daemon path, but any alternate construction of the exported Indexer must
	// never bind and serve the feed unauthenticated - the AnimeBytes RSS feed
	// embeds ab_passkey in its download links.
	if ix.cfg.APIKey == "" {
		return errors.New("indexer: indexer.feed_api_key is empty; refusing to serve the Torznab feed unauthenticated")
	}
	// Bind up front so a port-in-use error surfaces synchronously here and is
	// returned to the daemon's startIndexer, which logs it. The feed owns no
	// health marker (the compare loop does), so a bind failure never flips
	// container health.
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("indexer listen on %s: %w", listenAddr, err)
	}

	// The HTTP surface rides the shared webhttp plumbing (server bootstrap +
	// graceful shutdown). Logging is the standard access line (method, PATH
	// only, status, duration, request id) - adopted here because webhttp's
	// RequestLogger logs r.URL.Path and never the query string, so the Torznab
	// apikey (which arrives as a query parameter) cannot leak into the access
	// log; it sits outermost so a recovered panic logs as its 500. serve's own
	// domain line (scope/params/result counts) complements it - that line
	// whitelists the params it logs and likewise never logs apikey. Recoverer
	// turns a handler panic into a logged 500 rendered as a Torznab <error>
	// via torznabErrorResponder - not net/http's bare connection close, and
	// not webhttp's default JSON envelope, which is the wrong wire shape for
	// this XML endpoint. WriteTimeout is set (see writeTimeout): this endpoint
	// only emits finite XML and the upstream Prowlarr retry tree has a
	// calculable upper bound, so the deadline bounds stalled response
	// consumers while leaving the bounded retry budget intact.
	handler := webhttp.Chain(ix.handler(),
		webhttp.Logging(webhttp.WithLogger(ix.log)),
		webhttp.Recoverer(
			webhttp.WithRecoverLogger(ix.log),
			webhttp.WithRecoverResponder(torznabErrorResponder),
		),
	)
	srv := webhttp.NewServer(handler,
		webhttp.WithReadHeaderTimeout(readHeaderTimeout),
		webhttp.WithReadTimeout(readTimeout),
		webhttp.WithIdleTimeout(idleTimeout),
		webhttp.WithWriteTimeout(writeTimeout),
	)

	ix.log.Info("seadex-scout indexer listening",
		"addr", listenAddr, "upstreams", len(ix.upstreams))

	if err := webhttp.Run(ctx, srv, ln, nil, webhttp.WithShutdownGrace(shutdownGrace)); err != nil {
		return fmt.Errorf("indexer server: %w", err)
	}
	ix.log.Info("indexer shutdown complete", "cause", context.Cause(ctx))
	return nil
}

// torznabErrorResponder is the webhttp Recoverer ErrorResponder for the Torznab
// feed: it renders a recovered panic's 500 as a Torznab <error> document on the
// XML content type the arrs expect, in place of webhttp's default JSON envelope.
// Recoverer already logged the panic and only calls this when the response has
// not been committed; this just writes the body.
func torznabErrorResponder(w http.ResponseWriter, _ *http.Request, status int, _, msg string) {
	noCacheHeaders(w.Header())
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, renderError(errCodeUnknown, msg))
}

// noCacheHeaders marks a Torznab response as non-cacheable. The authenticated
// /ab RSS body embeds the operator's AnimeBytes passkey in its download links,
// so no cache may retain the representation beyond the request lifetime.
func noCacheHeaders(h http.Header) {
	h.Set("Cache-Control", "private, no-store, max-age=0")
	h.Set("Pragma", "no-cache")
}

// rejectTorznab renders a Torznab <error> rejection and logs one INFO line
// naming the reason. noCacheHeaders was already set by serve for every
// authenticated response.
func (ix *Indexer) rejectTorznab(w http.ResponseWriter, scope, reason string, code int, msg string) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	_, _ = io.WriteString(w, renderError(code, msg))
	ix.log.Info("indexer request rejected", "scope", scope, "reason", reason)
}

// logParam bounds and cleans a request-controlled string (URL path, Host,
// Torznab query params) before it reaches a log line - the same emit-boundary
// policy sanitizeUpstreamText applies to untrusted upstream text: single-line
// rune safety (runesafe.SanitizeSingleLine), then a 256-byte cap on a rune
// boundary (truncated output appends "...") so a caller holding the feed key
// cannot inject near-megabyte query values (NewServer permits up to 1 MiB of
// headers) into oversized Loki records. Structured JSON already prevents line
// injection; this bounds volume. The apikey is never passed through this
// helper or into any log.
func logParam(s string) string { return capLogText(s, 256) }

// handler builds the HTTP mux (a single Torznab endpoint).
func (ix *Indexer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", ix.serve)
	return mux
}

// allowAuthFailure rate-limits responses to bad apikey attempts (token
// bucket: burst 10, refill 10/min) so a LAN client cannot brute-force
// feed_api_key at wire speed or flood the log with per-attempt lines.
// Correct-key callers never consult it, so the happy path is untouched.
func (ix *Indexer) allowAuthFailure() bool {
	const burst, perMin = 10.0, 10.0
	ix.authFailMu.Lock()
	defer ix.authFailMu.Unlock()
	now := time.Now()
	if !ix.authFailLast.IsZero() {
		ix.authFailTokens = min(burst, ix.authFailTokens+now.Sub(ix.authFailLast).Minutes()*perMin)
	} else {
		ix.authFailTokens = burst
	}
	ix.authFailLast = now
	if ix.authFailTokens < 1 {
		return false
	}
	ix.authFailTokens--
	return true
}

// serve handles the Torznab endpoint. Every request must address a specific
// tracker feed - /nyaa or /ab by path, or a nyaa.*/ab.* host; an unscoped
// request is 404 (there is no combined feed). t=caps returns capabilities,
// everything else proxies that tracker's Prowlarr endpoint filtered to SeaDex's
// curation.
func (ix *Indexer) serve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if ix.cfg.APIKey == "" {
		// Fail closed at the handler too: Run already refuses to bind with an
		// empty feed_api_key, so this branch is unreachable in production, but
		// a second independent guard keeps any future construction path from
		// serving the passkey-bearing feed unauthenticated - and it is what
		// distinguishes "auth not configured" (this 503, an operator problem)
		// from "wrong key" (the 401 below). The static-token verifier itself
		// fails CLOSED on an empty configured key, so skipping this guard
		// could never open the gate; it would just misreport the unconfigured
		// state as an unauthorized caller.
		ix.log.Error("indexer request rejected", "reason", "feed_api_key not configured", "path", logParam(r.URL.Path))
		http.Error(w, "service unavailable: feed_api_key not configured", http.StatusServiceUnavailable)
		return
	}
	// Constant-time verification, with the length side-channel (CWE-208)
	// closed by comparing fixed-length SHA-256 digests rather than the raw
	// strings, lives in the shared library; the verifier is built once in New
	// (pre-hashed configured key): see webhttp.NewStaticTokenVerifier.
	if !ix.verifyKey.Verify(q.Get("apikey")) {
		// Failed attempts are throttled (allowAuthFailure) as a second defense
		// layer behind the key's entropy: past the burst, a brute-forcing or
		// misconfigured LAN client gets 429s and stops generating log lines.
		if !ix.allowAuthFailure() {
			// One token refills every six seconds (10/min): tell a compliant
			// retrying client when trying again can succeed.
			w.Header().Set("Retry-After", "6")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		ix.log.Info("indexer request rejected", "reason", "bad apikey", "path", logParam(r.URL.Path), "remote", logParam(r.RemoteAddr))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// Every authenticated caps/error/feed response is marked non-cacheable up
	// front: the /ab RSS body embeds the operator's AnimeBytes passkey in its
	// download links, and a browser, intermediary, or explicitly configured
	// reverse-proxy cache must never retain that credential-bearing body
	// beyond the request.
	noCacheHeaders(w.Header())
	scope := scopeFor(r.Host, r.URL.Path)
	if scope == "" {
		ix.log.Info("indexer request rejected", "reason", "no tracker scope", "path", logParam(r.URL.Path), "host", logParam(r.Host), "remote", logParam(r.RemoteAddr))
		http.Error(w, "not found: address a tracker feed at /nyaa or /ab", http.StatusNotFound)
		return
	}
	if strings.EqualFold(strings.TrimSpace(q.Get("t")), "caps") {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		_, _ = io.WriteString(w, renderCaps())
		ix.log.Info("indexer request", "scope", scope, "t", "caps")
		return
	}
	// The AnimeBytes RSS feed needs the operator's passkey to build grabbable
	// links, so without it a configured /ab feed has nothing to serve a periodic
	// RSS check (an empty-q request). Answer that with a Torznab error rather
	// than an empty feed, so Prowlarr's save-test fails with a clear reason and
	// the operator sets the passkey. An AB search (non-empty q) is unaffected:
	// it proxies Prowlarr, whose own link needs no passkey. An UNCONFIGURED AB
	// tracker (empty ab_torznab_url, the README's off switch) is not nudged: it
	// falls through to the empty feed below, the same shape as a tracker with
	// no data.
	if scope == upstreamAB && ix.cfg.ABTorznabURL != "" && ix.cfg.ABPasskey == "" && strings.TrimSpace(q.Get("q")) == "" {
		ix.rejectTorznab(w, scope, "ab passkey not configured", errCodeIncorrectCredentials,
			"AnimeBytes passkey not configured: set indexer.ab_passkey in seadex-scout to serve the AnimeBytes feed")
		return
	}
	items, stats := ix.query(r.Context(), q, scope)
	// A snapshot-unavailable state (the persisted feed failed to load before
	// any snapshot was installed - see snapFailed) is a local fault, not an
	// empty catalogue: an empty 200 feed would read as a clean no-match to
	// the arr, silently recording the fault as a successful search. Render a
	// Torznab <error>, exactly like an unavailable Prowlarr dependency.
	if stats.snapshotUnavailable {
		ix.rejectTorznab(w, scope, "feed snapshot unavailable", errCodeUnknown,
			"feed snapshot unavailable: the persisted SeaDex feed failed to load; results unavailable until a snapshot loads")
		return
	}
	// A total upstream failure (every queried Prowlarr upstream failed) is
	// reported as a Torznab <error>, not an empty 200 feed: an empty feed reads
	// as a clean "no SeaDex match" to the arr, which would silently record a
	// Prowlarr outage as a successful no-results search. A partial failure (one
	// of several upstreams answered) keeps the degraded-but-successful feed.
	if stats.upstreamFailed {
		ix.rejectTorznab(w, scope, "upstream query failed", errCodeUnknown,
			"upstream Prowlarr query failed; search results unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	_, _ = io.WriteString(w, renderFeed(items))
	// One INFO line per request: the incoming Torznab params plus a result
	// summary. `answered` is false when the feed deliberately skips a per-episode
	// query (so an empty result reads as a skip, not a no-match); `feed` is true
	// for an empty-q RSS check served from the synthesized SeaDex feed; `upstream`
	// is how many upstream results survived the Prowlarr fetch (post origin-filter) for a search,
	// `curated` how many items were returned after curation/synthesis, `returned`
	// the final count after the category filter.
	ix.log.Info("indexer request",
		"scope", scope,
		"t", logParam(q.Get("t")),
		"q", logParam(q.Get("q")),
		"season", logParam(q.Get("season")),
		"ep", logParam(q.Get("ep")),
		"cat", logParam(q.Get("cat")),
		"answered", stats.answered,
		"feed", stats.feed,
		"upstream", stats.upstream,
		"curated", stats.curated,
		"returned", len(items))
}

// scopeFor resolves which tracker's results a request targets: the URL path
// first (scopeFromPath), the Host subdomain as a fallback (scopeFromHost), or ""
// when neither names a tracker - which serve treats as 404, since there is no
// combined feed. Serving per-tracker lets an arr treat the feed as two indexers
// and gate each tracker's RSS/automatic/interactive use with that indexer's own
// flags - the arr is the only component that knows the search type (it is never
// carried in the Torznab request), so it owns that decision. Two
// addressing styles are supported so it works whether seadex-scout shares a host
// with the arrs or sits behind a reverse proxy: a path (.../nyaa, .../ab) for
// direct use, or a subdomain (nyaa.example.com, ab.example.com) a proxy can map
// to the single port without rewriting the path.
func scopeFor(host, path string) string {
	if s := scopeFromPath(path); s != "" {
		return s
	}
	return scopeFromHost(host)
}

// scopeFromPath maps the URL path to a tracker via its first segment: "/nyaa..."
// -> nyaa, "/ab..." -> ab, anything else (including "/" and a bare "/api") -> ""
// (no tracker; serve 404s it).
func scopeFromPath(p string) string { return scopeFromToken(firstSegment(p)) }

// scopeFromHost maps a request Host to a tracker via its leading DNS label:
// nyaa.example.com -> nyaa, ab.example.com -> ab, anything else (a bare internal
// name like seadex-scout:9118, or any non-tracker host) -> "". This lets a
// reverse proxy route per-tracker subdomains to the one port with no path
// rewrite; the Host must reach the app unmodified (the default for a Caddy/nginx
// reverse proxy).
func scopeFromHost(host string) string {
	label, _, _ := strings.Cut(host, ".")
	return scopeFromToken(strings.ToLower(label))
}

// scopeFromToken maps a lowercased tracker token (a path segment or DNS
// label) to its feed scope, or "" for any non-tracker token.
func scopeFromToken(s string) string {
	switch s {
	case upstreamNyaa:
		return upstreamNyaa
	case upstreamAB:
		return upstreamAB
	}
	return ""
}

// firstSegment returns the first non-empty path segment, lowercased.
func firstSegment(p string) string {
	p = strings.TrimLeft(p, "/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		p = p[:i]
	}
	return strings.ToLower(p)
}
