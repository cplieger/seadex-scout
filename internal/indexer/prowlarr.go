package indexer

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cplieger/httpx/v4"
	"github.com/cplieger/seadex-scout/internal/appinfo"
	"github.com/cplieger/seadex-scout/internal/credname"
	"github.com/cplieger/seadex-scout/internal/displaylink"
	"github.com/cplieger/urlform"
)

const (
	// upstreamMaxAttempts / upstreamBaseDelay bound the per-query retry.
	upstreamMaxAttempts = 3
	upstreamBaseDelay   = time.Second
	// upstreamMaxBytes bounds a single Torznab response before decode. 8 MiB
	// deliberately rejects pathological escape-heavy documents before decode:
	// 4 MiB of decoded ampersands can require about 20 MiB on the wire. Real
	// responses are ~150 KiB. The tighter cap also bounds the one allocation the
	// decode caps cannot: encoding/xml materializes a start element's whole
	// attribute slice per token at ~10x per-attr overhead (CWE-400).
	upstreamMaxBytes = 8 << 20
	// minEmbeddedSecretLen is the length from which a query VALUE on the
	// configured feed URL is treated as a credential by upstreamSecrets even
	// though its parameter NAME says nothing about credentials. It matches the
	// floor config already applies to indexer.feed_api_key, so a structural
	// value ("?indexer=1") is never substituted out of a diagnostic while an
	// unlabelled 32-hex token still is. A credential-NAMED parameter is a
	// secret at any length (credentialParamName), so this floor never gates
	// the shapes config actually warns about.
	minEmbeddedSecretLen = 16
)

// upstreamAttemptTimeout bounds ONE Prowlarr Torznab attempt: fetchAndParse
// derives each attempt's context from it (see there), so the package enforces
// its own retry arithmetic instead of relying on the composition root to wire
// an http.Client.Timeout that matches. server.go's writeTimeout is derived
// from the same value, which keeps the write deadline sized above the whole
// retry tree by construction. It is package-private for that reason: the root
// still owns the CLIENT (the X-Api-Key header rides redirects, so the redirect
// policy must stay there - see wireUpstreams), but it owes this package no
// knowledge of the per-attempt budget; its client timeout is only a transport
// backstop above this one.
//
// A var, not a const, ONLY so the test that pins this package-owned deadline
// can exercise it without spending a minute in real time (the same reason
// queryGateWait is one). writeTimeout is evaluated at init, so shortening it
// in a test does not shrink the write deadline.
var upstreamAttemptTimeout = 60 * time.Second

// --- Upstream search and retry classification ---

// upstream is one Prowlarr per-indexer Torznab endpoint (Nyaa or AnimeBytes).
// The feed proxies these to source real release data (title, seeders, size,
// Prowlarr-proxied download URL) and never talks to the trackers directly.
type upstream struct {
	http   *http.Client
	log    *slog.Logger
	name   string
	feed   string
	apiKey string
	// dropWarned / displayWarned bound the two filterDownloadURLs
	// diagnostics to one WARN per onset (plus one recovery INFO), so a
	// SYSTEMATIC condition - a Prowlarr endpoint whose emitted download
	// links sit on a different origin than the configured Torznab URL, or
	// an upstream emitting non-tracker display URLs - cannot WARN once per
	// query. The title harvest admits up to harvestTimeBudget /
	// harvestQueryInterval (~300) queries per rebuild, every rebuild,
	// while such a condition persists. Atomic because the server's
	// upstreams are shared across concurrent requests.
	dropWarned    atomic.Bool
	displayWarned atomic.Bool
}

// newUpstream builds one Prowlarr per-indexer Torznab endpoint. It is the ONE
// construction site for the type, and wireUpstreams is its only caller, so
// every consumer's upstreams are built the same way from the same fields.
func newUpstream(client *http.Client, log *slog.Logger, name, feed, apiKey string) *upstream {
	return &upstream{http: client, log: log, name: name, feed: feed, apiKey: apiKey}
}

