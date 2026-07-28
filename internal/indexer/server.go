package indexer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/httpx/v4"
	"github.com/cplieger/webhttp"
)

const (
	shutdownGrace     = 10 * time.Second
	readHeaderTimeout = 15 * time.Second
	readTimeout       = 30 * time.Second
	idleTimeout       = 120 * time.Second
	// authFailBurst/authFailRefill tune the failed-auth throttle
	// (authFailureLimiter): 10 free wrong-key attempts, then one accrued
	// every 6s (10/min) - enough headroom for an operator fixing a
	// misconfigured arr, tight enough that a flooding LAN client is
	// silenced within a second.
	authFailBurst  = 10
	authFailRefill = 6 * time.Second
	// maxConcurrentQueries bounds simultaneous expensive queries (queryGate).
	// The worst-case footprint of ONE in-flight search is roughly one
	// upstreamMaxBytes body plus the render builder - about 16 MiB, since a
	// scoped request queries exactly one upstream (fetchRaw takes
	// upstreamForScope, and serve rejects an unscoped request) - so four
	// leaves the 256 MiB container ample headroom for
	// the compare loop that shares the process, while sitting far above what the
	// arrs actually ask for: Sonarr and Radarr search a season at a time, the
	// per-episode barrage is skipped entirely (servesQuery), and real Torznab
	// responses are ~150 KiB rather than the cap. It is a resource bound, not a
	// rate limit - authFailureLimiter bounds bad-key FLOODS, this bounds the
	// memory a legitimate burst can hold at once.
	maxConcurrentQueries = 4
	// maxConcurrentFeeds bounds simultaneous synthesized-RSS renders (feedGate),
	// separately from the search bound. An empty-query RSS check touches no
	// upstream: it reads the already-loaded snapshot and renders it, holding at
	// most maxRenderedFeedBytes of builder and no Prowlarr body. Sharing the
	// search pool let a stalled Prowlarr starve it - one search holds its slot
	// for the whole bounded retry budget writeTimeout is derived from, so four
	// slow searches answered every RSS check busy even though the RSS path needs
	// nothing from Prowlarr. Two renders add at most ~16 MiB on top of the search
	// bound, and an RSS waiter only ever queues behind other sub-millisecond
	// renders, so queryGateWait always clears.
	maxConcurrentFeeds = 2
)

// writeTimeout bounds a stalled response consumer. net/http arms it when the
// request headers are read, so it must cover EVERYTHING the handler can spend
// before the body is written: the admission wait for a concurrency slot
// (queryGateWait), then the complete bounded Prowlarr retry budget -
// upstreamMaxAttempts full-timeout attempts plus the capped Retry-After waits
// between them - plus a one-minute render margin. Omitting the admission wait
// understated the bound, so a legitimate search that queued for a slot could
// have its rendered feed cut mid-write with the only signal a Debug line
// blaming the client. Deriving it from the budget's own constants keeps the
// deadline valid when the retry policy or the per-attempt timeout changes.
// (Evaluates to 6m5s today.) A var because queryGateWait is one; it is
// evaluated once at init, so a test shortening queryGateWait does not shrink
// the deadline.
var writeTimeout = queryGateWait + upstreamMaxAttempts*UpstreamAttemptTimeout +
	(upstreamMaxAttempts-1)*httpx.RetryAfterCap + time.Minute

// queryGateWait is how long an over-limit query waits for a slot before the
// feed answers busy. Long enough to absorb a burst of near-simultaneous
// searches (the arrs fire several as one library scan reaches several series),
// short enough that a queue cannot grow behind upstreams stuck in their bounded
// retry trees - waiting the full retry budget would turn one slow Prowlarr into
// a pile of parked requests. A waiter holds only its request state, not an
// upstream body, so the wait itself is cheap.
//
// A var, not a const, ONLY so the concurrency test can exercise the
// wait-expired path without spending it in real time.
var queryGateWait = 5 * time.Second

// listenAddr is the fixed LAN bind address for the Torznab feed server. The
// port is an internal detail (the container/compose port mapping publishes
// it), not an operator-tuned setting, so it is hardcoded rather than a key.
// A var rather than a const purely as a test seam: the server lifecycle
// tests point it at an ephemeral 127.0.0.1 port so they never collide with a
// real deployment's :9118.
var listenAddr = ":9118"

