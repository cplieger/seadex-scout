// Package anilist is a minimal AniList GraphQL client used only as a fallback
// when the Fribb map plus operator overrides miss an AniList ID. It fetches an
// entry's titles, format, and year so the match package can attempt a
// conservative title-plus-year match against the library.
//
// AniList publishes a per-minute request budget in response headers. The client
// spaces requests to a configured rate, reads X-RateLimit-Remaining/Reset to
// slow down before a 429, and honors Retry-After on a 429. Mapped items never
// reach this client, so steady-state AniList traffic is near zero.
package anilist

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/cplieger/httpx/v4"
	"github.com/cplieger/jsonx/bounded"
	"github.com/cplieger/runesafe"
	"github.com/cplieger/seadex-scout/internal/appinfo"
	"github.com/cplieger/seadex-scout/internal/titlekey"
)

// DefaultURL is the AniList GraphQL endpoint (title/format fallback).
const DefaultURL = "https://graphql.anilist.co"

const (
	// DefaultRate is the AniList request/minute ceiling. It is AniList contract
	// knowledge, so it lives here beside DefaultURL and the throttle that
	// enforces it; the wiring site (build.go) references it.
	DefaultRate = 30

	maxBodyBytes = 1 << 20
	maxAttempts  = 3
	baseDelay    = time.Second
	// lowRemaining is the X-RateLimit-Remaining threshold at or below which
	// the client proactively waits for the window reset to avoid a 429.
	lowRemaining = 2
	// defaultRetryAfter is used when a 429 carries no Retry-After header.
	defaultRetryAfter = 5 * time.Second
	// maxRetryAfter caps a server-supplied Retry-After (or reset-window) wait so a
	// pathological/hostile header cannot stall the AniList fallback and, via penalize,
	// every subsequent lookup. It doubles as the WithRateLimitRetry ceiling on
	// request's retry loop; the throttle consumes the wait verbatim, so the cap
	// must be applied here before penalize.
	maxRetryAfter = time.Minute
)

// ErrNotFound reports that AniList has no media for the requested ID.
var ErrNotFound = errors.New("anilist: media not found")

// ErrBatchRecord marks a record-local validation failure inside an otherwise
// well-formed batch response (match with errors.Is), distinguishing it from a
// request/envelope failure so FetchMany keeps fetching later chunks instead of
// reading one poisoned record as a total outage. FetchMany returns it alongside
// its partial result; batch COMPLETION is signaled separately by the result map
// (nil = no chunk completed), so an empty-but-non-nil map beside this error
// means the chunks completed but every record was malformed — still
// record-local, per-id fallback applies.
var ErrBatchRecord = errors.New("anilist: batch response")

// BatchRecordError names the ids whose CHUNK is not trustworthy as evidence of
// absence: either the chunk reported a record-local failure, or it never
// completed at all (FetchMany aborted at or before it). Err is whichever
// failure produced that scoping - a record-local error wrapping
// ErrBatchRecord, or an aborting envelope/request error that does NOT wrap it
// (joined with an earlier chunk's record error when there was one). So
// errors.Is(err, ErrBatchRecord) classifies the FAILURE and must not be read
// as "a *BatchRecordError was returned"; use errors.As for that.
//
// The scoping is the point. A record-local defect is confined to the chunk it
// arrived in: the other chunks completed cleanly and their absent ids ARE
// definitively answered. Returning one undifferentiated error for the whole call
// made a single malformed record in chunk 1 of 9 withdraw the completion
// evidence of the other eight, so ~450 already-answered ids each fell through to
// a rate-limited per-id Fetch - the ~1700-request cold cycle batching exists to
// avoid. Callers memoize negatives for every requested id EXCEPT UnverifiedIDs.
type BatchRecordError struct {
	Err error
	// UnverifiedIDs are the ids belonging to chunks that reported a
	// record-local failure OR that never completed (the aborting chunk and every
	// chunk after it). Absence of one of these from the result set proves
	// nothing, so it must not be memoized as not-found.
	UnverifiedIDs []int
}

func (e *BatchRecordError) Error() string { return e.Err.Error() }

func (e *BatchRecordError) Unwrap() error { return e.Err }

// --- upstream failure classification ---

// transientStatusError marks an upstream failure this client considers
// self-healing even though httpx's shared policy does not. httpx's
// *HTTPStatusError is transient for 502/503/504 only, which leaves a plain 500
// (AniList's generic GraphQL server fault) and a 408 terminal after a single
// attempt, and leaves a server-side failure delivered inside a 200 GraphQL
// envelope invisible to the retrier entirely. Both classes clear on their own,
// and the queries are idempotent, so they belong inside the bounded budget.
type transientStatusError struct{ err error }

func (e *transientStatusError) Error() string { return e.err.Error() }

func (e *transientStatusError) Unwrap() error { return e.err }

func (e *transientStatusError) IsTransient() bool { return true }

// retryableUpstreamStatus reports whether an upstream status is a self-healing
// server-side failure worth another attempt. It covers every 5xx (not just
// httpx's 502/503/504) plus 408 Request Timeout; a 4xx other than 408 is the
// client's own fault and never retried, and 429 has its own dedicated
// rate-limit path.
func retryableUpstreamStatus(code int) bool {
	return code == http.StatusRequestTimeout || (code >= 500 && code < 600)
}

