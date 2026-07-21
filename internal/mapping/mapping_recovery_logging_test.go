package mapping

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cplieger/slogx/capture"
)

// TestLoader_refreshCache_notModifiedLogsEndedRejectionStreak pins the 304
// recovery observability contract: a 304 that ends a persisted
// acceptance-guard rejection streak logs the recovery message carrying the
// ended_rejection_streak attribute, so the operator's signal that a
// persistent rejection streak healed cannot be silently removed.
func TestLoader_refreshCache_notModifiedLogsEndedRejectionStreak(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()
	previous := &Cache{
		FetchedAt:         time.Now().Add(-2 * time.Hour),
		ETag:              "v1",
		Records:           []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
		RejectedRefreshes: 3,
	}
	logger, logs := capture.New()
	loader := NewLoader(server.Client(), server.URL, "", time.Hour, logger)
	if _, err := loader.refreshCache(context.Background(), previous); err != nil {
		t.Fatalf("refreshCache error: %v", err)
	}
	if logs.CountExact("mapping: rejection streak ended by 304 revalidation") != 1 {
		t.Fatalf("refreshCache logs = %v, want one 304 rejection-streak recovery message", logs.Messages())
	}
	if !logs.HasAttr("", "ended_rejection_streak", "3") {
		t.Errorf("refreshCache logs = %v, want ended_rejection_streak=3", logs.Messages())
	}
	if !logs.HasAttr("", "records", "1") {
		t.Errorf("refreshCache logs = %v, want records=1", logs.Messages())
	}
}

// TestLoader_refreshCache_acceptedRefreshLogsEndedRejectionStreak pins the
// accepted-200 recovery observability contract: an accepted refresh that ends
// a persisted rejection streak carries the ended_rejection_streak attribute
// on the refreshed message, mirroring the 304 recovery signal.
func TestLoader_refreshCache_acceptedRefreshLogsEndedRejectionStreak(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"anilist_id":2,"type":"tv","tvdb_id":200}]`))
	}))
	defer server.Close()
	previous := &Cache{
		FetchedAt:         time.Now().Add(-2 * time.Hour),
		Records:           []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
		RejectedRefreshes: 3,
	}
	logger, logs := capture.New()
	loader := NewLoader(server.Client(), server.URL, "", time.Hour, logger)
	if _, err := loader.refreshCache(context.Background(), previous); err != nil {
		t.Fatalf("refreshCache error: %v", err)
	}
	if logs.CountExact("mapping: refreshed") != 1 {
		t.Fatalf("refreshCache logs = %v, want one refreshed message", logs.Messages())
	}
	if !logs.HasAttr("", "ended_rejection_streak", "3") {
		t.Errorf("refreshCache logs = %v, want ended_rejection_streak=3", logs.Messages())
	}
	if !logs.HasAttr("", "records", "1") {
		t.Errorf("refreshCache logs = %v, want records=1", logs.Messages())
	}
}
