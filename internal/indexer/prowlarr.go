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

	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/runesafe/v2"
	"github.com/cplieger/seadex-scout/internal/appinfo"
	"github.com/cplieger/seadex-scout/internal/credname"
	"github.com/cplieger/seadex-scout/internal/displaylink"
	"github.com/cplieger/urlform"
)

const (
	// upstreamMaxAttempts bounds the per-query retry.
	upstreamMaxAttempts = 3
	// upstreamMaxBytes bounds a single Torznab response before decode.
	upstreamMaxBytes = 8 << 20
	// minEmbeddedSecretLen is the length from which a query VALUE on the configured
	// feed URL is treated as a credential by upstreamSecrets even though its parameter
	// NAME says nothing about credentials.
	minEmbeddedSecretLen = 16
)

// upstreamAttemptTimeout bounds ONE Prowlarr Torznab attempt: search passes it to the
// retry loop (httpx.WithAttemptTimeout), so the package enforces its own retry
// arithmetic instead of relying on the composition root to wire an http.Client.Timeout
// that matches.
var upstreamAttemptTimeout = 60 * time.Second

// upstreamBaseDelay bounds the per-query retry's backoff. A var, not a const,
// for the same reason upstreamAttemptTimeout is one: the package's retry-
// exhaustion tests would otherwise spend the real backoff in wall-clock time
// (TestMain shortens it, exactly as it does the harvest's politeness gap).
// Production reads the unchanged one-second value.
var upstreamBaseDelay = time.Second