// transientEnvelopeError classifies a GraphQL envelope that arrived with a
// successful HTTP status but reports a SERVER-side failure in its errors[] list.
// AniList answers such faults as 200 with {"errors":[{"status":500,...}]}, so
// httpx.Do has already declared the attempt successful by the time the parser
// sees it and no retry ever happens. Returning a transient error here - at the
// retry boundary, before parsing - puts that class back inside the budget.
//
// Only the retryable statuses qualify: a 404 envelope is AniList's genuine
// not-found (Fetch's ErrNotFound contract depends on it reaching the parser),
// and an unparseable body is the parser's business, not the retrier's.
func transientEnvelopeError(raw []byte) error {
	var env struct {
		Errors gqlErrors `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil
	}
	for _, e := range env.Errors {
		if retryableUpstreamStatus(e.Status) {
			return &transientStatusError{err: fmt.Errorf("anilist: upstream reported status %d: %s",
				e.Status, sanitizeUpstreamMessage(e.Message))}
		}
	}
	return nil
}

// ErrRecordUnusable marks a rejection determined entirely by the upstream
// record's OWN CONTENT: no title that normalizes to a usable match key (a
// CJK-only or symbol-only title set), an over-cap title or format field, or
// text that is unsafe to memoize. It is PERMANENT - the same record fails
// identically on every future cycle - so it is not an outage and must not be
// classified as one.
//
// The distinction is load-bearing for alerting. Treated as transient, such a
// record is re-fetched every cycle forever, keeps Result.Degraded true, and
// advances the persisted AniList degradation streak until it escalates to a
// standing ERROR whose remediation text points at graphql.anilist.co
// reachability that is perfectly healthy. It is also not a plain not-found: the
// record exists, it just cannot be matched on, and the operator's real remedy is
// an overrides.json entry supplying the arr id directly. Callers therefore
// memoize it negatively (a definitive answer) and say so once, with that remedy
// named. Callers match with errors.Is.
var ErrRecordUnusable = errors.New("anilist: record unusable for matching")

// query fetches the fields needed for a title fallback match, plus the id so
// Fetch can bind the response to the requested identity (parseMediaForID).
const query = `query ($id: Int) { Media(id: $id, type: ANIME) { id format seasonYear startDate { year } title { romaji english native } } }`

// batchSize is AniList's Page perPage maximum; FetchMany resolves up to this
// many ids per request.
const batchSize = 50

// batchQuery fetches the same fields for many ids in one request via Page.media,
// which still counts as a single request against AniList's per-minute budget -
// so a cold cycle's hundreds of id-less lookups collapse to a handful of calls.
// Built from batchSize so the page size and the chunk size cannot drift apart.
var batchQuery = fmt.Sprintf(`query ($ids: [Int]) { Page(perPage: %d) { media(id_in: $ids, type: ANIME) { id format seasonYear startDate { year } title { romaji english native } } } }`, batchSize)

// Media is the subset of an AniList entry used for title matching.
type Media struct {
	Format string
	Titles []string
	Year   int
}

// Stats is a snapshot of client activity for cycle observability logs.
// Calls counts outbound HTTP attempts (retries included), so during 429 or
// transient-network episodes it exceeds the number of logical fetches;
// RateLimitWaits counts 429 responses plus proactive low-budget backoffs.
type Stats struct {
	Calls          int64
	RateLimitWaits int64
}

// --- client, requests, and rate-limit policy ---

// Client queries AniList with an adaptive throttle.
type Client struct {
	http     *http.Client
	log      *slog.Logger
	throttle *throttle
	url      string
	calls    atomic.Int64
	rlWaits  atomic.Int64
}

// NewClient returns an AniList client for url at rate requests per minute
// (values <= 0 are treated as 1). logger may be nil.
func NewClient(httpClient *http.Client, url string, rate int, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	if rate <= 0 {
		rate = 1
	}
	return &Client{
		http:     httpClient,
		log:      logger,
		throttle: &throttle{interval: time.Minute / time.Duration(rate)},
		url:      url,
	}
}

// Stats returns a snapshot of the cumulative HTTP-attempt and rate-limit-wait
// counts.
func (c *Client) Stats() Stats {
	return Stats{Calls: c.calls.Load(), RateLimitWaits: c.rlWaits.Load()}
}

// request marshals the GraphQL payload and performs one retried POST,
// returning the raw response body. Shared by Fetch and FetchMany. The throttle
// is claimed INSIDE the retry closure so every actual HTTP attempt reserves
// its own rate slot: a transient 5xx/transport retry would otherwise re-fire
// after only the backoff delay, exceeding the configured requests-per-minute
// ceiling. WithRateLimitRetry makes the 429's *httpx.RateLimitError retryable
// (httpx classifies it non-transient by default) and bounds its wait at
// maxRetryAfter; rateLimitError caps the hint to the same ceiling before
// throttle.penalize, so the retry wait and the penalty converge on one value —
// the extra per-attempt wait is effectively zero once the hint expires, while
// later callers retain the penalty.
func (c *Client) request(ctx context.Context, gql string, variables any) ([]byte, error) {
	body, err := json.Marshal(map[string]any{"query": gql, "variables": variables})
	if err != nil {
		return nil, fmt.Errorf("anilist: marshal request: %w", err)
	}
	return httpx.Do(ctx,
		func(ctx context.Context) ([]byte, error) {
			if err := c.throttle.wait(ctx); err != nil {
				return nil, err
			}
			raw, err := c.do(ctx, body)
			if err != nil {
				return nil, err
			}
			// Classify a server-side failure delivered inside a successful
			// envelope here, at the retry boundary: past this point httpx has
			// recorded the attempt as a success and the class can never be
			// retried (see transientEnvelopeError).
			if transientErr := transientEnvelopeError(raw); transientErr != nil {
				return nil, transientErr
			}
			return raw, nil
		},
		httpx.WithMaxAttempts(maxAttempts),
		httpx.WithBaseDelay(baseDelay),
		httpx.WithLabel("anilist"),
		httpx.WithLogger(c.log),
		httpx.WithRateLimitRetry(maxRetryAfter))
}

// Fetch returns the AniList media for the given ID, or ErrNotFound when AniList
// has no such anime. It throttles before the request and retries transient
// failures and 429s (honoring Retry-After). A non-positive id is rejected
// without a request; the identity invariant requires a positive requested id.
func (c *Client) Fetch(ctx context.Context, aniListID int) (Media, error) {
	if aniListID <= 0 {
		return Media{}, fmt.Errorf("anilist: invalid media id %d", aniListID)
	}
	raw, err := c.request(ctx, query, map[string]int{"id": aniListID})
	if err != nil {
		return Media{}, err
	}
	return parseMediaForID(raw, aniListID)
}

// FetchMany resolves many AniList ids in batched requests (up to batchSize ids
// each, every batch throttled and retried like Fetch), returning the media that
// exist keyed by id. An id AniList has no anime for is simply absent from the
// result (the caller treats an absent id as not-found). The result carries the
// batch-completion contract: a NIL map with an error means no chunk completed
// (a total failure); a NON-NIL map with an error means at least one chunk
// completed — even when the map is empty because every completed chunk
// definitively found no media — so the caller can fall back to a per-id Fetch
// for the remainder rather than losing the batch, and can tell an all-not-found
// chunk apart from a total outage. "The remainder" is named by the returned
// *BatchRecordError's UnverifiedIDs (the chunks that never completed), so the
// completed chunks' absences stay usable as definitive evidence. A
// record-local failure (ErrBatchRecord, a poisoned record inside an otherwise
// well-formed response) does NOT abort the batch: the chunk still counts as
// completed, later chunks are still fetched,
// and the first record error is surfaced alongside the merged result, so one
// malformed record cannot hide every id after it or read as a total outage to
// the caller. The response is untrusted: an id the current chunk never
// requested is dropped before the merge (retainRequested) and surfaced like
// any other record-local failure, so a malformed or compromised response
// cannot inject an unrelated Media or overwrite an earlier chunk's value.
func (c *Client) FetchMany(ctx context.Context, ids []int) (map[int]Media, error) {
	out := make(map[int]Media, len(ids))
	completed := false
	var firstRecordErr error
	var unverified []int
	answered := 0
	for chunk := range slices.Chunk(ids, batchSize) {
		page, err := c.fetchBatchChunk(ctx, chunk)
		maps.Copy(out, page)
		if err != nil && !errors.Is(err, ErrBatchRecord) {
			return completedBatch(out, completed, slices.Concat(unverified, ids[answered:]),
				joinRecordErr(firstRecordErr, err))
		}
		answered += len(chunk)
		completed = true
		if err != nil {
			// Record-local: this CHUNK's absences prove nothing, but every
			// other chunk still completed cleanly and its absences do. Collect
			// the untrustworthy ids rather than failing the whole call, so the
			// caller can still memoize the negatives it legitimately learned.
			unverified = append(unverified, chunk...)
			if firstRecordErr == nil {
				firstRecordErr = err
			}
		}
	}
	if firstRecordErr != nil {
		return out, &BatchRecordError{UnverifiedIDs: unverified, Err: firstRecordErr}
	}
	return out, nil
}

// fetchBatchChunk fetches and parses one chunk of FetchMany's id list. A
// request failure returns a nil page (nothing to merge); otherwise the parsed
// page is returned alongside the joined parse and identity-set errors, so
// FetchMany's caller-facing contract logic reads as one linear loop. A
// record-local failure (ErrBatchRecord) still returns the chunk's valid
// records, matching FetchMany's does-not-abort-the-batch rule.
func (c *Client) fetchBatchChunk(ctx context.Context, chunk []int) (map[int]Media, error) {
	raw, err := c.request(ctx, batchQuery, map[string]any{"ids": chunk})
	if err != nil {
		return nil, err
	}
	page, parseErr := parseMediaPage(raw)
	return page, errors.Join(parseErr, retainRequested(page, chunk))
}

// completedBatch applies FetchMany's nil-map-versus-partial-map contract to an
// aborting chunk failure: no chunk completed yet means a total failure (a NIL
// map with the error), while an earlier completed chunk means the merged
// partial result rides along so the caller can fall back for the remainder.
//
// "The remainder" has to be nameable for the caller to honor it, so a partial
// abort scopes the error the same way a record-local failure does: unverified
// carries every id whose chunk did NOT complete (the aborting chunk and every
// chunk after it, plus any earlier record-local chunk), and the completed
// chunks' absences stay definitive evidence the caller may memoize. Without
// the scoping an abort in chunk 3 of 9 withdrew the completion evidence of
// chunks 1-2 as well - the same defect BatchRecordError exists to prevent on
// the record-local path.
func completedBatch(out map[int]Media, completed bool, unverified []int, err error) (map[int]Media, error) {
	if !completed {
		return nil, err
	}
	return out, &BatchRecordError{UnverifiedIDs: unverified, Err: err}
}

// joinRecordErr preserves an earlier chunk's record-local diagnostic when a
// later chunk aborts the batch. The aborting error leads, so the abort stays
// the classification every caller reads first, while the poisoned record that
// the record-scoping design exists to surface is no longer silently dropped.
// The ids of that earlier chunk are already inside completedBatch's unverified
// set, so carrying the diagnostic never widens what the caller may memoize.
func joinRecordErr(recordErr, abortErr error) error {
	if recordErr == nil {
		return abortErr
	}
	return errors.Join(abortErr, recordErr)
}

// retainRequested enforces FetchMany's identity-set invariant on one parsed
// page: every id in the response must have been in the chunk that requested
// it. An unsolicited id is deleted from the page - never merged, where it
// could inject an unrelated Media or overwrite a value an earlier chunk
// legitimately resolved - and one such id (the first encountered in map
// iteration order, so arbitrary when several are unsolicited) is reported as
// an ErrBatchRecord-wrapped error so the caller sees the malformed response
// without losing the chunk's valid records.
func retainRequested(page map[int]Media, chunk []int) error {
	requested := make(map[int]struct{}, len(chunk))
	for _, id := range chunk {
		requested[id] = struct{}{}
	}
	var first error
	for id := range page {
		if _, ok := requested[id]; ok {
			continue
		}
		delete(page, id)
		if first == nil {
			first = fmt.Errorf("%w unexpected media id %d", ErrBatchRecord, id)
		}
	}
	return first
}

// do performs one GraphQL POST attempt, translating a 429 into a
// *httpx.RateLimitError carrying a capped Retry-After hint (retried by
// request's WithRateLimitRetry mode) and reading the rate headers on every
// non-429 response (error statuses included) to pre-empt the next 429.
func (c *Client) do(ctx context.Context, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", appinfo.UserAgent)

	c.calls.Add(1)
	resp, err := c.http.Do(req) //nolint:bodyclose // closed on every path: DrainClose (429/error statuses) or ReadLimitedBody's own close (200/404)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		httpx.DrainClose(resp.Body)
		return nil, c.rateLimitError(resp)
	}
	// The budget headers are read on EVERY response that is not itself a rate
	// limit, error statuses included: AniList stamps X-RateLimit-Remaining/Reset
	// on a 4xx/5xx too, and dropping a low-remaining signal there would let the
	// next lookup race into the 429 this pre-emption exists to avoid. A response
	// without the headers is a no-op (the Atoi guard), so error statuses that
	// carry no budget information are unaffected. Each exit below observes them
	// exactly once, and the success path observes them only AFTER
	// envelopeRateLimitError has had its say: a 429 delivered inside a
	// successful envelope is the same rate-limit response, so penalizing the
	// throttle through rateLimitError and again through a low-remaining header
	// would report one response as two Stats().RateLimitWaits.
	//
	// AniList mirrors a GraphQL-level not-found into the HTTP status: a
	// nonexistent id answers 404 while still carrying the normal envelope
	// {"data":{"Media":null},"errors":[{"message":"Not Found."}]} (verified
	// live). Pass the 404 body through to the parser so Fetch can honor its
	// ErrNotFound contract instead of surfacing an opaque HTTP 404.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		c.observeRateHeaders(resp)
		httpx.DrainClose(resp.Body)
		statusErr := httpx.CheckHTTPStatus(resp)
		if statusErr == nil {
			statusErr = fmt.Errorf("anilist: unexpected status %d", resp.StatusCode)
		}
		if retryableUpstreamStatus(resp.StatusCode) {
			return nil, &transientStatusError{err: statusErr}
		}
		return nil, statusErr
	}

	// ReadLimitedBody closes the body and fails loud with a distinct
	// *httpx.ResponseTooLargeError on an over-cap body, so an oversized
	// response surfaces as its own error rather than a silently truncated
	// payload that only fails later as a confusing JSON decode error.
	respBody, err := httpx.ReadLimitedBody(resp.Body, maxBodyBytes)
	if err != nil {
		c.observeRateHeaders(resp)
		return nil, fmt.Errorf("anilist: read response: %w", err)
	}
	// A 429 AniList reports INSIDE a successful envelope must take the same
	// dedicated rate-limit path as an HTTP 429, or the throttle is never
	// penalized and the next lookup spends budget inside the window AniList
	// just closed.
	if rlErr := c.envelopeRateLimitError(resp, respBody); rlErr != nil {
		return nil, rlErr
	}
	c.observeRateHeaders(resp)
	return respBody, nil
}

// envelopeRateLimitError applies the dedicated 429 path to a rate limit
// AniList reports inside a successful GraphQL envelope. retryableUpstreamStatus
// deliberately excludes 429 because 429 has its own path - but that path only
// ever ran on the HTTP status, so an envelope-delivered 429 surfaced as a
// terminal query error that penalized nothing.
func (c *Client) envelopeRateLimitError(resp *http.Response, raw []byte) error {
	var env struct {
		Errors gqlErrors `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil
	}
	for _, e := range env.Errors {
		if e.Status == http.StatusTooManyRequests {
			return c.rateLimitError(resp)
		}
	}
	return nil
}

