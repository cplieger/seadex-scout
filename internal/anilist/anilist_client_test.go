package anilist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/httpx/v4"
	"github.com/cplieger/seadex-scout/internal/appinfo"
	"github.com/cplieger/slogx/capture"
)

// verdictName renders a Verdict for a failure message; a bare uint8 tells the
// reader nothing about which of the four answers the batch produced.
func verdictName(v Verdict) string {
	switch v {
	case VerdictUnrequested:
		return "VerdictUnrequested"
	case VerdictFound:
		return "VerdictFound"
	case VerdictAbsent:
		return "VerdictAbsent"
	case VerdictUnverified:
		return "VerdictUnverified"
	}
	return fmt.Sprintf("Verdict(%d)", uint8(v))
}

// TestDoCapsHostileRetryAfterAndPenalizesThrottle proves a pathological
// server-supplied Retry-After cannot stall the client: the 429 becomes a
// *httpx.RateLimitError whose hint is clamped, and the shared throttle is
// penalized so subsequent lookups wait the clamped window too.
//
// The ceiling is maxThrottlePenalty, the POLITENESS one (l-f7). Note what this
// test would silently stop proving if the app went back to httpx.ParseRetryAfter:
// that helper caps at httpx.RetryAfterCap (60s) inside the library, so a hostile
// header would never reach the app's own ceiling and this assertion would pass
// for the wrong reason - the two 429 shapes (Retry-After here, X-RateLimit-Reset
// in TestDo429WithHostileResetHeaderIsCapped) would disagree by 5x on the same
// stated window. Both now clamp at the same number, which is the point.
func TestDoCapsHostileRetryAfterAndPenalizesThrottle(t *testing.T) {
	rec := capture.Default(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "86400") // a hostile day-long stall
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL, 60, nil)
	_, err := c.do(t.Context(), []byte(`{}`))

	var rle *httpx.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("do() err = %v, want *httpx.RateLimitError", err)
	}
	if rle.RetryAfter != maxThrottlePenalty {
		t.Errorf("RetryAfter = %v, want clamped at %v", rle.RetryAfter, maxThrottlePenalty)
	}
	if wait := c.throttle.reserve(); wait < maxThrottlePenalty-2*time.Second {
		t.Errorf("throttle wait after the 429 = %v, want pushed out to ~%v", wait, maxThrottlePenalty)
	}
	if got := c.Stats(); got.RateLimitWaits != 1 {
		t.Errorf("Stats().RateLimitWaits = %d, want 1", got.RateLimitWaits)
	}
	if !rec.Contains("anilist stated a rate-limit window too long to be one; honouring the politeness ceiling instead") {
		t.Errorf("a 24h Retry-After was clamped silently; an absurd header must leave a trace. log:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestDo429WithoutRetryAfterUsesDefault pins the fallback wait when the 429
// carries no Retry-After header, and the stable error message the retry loop
// and degraded-lookup logs carry.
func TestDo429WithoutRetryAfterUsesDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL, 60, nil)
	_, err := c.do(t.Context(), []byte(`{}`))

	var rle *httpx.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("do() err = %v, want *httpx.RateLimitError", err)
	}
	if rle.RetryAfter != defaultRetryAfter {
		t.Errorf("RetryAfter = %v, want the %v default", rle.RetryAfter, defaultRetryAfter)
	}
	if got, want := rle.Error(), "anilist: rate limited (429)"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// TestDoHonorsValidRetryAfterHeader pins the ordinary-header path between the
// missing-header default and the hostile-value cap: a valid delta-seconds
// Retry-After survives parsing into the rate-limit error's RetryAfter hint
// instead of being discarded for the default.
func TestDoHonorsValidRetryAfterHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL, 60, nil)
	_, err := c.do(t.Context(), []byte(`{}`))

	var rle *httpx.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("do() err = %v, want *httpx.RateLimitError", err)
	}
	if got := rle.RetryAfter; got != 17*time.Second {
		t.Errorf("RetryAfter = %v, want 17s from Retry-After", got)
	}
}

// TestDo429WithoutRetryAfterUsesResetHeader pins the reset-window fallback: a
// 429 that omits Retry-After but carries a future X-RateLimit-Reset must wait
// until that reset (not the no-evidence default), so the bounded attempts do not
// all land inside the same rate window.
func TestDo429WithoutRetryAfterUsesResetHeader(t *testing.T) {
	reset := time.Now().Add(30 * time.Second).Unix()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", reset))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL, 60, nil)
	_, err := c.do(t.Context(), []byte(`{}`))

	var rle *httpx.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("do() err = %v, want *httpx.RateLimitError", err)
	}
	hint := rle.RetryAfter
	if hint < 25*time.Second || hint > 31*time.Second {
		t.Errorf("RetryAfter = %v, want ~30s from X-RateLimit-Reset (not the %v default)", hint, defaultRetryAfter)
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
	_, err := c.do(t.Context(), []byte(`{}`))

	var rle *httpx.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("do() err = %v, want *httpx.RateLimitError", err)
	}
	if rle.RetryAfter != defaultRetryAfter {
		t.Errorf("RetryAfter = %v, want the %v default for a past reset", rle.RetryAfter, defaultRetryAfter)
	}
}

// TestFetchReturnsMediaAndCountsCalls exercises the full single-id path
// (throttle, POST, decode) against a hermetic server and checks the call
// counter feeding the cycle-complete log line.
func TestFetchReturnsMediaAndCountsCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":{"Media":{"id":154587,"format":"TV","seasonYear":2023,"title":{"romaji":"Sousou no Frieren","english":"Frieren"}}}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL, 100000, nil)
	m, err := c.Fetch(t.Context(), 154587)
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

