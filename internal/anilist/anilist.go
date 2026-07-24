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

	"github.com/cplieger/httpx/v3"
	"github.com/cplieger/seadex-scout/internal/appinfo"
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
	// every subsequent lookup. httpx.RetryWithBackoff and the throttle use the hint
	// verbatim, so the cap must be applied here.
	maxRetryAfter = time.Minute
)

// ErrNotFound reports that AniList has no media for the requested ID.
var ErrNotFound = errors.New("anilist: media not found")

// query fetches the fields needed for a title fallback match.
const query = `query ($id: Int) { Media(id: $id, type: ANIME) { format seasonYear startDate { year } title { romaji english native } } }`

// batchQuery fetches the same fields for many ids in one request via Page.media,
// which still counts as a single request against AniList's per-minute budget -
// so a cold cycle's hundreds of id-less lookups collapse to a handful of calls.
// Built from batchSize so the page size and the chunk size cannot drift apart.
var batchQuery = fmt.Sprintf(`query ($ids: [Int]) { Page(perPage: %d) { media(id_in: $ids, type: ANIME) { id format seasonYear startDate { year } title { romaji english native } } } }`, batchSize)

// batchSize is AniList's Page perPage maximum; FetchMany resolves up to this
// many ids per request.
const batchSize = 50

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
// ceiling. On a 429, RetryWithBackoff's capped hint and throttle.penalize
// converge on the same wait; the extra per-attempt wait is effectively zero
// once the hint expires, while later callers retain the penalty.
func (c *Client) request(ctx context.Context, gql string, variables any) ([]byte, error) {
	body, err := json.Marshal(map[string]any{"query": gql, "variables": variables})
	if err != nil {
		return nil, fmt.Errorf("anilist: marshal request: %w", err)
	}
	return httpx.RetryWithBackoff(ctx, maxAttempts, baseDelay, "anilist",
		func(ctx context.Context) ([]byte, error) {
			if err := c.throttle.wait(ctx); err != nil {
				return nil, err
			}
			return c.do(ctx, body)
		})
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
// result (the caller treats an absent id as not-found). On a request error it
// returns the media gathered so far together with the error, so the caller can
// fall back to a per-id Fetch for the remainder rather than losing the batch.
func (c *Client) FetchMany(ctx context.Context, ids []int) (map[int]Media, error) {
	out := make(map[int]Media, len(ids))
	for chunk := range slices.Chunk(ids, batchSize) {
		raw, err := c.request(ctx, batchQuery, map[string]any{"ids": chunk})
		if err != nil {
			return out, err
		}

		page, err := parseMediaPage(raw)
		if err != nil {
			return out, err
		}
		maps.Copy(out, page)
	}
	return out, nil
}

// do performs one GraphQL POST attempt, translating a 429 into a transient
// rate-limit error (carrying a capped Retry-After hint) and reading the rate
// headers to pre-empt the next 429.
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
// and returns the transient rate-limit error carrying that hint.
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
	return &rateLimitedError{retryAfter: wait}
}

