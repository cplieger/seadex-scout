package indexer

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/httpx/v3"
	"github.com/cplieger/seadex-scout/internal/appinfo"
	"github.com/cplieger/seadex-scout/internal/release"
)

const (
	// upstreamMaxAttempts / upstreamBaseDelay bound the per-query retry.
	upstreamMaxAttempts = 3
	upstreamBaseDelay   = time.Second
	// upstreamMaxBytes bounds a single Torznab response before decode.
	upstreamMaxBytes = 16 << 20
)

// upstream is one Prowlarr per-indexer Torznab endpoint (Nyaa or AnimeBytes).
// The feed proxies these to source real release data (title, seeders, size,
// Prowlarr-proxied download URL) and never talks to the trackers directly.
type upstream struct {
	http   *http.Client
	log    *slog.Logger
	name   string
	feed   string
	apiKey string
}

// search queries the Torznab endpoint with the forwarded params and returns the
// parsed items. The Prowlarr API key is sent as the X-Api-Key header (not a
// query param), so it never appears in a logged request URL.
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
func (u *upstream) search(ctx context.Context, params url.Values) ([]item, error) {
	parsed, err := url.Parse(u.feed)
	if err != nil {
		return nil, errors.New("invalid upstream feed URL")
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
		// Route the retry loop's own Debug/Warn lines through the upstream's
		// component logger so they carry component=indexer instead of
		// falling through to slog.Default().
		httpx.WithLogger(u.log))
	if err != nil {
		return nil, err
	}
	return u.filterDownloadURLs(items), nil
}

// fetchAndParse performs ONE search attempt: a single bounded HTTP fetch
// followed by the Torznab decode. Errors the enclosing Do should
// retry are marked transient: a 429/5xx status (with the 429's capped
// Retry-After carried as the transient error's RetryAfterHint, so the outer
// loop honors the upstream-requested delay), a garbled/truncated 2xx body,
// and a Torznab <error> document carrying a generic/server-side code (e.g.
// 900). A Torznab <error> document naming a deterministic auth/account
// (100-199) or request/parameter (200-299) code is terminal - retrying
// cannot recover a credentials or request-validation failure
// (terminalTorznabCode). Transient transport errors (timeouts, resets, DNS)
// already classify via httpx.IsTransient through the returned chain;
// anything else (a 4xx, an unparseable URL) stays terminal.
func (u *upstream) fetchAndParse(ctx context.Context, reqURL string) ([]item, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, http.NoBody)
	if err != nil {
		return nil, err
	}
	u.setHeaders(req)
	resp, err := u.http.Do(req) //nolint:bodyclose // closed on every path: DrainClose (non-2xx statuses) or ReadLimitedBody's own close (2xx)
	if err != nil {
		// LogSafeError reduces a URL-embedding *url.Error to its cause
		// (preserving errors.Is/As, so IsTransient still classifies it),
		// matching the redaction httpx.GetBytes (v2's Retry) applied here before.
		return nil, httpx.LogSafeError(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		retryAfter := httpx.ParseRetryAfter(resp.Header.Get("Retry-After"))
		httpx.DrainClose(resp.Body)
		statusErr := &httpx.StatusError{URL: reqURL, Code: resp.StatusCode}
		if errors.Is(statusErr, httpx.ErrRateLimited) || errors.Is(statusErr, httpx.ErrServerError) {
			return nil, &transientUpstreamError{err: statusErr, retryAfter: retryAfter}
		}
		return nil, statusErr
	}
	body, err := httpx.ReadLimitedBody(resp.Body, upstreamMaxBytes)
	if err != nil {
		return nil, err
	}
	items, err := parseTorznab(body)
	if err != nil {
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
		if docErr, ok := errors.AsType[*upstreamDocError](err); ok {
			// The document's code/description are attacker-influenced text
			// and the request carried the Prowlarr API key: a compromised
			// upstream could reflect the key into the error message, which
			// httpx.Do's retry logger and the harvest WARN would then expand
			// into the log stream (CWE-532). Classify on the ORIGINAL code
			// first, then redact any reflection of the key from both fields
			// before the error escapes this function.
			terminal := terminalTorznabCode(docErr.code)
			docErr.code = httpx.RedactSecretString(docErr.code, u.apiKey)
			docErr.description = httpx.RedactSecretString(docErr.description, u.apiKey)
			if terminal {
				return nil, docErr
			}
			return nil, &transientUpstreamError{err: err}
		}
		return nil, &transientUpstreamError{err: err, malformedBody: true}
	}
	return items, nil
}

