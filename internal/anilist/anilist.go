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

	"github.com/cplieger/httpx/v3"
	"github.com/cplieger/runesafe"
	"github.com/cplieger/seadex-scout/internal/appinfo"
	"github.com/cplieger/seadex-scout/internal/titlekey"
)

const (
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

// errBatchRecord marks a record-local validation failure inside an otherwise
// well-formed batch response, distinguishing it from a request/envelope
// failure so FetchMany can keep fetching later chunks instead of reading one
// poisoned record as a total outage.
var errBatchRecord = errors.New("anilist: batch response")

// query fetches the fields needed for a title fallback match.
const query = `query ($id: Int) { Media(id: $id, type: ANIME) { format seasonYear startDate { year } title { romaji english native } } }`

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
			return c.do(ctx, body)
		},
		httpx.WithMaxAttempts(maxAttempts),
		httpx.WithBaseDelay(baseDelay),
		httpx.WithLabel("anilist"),
		httpx.WithLogger(c.log),
		httpx.WithRateLimitRetry(maxRetryAfter))
}

// Fetch returns the AniList media for the given ID, or ErrNotFound when AniList
// has no such anime. It throttles before the request and retries transient
// failures and 429s (honoring Retry-After).
func (c *Client) Fetch(ctx context.Context, aniListID int) (Media, error) {
	raw, err := c.request(ctx, query, map[string]int{"id": aniListID})
	if err != nil {
		return Media{}, err
	}
	return parseMedia(raw)
}

// FetchMany resolves many AniList ids in batched requests (up to batchSize ids
// each, every batch throttled and retried like Fetch), returning the media that
// exist keyed by id. An id AniList has no anime for is simply absent from the
// result (the caller treats an absent id as not-found). On a request or
// envelope error it returns the media gathered so far together with the error,
// so the caller can fall back to a per-id Fetch for the remainder rather than
// losing the batch. A record-local failure (errBatchRecord, a poisoned record
// inside an otherwise well-formed response) does NOT abort the batch: later
// chunks are still fetched and the first record error is surfaced alongside
// the merged result, so one malformed record cannot hide every id after it or
// read as a total outage to the caller. The response is untrusted: an id the
// current chunk never requested is dropped before the merge (retainRequested)
// and surfaced like any other record-local failure, so a malformed or
// compromised response cannot inject an unrelated Media or overwrite an
// earlier chunk's value.
func (c *Client) FetchMany(ctx context.Context, ids []int) (map[int]Media, error) {
	out := make(map[int]Media, len(ids))
	var firstRecordErr error
	for chunk := range slices.Chunk(ids, batchSize) {
		raw, err := c.request(ctx, batchQuery, map[string]any{"ids": chunk})
		if err != nil {
			return out, err
		}

		page, parseErr := parseMediaPage(raw)
		retainErr := retainRequested(page, chunk)
		maps.Copy(out, page)
		if err := errors.Join(parseErr, retainErr); err != nil {
			if !errors.Is(err, errBatchRecord) {
				return out, err
			}
			if firstRecordErr == nil {
				firstRecordErr = err
			}
		}
	}
	return out, firstRecordErr
}

// retainRequested enforces FetchMany's identity-set invariant on one parsed
// page: every id in the response must have been in the chunk that requested
// it. An unsolicited id is deleted from the page - never merged, where it
// could inject an unrelated Media or overwrite a value an earlier chunk
// legitimately resolved - and the first such id is reported as an
// errBatchRecord-wrapped error so the caller sees the malformed response
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
			first = fmt.Errorf("%w unexpected media id %d", errBatchRecord, id)
		}
	}
	return first
}

// do performs one GraphQL POST attempt, translating a 429 into a
// *httpx.RateLimitError carrying a capped Retry-After hint (retried by
// request's WithRateLimitRetry mode) and reading the rate headers to pre-empt
// the next 429.
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
	// AniList mirrors a GraphQL-level not-found into the HTTP status: a
	// nonexistent id answers 404 while still carrying the normal envelope
	// {"data":{"Media":null},"errors":[{"message":"Not Found."}]} (verified
	// live). Pass the 404 body through to the parser so Fetch can honor its
	// ErrNotFound contract instead of surfacing an opaque HTTP 404.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		httpx.DrainClose(resp.Body)
		if statusErr := httpx.CheckHTTPStatus(resp); statusErr != nil {
			return nil, statusErr
		}
		return nil, fmt.Errorf("anilist: unexpected status %d", resp.StatusCode)
	}

	c.observeRateHeaders(resp)
	// ReadLimitedBody closes the body and fails loud with a distinct
	// *httpx.ResponseTooLargeError on an over-cap body, so an oversized
	// response surfaces as its own error rather than a silently truncated
	// payload that only fails later as a confusing JSON decode error.
	respBody, err := httpx.ReadLimitedBody(resp.Body, maxBodyBytes)
	if err != nil {
		return nil, fmt.Errorf("anilist: read response: %w", err)
	}
	return respBody, nil
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
	c.rlWaits.Add(1)
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
	wait = min(wait, maxRetryAfter)
	c.log.Warn("anilist rate limited (429); backing off", "retry_after", wait.Round(time.Second))
	c.throttle.penalize(wait)
	return &httpx.RateLimitError{Msg: "anilist: rate limited (429)", RetryAfter: wait}
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
	wait = min(wait, maxRetryAfter)
	c.rlWaits.Add(1)
	c.log.Warn("anilist low rate budget; backing off", "remaining", remaining, "wait", wait.Round(time.Second))
	c.throttle.penalize(wait)
}