// observeRateHeaders slows the throttle when the remaining budget is low,
// waiting for the reset window rather than racing into a 429.
func (c *Client) observeRateHeaders(resp *http.Response) {
	remaining, err := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining"))
	if err != nil || remaining > lowRemaining {
		return
	}
	wait := resetWait(resp)
	if wait == 0 {
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

// toMedia converts the wire shape to a Media, preferring seasonYear and
// falling back to the start-date year.
func (m *gqlMedia) toMedia() Media {
	year := m.SeasonYear
	if year == 0 {
		year = m.StartDate.Year
	}
	return Media{
		Titles: dedupeTitles(m.Title.Romaji, m.Title.English, m.Title.Native),
		Format: m.Format,
		Year:   year,
	}
}

// gqlError is the GraphQL error object shared by both response envelopes.
type gqlError struct {
	Message string `json:"message"`
}

// gqlResponse is the GraphQL envelope for the media query.
type gqlResponse struct {
	Data struct {
		Media *gqlMedia `json:"Media"`
	} `json:"data"`
	Errors []gqlError `json:"errors"`
}

// sanitizeUpstreamMessage bounds and cleans an untrusted upstream error
// message before it is wrapped into an error that reaches the logs: C0/C1
// controls (terminal escape and CSI/OSC introducers), DEL, Unicode line and
// paragraph separators, and bidi override/isolate runes become spaces so the
// message cannot forge log lines or reorder rendered text, and the result is
// capped at 200 bytes on a rune boundary so a long message stays valid UTF-8.
func sanitizeUpstreamMessage(s string) string {
	const maxLen = 200
	s = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7f, // C0 controls and DEL
			r >= 0x80 && r <= 0x9f,         // C1 controls (CSI/OSC introducers)
			r == '\u2028' || r == '\u2029', // line / paragraph separators
			r >= '\u202a' && r <= '\u202e', // bidi embeddings and overrides
			r >= '\u2066' && r <= '\u2069': // bidi isolates
			return ' '
		}
		return r
	}, s)
	if len(s) > maxLen {
		cut := maxLen
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "..."
	}
	return s
}

// parseMedia decodes the GraphQL envelope into a Media, returning ErrNotFound
// when AniList returned no media object.
func parseMedia(raw []byte) (Media, error) {
	var r gqlResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return Media{}, fmt.Errorf("anilist: decode response: %w", err)
	}
	if r.Data.Media == nil {
		if len(r.Errors) > 0 {
			return Media{}, fmt.Errorf("%w: %s", ErrNotFound, sanitizeUpstreamMessage(r.Errors[0].Message))
		}
		return Media{}, ErrNotFound
	}
	return r.Data.Media.toMedia(), nil
}

// gqlPage is the nullable Page object of the batched query; a pointer in the
// envelope distinguishes an explicit empty media array (valid, nothing found)
// from a missing/null Page (malformed response).
type gqlPage struct {
	Media []gqlMedia `json:"media"`
}

// gqlPageResponse is the GraphQL envelope for the batched Page(media) query.
type gqlPageResponse struct {
	Data struct {
		Page *gqlPage `json:"Page"`
	} `json:"data"`
	Errors []gqlError `json:"errors"`
}

// parseMediaPage decodes a batched Page(media) response into a map keyed by
// AniList id. A GraphQL-level error or a missing/null Page fails the batch;
// ids absent from the media array are simply not in the map (the caller
// treats them as not-found).
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
	out := make(map[int]Media, len(r.Data.Page.Media))
	for i := range r.Data.Page.Media {
		md := &r.Data.Page.Media[i]
		if md.ID <= 0 {
			return nil, fmt.Errorf("anilist: batch response media record %d missing id", i)
		}
		out[md.ID] = md.toMedia()
	}
	return out, nil
}

// dedupeTitles returns the non-empty titles in order, without duplicates.
func dedupeTitles(titles ...string) []string {
	seen := make(map[string]struct{}, len(titles))
	var out []string
	for _, t := range titles {
		if t == "" {
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

// --- rate-limit error + adaptive throttle ---

// rateLimitedError must satisfy httpx's retry-classification interfaces, or
// RetryWithBackoff silently stops retrying 429s / honoring the capped hint.
var (
	_ httpx.Transient      = (*rateLimitedError)(nil)
	_ httpx.RetryAfterHint = (*rateLimitedError)(nil)
)

// rateLimitedError is a transient error carrying a capped Retry-After hint, so
// httpx.RetryWithBackoff waits that duration and retries (httpx's own
// RateLimitError is classified non-transient, so a distinct type is used).
type rateLimitedError struct {
	retryAfter time.Duration
}

func (e *rateLimitedError) Error() string                 { return "anilist: rate limited (429)" }
func (e *rateLimitedError) IsTransient() bool             { return true }
func (e *rateLimitedError) RetryAfterHint() time.Duration { return e.retryAfter }

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
