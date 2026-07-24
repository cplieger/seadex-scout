package anilist

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"
)

func TestDedupeTitles(t *testing.T) {
	got := dedupeTitles("Frieren", "", " \t", "Frieren", "Sousou no Frieren")
	want := []string{"Frieren", "Sousou no Frieren"}
	if !slices.Equal(got, want) {
		t.Errorf("dedupeTitles() = %v, want %v", got, want)
	}
}

func TestParseMedia(t *testing.T) {
	raw := []byte(`{"data":{"Media":{"format":"TV","seasonYear":2023,"title":{"romaji":"Sousou no Frieren","english":"Frieren","native":"x"}}}}`)
	m, err := parseMedia(raw)
	if err != nil {
		t.Fatalf("parseMedia: %v", err)
	}
	if m.Format != "TV" || m.Year != 2023 {
		t.Errorf("format/year = %q/%d, want TV/2023", m.Format, m.Year)
	}
	want := []string{"Sousou no Frieren", "Frieren", "x"}
	if !slices.Equal(m.Titles, want) {
		t.Errorf("titles = %v, want %v", m.Titles, want)
	}
}

func TestParseMediaYearFallsBackToStartDate(t *testing.T) {
	raw := []byte(`{"data":{"Media":{"format":"MOVIE","startDate":{"year":2020},"title":{"romaji":"A"}}}}`)
	m, err := parseMedia(raw)
	if err != nil {
		t.Fatalf("parseMedia: %v", err)
	}
	if m.Year != 2020 {
		t.Errorf("year = %d, want startDate fallback 2020", m.Year)
	}
}

func TestParseMediaNotFoundCarriesMessage(t *testing.T) {
	raw := []byte(`{"data":{"Media":null},"errors":[{"message":"Not Found."}]}`)
	_, err := parseMedia(raw)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if got := err.Error(); got != "anilist: media not found: Not Found." {
		t.Errorf("err.Error() = %q, want upstream message preserved", got)
	}
}

// TestParseMediaNotFoundClassification pins the negative-memoization boundary:
// only an explicit Media null with no error, or AniList's verified not-found
// error shape (status 404 / message "Not Found."), may satisfy
// errors.Is(err, ErrNotFound). An HTTP-200 GraphQL failure or a malformed
// envelope must NOT — the matcher persists ErrNotFound as NotFound:true, so
// misclassifying a transient failure would silently suppress the id forever.
func TestParseMediaNotFoundClassification(t *testing.T) {
	tests := []struct {
		name         string
		raw          string
		wantErr      bool
		wantNotFound bool
	}{
		{name: "empty envelope", raw: `{}`, wantErr: true, wantNotFound: false},
		{name: "missing Media field", raw: `{"data":{}}`, wantErr: true, wantNotFound: false},
		{name: "null Media with non-not-found error", raw: `{"data":{"Media":null},"errors":[{"message":"Internal Server Error"}]}`, wantErr: true, wantNotFound: false},
		{name: "missing data with error", raw: `{"errors":[{"message":"bad request"}]}`, wantErr: true, wantNotFound: false},
		{name: "explicit null no error", raw: `{"data":{"Media":null}}`, wantErr: true, wantNotFound: true},
		{name: "null Media with status 404", raw: `{"data":{"Media":null},"errors":[{"message":"Something went wrong","status":404}]}`, wantErr: true, wantNotFound: true},
		{name: "null Media with Not Found message", raw: `{"data":{"Media":null},"errors":[{"message":"Not Found."}]}`, wantErr: true, wantNotFound: true},
		{name: "embedded control cannot launder into Not Found", raw: `{"data":{"Media":null},"errors":[{"message":"Not\nFound."}]}`, wantErr: true, wantNotFound: false},
		{name: "null Media with Not Found plus second error", raw: `{"data":{"Media":null},"errors":[{"message":"Not Found."},{"message":"Internal Server Error"}]}`, wantErr: true, wantNotFound: false},
		{name: "non-object Media fails decode", raw: `{"data":{"Media":123}}`, wantErr: true, wantNotFound: false},
		{name: "partial response with non-null Media and errors", raw: `{"data":{"Media":{"format":"TV","title":{"romaji":"A"}}},"errors":[{"message":"field resolution failed"}]}`, wantErr: true, wantNotFound: false},
		{name: "empty Media object has no usable title", raw: `{"data":{"Media":{}}}`, wantErr: true, wantNotFound: false},
		{name: "whitespace-only titles are not usable", raw: `{"data":{"Media":{"title":{"romaji":" ","english":"\t"}}}}`, wantErr: true, wantNotFound: false},
		{name: "punctuation-only title normalizes to no match key", raw: `{"data":{"Media":{"format":"TV","title":{"romaji":"!!!"}}}}`, wantErr: true, wantNotFound: false},
		{name: "decorated title keeps a match key", raw: `{"data":{"Media":{"format":"TV","title":{"romaji":"(A)"}}}}`, wantErr: false, wantNotFound: false},
		{name: "invalid UTF-8 in title rejected before decode", raw: "{\"data\":{\"Media\":{\"format\":\"TV\",\"title\":{\"romaji\":\"A\xff\"}}}}", wantErr: true, wantNotFound: false},
		{name: "lone surrogate escape decoding to U+FFFD rejected", raw: `{"data":{"Media":{"format":"TV","title":{"romaji":"A\ud800"}}}}`, wantErr: true, wantNotFound: false},
		{name: "media present", raw: `{"data":{"Media":{"format":"TV","seasonYear":2023,"title":{"romaji":"A"}}}}`, wantErr: false, wantNotFound: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseMedia([]byte(tt.raw))
			if tt.wantErr && err == nil {
				t.Fatal("want error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("parseMedia: %v", err)
			}
			if got := errors.Is(err, ErrNotFound); got != tt.wantNotFound {
				t.Errorf("errors.Is(err, ErrNotFound) = %v (err = %v), want %v", got, err, tt.wantNotFound)
			}
		})
	}
}

