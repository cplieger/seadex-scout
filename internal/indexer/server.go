package indexer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/seadex-scout/internal/secretref"
	"github.com/cplieger/webhttp"
)

const (
	shutdownGrace     = 10 * time.Second
	readHeaderTimeout = 15 * time.Second
	readTimeout       = 30 * time.Second
	idleTimeout       = 120 * time.Second
	// maxHeaderBytes bounds the request line plus headers, the pre-auth allocation gate
	// this endpoint's shape allows and webhttp leaves to the app.
	maxHeaderBytes = 16 << 10
)

// writeTimeout bounds a stalled response consumer. net/http arms it when the
// request headers are read, so it must cover EVERYTHING the handler can spend
// before the body is written: the complete bounded Prowlarr retry budget plus a
// one-minute render margin. Deriving it from the budget's own constants keeps the
// deadline valid when the retry policy changes. It is a complete bound, and that
// is the point of the snapshot never being loaded on the request path: a
// handler's snapshot access is one lock-free read, so no filesystem wait can
// outlive it. A var so a test can shorten it.
var writeTimeout = upstreamMaxAttempts*upstreamAttemptTimeout +
	(upstreamMaxAttempts-1)*httpx.RetryAfterCap + time.Minute

// listenAddr is the fixed LAN bind address for the Torznab feed server. The port
// is an internal detail (the compose port mapping publishes it), not an
// operator-tuned setting. A var purely as a test seam: the lifecycle tests point
// it at an ephemeral port so they never collide with a real deployment's :9118.
var listenAddr = ":9118"

// unusableFeedKey reports whether the configured feed_api_key cannot be trusted as
// the feed's authentication gate: it is absent, or it still holds an unexpanded
// environment-variable reference in EITHER spelling. The second case matters
// because this key IS the gate - a placeholder is guessable from the public
// config.example, and the /ab RSS body's download links carry the operator's
// AnimeBytes passkey. The reference test is internal/secretref's, shared with
// internal/config, so the two cannot disagree about which spellings count.
func unusableFeedKey(key string) bool { return secretref.Unusable(key) }

// unusableABPasskey reports whether indexer.ab_passkey can build a grabbable
// AnimeBytes link: it cannot when absent, and it cannot when it still holds an
// environment-variable reference - url.PathEscape would mint that placeholder into
// every AB download link, so every arr grab fails at the tracker while the feed
// reports success. Both take the documented empty-passkey path instead.
func unusableABPasskey(passkey string) bool { return secretref.Unusable(passkey) }