// resetWait returns the time remaining until the X-RateLimit-Reset window
// ends, or 0 when the header is absent, malformed, or already past.
func resetWait(resp *http.Response) time.Duration {
	reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil {
		return 0
	}
	if until := time.Until(time.Unix(reset, 0)); until > 0 {
		return until
	}
	return 0
}

// rateLimitError handles a 429 response: it derives a capped wait from
// Retry-After (or X-RateLimit-Reset, or the default), penalizes the throttle,
// and returns the *httpx.RateLimitError carrying that wait as its RetryAfter
// hint, which request's WithRateLimitRetry mode retries.
func (c *Client) rateLimitError(resp *http.Response) error {
	wait := httpx.ParseRetryAfter(resp.Header.Get("Retry-After"))
	if wait <= 0 {
		// A 429 without a usable Retry-After often still carries the
		// window end in X-RateLimit-Reset; waiting for that instead of a
		// blind default keeps the bounded attempts from all landing
		// inside the same rate window.
		wait = resetWait(resp)
	}
	if wait <= 0 {
		wait = defaultRetryAfter
	}
	upstream := wait
	wait = c.backOff(wait)
	c.log.Warn("anilist rate limited (429); backing off",
		"retry_after", wait.Round(time.Second),
		"upstream_retry_after", upstream.Round(time.Second))
	return &httpx.RateLimitError{Msg: "anilist: rate limited (429)", RetryAfter: wait}
}

