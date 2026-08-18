package mapping

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/seadex-scout/internal/degradation"
)

// TestLoader_refreshCache_rejectionStreakCountsAndResets pins the
// consecutive-rejection streak: each acceptance-guard rejection (here the
// below-half-size shrink guard) advances the persisted Cache.RejectedRefreshes
// - the ONE carrier, read by both the scout's escalation and its
// stale_consecutive_rejections attribute - and an eventually accepted refresh
// resets the streak to zero.
func TestLoader_refreshCache_rejectionStreakCountsAndResets(t *testing.T) {
	var accept atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if accept.Load() {
			_, _ = w.Write([]byte(`[{"anilist_id":1,"type":"tv","tvdb_id":100},{"anilist_id":2,"type":"tv","tvdb_id":200},{"anilist_id":3,"type":"tv","tvdb_id":300},{"anilist_id":4,"type":"tv","tvdb_id":400}]`))
			return
		}
		// One record replacing four trips the below-half-size shrink guard.
		_, _ = w.Write([]byte(`[{"anilist_id":9,"type":"tv","tvdb_id":900}]`))
	}))
	defer ts.Close()

	prev := &Cache{
		FetchedAt: time.Now().Add(-2 * time.Hour),
		Records: []Record{
			{AniListID: 1, Type: "TV", TvdbID: 100},
			{AniListID: 2, Type: "TV", TvdbID: 200},
			{AniListID: 3, Type: "TV", TvdbID: 300},
			{AniListID: 4, Type: "TV", TvdbID: 400},
		},
	}
	l := NewLoader(ts.Client(), ts.URL, WithRefresh(time.Hour), WithLogger(discardLogger()))
	for i := 1; i <= degradation.TickEscalationThreshold; i++ {
		next, err := l.refreshCache(t.Context(), prev)
		var stale *StaleMapError
		if !errors.As(err, &stale) {
			t.Fatalf("rejection %d error = %v, want a *StaleMapError", i, err)
		}
		if next.RejectedRefreshes != i {
			t.Fatalf("RejectedRefreshes after %d rejections = %d, want %d", i, next.RejectedRefreshes, i)
		}
		*prev = next
	}

	accept.Store(true)
	next, err := l.refreshCache(t.Context(), prev)
	if err != nil {
		t.Fatalf("accepted refresh after rejections returned error: %v", err)
	}
	if next.RejectedRefreshes != 0 {
		t.Errorf("accepted refresh RejectedRefreshes = %d, want 0 (acceptance resets the streak)", next.RejectedRefreshes)
	}
	if len(next.Records) != 4 {
		t.Errorf("accepted refresh kept %d records, want 4", len(next.Records))
	}
}

// TestLoader_refreshCache_notModifiedResetsRejectionStreak pins the 304 reset:
// upstream affirming that the cached map is current ends any acceptance-guard
// rejection streak.
func TestLoader_refreshCache_notModifiedResetsRejectionStreak(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer ts.Close()
	prev := &Cache{
		FetchedAt:         time.Now().Add(-2 * time.Hour),
		ETag:              "v1",
		Records:           []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
		RejectedRefreshes: 3,
	}
	l := NewLoader(ts.Client(), ts.URL, WithRefresh(time.Hour), WithLogger(discardLogger()))
	next, err := l.refreshCache(t.Context(), prev)
	if err != nil {
		t.Fatalf("304 refresh returned error: %v", err)
	}
	if next.RejectedRefreshes != 0 {
		t.Errorf("304 RejectedRefreshes = %d, want 0 (a 304 resets the streak)", next.RejectedRefreshes)
	}
}