// Run serves the Torznab endpoint from the current feed snapshot until ctx is
// cancelled. It first starts the snapshot cache's own reload clock - the one piece
// of startup work, owned by this lifecycle method rather than by New - then listens
// immediately, so an arr's caps Test succeeds right away. It serves whatever feed
// the last compare cycle produced, taking this process's cycles in-process and a
// `poll` cycle's from the file. It owns no health marker, so a feed failure never
// flips container health.
func (ix *Indexer) Run(ctx context.Context) error {
	// Fail closed at the network boundary: config.Validate already rejects an empty
	// or unresolved feed_api_key on the daemon path, but any alternate construction
	// must never bind and serve the passkey-bearing feed behind a guessable or
	// absent gate. Field-name-only: the rejected value is a credential.
	if ix.keyUnusable {
		return errors.New("indexer: indexer.feed_api_key is empty or an unresolved ${VAR} reference; refusing to serve the Torznab feed")
	}
	// An unexpanded ${VAR} passkey cannot build a grabbable AB link, so a feed with
	// AnimeBytes ON takes the empty-passkey path. Say why once at startup: config
	// only rejects the unresolved form for feed_api_key. Gated on the tracker being
	// enabled, so a blank ab_torznab_url is not nudged about a parked passkey.
	if ix.enablement.enabled(upstreamAB) && secretref.Unexpanded(ix.enablement.ABPasskey) {
		ix.log.Warn("indexer.ab_passkey still holds an unexpanded environment-variable reference " +
			"(${VAR} or $VAR); the variable is unset or not allowlisted " +
			"(SONARR_/RADARR_/SEADEX_SCOUT_), so no grabbable AnimeBytes link can be derived - " +
			"the /ab RSS feed answers a Torznab error until it is set")
	}
	// The one piece of startup work, deliberately here and not in New. The cache's
	// loader goroutine lives for ctx, so it stops with the server.
	ix.cache.start(ctx)
	// Bind up front so a port-in-use error surfaces synchronously and is returned to
	// the daemon's startIndexer. The feed owns no health marker, so a bind failure
	// never flips container health.
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("indexer listen on %s: %w", listenAddr, err)
	}

	// The HTTP surface rides the shared webhttp plumbing: bootstrap plus graceful
	// shutdown here, the middleware stack in chain.
	srv := webhttp.NewServer(ix.chain(),
		webhttp.WithReadHeaderTimeout(readHeaderTimeout),
		webhttp.WithReadTimeout(readTimeout),
		webhttp.WithIdleTimeout(idleTimeout),
		webhttp.WithWriteTimeout(writeTimeout),
		webhttp.WithMaxHeaderBytes(maxHeaderBytes),
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
// feed: it renders a recovered panic's 500 as a Torznab <error> document on the XML
// content type the arrs expect. Recoverer already logged the panic and only calls
// this when the response has not been committed.
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

// rejectTorznab renders a Torznab <error> rejection and logs one INFO line naming
// the reason. The implicit HTTP 200 is deliberate: Torznab error documents ride
// 200 and the arrs classify by the <error> body, which is what surfaces the
// description on Prowlarr's save-test. Only a recovered panic writes a real 5xx.
func (ix *Indexer) rejectTorznab(w http.ResponseWriter, scope, reason string, code int, msg string) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	_, _ = io.WriteString(w, renderError(code, msg))
	ix.log.Info("indexer request rejected", "scope", scope, "reason", reason)
}

// logParam bounds and cleans a request-controlled string (URL path, Host, Torznab
// query params) before it reaches a log line - the same emit-boundary policy
// sanitizeUpstreamText applies: single-line rune safety, then a 256-byte cap on a
// rune boundary. maxHeaderBytes already refuses the megabyte shape, so this is the
// SECOND bound: it cuts a param that is legal at 16 KiB down to a legible field.
func logParam(s string) string { return capLogText(s, 256) }

// handler builds the HTTP mux (a single Torznab endpoint).
func (ix *Indexer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", ix.serve)
	return mux
}

// chain assembles the middleware stack Run serves. Order (outermost first): -
// SecurityHeaders: the OUTERMOST baseline (nosniff, X-Frame-Options: DENY,
// Referrer-Policy, Content-Security-Policy), set before anything else runs so every
// response carries it - a recovered panic's 500 AND authFailureLimiter's 429, which
// short-circuits its own response and would skip the headers entirely from any inner
// position.
func (ix *Indexer) chain() http.Handler {
	return webhttp.Chain(ix.handler(),
		// default-src 'none' is the whole policy this endpoint needs: every response is
		// a self-contained document with no subresources, so an inert document is the
		// correct browser reading of the credential-bearing /ab feed.
		webhttp.SecurityHeaders(webhttp.WithCSP("default-src 'none'")),
		ix.authFailureLimiter(),
		webhttp.Logging(webhttp.WithLogger(ix.log), webhttp.WithClientIP()),
		webhttp.Recoverer(
			webhttp.WithRecoverLogger(ix.log),
			webhttp.WithRecoverResponder(torznabErrorResponder),
		),
	)
}

// authFailureLimiter rate-limits bad-apikey requests through webhttp's failed-auth
// preset, which owns the tuning: a shared token bucket of burst 10 with one token
// every 6s, and a 429 envelope of code "too_many_auth_failures". Those numbers used
// to be local constants hand-copied by sibling services. The human message stays
// caller-owned, because naming the credential is what makes the refusal legible.
func (ix *Indexer) authFailureLimiter() webhttp.Middleware {
	return webhttp.FailedAuthRateLimit(func(r *http.Request) bool {
		return !ix.keyUnusable && !ix.verifyKey.Verify(r.URL.Query().Get("apikey"))
	}, "too many failed apikey attempts")
}

