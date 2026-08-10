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
	"github.com/cplieger/seadex-scout/internal/mediatype"
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
	// defaultRetryAfter is the wait applied when a 429 carries neither a usable
	// Retry-After nor a future X-RateLimit-Reset. AniList's budget is per-minute,
	// so with no upstream evidence the client sits out a whole window: a shorter
	// value puts every one of maxAttempts attempts AND the shared throttle penalty
	// inside the same window, which is the failure rateLimitError's reset-window
	// branch exists to avoid. It is also observeRateHeaders' fallback when a
	// low-budget response omits the reset header, so both no-evidence paths sit
	// out one window instead of disagreeing by 12x.
	defaultRetryAfter = time.Minute
	// maxRetryAfter is the PER-ATTEMPT ceiling: the longest one lookup's retry
	// loop will wait for a rate-limit hint, passed to httpx.WithRateLimitRetry so
	// a pathological header cannot stall a single request. httpx enforces it
	// itself (it waits min(hint, maxWait)), so nothing in this package needs to
	// clamp the retry path.
	maxRetryAfter = time.Minute
	// maxThrottlePenalty is the POLITENESS ceiling: the longest the shared
	// process-wide throttle will sit out an upstream-stated window, applied in
	// backOff before penalize.
	//
	// It is a SEPARATE number from maxRetryAfter because the two waits answer
	// different questions, and one number serving both was the defect (l-f7).
	// The per-attempt ceiling keeps a single lookup responsive; this one decides
	// how strictly the client honours an upstream that has told it to stop. Held
	// at a minute, a stated window longer than that was honoured for 60s and then
	// discarded - the throttle handed out slots again at the ordinary spacing and
	// the client resumed spending budget inside a window AniList had explicitly
	// closed, so a cold reconcile's remaining prefetch chunks re-probed a
	// rate-limited community index once a minute and every probe returned another
	// 429. This app is deliberately polite to its upstreams everywhere else (a
	// fixed page delay, a User-Agent, a header-adaptive throttle); that was the
	// one place it argued with one.
	//
	// Derived rather than invented (the app's convention for a second threshold):
	// five times the per-attempt ceiling. The cost is bounded and small - the
	// AniList half of a cold reconcile is ~9 batched requests inside a ~25-minute
	// pass, against a 3h health-marker lease - so a few minutes is noise against
	// the deadline while a minute was not enough to honour a real window.
	//
	// Still a CLAMP rather than a plausibility gate, deliberately. Treating an
	// implausible value as absent (falling back to defaultRetryAfter) is the
	// publish-or-drop stance this repo takes for untrusted input elsewhere, and it
	// is the wrong answer here: for a window a little past the ceiling - the
	// likely case - clamping waits the ceiling and is partially rude, while
	// falling back to one minute is MORE rude for the remainder. The clamp is the
	// politer reading near the boundary. What the clamp must not do is hide an
	// absurd value, which is why backOff diagnoses one (see implausibleWindow).
	maxThrottlePenalty = 5 * maxRetryAfter
	// implausibleWindow is when an upstream-stated wait stops being a window this
	// client is merely unwilling to honour in full and becomes evidence the header
	// is wrong: AniList's budget is per-minute, so a stated wait an order of
	// magnitude past the politeness ceiling is a bug or a hostile intermediary,
	// not a rate-limit window. Clamping it silently would render a 24h header as a
	// few minutes and leave no trace of the upstream defect.
	implausibleWindow = 10 * maxThrottlePenalty
)

// ErrNotFound reports that AniList has no media for the requested ID.
var ErrNotFound = errors.New("anilist: media not found")

// errBatchRecord marks a record-local validation failure inside an otherwise
// well-formed batch response, distinguishing it from a request/envelope failure
// so FetchMany keeps fetching later chunks instead of reading one poisoned
// record as a total outage. It is FetchMany's own internal classification and is
// deliberately unexported: what a CALLER reads is BatchResult.Verdicts, which
// already says per id whether the answer is trustworthy. An aborting
// envelope/request error does NOT wrap it (it is joined with an earlier chunk's
// record error when there was one), so errors.Is classifies the FAILURE rather
// than naming a wrapper type.
//
// No error out of FetchMany carries id sets. Which ids a failure makes
// untrustworthy is BatchResult.Verdicts' job (VerdictUnverified for a chunk that
// answered poisoned, VerdictUnrequested for one never asked), so a caller never
// joins an error's id lists against the media map to work out what it may
// memoize. Getting that join wrong was compile-clean and cost ~450
// already-answered ids a rate-limited per-id Fetch each - the ~1700-request cold
// cycle batching exists to avoid.
var errBatchRecord = errors.New("anilist: batch response")