// TestFetchRejectsMismatchedMediaID pins the single-fetch identity invariant
// (the per-id equivalent of the batch path's retainRequested): a response
// carrying a valid Media for a DIFFERENT id than the one requested is
// rejected as a plain lookup failure, so a malformed or compromised endpoint
// cannot get the wrong titles memoized under the requested id.
func TestFetchRejectsMismatchedMediaID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":{"Media":{"id":601,"format":"TV","seasonYear":2007,"title":{"romaji":"Clannad"}}}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL, 100000, nil)
	if _, err := c.Fetch(t.Context(), 600); err == nil || !strings.Contains(err.Error(), "does not match requested id") {
		t.Fatalf("Fetch mismatched identity error = %v, want identity rejection", err)
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
	res, err := c.FetchMany(t.Context(), ids)
	out := res.Media
	if err != nil {
		t.Fatalf("FetchMany: %v", err)
	}
	for _, id := range ids {
		if got := res.Verdicts[id]; got != VerdictFound {
			t.Errorf("Verdicts[%d] = %s, want VerdictFound for a fully successful batch", id, verdictName(got))
		}
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
	res, err := c.FetchMany(t.Context(), ids)
	out := res.Media
	if err == nil {
		t.Fatal("FetchMany must surface the second chunk's GraphQL error")
	}
	if len(out) != 1 || out[1].Titles[0] != "t1" {
		t.Errorf("partial result = %+v, want the first chunk's id 1 preserved", out)
	}
}

// TestFetchManyPreservesValidRecordsOnRecordError pins the same-chunk salvage
// contract: when parseMediaPage returns valid records alongside a record-level
// error from the same response, FetchMany copies the valid records into the
// result before surfacing the error, so the caller keeps what parsed instead
// of losing the whole chunk.
func TestFetchManyPreservesValidRecordsOnRecordError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":{"Page":{"media":[{"id":1,"format":"TV","seasonYear":2020,"title":{"romaji":"valid"}},{"id":0,"format":"TV","seasonYear":2020,"title":{"romaji":"poisoned"}}]}}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL, 100000, nil)
	res, err := c.FetchMany(t.Context(), []int{1, 2})
	out := res.Media
	if err == nil {
		t.Fatal("FetchMany must surface the invalid record")
	}
	if len(out) != 1 {
		t.Fatalf("FetchMany returned %d valid records, want 1", len(out))
	}
	if got := out[1].Titles; !slices.Equal(got, []string{"valid"}) {
		t.Errorf("out[1].Titles = %v, want [valid]", got)
	}
}

// TestFetchManyContinuesAfterRecordError pins the record-local-vs-envelope
// distinction: a poisoned record in the first chunk must not abort the batch,
// so with stable id ordering one malformed record cannot permanently hide
// every valid id in later chunks (which the caller would otherwise misread as
// a total outage). Later chunks are still fetched, their media merged, and the
// first record error surfaced alongside the merged result.
func TestFetchManyContinuesAfterRecordError(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			fmt.Fprint(w, `{"data":{"Page":{"media":[{"id":0,"format":"TV","seasonYear":2020,"title":{"romaji":"poisoned"}}]}}}`)
			return
		}
		fmt.Fprint(w, `{"data":{"Page":{"media":[{"id":51,"format":"TV","seasonYear":2020,"title":{"romaji":"t51"}}]}}}`)
	}))
	defer srv.Close()

	ids := make([]int, 60) // two chunks: the first is poisoned, the second valid
	for i := range ids {
		ids[i] = i + 1
	}
	c := NewClient(srv.Client(), srv.URL, 100000, nil)
	res, err := c.FetchMany(t.Context(), ids)
	out := res.Media
	if err == nil {
		t.Fatal("FetchMany must surface the first chunk's record error")
	}
	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls != 2 {
		t.Errorf("batch calls = %d, want 2 (a record error must not abort later chunks)", gotCalls)
	}
	if got := out[51].Titles; !slices.Equal(got, []string{"t51"}) {
		t.Errorf("out[51].Titles = %v, want [t51] (second chunk fetched despite the first chunk's record error)", got)
	}
}

// TestFetchManyDropsUnsolicitedID pins FetchMany's identity-set invariant: an
// id the request chunk never asked for is untrusted response data - it is
// omitted from the merged result (never injected, never allowed to overwrite
// another chunk's value) and surfaced as a record-local error, while the
// requested sibling records still resolve.
func TestFetchManyDropsUnsolicitedID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":{"Page":{"media":[{"id":1,"format":"TV","seasonYear":2020,"title":{"romaji":"t1"}},{"id":999,"format":"TV","seasonYear":2020,"title":{"romaji":"injected"}}]}}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL, 100000, nil)
	res, err := c.FetchMany(t.Context(), []int{1, 2})
	out := res.Media
	if err == nil {
		t.Fatal("FetchMany must surface the unsolicited id as a record error")
	}
	if !errors.Is(err, errBatchRecord) {
		t.Errorf("error = %v, want errBatchRecord classification (later chunks must not be aborted)", err)
	}
	if !strings.Contains(err.Error(), "unexpected media id 999") {
		t.Errorf("error = %q, want the unexpected-id context", err.Error())
	}
	if _, ok := out[999]; ok {
		t.Error("unsolicited id 999 was merged into the result")
	}
	if got := out[1].Titles; !slices.Equal(got, []string{"t1"}) {
		t.Errorf("out[1].Titles = %v, want [t1] (valid sibling must survive)", got)
	}
}