// terminalTorznabCode reports whether a Torznab <error> document's code names
// a deterministic failure a retry cannot recover: the Newznab error ranges
// 100-199 (incorrect credentials, account problems) and 200-299 (missing or
// invalid request parameters) stay wrong on every attempt until the operator
// fixes configuration, so retrying only multiplies upstream load and warning
// noise while delaying the error. Generic/server-side codes (e.g. 900
// "unknown error") and a code that does not parse as a number are NOT
// terminal: they may recover, and an unknown shape defaults to the bounded
// retry rather than failing fast.
func terminalTorznabCode(code string) bool {
	n, err := strconv.Atoi(code)
	return err == nil && n >= 100 && n < 300
}

// transientUpstreamError marks an upstream failure retryable for
// httpx.Do (via the httpx.Transient interface): a retryable
// status or a malformed successful body, neither of which IsTransient
// classifies on its own. retryAfter, when positive, is the 429's parsed and
// capped Retry-After (via httpx.ParseRetryAfter, so it can never exceed
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
	tue, ok := errors.AsType[*transientUpstreamError](err)
	return ok && tue.malformedBody
}

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
	for i := range items {
		if !sameHTTPOrigin(items[i].DownloadURL, feedURL) {
			dropped++
			continue
		}
		// The two passthrough display-URL fields are not fetch targets, but
		// the arr renders <comments> as the item's clickable info link and a
		// URL that parses to no tracker key skips the curation gate entirely,
		// so a tampered upstream could attach a javascript:/data: or
		// foreign-host link to a legitimately curated item. Blank (never
		// drop) anything that is not a userinfo-free absolute http(s) URL on
		// this upstream's own tracker host: a healthy Prowlarr always hands
		// out the served tracker's canonical page URLs here. Display
		// sanitization is independent of key extraction - a URL that fails
		// this gate is blanked even when a tracker key could still be
		// derived from it (e.g. a scheme-relative //host/... form), leaving
		// such an item to match by info hash alone, which fails closed for
		// a URL shape a healthy Prowlarr never emits.
		items[i].InfoURL = sanitizeDisplayURL(u.name, items[i].InfoURL)
		items[i].GUID = sanitizeDisplayURL(u.name, items[i].GUID)
		out = append(out, items[i])
	}
	if dropped > 0 {
		u.log.Warn("upstream items dropped: download URL not on the Prowlarr endpoint origin",
			"upstream", u.name, "dropped", dropped, "kept", len(out),
			"expected_origin", feedURL.Scheme+"://"+feedURL.Host)
	}
	return out
}

// sameHTTPOrigin reports whether raw is an absolute http or https URL, free of
// userinfo, whose scheme and host (including port) match origin's.
func sameHTTPOrigin(raw string, origin *url.URL) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil {
		return false
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return false
	}
	return strings.EqualFold(scheme, origin.Scheme) && strings.EqualFold(parsed.Host, origin.Host)
}

// sanitizeDisplayURL returns raw when it is an absolute http(s) URL, free of
// userinfo, whose host belongs to the scope's own tracker (release.IsNyaaHost
// for the nyaa upstream, release.IsAnimeBytesHost for AB), else "" - the item
// survives with the field blanked (writeItem omits an empty <comments> and
// item.guid() falls back to InfoHash/DownloadURL). Used on the passthrough
// display-URL fields (InfoURL, GUID) that neither the origin filter (fetch
// targets only) nor the curation gate (key-bearing URLs only) constrains.
// Healthy Prowlarr output carries the served tracker's canonical page URLs
// here, so a foreign-host or userinfo-bearing link (a phishing target a
// tampered upstream could attach to a curated item) is blanked rather than
// rendered clickable.
func sanitizeDisplayURL(scope, raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil {
		return ""
	}
	s := strings.ToLower(u.Scheme)
	if s != "http" && s != "https" {
		return ""
	}
	switch scope {
	case upstreamNyaa:
		if !release.IsNyaaHost(u.Hostname()) {
			return ""
		}
	case upstreamAB:
		if !release.IsAnimeBytesHost(u.Hostname()) {
			return ""
		}
	default:
		return ""
	}
	return raw
}

// setHeaders sets the User-Agent, Accept, and the Prowlarr API key header.
func (u *upstream) setHeaders(req *http.Request) {
	req.Header.Set("User-Agent", appinfo.UserAgent)
	req.Header.Set("Accept", "application/rss+xml, application/xml")
	if u.apiKey != "" {
		req.Header.Set("X-Api-Key", u.apiKey)
	}
}