// --- GraphQL response parsing ---

// gqlMedia is the media object shape shared by the single and batched queries
// (the single query returns no id; the field stays zero there).
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
// title, so a malformed payload degrades and is retried next cycle instead of
// being memoized as a permanent empty or bloated Media.
func (m *gqlMedia) toMedia() (Media, error) {
	for _, t := range []string{m.Title.Romaji, m.Title.English, m.Title.Native} {
		if len(t) > maxTitleBytes {
			return Media{}, fmt.Errorf("media title exceeds %d bytes", maxTitleBytes)
		}
	}
	if len(m.Format) > maxFormatBytes {
		return Media{}, fmt.Errorf("media format exceeds %d bytes", maxFormatBytes)
	}
	year := m.SeasonYear
	if year == 0 {
		year = m.StartDate.Year
	}
	titles := dedupeTitles(m.Title.Romaji, m.Title.English, m.Title.Native)
	if !hasMatchableTitle(titles) {
		return Media{}, errors.New("media missing usable title")
	}
	return Media{Titles: titles, Format: m.Format, Year: year}, nil
}

// gqlError is the GraphQL error object shared by both response envelopes.
type gqlError struct {
	Message string `json:"message"`
	Status  int    `json:"status"`
}

// gqlResponse is the GraphQL envelope for the media query. Media is a
// json.RawMessage so parseMedia can distinguish a missing Media field (a
// malformed or failed response) from an explicit null (AniList's genuine
// not-found), which a typed pointer alone cannot.
type gqlResponse struct {
	Data *struct {
		Media json.RawMessage `json:"Media"`
	} `json:"data"`
	Errors []gqlError `json:"errors"`
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
	s = runesafe.SanitizeSingleLine(s)
	if len(s) > maxLen {
		s = runesafe.CapBytes(s, maxLen) + "..."
	}
	return s
}

// mediaQueryError wraps an upstream GraphQL error into the plain
// (non-not-found) query error surfaced to callers.
func mediaQueryError(e gqlError) error {
	return fmt.Errorf("anilist: query error: %s", sanitizeUpstreamMessage(e.Message))
}

// classifyNullMedia maps an explicit Media null plus its error list to the
// error parseMedia surfaces: ErrNotFound for no error or AniList's verified
// not-found shape (a sole error with status 404 / message "Not Found."), and a
// plain query error for anything else.
func classifyNullMedia(errs []gqlError) error {
	if len(errs) == 0 {
		return ErrNotFound
	}
	message := sanitizeUpstreamMessage(errs[0].Message)
	normalized := strings.TrimSuffix(strings.TrimSpace(message), ".")
	if len(errs) == 1 && (errs[0].Status == http.StatusNotFound || strings.EqualFold(normalized, "not found")) {
		return fmt.Errorf("%w: %s", ErrNotFound, message)
	}
	return mediaQueryError(errs[0])
}

// parseMedia decodes the GraphQL envelope into a Media. Only an explicit
// Media null with no error, or AniList's verified not-found error shape
// (a sole error with status 404 / message "Not Found."), is classified as
// ErrNotFound — the matcher negative-memoizes ErrNotFound, so an HTTP-200
// GraphQL failure, a mixed error envelope, a partial response (non-null Media
// alongside field-resolution errors), or a malformed envelope must surface as
// a plain error (degraded, retried next cycle) rather than permanently
// suppressing the id.
func parseMedia(raw []byte) (Media, error) {
	var r gqlResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return Media{}, fmt.Errorf("anilist: decode response: %w", err)
	}
	if r.Data == nil || len(r.Data.Media) == 0 {
		if len(r.Errors) > 0 {
			return Media{}, mediaQueryError(r.Errors[0])
		}
		return Media{}, errors.New("anilist: response missing Media")
	}
	mediaRaw := bytes.TrimSpace(r.Data.Media)
	if bytes.Equal(mediaRaw, []byte("null")) {
		return Media{}, classifyNullMedia(r.Errors)
	}
	// A GraphQL partial response carries a non-null Media beside
	// field-resolution errors; accepting it would memoize incomplete
	// titles/year, so it fails like any other query error.
	if len(r.Errors) > 0 {
		return Media{}, mediaQueryError(r.Errors[0])
	}
	var media gqlMedia
	if err := json.Unmarshal(mediaRaw, &media); err != nil {
		return Media{}, fmt.Errorf("anilist: decode Media: %w", err)
	}
	parsed, err := media.toMedia()
	if err != nil {
		return Media{}, fmt.Errorf("anilist: invalid Media: %w", err)
	}
	return parsed, nil
}