// upstream is one Prowlarr per-indexer Torznab endpoint (Nyaa or AnimeBytes).
// The feed proxies these to source real release data (title, seeders, size,
// Prowlarr-proxied download URL) and never talks to the trackers directly.
type upstream struct {
	http   *http.Client
	log    *slog.Logger
	name   string
	feed   string
	apiKey string
	// dropWarned / displayWarned bound the two filterDownloadURLs diagnostics to one
	// WARN per onset (plus one recovery INFO), so a SYSTEMATIC condition - a Prowlarr
	// endpoint whose emitted download links sit on a different origin than the
	// configured Torznab URL, or an upstream emitting non-tracker display URLs - cannot
	// WARN once per query.
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
func (u *upstream) search(ctx context.Context, params url.Values) ([]item, int, error) {
	parsed, err := url.Parse(u.feed)
	if err != nil {
		// Deliberately NOT wrapped: a *url.Error echoes the raw configured URL, which
		// may carry a username-only userinfo token (validateHTTPURL accepts one), and
		// this error reaches httpx.Do's retry logger - the same redaction stance the
		// StatusError path below applies (CWE-532).
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
		// Demote httpx's terminal "retries exhausted" line to Debug. BOTH callers of
		// search publish their own WARN for the same failed query with strictly more
		// context - the harvest names the show, the query shape and the page and drives
		// its latch state (classifyHarvestError), the request path names the upstream
		// (query.go's fetchRaw) - so leaving httpx's verdict at Warn produced two
		// terminal WARNs per failure, six per three-show homogeneous malformed run, and
		// doubled the Loki volume of exactly the incident the once-per-onset cadence
		// exists to keep readable.
		httpx.WithExhaustedLevel(slog.LevelDebug),
		// The per-attempt bound is the loop's, not a context this callback derives:
		// WithAttemptTimeout installs it AND marks its expiry retryable
		// (httpx.AttemptTimeout), which IsTransient consults ahead of its
		// caller-context rejection.
		httpx.WithAttemptTimeout(upstreamAttemptTimeout))
	if err != nil {
		return nil, 0, err
	}
	return u.filterDownloadURLs(items, parsed), len(items), nil
}

// fetchAndParse performs ONE search attempt: a single bounded HTTP fetch followed by
// the Torznab decode.
func (u *upstream) fetchAndParse(ctx context.Context, reqURL string) ([]item, error) {
	// GetBytes owns the one-attempt HTTP mechanics: request construction, header
	// injection, transport-error reduction, non-2xx drain + *StatusError, the
	// Retry-After parse, and the bounded body read.
	body, err := httpx.GetBytes(ctx, u.http, reqURL,
		httpx.WithMaxAttempts(1),
		httpx.WithHeaders(u.setHeaders),
		httpx.WithMaxBodyBytes(upstreamMaxBytes),
		httpx.WithLogger(u.log))
	if err != nil {
		return nil, attemptError(err)
	}
	items, err := parseTorznab(body)
	if err != nil {
		return nil, u.classifyParseError(err)
	}
	return items, nil
}

// attemptError maps a GetBytes failure onto the retry taxonomy the enclosing
// Do reads. GetBytes runs with WithMaxAttempts(1), so its every failure exit
// arrives here and this function alone decides whether the search spends
// another of its upstreamMaxAttempts.
func attemptError(err error) error {
	if statusErr, ok := errors.AsType[*httpx.StatusError](err); ok {
		// httpx.IsRetryableStatus is the SAME rule GetBytes's own attempt
		// function applies, not a restatement of it, so this caller's verdict
		// and the door's cannot disagree - which is why the library exports it
		// for exactly this WithMaxAttempts(1)-under-an-outer-Do composition.
		if !httpx.IsRetryableStatus(statusErr.Code) {
			return statusErr
		}
		// Unwrap to the status itself: GetBytes's "retries exhausted after 0s"
		// prefix is noise for a budget the app deliberately set to one attempt.
		return &transientUpstreamError{err: statusErr, retryAfter: retryAfterHint(err)}
	}
	// A per-attempt deadline is NOT classified here: the enclosing Do installed the
	// bound (WithAttemptTimeout) and marks its expiry, which is the only place that can
	// tell the attempt's deadline from the caller's.
	return httpx.LogSafeError(err)
}

// retryAfterHint returns the capped Retry-After an upstream asked for, or zero
// when it named none. The hint rides the error chain as an
// httpx.RetryAfterHint implementer (GetBytes attaches it to the exhaustion
// error), and it is already bounded by httpx.RetryAfterCap at the parse, so it
// satisfies the interface's pre-capped contract on the way back out.
func retryAfterHint(err error) time.Duration {
	if h, ok := errors.AsType[httpx.RetryAfterHint](err); ok {
		return h.RetryAfterHint()
	}
	return 0
}

// upstreamSecrets returns every credential the app TRANSMITS to this upstream and must
// therefore keep out of an error message or log line (CWE-532): the X-Api-Key header
// value, plus any credential embedded in the CONFIGURED feed URL.
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

// userinfoSecrets returns every wire representation of a credential embedded in the
// configured feed URL's userinfo.
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
func credentialParamName(name string) bool {
	decoded, err := url.QueryUnescape(name)
	if err != nil {
		decoded = name
	}
	return credname.ContainsWord(decoded)
}

// redactSecrets removes every credential this upstream carries from untrusted
// upstream text. It runs on the error path only, and redactAndBound runs it on BOTH
// sides of the sanitizer, so each exact-substring replacement sees the credential in
// the form the text carries at that position. An empty secret is a no-op
// (httpx.RedactSecretString).
func (u *upstream) redactSecrets(s string) string {
	for _, secret := range u.upstreamSecrets() {
		s = httpx.RedactSecretString(s, httpx.Secret(secret))
	}
	return s
}

// redactAndBound is the emit-boundary composition for untrusted upstream text that
// carries this upstream's credentials: redact, sanitize, redact again, then cap.
// Order is the correctness argument:
//
//   - The PRE-pass catches a credential the sanitizer would garble: it maps an
//     unsafe rune to a space and an invalid UTF-8 byte to U+FFFD, after which the
//     byte-exact needle no longer matches and a near-complete fragment survives.
//   - The POST-pass catches a credential the sanitizer CONSTRUCTS. Four of this
//     upstream's needles carry a U+0020 (a userinfo value configured as
//     user%20name, or a '+' that decodes to a space via url.QueryUnescape - base64
//     passkeys hit this routinely), so an upstream echoing correct<DEL>horse
//     defeats the pre-pass needle and the sanitizer then reassembles "correct
//     horse" from it. DEL stands in for a C0 byte because encoding/xml rejects C0
//     outright.
//   - The CAP is last so a credential straddling the bound is already gone rather
//     than sliced into a surviving prefix.
//
// The bound is upstreamTextMaxBytes with the marker counted inside it
// (SanitizeSingleLineCapped), so upstreamDocError.Error()'s surviving sanitize
// pass is a byte-for-byte no-op on this output.
func (u *upstream) redactAndBound(s string) string {
	s = u.redactSecrets(s)
	s = runesafe.SanitizeSingleLine(s)
	s = u.redactSecrets(s)
	text, _ := runesafe.SanitizeSingleLineCapped(s, upstreamTextMaxBytes, "...")
	return text
}

// classifyParseError maps a parseTorznab failure onto the retry taxonomy.
func (u *upstream) classifyParseError(err error) error {
	if docErr, ok := errors.AsType[*upstreamDocError](err); ok {
		// The document's code/description are attacker-influenced text and the request
		// carried the Prowlarr API key: a compromised upstream could reflect the key
		// into the error message, which httpx.Do's retry logger and the harvest WARN
		// would then expand into the log stream (CWE-532).
		terminal := terminalTorznabCode(docErr.codeNum)
		docErr.code = u.redactAndBound(docErr.code)
		docErr.description = u.redactAndBound(docErr.description)
		if terminal {
			return docErr
		}
		return &transientUpstreamError{err: err}
	}
	if limitErr, ok := errors.AsType[*torznabLimitError](err); ok {
		// App-controlled message; keep it verbatim.
		return &transientUpstreamError{err: limitErr, malformedBody: true}
	}
	// A generic decode failure can echo attacker-controlled body text verbatim
	// (encoding/xml returns raw strconv errors quoting the full unparsed <size>/length
	// value, up to the wire cap upstreamMaxBytes), and the request carried the Prowlarr
	// API key: redact any reflection of the key before the error reaches httpx.Do's
	// retry logger or fetchRaw's WARN - the same emit-boundary policy the
	// upstreamDocError path applies. The redaction runs on BOTH sides of the sanitizer
	// (redactAndBound), because the sanitizer can both garble a credential the
	// pre-pass would have caught and reassemble one from text no needle matches.
	msg := u.redactAndBound(err.Error())
	return &transientUpstreamError{err: errors.New(msg), malformedBody: true}
}

// terminalTorznabCode reports whether a Torznab <error> document's parsed code
// (upstreamDocError.codeNum, -1 for non-numeric) names a deterministic failure a retry
// cannot recover: the Newznab error ranges 100-199 (incorrect credentials, account
// problems) and 200-299 (missing or invalid request parameters) stay wrong on every
// attempt until the operator fixes configuration, so retrying only multiplies upstream
// load and warning noise while delaying the error.
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
	// GetBytes reads the body only after its status check admitted a 2xx, so an
	// over-cap body is by construction the read failure of a SUCCESSFUL response - the
	// same class as a garbled or over-cardinality one, and scoped to the one result set
	// that came back too large rather than to the upstream's availability.
	_, tooLarge := errors.AsType[*httpx.ResponseTooLargeError](err)
	return tooLarge
}