// --- upstream failure classification ---

// retryableUpstreamStatus reports whether an upstream status is a self-healing
// server-side failure worth another attempt. It covers every 5xx (not just
// httpx's 502/503/504) plus 408 Request Timeout; a 4xx other than 408 is the
// client's own fault and never retried, and 429 has its own dedicated
// rate-limit path.
func retryableUpstreamStatus(code int) bool {
	return code == http.StatusRequestTimeout || (code >= 500 && code < 600)
}

// envelopeErrors decodes the untrusted GraphQL errors[] list, the one shape
// both envelope classifiers (transientEnvelopeError, envelopeRateLimitError)
// read. An undecodable body carries NO envelope error: that failure is the
// parser's business, and the retry boundary must not invent one.
func envelopeErrors(raw []byte) gqlErrors {
	var env struct {
		Errors gqlErrors `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil
	}
	return env.Errors
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
	for _, e := range envelopeErrors(raw) {
		if retryableUpstreamStatus(e.Status) {
			return httpx.MarkTransient(fmt.Errorf("anilist: upstream reported status %d: %s",
				e.Status, sanitizeUpstreamMessage(e.Message)))
		}
	}
	return nil
}

// ErrRecordUnusable marks a rejection determined entirely by the upstream
// record's OWN CONTENT: after every individually defective title has been
// dropped, no title remains that normalizes to a usable match key (a CJK-only
// or symbol-only title set, or a set whose every member was over-cap or unsafe
// to memoize). It is PERMANENT - the same record fails identically on every
// future cycle - so it is not an outage and must not be classified as one.
//
// It is deliberately NOT raised for a defective FORMAT field (knownFormat
// collapses that to the unknown sentinel, l-f140) and no longer for an
// individually defective TITLE either: a bad title costs the record that title,
// not its usable siblings (h-f1). Only an EMPTY survivor set reaches here.
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
	// Format is a canonical internal/mediatype token (the AniList MediaFormat
	// member, upper-cased and trimmed) or "" when the type is unknown -
	// including when the wire value was unrecognized, over-long, or not
	// single-line-safe. knownFormat enforces that, so every reader (and the
	// memo that persists it) can treat the field as bounded safe text.
	Format string
	Titles []string
	Year   int
}

// Verdict is what the batch learned about ONE requested id. It replaces the
// Completed / UnverifiedIDs / UnrequestedIDs join a caller used to compute from
// three separate channels: the caller reads one verdict per id and never
// reasons about chunks.
type Verdict uint8

const (
	// VerdictUnrequested means no request ever covered this id (the batch
	// aborted at or before its chunk). No answer exists yet, so the caller may
	// re-batch. It is the ZERO value deliberately - an id missing from the map,
	// or a nil map after a total failure, must read as "no answer" rather than
	// as an answer nobody produced.
	VerdictUnrequested Verdict = iota
	// VerdictFound means BatchResult.Media holds the media for this id.
	VerdictFound
	// VerdictAbsent means a completed chunk answered definitively - AniList has
	// no such anime. Safe to memoize negatively.
	VerdictAbsent
	// VerdictUnverified means this id's chunk answered, but not trustworthily (a
	// record-local defect). Absence proves nothing; re-asking returns the same
	// poisoned record, so the caller falls back per id.
	VerdictUnverified
)

// BatchResult is what FetchMany resolved, answered PER REQUESTED ID rather than
// through a set of mechanism-shaped channels the caller had to join (l-f5,
// l-f135). The outcomes a caller must tell apart are then all one value: media
// exists, absence is definitive, absence proves nothing, or no request covered
// the id at all. Reading the old nil-versus-empty convention (and later the
// Completed / UnverifiedIDs / UnrequestedIDs join) backwards was compile-clean
// and flipped hundreds of ids between "retry per-id" and "negative-memoize for
// the memo's TTL".
type BatchResult struct {
	// Media holds the media that exist, keyed by AniList id. An id AniList has
	// no anime for is absent; whether that absence is trustworthy evidence is
	// what Verdicts answers.
	Media map[int]Media
	// Verdicts carries exactly one entry per id passed to FetchMany, except
	// after a TOTAL failure (no chunk completed at all), where it is empty and
	// every id therefore reads VerdictUnrequested from the zero value.
	Verdicts map[int]Verdict
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
// maxRetryAfter — the PER-ATTEMPT ceiling, so one lookup can never stall on a
// long hint. rateLimitError clamps the hint it carries to the longer
// maxThrottlePenalty instead, because that value also becomes the shared
// throttle's penalty, and how long the client honours a stated window is a
// different question from how long one attempt may block (l-f7). httpx waits
// min(hint, maxRetryAfter), so the two ceilings compose without either needing
// to know the other: the attempt waits a minute at most, while later callers
// retain the full penalty.
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
		httpx.WithRateLimitRetry(maxRetryAfter),
		// Demote httpx's terminal "http retries exhausted" line to Debug. Every
		// exhausted request is republished by the matcher with strictly more
		// context: a total batch outage as "anilist batch prefetch failed;
		// skipping per-id fallback for pending ids" (match's prefetch, carrying
		// the pending count) and a per-id miss as "anilist fallback failed"
		// (carrying al_id, plus the repeated-failure gate line), and a sustained
		// outage escalates to ERROR with consecutive_anilist_degraded
		// (scout.recordAniListDegradation). Leaving both at Warn reports one
		// AniList outage twice, generic line first. request is SHARED by Fetch
		// (per-id) and FetchMany (batch), so this covers both paths - wider than
		// the single call path the finding named, and correct, because both
		// already publish their own contextual record. Demoting rather than
		// dropping the logger keeps the per-attempt retry diagnostics - the same
		// rule internal/seadex and internal/indexer's Prowlarr door already
		// apply (l-f20).
		httpx.WithExhaustedLevel(slog.LevelDebug))
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
// each, every batch throttled and retried like Fetch), returning a BatchResult
// whose Media holds the media that exist keyed by id and whose Verdicts answers,
// per REQUESTED id, what the batch learned about it.
//
// Verdicts is the whole caller contract: VerdictFound and VerdictAbsent are the
// definitive answers a completed chunk produced, VerdictUnverified marks an id
// whose chunk answered untrustworthily, and VerdictUnrequested marks one no
// request ever covered - the aborting chunk and every chunk after it - which a
// caller can re-batch instead of falling back one id at a time. A TOTAL failure
// (no chunk completed) returns a zero BatchResult with the error, so every id
// reads VerdictUnrequested and the caller can tell an all-not-found batch apart
// from an outage without a completion flag.
//
// A record-local failure (errBatchRecord, a poisoned record inside an otherwise
// well-formed response) does NOT abort the batch: the chunk still counts as
// completed, later chunks are still fetched,
// and the first record error is surfaced alongside the merged result, so one
// malformed record cannot hide every id after it or read as a total outage to
// the caller. The response is untrusted: an id the current chunk never
// requested is dropped before the merge (retainRequested) and surfaced like
// any other record-local failure, so a malformed or compromised response
// cannot inject an unrelated Media or overwrite an earlier chunk's value.
func (c *Client) FetchMany(ctx context.Context, ids []int) (BatchResult, error) {
	out := make(map[int]Media, len(ids))
	verdicts := make(map[int]Verdict, len(ids))
	completed := false
	var firstRecordErr error
	for chunk := range slices.Chunk(ids, batchSize) {
		page, err := c.fetchBatchChunk(ctx, chunk)
		maps.Copy(out, page)
		if err != nil && !errors.Is(err, errBatchRecord) {
			return abortedBatch(out, verdicts, completed, err, firstRecordErr)
		}
		completed = true
		// A record-local chunk's ABSENCES prove nothing, but the records it did
		// return are still valid - so a found id is VerdictFound whichever kind
		// of chunk answered it, and only the absences differ.
		absent := VerdictAbsent
		if err != nil {
			absent = VerdictUnverified
			if firstRecordErr == nil {
				firstRecordErr = err
			}
		}
		recordChunkVerdicts(verdicts, chunk, page, absent)
	}
	if firstRecordErr != nil {
		return BatchResult{Media: out, Verdicts: verdicts}, firstRecordErr
	}
	return BatchResult{Media: out, Verdicts: verdicts}, nil
}

// abortedBatch builds FetchMany's answer for a chunk failure that ABORTS the
// batch. The aborting chunk and every chunk after it keep their
// VerdictUnrequested zero value, so the ids no request covered are nameable
// without a second id list. With nothing completed yet the whole call is a total
// failure; otherwise the completed chunks' verdicts ride along so their absences
// stay definitive evidence. An earlier chunk's record diagnostic is preserved
// with the abort leading, since the abort is the classification a caller reads
// first.
func abortedBatch(
	out map[int]Media, verdicts map[int]Verdict, completed bool, err, firstRecordErr error,
) (BatchResult, error) {
	if firstRecordErr != nil {
		err = errors.Join(err, firstRecordErr)
	}
	if !completed {
		return BatchResult{}, err
	}
	return BatchResult{Media: out, Verdicts: verdicts}, err
}

// recordChunkVerdicts records one completed chunk's per-id verdicts: an id the
// page answered is VerdictFound, and every other requested id takes the absent
// verdict the chunk's trustworthiness selected (VerdictAbsent for a clean chunk,
// VerdictUnverified for a record-local failure).
func recordChunkVerdicts(verdicts map[int]Verdict, chunk []int, page map[int]Media, absent Verdict) {
	for _, id := range chunk {
		if _, ok := page[id]; ok {
			verdicts[id] = VerdictFound
			continue
		}
		verdicts[id] = absent
	}
}

// fetchBatchChunk fetches and parses one chunk of FetchMany's id list. A
// request failure returns a nil page (nothing to merge); otherwise the parsed
// page is returned alongside the joined parse and identity-set errors, so
// FetchMany's caller-facing contract logic reads as one linear loop. A
// record-local failure (errBatchRecord) still returns the chunk's valid
// records, matching FetchMany's does-not-abort-the-batch rule.
func (c *Client) fetchBatchChunk(ctx context.Context, chunk []int) (map[int]Media, error) {
	raw, err := c.request(ctx, batchQuery, map[string]any{"ids": chunk})
	if err != nil {
		return nil, err
	}
	page, parseErr := parseMediaPage(raw)
	return page, errors.Join(parseErr, retainRequested(page, chunk))
}

// retainRequested enforces FetchMany's identity-set invariant on one parsed
// page: every id in the response must have been in the chunk that requested
// it. An unsolicited id is deleted from the page - never merged, where it
// could inject an unrelated Media or overwrite a value an earlier chunk
// legitimately resolved - and one such id (the first encountered in map
// iteration order, so arbitrary when several are unsolicited) is reported as
// an errBatchRecord-wrapped error so the caller sees the malformed response
// without losing the chunk's valid records. When MORE than one id is
// unsolicited the count rides the error too, so one stray id reads differently
// from a wholesale identity-set violation.
func retainRequested(page map[int]Media, chunk []int) error {
	requested := make(map[int]struct{}, len(chunk))
	for _, id := range chunk {
		requested[id] = struct{}{}
	}
	var first error
	unsolicited := 0
	for id := range page {
		if _, ok := requested[id]; ok {
			continue
		}
		delete(page, id)
		unsolicited++
		if first == nil {
			first = fmt.Errorf("%w unexpected media id %d", errBatchRecord, id)
		}
	}
	if unsolicited > 1 {
		// The magnitude separates one stray id from a wholesale identity-set
		// violation; the named id alone reads identically for both.
		first = fmt.Errorf("%w (%d unsolicited ids dropped)", first, unsolicited)
	}
	return first
}

// do performs one GraphQL POST attempt, translating a 429 into a
// *httpx.RateLimitError carrying a capped Retry-After hint (retried by
// request's WithRateLimitRetry mode) and reading the rate headers on every
// response that is not itself a rate limit (error statuses included, an
// envelope-delivered 429 excluded) to pre-empt the next 429.
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
	// exactly once.
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
			return nil, httpx.MarkTransient(statusErr)
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
	// Order matters: an envelope-delivered 429 is the SAME rate-limit response,
	// so observing a low-remaining header before classifying it penalizes the
	// throttle through both paths and reports one response as two
	// Stats().RateLimitWaits (pinned by TestEnvelopeRateLimitCountsOneWait).
	c.observeRateHeaders(resp)
	return respBody, nil
}

// envelopeRateLimitError applies the dedicated 429 path to a rate limit
// AniList reports inside a successful GraphQL envelope. retryableUpstreamStatus
// deliberately excludes 429 because 429 has its own path - but that path only
// ever ran on the HTTP status, so an envelope-delivered 429 surfaced as a
// terminal query error that penalized nothing.
func (c *Client) envelopeRateLimitError(resp *http.Response, raw []byte) error {
	for _, e := range envelopeErrors(raw) {
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
	// ParseRetryAfterResponse, not ParseRetryAfter: the latter caps at
	// httpx.RetryAfterCap (60s) INSIDE the library, so it could never deliver a
	// longer stated window to the politeness ceiling at all - the Retry-After path
	// would stay truncated at a minute while the X-RateLimit-Reset path honoured
	// the full window, and the two 429 shapes would disagree for no reason the
	// upstream expressed. The library documents this accessor for exactly this
	// choice ("preserves the raw duration so callers can make their own
	// decisions"); backOff applies the app's own ceiling, and httpx still caps the
	// per-ATTEMPT wait independently via WithRateLimitRetry (l-f7).
	wait := httpx.ParseRetryAfterResponse(resp)
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

// backOff caps an upstream-supplied wait at the POLITENESS ceiling
// (maxThrottlePenalty), counts it, and penalizes the shared throttle, returning
// the capped value the caller logs. It is the one place that ceiling is applied,
// so no back-off path can hand throttle.penalize an unbounded upstream duration.
//
// It is deliberately NOT maxRetryAfter: that is the per-attempt ceiling and
// httpx enforces it inside the retry loop on its own, so clamping to it here only
// ever shortened how long the client honoured a window every LATER lookup shares
// (l-f7).
//
// An absurd value is clamped like any other but also reported, because silently
// rendering a 24h header as a few minutes leaves no trace of an upstream defect.
// Warn, not error: a bogus header is the upstream's problem and the clamp already
// contains it, so it needs no operator action here.
func (c *Client) backOff(wait time.Duration) time.Duration {
	if wait >= implausibleWindow {
		c.log.Warn("anilist stated a rate-limit window too long to be one; honouring the politeness ceiling instead",
			"stated", wait, "ceiling", maxThrottlePenalty)
	}
	wait = min(wait, maxThrottlePenalty)
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
		wait = defaultRetryAfter
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

// maxTitleBytes is the per-title wire limit. The 1 MiB body cap bounds each
// response, but a decoded title outlives the request in the matcher's memo and
// state.json, so a compromised upstream could otherwise inflate state and
// exhaust memory one near-cap title at a time. An over-limit title is DROPPED
// from the record's title set, never truncated — truncation could forge a false
// normalized-title match — and never fatal to the record, whose three titles are
// independent facts (see toMedia, h-f1). The cap still does its job: it bounds
// what any single title can contribute to the memo.
//
// The format field needs no such cap: knownFormat admits only a member of
// AniList's own MediaFormat enum and publishes it in mediatype's canonical
// form, so Media.Format is by construction one of seven short ASCII tokens or
// the empty unknown sentinel, whatever the wire sent (l-f140).
const maxTitleBytes = 1024

// toMedia converts the wire shape to a Media, preferring seasonYear and
// falling back to the start-date year. It DROPS an individual title that
// exceeds the wire limit or is unsafe to memoize, and rejects the record only
// when no usable (non-blank, matchable) title survives.
//
// A defective FORMAT field is deliberately NOT a rejection: knownFormat already
// collapses anything that is not a member of AniList's own MediaFormat enum to
// the unknown sentinel "", so a hostile or garbled format value costs the
// record only its arr hint and never its usable titles (l-f140). Rejecting the
// whole record for it was strictly worse, because ErrRecordUnusable is a
// DEFINITIVE answer the matcher negative-memoizes: a record whose only defect
// was a stray control rune or an over-long format string could never be
// title-matched again until the memo expired, with an overrides.json entry as
// the operator's only remedy - while the neighbouring defect class (an
// unrecognized token like "NOT_A_FORMAT") kept its titles.
//
// Every remaining rejection here is a function of the RECORD'S OWN CONTENT, so
// it is permanent: the same upstream record fails identically on every future
// cycle. They therefore wrap ErrRecordUnusable, which the matcher treats as a
// definitive answer (negative-memoized, like a not-found) instead of a
// transient outage to retry forever - see that sentinel's doc for why the
// distinction is load-bearing for alerting.
//
// Every gate here is a fact about the WIRE record or about Media's own
// published contract (its byte cap, its safe-text rule, its unknown sentinels
// "" and 0, and the shared key domains in internal/mediatype and
// internal/titlekey that a token/title must fall inside to be usable at all) -
// deliberately not a consumer's weighting of evidence, and no rule here is
// stated in terms of a match symbol. The runner-up shape (l-f95) was moving the
// title/year/format rules out to a consumer-side evidence adapter in match; it
// was declined because these are normalizations of untrusted input into this
// package's OWN sentinels, and a consumer-side adapter would have to be applied
// by every reader of a Media (the matcher, the memo it persists, and the feed's
// stale-title tier) instead of once at the boundary that produced it.
func (m *gqlMedia) toMedia() (Media, error) {
	// One list of the wire title fields, used for both validation and dedupe,
	// so a future title field cannot be validated in one place and dropped in
	// the other.
	//
	// A defective title costs the record THAT TITLE, not its siblings. Each of
	// the three is an independent fact: each is memoized and republished on its
	// own, and the byte cap exists so no SINGLE title can inflate state.json -
	// so nothing about one bad alias requires the other two to die with it. This
	// is the rule knownFormat already states for the neighbouring field ("only
	// the TYPE claim is ever discarded, never the record's usable titles"), and
	// rejecting the whole record broke it: an English title AniList happened to
	// serve with a stray control rune made an anime permanently unmatchable -
	// negative-memoized, no finding, and a movie routed to Sonarr for want of a
	// format hint - with an overrides.json entry as the operator's only remedy
	// (h-f1). The failure was silent, which is what made it worth closing.
	//
	// The 3 x maxTitleBytes raw bound is unchanged: dropping members can only
	// shrink the survivor set, never grow it.
	wireTitles := make([]string, 0, 3)
	for _, t := range []string{m.Title.Romaji, m.Title.English, m.Title.Native} {
		if len(t) > maxTitleBytes || unsafeWireText(t) {
			continue
		}
		wireTitles = append(wireTitles, t)
	}
	// Both wire year fields are untrusted, and Media.Year's contract is a
	// four-digit release year with 0 as its documented unknown sentinel - so an
	// impossible value (negative, or outside four digits) is not usable evidence
	// for ANY consumer: it cannot match a real library year, and it outlives the
	// request in the matcher's memo and state.json (Memo.StaleTitle republishes
	// it to the feed's stale-title tier). Normalizing it to the sentinel at this
	// one boundary is what keeps every reader from having to re-check it.
	// Falling back through startDate first.
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

// knownFormat returns the CANONICAL form of format when it names a real AniList
// media format, else "" - Media.Format's own documented "type unknown" value.
//
// Returning the canonical token rather than the raw wire string is what makes
// Media.Format bounded and single-line-safe by construction (l-f140): every
// value it can carry is one of internal/mediatype's seven short ASCII members
// or the empty sentinel, so no byte cap and no unsafeWireText gate is needed on
// the wire field, and a record whose only defect is its format keeps its usable
// titles instead of being rejected outright.
//
// The accepted vocabulary and its canonical form live in the shared
// internal/mediatype leaf, not in a private copy here (l-f87): the token this
// function admits is fed verbatim into a mapping Record.Type and classified
// there, so both halves must agree on the token set AND on the canonical form,
// and one shared home is what makes that structural instead of two mirrored
// copies drifting silently.
//
// This gate is a wire-shape fact - does the token name a member of AniList's
// own MediaFormat enum - so it stays at this boundary, and "" is this package's
// published unknown sentinel, not a consumer's convention. It is load-bearing
// downstream because arr routing reads the format by exclusion (MOVIE routes to
// Radarr, everything else to Sonarr), so an unrecognized non-empty token did
// not read as "unknown", it read as "not a movie" - a garbled or hostile value
// like "NOT_A_FORMAT" supplied false Sonarr evidence for an entirely unmapped
// entry, removed the Radarr candidate that a title+year match would otherwise
// have left ambiguous, and persisted that wrong match in state.json for the
// memo's life (l-f12). An empty format has always behaved correctly, leaving
// the arr unknown and the cross-arr candidates ambiguous, so collapsing an
// unrecognized token onto empty is the fix.
//
// Fail direction: a format AniList ADDS in future is unrecognized here and
// degrades to unknown, which costs a title-fallback match its arr hint rather
// than routing it wrongly. That is the safe side, and only the TYPE claim is
// ever discarded, never the record's usable titles.
func knownFormat(format string) string {
	canonical := mediatype.Normalize(format)
	if mediatype.Known(canonical) {
		return canonical
	}
	return ""
}

// unsafeWireText reports whether an untrusted AniList TITLE must be rejected
// rather than sanitized or memoized. JSON escapes are valid UTF-8 wire bytes
// but may decode to U+FFFD (a lone surrogate), controls, line separators, or
// bidi controls; titlekey.Normalize would strip those runes into a forged match
// key, and a title outlives the request in the matcher's memo and state.json.
// The format field needs no such guard: knownFormat republishes it as a
// canonical mediatype token, so an unsafe rune can only make the token
// unrecognized (l-f140).
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
//
// A JSON null needs no pre-check here: bounded.Array reports it through
// Decoder.Open (ok=false, no error) and yields a nil slice, which is exactly
// this field's null contract — unlike boundedMediaList, which must read null as
// UNSET and therefore keeps its own pre-check.
func (l *gqlErrors) UnmarshalJSON(data []byte) error {
	*l = nil
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
// UTF-8. The composition lives in the library
// (runesafe.SanitizeSingleLineBounded), shared with internal/indexer's
// capLogText - a fix to the single-line bounded preset belongs there, not in
// either app-side wrapper (internal/logattr owns the separate
// structured-attribute policy; see its package doc for the split).
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
// suppressing the id. It also enforces the single-response identity invariant
// unconditionally: a decoded Media whose id differs from expectedID is
// rejected as a plain (transient, non-memoized) error, so a caller passing 0
// asserts the record carries no id —
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
	if media.ID != expectedID {
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
// record-local failure (errBatchRecord) like every other per-record defect
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
// errBatchRecord: the response shape itself violates the query's perPage
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
// errBatchRecord-wrapped error alongside the chunk's valid records, so one
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
// failure surfaces the first offender via an errBatchRecord-wrapped error
// beside the valid sibling records. When MORE than one record is rejected the
// count rides the error too, so one poisoned record reads differently from a
// wholesale schema drift.
func parsePageRecords(media []json.RawMessage) (map[int]Media, error) {
	set := newPageRecordSet(len(media))
	var recordErr error
	rejected := 0
	for i := range media {
		accepted := len(set.out)
		if err := set.add(media[i], i); err != nil {
			// A duplicate id also invalidates the record already accepted for
			// that id, so charge every record this failure excluded - the
			// offender plus whatever it retracted - or the magnitude signal
			// would under-report a conflict as a single poisoned record.
			rejected += 1 + accepted - len(set.out)
			if recordErr == nil {
				recordErr = err
			}
		}
	}
	if rejected > 1 {
		// The first offender alone cannot tell one poisoned record apart from a
		// wholesale schema drift; the count is the operator's magnitude signal.
		recordErr = fmt.Errorf("%w (%d of %d records rejected)", recordErr, rejected, len(media))
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
// errBatchRecord-wrapped reason it was skipped (nil when it was accepted).
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
		return fmt.Errorf("%w media record %d is undecodable: %s", errBatchRecord, i, sanitizeUpstreamMessage(err.Error()))
	}
	if decoded.ID <= 0 {
		return fmt.Errorf("%w media record %d missing id", errBatchRecord, i)
	}
	if s.claim(decoded.ID) {
		return fmt.Errorf("%w media record %d duplicates id %d", errBatchRecord, i, decoded.ID)
	}
	parsed, err := decoded.toMedia()
	if err != nil {
		return fmt.Errorf("%w media record %d (id %d): %v", errBatchRecord, i, decoded.ID, err)
	}
	s.out[decoded.ID] = parsed
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

// hasMatchableTitle reports whether at least one title falls inside the shared
// normalized-title key domain (internal/titlekey, the dependency-free leaf both
// this client and the matcher read, holding the lowercased [a-z0-9] key). A
// payload whose every title normalizes to an empty key (punctuation-only, or
// entirely non-ASCII) carries no usable title at all: it would parse into a
// Media that can never key anything and would be memoized as a permanent false
// negative; erroring instead lets the lookup degrade and retry next cycle.
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