func TestParseMediaPage(t *testing.T) {
	raw := []byte(`{"data":{"Page":{"media":[` +
		`{"id":1,"format":"TV","seasonYear":2023,"title":{"romaji":"A"}},` +
		`{"id":2,"format":"MOVIE","startDate":{"year":2019},"title":{"romaji":"B","english":"B"}}` +
		`]}}}`)
	out, err := parseMediaPage(raw)
	if err != nil {
		t.Fatalf("parseMediaPage: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	if out[1].Year != 2023 {
		t.Errorf("id 1 year = %d, want 2023", out[1].Year)
	}
	if out[2].Year != 2019 {
		t.Errorf("id 2 year = %d, want startDate fallback 2019", out[2].Year)
	}
	if !slices.Equal(out[2].Titles, []string{"B"}) {
		t.Errorf("id 2 titles = %v, want deduped [B]", out[2].Titles)
	}
}

func TestParseMediaPageErrorFailsBatch(t *testing.T) {
	raw := []byte(`{"errors":[{"message":"bad request"}]}`)
	if _, err := parseMediaPage(raw); err == nil {
		t.Error("a GraphQL-level error must fail the batch")
	}
}

// TestParseMediaFieldLimits pins the per-field wire limits on the untrusted
// AniList boundary in BOTH the single and batch parsers: boundary-sized
// title/format fields are accepted while max+1 values are rejected outright
// (never truncated, which could forge a normalized-title match), so a hostile
// near-body-cap payload cannot inflate the memo or state.json.
func TestParseMediaFieldLimits(t *testing.T) {
	okTitle := strings.Repeat("a", maxTitleBytes)
	bigTitle := strings.Repeat("a", maxTitleBytes+1)
	okFormat := strings.Repeat("F", maxFormatBytes)
	bigFormat := strings.Repeat("F", maxFormatBytes+1)

	tests := []struct {
		name    string
		fields  string // media object body, without the enclosing braces
		wantErr bool
	}{
		{name: "boundary-sized romaji accepted", fields: `"title":{"romaji":"` + okTitle + `"}`, wantErr: false},
		{name: "over-limit romaji rejected", fields: `"title":{"romaji":"` + bigTitle + `"}`, wantErr: true},
		{name: "over-limit english rejected", fields: `"title":{"romaji":"A","english":"` + bigTitle + `"}`, wantErr: true},
		{name: "over-limit native rejected", fields: `"title":{"romaji":"A","native":"` + bigTitle + `"}`, wantErr: true},
		{name: "boundary-sized format accepted", fields: `"format":"` + okFormat + `","title":{"romaji":"A"}`, wantErr: false},
		{name: "over-limit format rejected", fields: `"format":"` + bigFormat + `","title":{"romaji":"A"}`, wantErr: true},
		{name: "control rune in format rejected", fields: `"format":"TV\n","title":{"romaji":"A"}`, wantErr: true},
		{name: "bidi override in format rejected", fields: `"format":"TV\u202e","title":{"romaji":"A"}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			single := []byte(`{"data":{"Media":{` + tt.fields + `}}}`)
			if _, err := parseMedia(single); (err != nil) != tt.wantErr {
				t.Errorf("parseMedia err = %v, wantErr %v", err, tt.wantErr)
			}
			batch := []byte(`{"data":{"Page":{"media":[{"id":1,` + tt.fields + `}]}}}`)
			if _, err := parseMediaPage(batch); (err != nil) != tt.wantErr {
				t.Errorf("parseMediaPage err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestParseMediaPageNullableEnvelope(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "missing data", raw: `{}`, wantErr: true},
		{name: "null Page", raw: `{"data":{"Page":null}}`, wantErr: true},
		{name: "missing Page", raw: `{"data":{}}`, wantErr: true},
		{name: "missing media", raw: `{"data":{"Page":{}}}`, wantErr: true},
		{name: "null media", raw: `{"data":{"Page":{"media":null}}}`, wantErr: true},
		{name: "non-array media (string) rejected", raw: `{"data":{"Page":{"media":"nope"}}}`, wantErr: true},
		{name: "non-array media (object) rejected", raw: `{"data":{"Page":{"media":{}}}}`, wantErr: true},
		{name: "type-mismatched element fails batch", raw: `{"data":{"Page":{"media":[{"id":"x","title":{"romaji":"A"}}]}}}`, wantErr: true},
		{name: "duplicate media ending in null", raw: `{"data":{"Page":{"media":[{"id":1,"title":{"romaji":"A"}}],"media":null}}}`, wantErr: true},
		{name: "record with whitespace-only title fails batch", raw: `{"data":{"Page":{"media":[{"id":1,"title":{"romaji":" "}}]}}}`, wantErr: true},
		{name: "record with punctuation-only title fails batch", raw: `{"data":{"Page":{"media":[{"id":1,"title":{"romaji":"!!!"}}]}}}`, wantErr: true},
		{name: "record with no title fails batch", raw: `{"data":{"Page":{"media":[{"id":1}]}}}`, wantErr: true},
		{name: "invalid UTF-8 in title rejected before decode", raw: "{\"data\":{\"Page\":{\"media\":[{\"id\":1,\"title\":{\"romaji\":\"A\xff\"}}]}}}", wantErr: true},
		{name: "lone surrogate escape decoding to U+FFFD fails batch", raw: `{"data":{"Page":{"media":[{"id":1,"title":{"romaji":"A\ud800"}}]}}}`, wantErr: true},
		{name: "empty media array", raw: `{"data":{"Page":{"media":[]}}}`, wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := parseMediaPage([]byte(tt.raw))
			if tt.wantErr {
				if err == nil {
					t.Fatal("a malformed envelope must fail the batch, got nil error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMediaPage: %v", err)
			}
			if len(out) != 0 {
				t.Errorf("len = %d, want empty map for an explicit empty media array", len(out))
			}
		})
	}
}

// TestParseMediaPageBoundsMediaCardinality pins the wire-cardinality bound on
// the untrusted batch decode: exactly batchSize compact valid records parse,
// while batchSize+1 records fail the whole batch as an envelope error (never
// ErrBatchRecord) rejected before the extra element is decoded — so a hostile
// endpoint cannot expand an attacker-sized media array into []gqlMedia under
// the 1 MiB body cap.
func TestParseMediaPageBoundsMediaCardinality(t *testing.T) {
	page := func(n int) []byte {
		records := make([]string, n)
		for i := range records {
			id := strconv.Itoa(i + 1)
			records[i] = `{"id":` + id + `,"title":{"romaji":"T` + id + `"}}`
		}
		return []byte(`{"data":{"Page":{"media":[` + strings.Join(records, ",") + `]}}}`)
	}

	out, err := parseMediaPage(page(batchSize))
	if err != nil {
		t.Fatalf("parseMediaPage with %d records: %v", batchSize, err)
	}
	if len(out) != batchSize {
		t.Errorf("len = %d, want %d", len(out), batchSize)
	}

	_, err = parseMediaPage(page(batchSize + 1))
	if err == nil {
		t.Fatalf("parseMediaPage with %d records must be rejected", batchSize+1)
	}
	if errors.Is(err, ErrBatchRecord) {
		t.Errorf("over-cardinality must be an envelope error, got record-local %v", err)
	}
}

func TestObserveRateHeadersCapsResetWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := NewClient(http.DefaultClient, "https://example.invalid/graphql", 30, nil)
		resp := &http.Response{Header: make(http.Header)}
		resp.Header.Set("X-RateLimit-Remaining", "1")
		resp.Header.Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(24*time.Hour).Unix(), 10))

		client.observeRateHeaders(resp)

		if wait := client.throttle.reserve(); wait != maxRetryAfter {
			t.Errorf("low-budget reset wait = %v, want exactly the %v cap", wait, maxRetryAfter)
		}
	})
}

// parseMedia is parseMediaForID without the identity invariant — a test-local
// shorthand for exercising the envelope contract (production always binds the
// requested id via Fetch -> parseMediaForID).
func parseMedia(raw []byte) (Media, error) {
	return parseMediaForID(raw, 0)
}

// reserve claims the next slot and returns how long to wait before using it.
// Test-only observation helper for throttle state; production code reserves
// via wait/reserveSlot.
func (t *throttle) reserve() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	return t.reserveSlotLocked(now).Sub(now)
}

// TestThrottleReserveSpacesRequests pins the spacing math: the first slot is
// immediate, and each subsequent reserve is spaced one interval after the
// previous slot (not after the call), so N requests spread across (N-1)
// intervals. synctest's fake clock makes the assertions exact.
func TestThrottleReserveSpacesRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		th := &throttle{interval: 100 * time.Millisecond}
		if got := th.reserve(); got != 0 {
			t.Errorf("first reserve wait = %v, want 0", got)
		}
		if got := th.reserve(); got != 100*time.Millisecond {
			t.Errorf("second reserve wait = %v, want 100ms", got)
		}
		if got := th.reserve(); got != 200*time.Millisecond {
			t.Errorf("third reserve wait = %v, want 200ms", got)
		}
	})
}

// TestThrottlePenalizeNeverShortensSchedule pins penalize's monotonicity: a
// penalty pushes the next slot out, and a later smaller penalty can never pull
// an already-scheduled slot back in (a 429 backoff must not be cancelled by a
// subsequent low-budget hint with a nearer reset).
func TestThrottlePenalizeNeverShortensSchedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		th := &throttle{interval: time.Millisecond}
		th.penalize(500 * time.Millisecond)
		th.penalize(time.Millisecond) // smaller penalty must not shorten the schedule
		if got := th.reserve(); got != 500*time.Millisecond {
			t.Errorf("reserve after penalties = %v, want 500ms", got)
		}
	})
}

// TestThrottleWaitRevalidatesReservationAfterPenalty pins the penalty-epoch
// revalidation: a waiter already holding a reserved slot when penalize fires
// must NOT wake and issue its request inside the penalty window on the stale
// pre-penalty slot - it re-reserves at the end of the penalized schedule, and
// a subsequent reservation stays interval-spaced behind it.
func TestThrottleWaitRevalidatesReservationAfterPenalty(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		th := &throttle{interval: time.Second}
		start := time.Now()
		if err := th.wait(context.Background()); err != nil {
			t.Fatalf("first wait: %v", err)
		}
		// Second waiter holds a reservation one interval out (start+1s).
		done := make(chan time.Time, 1)
		go func() {
			if err := th.wait(context.Background()); err != nil {
				t.Errorf("penalized wait: %v", err)
			}
			done <- time.Now()
		}()
		synctest.Wait() // the waiter has reserved its slot and is sleeping
		// A 429 penalty lands before the outstanding slot matures.
		th.penalize(5 * time.Second)
		woke := <-done
		if got := woke.Sub(start); got != 5*time.Second {
			t.Errorf("penalized waiter proceeded after %v, want exactly the 5s penalty epoch", got)
		}
		// The re-reserved slot consumed start+5s, so the next reservation is
		// interval-spaced behind it at start+6s.
		if got := th.reserve(); got != time.Second {
			t.Errorf("reserve after penalized waiter = %v, want one interval (1s)", got)
		}
	})
}

// TestNewClientCoercesNonPositiveRate pins the documented constructor
// contract that rate values <= 0 are treated as 1 request per minute, so a
// zero rate cannot divide by zero and a negative rate cannot disable the
// throttle spacing.
func TestNewClientCoercesNonPositiveRate(t *testing.T) {
	tests := []struct {
		name string
		rate int
	}{
		{name: "zero rate", rate: 0},
		{name: "negative rate", rate: -5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := NewClient(http.DefaultClient, "https://example.invalid/graphql", tt.rate, nil)
				if got := c.throttle.reserve(); got != 0 {
					t.Errorf("first reserve wait = %v, want 0", got)
				}
				if got := c.throttle.reserve(); got != time.Minute {
					t.Errorf("second reserve wait = %v, want %v (rate coerced to 1/min)", got, time.Minute)
				}
			})
		})
	}
}

func TestObserveRateHeadersMissingResetDefaultsToMinute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := NewClient(http.DefaultClient, "https://example.invalid/graphql", 30, nil)
		resp := &http.Response{Header: make(http.Header)}
		resp.Header.Set("X-RateLimit-Remaining", "1")

		client.observeRateHeaders(resp)

		if wait := client.throttle.reserve(); wait != time.Minute {
			t.Errorf("low-budget wait with no reset header = %v, want exactly the %v default", wait, time.Minute)
		}
		if got := client.Stats().RateLimitWaits; got != 1 {
			t.Errorf("Stats().RateLimitWaits = %d, want 1", got)
		}
	})
}

func TestObserveRateHeadersMalformedResetDefaultsToMinute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := NewClient(http.DefaultClient, "https://example.invalid/graphql", 30, nil)
		resp := &http.Response{Header: make(http.Header)}
		resp.Header.Set("X-RateLimit-Remaining", "0")
		resp.Header.Set("X-RateLimit-Reset", "not-a-timestamp")

		client.observeRateHeaders(resp)

		if wait := client.throttle.reserve(); wait != time.Minute {
			t.Errorf("low-budget wait with malformed reset = %v, want exactly the %v default", wait, time.Minute)
		}
	})
}

// TestSanitizeUpstreamMessage pins the log-forging boundary on untrusted
// upstream error messages: short clean text passes unchanged; C0/C1 controls,
// DEL, line/paragraph separators, and bidi override/isolate runes become
// spaces; and the 200-byte cap cuts on a rune boundary so the result stays
// valid UTF-8 even when the boundary lands inside a multibyte rune.
func TestSanitizeUpstreamMessage(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"short clean text unchanged", "Media not found.", "Media not found."},
		{"C0 newline and DEL cleaned", "line1\nline2\x7f", "line1 line2 "},
		{"C1 CSI and OSC cleaned", "a\u009bb\u009dc", "a b c"},
		{"line and paragraph separators cleaned", "a\u2028b\u2029c", "a b c"},
		{"bidi overrides and isolates cleaned", "a\u202eb\u2066c\u2069d", "a b c d"},
		{"bidi ALM LRM RLM marks cleaned", "a\u061cb\u200ec\u200fd", "a b c d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeUpstreamMessage(tt.in); got != tt.want {
				t.Errorf("sanitizeUpstreamMessage(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestSanitizeUpstreamMessageRuneBoundaryCut pins the cap's UTF-8 safety: a
// clean message whose 200-byte retained-message boundary falls inside a
// multibyte rune is cut back to the rune start, stays valid UTF-8, and remains
// bounded by the 200-byte retained cap plus the three-byte "..." ellipsis
// (203 bytes total).
func TestSanitizeUpstreamMessageRuneBoundaryCut(t *testing.T) {
	// 199 ASCII bytes then a 3-byte rune: the 200-byte boundary lands inside it.
	in := strings.Repeat("a", 199) + "\u4e16\u754c"
	got := sanitizeUpstreamMessage(in)
	if !utf8.ValidString(got) {
		t.Errorf("sanitizeUpstreamMessage() = %q is not valid UTF-8", got)
	}
	if want := strings.Repeat("a", 199) + "..."; got != want {
		t.Errorf("sanitizeUpstreamMessage() = %q, want the cut moved back to the rune start (%q)", got, want)
	}
	if len(got) > 200+len("...") {
		t.Errorf("len = %d, want bounded by 203", len(got))
	}
}

// TestObserveRateHeadersThresholdBoundary pins the lowRemaining gate on both
// sides: a remaining budget AT the threshold (2) backs off for the default
// minute window, while a budget just above it (3), a missing header, and a
// malformed header leave the throttle untouched.
func TestObserveRateHeadersThresholdBoundary(t *testing.T) {
	tests := []struct {
		name        string
		remaining   string
		wantBackoff bool
	}{
		{name: "at threshold backs off", remaining: "2", wantBackoff: true},
		{name: "just above threshold does not back off", remaining: "3", wantBackoff: false},
		{name: "missing header does not back off", remaining: "", wantBackoff: false},
		{name: "malformed header does not back off", remaining: "many", wantBackoff: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := NewClient(http.DefaultClient, "https://example.invalid/graphql", 100000, nil)
				resp := &http.Response{Header: make(http.Header)}
				if tt.remaining != "" {
					resp.Header.Set("X-RateLimit-Remaining", tt.remaining)
				}
				client.observeRateHeaders(resp)
				wait := client.throttle.reserve()
				if tt.wantBackoff {
					if wait != time.Minute {
						t.Errorf("wait = %v, want exactly the %v default backoff at the lowRemaining threshold", wait, time.Minute)
					}
					if got := client.Stats().RateLimitWaits; got != 1 {
						t.Errorf("Stats().RateLimitWaits = %d, want 1", got)
					}
				} else {
					if wait != 0 {
						t.Errorf("wait = %v, want 0 (no backoff above the threshold)", wait)
					}
					if got := client.Stats().RateLimitWaits; got != 0 {
						t.Errorf("Stats().RateLimitWaits = %d, want 0", got)
					}
				}
			})
		})
	}
}

// TestBoundedMediaListUnmarshalTruncatedData pins the json.Unmarshaler
// contract of boundedMediaList against inputs the outer decoder never
// produces (encoding/json hands UnmarshalJSON syntax-valid values only, so
// these EOF branches are unreachable through parseMediaPage): a truncated
// value must error and leave the list unset, never a silent empty decode.
func TestBoundedMediaListUnmarshalTruncatedData(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "empty input", data: ""},
		{name: "unclosed array", data: "["},
		{name: "truncated element", data: `[{"id":`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var l boundedMediaList
			if err := l.UnmarshalJSON([]byte(tt.data)); err == nil {
				t.Fatal("UnmarshalJSON on truncated data = nil error, want error")
			}
			if l.set {
				t.Error("l.set = true after a failed decode, want unset")
			}
			if l.records != nil {
				t.Errorf("l.records = %v after a failed decode, want nil", l.records)
			}
		})
	}
}