// backOff caps an upstream-supplied wait at maxRetryAfter, counts it, and
// penalizes the throttle, returning the capped value the caller logs. It is
// the one place the ceiling is applied, so no back-off path can hand
// throttle.penalize an unbounded upstream duration.
func (c *Client) backOff(wait time.Duration) time.Duration {
	wait = min(wait, maxRetryAfter)
	c.rlWaits.Add(1)
	c.throttle.penalize(wait)
	return wait
}

// observeRateHeaders slows the throttle when the remaining budget is low,
// waiting for the reset window rather than racing into a 429.
func (c *Client) observeRateHeaders(resp *http.Response) {
	remaining, err := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining"))
	if err != nil || remaining > lowRemaining {
		return
	}
	wait := resetWait(resp)
	if wait <= 0 {
		wait = time.Minute
	}
	upstream := wait
	wait = c.backOff(wait)
	c.log.Warn("anilist low rate budget; backing off", "remaining", remaining,
		"wait", wait.Round(time.Second),
		"upstream_wait", upstream.Round(time.Second))
}

// --- GraphQL response parsing ---

// gqlMedia is the media object shape shared by the single and batched queries
// (both select id; the single path binds it to the requested identity in
// parseMediaForID, the batch path via retainRequested/parsePageRecords).
type gqlMedia struct {
	Title struct {
		Romaji  string `json:"romaji"`
		English string `json:"english"`
		Native  string `json:"native"`
	} `json:"title"`
	Format    string `json:"format"`
	StartDate struct {
		Year int `json:"year"`
	} `json:"startDate"`
	ID         int `json:"id"`
	SeasonYear int `json:"seasonYear"`
}