// serve handles the Torznab endpoint. Every request must address a specific tracker
// feed - /nyaa or /ab by path, or a nyaa.*/ab.* host; an unscoped request is 404.
// t=caps returns capabilities, everything else proxies that tracker's Prowlarr
// endpoint filtered to SeaDex's curation. Each response policy lives in a helper.
func (ix *Indexer) serve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if !ix.authorizeRequest(w, r, q) {
		return
	}
	// Every authenticated caps/error/feed response is marked non-cacheable up front:
	// the /ab RSS body embeds the operator's AnimeBytes passkey in its download
	// links, and no cache may retain that body beyond the request.
	noCacheHeaders(w.Header())
	scope, ok := ix.requireScope(w, r)
	if !ok {
		return
	}
	if ix.serveCaps(w, q, scope) {
		return
	}
	if ix.rejectMissingABPasskey(w, q, scope) {
		return
	}
	ix.serveQuery(w, r, q, scope)
}

// authorizeRequest applies serve's authentication policy and reports whether
// the request may proceed; on rejection the response has been written.
func (ix *Indexer) authorizeRequest(w http.ResponseWriter, r *http.Request, q url.Values) bool {
	if ix.keyUnusable {
		// Fail closed at the handler too: Run already refuses to bind with an empty or
		// unresolved feed_api_key, but a second independent guard keeps a future
		// construction path from serving the passkey-bearing feed behind an absent or
		// guessable gate - and it distinguishes "auth not configured" (this 503) from
		// "wrong key" (the 401 below).
		ix.log.Error("indexer request rejected", "reason", "feed_api_key not configured", "path", logParam(r.URL.Path))
		http.Error(w, "service unavailable: feed_api_key not configured", http.StatusServiceUnavailable)
		return false
	}
	// Constant-time verification, with the length side-channel (CWE-208) closed by
	// comparing fixed-length SHA-256 digests rather than raw strings, lives in the
	// shared library; the verifier is built once in New.
	if !ix.verifyKey.Verify(q.Get("apikey")) {
		// Volume is bounded upstream: authFailureLimiter 429s over-budget bad-key
		// requests before the access logger, so this line is capped at its rate.
		ix.log.Info("indexer request rejected", "reason", "bad apikey", "path", logParam(r.URL.Path), "client_ip", webhttp.ClientIP(r))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

// requireScope resolves the request's tracker scope and reports whether one
// was addressed; an unscoped request has been answered with the 404.
func (ix *Indexer) requireScope(w http.ResponseWriter, r *http.Request) (string, bool) {
	scope := scopeFor(r.Host, r.URL.Path)
	if scope == "" {
		ix.log.Info("indexer request rejected", "reason", "no tracker scope", "path", logParam(r.URL.Path), "host", logParam(r.Host), "client_ip", webhttp.ClientIP(r))
		http.Error(w, "not found: address a tracker feed at /nyaa or /ab", http.StatusNotFound)
		return "", false
	}
	return scope, true
}

// serveCaps answers a t=caps capabilities request, reporting whether it
// handled the request.
func (ix *Indexer) serveCaps(w http.ResponseWriter, q url.Values, scope string) bool {
	if !strings.EqualFold(strings.TrimSpace(q.Get("t")), "caps") {
		return false
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	_, _ = io.WriteString(w, renderCaps())
	ix.log.Info("indexer request", "scope", scope, "t", "caps")
	return true
}

// rejectMissingABPasskey applies serve's AB configuration guard, reporting whether
// it rejected the request. The AnimeBytes RSS feed needs the operator's passkey to
// build grabbable links, so without it a configured /ab feed has nothing to serve
// an RSS check. Answer with a Torznab error rather than an empty feed, so
// Prowlarr's save-test fails with a reason. An AB search is unaffected (it proxies
// Prowlarr), and an UNCONFIGURED AB tracker is not nudged at all.
func (ix *Indexer) rejectMissingABPasskey(w http.ResponseWriter, q url.Values, scope string) bool {
	if scope != upstreamAB || !ix.enablement.enabled(upstreamAB) || !unusableABPasskey(ix.enablement.ABPasskey) || !isFeedRequest(q) {
		return false
	}
	ix.rejectTorznab(w, scope, "ab passkey not configured", errCodeIncorrectCredentials,
		"AnimeBytes passkey not configured: set indexer.ab_passkey in seadex-scout to serve the AnimeBytes feed")
	return true
}

// serveQuery runs the tracker query and renders the feed, translating the two
// local-fault outcomes (snapshot unavailable, total upstream failure) into Torznab
// errors, then logs the one INFO line per request.
func (ix *Indexer) serveQuery(w http.ResponseWriter, r *http.Request, q url.Values, scope string) {
	items, stats, fault := ix.query(r.Context(), q, scope)
	// A request that could not answer with a feed at all arrives as a fault carrying
	// its own Torznab error text: an empty 200 feed would read as a clean no-match,
	// silently recording the fault as a successful search. One arm here means a new
	// fault cannot be forgotten into a false-empty feed.
	if fault != nil {
		ix.rejectTorznab(w, scope, fault.summary, fault.code, fault.detail)
		return
	}
	doc, rendered := renderFeed(items)
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	if _, err := io.WriteString(w, doc); err != nil {
		// The client went away or the write deadline fired mid-body: the request log's
		// `returned` count describes what was RENDERED, not what the arr received.
		if errors.Is(err, os.ErrDeadlineExceeded) {
			ix.log.Warn("indexer feed write deadline expired mid-body; the arr received a partial feed",
				"scope", scope, "rendered_items", rendered,
				"write_timeout", writeTimeout, "error", err)
		} else {
			ix.log.Debug("indexer feed write failed; client received a partial feed",
				"scope", scope, "rendered_items", rendered, "error", err)
		}
	}
	if rendered < len(items) {
		// renderFeed degraded to a truncated-but-valid document (the byte budget);
		// without this WARN the request log would falsely report the full result count
		// while the arr received a partial feed. Counts only.
		ix.log.Warn("indexer feed truncated by the render byte budget",
			"scope", scope,
			"requested_items", len(items),
			"rendered_items", rendered,
			"max_bytes", maxRenderedFeedBytes)
	}
	// One INFO line per request: the incoming Torznab params plus a result summary.
	ix.log.Info("indexer request",
		"scope", scope,
		"t", logParam(q.Get("t")),
		"q", logParam(q.Get("q")),
		"season", logParam(q.Get("season")),
		"ep", logParam(q.Get("ep")),
		"cat", logParam(q.Get("cat")),
		"answered", stats.answered,
		"feed", stats.feed,
		"upstream_fetched", stats.upstreamFetched,
		"upstream", stats.upstream,
		"curated", stats.curated,
		"identity_conflicts", stats.identityConflicts,
		"returned", rendered)
}

// scopeFor resolves which tracker's results a request targets: the URL path first,
// the Host subdomain as a fallback, or "" when neither names a tracker - which
// serve treats as 404, since there is no combined feed. Serving per-tracker lets an
// arr treat the feed as two indexers and gate each tracker's RSS/search use with
// that indexer's own flags. Two addressing styles are supported so it works
// whether seadex-scout shares a host with the arrs or sits behind a proxy.
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
// nyaa.example.com -> nyaa, ab.example.com -> ab, anything else -> "". This lets a
// reverse proxy route per-tracker subdomains to the one port with no path rewrite;
// the Host must reach the app unmodified.
func scopeFromHost(host string) string {
	canonical := webhttp.CanonicalHost(host)
	if canonical == "" {
		return ""
	}
	label, _, _ := strings.Cut(canonical, ".")
	return scopeFromToken(label)
}

// scopeFromToken maps a lowercased tracker token (a path segment or DNS
// label) to its feed scope, or "" for any non-tracker token.
func scopeFromToken(s string) string {
	if validScope(s) {
		return s
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