// unusableFeedKey reports whether the configured feed_api_key cannot be trusted
// as the feed's authentication gate: it is absent, or it still holds a literal
// ${VAR} reference the config loader could not expand. The second case matters
// because this key IS the gate - an unexpanded placeholder is a credential a LAN
// attacker can guess from the public config.example, and the /ab RSS body's
// download links carry the operator's AnimeBytes passkey. config's
// validateIndexerEndpoints rejects both on the daemon path; these guards keep
// any other construction of Indexer from binding or serving behind them.
func unusableFeedKey(key string) bool {
	return key == "" || strings.Contains(key, "${")
}

// Run serves the Torznab endpoint from the persisted feed snapshot until ctx is
// cancelled. It first warms the feed from the last persisted snapshot (see
// warmSnapshot) - the one piece of startup work, owned by this lifecycle method
// rather than by New. The endpoint then listens immediately (so an arr's caps
// Test succeeds right away); it serves whatever feed the last compare cycle
// persisted (empty until the first cycle on a fresh install) and reloads the
// snapshot when a cycle rewrites it. It owns no health marker - the daemon that
// runs it does - so a feed failure never flips container health.
func (ix *Indexer) Run(ctx context.Context) error {
	// Fail closed at the network boundary: config.Validate (validateIndexer)
	// already rejects a configured feed with an empty or still-unresolved
	// feed_api_key on the daemon path, but any alternate construction of the
	// exported Indexer must never bind and serve the feed with a guessable or
	// absent gate - the AnimeBytes RSS feed embeds ab_passkey in its download
	// links. Field-name-only: the rejected value is a credential.
	if unusableFeedKey(ix.apiKey) {
		return errors.New("indexer: indexer.feed_api_key is empty or an unresolved ${VAR} reference; refusing to serve the Torznab feed")
	}
	// The one piece of startup work, deliberately here and not in New: all
	// background work begins under the explicit lifecycle method.
	ix.warmSnapshot()
	// Bind up front so a port-in-use error surfaces synchronously here and is
	// returned to the daemon's startIndexer, which logs it. The feed owns no
	// health marker (the compare loop does), so a bind failure never flips
	// container health.
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("indexer listen on %s: %w", listenAddr, err)
	}

	// The HTTP surface rides the shared webhttp plumbing: server bootstrap +
	// graceful shutdown here, the middleware stack in chain. WriteTimeout is
	// set (see writeTimeout): this endpoint only emits finite XML and the
	// upstream Prowlarr retry tree has a calculable upper bound, so the
	// deadline bounds stalled response consumers while leaving the bounded
	// retry budget intact.
	srv := webhttp.NewServer(ix.chain(),
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
// authenticated response. The implicit HTTP 200 is deliberate: Newznab/
// Torznab error documents ride 200 and the arrs/Prowlarr classify by the
// <error> body (that is what surfaces the description on Prowlarr's
// save-test); only torznabErrorResponder, which answers a recovered
// panic, writes a real 5xx status.
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

// chain assembles the middleware stack Run serves. Order (outermost first):
//
//   - authFailureLimiter: rejects over-budget bad-apikey requests BEFORE the
//     access logger, so a flooding or brute-forcing LAN client cannot fill
//     the slog/Loki stream at wire speed - suppressing that flood is the
//     throttle's whole point, so it must sit outside Logging. Admitted
//     wrong-key requests still fall through to serve's 401 + domain line,
//     now bounded by the bucket's rate.
//   - Logging: the standard access line (method, PATH only, status,
//     duration, request id, client_ip) - webhttp's RequestLogger logs
//     r.URL.Path and
//     never the query string, so the Torznab apikey (which arrives as a
//     query parameter) cannot leak into the access log. serve's own domain
//     line (scope/params/result counts) complements it - that line
//     whitelists the params it logs and likewise never logs apikey.
//     WithClientIP is passed with NO trusted ranges, the spoof-proof default:
//     it logs the immediate socket peer, which is the real client in the
//     supported deployment (the arrs and Prowlarr reach this port directly on
//     the container network; nothing publishes it through a reverse proxy).
//     Behind a proxy the operator would pass that proxy's CIDRs
//     (webhttp.ParseCIDRs) so a trusted X-Forwarded-For resolves the real
//     client - deliberately NOT done speculatively, since trusting a
//     forwarded header with no proxy in front of it is how the field becomes
//     spoofable. The attribute name is the fleet's `client_ip`, so a shared
//     Loki query over every webhttp consumer's access lines includes this app.
//   - Recoverer: turns a handler panic into a logged 500 rendered as a
//     Torznab <error> via torznabErrorResponder - not net/http's bare
//     connection close, and not webhttp's default JSON envelope, which is
//     the wrong wire shape for this XML endpoint. It sits inside Logging so
//     a recovered panic logs as its 500.
//   - SecurityHeaders: the innermost baseline (nosniff, X-Frame-Options:
//     DENY, Referrer-Policy, Content-Security-Policy), set before the handler
//     runs so every response - including a recovered panic's 500 - carries
//     it. Defense in depth for the credential-bearing /ab feed opened in a
//     browser; the arrs ignore all of them.
func (ix *Indexer) chain() http.Handler {
	return webhttp.Chain(ix.handler(),
		ix.authFailureLimiter(),
		webhttp.Logging(webhttp.WithLogger(ix.log), webhttp.WithClientIP()),
		webhttp.Recoverer(
			webhttp.WithRecoverLogger(ix.log),
			webhttp.WithRecoverResponder(torznabErrorResponder),
		),
		// default-src 'none' is the whole policy this endpoint needs: every
		// response is a self-contained XML or plain-text document with no
		// subresources, so an inert document is the correct browser reading of
		// the credential-bearing /ab feed. The arrs ignore the header.
		webhttp.SecurityHeaders(webhttp.WithCSP("default-src 'none'")),
	)
}

// authFailureLimiter rate-limits bad-apikey requests through a shared
// webhttp.RateLimiter token bucket (burst authFailBurst, one token accrued
// per authFailRefill). Requests presenting a correct key never consume a
// token - the predicate verifies with the same pre-hashed constant-time
// verifier serve uses - so the arrs' happy path can never be throttled, not
// even mid-flood; over-budget bad-key requests get a 429 (with a computed
// Retry-After hint) before reaching the logger or the handler. The
// empty-configured-key guard keeps serve's fail-closed 503 diagnostic
// reachable for alternate constructions (Run refuses to bind in that state,
// so it is test-only).
//
// Wire-speed key guessing remains answerable in principle (a correct guess
// is never throttled, so 200-vs-429 is an oracle); that residue is an
// accepted trade: feed_api_key is a high-entropy operator secret on a
// LAN-only bind, and the alternative - throttling verification itself -
// would let any flooding client lock out the legitimate arr. The realistic
// threats are the log flood and misconfigured-client spam, both bounded
// here.
func (ix *Indexer) authFailureLimiter() webhttp.Middleware {
	return webhttp.RateLimiter(authFailBurst, authFailRefill,
		webhttp.WithRateLimitWhen(func(r *http.Request) bool {
			return !unusableFeedKey(ix.apiKey) && !ix.verifyKey.Verify(r.URL.Query().Get("apikey"))
		}),
		webhttp.WithRateLimitError("too_many_auth_failures", "too many failed apikey attempts"),
	)
}

// serve handles the Torznab endpoint. Every request must address a specific
// tracker feed - /nyaa or /ab by path, or a nyaa.*/ab.* host; an unscoped
// request is 404 (there is no combined feed). t=caps returns capabilities,
// everything else proxies that tracker's Prowlarr endpoint filtered to SeaDex's
// curation. serve is the top-down dispatcher reading in protocol order; each
// response policy lives in its own helper.
func (ix *Indexer) serve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if !ix.authorizeRequest(w, r, q) {
		return
	}
	// Every authenticated caps/error/feed response is marked non-cacheable up
	// front: the /ab RSS body embeds the operator's AnimeBytes passkey in its
	// download links, and a browser, intermediary, or explicitly configured
	// reverse-proxy cache must never retain that credential-bearing body
	// beyond the request.
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
	if unusableFeedKey(ix.apiKey) {
		// Fail closed at the handler too: Run already refuses to bind with an
		// empty or unresolved feed_api_key, so this branch is unreachable in
		// production, but a second independent guard keeps any future
		// construction path from serving the passkey-bearing feed behind an
		// absent or guessable gate - and it is what
		// distinguishes "auth not configured" (this 503, an operator problem)
		// from "wrong key" (the 401 below). The static-token verifier itself
		// fails CLOSED on an empty configured key, so skipping this guard
		// could never open the gate; it would just misreport the unconfigured
		// state as an unauthorized caller - and for an unresolved ${VAR} it
		// would accept the guessable placeholder as a valid key.
		ix.log.Error("indexer request rejected", "reason", "feed_api_key not configured", "path", logParam(r.URL.Path))
		http.Error(w, "service unavailable: feed_api_key not configured", http.StatusServiceUnavailable)
		return false
	}
	// Constant-time verification, with the length side-channel (CWE-208)
	// closed by comparing fixed-length SHA-256 digests rather than the raw
	// strings, lives in the shared library; the verifier is built once in New
	// (pre-hashed configured key): see webhttp.NewStaticTokenVerifier.
	if !ix.verifyKey.Verify(q.Get("apikey")) {
		// Volume is bounded upstream: authFailureLimiter (see chain) 429s
		// over-budget bad-key requests before the access logger, so this
		// domain line and its 401 are capped at the bucket's rate.
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

// rejectMissingABPasskey applies serve's AB configuration guard, reporting
// whether it rejected the request. The AnimeBytes RSS feed needs the
// operator's passkey to build grabbable links, so without it a configured
// /ab feed has nothing to serve a periodic RSS check (an empty-q request).
// Answer that with a Torznab error rather than an empty feed, so Prowlarr's
// save-test fails with a clear reason and the operator sets the passkey. An
// AB search (non-empty q) is unaffected: it proxies Prowlarr, whose own link
// needs no passkey. An UNCONFIGURED AB tracker (empty ab_torznab_url, the
// README's off switch) is not nudged: it falls through to the empty feed
// (see serveQuery), the same shape as a tracker with no data.
func (ix *Indexer) rejectMissingABPasskey(w http.ResponseWriter, q url.Values, scope string) bool {
	if scope != upstreamAB || !ix.enablement.enabled(upstreamAB) || ix.enablement.ABPasskey != "" || strings.TrimSpace(q.Get("q")) != "" {
		return false
	}
	ix.rejectTorznab(w, scope, "ab passkey not configured", errCodeIncorrectCredentials,
		"AnimeBytes passkey not configured: set indexer.ab_passkey in seadex-scout to serve the AnimeBytes feed")
	return true
}

// acquireQuery reserves one of gate's expensive-work slots, waiting at most
// queryGateWait for one. It reports false when the wait expired (the caller
// answers busy) or the request's own context ended first (the client gave up;
// the caller returns without writing, since there is nobody to write to). The
// matching releaseQuery must run on every acquired path, on the SAME gate.
func (ix *Indexer) acquireQuery(ctx context.Context, gate chan struct{}) (acquired, clientGone bool) {
	select {
	case gate <- struct{}{}:
		return true, false
	default:
	}
	// Only a genuinely over-limit request pays for a timer.
	timer := time.NewTimer(queryGateWait)
	defer timer.Stop()
	select {
	case gate <- struct{}{}:
		return true, false
	case <-ctx.Done():
		return false, true
	case <-timer.C:
		return false, false
	}
}

// releaseQuery returns a slot reserved by acquireQuery on the same gate.
func (ix *Indexer) releaseQuery(gate chan struct{}) { <-gate }

// serveQuery runs the tracker query and renders the feed, translating the
// two local-fault outcomes (snapshot unavailable, total upstream failure)
// into Torznab errors, then logs the one INFO line per request.
//
// The whole expensive body runs under the concurrency gate (see queryGate): the
// upstream fetch, the decode, and the render are what a burst could stack into
// an OOM, so the slot is held across all three and released before the request
// returns. A request servesQuery declines is exempt - it performs no snapshot
// read and no upstream call, so it cannot contribute to the footprint the gate
// bounds.
func (ix *Indexer) serveQuery(w http.ResponseWriter, r *http.Request, q url.Values, scope string) {
	// Only a request that will actually do the expensive work takes a slot.
	// query answers a per-episode query with nothing WITHOUT touching the
	// snapshot or an upstream (servesQuery), so gating it would answer
	// Sonarr's per-episode barrage - the highest-volume request class, one
	// query per episode per scene-title alias - with a Torznab <error>
	// during a burst instead of the cheap empty feed the skip promises, and
	// an <error> is a FAILED search to the arr (it counts toward the
	// indexer-failure escalation that disables the indexer, RSS included).
	if servesQuery(q) {
		// A synthesized-RSS check reads only local state, so it takes its own
		// pool: it must stay servable while every search slot is parked in a
		// bounded Prowlarr retry tree. pool names which bound was hit: the two
		// have different causes (a stalled Prowlarr retry tree vs simultaneous
		// local renders) and different operator actions, and the limit value
		// alone stops distinguishing them the moment either constant is
		// retuned.
		gate, limit, pool := ix.queryGate, maxConcurrentQueries, "search"
		if isFeedRequest(q) {
			gate, limit, pool = ix.feedGate, maxConcurrentFeeds, "rss"
		}
		acquired, clientGone := ix.acquireQuery(r.Context(), gate)
		if clientGone {
			// The arr hung up while waiting; writing a response would only log a
			// failed write. The access line still records the request.
			ix.log.Debug("indexer request abandoned while waiting for a query slot", "scope", scope)
			return
		}
		if !acquired {
			// Busy, not broken: a Torznab <error> (the endpoint's own wire shape,
			// unlike webhttp's JSON 429 envelope) plus a Retry-After so the arr
			// backs off rather than treating the feed as failed. WARN, not ERROR:
			// the condition clears itself as in-flight searches finish, which is
			// exactly the transient/self-healing side of the level rule.
			w.Header().Set("Retry-After", strconv.Itoa(int(queryGateWait.Seconds())))
			ix.log.Warn("indexer at its concurrent-query limit; request answered busy",
				"scope", scope, "pool", pool, "limit", limit, "waited", queryGateWait)
			ix.rejectTorznab(w, scope, "concurrent query limit reached", errCodeUnknown,
				"too many concurrent requests in flight; retry shortly")
			return
		}
		defer ix.releaseQuery(gate)
	}

	items, stats, fault := ix.query(r.Context(), q, scope)
	// A request query could not answer with a feed at all (the persisted
	// snapshot failed to load before any snapshot was installed, or every
	// queried Prowlarr upstream failed) arrives as a fault carrying its own
	// Torznab error text: an empty 200 feed would read as a clean no-match to
	// the arr, silently recording the fault as a successful search. One arm
	// here means a new fault cannot be forgotten into a false-empty feed.
	if fault != nil {
		ix.rejectTorznab(w, scope, fault.summary, fault.code, fault.detail)
		return
	}
	doc, rendered := renderFeed(items)
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	if _, err := io.WriteString(w, doc); err != nil {
		// The client went away or the write deadline fired mid-body: the
		// request log's `returned` count below describes what was RENDERED,
		// not what the arr received, so record the partial delivery. Debug,
		// not Warn - an arr cancelling a slow search is routine.
		ix.log.Debug("indexer feed write failed; client received a partial feed",
			"scope", scope, "rendered_items", rendered, "error", err)
	}
	if rendered < len(items) {
		// renderFeed degraded to a truncated-but-valid document (the byte
		// budget); without this WARN the only request log would falsely
		// report the full result count while the arr silently received a
		// partial feed. Counts only - no item fields or URLs.
		ix.log.Warn("indexer feed truncated by the render byte budget",
			"scope", scope,
			"requested_items", len(items),
			"rendered_items", rendered,
			"max_bytes", maxRenderedFeedBytes)
	}
	// One INFO line per request: the incoming Torznab params plus a result
	// summary. `answered` is false when the feed deliberately skips a per-episode
	// query (so an empty result reads as a skip, not a no-match); `feed` is true
	// for an empty-q RSS check served from the synthesized SeaDex feed;
	// `upstream_fetched` is how many results the upstream page carried BEFORE the
	// Prowlarr fetch's download-URL origin filter and `upstream` how many survived
	// it (a gap between them is that filter dropping items) for a search,
	// `curated` how many items survived curation/synthesis (pre cat-filter/paging), `returned`
	// the count actually EMITTED into the rendered document (the render byte
	// budget can truncate below the post-category-filter count).
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
		"returned", rendered)
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
//
// The authority is canonicalized through webhttp.CanonicalHost - the shared
// strict parser the rest of the app's HTTP stack already uses - rather than by
// splitting the raw Host on its first dot. The raw split does not actually parse
// an authority: it admitted malformed values ("ab..example", "ab.example.com:")
// as the AB scope, and, worse, a bare tracker host carrying a port ("ab:9118" -
// the shape a direct LAN client uses) failed to select any scope because the port
// rode along in the label. CanonicalHost lowercases, strips the port and IPv6
// brackets, allows at most one trailing FQDN dot, and returns "" for anything
// outside the RFC 3986 authority grammar, so a second divergent Host
// interpretation no longer sits beside the shared stack's (l-f25).
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