// Per-field wire limits. The 1 MiB body cap bounds each response, but the
// decoded strings outlive the request in the matcher's memo and state.json, so
// a compromised upstream could otherwise inflate state and exhaust memory one
// near-cap title at a time. Over-limit fields are rejected, never truncated —
// truncation could forge a false normalized-title match.
const (
	maxTitleBytes  = 1024
	maxFormatBytes = 64
)

// toMedia converts the wire shape to a Media, preferring seasonYear and
// falling back to the start-date year. It rejects a media whose title or
// format field exceeds the wire limits, or that has no usable (non-blank)
// title.
//
// Every rejection here is a function of the RECORD'S OWN CONTENT, so it is
// permanent: the same upstream record fails identically on every future cycle.
// They therefore wrap ErrRecordUnusable, which the matcher treats as a
// definitive answer (negative-memoized, like a not-found) instead of a
// transient outage to retry forever - see that sentinel's doc for why the
// distinction is load-bearing for alerting.
func (m *gqlMedia) toMedia() (Media, error) {
	// One list of the wire title fields, used for both validation and dedupe,
	// so a future title field cannot be validated in one place and dropped in
	// the other.
	wireTitles := []string{m.Title.Romaji, m.Title.English, m.Title.Native}
	for _, t := range wireTitles {
		if len(t) > maxTitleBytes {
			return Media{}, fmt.Errorf("%w: media title exceeds %d bytes", ErrRecordUnusable, maxTitleBytes)
		}
		if unsafeWireText(t) {
			return Media{}, fmt.Errorf("%w: media title contains invalid single-line text", ErrRecordUnusable)
		}
	}
	if len(m.Format) > maxFormatBytes {
		return Media{}, fmt.Errorf("%w: media format exceeds %d bytes", ErrRecordUnusable, maxFormatBytes)
	}
	if unsafeWireText(m.Format) {
		return Media{}, fmt.Errorf("%w: media format contains invalid single-line text", ErrRecordUnusable)
	}
	// Both wire year fields are untrusted, and match.findByTitle treats every
	// nonzero Year as a HARD constraint - so an impossible value (negative, or
	// outside four digits) cannot match a real library year and turns an
	// otherwise usable title into a persistent false negative that Memo also
	// retains as StaleTitle. Map impossible evidence to the existing unknown
	// sentinel 0 instead, falling back through startDate first.
	year := m.SeasonYear
	if !plausibleYear(year) {
		year = m.StartDate.Year
	}
	if !plausibleYear(year) {
		year = 0
	}
	titles := dedupeTitles(wireTitles...)
	if !hasMatchableTitle(titles) {
		return Media{}, fmt.Errorf("%w: media missing usable title", ErrRecordUnusable)
	}
	return Media{Titles: titles, Format: knownFormat(m.Format), Year: year}, nil
}

// plausibleYear reports whether an untrusted wire year is a possible release
// year: a four-digit value. Anything else (0/unset, negative, or out of range)
// carries no usable evidence, and the caller maps it to the unknown sentinel 0
// rather than publishing it as a hard match constraint.
func plausibleYear(year int) bool {
	return year >= 1000 && year <= 9999
}

// anilistFormats is AniList's MediaFormat enum as it applies to anime (its
// MANGA/NOVEL/ONE_SHOT members cannot appear on a SeaDex entry). Matched
// case-insensitively after normalization, mirroring mapping's unexported
// normalizeType canonical form (upper-cased, trimmed).
var anilistFormats = map[string]struct{}{
	"TV": {}, "TV_SHORT": {}, "MOVIE": {}, "SPECIAL": {},
	"OVA": {}, "ONA": {}, "MUSIC": {},
}

// knownFormat returns format when it names a real AniList media format, else
// "" - the value that means "type unknown" to every consumer.
//
// The format is the ONLY arr-routing evidence the AniList fallback carries, and
// match.formatArr routes it by exclusion: MOVIE goes to Radarr and EVERYTHING
// else to Sonarr. An unrecognized non-empty token therefore did not read as
// "unknown", it read as "not a movie" - so a garbled or hostile value like
// "NOT_A_FORMAT" supplied false Sonarr evidence for an entirely unmapped entry,
// removed the Radarr candidate that a title+year match would otherwise have
// left ambiguous, and persisted that wrong match in state.json for the memo's
// life (l-f12). An empty format has always behaved correctly here, leaving the
// arr unknown and the cross-arr candidates ambiguous, so collapsing an
// unrecognized token onto empty is the fix.
//
// Fail direction: a format AniList ADDS in future is unrecognized here and
// degrades to unknown, which costs a title-fallback match its arr hint rather
// than routing it wrongly. That is the safe side, and the length/safety gates
// above still reject a hostile record outright - only the TYPE claim is
// discarded, never the usable titles.
func knownFormat(format string) string {
	if _, ok := anilistFormats[strings.ToUpper(strings.TrimSpace(format))]; ok {
		return format
	}
	return ""
}

// unsafeWireText reports whether an untrusted AniList string field must be
// rejected rather than sanitized or memoized. JSON escapes are valid UTF-8
// wire bytes but may decode to U+FFFD (a lone surrogate), controls, line
// separators, or bidi controls; titlekey.Normalize would strip those runes
// into a forged match key, and both titles and format outlive the request in
// the matcher's memo and state.json, so the two fields share one guard.
func unsafeWireText(s string) bool {
	return strings.ContainsRune(s, utf8.RuneError) || runesafe.SanitizeSingleLine(s) != s
}

// gqlError is the GraphQL error object shared by both response envelopes.
type gqlError struct {
	Message string `json:"message"`
	Status  int    `json:"status"`
}

// maxEnvelopeErrors bounds the untrusted GraphQL errors[] array. The 1 MiB
// body cap alone permits ~350k empty objects, which json.Unmarshal expands
// into []gqlError before any consumer looks at errs[0] (CWE-400, the same
// amplification boundedMediaList exists to close). A real envelope carries a
// handful of errors; an over-cap array is malformed by construction.
const maxEnvelopeErrors = 32

