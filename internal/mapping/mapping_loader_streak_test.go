package mapping

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/degradation"
)

// TestLoader_refreshCache_rejectionStreakCountsAndResets pins the
// consecutive-rejection streak: each acceptance-guard rejection (here the
// below-half-size shrink guard) advances the persisted Cache.RejectedRefreshes
// and carries the streak on the *StaleMapError (ConsecutiveRejections), and an
// eventually accepted refresh resets the streak to zero.
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
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	for i := 1; i <= degradation.EscalationThreshold; i++ {
		next, err := l.refreshCache(context.Background(), prev)
		var stale *StaleMapError
		if !errors.As(err, &stale) {
			t.Fatalf("rejection %d error = %v, want a *StaleMapError", i, err)
		}
		if next.RejectedRefreshes != i {
			t.Fatalf("RejectedRefreshes after %d rejections = %d, want %d", i, next.RejectedRefreshes, i)
		}
		if stale.ConsecutiveRejections() != i {
			t.Fatalf("ConsecutiveRejections after %d rejections = %d, want %d", i, stale.ConsecutiveRejections(), i)
		}
		*prev = next
	}

	accept.Store(true)
	next, err := l.refreshCache(context.Background(), prev)
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
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	next, err := l.refreshCache(context.Background(), prev)
	if err != nil {
		t.Fatalf("304 refresh returned error: %v", err)
	}
	if next.RejectedRefreshes != 0 {
		t.Errorf("304 RejectedRefreshes = %d, want 0 (a 304 resets the streak)", next.RejectedRefreshes)
	}
}

// TestLoader_refreshCache_fetchFailureKeepsRejectionStreak pins that a
// transient outage is not a guard rejection: a fetch failure neither advances
// the persisted streak nor resets it, and its *StaleMapError reports zero
// consecutive rejections (so the scout never escalates on an outage).
func TestLoader_refreshCache_fetchFailureKeepsRejectionStreak(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusNotFound)
	}))
	defer ts.Close()
	prev := &Cache{
		FetchedAt:         time.Now().Add(-2 * time.Hour),
		Records:           []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
		RejectedRefreshes: 3,
	}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	next, err := l.refreshCache(context.Background(), prev)
	var stale *StaleMapError
	if !errors.As(err, &stale) {
		t.Fatalf("fetch-failure error = %v, want a *StaleMapError", err)
	}
	if next.RejectedRefreshes != 3 {
		t.Errorf("fetch-failure RejectedRefreshes = %d, want 3 (outages neither advance nor reset the streak)", next.RejectedRefreshes)
	}
	if stale.ConsecutiveRejections() != 0 {
		t.Errorf("fetch-failure ConsecutiveRejections = %d, want 0 (not a guard rejection)", stale.ConsecutiveRejections())
	}
}

// TestLoader_refreshCache_recordCapBreachAdvancesRejectionStreak pins the
// record-cap exception to the "parse failures don't advance the streak" rule:
// an over-cap body is a persistent guard refusal (an over-cap upstream list
// re-downloads and rejects every cycle, never self-healing), so acceptRefresh
// must route it through rejectRefresh — the errors.Is-matchable sentinel
// survives the *StaleMapError wrap, the stale map is kept, and the persisted
// streak advances so ConsecutiveRejections reaches
// degradation.EscalationThreshold (the scout's WARN→ERROR escalation point)
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
		RejectedRefreshes: degradation.EscalationThreshold - 1,
	}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	next, err := l.refreshCache(context.Background(), prev)
	stale, ok := errors.AsType[*StaleMapError](err)
	if !ok {
		t.Fatalf("cap-breach refresh error = %v, want a *StaleMapError guard rejection", err)
	}
	if !errors.Is(err, errRecordCapExceeded) {
		t.Errorf("cap-breach error does not match errRecordCapExceeded through the StaleMapError wrap: %v", err)
	}
	if len(next.Records) != 1 || next.Records[0].AniListID != 1 {
		t.Fatalf("cap-breach refresh records = %+v, want stale record id 1", next.Records)
	}
	if next.RejectedRefreshes != degradation.EscalationThreshold {
		t.Errorf("cap-breach RejectedRefreshes = %d, want %d (a cap breach advances the streak)", next.RejectedRefreshes, degradation.EscalationThreshold)
	}
	if stale.ConsecutiveRejections() != degradation.EscalationThreshold {
		t.Errorf("cap-breach ConsecutiveRejections = %d, want %d (the scout escalates to ERROR at the threshold)", stale.ConsecutiveRejections(), degradation.EscalationThreshold)
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
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	next, err := l.refreshCache(context.Background(), prev)
	stale, ok := errors.AsType[*StaleMapError](err)
	if !ok {
		t.Fatalf("parse-failure refresh error = %v, want a *StaleMapError", err)
	}
	if errors.Is(err, errRecordCapExceeded) {
		t.Errorf("parse-failure error wrongly matches errRecordCapExceeded: %v", err)
	}
	if next.RejectedRefreshes != 3 {
		t.Errorf("parse-failure RejectedRefreshes = %d, want 3 (transient parse failures neither advance nor reset the streak)", next.RejectedRefreshes)
	}
	if stale.ConsecutiveRejections() != 0 {
		t.Errorf("parse-failure ConsecutiveRejections = %d, want 0 (not a guard rejection)", stale.ConsecutiveRejections())
	}
}