// search queries the Torznab endpoint with the forwarded params and returns the
// parsed items. The Prowlarr API key is sent as the X-Api-Key header (not a
// query param), so it never appears in a logged request URL. It returns the
// filtered items plus the RAW parsed-item count of the response (before
// filterDownloadURLs), so the request log line's upstream_fetched reports what
// the upstream actually returned, not what survived the origin
// filter.
//
// The retry boundary encloses the WHOLE attempt - transport, status, bounded
// body read, AND the Torznab decode - so a transient truncated or malformed
// 200 response participates in the same bounded budget as a failed request
// (the query is an idempotent GET). Exactly one layer owns multiple attempts:
// the outer Do runs upstreamMaxAttempts total, and fetchAndParse
// performs exactly one bounded GET per call, so there is no nested retry
// explosion. A 429's capped Retry-After survives as a RetryAfterHint on the
// transient error, so Do waits the upstream-requested delay
// instead of its jittered backoff.
func (u *upstream) search(ctx context.Context, params url.Values) ([]item, int, error) {
	parsed, err := url.Parse(u.feed)
	if err != nil {
		// Deliberately NOT wrapped: a *url.Error echoes the raw configured
		// URL, which may carry a username-only userinfo token
		// (validateHTTPURL accepts one), and this error reaches httpx.Do's
		// retry logger - the same redaction stance the StatusError path
		// below applies (CWE-532).
		return nil, 0, errors.New("invalid upstream feed URL")
	}
	// Merge the Torznab params into RawQuery component-wise: appending to the
	// raw string would land them after any fragment on the configured
	// endpoint, where net/http strips them before sending.
	if enc := params.Encode(); enc != "" {
		if parsed.RawQuery != "" {
			parsed.RawQuery += "&"
		}
		parsed.RawQuery += enc
	}
	reqURL := parsed.String()

	items, err := httpx.Do(ctx,
		func(ctx context.Context) ([]item, error) {
			return u.fetchAndParse(ctx, reqURL)
		},
		httpx.WithMaxAttempts(upstreamMaxAttempts),
		httpx.WithBaseDelay(upstreamBaseDelay),
		httpx.WithLabel("torznab "+u.name),
		// Route the retry loop's own Debug lines through the upstream's
		// component logger so they carry component=indexer instead of
		// falling through to slog.Default().
		httpx.WithLogger(u.log),
		// Demote httpx's terminal "retries exhausted" line to Debug. BOTH
		// callers of search publish their own WARN for the same failed query
		// with strictly more context - the harvest names the show, the query
		// shape and the page and drives its latch state (classifyHarvestError),
		// the request path names the upstream (query.go's fetchRaw) - so leaving
		// httpx's verdict at Warn produced two terminal WARNs per failure, six per
		// three-show homogeneous malformed run, and doubled the Loki volume of
		// exactly the incident the once-per-onset cadence exists to keep
		// readable. Demoting rather than dropping the logger keeps the
		// per-attempt retry diagnostics, which are the half worth having here.
		httpx.WithExhaustedLevel(slog.LevelDebug))
	if err != nil {
		return nil, 0, err
	}
	return u.filterDownloadURLs(items), len(items), nil
}

// fetchAndParse performs ONE search attempt: a single bounded HTTP fetch
// followed by the Torznab decode. The attempt's own deadline
// (upstreamAttemptTimeout) is derived HERE, so the per-attempt bound the retry
// budget is sized against is enforced by the package that owns that budget
// rather than by an unenforced obligation on whoever built the client; a
// client-level http.Client.Timeout may still sit above it as a transport
// backstop. Classification keeps reading the CALLER's context (attemptError
// takes it, not the attempt context), which is exactly the split it documents:
// caller context still live means the attempt timer fired, hence retryable.
//
// Errors the enclosing Do should
// retry are marked transient: a 408/429/5xx status (with the response's capped
// Retry-After carried as the transient error's RetryAfterHint, so the outer
// loop honors the upstream-requested delay), a garbled/truncated 2xx body,
// and a Torznab <error> document carrying a generic/server-side code (e.g.
// 900). A Torznab <error> document naming a deterministic auth/account
// (100-199) or request/parameter (200-299) code is terminal - retrying
// cannot recover a credentials or request-validation failure
// (terminalTorznabCode). Transient transport errors (timeouts, resets, DNS)
// already classify via httpx.IsTransient through the returned chain;
// anything else (a non-retryable 4xx, an unparseable URL) stays terminal.
func (u *upstream) fetchAndParse(ctx context.Context, reqURL string) ([]item, error) {
	// GetBytes owns the one-attempt HTTP mechanics: request construction, header
	// injection, transport-error reduction, non-2xx drain + *StatusError, the
	// Retry-After parse, and the bounded body read. WithMaxAttempts(1) keeps the
	// attempt budget single-owner - the enclosing typed Do runs the retries, so
	// letting GetBytes retry too would multiply the two budgets - which makes
	// attemptError, not GetBytes, the owner of every retry decision here.
	//
	// The attempt context bounds the whole attempt (connect, headers, and the
	// bounded body read); attemptError below is deliberately handed the CALLER's
	// ctx so an expired attempt timer stays retryable while an expired caller
	// context stays terminal.
	attemptCtx, cancel := context.WithTimeout(ctx, upstreamAttemptTimeout)
	defer cancel()
	body, err := httpx.GetBytes(attemptCtx, u.http, reqURL,
		httpx.WithMaxAttempts(1),
		httpx.WithHeaders(u.setHeaders),
		httpx.WithMaxBodyBytes(upstreamMaxBytes),
		httpx.WithLogger(u.log))
	if err != nil {
		return nil, u.attemptError(ctx, err)
	}
	items, err := parseTorznab(body)
	if err != nil {
		return nil, u.classifyParseError(err)
	}
	return items, nil
}

