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

	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/jsonx/bounded"
	"github.com/cplieger/runesafe/v2"
	"github.com/cplieger/seadex-scout/internal/appinfo"
	"github.com/cplieger/seadex-scout/internal/mediatype"
	"github.com/cplieger/seadex-scout/internal/titlekey"
)

// DefaultURL is the AniList GraphQL endpoint (title/format fallback).
const DefaultURL = "https://graphql.anilist.co"

const (
	// DefaultRate is the AniList request/minute ceiling. It is AniList contract
	DefaultRate = 30

	maxBodyBytes = 1 << 20
	maxAttempts  = 3
	baseDelay    = time.Second
	// lowRemaining is the X-RateLimit-Remaining threshold at or below which
	// the client proactively waits for the window reset to avoid a 429.
	lowRemaining = 2
	// defaultRetryAfter is the wait applied when a 429 carries neither a usable
	// Retry-After nor a future X-RateLimit-Reset: AniList's budget is per-minute, so
	// with no upstream evidence the client sits out a whole window.
	defaultRetryAfter = time.Minute
	// maxRetryAfter is the PER-ATTEMPT ceiling: the longest one lookup's retry
	// loop will wait for a rate-limit hint, passed to httpx.WithRateLimitRetry so
	// a pathological header cannot stall a single request. httpx enforces it
	maxRetryAfter = time.Minute
	// maxThrottlePenalty is the POLITENESS ceiling: the longest the shared
	// process-wide throttle will sit out an upstream-stated window, applied in backOff
	// before penalize. It is a SEPARATE number from the per-attempt maxRetryAfter
	// because honouring a stated window and keeping one lookup responsive are different
	// questions; derived as five times that ceiling rather than invented.
	maxThrottlePenalty = 5 * maxRetryAfter
	// implausibleWindow is where an upstream-stated wait stops being a window this
	// client is merely unwilling to honour in full and becomes evidence the header is
	// wrong: AniList's budget is per-minute, so an order of magnitude past the
	// politeness ceiling is a bug or a hostile intermediary, and clamping it silently
	// would leave no trace of the upstream defect.
	implausibleWindow = 10 * maxThrottlePenalty
)

// ErrNotFound reports that AniList has no media for the requested ID.
var ErrNotFound = errors.New("anilist: media not found")

// errBatchRecord marks a record-local validation failure inside an otherwise
// well-formed batch response, distinguishing it from a request/envelope failure so
// FetchMany keeps fetching later chunks instead of reading one poisoned record as a
// total outage. Deliberately unexported: what a CALLER reads is BatchResult.Verdicts.
var errBatchRecord = errors.New("anilist: batch response")

// --- upstream failure classification ---

// retryableUpstreamStatus reports whether an upstream status is a self-healing
// server-side failure worth another attempt: httpx.IsRetryableStatus, narrowed twice.
// 429 is excluded because this client gives a rate limit its own dedicated path (which
// penalizes the shared throttle), and a status of 600 or more is excluded because this
// predicate also reads e.Status out of the untrusted GraphQL errors[] envelope.
func retryableUpstreamStatus(code int) bool {
	if code == http.StatusTooManyRequests || code >= 600 {
		return false
	}
	return httpx.IsRetryableStatus(code)
}

