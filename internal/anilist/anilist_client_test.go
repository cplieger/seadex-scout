package anilist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/httpx/v2"
)

// TestDoCapsHostileRetryAfterAndPenalizesThrottle proves a pathological
// server-supplied Retry-After cannot stall the fallback: the 429 becomes a
// transient rateLimitedError whose hint is capped at maxRetryAfter, and the
// throttle is penalized so subsequent lookups wait the capped window too.
func TestDoCapsHostileRetryAfterAndPenalizesThrottle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "86400") // a hostile day-long stall
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL, 60, nil)
	_, err := c.do(context.Background(), []byte(`{}`))

	var rle *rateLimitedError
	if !errors.As(err, &rle) {
		t.Fatalf("do() err = %v, want *rateLimitedError", err)
	}
	if !rle.IsTransient() {
		t.Error("a 429 must classify transient so RetryWithBackoff retries it")
	}
	if hint := rle.RetryAfterHint(); hint != maxRetryAfter {
		t.Errorf("RetryAfterHint() = %v, want capped at %v", hint, maxRetryAfter)
	}
	if wait := c.throttle.reserve(); wait < maxRetryAfter-2*time.Second {
		t.Errorf("throttle wait after the 429 = %v, want pushed out to ~%v", wait, maxRetryAfter)
	}
	if got := c.Stats(); got.RateLimitWaits != 1 {
		t.Errorf("Stats().RateLimitWaits = %d, want 1", got.RateLimitWaits)
	}
}

// TestDo429WithoutRetryAfterUsesDefault pins the fallback wait when the 429
// carries no Retry-After header.
func TestDo429WithoutRetryAfterUsesDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL, 60, nil)
	_, err := c.do(context.Background(), []byte(`{}`))

	var rle *rateLimitedError
	if !errors.As(err, &rle) {
		t.Fatalf("do() err = %v, want *rateLimitedError", err)
	}
	if hint := rle.RetryAfterHint(); hint != defaultRetryAfter {
		t.Errorf("RetryAfterHint() = %v, want the %v default", hint, defaultRetryAfter)
	}
}

// TestDoHonorsValidRetryAfterHeader pins the ordinary-header path between the
// missing-header default and the hostile-value cap: a valid delta-seconds
// Retry-After survives parsing into the rateLimitedError hint instead of being
// discarded for the default.
func TestDoHonorsValidRetryAfterHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL, 60, nil)
	_, err := c.do(context.Background(), []byte(`{}`))

	var rle *rateLimitedError
	if !errors.As(err, &rle) {
		t.Fatalf("do() err = %v, want *rateLimitedError", err)
	}
	if got := rle.RetryAfterHint(); got != 17*time.Second {
		t.Errorf("RetryAfterHint() = %v, want 17s from Retry-After", got)
	}
}

// TestDo429WithoutRetryAfterUsesResetHeader pins the reset-window fallback: a
// 429 that omits Retry-After but carries a future X-RateLimit-Reset must wait
// until that reset (not the blind 5s default), so the bounded attempts do not
// all land inside the same rate window.
func TestDo429WithoutRetryAfterUsesResetHeader(t *testing.T) {
	reset := time.Now().Add(30 * time.Second).Unix()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", reset))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL, 60, nil)
	_, err := c.do(context.Background(), []byte(`{}`))

	var rle *rateLimitedError
	if !errors.As(err, &rle) {
		t.Fatalf("do() err = %v, want *rateLimitedError", err)
	}
	hint := rle.RetryAfterHint()
	if hint <= defaultRetryAfter || hint > 31*time.Second {
		t.Errorf("RetryAfterHint() = %v, want ~30s from X-RateLimit-Reset (not the %v default)", hint, defaultRetryAfter)
	}
}

// TestDo429WithPastResetFallsBackToDefault pins the guard on a stale reset: a
// reset timestamp already in the past yields a non-positive wait, which must
// fall back to the default rather than a zero/negative penalty.
func TestDo429WithPastResetFallsBackToDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(-time.Minute).Unix()))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL, 60, nil)
	_, err := c.do(context.Background(), []byte(`{}`))

	var rle *rateLimitedError
	if !errors.As(err, &rle) {
		t.Fatalf("do() err = %v, want *rateLimitedError", err)
	}
	if hint := rle.RetryAfterHint(); hint != defaultRetryAfter {
		t.Errorf("RetryAfterHint() = %v, want the %v default for a past reset", hint, defaultRetryAfter)
	}
}

// TestFetchReturnsMediaAndCountsCalls exercises the full single-id path
// (throttle, POST, decode) against a hermetic server and checks the call
// counter feeding the cycle-complete log line.
func TestFetchReturnsMediaAndCountsCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":{"Media":{"format":"TV","seasonYear":2023,"title":{"romaji":"Sousou no Frieren","english":"Frieren"}}}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL, 100000, nil)
	m, err := c.Fetch(context.Background(), 154587)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if m.Format != "TV" || m.Year != 2023 || len(m.Titles) != 2 {
		t.Errorf("Fetch() = %+v, want TV/2023 with 2 titles", m)
	}
	if got := c.Stats(); got.Calls != 1 {
		t.Errorf("Stats().Calls = %d, want 1", got.Calls)
	}
}