// gqlPage is the nullable Page object of the batched query; pointers in the
// envelope distinguish an explicit empty media array (valid, nothing found)
// from a missing/null Page or media field (malformed response).
type gqlPage struct {
	Media *[]gqlMedia `json:"media"`
}

// gqlPageResponse is the GraphQL envelope for the batched Page(media) query.
type gqlPageResponse struct {
	Data struct {
		Page *gqlPage `json:"Page"`
	} `json:"data"`
	Errors []gqlError `json:"errors"`
}

// parseMediaPage decodes a batched Page(media) response into a map keyed by
// AniList id. A GraphQL-level error or a missing/null Page or media field
// fails the batch; the record loop's per-record invariants (positive id,
// valid fields, no duplicate ids) live in parsePageRecords - a rejected
// record is skipped and surfaced via an errBatchRecord-wrapped error
// alongside the chunk's valid records, so one poisoned record cannot discard
// the chunk or read as a total outage - a skipped id is absent from the map
// AND covered by the non-nil error, so the caller never negative-memoizes it,
// and FetchMany distinguishes the record-local failure from an envelope
// failure and keeps fetching later chunks. Ids absent from the media array of
// an error-free response are simply not in the map (the caller treats them as
// not-found).
func parseMediaPage(raw []byte) (map[int]Media, error) {
	var r gqlPageResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("anilist: decode batch response: %w", err)
	}
	if len(r.Errors) > 0 {
		return nil, fmt.Errorf("anilist: batch query error: %s", sanitizeUpstreamMessage(r.Errors[0].Message))
	}
	if r.Data.Page == nil {
		return nil, errors.New("anilist: batch response missing Page")
	}
	if r.Data.Page.Media == nil {
		return nil, errors.New("anilist: batch response missing media")
	}
	return parsePageRecords(*r.Data.Page.Media)
}

// parsePageRecords validates one batch response's record list into a map
// keyed by AniList id: a record with a non-positive id or rejected fields
// (toMedia) is skipped, and a DUPLICATE id is conflicting untrusted data -
// two records claiming one identity - so NO record for that id is returned
// (the earlier occurrence is deleted and the id stays excluded however many
// duplicates follow) rather than silently letting the last write win. Each
// failure surfaces the first offender via an errBatchRecord-wrapped error
// beside the valid sibling records.
func parsePageRecords(media []gqlMedia) (map[int]Media, error) {
	out := make(map[int]Media, len(media))
	seen := make(map[int]bool, len(media))
	var recordErr error
	record := func(err error) {
		if recordErr == nil {
			recordErr = err
		}
	}
	for i := range media {
		md := &media[i]
		if md.ID <= 0 {
			record(fmt.Errorf("%w media record %d missing id", errBatchRecord, i))
			continue
		}
		if seen[md.ID] {
			delete(out, md.ID)
			record(fmt.Errorf("%w media record %d duplicates id %d", errBatchRecord, i, md.ID))
			continue
		}
		seen[md.ID] = true
		parsed, err := md.toMedia()
		if err != nil {
			record(fmt.Errorf("%w media record %d (id %d): %v", errBatchRecord, i, md.ID, err))
			continue
		}
		out[md.ID] = parsed
	}
	return out, recordErr
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
// backing off when the budget is low or a 429 was seen.
type throttle struct {
	next     time.Time
	interval time.Duration
	mu       sync.Mutex
}

// wait blocks until this request's reserved slot, or ctx is cancelled.
func (t *throttle) wait(ctx context.Context) error {
	return httpx.SleepCtx(ctx, t.reserve())
}

// reserve claims the next slot and returns how long to wait before using it.
func (t *throttle) reserve() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	start := now
	if t.next.After(now) {
		start = t.next
	}
	t.next = start.Add(t.interval)
	return start.Sub(now)
}

// penalize pushes the next slot out by at least d from now.
func (t *throttle) penalize(d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if until := time.Now().Add(d); until.After(t.next) {
		t.next = until
	}
}