// TestLoader_refreshCache_transportFailureKeepsRejectionStreak pins that a
// transient outage is not a persistent refusal: a transport failure (no
// response at all) neither advances the persisted streak nor resets it, so the
// scout never escalates on an outage. It used to assert this with a 404, which l-f100
// reclassified as PERSISTENT - see
// TestLoader_refreshCache_operatorRemedyStatusAdvancesRejectionStreak - so the
// transient side is now pinned with the one fetch failure that carries no HTTP
// status at all.
func TestLoader_refreshCache_transportFailureKeepsRejectionStreak(t *testing.T) {
	prev := &Cache{
		FetchedAt:         time.Now().Add(-2 * time.Hour),
		Records:           []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
		RejectedRefreshes: 3,
	}
	l := NewLoader(&http.Client{Transport: errTransport{}}, "http://unused.invalid", WithRefresh(time.Hour), WithLogger(discardLogger()))
	next, err := l.refreshCache(t.Context(), prev)
	var stale *StaleMapError
	if !errors.As(err, &stale) {
		t.Fatalf("transport-failure error = %v, want a *StaleMapError", err)
	}
	if next.RejectedRefreshes != 3 {
		t.Errorf("transport-failure RejectedRefreshes = %d, want 3 (outages neither advance nor reset the streak)", next.RejectedRefreshes)
	}
}

// TestLoader_refreshCache_operatorRemedyStatusAdvancesRejectionStreak pins
// l-f100's persistent half: a status on the FIXED Fribb URL whose only remedy is
// the operator (a 404 or 410 on a URL that is a package constant) is a
// persistent refusal, not a transient outage. Before l-f100, only
// *httpx.ResponseTooLargeError advanced the streak, so such a status warned
// forever from a zero streak and the scout's WARN never escalated.
func TestLoader_refreshCache_operatorRemedyStatusAdvancesRejectionStreak(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", status)
		}))
		prev := &Cache{
			FetchedAt: time.Now().Add(-2 * time.Hour),
			Records:   []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
		}
		l := NewLoader(ts.Client(), ts.URL, WithRefresh(time.Hour), WithLogger(discardLogger()))
		next, err := l.refreshCache(t.Context(), prev)
		ts.Close()
		if _, ok := errors.AsType[*StaleMapError](err); !ok {
			t.Errorf("status %d error = %v, want a *StaleMapError", status, err)
			continue
		}
		if next.RejectedRefreshes != 1 {
			t.Errorf("status %d RejectedRefreshes = %d, want 1 (only the operator clears this status)", status, next.RejectedRefreshes)
		}
	}
}

// TestLoader_refreshCache_comeBackLaterStatusKeepsRejectionStreak pins the other
// half: a come-back-later status (a 5xx here; a 408 and a 429 take the same
// arm) neither advances the persisted streak nor resets it, however many cycles
// in a row it arrives. Exhausting one cycle's retry budget is not evidence of
// permanence, and the ERROR the streak escalates to at
// degradation.TickEscalationThreshold tells the operator to inspect the upstream
// or delete state.json - the wrong instruction for an outage that clears on its
// own.
func TestLoader_refreshCache_comeBackLaterStatusKeepsRejectionStreak(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer ts.Close()

	prev := &Cache{
		FetchedAt:         time.Now().Add(-2 * time.Hour),
		Records:           []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
		RejectedRefreshes: 3,
	}
	l := NewLoader(ts.Client(), ts.URL, WithRefresh(time.Hour), WithLogger(discardLogger()))
	for i := 1; i <= degradation.TickEscalationThreshold; i++ {
		next, err := l.refreshCache(t.Context(), prev)
		if _, ok := errors.AsType[*StaleMapError](err); !ok {
			t.Fatalf("500 %d error = %v, want a *StaleMapError", i, err)
		}
		if next.RejectedRefreshes != 3 {
			t.Fatalf("RejectedRefreshes after %d server errors = %d, want 3 (a self-healing status neither advances nor resets the streak)", i, next.RejectedRefreshes)
		}
		*prev = next
	}
}