// retryableUpstreamStatus reports whether an upstream status is worth another
// attempt from the enclosing Do. It mirrors GetBytes's own retryable set
// (408/429/5xx); the app restates it because GetBytes deliberately does not mark
// its exhaustion error Transient - after WithMaxAttempts(1) that decision is the
// caller's policy, and only this loop knows the budget it owns.
func retryableUpstreamStatus(code int) bool {
	return code == http.StatusRequestTimeout ||
		code == http.StatusTooManyRequests ||
		(code >= 500 && code < 600)
}

// attemptError maps a GetBytes failure onto the retry taxonomy the enclosing
// Do reads. GetBytes runs with WithMaxAttempts(1), so its every failure exit
// arrives here and this function alone decides whether the search spends
// another of its upstreamMaxAttempts.
//
// Three classes:
//
//   - A non-2xx surfaces as *httpx.StatusError. A self-healing status
//     (retryableUpstreamStatus) becomes transient, carrying the response's
//     already-capped Retry-After forward so Do waits the upstream-requested
//     delay instead of its jittered backoff; GetBytes exposes that hint on its
//     exhaustion error via the httpx.RetryAfterHint interface, which it
//     deliberately does not pair with Transient (the retry decision is the
//     caller's). Any other status (auth/config 4xx) stays terminal and fails
//     the search on the first attempt.
//   - A per-attempt deadline. fetchAndParse gives every attempt a context
//     bounded by upstreamAttemptTimeout (and the root's client may carry a
//     looser http.Client.Timeout as a transport backstop); when either fires
//     the error matches
//     context.DeadlineExceeded - which httpx.IsTransient deliberately treats as
//     TERMINAL before consulting net.Error or the Transient interface, because a
//     caller's expired context must never be retried. That collapsed the
//     documented three-attempt budget to one attempt whenever the attempt timer
//     (not the caller's context) expired: an interactive search failed
//     immediately and the title harvest latched the tracker scope for the whole
//     rebuild. The caller's own context is the terminal signal, so the split is
//     on ctx.Err(): still live means the attempt timer fired, which
//     is retryable. The replacement error deliberately does NOT wrap
//     context.DeadlineExceeded (that identity is what IsTransient rejects
//     first); it carries a fixed log-safe message instead. An actually expired
//     caller context falls through unchanged and stays single-attempt.
//   - Everything else (transport resets, DNS, an over-cap body) passes through
//     unchanged for httpx.IsTransient to classify through the error chain.
//
// No app-side URL scrub is needed on any path: httpx's redactor drops the whole
// userinfo component and REDACTs every query value, so the username-only
// Prowlarr token that validateHTTPURL accepts cannot reach a log line through
// *StatusError (CWE-532).
func (u *upstream) attemptError(ctx context.Context, err error) error {
	if statusErr, ok := errors.AsType[*httpx.StatusError](err); ok {
		if !retryableUpstreamStatus(statusErr.Code) {
			return statusErr
		}
		// Unwrap to the status itself: GetBytes's "retries exhausted after 0s"
		// prefix is noise for a budget the app deliberately set to one attempt.
		return &transientUpstreamError{err: statusErr, retryAfter: retryAfterHint(err)}
	}
	if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return &transientUpstreamError{err: errors.New("upstream request timed out")}
	}
	return httpx.LogSafeError(err)
}

// retryAfterHint returns the capped Retry-After an upstream asked for, or zero
// when it named none. The hint rides the error chain as an
// httpx.RetryAfterHint implementer (GetBytes attaches it to the exhaustion
// error), and it is already bounded by httpx.RetryAfterCap at the parse, so it
// satisfies the interface's pre-capped contract on the way back out.
func retryAfterHint(err error) time.Duration {
	var h httpx.RetryAfterHint
	if errors.As(err, &h) {
		return h.RetryAfterHint()
	}
	return 0
}