// gqlErrors is the bounded decode of the untrusted errors[] array. A named
// slice type keeps every existing len()/index/range site working while the
// cardinality cap runs BEFORE an element is materialized.
type gqlErrors []gqlError

// UnmarshalJSON implements the bounded element-at-a-time decode described on
// maxEnvelopeErrors. An over-cap array fails the decode, which matches the
// existing policy: transientEnvelopeError already treats an undecodable body
// as "no envelope error" and the parsers already surface it as a plain
// retryable error.
func (l *gqlErrors) UnmarshalJSON(data []byte) error {
	*l = nil
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil
	}
	dec := bounded.NewDecoder(bytes.NewReader(data), 0)
	records, err := bounded.Array(dec, nil, maxEnvelopeErrors, "errors",
		func(e *gqlError) error { return dec.Decode(e) })
	if err != nil {
		return fmt.Errorf("errors: %w", err)
	}
	*l = records
	return nil
}

// gqlResponse is the GraphQL envelope for the media query. Media is a
// json.RawMessage so parseMediaForID can distinguish a missing Media field (a
// malformed or failed response) from an explicit null (AniList's genuine
// not-found), which a typed pointer alone cannot.
type gqlResponse struct {
	Data *struct {
		Media json.RawMessage `json:"Media"`
	} `json:"data"`
	Errors gqlErrors `json:"errors"`
}

// sanitizeUpstreamMessage bounds and cleans an untrusted upstream error
// message before it is wrapped into an error that reaches the logs. The
// message lands inline in a single log line, so the strict single-line
// policy applies (runesafe.SanitizeSingleLine: C0 controls including CR/LF,
// DEL, C1 controls, Unicode line and paragraph separators, and every
// Bidi_Control rune each become a space), and the retained message is capped
// at 200 bytes on a rune boundary via runesafe.CapBytes (truncated output
// appends "...", for a 203-byte maximum) so a long message stays valid
// UTF-8.
func sanitizeUpstreamMessage(s string) string {
	const maxLen = 200
	return runesafe.SanitizeSingleLineBounded(s, maxLen)
}

// mediaQueryError wraps an upstream GraphQL error into the plain
// (non-not-found) query error surfaced to callers.
func mediaQueryError(e gqlError) error {
	return fmt.Errorf("anilist: query error: %s", sanitizeUpstreamMessage(e.Message))
}

// classifyNullMedia maps an explicit Media null plus its error list to the
// error parseMediaForID surfaces: ErrNotFound for no error or AniList's verified
// not-found shape (a sole error with status 404 / message "Not Found."), and a
// plain query error for anything else. Classification runs on the ORIGINAL
// upstream message: sanitizeUpstreamMessage replaces embedded controls and
// bidi marks with spaces, so classifying the sanitized text would let a
// malformed message such as "Not\nFound." launder into the trusted "Not
// Found." sentinel and be negative-memoized. It also does NOT trim the raw
// message: TrimSpace is the same laundering by another route, normalizing
// "\nNot Found.\n" (or the tab/CR forms) into the trusted sentinel. A status
// 404 stays authoritative on its own; the message-only fallback must match the
// verified raw phrase exactly. Only the text rendered into the returned error
// is sanitized.
func classifyNullMedia(errs []gqlError) error {
	if len(errs) == 0 {
		return ErrNotFound
	}
	message := sanitizeUpstreamMessage(errs[0].Message)
	rawNormalized := strings.TrimSuffix(errs[0].Message, ".")
	if len(errs) == 1 && (errs[0].Status == http.StatusNotFound || strings.EqualFold(rawNormalized, "not found")) {
		return fmt.Errorf("%w: %s", ErrNotFound, message)
	}
	return mediaQueryError(errs[0])
}

// validateResponse gates a response body before it reaches encoding/json.
//
// UTF-8: json.Unmarshal replaces malformed UTF-8 inside JSON strings with
// U+FFFD instead of failing, so without this gate a wire title with invalid
// bytes could lossily normalize to a legitimate title key, be title-matched,
// and be memoized even though the upstream payload was not valid JSON text.
// This half stays app-side because it is a CONTENT policy: it matters only
// because the decoded titles outlive the request in the matcher's memo and
// state.json.
//
// Structure: bounded.Preflight owns the rest. encoding/json accepts a
// duplicate object key and applies the LAST occurrence to the struct field,
// discarding the earlier value unseen - which erases the evidence every
// downstream invariant relies on, since a body carrying both a valid Media and
// a later null Media would reach classifyNullMedia as a genuine not-found and
// be negative-memoized, and a batch could have its Page.media replaced by an
// empty array. The preflight also bounds nesting, because json.Decoder.Token
// does not apply encoding/json's own depth limit and an all-opens body would
// otherwise recurse once per byte. Either rejection surfaces as a plain
// retryable error. Shared by parseMediaForID and parseMediaPage.
func validateResponse(raw []byte) error {
	if !utf8.Valid(raw) {
		return errors.New("anilist: response is not valid UTF-8")
	}
	if err := bounded.Preflight(bytes.NewReader(raw)); err != nil {
		return fmt.Errorf("anilist: ambiguous response JSON: %w", err)
	}
	return nil
}

// parseMediaForID decodes the GraphQL envelope into a Media. Only an explicit
// Media null with no error, or AniList's verified not-found error shape
// (a sole error with status 404 / message "Not Found."), is classified as
// ErrNotFound — the matcher negative-memoizes ErrNotFound, so an HTTP-200
// GraphQL failure, a mixed error envelope, a partial response (non-null Media
// alongside field-resolution errors), or a malformed envelope must surface as
// a plain error (degraded, retried next cycle) rather than permanently
// suppressing the id. When expectedID is positive it also enforces the
// single-response identity invariant: a decoded Media whose id differs from
// the requested id is rejected as a plain (transient, non-memoized) error —
// the batch path's retainRequested equivalent for the per-id fallback, so a
// malformed or compromised endpoint cannot answer a request for one id with
// a valid Media for another and have it memoized under the wrong key.
func parseMediaForID(raw []byte, expectedID int) (Media, error) {
	if err := validateResponse(raw); err != nil {
		return Media{}, err
	}
	var r gqlResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return Media{}, fmt.Errorf("anilist: decode response: %s", sanitizeUpstreamMessage(err.Error()))
	}
	mediaRaw, err := mediaPayload(&r)
	if err != nil {
		return Media{}, err
	}
	var media gqlMedia
	if err = json.Unmarshal(mediaRaw, &media); err != nil {
		return Media{}, fmt.Errorf("anilist: decode Media: %s", sanitizeUpstreamMessage(err.Error()))
	}
	if expectedID > 0 && media.ID != expectedID {
		return Media{}, fmt.Errorf("anilist: response media id %d does not match requested id %d", media.ID, expectedID)
	}
	parsed, err := media.toMedia()
	if err != nil {
		return Media{}, fmt.Errorf("anilist: invalid Media: %w", err)
	}
	return parsed, nil
}