// TestParseMediaPageDuplicateIDExcluded pins the duplicate-id policy: records
// claiming the same id are conflicting untrusted data, so NO record for that
// id is returned (never last-write-wins, and a third duplicate stays excluded
// too) while a valid sibling survives and the conflict surfaces as a
// record-local error.
func TestParseMediaPageDuplicateIDExcluded(t *testing.T) {
	raw := []byte(`{"data":{"Page":{"media":[` +
		`{"id":1,"format":"TV","seasonYear":2020,"title":{"romaji":"first"}},` +
		`{"id":1,"format":"TV","seasonYear":2021,"title":{"romaji":"second"}},` +
		`{"id":1,"format":"TV","seasonYear":2022,"title":{"romaji":"third"}},` +
		`{"id":2,"format":"TV","seasonYear":2020,"title":{"romaji":"sibling"}}]}}}`)
	out, err := parseMediaPage(raw)
	if err == nil {
		t.Fatal("parseMediaPage must surface the duplicate id")
	}
	if !errors.Is(err, errBatchRecord) {
		t.Errorf("error = %v, want errBatchRecord classification", err)
	}
	if want := "(3 of 4 records rejected)"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v, want magnitude %q: every record the conflict excluded counts, not just the later duplicates", err, want)
	}
	if got, ok := out[1]; ok {
		t.Errorf("out[1] = %+v, want the conflicting duplicate excluded, not one record chosen", got)
	}
	if got := out[2].Titles; !slices.Equal(got, []string{"sibling"}) {
		t.Errorf("out[2].Titles = %v, want [sibling]", got)
	}
}

