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
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cplieger/httpx/v2"
)

const (
	userAgent    = "seadex-scout (+https://github.com/cplieger/seadex-scout)"
	maxBodyBytes = 1 << 20
	maxAttempts  = 3
	baseDelay    = time.Second
	// lowRemaining is the X-RateLimit-Remaining threshold below which the
	// client proactively waits for the window reset to avoid a 429.
	lowRemaining = 2
	// defaultRetryAfter is used when a 429 carries no Retry-After header.
	defaultRetryAfter = 5 * time.Second
)

// ErrNotFound reports that AniList has no media for the requested ID.
var ErrNotFound = errors.New("anilist: media not found")

// query fetches the fields needed for a title fallback match.
const query = `query ($id: Int) { Media(id: $id, type: ANIME) { format seasonYear startDate { year } title { romaji english native } } }`

// Media is the subset of an AniList entry used for title matching.
type Media struct {
	Format string
	Titles []string
	Year   int
}

// Stats is a snapshot of client activity for metrics.
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

// Stats returns a snapshot of the cumulative call and rate-limit-wait counts.
func (c *Client) Stats() Stats {
	return Stats{Calls: c.calls.Load(), RateLimitWaits: c.rlWaits.Load()}
}

// Fetch returns the AniList media for the given ID, or ErrNotFound when AniList
// has no such anime. It throttles before the request and retries transient
// failures and 429s (honoring Retry-After).
func (c *Client) Fetch(ctx context.Context, aniListID int) (Media, error) {
	if err := c.throttle.wait(ctx); err != nil {
		return Media{}, err
	}
	c.calls.Add(1)

	body, err := json.Marshal(map[string]any{
		"query":     query,
		"variables": map[string]int{"id": aniListID},
	})
	if err != nil {
		return Media{}, fmt.Errorf("anilist: marshal request: %w", err)
	}

	raw, err := httpx.RetryWithBackoff(ctx, maxAttempts, baseDelay, "anilist",
		func(ctx context.Context) ([]byte, error) { return c.do(ctx, body) })
	if err != nil {
		return Media{}, err
	}
	return parseMedia(raw)
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
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.http.Do(req) //nolint:bodyclose // drained and closed via httpx.DrainClose below
	if err != nil {
		return nil, err
	}
	defer httpx.DrainClose(resp.Body)

	if resp.StatusCode == http.StatusTooManyRequests {
		c.rlWaits.Add(1)
		wait := httpx.ParseRetryAfter(resp.Header.Get("Retry-After"))
		if wait <= 0 {
			wait = defaultRetryAfter
		}
		c.throttle.penalize(wait)
		return nil, &rateLimitedError{retryAfter: wait}
	}
	if resp.StatusCode != http.StatusOK {
		if statusErr := httpx.CheckHTTPStatus(resp); statusErr != nil {
			return nil, statusErr
		}
		return nil, fmt.Errorf("anilist: unexpected status %d", resp.StatusCode)
	}

	c.observeRateHeaders(resp)
	return io.ReadAll(httpx.LimitedBody(resp, maxBodyBytes))
}

// observeRateHeaders slows the throttle when the remaining budget is low,
// waiting for the reset window rather than racing into a 429.
func (c *Client) observeRateHeaders(resp *http.Response) {
	remaining, err := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining"))
	if err != nil || remaining > lowRemaining {
		return
	}
	wait := time.Minute
	if reset, rerr := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); rerr == nil {
		if until := time.Until(time.Unix(reset, 0)); until > 0 {
			wait = until
		}
	}
	c.log.Debug("anilist: low rate budget, backing off", "remaining", remaining, "wait", wait.Round(time.Second))
	c.throttle.penalize(wait)
}

// gqlResponse is the GraphQL envelope for the media query.
type gqlResponse struct {
	Data struct {
		Media *struct {
			Title struct {
				Romaji  string `json:"romaji"`
				English string `json:"english"`
				Native  string `json:"native"`
			} `json:"title"`
			Format    string `json:"format"`
			StartDate struct {
				Year int `json:"year"`
			} `json:"startDate"`
			SeasonYear int `json:"seasonYear"`
		} `json:"Media"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
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
			return Media{}, fmt.Errorf("%w: %s", ErrNotFound, r.Errors[0].Message)
		}
		return Media{}, ErrNotFound
	}
	m := r.Data.Media
	year := m.SeasonYear
	if year == 0 {
		year = m.StartDate.Year
	}
	return Media{
		Titles: dedupeTitles(m.Title.Romaji, m.Title.English, m.Title.Native),
		Format: m.Format,
		Year:   year,
	}, nil
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