// TestLoader_refreshCache_terminalNon2xxReachesEscalationThreshold pins the
// operator-visible half of l-f100: a permanently 404ing Fribb URL escalates the
// scout's mapping log from WARN to ERROR only once the streak reaches
// degradation.TickEscalationThreshold consecutive cycles, so the first refusals
// stay a WARN.
func TestLoader_refreshCache_terminalNon2xxReachesEscalationThreshold(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone for good", http.StatusNotFound)
	}))
	defer ts.Close()

	prev := &Cache{
		FetchedAt: time.Now().Add(-2 * time.Hour),
		Records:   []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
	}
	l := NewLoader(ts.Client(), ts.URL, WithRefresh(time.Hour), WithLogger(discardLogger()))
	for i := 1; i <= degradation.TickEscalationThreshold; i++ {
		next, err := l.refreshCache(t.Context(), prev)
		if err == nil {
			t.Fatalf("refresh %d returned nil error, want a degraded refresh", i)
		}
		if next.RejectedRefreshes != i {
			t.Fatalf("RejectedRefreshes after %d refusals = %d, want %d", i, next.RejectedRefreshes, i)
		}
		if escalates := next.RejectedRefreshes >= degradation.TickEscalationThreshold; escalates != (i >= degradation.TickEscalationThreshold) {
			t.Fatalf("after %d refusals the scout escalation gate = %v, want %v (only the threshold escalates)", i, escalates, i >= degradation.TickEscalationThreshold)
		}
		*prev = next
	}
}

// TestLoader_refreshCache_recordCapBreachAdvancesRejectionStreak pins the
// record-cap exception to the "parse failures don't advance the streak" rule:
// an over-cap body is a persistent guard refusal (an over-cap upstream list
// re-downloads and rejects every cycle, never self-healing), so acceptRefresh
// must route it through rejectRefresh — the errors.Is-matchable sentinel
// survives the *StaleMapError wrap, the stale map is kept, and the persisted
// streak advances so rejections reaches
// degradation.TickEscalationThreshold (the scout's WARN→ERROR escalation point)
// instead of degrading at WARN forever.
func TestLoader_refreshCache_recordCapBreachAdvancesRejectionStreak(t *testing.T) {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i <= maxFribbRecords; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"anilist_id":%d}`, i+1)
	}
	b.WriteByte(']')
	body := b.String()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()

	prev := &Cache{
		FetchedAt:         time.Now().Add(-2 * time.Hour),
		Records:           []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
		RejectedRefreshes: degradation.TickEscalationThreshold - 1,
	}
	l := NewLoader(ts.Client(), ts.URL, WithRefresh(time.Hour), WithLogger(discardLogger()))
	next, err := l.refreshCache(t.Context(), prev)
	if _, ok := errors.AsType[*StaleMapError](err); !ok {
		t.Fatalf("cap-breach refresh error = %v, want a *StaleMapError guard rejection", err)
	}
	if !errors.Is(err, errRecordCapExceeded) {
		t.Errorf("cap-breach error does not match errRecordCapExceeded through the StaleMapError wrap: %v", err)
	}
	if len(next.Records) != 1 || next.Records[0].AniListID != 1 {
		t.Fatalf("cap-breach refresh records = %+v, want stale record id 1", next.Records)
	}
	if next.RejectedRefreshes != degradation.TickEscalationThreshold {
		t.Errorf("cap-breach RejectedRefreshes = %d, want %d (a cap breach advances the streak)", next.RejectedRefreshes, degradation.TickEscalationThreshold)
	}
}

// TestLoader_refreshCache_transientParseFailureKeepsRejectionStreak pins the
// other side of the record-cap exception: an ordinary malformed body is a
// transient parse failure (a partial download or upstream hiccup that can
// self-heal next cycle), so it degrades to the stale map WITHOUT advancing or
// resetting the persisted streak, and its *StaleMapError reports zero
// consecutive rejections — the scout must never escalate to ERROR on it.
func TestLoader_refreshCache_transientParseFailureKeepsRejectionStreak(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"anilist_id":1,`)) // truncated mid-record
	}))
	defer ts.Close()

	prev := &Cache{
		FetchedAt:         time.Now().Add(-2 * time.Hour),
		Records:           []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
		RejectedRefreshes: 3,
	}
	l := NewLoader(ts.Client(), ts.URL, WithRefresh(time.Hour), WithLogger(discardLogger()))
	next, err := l.refreshCache(t.Context(), prev)
	if _, ok := errors.AsType[*StaleMapError](err); !ok {
		t.Fatalf("parse-failure refresh error = %v, want a *StaleMapError", err)
	}
	if errors.Is(err, errRecordCapExceeded) {
		t.Errorf("parse-failure error wrongly matches errRecordCapExceeded: %v", err)
	}
	if next.RejectedRefreshes != 3 {
		t.Errorf("parse-failure RejectedRefreshes = %d, want 3 (transient parse failures neither advance nor reset the streak)", next.RejectedRefreshes)
	}
}