// TestParseMediaPageUndecodableDuplicateIDExcluded pins the same fail-closed
// policy across an UNDECODABLE duplicate, in either order. encoding/json keeps
// populating decodable fields after a type error, so a positive id on a
// malformed element is still an identity claim: whether the malformed record
// precedes or follows the well-formed one, no record for that id survives and
// an unrelated sibling does.
func TestParseMediaPageUndecodableDuplicateIDExcluded(t *testing.T) {
	const (
		valid     = `{"id":1,"format":"TV","seasonYear":2020,"title":{"romaji":"valid"}}`
		malformed = `{"id":1,"format":"TV","seasonYear":"bad","title":{"romaji":"malformed"}}`
		sibling   = `{"id":2,"format":"TV","seasonYear":2020,"title":{"romaji":"sibling"}}`
	)
	for name, media := range map[string][]string{
		"malformed first":  {malformed, valid, sibling},
		"malformed second": {valid, malformed, sibling},
	} {
		t.Run(name, func(t *testing.T) {
			raw := []byte(`{"data":{"Page":{"media":[` + strings.Join(media, ",") + `]}}}`)
			out, err := parseMediaPage(raw)
			if err == nil {
				t.Fatal("parseMediaPage must surface the undecodable record")
			}
			if !errors.Is(err, errBatchRecord) {
				t.Errorf("error = %v, want errBatchRecord classification", err)
			}
			if got, ok := out[1]; ok {
				t.Errorf("out[1] = %+v, want the conflicting id excluded regardless of order", got)
			}
			if got := out[2].Titles; !slices.Equal(got, []string{"sibling"}) {
				t.Errorf("out[2].Titles = %v, want [sibling] (valid sibling must survive)", got)
			}
		})
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
		fmt.Fprint(w, `{"data":{"Media":{"id":1,"format":"TV","seasonYear":2023,"title":{"romaji":"A"}}}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL, 100000, nil)
	if _, err := c.Fetch(t.Context(), 1); err != nil {
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
	got, err := c.do(t.Context(), []byte(`{}`))
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

	res, err := c.FetchMany(ctx, []int{1})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("FetchMany() error = %v, want context.Canceled", err)
	}
	if len(res.Media) != 0 {
		t.Errorf("FetchMany() result = %v, want empty partial result", res.Media)
	}
	if len(res.Verdicts) != 0 {
		t.Errorf("Verdicts = %v, want none when no chunk completed (every id reads VerdictUnrequested)", res.Verdicts)
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
	_, err := c.Fetch(t.Context(), 999999999)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Fetch() error = %v, want ErrNotFound (AniList mirrors not-found into HTTP 404)", err)
	}
}

// TestFetchErrorStatusClassification pins the non-429 error-status path: a
// 4xx or 5xx (other than the AniList 404 not-found form) surfaces as the
// typed httpx error and is never ErrNotFound. It also pins the retry split:
// a self-healing server-side status (5xx, and 408) is retried inside the
// bounded budget, while a client-side fault is attempted exactly once. httpx's
// own HTTPStatusError policy is transient for 502/503/504 only, so a plain 500
// is retryable here because THIS client classifies it, not httpx.
func TestFetchErrorStatusClassification(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		wantAuth    bool
		wantGeneric bool
		wantCalls   int
	}{
		{name: "500 internal error", status: http.StatusInternalServerError, wantCalls: maxAttempts},
		{name: "408 request timeout", status: http.StatusRequestTimeout, wantCalls: maxAttempts},
		{name: "400 bad request", status: http.StatusBadRequest, wantCalls: 1},
		{name: "401 unauthorized", status: http.StatusUnauthorized, wantAuth: true, wantCalls: 1},
		{name: "204 unexpected status", status: http.StatusNoContent, wantGeneric: true, wantCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()
			c := NewClient(srv.Client(), srv.URL, 100000, nil)
			_, err := c.Fetch(t.Context(), 1)
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
			if got := c.Stats().Calls; got != int64(tt.wantCalls) {
				t.Errorf("Stats().Calls = %d, want %d (a self-healing status retries inside the budget; a client fault does not)", got, tt.wantCalls)
			}
		})
	}
}

// TestDo429WithHostileResetHeaderIsCapped pins the app-level ceilings on the
// reset-window fallback: a 429 that omits Retry-After but carries a pathological
// far-future X-RateLimit-Reset must not stall anything unboundedly.
//
// The two ceilings are DIFFERENT numbers and this pins both (l-f7): the hint the
// retry loop waits on is bounded by maxRetryAfter (per-attempt responsiveness),
// while the shared throttle penalty is bounded by the longer
// maxThrottlePenalty (politeness - how long the client honours a stated window
// for every LATER lookup). Collapsing them is what made a real window longer than
// a minute get honoured for 60s and then discarded.
//
// A 24h header is also absurd rather than merely long, so it must leave a trace:
// clamping it silently would hide an upstream defect.
func TestDo429WithHostileResetHeaderIsCapped(t *testing.T) {
	rec := capture.Default(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(24*time.Hour).Unix()))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL, 60, nil)
	_, err := c.do(t.Context(), []byte(`{}`))

	var rle *httpx.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("do() err = %v, want *httpx.RateLimitError", err)
	}
	if rle.RetryAfter != maxThrottlePenalty {
		t.Errorf("RetryAfter = %v, want the clamped %v", rle.RetryAfter, maxThrottlePenalty)
	}
	if wait := c.throttle.reserve(); wait > maxThrottlePenalty {
		t.Errorf("throttle wait after the 429 = %v, want capped at the politeness ceiling %v", wait, maxThrottlePenalty)
	}
	// httpx enforces the per-attempt ceiling itself (it waits min(hint, maxWait)),
	// so a hint above maxRetryAfter cannot stall one attempt regardless.
	if maxRetryAfter >= maxThrottlePenalty {
		t.Errorf("maxRetryAfter (%v) must stay BELOW maxThrottlePenalty (%v), or the two ceilings have collapsed back into one",
			maxRetryAfter, maxThrottlePenalty)
	}
	if !rec.Contains("anilist stated a rate-limit window too long to be one; honouring the politeness ceiling instead") {
		t.Errorf("a 24h window was clamped silently; an absurd header must leave a trace. log:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestDoTransportErrorPropagatesAndCountsAttempt pins the transport-failure
// branch of do and the documented Stats contract on it: a connection-level
// failure surfaces as a plain transport error (never ErrNotFound), and
// Stats().Calls still counts the attempt because the counter tracks outbound
// HTTP attempts, not completed responses.
func TestDoTransportErrorPropagatesAndCountsAttempt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	client := srv.Client()
	srv.Close() // connection refused from here on

	c := NewClient(client, srv.URL, 100000, nil)
	_, err := c.do(t.Context(), []byte(`{}`))
	if err == nil {
		t.Fatal("do() against a closed server = nil error, want a transport error")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("do() transport error = %v, must not classify as ErrNotFound", err)
	}
	if got := c.Stats().Calls; got != 1 {
		t.Errorf("Stats().Calls = %d, want 1 (a failed transport attempt still counts)", got)
	}
}

// TestFetchManyNoIDsMakesNoRequests pins the empty-input boundary of the
// batched fetch: no ids means no chunks, no outbound attempts, an empty map,
// and a nil error.
func TestFetchManyNoIDsMakesNoRequests(t *testing.T) {
	c := NewClient(http.DefaultClient, "http://127.0.0.1:1", 60, nil)
	res, err := c.FetchMany(t.Context(), nil)
	if err != nil {
		t.Fatalf("FetchMany(nil): %v", err)
	}
	if len(res.Media) != 0 {
		t.Errorf("FetchMany(nil) = %v, want empty map", res.Media)
	}
	if len(res.Verdicts) != 0 {
		t.Errorf("Verdicts = %v, want none (no ids were requested)", res.Verdicts)
	}
	if got := c.Stats().Calls; got != 0 {
		t.Errorf("Stats().Calls = %d, want 0 (no ids, no requests)", got)
	}
}

// TestDoRejectsUnparseableURL pins the request-construction error branch: an
// unparseable client URL surfaces as an error before any outbound attempt, so
// Stats().Calls stays 0 (the attempt counter tracks outbound HTTP attempts).
func TestDoRejectsUnparseableURL(t *testing.T) {
	c := NewClient(http.DefaultClient, "://missing-scheme", 60, nil)
	_, err := c.do(t.Context(), []byte(`{}`))
	if err == nil {
		t.Fatal("do() with an unparseable URL = nil error, want a request-construction error")
	}
	if got := c.Stats().Calls; got != 0 {
		t.Errorf("Stats().Calls = %d, want 0 (a request that cannot be built is never an outbound attempt)", got)
	}
}

// TestFetchManyFirstChunkFailureReturnsIncomplete pins the verdict contract's
// total-failure side: a request/envelope failure before any chunk completes
// returns NO verdicts (and no media) together with the error, so every id reads
// VerdictUnrequested — the signal callers use to distinguish a genuine outage
// from a completed batch that found no media.
func TestFetchManyFirstChunkFailureReturnsIncomplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"errors":[{"message":"boom"}]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL, 100000, nil)
	res, err := c.FetchMany(t.Context(), []int{1, 2})
	if err == nil {
		t.Fatal("FetchMany must surface the first chunk's envelope error")
	}
	for _, id := range []int{1, 2} {
		if got := res.Verdicts[id]; got != VerdictUnrequested {
			t.Errorf("Verdicts[%d] = %s, want VerdictUnrequested (no chunk completed)", id, verdictName(got))
		}
	}
	if len(res.Media) != 0 {
		t.Errorf("FetchMany() media = %v, want none (no chunk completed)", res.Media)
	}
}

// TestFetchManyAllNotFoundThenFailureReturnsCompletedEmpty pins the verdict
// contract's partial side: a first chunk that completes with every id
// definitively not found (a valid empty media array) followed by a failed
// second chunk returns VerdictAbsent for the first chunk's ids and
// VerdictUnrequested for the abandoned second, plus the error - so the caller
// can tell "a chunk completed and found nothing" apart from a total outage.
func TestFetchManyAllNotFoundThenFailureReturnsCompletedEmpty(t *testing.T) {
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
		fmt.Fprint(w, `{"data":{"Page":{"media":[]}}}`)
	}))
	defer srv.Close()

	ids := make([]int, 60) // two chunks: the first completes all-not-found, the second fails
	for i := range ids {
		ids[i] = i + 1
	}
	c := NewClient(srv.Client(), srv.URL, 100000, nil)
	res, err := c.FetchMany(t.Context(), ids)
	if err == nil {
		t.Fatal("FetchMany must surface the second chunk's envelope error")
	}
	for _, id := range ids[:batchSize] {
		if got := res.Verdicts[id]; got != VerdictAbsent {
			t.Errorf("Verdicts[%d] = %s, want VerdictAbsent (the first chunk completed all-not-found)", id, verdictName(got))
		}
	}
	for _, id := range ids[batchSize:] {
		if got := res.Verdicts[id]; got != VerdictUnrequested {
			t.Errorf("Verdicts[%d] = %s, want VerdictUnrequested (the second chunk aborted)", id, verdictName(got))
		}
	}
	if len(res.Media) != 0 {
		t.Errorf("FetchMany() result = %v, want empty (every completed id was not-found)", res.Media)
	}
}

// TestFetchManyRequestFailureAfterCompletedChunkReturnsPartial pins the
// request-layer side of the completion contract: an HTTP-level failure (a
// non-transient 400) on a later chunk after an earlier chunk completed must
// return the merged partial result beside the typed httpx status error --
// the same partial-result shape the parse-layer tests pin, on the other
// error path.
func TestFetchManyRequestFailureAfterCompletedChunkReturnsPartial(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n > 1 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, `{"data":{"Page":{"media":[{"id":1,"format":"TV","seasonYear":2020,"title":{"romaji":"t1"}}]}}}`)
	}))
	defer srv.Close()

	ids := make([]int, 60) // two chunks: the first completes, the second fails at the HTTP layer
	for i := range ids {
		ids[i] = i + 1
	}
	c := NewClient(srv.Client(), srv.URL, 100000, nil)
	res, err := c.FetchMany(t.Context(), ids)
	if err == nil {
		t.Fatal("FetchMany must surface the second chunk's HTTP failure")
	}
	var statusErr *httpx.HTTPStatusError
	if !errors.As(err, &statusErr) {
		t.Errorf("error = %v, want *httpx.HTTPStatusError from the failed chunk", err)
	}
	// The abort path's own contract: the aborting chunk and every chunk after it
	// are UNREQUESTED, while the completed chunk's absences stay memoizable
	// negatives. If the scoping regressed to include the completed chunk's ids,
	// match.prefetch would stop negative-memoizing ids it legitimately proved
	// absent; if it regressed to exclude the aborting chunk's, it would
	// negative-memoize ids whose chunk never ran.
	for _, id := range ids[batchSize:] {
		if got := res.Verdicts[id]; got != VerdictUnrequested {
			t.Errorf("Verdicts[%d] = %s, want VerdictUnrequested (the aborting second chunk)", id, verdictName(got))
		}
	}
	for _, id := range ids[1:batchSize] {
		if got := res.Verdicts[id]; got != VerdictAbsent {
			t.Errorf("Verdicts[%d] = %s, want VerdictAbsent: it belongs to the completed first chunk", id, verdictName(got))
		}
	}
	if got := res.Verdicts[1]; got != VerdictFound {
		t.Fatalf("Verdicts[1] = %s, want VerdictFound (the completed first chunk is preserved)", verdictName(got))
	}
	if got := res.Media[1].Titles; !slices.Equal(got, []string{"t1"}) {
		t.Errorf("media[1].Titles = %v, want [t1] (completed chunk preserved on a later HTTP failure)", got)
	}
}

// TestRequestMarshalErrorMakesNoAttempt pins the request-construction
// boundary of the shared request helper: variables that cannot marshal
// surface as the wrapped marshal error before any throttle wait or outbound
// attempt, so Stats().Calls stays 0 (the same no-attempt invariant
// TestDoRejectsUnparseableURL pins for the URL-construction branch).
func TestRequestMarshalErrorMakesNoAttempt(t *testing.T) {
	c := NewClient(http.DefaultClient, "http://127.0.0.1:1", 60, nil)
	_, err := c.request(t.Context(), query, map[string]any{"bad": make(chan int)})
	if err == nil {
		t.Fatal("request() with unmarshalable variables = nil error, want a marshal error")
	}
	if !strings.Contains(err.Error(), "anilist: marshal request:") {
		t.Errorf("error = %q, want the anilist marshal-request wrap", err)
	}
	if got := c.Stats().Calls; got != 0 {
		t.Errorf("Stats().Calls = %d, want 0 (a request that cannot be marshaled is never an outbound attempt)", got)
	}
}

// TestFetchManyKeepsFirstRecordErrorAcrossChunks pins the documented
// "first record error is surfaced" side of the batch contract: when two
// chunks each contain a poisoned record, the error returned beside the merged
// result is the FIRST chunk's record error, not overwritten by the second
// chunk's, while both chunks' valid records still merge. It also pins the
// ACCUMULATION of VerdictUnverified across both poisoned chunks: the matcher
// reads the verdict to tell an untrustworthy absence from a definitive miss, so
// a regression to last-chunk-only marking would negative-memoize the first
// chunk's ids for the memo TTL on the strength of a response that was not
// trustworthy.
func TestFetchManyKeepsFirstRecordErrorAcrossChunks(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			// Chunk 1: a missing-id record beside a valid sibling.
			fmt.Fprint(w, `{"data":{"Page":{"media":[{"id":0,"format":"TV","seasonYear":2020,"title":{"romaji":"poisoned-one"}},{"id":1,"format":"TV","seasonYear":2020,"title":{"romaji":"t1"}}]}}}`)
			return
		}
		// Chunk 2: a duplicate-id conflict, a different record error.
		fmt.Fprint(w, `{"data":{"Page":{"media":[{"id":51,"format":"TV","seasonYear":2020,"title":{"romaji":"first"}},{"id":51,"format":"TV","seasonYear":2020,"title":{"romaji":"second"}},{"id":52,"format":"TV","seasonYear":2020,"title":{"romaji":"t52"}}]}}}`)
	}))
	defer srv.Close()

	ids := make([]int, 60) // two chunks, each carrying its own record error
	for i := range ids {
		ids[i] = i + 1
	}
	c := NewClient(srv.Client(), srv.URL, 100000, nil)
	res, err := c.FetchMany(t.Context(), ids)
	out := res.Media
	if err == nil {
		t.Fatal("FetchMany must surface a record error")
	}
	if !errors.Is(err, errBatchRecord) {
		t.Errorf("error = %v, want errBatchRecord classification", err)
	}
	for _, id := range ids {
		want := VerdictUnverified
		if _, found := out[id]; found {
			want = VerdictFound
		}
		if got := res.Verdicts[id]; got != want {
			t.Errorf("Verdicts[%d] = %s, want %s: BOTH chunks were poisoned, so no absence is definitive",
				id, verdictName(got), verdictName(want))
		}
	}
	if !strings.Contains(err.Error(), "media record 0 missing id") {
		t.Errorf("error = %q, want the FIRST chunk's record error (missing id), not a later chunk's", err.Error())
	}
	if strings.Contains(err.Error(), "duplicates id") {
		t.Errorf("error = %q, must not be overwritten by the second chunk's duplicate-id error", err.Error())
	}
	if got := out[1].Titles; len(got) != 1 || got[0] != "t1" {
		t.Errorf("out[1].Titles = %v, want [t1]", got)
	}
	if got := out[52].Titles; len(got) != 1 || got[0] != "t52" {
		t.Errorf("out[52].Titles = %v, want [t52] (second chunk still merged)", got)
	}
}

// TestFetchRejectsNonPositiveIDWithoutRequest pins Fetch's fast-rejection
// guard: a zero or negative id is rejected locally with the invalid-media-id
// error and never issues an HTTP attempt (Stats().Calls stays 0) - relaxing
// the guard's boundary from <= 0 to < 0 would instead send id 0 through
// three doomed HTTP attempts against the unroutable base URL.
func TestFetchRejectsNonPositiveIDWithoutRequest(t *testing.T) {
	c := NewClient(http.DefaultClient, "http://127.0.0.1:1", 60, nil)

	if _, err := c.Fetch(t.Context(), 0); err == nil || !strings.Contains(err.Error(), "invalid media id 0") {
		t.Errorf("Fetch(0) error = %v, want invalid-media-id rejection", err)
	}
	if _, err := c.Fetch(t.Context(), -1); err == nil || !strings.Contains(err.Error(), "invalid media id -1") {
		t.Errorf("Fetch(-1) error = %v, want invalid-media-id rejection", err)
	}
	if got := c.Stats().Calls; got != 0 {
		t.Errorf("Stats().Calls = %d, want 0 (invalid ids must not issue requests)", got)
	}
}

// TestDoSendsRequiredHeaders pins the outbound identity/content contract the
// package doc and appinfo document: every GraphQL POST carries the shared
// appinfo.UserAgent (AniList politeness -- one consistent identity across
// clients) and the JSON Content-Type/Accept pair.
func TestDoSendsRequiredHeaders(t *testing.T) {
	var mu sync.Mutex
	var gotUA, gotContentType, gotAccept, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotUA = r.Header.Get("User-Agent")
		gotContentType = r.Header.Get("Content-Type")
		gotAccept = r.Header.Get("Accept")
		gotMethod = r.Method
		mu.Unlock()
		fmt.Fprint(w, `{"data":{"Media":{"id":1,"format":"TV","seasonYear":2023,"title":{"romaji":"A"}}}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL, 100000, nil)
	if _, err := c.Fetch(t.Context(), 1); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotUA != appinfo.UserAgent {
		t.Errorf("User-Agent = %q, want %q", gotUA, appinfo.UserAgent)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q, want application/json", gotAccept)
	}
}

// TestTransientEnvelopeStatusRetries pins the class httpx cannot see: AniList
// answers a server-side fault as HTTP 200 carrying {"errors":[{"status":500}]},
// so by the time the parser reaches it the attempt has already been recorded as
// a success and no retry can ever happen. Classifying it at the retry boundary
// puts it back inside the bounded budget. A 404 envelope must NOT be caught by
// that check - it is AniList's genuine not-found and Fetch's ErrNotFound
// contract depends on it reaching the parser.
func TestTransientEnvelopeStatusRetries(t *testing.T) {
	for name, tc := range map[string]struct {
		body      string
		wantCalls int64
		wantFound bool
	}{
		"server fault inside a 200 envelope": {
			body:      `{"errors":[{"message":"Internal Server Error","status":500}]}`,
			wantCalls: int64(maxAttempts),
		},
		"not-found envelope is terminal": {
			body:      `{"data":{"Media":null},"errors":[{"message":"Not Found.","status":404}]}`,
			wantCalls: 1,
			wantFound: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			c := NewClient(srv.Client(), srv.URL, 100000, nil)
			_, err := c.Fetch(t.Context(), 1)

			if err == nil {
				t.Fatal("Fetch() = nil error, want an error")
			}
			if got := errors.Is(err, ErrNotFound); got != tc.wantFound {
				t.Errorf("errors.Is(err, ErrNotFound) = %v, want %v (err = %v)", got, tc.wantFound, err)
			}
			if got := c.Stats().Calls; got != tc.wantCalls {
				t.Errorf("Stats().Calls = %d, want %d", got, tc.wantCalls)
			}
		})
	}
}

// TestFetchManyScopesRecordErrorToItsChunk pins the batch verdicts' SCOPE. A
// record-local defect in one chunk must not withdraw the completion evidence of
// the others: only the offending chunk's ids read VerdictUnverified, so a caller
// can still memoize the negatives the clean chunks definitively answered.
// Without the scoping, one malformed record dumped every pending id into a
// rate-limited per-id fallback.
func TestFetchManyScopesRecordErrorToItsChunk(t *testing.T) {
	// Two chunks: the first carries a record with no usable title (record-local),
	// the second is clean and answers nothing (its ids are definitively absent).
	ids := make([]int, 0, batchSize+2)
	for i := 1; i <= batchSize+2; i++ {
		ids = append(ids, i)
	}
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = io.WriteString(w, `{"data":{"Page":{"media":[{"id":1,"title":{"romaji":"","english":"","native":""}}]}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"Page":{"media":[]}}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL, 100000, nil)
	res, err := c.FetchMany(t.Context(), ids)

	if err == nil {
		t.Fatal("FetchMany() error = nil, want the record-local failure")
	}
	if !errors.Is(err, errBatchRecord) {
		t.Errorf("errors.Is(err, errBatchRecord) = false, want true (the sentinel must survive)")
	}
	for _, id := range ids[:batchSize] {
		if got := res.Verdicts[id]; got != VerdictUnverified {
			t.Errorf("Verdicts[%d] = %s, want VerdictUnverified (the failing chunk)", id, verdictName(got))
		}
	}
	for _, id := range ids[batchSize:] {
		if got := res.Verdicts[id]; got != VerdictAbsent {
			t.Errorf("Verdicts[%d] = %s, want VerdictAbsent: it belongs to the clean second chunk", id, verdictName(got))
		}
	}
}

// TestFetchDoesNotRetryUnparseableBody pins transientEnvelopeError's documented
// exclusion: an unparseable body is the parser's business, not the retrier's.
// The envelope classifier runs on EVERY 200 before parsing, so if its
// json.Unmarshal guard stopped returning nil the same malformed body would be
// read as an upstream fault and burn all maxAttempts rate-limited AniList
// requests per lookup on a response that can never succeed.
func TestFetchDoesNotRetryUnparseableBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":`)
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL, 100000, nil)
	_, err := c.Fetch(t.Context(), 1)
	if err == nil {
		t.Fatal("Fetch() on an unparseable body = nil error, want a parse failure")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, must not be negative-memoizable", err)
	}
	if got := c.Stats().Calls; got != 1 {
		t.Errorf("Stats().Calls = %d, want 1 (a malformed body must not be retried as an upstream fault)", got)
	}
}