// mediaPayload classifies the single-media envelope and returns the raw
// non-null Media value: a missing data/Media field or a GraphQL error fails
// plainly, an explicit null routes to classifyNullMedia (the one path that may
// yield ErrNotFound), and a partial response (non-null Media beside
// field-resolution errors) fails like any other query error, because
// accepting it would memoize incomplete titles/year.
func mediaPayload(r *gqlResponse) (json.RawMessage, error) {
	if r.Data == nil || len(r.Data.Media) == 0 {
		if len(r.Errors) > 0 {
			return nil, mediaQueryError(r.Errors[0])
		}
		return nil, errors.New("anilist: response missing Media")
	}
	mediaRaw := bytes.TrimSpace(r.Data.Media)
	if bytes.Equal(mediaRaw, []byte("null")) {
		return nil, classifyNullMedia(r.Errors)
	}
	if len(r.Errors) > 0 {
		return nil, mediaQueryError(r.Errors[0])
	}
	return mediaRaw, nil
}

// gqlPage is the nullable Page object of the batched query; the Page pointer
// and media's set flag distinguish an explicit empty media array (valid,
// nothing found) from a missing/null Page or media field (malformed response).
type gqlPage struct {
	Media boundedMediaList `json:"media"`
}

// boundedMediaList decodes the untrusted Page.media array element by element
// via json.Decoder, rejecting the element after batchSize BEFORE decoding or
// appending it. The batched query requests perPage=batchSize, so a longer
// array is malformed by construction; without the bound a hostile endpoint
// could pack hundreds of thousands of tiny objects under the 1 MiB body cap
// and json.Unmarshal would expand them all before parsePageRecords validates
// anything (CWE-400 resource exhaustion). A post-decode length check would be
// too late: the allocation has already happened. set stays false for a
// missing field (UnmarshalJSON never runs)
// or an explicit null, both rejected by parseMediaPage as a malformed
// envelope; an explicit empty array sets it with zero records (valid).
//
// The elements are retained RAW and materialized one at a time by
// parsePageRecords, so an element whose field types are out of schema is a
// record-local failure (ErrBatchRecord) like every other per-record defect
// instead of failing the whole envelope - the classification FetchMany reads
// to decide whether the remaining chunks are still worth fetching.
type boundedMediaList struct {
	records []json.RawMessage
	set     bool
}

// UnmarshalJSON implements the bounded element-at-a-time decode described on
// boundedMediaList via jsonx/bounded's Array (cap checked BEFORE the element
// is decoded, so over-cardinality never materializes the excess).
// Over-cardinality is an envelope error (the whole batch fails), not an
// ErrBatchRecord: the response shape itself violates the query's perPage
// contract, so no record in it is trustworthy.
func (l *boundedMediaList) UnmarshalJSON(data []byte) error {
	// encoding/json processes duplicate object keys in order, invoking this
	// method once per occurrence on the same receiver. Reset before each
	// value so a later null cannot retain an earlier array.
	l.records = nil
	l.set = false
	// The explicit null pre-check STAYS app-side: bounded.Array nulls to
	// (nil, nil) by Unmarshal parity, but this field's contract must read
	// null as UNSET (rejected like a missing field), never as a valid empty
	// array.
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil
	}
	dec := bounded.NewDecoder(bytes.NewReader(data), 0)
	records, err := bounded.Array(dec, nil, batchSize, "media",
		func(m *json.RawMessage) error { return dec.Decode(m) })
	if err != nil {
		return fmt.Errorf("media: %w", err)
	}
	l.records = records
	l.set = true
	return nil
}

// gqlPageResponse is the GraphQL envelope for the batched Page(media) query.
type gqlPageResponse struct {
	Data struct {
		Page *gqlPage `json:"Page"`
	} `json:"data"`
	Errors gqlErrors `json:"errors"`
}

// parseMediaPage decodes a batched Page(media) response into a map keyed by
// AniList id. A GraphQL-level error or a missing/null Page or media field
// fails the batch; the record loop's per-record invariants (a decodable
// element, positive id, valid fields, no duplicate ids) live in
// parsePageRecords - a rejected record is skipped and surfaced via an
// ErrBatchRecord-wrapped error alongside the chunk's valid records, so one
// poisoned record cannot discard
// the chunk or read as a total outage - a skipped id is absent from the map
// AND covered by the non-nil error, so the caller never negative-memoizes it,
// and FetchMany distinguishes the record-local failure from an envelope
// failure and keeps fetching later chunks. Ids absent from the media array of
// an error-free response are simply not in the map (the caller treats them as
// not-found).
func parseMediaPage(raw []byte) (map[int]Media, error) {
	if err := validateResponse(raw); err != nil {
		return nil, err
	}
	var r gqlPageResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("anilist: decode batch response: %s", sanitizeUpstreamMessage(err.Error()))
	}
	if len(r.Errors) > 0 {
		return nil, fmt.Errorf("anilist: batch query error: %s", sanitizeUpstreamMessage(r.Errors[0].Message))
	}
	if r.Data.Page == nil {
		return nil, errors.New("anilist: batch response missing Page")
	}
	if !r.Data.Page.Media.set {
		return nil, errors.New("anilist: batch response missing media")
	}
	return parsePageRecords(r.Data.Page.Media.records)
}