// TestLoader_refreshCache_overCapBodyAdvancesRejectionStreak pins the download
// size cap as a PERSISTENT guard refusal rather than a transient fetch failure:
// the cap is deterministic on upstream size, so a list that has organically
// grown past maxMapBytes re-downloads and is refused every cycle without ever
// self-healing, which is what the streak exists to escalate.
func TestLoader_refreshCache_overCapBodyAdvancesRejectionStreak(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A body over maxMapBytes; the content never has to parse, the wire
		// layer refuses it first.
		w.Header().Set("Content-Type", "application/json")
		chunk := strings.Repeat("a", 1<<20)
		for written := 0; written <= maxMapBytes; written += len(chunk) {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	}))
	defer ts.Close()

	prev := &Cache{
		FetchedAt: time.Now().Add(-2 * time.Hour),
		Records:   []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
	}
	l := NewLoader(ts.Client(), ts.URL, WithRefresh(time.Hour), WithLogger(discardLogger()))
	next, err := l.refreshCache(t.Context(), prev)
	var stale *StaleMapError
	if !errors.As(err, &stale) {
		t.Fatalf("over-cap body error = %v, want a *StaleMapError", err)
	}
	if next.RejectedRefreshes != 1 {
		t.Errorf("RejectedRefreshes = %d, want 1 (an over-cap body never self-heals)", next.RejectedRefreshes)
	}
	if got := stale.LogAttrs(); !attrsContain(got, "stale_reason", "refresh exceeded size cap") {
		t.Errorf("stale_reason attrs = %v, want the size-cap reason", got)
	}
}

// TestLoader_refreshCache_nonArrayBodyAdvancesRejectionStreak pins a non-array
// top-level document as content-shape evidence that the upstream schema moved:
// truncation cannot change a body's FIRST token, so this class fails identically
// every cycle and must escalate, unlike the mid-stream malformed body pinned by
// TestLoader_refreshCache_transientParseFailureKeepsRejectionStreak.
func TestLoader_refreshCache_nonArrayBodyAdvancesRejectionStreak(t *testing.T) {
	for _, tc := range map[string]string{
		"object document": `{"data":[{"anilist_id":1,"type":"tv","tvdb_id":100}]}`,
		"null document":   `null`,
	} {
		body := tc
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		prev := &Cache{
			FetchedAt: time.Now().Add(-2 * time.Hour),
			Records:   []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
		}
		l := NewLoader(ts.Client(), ts.URL, WithRefresh(time.Hour), WithLogger(discardLogger()))
		next, err := l.refreshCache(t.Context(), prev)
		ts.Close()
		var stale *StaleMapError
		if !errors.As(err, &stale) {
			t.Errorf("body %q error = %v, want a *StaleMapError", body, err)
			continue
		}
		if next.RejectedRefreshes != 1 {
			t.Errorf("body %q RejectedRefreshes = %d, want 1 (a moved schema never self-heals)", body, next.RejectedRefreshes)
		}
	}
}