// upstreamSecrets returns every credential the app TRANSMITS to this upstream
// and must therefore keep out of an error message or log line (CWE-532): the
// X-Api-Key header value, plus any credential embedded in the CONFIGURED feed
// URL. config.validateHTTPURL deliberately accepts both embedded shapes
// (urlEmbedsCredential only WARNs), and both reach the upstream: Go's
// http.Client turns userinfo into an Authorization: Basic header on the
// outgoing request, and a query value rides the request URL. A compromised or
// spoofed Prowlarr can therefore reflect either one back inside an <error>
// document or a decode-error message, so both belong in the same
// exact-substring redaction the header key already gets.
//
// The query is scanned on the RAW query string, split on both '&' and ';': that
// is the string the outgoing request carries, and it is a strict superset of
// url.Values (which discards a whole semicolon-delimited pair, leaving its value
// transmitted but uncollected). A value is a secret when its parameter name is
// credential-like at ANY length, or when an unlabelled value is long enough to
// be a token (minEmbeddedSecretLen); both the raw and the percent-decoded form
// are registered, since a reflection can echo either.
func (u *upstream) upstreamSecrets() []string {
	secrets := []string{u.apiKey}
	parsed, err := url.Parse(u.feed)
	if err != nil {
		return secrets
	}
	if parsed.User != nil {
		secrets = append(secrets, userinfoSecrets(parsed.User)...)
	}
	for _, pair := range strings.FieldsFunc(parsed.RawQuery, isRawQuerySeparator) {
		name, value, hasValue := strings.Cut(pair, "=")
		if !hasValue {
			// A pair with no '=' carries no name/value split at all: the whole
			// token is what rides the outgoing request, so treat it as an
			// UNLABELLED value (credentialParamName has no name to judge, so the
			// minEmbeddedSecretLen floor alone decides - a structural flag like
			// "?rss" stays out, a bare token does not).
			name, value = "", name
		}
		if value == "" {
			continue
		}
		if !credentialParamName(name) && len(value) < minEmbeddedSecretLen {
			continue
		}
		secrets = append(secrets, value)
		if decoded, err := url.QueryUnescape(value); err == nil && decoded != value {
			secrets = append(secrets, decoded)
		}
	}
	return secrets
}

// userinfoSecrets returns every wire representation of a credential embedded in
// the configured feed URL's userinfo. Userinfo is a credential POSITION by
// construction; length is irrelevant there, and mangling an unlucky diagnostic
// is the safe direction. The plaintext components alone are NOT enough:
// net/http never transmits them verbatim - it sends
// "Authorization: Basic base64(user:pass)" - so a hostile upstream reflecting
// that header (or the bare token) back inside an <error> document would escape
// an exact-substring scrub that only knows the plaintext. Both encoded forms are
// registered, longest first, so the full header is replaced before its token
// substring is.
func userinfoSecrets(user *url.Userinfo) []string {
	username := user.Username()
	password, _ := user.Password()
	token := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	return []string{username, password, "Basic " + token, token}
}

// isRawQuerySeparator reports whether r separates two raw query pairs. ';' is
// included because config's own credential scan treats it as a separator
// (urlform.RawQueryNames): net/url discards such a pair wholesale while the raw
// query - credential and all - still rides the outgoing request.
func isRawQuerySeparator(r rune) bool {
	return r == '&' || r == ';'
}

// credentialParamName reports whether a query-parameter name (possibly
// percent-encoded) names a credential, so its value is a secret regardless of
// length. The name vocabulary is internal/credname's - the same leaf
// internal/config's operator warning reads its exact-match list from - and this
// consumer takes the deliberately BROADER ContainsWord policy over it: a name
// config adds later is already covered here rather than silently unredacted.
// Over-matching only costs a mangled diagnostic; under-matching writes a
// credential to Loki (CWE-532). A shared leaf rather than reaching into
// internal/config, which this package deliberately does not depend on (only the
// composition root imports config).
func credentialParamName(name string) bool {
	decoded, err := url.QueryUnescape(name)
	if err != nil {
		decoded = name
	}
	return credname.ContainsWord(decoded)
}