// parsePageRecords validates one batch response's record list into a map
// keyed by AniList id: an UNDECODABLE element (a field whose JSON type is out
// of schema) or a record with a non-positive id or rejected fields (toMedia)
// is skipped, and a DUPLICATE id is conflicting untrusted data - two records
// claiming one identity - so NO record for that id is returned
// (the earlier occurrence is deleted and the id stays excluded however many
// duplicates follow) rather than silently letting the last write win. Each
// failure surfaces the first offender via an ErrBatchRecord-wrapped error
// beside the valid sibling records.
func parsePageRecords(media []json.RawMessage) (map[int]Media, error) {
	set := newPageRecordSet(len(media))
	var recordErr error
	for i := range media {
		if err := set.add(media[i], i); err != nil && recordErr == nil {
			recordErr = err
		}
	}
	return set.out, recordErr
}

// pageRecordSet accumulates one batch's accepted records under the duplicate
// rule: an id claimed twice yields NO record for that id, however many
// duplicates follow.
type pageRecordSet struct {
	out  map[int]Media
	seen map[int]bool
}

func newPageRecordSet(n int) *pageRecordSet {
	return &pageRecordSet{out: make(map[int]Media, n), seen: make(map[int]bool, n)}
}

// claim records an identity claim on id and reports whether it is a DUPLICATE.
// A duplicate also drops any value already accepted for that id, so the id
// stays excluded however many duplicates follow.
func (s *pageRecordSet) claim(id int) bool {
	dup := s.seen[id]
	if dup {
		delete(s.out, id)
	}
	s.seen[id] = true
	return dup
}

// add validates one batch element into the set, returning the
// ErrBatchRecord-wrapped reason it was skipped (nil when it was accepted).
func (s *pageRecordSet) add(raw json.RawMessage, i int) error {
	var decoded gqlMedia
	if err := json.Unmarshal(raw, &decoded); err != nil {
		// encoding/json continues populating decodable fields after a type
		// error, so a positive id on an undecodable element is still an
		// identity claim: claim it (dropping any earlier value), so a
		// malformed/well-formed duplicate pair fails closed in either order.
		if decoded.ID > 0 {
			s.claim(decoded.ID)
		}
		return fmt.Errorf("%w media record %d is undecodable: %s", ErrBatchRecord, i, sanitizeUpstreamMessage(err.Error()))
	}
	md := &decoded
	if md.ID <= 0 {
		return fmt.Errorf("%w media record %d missing id", ErrBatchRecord, i)
	}
	if s.claim(md.ID) {
		return fmt.Errorf("%w media record %d duplicates id %d", ErrBatchRecord, i, md.ID)
	}
	parsed, err := md.toMedia()
	if err != nil {
		return fmt.Errorf("%w media record %d (id %d): %v", ErrBatchRecord, i, md.ID, err)
	}
	s.out[md.ID] = parsed
	return nil
}

// dedupeTitles returns the usable (non-blank) titles in order, without
// duplicates; a whitespace-only title cannot key a normalized-title match, so
// it is as unusable as an empty one.
func dedupeTitles(titles ...string) []string {
	seen := make(map[string]struct{}, len(titles))
	var out []string
	for _, t := range titles {
		if strings.TrimSpace(t) == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// hasMatchableTitle reports whether at least one title survives the match
// package's normalized-title key domain (titlekey.Normalize, the shared
// implementation of the lowercased [a-z0-9] key). A payload whose every title
// normalizes to an empty key (punctuation-only, or entirely non-ASCII) would
// parse into a Media that can never match and would be memoized as a
// permanent false negative; erroring instead lets the lookup degrade and
// retry next cycle.
func hasMatchableTitle(titles []string) bool {
	for _, title := range titles {
		if titlekey.Normalize(title) != "" {
			return true
		}
	}
	return false
}

// --- adaptive throttle ---

// throttle spaces requests to a minimum interval, with a penalty hook for
// backing off when the budget is low or a 429 was seen. Each request reserves
// a slot TIMESTAMP (not a fixed sleep duration), and wait revalidates the
// reservation against the shared penalty epoch after sleeping: a penalty
// raised while a reservation was outstanding would otherwise be invisible to
// that waiter, which would wake on its stale pre-penalty slot and spend
// budget inside the reset/Retry-After window the upstream told the client to
// sit out.
type throttle struct {
	next         time.Time
	penaltyUntil time.Time
	interval     time.Duration
	mu           sync.Mutex
}

// wait blocks until this request's reserved slot, or ctx is cancelled. A slot
// that predates a penalty raised after it was reserved is stale: the waiter
// re-reserves at the end of the current schedule (preserving both the penalty
// wait and the configured spacing against sibling waiters) and sleeps again.
func (t *throttle) wait(ctx context.Context) error {
	slot := t.reserveSlot()
	for {
		if err := httpx.SleepCtx(ctx, time.Until(slot)); err != nil {
			return err
		}
		t.mu.Lock()
		if !slot.Before(t.penaltyUntil) {
			t.mu.Unlock()
			return nil
		}
		slot = t.reserveSlotLocked(time.Now())
		t.mu.Unlock()
	}
}

// reserveSlot claims and returns the next slot timestamp. The clock is
// sampled under t.mu so a caller descheduled before acquiring the lock
// cannot schedule from a stale timestamp and hand out already-expired
// slots that break the minimum spacing.
func (t *throttle) reserveSlot() time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.reserveSlotLocked(time.Now())
}

// reserveSlotLocked claims the next slot with t.mu already held.
func (t *throttle) reserveSlotLocked(now time.Time) time.Time {
	start := now
	if t.next.After(now) {
		start = t.next
	}
	t.next = start.Add(t.interval)
	return start
}

// penalize pushes the next slot out by at least d from now and advances the
// penalty epoch, invalidating every outstanding pre-penalty reservation (wait
// re-reserves them after the epoch). A smaller later penalty never shortens
// either the schedule or the epoch.
func (t *throttle) penalize(d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	until := time.Now().Add(d)
	if until.After(t.penaltyUntil) {
		t.penaltyUntil = until
	}
	if until.After(t.next) {
		t.next = until
	}
}
