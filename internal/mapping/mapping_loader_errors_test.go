package mapping

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestNewLoader_nilLoggerDefaults pins the nil-logger constructor branch: a
// nil logger falls back to slog.Default() so the loader's log calls cannot
// panic on the fresh-reuse path (which logs at Debug).
func TestNewLoader_nilLoggerDefaults(t *testing.T) {
	l := NewLoader(nil, "http://unused.invalid", "", time.Hour, nil)
	next, err := l.refreshCache(context.Background(), freshCache())
	if err != nil {
		t.Fatalf("refreshCache with nil logger error: %v", err)
	}
	if len(next.Records) != 1 {
		t.Errorf("nil-logger fresh reuse kept %d records, want 1", len(next.Records))
	}
}

// TestLoader_refreshCache_badURLErrors pins conditionalGet's request-build
// error path (http.NewRequestWithContext): an unparseable URL fails the
// conditional GET, surfacing the no-cache-available
// error on first boot.
func TestLoader_refreshCache_badURLErrors(t *testing.T) {
	l := NewLoader(&http.Client{}, "://not-a-url", "", time.Hour, discardLogger())
	if _, err := l.refreshCache(context.Background(), &Cache{}); err == nil {
		t.Fatal("refreshCache with unparseable URL = nil error, want error")
	}
}

// TestLoader_refreshCache_unexpectedStatusKeepsStale pins httpx.DoConditional's
// unexpected-status classification: a status that is neither 200/304 nor >=400 (a 204)
// surfaces as an error from the conditional GET and maps to the explicit
// "unexpected status" error and the stale map is kept.
func TestLoader_refreshCache_unexpectedStatusKeepsStale(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()
	prev := &Cache{
		FetchedAt: time.Now().Add(-2 * time.Hour),
		Records:   []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
	}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	next, err := l.refreshCache(context.Background(), prev)
	if err == nil {
		t.Fatal("204 refresh returned nil error, want unexpected-status degraded error")
	}
	if len(next.Records) != 1 || next.Records[0].AniListID != 1 {
		t.Fatalf("204 refresh records = %+v, want stale record id 1", next.Records)
	}
}

// TestLoader_refreshCache_overCapBodyKeepsStale pins the fail-closed download
// bound: a 200 whose body exceeds maxMapBytes is rejected (ReadLimitedBody's
// ResponseTooLargeError) and the stale map is kept, rather than a truncated
// body being parsed as the new map.
func TestLoader_refreshCache_overCapBodyKeepsStale(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, maxMapBytes+1))
	}))
	defer ts.Close()
	prev := &Cache{
		FetchedAt: time.Now().Add(-2 * time.Hour),
		Records:   []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
	}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	next, err := l.refreshCache(context.Background(), prev)
	if err == nil {
		t.Fatal("over-cap refresh returned nil error, want degraded error (fail closed)")
	}
	if len(next.Records) != 1 || next.Records[0].AniListID != 1 {
		t.Fatalf("over-cap refresh records = %+v, want stale record id 1", next.Records)
	}
}

// TestLoader_Load_canceledContextSkipsOverrides pins applyOverrides'
// context-cancellation branch: a canceled context skips the overrides read
// silently (no overlay, no warn) while the fresh cache still serves.
func TestLoader_Load_canceledContextSkipsOverrides(t *testing.T) {
	dir := t.TempDir()
	overrides := filepath.Join(dir, "overrides.json")
	if err := os.WriteFile(overrides, []byte(`[{"anilist_id":1,"type":"movie"}]`), 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l := NewLoader(nil, "http://unused.invalid", overrides, time.Hour, discardLogger())
	_, idx, err := l.Load(ctx, freshCache())
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	rec, ok := idx.Lookup(1)
	if !ok {
		t.Fatal("canceled-context Load lost the cached record for id 1")
	}
	if rec.Type != "TV" {
		t.Errorf("canceled-context Load applied the override: Type = %q, want TV", rec.Type)
	}
}

// TestLoader_Load_directoryOverridesIgnored pins the unreadable-overrides warn
// branch through a root-safe injection (a directory at the overrides path, not
// a permission bit): the read error is logged and ignored, and the Fribb
// record survives unmodified.
func TestLoader_Load_directoryOverridesIgnored(t *testing.T) {
	l := NewLoader(nil, "http://unused.invalid", t.TempDir(), time.Hour, discardLogger())
	_, idx, err := l.Load(context.Background(), freshCache())
	if err != nil {
		t.Fatalf("Load with directory overrides error: %v", err)
	}
	if rec, ok := idx.Lookup(1); !ok || rec.Type != "TV" {
		t.Errorf("directory overrides changed the record: %+v ok=%v", rec, ok)
	}
}

// TestLoader_Load_zeroIDOverrideIgnored pins applyOverrides' keying guard: an
// override record with anilist_id 0 is not indexed (it cannot key a SeaDex
// lookup), so no phantom entry appears.
func TestLoader_Load_zeroIDOverrideIgnored(t *testing.T) {
	dir := t.TempDir()
	overrides := filepath.Join(dir, "overrides.json")
	if err := os.WriteFile(overrides, []byte(`[{"anilist_id":0,"type":"movie"}]`), 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	l := NewLoader(nil, "http://unused.invalid", overrides, time.Hour, discardLogger())
	_, idx, err := l.Load(context.Background(), freshCache())
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if idx.Len() != 1 {
		t.Errorf("zero-id override was indexed: Len = %d, want 1", idx.Len())
	}
	if _, ok := idx.Lookup(0); ok {
		t.Error("Lookup(0) found a phantom zero-id override entry")
	}
}