// redactSecrets removes every credential this upstream carries from untrusted
// upstream text. It runs on the error path only, and BEFORE the text is
// bounded, so each exact-substring replacement always sees the intact
// credential. An empty secret is a no-op (httpx.RedactSecretString).
func (u *upstream) redactSecrets(s string) string {
	for _, secret := range u.upstreamSecrets() {
		s = httpx.RedactSecretString(s, secret)
	}
	return s
}

// classifyParseError maps a parseTorznab failure onto the retry taxonomy.
// A syntactically valid Torznab <error> document (upstreamDocError:
// bad credentials, a named indexer failure) is a deliberate
// upstream-scoped answer, not a garbled body, so it never carries
// the malformedBody marker - after the search fails, the harvest
// latches the failed scope instead of treating an upstream-wide
// auth/config failure as one show's poison result set. Its
// retryability splits on the numeric Torznab code: a deterministic
// auth/account (100-199) or request/parameter (200-299) error
// cannot recover without a config change, so it returns terminal
// and the enclosing Do fails fast; a generic/server-side or
// unparseable code stays transient within the bounded budget.
func (u *upstream) classifyParseError(err error) error {
	if docErr, ok := errors.AsType[*upstreamDocError](err); ok {
		// The document's code/description are attacker-influenced text
		// and the request carried the Prowlarr API key: a compromised
		// upstream could reflect the key into the error message, which
		// httpx.Do's retry logger and the harvest WARN would then expand
		// into the log stream (CWE-532). Classify on the ORIGINAL code
		// first, then redact any reflection of the key from both fields
		// before the error escapes this function. The fields are RAW
		// (untruncated) here - upstreamDocError.Error() sanitizes at the
		// emit boundary - so the exact-substring replacement always sees
		// the intact key.
		terminal := terminalTorznabCode(docErr.codeNum)
		docErr.code = u.redactSecrets(docErr.code)
		docErr.description = u.redactSecrets(docErr.description)
		if terminal {
			return docErr
		}
		return &transientUpstreamError{err: err}
	}
	if limitErr, ok := errors.AsType[*torznabLimitError](err); ok {
		// App-controlled message; keep it verbatim.
		return &transientUpstreamError{err: limitErr, malformedBody: true}
	}
	// A generic decode failure can echo attacker-controlled body text
	// verbatim (encoding/xml returns raw strconv errors quoting the full
	// unparsed <size>/length value, up to the wire cap upstreamMaxBytes), and the
	// request carried the Prowlarr API key: redact any reflection of the
	// key FIRST (the exact-substring replacement must see intact text),
	// then bound the text, before the error reaches httpx.Do's retry
	// logger or fetchRaw's WARN - the same emit-boundary policy the
	// upstreamDocError path applies.
	msg := sanitizeUpstreamText(u.redactSecrets(err.Error()))
	return &transientUpstreamError{err: errors.New(msg), malformedBody: true}
}

// terminalTorznabCode reports whether a Torznab <error> document's parsed
// code (upstreamDocError.codeNum, -1 for non-numeric) names a deterministic
// failure a retry cannot recover: the Newznab error ranges 100-199 (incorrect
// credentials, account problems) and 200-299 (missing or invalid request
// parameters) stay wrong on every attempt until the operator fixes
// configuration, so retrying only multiplies upstream load and warning noise
// while delaying the error. Generic/server-side codes (e.g. 900 "unknown
// error") and a non-numeric code are NOT terminal: they may recover, and an
// unknown shape defaults to the bounded retry rather than failing fast.
func terminalTorznabCode(n int) bool {
	return n >= 100 && n < 300
}

// transientUpstreamError marks an upstream failure retryable for
// httpx.Do (via the httpx.Transient interface): a retryable
// status or a malformed successful body, neither of which IsTransient
// classifies on its own. retryAfter, when positive, is the upstream's parsed
// and capped Retry-After (httpx parses it, so it can never exceed
// httpx.RetryAfterCap), exposed through RetryAfterHint so Do
// waits the upstream-requested delay instead of its jittered backoff.
// malformedBody distinguishes the decode failure of a SUCCESSFUL (2xx)
// response from the status/transport failures: after retry exhaustion the
// harvest treats a persistently malformed body as specific to one show's
// result set (malformedUpstreamBody), never as evidence the upstream itself
// is down. A valid Torznab <error> document (upstreamDocError) with a
// retryable generic/server-side code is the one 2xx parse failure that stays
// UNMARKED: it is an upstream-scoped answer, not a garbled body. A doc error
// with a deterministic auth/request code never reaches this wrapper at all -
// it returns terminal from fetchAndParse (terminalTorznabCode).
type transientUpstreamError struct {
	err           error
	retryAfter    time.Duration
	malformedBody bool
}