// TestLoader_refreshCache_streakAdvancesWithNoUsableCache pins the streak as
// state about the UPSTREAM, not about the cache: on a first boot whose every
// refresh is refused there is no usable stale map to return, and gating the
// increment on one would freeze the streak at 0 and leave the loader degrading
// at WARN forever while the feed and comparison stay disabled.
func TestLoader_refreshCache_streakAdvancesWithNoUsableCache(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`)) // a moved schema, refused every cycle
	}))
	defer ts.Close()

	prev := &Cache{} // first boot: no records, no validators
	l := NewLoader(ts.Client(), ts.URL, WithRefresh(time.Hour), WithLogger(discardLogger()))
	for i := 1; i <= degradation.TickEscalationThreshold; i++ {
		next, err := l.refreshCache(t.Context(), prev)
		if err == nil {
			t.Fatalf("refresh %d returned nil error, want the no-cache error", i)
		}
		var stale *StaleMapError
		if errors.As(err, &stale) {
			t.Fatalf("refresh %d returned a *StaleMapError, want the no-cache error (there is no usable map to serve)", i)
		}
		if next.RejectedRefreshes != i {
			t.Fatalf("RejectedRefreshes after %d refusals = %d, want %d", i, next.RejectedRefreshes, i)
		}
		*prev = next
	}
	if prev.RejectedRefreshes < degradation.TickEscalationThreshold {
		t.Errorf("streak reached %d, want >= %d so the scout can escalate", prev.RejectedRefreshes, degradation.TickEscalationThreshold)
	}
}

// TestLoader_refreshCache_notModifiedWithoutUsableCacheAdvancesStreak pins the
// documented streak classification of a 304 answered to a request that carried
// NO validators (conditionalGet suppresses them whenever the cache is
// unusable): that is an upstream or intermediary protocol violation, which
// repeats identically every cycle and never self-heals, so reuseCachedRecords
// routes it through rejectRefresh rather than plain staleOrFail. The two
// existing unusable-cache 304 tests assert only the error and the suppressed
// validators, so a regression back to staleOrFail would freeze the streak at 0
// and the scout's WARN would never escalate to ERROR at
// degradation.TickEscalationThreshold - a permanently broken upstream degrading
// silently at WARN forever.
func TestLoader_refreshCache_notModifiedWithoutUsableCacheAdvancesStreak(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
			t.Error("conditional GET sent validators despite an unusable cache; they must be suppressed")
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer ts.Close()

	prev := &Cache{FetchedAt: time.Now().Add(-2 * time.Hour), ETag: "v1"}
	l := NewLoader(ts.Client(), ts.URL, WithRefresh(time.Hour), WithLogger(discardLogger()))
	for i := 1; i <= degradation.TickEscalationThreshold; i++ {
		next, err := l.refreshCache(t.Context(), prev)
		if err == nil {
			t.Fatalf("304 %d over an unusable cache returned nil error, want the no-cache error", i)
		}
		var stale *StaleMapError
		if errors.As(err, &stale) {
			t.Fatalf("304 %d returned a *StaleMapError, want the no-cache error (there is no usable map to serve)", i)
		}
		if next.RejectedRefreshes != i {
			t.Fatalf("RejectedRefreshes after %d unusable-cache 304s = %d, want %d (a protocol-violating 304 never self-heals)", i, next.RejectedRefreshes, i)
		}
		*prev = next
	}
}

// TestIsPersistentRefreshFailure is the table over the ONE home of the
// transient-vs-persistent refresh classification (l-f100's structural half).
// Before it, each failure arm of refreshCache chose staleOrFail or
// rejectRefresh for itself, so Cache.RejectedRefreshes' documented list and the
// code implementing it could drift - and had, in three arms. Every class named
// persistent in isPersistentRefreshFailure's doc comment appears here, so adding a
// class to the doc without the code (or the reverse) fails.
//
// The ACCEPTED third outcome is not a failure and so is not classified here:
// its two reset paths are pinned end-to-end by
// TestLoader_refreshCache_rejectionStreakCountsAndResets (an accepted refresh)
// and TestLoader_refreshCache_notModifiedResetsRejectionStreak (a 304 over a
// usable cache, including its "rejection streak ended by 304 revalidation"
// INFO, pinned by TestLoader_refreshCache_notModifiedLogsEndedRejectionStreak).
func TestIsPersistentRefreshFailure(t *testing.T) {
	for name, tc := range map[string]struct {
		cause error
		class refreshFailureClass
		want  bool
	}{
		// Persistent: each one re-downloads or re-refuses identically forever.
		"parse-time record cap":   {fmt.Errorf("parse: %w", errRecordCapExceeded), failureParse, true},
		"identifier budget":       {fmt.Errorf("parse: %w", errIdentifierBudgetExceeded), failureParse, true},
		"non-array document":      {logSafeCause(fmt.Errorf("%w (got null)", errNotJSONArray)), failureParse, true},
		"download size cap":       {&httpx.ResponseTooLargeError{Limit: maxMapBytes}, failureFetch, true},
		"validator-less 304":      {nil, failureNotModifiedUnusable, true},
		"acceptance invariant":    {errors.New("arr identifier coverage 1/200 is below minimum 2"), failureValidation, true},
		"whole-map shrink guard":  {nil, failureShrunk, true},
		"operator-remedy 404":     {&httpx.HTTPStatusError{Code: http.StatusNotFound}, failureFetch, true},
		"operator-remedy 410":     {&httpx.HTTPStatusError{Code: http.StatusGone}, failureFetch, true},
		"operator-remedy 301":     {&httpx.HTTPStatusError{Code: http.StatusMovedPermanently}, failureFetch, true},
		"operator-remedy 400":     {&httpx.HTTPStatusError{Code: http.StatusBadRequest}, failureFetch, true},
		"terminal 401":            {&httpx.AuthError{Msg: "invalid API key (401)"}, failureFetch, true},
		"wrapped operator status": {fmt.Errorf("fetch: %w", &httpx.HTTPStatusError{Code: http.StatusNotFound}), failureFetch, true},
		// Transient: can succeed on the next attempt.
		"mid-stream truncation":      {logSafeCause(errors.New("unexpected EOF at element 4")), failureParse, false},
		"transport error":            {errors.New("transport refused by test"), failureFetch, false},
		"2xx without representation": {errors.New("unexpected status 204 on conditional request"), failureFetch, false},
		// Come-back-later statuses: retry exhaustion within one cycle is not
		// evidence the upstream needs an operator, so they must NOT advance the
		// streak toward the operator-action ERROR.
		"come-back-later 408":     {&httpx.HTTPStatusError{Code: http.StatusRequestTimeout}, failureFetch, false},
		"come-back-later 500":     {&httpx.HTTPStatusError{Code: http.StatusInternalServerError}, failureFetch, false},
		"come-back-later 503":     {&httpx.HTTPStatusError{Code: http.StatusServiceUnavailable}, failureFetch, false},
		"come-back-later 429":     {&httpx.RateLimitError{Msg: "rate limited (429)"}, failureFetch, false},
		"wrapped come-back-later": {fmt.Errorf("fetch: %w", &httpx.HTTPStatusError{Code: http.StatusBadGateway}), failureFetch, false},
	} {
		if got := isPersistentRefreshFailure(tc.class, tc.cause); got != tc.want {
			t.Errorf("isPersistentRefreshFailure(%v, %v) = %v, want %v (%s)", tc.class, tc.cause, got, tc.want, name)
		}
	}
}

// attrsContain reports whether the flattened slog key/value pairs carry key
// with the wanted value.
func attrsContain(attrs []any, key, want string) bool {
	for i := 0; i+1 < len(attrs); i += 2 {
		if k, ok := attrs[i].(string); ok && k == key {
			if v, ok := attrs[i+1].(string); ok {
				return v == want
			}
		}
	}
	return false
}