// envelopeErrors decodes the untrusted GraphQL errors[] list, the one shape both
// envelope classifiers read. An undecodable body carries NO envelope error: that
// failure is the parser's business, and the retry boundary must not invent one.
func envelopeErrors(raw []byte) gqlErrors {
	var env struct {
		Errors gqlErrors `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil
	}
	return env.Errors
}

// transientEnvelopeError classifies a GraphQL envelope that arrived with a successful
// HTTP status but reports a SERVER-side failure in its errors[] list. AniList answers
// such faults as 200, so httpx.Do has already declared the attempt successful and no
// retry ever happens; returning a transient error here puts that class back inside the
// budget. Only retryable statuses qualify - a 404 envelope is a genuine not-found.
func transientEnvelopeError(raw []byte) error {
	for _, e := range envelopeErrors(raw) {
		if retryableUpstreamStatus(e.Status) {
			return httpx.MarkTransient(fmt.Errorf("anilist: upstream reported status %d: %s",
				e.Status, sanitizeUpstreamMessage(e.Message)))
		}
	}
	return nil
}

// ErrRecordUnusable marks a rejection determined entirely by the upstream record's OWN
// CONTENT: after every individually defective title has been dropped, no title remains
// that normalizes to a usable match key. It is PERMANENT - the same record fails
// identically on every future cycle - so callers memoize it negatively instead of
// re-fetching forever and escalating a healthy upstream to a standing ERROR. The
// operator's real remedy is an overrides.json entry supplying the arr id directly.
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
	// Format is a canonical internal/mediatype token (the AniList MediaFormat member,
	// upper-cased and trimmed) or "" when the type is unknown; knownFormat enforces
	// that, so every reader can treat the field as bounded safe text.
	Format string
	Titles []string
	Year   int
}

// Verdict is what the batch learned about ONE requested id: the caller reads one
// verdict per id and never reasons about chunks.
type Verdict uint8

const (
	// VerdictUnrequested means no request ever covered this id (the batch aborted at or
	// before its chunk), so the caller may re-batch. It is the ZERO value deliberately:
	// a missing entry, or a nil map after a total failure, must read as "no answer".
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
// through mechanism-shaped channels a caller had to join: media exists, absence is
// definitive, absence proves nothing, or no request covered the id at all. No error
// out of FetchMany carries id sets either, so a caller never joins an error's id lists
// against the media map to work out what it may memoize.
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

// Stats is a snapshot of client activity for cycle observability logs. Calls counts
// outbound HTTP attempts (retries included), so it exceeds the number of logical
// fetches during a 429 episode; RateLimitWaits counts 429s plus proactive backoffs.
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

// cfg holds the resolved tuning knobs for a Client.
type cfg struct {
	logger *slog.Logger
	rate   int
}

// Option tunes a Client built by NewClient.
type Option func(*cfg)

// WithRate sets the request ceiling in requests per minute the client spaces
// itself to. Values <= 0 mean one request per minute, which is also the default.
func WithRate(rate int) Option {
	return func(c *cfg) { c.rate = rate }
}

// WithLogger sets the logger the client's diagnostics go to. Defaults to
// slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(c *cfg) { c.logger = l }
}

// NewClient returns an AniList client for url.
func NewClient(httpClient *http.Client, url string, opts ...Option) *Client {
	c := &cfg{}
	for _, o := range opts {
		if o != nil {
			o(c)
		}
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	if c.rate <= 0 {
		c.rate = 1
	}
	return &Client{
		http:     httpClient,
		log:      c.logger,
		throttle: &throttle{interval: time.Minute / time.Duration(c.rate)},
		url:      url,
	}
}

// Stats returns a snapshot of the cumulative HTTP-attempt and rate-limit-wait
// counts.
func (c *Client) Stats() Stats {
	return Stats{Calls: c.calls.Load(), RateLimitWaits: c.rlWaits.Load()}
}

// request marshals the GraphQL payload and performs one retried POST, returning the
// raw response body. Shared by Fetch and FetchMany. The throttle is claimed INSIDE the
// retry closure so every actual HTTP attempt reserves its own rate slot; a transient
// retry would otherwise re-fire after the backoff alone and exceed the configured
// ceiling. WithRateLimitRetry bounds one attempt's wait at maxRetryAfter while the
// hint rateLimitError carries keeps the longer shared-throttle penalty.
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
			// Classify a server-side failure delivered inside a successful envelope
			// here, at the retry boundary: past this point httpx has recorded the
			// attempt as a success and the class can never be retried.
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
		// Demote httpx's terminal "http retries exhausted" line to Debug: every
		// exhausted request is republished by the matcher with strictly more context,
		// so leaving both at Warn reports one AniList outage twice. Demoting rather
		// than dropping the logger keeps the per-attempt retry diagnostics.
		httpx.WithExhaustedLevel(slog.LevelDebug))
}

// Fetch returns the AniList media for the given ID, or ErrNotFound when AniList has no
// such anime. It throttles before the request and retries transient failures and 429s
// (honoring Retry-After). A non-positive id is rejected without a request.
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

// FetchMany resolves many AniList ids in batched requests (up to batchSize ids each,
// every batch throttled and retried like Fetch), returning a BatchResult whose Media
// holds the media that exist keyed by id and whose Verdicts answers, per REQUESTED id,
// what the batch learned about it.
//
// A TOTAL failure (no chunk completed) returns a zero BatchResult with the error, so
// every id reads VerdictUnrequested and an all-not-found batch is distinguishable from
// an outage. A record-local failure does NOT abort the batch: later chunks are still
// fetched, and an id the current chunk never requested is dropped before the merge
// (retainRequested), so a compromised response cannot overwrite an earlier value.
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

// abortedBatch builds FetchMany's answer for a chunk failure that ABORTS the batch.
// The aborting chunk and every chunk after it keep their VerdictUnrequested zero value,
// so the ids no request covered are nameable without a second id list; completed
// chunks' verdicts ride along so their absences stay definitive evidence.
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

// recordChunkVerdicts records one completed chunk's per-id verdicts: an id the page
// answered is VerdictFound, and every other requested id takes the absent verdict the
// chunk's trustworthiness selected.
func recordChunkVerdicts(verdicts map[int]Verdict, chunk []int, page map[int]Media, absent Verdict) {
	for _, id := range chunk {
		if _, ok := page[id]; ok {
			verdicts[id] = VerdictFound
			continue
		}
		verdicts[id] = absent
	}
}

// fetchBatchChunk fetches and parses one chunk of FetchMany's id list. A request
// failure returns a nil page; otherwise the parsed page is returned alongside the
// joined parse and identity-set errors, a record-local failure included.
func (c *Client) fetchBatchChunk(ctx context.Context, chunk []int) (map[int]Media, error) {
	raw, err := c.request(ctx, batchQuery, map[string]any{"ids": chunk})
	if err != nil {
		return nil, err
	}
	page, parseErr := parseMediaPage(raw)
	return page, errors.Join(parseErr, retainRequested(page, chunk))
}

// retainRequested enforces FetchMany's identity-set invariant on one parsed page:
// every id in the response must have been in the chunk that requested it. An
// unsolicited id is deleted from the page - never merged, where it could inject an
// unrelated Media or overwrite an earlier chunk's value - and one is reported as an
// errBatchRecord error, with the count when several are, keeping the valid records.
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

// do performs one GraphQL POST attempt, translating a 429 into a *httpx.RateLimitError
// carrying a capped Retry-After hint and reading the rate headers on every response
// that is not itself a rate limit, to pre-empt the next 429.
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
	// The budget headers are read on EVERY response that is not itself a rate limit,
	// error statuses included: AniList stamps them on a 4xx/5xx too, and dropping a
	// low-remaining signal there would race the next lookup into the 429 this exists to
	// avoid. AniList also mirrors a not-found into a 404 that still carries the normal
	// envelope, so that body passes through to the parser for Fetch's ErrNotFound.
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

	// ReadLimitedBody closes the body and fails loud with a distinct error on an
	// over-cap body, so an oversized response is not a silently truncated payload.
	respBody, err := httpx.ReadLimitedBody(resp.Body, maxBodyBytes)
	if err != nil {
		c.observeRateHeaders(resp)
		return nil, fmt.Errorf("anilist: read response: %w", err)
	}
	// A 429 AniList reports INSIDE a successful envelope must take the same dedicated
	// rate-limit path as an HTTP 429, or the throttle is never penalized.
	if rlErr := c.envelopeRateLimitError(resp, respBody); rlErr != nil {
		return nil, rlErr
	}
	// Order matters: an envelope-delivered 429 is the SAME response, so observing a
	// low-remaining header before classifying it would penalize twice and count two waits.
	c.observeRateHeaders(resp)
	return respBody, nil
}

// envelopeRateLimitError applies the dedicated 429 path to a rate limit AniList
// reports inside a successful GraphQL envelope. That path only ever ran on the HTTP
// status, so such a 429 surfaced as a terminal query error that penalized nothing.
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

// rateLimitError handles a 429 response: it derives a capped wait from Retry-After (or
// X-RateLimit-Reset, or the default), penalizes the throttle, and returns the
// *httpx.RateLimitError carrying that wait as its retry hint.
func (c *Client) rateLimitError(resp *http.Response) error {
	// ParseRetryAfterResponse, not ParseRetryAfter: the latter caps at 60s INSIDE the
	// library, so a longer stated window could never reach the politeness ceiling and
	// the two 429 shapes would disagree for no reason the upstream expressed.
	wait := httpx.ParseRetryAfterResponse(resp)
	if wait <= 0 {
		// A 429 without a usable Retry-After often still carries the window end in
		// X-RateLimit-Reset, which keeps the bounded attempts out of one rate window.
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
// (maxThrottlePenalty), counts it, and penalizes the shared throttle, returning the
// capped value the caller logs. It is deliberately NOT maxRetryAfter, the per-attempt
// ceiling httpx enforces itself. An absurd value is clamped like any other but also
// warned, since silently rendering a 24h header as minutes leaves no trace.
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

// maxTitleBytes is the per-title wire limit. The 1 MiB body cap bounds each response,
// but a decoded title outlives the request in the matcher's memo and state.json, so a
// compromised upstream could otherwise inflate state one near-cap title at a time. An
// over-limit title is DROPPED from the record's title set, never truncated (which
// could forge a false normalized-title match) and never fatal to the record.
const maxTitleBytes = 1024

// toMedia converts the wire shape to a Media, preferring seasonYear and falling back
// to the start-date year. It DROPS an individual title that exceeds the wire limit or
// is unsafe to memoize, and rejects the record only when no usable (non-blank,
// matchable) title survives. A defective FORMAT field is deliberately NOT a rejection:
// knownFormat collapses it to the unknown sentinel, so a garbled format costs the
// record its arr hint and never its titles. Every rejection here is a function of the
// RECORD'S OWN CONTENT, hence permanent, so they wrap ErrRecordUnusable.
func (m *gqlMedia) toMedia() (Media, error) {
	// One list of the wire title fields, used for both validation and dedupe, so a
	// future title field cannot be validated in one place and dropped in the other.
	// A defective title costs the record THAT TITLE, not its siblings: each of the
	// three is an independent fact, memoized and republished on its own, and the byte
	// cap already stops any single title from inflating state.json.
	wireTitles := make([]string, 0, 3)
	for _, t := range []string{m.Title.Romaji, m.Title.English, m.Title.Native} {
		if len(t) > maxTitleBytes || unsafeWireText(t) {
			continue
		}
		wireTitles = append(wireTitles, t)
	}
	// Both wire year fields are untrusted and Media.Year's contract is a four-digit
	// release year with 0 as its unknown sentinel, so an impossible value is normalized
	// to the sentinel at this one boundary rather than re-checked by every reader.
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

// plausibleYear reports whether an untrusted wire year is a possible release year: a
// four-digit value. Anything else carries no usable evidence, and the caller maps it to
// the unknown sentinel 0 rather than publishing it as a hard match constraint.
func plausibleYear(year int) bool {
	return year >= 1000 && year <= 9999
}

// knownFormat returns the CANONICAL form of format when it names a real AniList media
// format, else "" - Media.Format's own documented "type unknown" value. Returning the
// canonical token rather than the raw wire string is what makes the field bounded and
// single-line-safe by construction, and the accepted vocabulary lives in the shared
// internal/mediatype leaf so this half and the mapping half cannot drift.
//
// It is load-bearing because arr routing reads the format by exclusion (MOVIE routes
// to Radarr, everything else to Sonarr): an unrecognized non-empty token did not read
// as "unknown", it read as "not a movie" and supplied false Sonarr evidence. A format
// AniList adds in future degrades to unknown, which is the safe side.
func knownFormat(format string) string {
	canonical := mediatype.Normalize(format)
	if mediatype.Known(canonical) {
		return canonical
	}
	return ""
}

// unsafeWireText reports whether an untrusted AniList TITLE must be rejected rather
// than sanitized or memoized. JSON escapes are valid UTF-8 wire bytes but may decode to
// U+FFFD, controls, line separators or bidi controls; titlekey.Normalize would strip
// those runes into a forged match key, and a title outlives the request in state.json.
func unsafeWireText(s string) bool {
	return strings.ContainsRune(s, utf8.RuneError) || runesafe.SanitizeSingleLine(s) != s
}

// gqlError is the GraphQL error object shared by both response envelopes.
type gqlError struct {
	Message string `json:"message"`
	Status  int    `json:"status"`
}

// maxEnvelopeErrors bounds the untrusted GraphQL errors[] array. The 1 MiB body cap
// alone permits ~350k empty objects, which json.Unmarshal expands into []gqlError
// before any consumer looks at errs[0] (CWE-400). A real envelope carries a handful.
const maxEnvelopeErrors = 32

// gqlErrors is the bounded decode of the untrusted errors[] array. A named
// slice type keeps every existing len()/index/range site working while the
// cardinality cap runs BEFORE an element is materialized.
type gqlErrors []gqlError

// UnmarshalJSON implements the bounded element-at-a-time decode described on
// maxEnvelopeErrors. An over-cap array fails the decode, which matches the existing
// policy: an undecodable body already reads as "no envelope error". A JSON null needs
// no pre-check here, since a nil slice is exactly this field's null contract.
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

// gqlResponse is the GraphQL envelope for the media query. Media is a json.RawMessage
// so parseMediaForID can distinguish a missing Media field (a malformed or failed
// response) from an explicit null (a genuine not-found), which a pointer alone cannot.
type gqlResponse struct {
	Data *struct {
		Media json.RawMessage `json:"Media"`
	} `json:"data"`
	Errors gqlErrors `json:"errors"`
}

// sanitizeUpstreamMessage bounds and cleans an untrusted upstream error message before
// it is wrapped into an error that reaches the logs: the strict single-line policy
// (controls, line separators and bidi controls become spaces) plus a 200-byte cap on a
// rune boundary. The composition lives in runesafe.SanitizeSingleLineBounded, so a fix
// to the preset belongs there rather than in either app-side wrapper.
func sanitizeUpstreamMessage(s string) string {
	const maxLen = 200
	return runesafe.SanitizeSingleLineBounded(s, maxLen)
}

// mediaQueryError wraps an upstream GraphQL error into the plain
// (non-not-found) query error surfaced to callers.
func mediaQueryError(e gqlError) error {
	return fmt.Errorf("anilist: query error: %s", sanitizeUpstreamMessage(e.Message))
}

// classifyNullMedia maps an explicit Media null plus its error list to the error
// parseMediaForID surfaces: ErrNotFound for no error or AniList's verified not-found
// shape (a sole error with status 404 / message "Not Found."), and a plain query error
// for anything else. Classification runs on the ORIGINAL upstream message and does not
// trim it: either laundering would let a control-bearing message become the trusted
// sentinel and be negative-memoized. Only the returned error's text is sanitized.
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
// UTF-8: json.Unmarshal replaces malformed UTF-8 inside JSON strings with U+FFFD
// instead of failing, so a wire title with invalid bytes could lossily normalize to a
// legitimate title key, be title-matched, and be memoized. That half stays app-side
// because it is a CONTENT policy. Structure: bounded.Preflight owns the rest, because
// encoding/json applies the LAST duplicate object key and discards the earlier value
// unseen, erasing the evidence every downstream invariant relies on.
func validateResponse(raw []byte) error {
	if !utf8.Valid(raw) {
		return errors.New("anilist: response is not valid UTF-8")
	}
	if err := bounded.Preflight(bytes.NewReader(raw)); err != nil {
		return fmt.Errorf("anilist: ambiguous response JSON: %w", err)
	}
	return nil
}

// parseMediaForID decodes the GraphQL envelope into a Media. Only an explicit Media
// null with no error, or AniList's verified not-found error shape, is classified as
// ErrNotFound - the matcher negative-memoizes that, so an HTTP-200 GraphQL failure, a
// partial response or a malformed envelope must surface as a plain retryable error. It
// also enforces the identity invariant unconditionally: a decoded Media whose id
// differs from expectedID is rejected, so no answer is memoized under the wrong key.
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

// mediaPayload classifies the single-media envelope and returns the raw non-null Media
// value: a missing data/Media field or a GraphQL error fails plainly, an explicit null
// routes to classifyNullMedia, and a partial response fails like any other query error
// because accepting it would memoize incomplete titles/year.
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

// boundedMediaList decodes the untrusted Page.media array element by element,
// rejecting the element after batchSize BEFORE decoding or appending it: the query
// requests perPage=batchSize, so a longer array is malformed by construction, and a
// post-decode length check would come after the allocation (CWE-400). set stays false
// for a missing field or an explicit null, both rejected as a malformed envelope.
// Elements are retained RAW, so an out-of-schema one is a record-local failure.
type boundedMediaList struct {
	records []json.RawMessage
	set     bool
}

// UnmarshalJSON implements the bounded element-at-a-time decode described on
// boundedMediaList (the cap is checked BEFORE the element is decoded). Over-cardinality
// is an envelope error, not an errBatchRecord: the response itself violates the query's
// perPage contract, so no record in it is trustworthy.
func (l *boundedMediaList) UnmarshalJSON(data []byte) error {
	// encoding/json processes duplicate object keys in order, invoking this
	// method once per occurrence on the same receiver. Reset before each
	// value so a later null cannot retain an earlier array.
	l.records = nil
	l.set = false
	// The explicit null pre-check STAYS app-side: this field's contract must read null
	// as UNSET (rejected like a missing field), never as a valid empty array.
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

// parseMediaPage decodes a batched Page(media) response into a map keyed by AniList
// id. A GraphQL-level error or a missing/null Page or media field fails the batch; the
// per-record invariants live in parsePageRecords, whose rejected record is skipped and
// surfaced via an errBatchRecord error beside the chunk's valid records, so one
// poisoned record cannot hide the rest or read as a total outage.
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

// parsePageRecords validates one batch response's record list into a map keyed by
// AniList id: an UNDECODABLE element, or a record with a non-positive id or rejected
// fields, is skipped, and a DUPLICATE id is conflicting untrusted data - two records
// claiming one identity - so NO record for that id is returned. The first offender
// surfaces via an errBatchRecord error, with the count when more than one was rejected.
func parsePageRecords(media []json.RawMessage) (map[int]Media, error) {
	set := newPageRecordSet(len(media))
	var recordErr error
	rejected := 0
	for i := range media {
		accepted := len(set.out)
		if err := set.add(media[i], i); err != nil {
			// A duplicate id also invalidates the record already accepted for that id,
			// so charge every record this failure excluded, not just the offender.
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
		// encoding/json continues populating decodable fields after a type error, so a
		// positive id on an undecodable element is still an identity claim: claim it, so
		// a malformed/well-formed duplicate pair fails closed in either order.
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
// normalized-title key domain (internal/titlekey, the leaf both this client and the
// matcher read). A payload whose every title normalizes to an empty key carries no
// usable title and would be memoized as a permanent false negative, so it errors and
// the lookup retries next cycle instead.
func hasMatchableTitle(titles []string) bool {
	for _, title := range titles {
		if titlekey.Normalize(title) != "" {
			return true
		}
	}
	return false
}

// --- adaptive throttle ---

// throttle spaces requests to a minimum interval, with a penalty hook for backing off
// when the budget is low or a 429 was seen. Each request reserves a slot TIMESTAMP
// (not a fixed sleep), and wait revalidates the reservation against the shared penalty
// epoch after sleeping, so a penalty raised meanwhile cannot be invisible to a waiter.
type throttle struct {
	next         time.Time
	penaltyUntil time.Time
	interval     time.Duration
	mu           sync.Mutex
}

// wait blocks until this request's reserved slot, or ctx is cancelled. A slot that
// predates a penalty raised after it was reserved is stale: the waiter re-reserves at
// the end of the current schedule and sleeps again.
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

// reserveSlot claims and returns the next slot timestamp. The clock is sampled under
// t.mu so a caller descheduled before acquiring the lock cannot schedule from a stale
// timestamp and hand out already-expired slots.
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

// penalize pushes the next slot out by at least d from now and advances the penalty
// epoch, invalidating every outstanding pre-penalty reservation. A smaller later
// penalty never shortens either the schedule or the epoch.
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
