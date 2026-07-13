package mapping

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func nopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func freshCache() *Cache {
	return &Cache{
		FetchedAt: time.Now(),
		Records:   []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
	}
}

func TestLoader_refreshCache_reusesFreshCache(t *testing.T) {
	l := NewLoader(nil, "http://unused.invalid", "", time.Hour, nopLogger())
	next, err := l.refreshCache(context.Background(), freshCache())
	if err != nil {
		t.Fatalf("refreshCache error: %v", err)
	}
	if len(next.Records) != 1 {
		t.Errorf("fresh reuse kept %d records, want 1 (no fetch)", len(next.Records))
	}
}

func TestLoader_refreshCache_refreshesOn200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", "v-new")
		_, _ = w.Write([]byte(`[{"anilist_id":42,"type":"tv"}]`))
	}))
	defer ts.Close()
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, nopLogger())
	next, err := l.refreshCache(context.Background(), &Cache{})
	if err != nil {
		t.Fatalf("refreshCache error: %v", err)
	}
	if len(next.Records) != 1 || next.Records[0].AniListID != 42 {
		t.Errorf("refresh records = %+v, want one record id 42", next.Records)
	}
	if next.ETag != "v-new" {
		t.Errorf("refresh ETag = %q, want v-new", next.ETag)
	}
}

func TestLoader_refreshCache_notModifiedBumpsTimestamp(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == "v1" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		t.Errorf("expected If-None-Match v1, got %q", r.Header.Get("If-None-Match"))
	}))
	defer ts.Close()
	prev := &Cache{
		FetchedAt: time.Now().Add(-2 * time.Hour),
		ETag:      "v1",
		Records:   []Record{{AniListID: 1, Type: "TV"}},
	}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, nopLogger())
	next, err := l.refreshCache(context.Background(), prev)
	if err != nil {
		t.Fatalf("refreshCache error: %v", err)
	}
	if len(next.Records) != 1 {
		t.Errorf("304 lost records: got %d, want 1", len(next.Records))
	}
	if !next.FetchedAt.After(prev.FetchedAt) {
		t.Error("304 did not bump FetchedAt")
	}
}

func TestLoader_refreshCache_parseFailKeepsStale(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{ not-an-array`))
	}))
	defer ts.Close()
	prev := &Cache{
		FetchedAt: time.Now().Add(-2 * time.Hour),
		Records:   []Record{{AniListID: 1, Type: "TV"}},
	}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, nopLogger())
	next, err := l.refreshCache(context.Background(), prev)
	if err == nil {
		t.Fatal("parse failure returned nil error, want degraded error")
	}
	if len(next.Records) != 1 {
		t.Errorf("parse failure lost stale records: got %d, want 1", len(next.Records))
	}
}

func TestLoader_Load_overrideWinsOverFribb(t *testing.T) {
	dir := t.TempDir()
	overrides := filepath.Join(dir, "overrides.json")
	if err := os.WriteFile(overrides, []byte(`[{"anilist_id":1,"type":"movie","tmdb_movies":[42]}]`), 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	l := NewLoader(nil, "http://unused.invalid", overrides, time.Hour, nopLogger())
	_, idx, err := l.Load(context.Background(), freshCache())
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	rec, ok := idx.Lookup(1)
	if !ok {
		t.Fatal("Lookup(1) not found after override")
	}
	if rec.Type != "MOVIE" || len(rec.TmdbMovies) != 1 || rec.TmdbMovies[0] != 42 {
		t.Errorf("override not applied: got %+v, want Type MOVIE / TmdbMovies [42]", rec)
	}
}

func TestLoader_Load_missingAndMalformedOverridesIgnored(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.json")
	l := NewLoader(nil, "http://unused.invalid", missing, time.Hour, nopLogger())
	_, idx, err := l.Load(context.Background(), freshCache())
	if err != nil {
		t.Fatalf("Load with missing overrides error: %v", err)
	}
	if rec, ok := idx.Lookup(1); !ok || rec.Type != "TV" {
		t.Errorf("missing overrides changed the Fribb record: %+v ok=%v", rec, ok)
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{ not valid`), 0o644); err != nil {
		t.Fatalf("write bad overrides: %v", err)
	}
	l2 := NewLoader(nil, "http://unused.invalid", bad, time.Hour, nopLogger())
	if _, _, err := l2.Load(context.Background(), freshCache()); err != nil {
		t.Fatalf("Load with malformed overrides returned error, want ignored: %v", err)
	}
}