func (e *transientUpstreamError) Error() string                 { return e.err.Error() }
func (e *transientUpstreamError) Unwrap() error                 { return e.err }
func (e *transientUpstreamError) IsTransient() bool             { return true }
func (e *transientUpstreamError) RetryAfterHint() time.Duration { return e.retryAfter }

// malformedUpstreamBody reports whether err is (or wraps) the decode failure
// of a successful upstream response: the query reached the upstream and it
// answered 2xx, so the failure is scoped to the one result set that would not
// parse, not to the upstream's availability. Status failures (429/5xx,
// auth/config 4xx), transport errors, and a valid Torznab <error> document
// delivered with HTTP 200 (an upstream-scoped answer - see upstreamDocError)
// never carry the marker.
func malformedUpstreamBody(err error) bool {
	if tue, ok := errors.AsType[*transientUpstreamError](err); ok {
		return tue.malformedBody
	}
	// GetBytes reads the body only after its status check admitted a 2xx, so
	// an over-cap body is by construction the read failure of a SUCCESSFUL
	// response - the same class as a garbled or over-cardinality one, and
	// scoped to the one result set that came back too large rather than to
	// the upstream's availability. It stays TERMINAL (no transient wrapper,
	// so a deterministic overflow still does not burn the retry budget and
	// the single-attempt contract is unchanged); only the harvest's
	// show-vs-scope attribution changes.
	_, tooLarge := errors.AsType[*httpx.ResponseTooLargeError](err)
	return tooLarge
}

// --- Download/display URL gates ---

// filterDownloadURLs drops items whose download URL is not an absolute http(s)
// URL on the same origin as the configured Prowlarr Torznab endpoint. The
// curation lookup only proves an identifier is in the SeaDex snapshot; it does
// not bind the download target, so a tampered Prowlarr response could
// otherwise pair a curated id with an internal or attacker-controlled URL the
// arr then fetches as a curated release (SSRF / arbitrary download, CWE-918).
// A healthy Prowlarr hands out its own proxy links on the queried endpoint's
// origin, so same-origin is the safe default; the rejected URL itself is never
// logged.
func (u *upstream) filterDownloadURLs(items []item) []item {
	feedURL, err := url.Parse(u.feed)
	if err != nil {
		// An unparseable configured endpoint cannot anchor the origin check;
		// fail closed rather than passing unvalidated download targets through.
		u.log.Warn("upstream feed URL unparseable; dropping all items", "upstream", u.name)
		return nil
	}
	out := make([]item, 0, len(items))
	dropped := 0
	blankedDisplay := 0
	// observedDisplay counts the display-URL fields actually INSPECTED on the
	// surviving items. A page of items carrying neither an InfoURL nor a GUID
	// observes nothing about the display gate, so it must not be allowed to
	// clear displayWarned - the same reasoning the len(items) == 0 early
	// return already applies to the page as a whole.
	observedDisplay := 0
	for i := range items {
		if !sameHTTPOrigin(items[i].DownloadURL, feedURL) {
			dropped++
			continue
		}
		observed, blanked := u.sanitizeItemDisplayURLs(&items[i])
		observedDisplay += observed
		blankedDisplay += blanked
		out = append(out, items[i])
	}
	if len(items) == 0 {
		// An empty page observes nothing: neither an onset nor a recovery.
		// Clearing dropWarned/displayWarned here would let a title-harvest
		// query that matched no results re-arm the WARN, so a persisting
		// systematic condition would still WARN once per non-empty page -
		// the flood the onset gate exists to bound.
		return out
	}
	u.reportDroppedDownloadURLs(dropped, len(out), feedURL)
	if observedDisplay > 0 {
		// The display gate's observation set is the display-URL FIELDS actually
		// inspected, not the input page and not the surviving item count: when
		// every item is dropped for an off-origin download URL, or when the
		// survivors carry neither an InfoURL nor a GUID, no display URL was
		// inspected at all. Reporting a zero blanked count there would clear
		// displayWarned and announce a recovery on the strength of a page
		// nothing was observed on, letting a persistent display-URL fault
		// re-arm a WARN on the next surviving bad item.
		u.reportBlankedDisplayURLs(blankedDisplay, len(out))
	}
	return out
}