// TestEnvelopeRateLimitCountsOneWait pins the interaction between the
// envelope-429 path and the proactive low-budget observer: a single HTTP-200
// carrying errors:[{status:429}] alongside X-RateLimit-Remaining: 0 is ONE
// rate-limit response, so it must penalize the throttle once and report one
// Stats().RateLimitWaits. Observing the budget headers before classifying the
// envelope reported that one response as two waits, which desynchronizes the
// cycle-complete anilist_waits counter from the events it documents.
func TestEnvelopeRateLimitCountsOneWait(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.Header().Set("X-RateLimit-Remaining", "0")
			_, _ = io.WriteString(w, `{"errors":[{"message":"Too Many Requests","status":429}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"Media":{"id":1,"format":"TV","seasonYear":2023,"title":{"romaji":"A"}}}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL, 100000, nil)
	if _, err := c.Fetch(t.Context(), 1); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got := c.Stats()
	if got.Calls != 2 {
		t.Errorf("Stats().Calls = %d, want 2 (the envelope 429 is retried once)", got.Calls)
	}
	if got.RateLimitWaits != 1 {
		t.Errorf("Stats().RateLimitWaits = %d, want 1 (one rate-limit response is one wait)", got.RateLimitWaits)
	}
}

// TestFetchManyJoinsEarlierRecordErrorWithLaterAbort pins the error-join
// preservation rule on the one path that reaches it: a record-local defect in
// an earlier chunk followed by an ABORTING envelope failure in a later one. The
// abort must lead (it is the classification every caller reads first) while the
// poisoned record the scoping design exists to surface stays in the joined
// error, and no id reads a definitive verdict, so nothing untrustworthy is
// negative-memoized.
func TestFetchManyJoinsEarlierRecordErrorWithLaterAbort(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			// Chunk 1: a missing-id record (record-local) beside a valid sibling.
			_, _ = io.WriteString(w, `{"data":{"Page":{"media":[{"id":0,"title":{"romaji":"poisoned"}},{"id":1,"format":"TV","seasonYear":2020,"title":{"romaji":"t1"}}]}}}`)
			return
		}
		// Chunk 2: a GraphQL-level envelope error, which aborts the batch.
		_, _ = io.WriteString(w, `{"errors":[{"message":"boom"}]}`)
	}))
	defer srv.Close()

	ids := make([]int, batchSize+2) // two chunks: record-local, then aborting
	for i := range ids {
		ids[i] = i + 1
	}
	c := NewClient(srv.Client(), srv.URL, 100000, nil)
	res, err := c.FetchMany(t.Context(), ids)
	if err == nil {
		t.Fatal("FetchMany must surface the aborting chunk's envelope error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %q, want the aborting envelope error to lead", err.Error())
	}
	if !strings.Contains(err.Error(), "media record 0 missing id") {
		t.Errorf("error = %q, want the earlier chunk's record diagnostic preserved", err.Error())
	}
	if got := res.Verdicts[1]; got != VerdictFound {
		t.Errorf("Verdicts[1] = %s, want VerdictFound (the poisoned chunk's valid record is still valid)", verdictName(got))
	}
	for _, id := range ids[1:batchSize] {
		if got := res.Verdicts[id]; got != VerdictUnverified {
			t.Errorf("Verdicts[%d] = %s, want VerdictUnverified (its chunk answered poisoned)", id, verdictName(got))
		}
	}
	for _, id := range ids[batchSize:] {
		if got := res.Verdicts[id]; got != VerdictUnrequested {
			t.Errorf("Verdicts[%d] = %s, want VerdictUnrequested (the aborting chunk)", id, verdictName(got))
		}
	}
	if got := res.Media[1].Titles; !slices.Equal(got, []string{"t1"}) {
		t.Errorf("media[1].Titles = %v, want [t1] (the completed chunk's valid record survives)", got)
	}
}

// TestDoObservesRateHeadersOnErrorStatus pins the documented reason do() reads
// the budget headers on a non-429 ERROR status too: AniList stamps
// X-RateLimit-Remaining/Reset on a 4xx/5xx as well, and dropping a
// low-remaining signal there lets the next lookup race into the 429 the
// pre-emption exists to avoid. A 400 is used so the response is terminal in one
// attempt, isolating the observation from the retry budget.
func TestDoObservesRateHeadersOnErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(30*time.Second).Unix()))
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL, 100000, nil)
	if _, err := c.do(t.Context(), []byte(`{}`)); err == nil {
		t.Fatal("do() on a 400 = nil error, want a status error")
	}
	if got := c.Stats().RateLimitWaits; got != 1 {
		t.Errorf("Stats().RateLimitWaits = %d, want 1 (an exhausted budget reported on an error status must still back off)", got)
	}
	if wait := c.throttle.reserve(); wait <= 0 {
		t.Errorf("throttle wait after the low-budget 400 = %v, want the reset window", wait)
	}
}

// TestFetchManyScopesUnrequestedIDsToTheAbandonedTail pins the PRODUCER half of
// the VerdictUnrequested contract. VerdictUnverified says "do not memoize this
// absence"; VerdictUnrequested says the strictly narrower "no request ever
// covered this id, so re-batch it 50 at a time" - and match.prefetch switches on
// exactly that verdict to avoid regressing an abandoned tail into one
// rate-limited per-id Fetch each. The consumer's own test builds its verdict map
// by hand, so nothing in the tree fails if FetchMany stops distinguishing the
// two. Three chunks are needed to tell them apart: chunk 1 completes with a
// record-local defect (unverified but ANSWERED, so never re-batched), chunk 2
// aborts, and chunk 3 is never requested.
func TestFetchManyScopesUnrequestedIDsToTheAbandonedTail(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			// Chunk 1: a missing-id record (record-local) beside a valid sibling.
			_, _ = io.WriteString(w, `{"data":{"Page":{"media":[{"id":0,"title":{"romaji":"poisoned"}},{"id":1,"format":"TV","seasonYear":2020,"title":{"romaji":"t1"}}]}}}`)
			return
		}
		// Chunk 2: a GraphQL-level envelope error, which aborts the batch.
		_, _ = io.WriteString(w, `{"errors":[{"message":"boom"}]}`)
	}))
	defer srv.Close()

	ids := make([]int, 3*batchSize) // record-local, aborting, never requested
	for i := range ids {
		ids[i] = i + 1
	}
	c := NewClient(srv.Client(), srv.URL, 100000, nil)
	res, err := c.FetchMany(t.Context(), ids)
	if err == nil {
		t.Fatal("FetchMany must surface the aborting chunk's envelope error")
	}
	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls != 2 {
		t.Errorf("batch calls = %d, want 2 (the third chunk must never be requested after an abort)", gotCalls)
	}
	for _, id := range ids[batchSize:] {
		if got := res.Verdicts[id]; got != VerdictUnrequested {
			t.Errorf("Verdicts[%d] = %s, want VerdictUnrequested: the abandoned tail is the aborting chunk plus every chunk after it, never the answered record-local chunk",
				id, verdictName(got))
		}
	}
	for _, id := range ids[1:batchSize] {
		if got := res.Verdicts[id]; got != VerdictUnverified {
			t.Errorf("Verdicts[%d] = %s, want VerdictUnverified: the record-local chunk ANSWERED, so re-batching it would only re-fetch the same poisoned record",
				id, verdictName(got))
		}
	}
	if got := res.Verdicts[1]; got != VerdictFound {
		t.Errorf("Verdicts[1] = %s, want VerdictFound (the first chunk completed)", verdictName(got))
	}
}