func TestLoader_refreshCache_emptyRefreshKeepsStale(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer ts.Close()

	prev := &Cache{
		FetchedAt: time.Now().Add(-2 * time.Hour),
		Records:   []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
	}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, nopLogger())
	next, err := l.refreshCache(context.Background(), prev)
	if err == nil {
		t.Fatal("empty refresh returned nil error, want degraded error")
	}
	if len(next.Records) != 1 || next.Records[0].AniListID != 1 {
		t.Fatalf("empty refresh records = %+v, want stale record id 1", next.Records)
	}
	if next.Records[0].TvdbID != 100 {
		t.Errorf("empty refresh stale TvdbID = %d, want 100", next.Records[0].TvdbID)
	}
}

func TestLoader_refreshCache_httpErrorKeepsStale(t *testing.T) {
	const lastModified = "Mon, 02 Jan 2006 15:04:05 GMT"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("If-None-Match"); got != "v1" {
			t.Errorf("If-None-Match = %q, want v1", got)
		}
		if got := r.Header.Get("If-Modified-Since"); got != lastModified {
			t.Errorf("If-Modified-Since = %q, want %q", got, lastModified)
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer ts.Close()

	prev := &Cache{
		FetchedAt:    time.Now().Add(-2 * time.Hour),
		ETag:         "v1",
		LastModified: lastModified,
		Records:      []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
	}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, nopLogger())
	next, err := l.refreshCache(context.Background(), prev)
	if err == nil {
		t.Fatal("HTTP error refresh returned nil error, want degraded error")
	}
	if len(next.Records) != 1 || next.Records[0].AniListID != 1 {
		t.Fatalf("HTTP error refresh records = %+v, want stale record id 1", next.Records)
	}
	if next.ETag != "v1" || next.LastModified != lastModified {
		t.Errorf("HTTP error refresh validators = ETag %q LastModified %q, want stale validators", next.ETag, next.LastModified)
	}
}

// TestLoader_refreshCache_notModifiedEmptyCacheErrors covers the 304/empty-cache
// guard: a record-less cache must suppress the conditional-GET validators (so the
// server returns a full 200) and, if a 304 arrives anyway, refreshCache must error
// rather than reuse zero records.
func TestLoader_refreshCache_notModifiedEmptyCacheErrors(t *testing.T) {
	var sawValidators bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
			sawValidators = true
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer ts.Close()

	prev := &Cache{ETag: "v1", LastModified: "Mon, 02 Jan 2006 15:04:05 GMT"}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, nopLogger())
	next, err := l.refreshCache(context.Background(), prev)
	if err == nil {
		t.Fatal("304 with a record-less cache returned nil error, want a no-cache-available error")
	}
	if len(next.Records) != 0 {
		t.Errorf("304 with empty cache produced %d records, want 0 (must not reuse zero records)", len(next.Records))
	}
	if sawValidators {
		t.Error("conditional GET sent validators despite a record-less cache; they must be suppressed so the server returns a full 200")
	}
}

// TestLoader_refreshCache_noCacheAvailableErrors covers the three first-boot
// degradation branches (empty prev cache): a fetch failure, a parse failure, and
// a zero-record refresh must each return a no-cache-available error rather than
// falling through to a nil-error success.
func TestLoader_refreshCache_noCacheAvailableErrors(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"parse fail", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{ not-an-array`))
		}},
		{"zero records", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`[]`))
		}},
		{"fetch fail", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusNotFound)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(tc.handler)
			defer ts.Close()
			l := NewLoader(ts.Client(), ts.URL, "", time.Hour, nopLogger())
			next, err := l.refreshCache(context.Background(), &Cache{})
			if err == nil {
				t.Fatalf("%s with no prior cache returned nil error, want a degraded no-cache-available error", tc.name)
			}
			if len(next.Records) != 0 {
				t.Errorf("%s with no prior cache produced %d records, want 0", tc.name, len(next.Records))
			}
		})
	}
}