// sanitizeItemDisplayURLs sanitizes one surviving item's two passthrough
// display-URL fields in place, returning how many of them were INSPECTED and
// how many were blanked (the counts filterDownloadURLs' onset ladder reads).
//
// The display-URL fields are not fetch targets, but the arr renders <comments>
// as the item's clickable info link and a URL that parses to no tracker key
// skips the curation gate entirely, so a tampered upstream could attach a
// javascript:/data: or foreign-host link to a legitimately curated item. Blank
// (never drop) anything that is not a userinfo-free absolute http(s) URL on
// this upstream's own tracker host: a healthy Prowlarr always hands out the
// served tracker's canonical page URLs here. Display sanitization is
// independent of key extraction - a URL that fails this gate is blanked even
// when a tracker key could still be derived from it (e.g. a scheme-relative
// //host/... form), leaving such an item to match by info hash alone, which
// fails closed for a URL shape a healthy Prowlarr never emits.
func (u *upstream) sanitizeItemDisplayURLs(it *item) (observed, blanked int) {
	for _, field := range []*string{&it.InfoURL, &it.GUID} {
		if *field == "" {
			continue
		}
		observed++
		if s := sanitizeDisplayURL(u.name, *field); s != *field {
			blanked++
			*field = s
		}
	}
	return observed, blanked
}

// reportDroppedDownloadURLs logs the origin-filter outcome: the first
// transition into dropping items WARNs, subsequent dropping rounds stay at
// Debug, and the first clean round after a dropping one logs the recovery.
// The rejected URL itself is never logged.
func (u *upstream) reportDroppedDownloadURLs(dropped, kept int, feedURL *url.URL) {
	const msg = "upstream items dropped: download URL not on the Prowlarr endpoint origin"
	if dropped == 0 {
		if u.dropWarned.CompareAndSwap(true, false) {
			u.log.Info("upstream download URLs back on the Prowlarr endpoint origin", "upstream", u.name)
		}
		return
	}
	log := u.log.Debug
	if u.dropWarned.CompareAndSwap(false, true) {
		log = u.log.Warn
	}
	log(msg, "upstream", u.name, "dropped", dropped, "kept", kept,
		"expected_origin", feedURL.Scheme+"://"+feedURL.Host)
}

// reportBlankedDisplayURLs logs the display-URL sanitization outcome on the
// same first-transition-WARN ladder as reportDroppedDownloadURLs. Counts only:
// the rejected value is never logged (it can be attacker-shaped text).
func (u *upstream) reportBlankedDisplayURLs(blanked, kept int) {
	const msg = "upstream display URLs blanked: not the tracker's own canonical http(s) page URL"
	if blanked == 0 {
		if u.displayWarned.CompareAndSwap(true, false) {
			u.log.Info("upstream display URLs back on the tracker's canonical host", "upstream", u.name)
		}
		return
	}
	log := u.log.Debug
	if u.displayWarned.CompareAndSwap(false, true) {
		log = u.log.Warn
	}
	log(msg, "upstream", u.name, "blanked", blanked, "kept_items", kept)
}

// isHTTPScheme reports whether scheme is http or https, case-insensitively.
func isHTTPScheme(scheme string) bool {
	s := strings.ToLower(scheme)
	return s == "http" || s == "https"
}

// httpNoUserinfoURL parses raw and returns it when it is an absolute
// http or https URL free of userinfo. Its one consumer is sameHTTPOrigin,
// which gates a FETCH target and therefore stays on net/url (the parser of
// record for what the HTTP client will dial); the two DISPLAY gates -
// sanitizeDisplayURL here and snapshotInfoURLAllowed in reload.go - read
// their structural facts from urlform instead, because a browser is their
// parser of record. Anything else returns nil, false.
func httpNoUserinfoURL(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil {
		return nil, false
	}
	if !isHTTPScheme(u.Scheme) {
		return nil, false
	}
	return u, true
}

// sameHTTPOrigin reports whether raw is an absolute http or https URL, free of
// userinfo, whose ORIGIN matches origin's: scheme and hostname compared
// case-insensitively, and the EFFECTIVE port compared after defaulting an
// omitted port to the scheme's (80 for http, 443 for https). Comparing the
// serialized authority verbatim would read
// "https://prowlarr.example" and "https://prowlarr.example:443" as different
// origins, and a reverse proxy or an external-URL setting can legitimately add
// or drop the default port - which would drop every Prowlarr proxy link and
// answer the arr a successful EMPTY feed while the upstream held curated
// releases. A non-default port difference still rejects.
func sameHTTPOrigin(raw string, origin *url.URL) bool {
	parsed, ok := httpNoUserinfoURL(raw)
	if !ok {
		return false
	}
	if !strings.EqualFold(parsed.Scheme, origin.Scheme) {
		return false
	}
	if !strings.EqualFold(parsed.Hostname(), origin.Hostname()) {
		return false
	}
	return effectiveHTTPPort(parsed) == effectiveHTTPPort(origin)
}