// TestFetchManyChunksBatchesAndMergesResults proves the batching contract: 120
// ids split into batchSize-bounded requests (50+50+20), every id resolves into
// the merged map, and the call counter reads one per batch (the ~N/50 shape the
// cycle-complete log line documents).
func TestFetchManyChunksBatchesAndMergesResults(t *testing.T) {
	var mu sync.Mutex
	var batchSizes []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Variables struct {
				IDs []int `json:"ids"`
			} `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode batch request: %v", err)
		}
		mu.Lock()
		batchSizes = append(batchSizes, len(req.Variables.IDs))
		mu.Unlock()
		media := make([]string, 0, len(req.Variables.IDs))
		for _, id := range req.Variables.IDs {
			media = append(media, fmt.Sprintf(`{"id":%d,"format":"TV","seasonYear":2020,"title":{"romaji":"t%d"}}`, id, id))
		}
		fmt.Fprintf(w, `{"data":{"Page":{"media":[%s]}}}`, strings.Join(media, ","))
	}))
	defer srv.Close()

	ids := make([]int, 120)
	for i := range ids {
		ids[i] = i + 1
	}
	c := NewClient(srv.Client(), srv.URL, 100000, nil)
	out, err := c.FetchMany(context.Background(), ids)
	if err != nil {
		t.Fatalf("FetchMany: %v", err)
	}

	wantBatches := []int{50, 50, 20}
	if !slices.Equal(batchSizes, wantBatches) {
		t.Errorf("batch sizes = %v, want %v", batchSizes, wantBatches)
	}
	if len(out) != 120 {
		t.Fatalf("merged result has %d ids, want 120", len(out))
	}
	if got := out[77].Titles; len(got) != 1 || got[0] != "t77" {
		t.Errorf("out[77].Titles = %v, want [t77]", got)
	}
	if got := c.Stats(); got.Calls != 3 {
		t.Errorf("Stats().Calls = %d, want 3 (one per batch)", got.Calls)
	}
}

// TestFetchManyReturnsPartialResultsOnError pins the documented contract that a
// mid-run request failure returns the media gathered so far together with the
// error, so the caller can fall back per-id instead of losing the batch.
func TestFetchManyReturnsPartialResultsOnError(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n > 1 {
			fmt.Fprint(w, `{"errors":[{"message":"boom"}]}`)
			return
		}
		fmt.Fprint(w, `{"data":{"Page":{"media":[{"id":1,"format":"TV","seasonYear":2020,"title":{"romaji":"t1"}}]}}}`)
	}))
	defer srv.Close()

	ids := make([]int, 60) // two chunks: the first succeeds, the second fails
	for i := range ids {
		ids[i] = i + 1
	}
	c := NewClient(srv.Client(), srv.URL, 100000, nil)
	out, err := c.FetchMany(context.Background(), ids)
	if err == nil {
		t.Fatal("FetchMany must surface the second chunk's GraphQL error")
	}
	if len(out) != 1 || out[1].Titles[0] != "t1" {
		t.Errorf("partial result = %+v, want the first chunk's id 1 preserved", out)
	}
}

// TestFetchCountsEveryHTTPAttempt proves Stats().Calls counts outbound HTTP
// attempts, not logical fetches: two 429s followed by success are three
// attempts (and two rate-limit waits), so the counter keeps its request-volume
// signal exactly during rate-limit episodes.
func TestFetchCountsEveryHTTPAttempt(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n <= 2 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{"data":{"Media":{"format":"TV","seasonYear":2023,"title":{"romaji":"A"}}}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL, 100000, nil)
	if _, err := c.Fetch(context.Background(), 1); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got := c.Stats()
	if got.Calls != 3 {
		t.Errorf("Stats().Calls = %d, want 3 (every HTTP attempt counted)", got.Calls)
	}
	if got.RateLimitWaits != 2 {
		t.Errorf("Stats().RateLimitWaits = %d, want 2", got.RateLimitWaits)
	}
}

// TestDoBoundsOversizedResponse pins the untrusted-response size boundary: a
// body larger than maxBodyBytes fails loud as httpx.ReadLimitedBody's distinct
// *httpx.ResponseTooLargeError (with no bytes returned), so a hostile or
// broken upstream cannot balloon memory and an over-cap response is never a
// silently truncated payload that only fails later as a confusing JSON decode
// error.
func TestDoBoundsOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", maxBodyBytes+1)))
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL, 60, nil)
	got, err := c.do(context.Background(), []byte(`{}`))
	var tooLarge *httpx.ResponseTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("do() err = %v, want *httpx.ResponseTooLargeError for an over-cap body", err)
	}
	if tooLarge.Limit != maxBodyBytes {
		t.Errorf("ResponseTooLargeError.Limit = %d, want %d", tooLarge.Limit, maxBodyBytes)
	}
	if got != nil {
		t.Errorf("do() returned %d bytes alongside the error, want nil (no truncated payload)", len(got))
	}
}

// TestFetchCanceledBeforeReservedSlot pins the pre-request cancellation branch:
// a context canceled while waiting for a throttle reservation returns
// context.Canceled before counting or issuing an AniList request.
func TestFetchCanceledBeforeReservedSlot(t *testing.T) {
	c := NewClient(http.DefaultClient, "http://127.0.0.1:1", 60, nil)
	c.throttle.penalize(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.Fetch(ctx, 1)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Fetch() error = %v, want context.Canceled", err)
	}
	if got := c.Stats().Calls; got != 0 {
		t.Errorf("Stats().Calls = %d, want 0 when canceled before request", got)
	}
}

// TestFetchManyCanceledBeforeReservedSlot pins the same branch for the batched
// path, including the documented partial-result shape and the requirement not
// to count a request that never starts.
func TestFetchManyCanceledBeforeReservedSlot(t *testing.T) {
	c := NewClient(http.DefaultClient, "http://127.0.0.1:1", 60, nil)
	c.throttle.penalize(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	out, err := c.FetchMany(ctx, []int{1})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("FetchMany() error = %v, want context.Canceled", err)
	}
	if len(out) != 0 {
		t.Errorf("FetchMany() result = %v, want empty partial result", out)
	}
	if got := c.Stats().Calls; got != 0 {
		t.Errorf("Stats().Calls = %d, want 0 when canceled before request", got)
	}
}

// TestFetchNotFound404ReturnsErrNotFound pins the AniList not-found wire
// shape: a nonexistent id answers HTTP 404 while still carrying the normal
// GraphQL envelope with a null Media (verified live), and Fetch must classify
// that as ErrNotFound so the matcher memoizes it negatively instead of
// degrading the cycle and re-fetching every poll.
func TestFetchNotFound404ReturnsErrNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"errors":[{"message":"Not Found.","status":404}],"data":{"Media":null}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL, 100000, nil)
	_, err := c.Fetch(context.Background(), 999999999)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Fetch() error = %v, want ErrNotFound (AniList mirrors not-found into HTTP 404)", err)
	}
}

// TestFetchErrorStatusClassification pins the non-429 error-status path: a
// 4xx or 5xx (other than the AniList 404 not-found form) surfaces as the
// typed httpx error, is never ErrNotFound, and a non-transient status is not
// retried (exactly one HTTP attempt against the rate budget).
func TestFetchErrorStatusClassification(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		wantAuth    bool
		wantGeneric bool
	}{
		{name: "500 internal error", status: http.StatusInternalServerError},
		{name: "400 bad request", status: http.StatusBadRequest},
		{name: "401 unauthorized", status: http.StatusUnauthorized, wantAuth: true},
		{name: "204 unexpected status", status: http.StatusNoContent, wantGeneric: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()
			c := NewClient(srv.Client(), srv.URL, 100000, nil)
			_, err := c.Fetch(context.Background(), 1)
			if err == nil {
				t.Fatalf("Fetch() on status %d = nil error, want typed error", tt.status)
			}
			if errors.Is(err, ErrNotFound) {
				t.Errorf("Fetch() on status %d = ErrNotFound, want a status error", tt.status)
			}
			switch {
			case tt.wantGeneric:
				want := "anilist: unexpected status 204"
				if err.Error() != want {
					t.Errorf("Fetch() on 204 error = %q, want %q", err.Error(), want)
				}
			case tt.wantAuth:
				var authErr *httpx.AuthError
				if !errors.As(err, &authErr) {
					t.Errorf("Fetch() on 401 error = %v, want *httpx.AuthError", err)
				}
			default:
				var statusErr *httpx.HTTPStatusError
				if !errors.As(err, &statusErr) {
					t.Errorf("Fetch() on status %d error = %v, want *httpx.HTTPStatusError", tt.status, err)
				} else if statusErr.Code != tt.status {
					t.Errorf("HTTPStatusError.Code = %d, want %d", statusErr.Code, tt.status)
				}
			}
			if got := c.Stats().Calls; got != 1 {
				t.Errorf("Stats().Calls = %d, want 1 (non-transient status must not retry)", got)
			}
		})
	}
}

// TestDo429WithHostileResetHeaderIsCapped pins the app-level maxRetryAfter
// cap on the reset-window fallback: a 429 that omits Retry-After but carries a
// pathological far-future X-RateLimit-Reset must not stall the fallback - the
// hint and the throttle penalty are both capped at maxRetryAfter.
func TestDo429WithHostileResetHeaderIsCapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(24*time.Hour).Unix()))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL, 60, nil)
	_, err := c.do(context.Background(), []byte(`{}`))

	var rle *rateLimitedError
	if !errors.As(err, &rle) {
		t.Fatalf("do() err = %v, want *rateLimitedError", err)
	}
	if hint := rle.RetryAfterHint(); hint != maxRetryAfter {
		t.Errorf("RetryAfterHint() = %v, want capped at %v (a hostile reset window must not stall the fallback)", hint, maxRetryAfter)
	}
	if wait := c.throttle.reserve(); wait > maxRetryAfter {
		t.Errorf("throttle wait after the 429 = %v, want capped at %v", wait, maxRetryAfter)
	}
}