// filterDownloadURLs drops items whose download URL is not an absolute http(s)
// URL on the same origin as the configured Prowlarr Torznab endpoint. The
// curation lookup only proves an identifier is in the SeaDex snapshot; it does
// not bind the download target, so a tampered Prowlarr response could
// otherwise pair a curated id with an internal or attacker-controlled URL the
// arr then fetches as a curated release (SSRF / arbitrary download, CWE-918).
func (u *upstream) filterDownloadURLs(items []item, feedURL *url.URL) []item {
	out := make([]item, 0, len(items))
	dropped := 0
	blankedDisplay := 0
	// observedDisplay counts the display-URL fields actually INSPECTED on the surviving
	// items.
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
		return out
	}
	u.reportDroppedDownloadURLs(dropped, len(out), feedURL)
	if observedDisplay > 0 {
		// The display gate's observation set is the display-URL FIELDS actually
		// inspected, not the input page and not the surviving item count: when every
		// item is dropped for an off-origin download URL, or when the survivors carry
		// neither an InfoURL nor a GUID, no display URL was inspected at all.
		u.reportBlankedDisplayURLs(blankedDisplay, len(out))
	}
	return out
}

// sanitizeItemDisplayURLs sanitizes one surviving item's two passthrough
// display-URL fields in place, returning how many of them were INSPECTED and
// how many were blanked (the counts filterDownloadURLs' onset ladder reads).
// An admitted field is rewritten to the gate's vouched spelling, which is not a
// blanking and must not be counted as one - the same accounting
// sanitizeSnapshotInfoURLs applies on the persisted side.
func (u *upstream) sanitizeItemDisplayURLs(it *item) (observed, blanked int) {
	for _, field := range []*string{&it.InfoURL, &it.GUID} {
		if *field == "" {
			continue
		}
		observed++
		cleaned, ok := sanitizeDisplayURL(u.name, *field)
		if !ok {
			blanked++
		}
		*field = cleaned
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
// case-insensitively, port compared after defaulting an omitted one to the
// scheme's (80/443).
//
// Both comparisons fold ASCII-only, deliberately not strings.EqualFold: full
// Unicode simple folding has ASCII-producing mappings (measured across
// Unicode 15 to 17 / Go 1.26 to 1.27: U+0390, U+03B0 and U+FB05 newly fold to
// ASCII-adjacent runes), so it could launder a homograph host into a
// canonical one, and a gate whose answers move with the toolchain's fold
// table is not a gate. urlform's folds are non-ASCII-to-ASCII-proof.
func sameHTTPOrigin(raw string, origin *url.URL) bool {
	parsed, ok := httpNoUserinfoURL(raw)
	if !ok {
		return false
	}
	if !urlform.EqualASCIIFold(parsed.Scheme, origin.Scheme) {
		return false
	}
	if urlform.FoldHostASCII(parsed.Hostname()) != urlform.FoldHostASCII(origin.Hostname()) {
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

// httpDisplayForm admits a raw URL as a browser-destined DISPLAY link and
// returns its classified form: an absolute http(s) form, free of userinfo and
// of the smuggling shapes a browser reads differently from net/url. It is the
// shared admission prefix of BOTH its consumers: sanitizeDisplayURL (search-path
// display links) and trackerKeyFromURL (match.go, the curation IDENTITY gate),
// so relaxing it changes what mints a curation key too.
//
// Both consumers read f.Trimmed rather than the original spelling (h-f8): it is
// the preprocessed string the vouch step actually judged, so admission, id
// extraction and the emitted link read the same string.
func httpDisplayForm(raw string) (f urlform.Form, ok bool) {
	f = urlform.Classify(raw)
	if !displaylink.VouchForm(&f) || f.Host == "" {
		return urlform.Form{}, false
	}
	return f, true
}

// sanitizeDisplayURL reports whether raw is a display-admissible URL
// (httpDisplayForm) whose host belongs to the scope's own tracker
// (scopeOfHost), and returns the VOUCHED spelling for the caller to emit
// (urlform's WHATWG-preprocessed Form.Trimmed - the string the gate actually
// judged). On refusal the caller blanks the field and the item survives
// (writeItem omits an empty <comments>; item.guid() falls back to
// InfoHash/DownloadURL).
//
// Returning the vouched reading rather than the original is the h-f8 rule
// trackerKeyFromURL and snapshotInfoURLAllowed already follow: an edge-padded
// upstream value ("http://nyaa.si  ") is vouched on the browser's reading of
// it, so passing the padded original through would hand the arr UI a
// <comments> link net/url refuses to parse. It also keeps the emitted GUID on
// the same spelling trackerKeyFromURL keys the curation set by.
func sanitizeDisplayURL(scope, raw string) (cleaned string, ok bool) {
	f, ok := httpDisplayForm(raw)
	if !ok {
		return "", false
	}
	if scope == "" || scopeOfHost(f.Host) != scope {
		return "", false
	}
	return f.Trimmed, true
}

// setHeaders sets the User-Agent, Accept, and the Prowlarr API key header.
func (u *upstream) setHeaders(req *http.Request) {
	req.Header.Set("User-Agent", appinfo.UserAgent)
	req.Header.Set("Accept", "application/rss+xml, application/xml")
	if u.apiKey != "" {
		req.Header.Set("X-Api-Key", u.apiKey)
	}
}