// effectiveHTTPPort returns u's explicit port, or its http(s) scheme default
// when the authority omits one. Any other scheme yields the empty string, which
// still compares equal to itself - sameHTTPOrigin has already required an
// http(s) scheme on both sides.
func effectiveHTTPPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

// httpDisplayHost admits a raw URL as a browser-destined DISPLAY link and
// returns its host evidence: an absolute http(s) form, free of userinfo and of
// the smuggling shapes a browser reads differently from net/url. It is the
// shared admission prefix of BOTH its consumers: sanitizeDisplayURL
// (search-path display links) and trackerKeyFromURL (match.go, the curation
// IDENTITY gate), so relaxing it changes what mints a curation key, not only
// what renders as a clickable link.
//
// The structural legs are internal/displaylink's, the app's one home for that
// vouch step (h-f13) - shared with internal/trackerlink's publisher and with
// reload.go's snapshotInfoURLAllowed, each of which keeps only its own host
// policy on top, exactly as this gate does. Those legs read their facts from
// urlform, the app's classifier of record for this browser-vs-net/url
// divergence. The hand-rolled net/url version they replace was a second
// taxonomy of the same knowledge, drifting from what the library learns
// (l-f24): it accepted hidden-host, protocol-relative and backslash-authority
// forms whose browser reading differs from its own, since it only checked
// scheme and userinfo.
//
// The one leg that stays HERE is the non-empty host, because this gate's host
// is load-bearing: the caller's tracker.Is*Host lookup (itself gated on
// urlform.IsASCIIHost) must see the same string a browser would navigate to.
// Returned Host is urlform's ASCII-lowercased evidence.
func httpDisplayHost(raw string) (host string, ok bool) {
	f, ok := httpDisplayForm(raw)
	if !ok {
		return "", false
	}
	return f.Host, true
}

// httpDisplayForm is httpDisplayHost returning the whole classified form, for a
// caller that must parse the VOUCHED reading of the URL rather than its original
// spelling (trackerKeyFromURL's id extraction; h-f8). Emitting or re-parsing
// f.Trimmed is the point: it is the preprocessed string the vouch step actually
// judged, so admission and the id extraction can no longer read two different
// strings.
func httpDisplayForm(raw string) (f urlform.Form, ok bool) {
	f = urlform.Classify(raw)
	if !displaylink.VouchForm(&f) || f.Host == "" {
		return urlform.Form{}, false
	}
	return f, true
}

// sanitizeDisplayURL returns raw when it is a display-admissible URL
// (httpDisplayHost) whose host belongs to the scope's own tracker (scopeOfHost,
// the single home of the host->scope mapping),
// else "" - the item survives with the field blanked (writeItem omits an empty
// <comments> and item.guid() falls back to InfoHash/DownloadURL). Used on the
// passthrough display-URL fields (InfoURL, GUID) that neither the origin filter
// (fetch targets only) nor the curation gate (key-bearing URLs only)
// constrains. Healthy Prowlarr output carries the served tracker's canonical
// page URLs here, so a foreign-host or userinfo-bearing link (a phishing target
// a tampered upstream could attach to a curated item) is blanked rather than
// rendered clickable. The host match is safe against homograph lookalikes
// because scopeOfHost delegates to tracker.LookupByHost, which
// carries the centralized ASCII/homograph gate (urlform.IsASCIIHost) every
// host-table match inherits.
func sanitizeDisplayURL(scope, raw string) string {
	host, ok := httpDisplayHost(raw)
	if !ok {
		return ""
	}
	if scope == "" || scopeOfHost(host) != scope {
		return ""
	}
	return raw
}

// --- Request headers ---

// setHeaders sets the User-Agent, Accept, and the Prowlarr API key header.
func (u *upstream) setHeaders(req *http.Request) {
	req.Header.Set("User-Agent", appinfo.UserAgent)
	req.Header.Set("Accept", "application/rss+xml, application/xml")
	if u.apiKey != "" {
		req.Header.Set("X-Api-Key", u.apiKey)
	}
}
