package mapping

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func freshCache() *Cache {
	return &Cache{
		FetchedAt: time.Now(),
		Records:   []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
	}
}

func TestLoader_refreshCache_reusesFreshCache(t *testing.T) {
	l := NewLoader(nil, "http://unused.invalid", "", time.Hour, discardLogger())
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
		_, _ = w.Write([]byte(`[{"anilist_id":42,"type":"tv","tvdb_id":100}]`))
	}))
	defer ts.Close()
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
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
		Records:   []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
	}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
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
		Records:   []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
	}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	next, err := l.refreshCache(context.Background(), prev)
	if err == nil {
		t.Fatal("parse failure returned nil error, want degraded error")
	}
	if len(next.Records) != 1 {
		t.Errorf("parse failure lost stale records: got %d, want 1", len(next.Records))
	}
}

// TestLoader_Load_nilCacheFetches pins the documented no-persisted-cache
// entry point: Load(ctx, nil) must take the ordinary initial-fetch route (no
// panic on the nil previous cache) and return the fetched records in both the
// persisted Cache and the built Index.
func TestLoader_Load_nilCacheFetches(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"anilist_id":42,"type":"tv","tvdb_id":100}]`))
	}))
	defer ts.Close()
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	next, idx, err := l.Load(context.Background(), nil)
	if err != nil {
		t.Fatalf("Load(nil) error: %v", err)
	}
	if len(next.Records) != 1 || next.Records[0].AniListID != 42 {
		t.Errorf("Load(nil) cache records = %+v, want one record id 42", next.Records)
	}
	if rec, ok := idx.Lookup(42); !ok || rec.TvdbID != 100 {
		t.Errorf("Load(nil) index Lookup(42) = %+v ok=%v, want the fetched record", rec, ok)
	}
}

// TestLoader_Load_canonicalizesPersistedCacheBeforeTheRefreshDecision pins the
// input-boundary canonicalization (h-f25): a persisted cache whose only record
// carries non-canonical ids - a MOVIE with tmdb_movies [0] and a blank imdb id -
// holds NO usable arr identifier, so it is not a usable cache and its validators
// must not be sent. Without the boundary pass the raw record answered
// HasArrIdentifier true (the zero and the blank survive an un-normalized read)
// while the served index answered false, so the refresh sent If-None-Match and a
// 304 revalidated the unusable cache indefinitely instead of obtaining a
// replacement 200. It also pins that the caller's own Cache is never mutated.
func TestLoader_Load_canonicalizesPersistedCacheBeforeTheRefreshDecision(t *testing.T) {
	var sentValidators atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
			sentValidators.Store(true)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte(`[{"anilist_id":42,"type":"tv","tvdb_id":100}]`))
	}))
	defer ts.Close()
	prev := &Cache{
		FetchedAt:    time.Now().Add(-2 * time.Hour),
		ETag:         "v1",
		LastModified: "Wed, 01 Jul 2026 12:00:00 GMT",
		Records:      []Record{{AniListID: 7, Type: "MOVIE", TmdbMovies: []int{0}, IMDbIDs: []string{"  "}}},
	}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	next, idx, err := l.Load(context.Background(), prev)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if sentValidators.Load() {
		t.Error("refresh sent cache validators for an unusable cache; a 304 would freeze the non-canonical records indefinitely")
	}
	if len(next.Records) != 1 || next.Records[0].AniListID != 42 {
		t.Fatalf("returned cache records = %+v, want the full 200 replacement (one record id 42)", next.Records)
	}
	if rec, ok := idx.Lookup(42); !ok || rec.TvdbID != 100 {
		t.Errorf("index Lookup(42) = %+v ok=%v, want the replacement record", rec, ok)
	}
	// The canonicalization runs on a private copy: the caller's State is the
	// persisted cache and must be left exactly as it was handed over.
	if len(prev.Records[0].TmdbMovies) != 1 || prev.Records[0].TmdbMovies[0] != 0 ||
		len(prev.Records[0].IMDbIDs) != 1 || prev.Records[0].Type != "MOVIE" {
		t.Errorf("Load mutated the caller's cache: %+v", prev.Records[0])
	}
}

func TestLoader_Load_overrideWinsOverFribb(t *testing.T) {
	dir := t.TempDir()
	overrides := filepath.Join(dir, "overrides.json")
	if err := os.WriteFile(overrides, []byte(`[{"anilist_id":1,"type":"movie","tmdb_movies":[42]}]`), 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	l := NewLoader(nil, "http://unused.invalid", overrides, time.Hour, discardLogger())
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
	l := NewLoader(nil, "http://unused.invalid", missing, time.Hour, discardLogger())
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
	l2 := NewLoader(nil, "http://unused.invalid", bad, time.Hour, discardLogger())
	if _, _, err := l2.Load(context.Background(), freshCache()); err != nil {
		t.Fatalf("Load with malformed overrides returned error, want ignored: %v", err)
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
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
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
	var sawValidators atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
			sawValidators.Store(true)
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer ts.Close()

	prev := &Cache{ETag: "v1", LastModified: "Mon, 02 Jan 2006 15:04:05 GMT"}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	next, err := l.refreshCache(context.Background(), prev)
	if err == nil {
		t.Fatal("304 with a record-less cache returned nil error, want a no-cache-available error")
	}
	if len(next.Records) != 0 {
		t.Errorf("304 with empty cache produced %d records, want 0 (must not reuse zero records)", len(next.Records))
	}
	if sawValidators.Load() {
		t.Error("conditional GET sent validators despite a record-less cache; they must be suppressed so the server returns a full 200")
	}
}

// TestLoader_refreshCache_noCacheAvailableErrors covers the three first-boot
// degradation branches (empty prev cache): a fetch failure, a parse failure, and
// a zero-record refresh must each return a no-cache-available error rather than
// falling through to a nil-error success.
func TestLoader_refreshCache_noCacheAvailableErrors(t *testing.T) {
	tests := []struct {
		handler http.HandlerFunc
		name    string
	}{
		{name: "parse fail", handler: func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{ not-an-array`))
		}},
		{name: "zero records", handler: func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`[]`))
		}},
		{name: "fetch fail", handler: func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusNotFound)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(tc.handler)
			defer ts.Close()
			l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
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

func TestLoader_Load_degradedRefreshStillAppliesOverrides(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusNotFound)
	}))
	defer ts.Close()

	dir := t.TempDir()
	overrides := filepath.Join(dir, "overrides.json")
	if err := os.WriteFile(overrides, []byte(`[{"anilist_id":1,"type":"movie","tmdb_movies":[42]}]`), 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}

	prev := &Cache{
		FetchedAt: time.Now().Add(-2 * time.Hour),
		ETag:      "v1",
		Records:   []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
	}
	l := NewLoader(ts.Client(), ts.URL, overrides, time.Hour, discardLogger())
	_, idx, err := l.Load(context.Background(), prev)
	if err == nil {
		t.Fatal("Load with a failed refresh returned nil error, want a degraded error")
	}
	rec, ok := idx.Lookup(1)
	if !ok {
		t.Fatal("degraded Load lost the stale record for id 1")
	}
	if rec.Type != "MOVIE" || len(rec.TmdbMovies) != 1 || rec.TmdbMovies[0] != 42 {
		t.Errorf("degraded Load did not overlay overrides: got %+v, want Type MOVIE / TmdbMovies [42]", rec)
	}
}

// TestLoader_Load_noOverridesPathServesFribbUnmodified pins applyOverrides'
// empty-path early return: a loader constructed with no overrides file
// configured serves the Fribb map untouched (no read attempt, no overlay).
func TestLoader_Load_noOverridesPathServesFribbUnmodified(t *testing.T) {
	l := NewLoader(nil, "http://unused.invalid", "", time.Hour, discardLogger())
	_, idx, err := l.Load(context.Background(), freshCache())
	if err != nil {
		t.Fatalf("Load with no overrides path error: %v", err)
	}
	if idx.Len() != 1 {
		t.Fatalf("Load with no overrides path indexed %d records, want 1", idx.Len())
	}
	if rec, ok := idx.Lookup(1); !ok || rec.Type != "TV" || rec.TvdbID != 100 {
		t.Errorf("Load with no overrides path record = %+v ok=%v, want unmodified TV/100", rec, ok)
	}
}

// TestLoader_refreshCache_futureFetchedAtForcesFetch pins the clock-skew guard
// in the fresh-reuse condition (age >= 0): a cache stamped in the future
// (clock skew or a corrupt state file) is never treated as fresh, so the
// loader revalidates against upstream instead of trusting the bad timestamp
// until it drifts back into range.
func TestLoader_refreshCache_futureFetchedAtForcesFetch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"anilist_id":42,"type":"tv","tvdb_id":100}]`))
	}))
	defer ts.Close()
	prev := &Cache{
		FetchedAt: time.Now().Add(2 * time.Hour), // future: skew or a corrupt state file
		Records:   []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
	}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	next, err := l.refreshCache(context.Background(), prev)
	if err != nil {
		t.Fatalf("refreshCache with future FetchedAt error: %v", err)
	}
	if len(next.Records) != 1 || next.Records[0].AniListID != 42 {
		t.Fatalf("future-FetchedAt cache was reused as fresh: records = %+v, want fetched record id 42", next.Records)
	}
}

// TestLoader_refreshCache_futureFetchedAtFailedFetchClampsStaleAge pins the
// stale-age clamp on the degradation telemetry: when a future FetchedAt
// (clock skew or a corrupt state file) forces revalidation and that fetch
// fails, the StaleMapError must report a non-negative age in both LogAttrs
// and the error text instead of a misleading "fetched -2h0m0s ago".
func TestLoader_refreshCache_futureFetchedAtFailedFetchClampsStaleAge(t *testing.T) {
	prev := &Cache{
		FetchedAt: time.Now().Add(2 * time.Hour), // future: skew or a corrupt state file
		Records:   []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
	}
	l := NewLoader(&http.Client{Transport: errTransport{}}, "http://unused.invalid", "", time.Hour, discardLogger())
	next, err := l.refreshCache(context.Background(), prev)
	if len(next.Records) != 1 {
		t.Fatalf("future-FetchedAt failed refresh records = %+v, want stale record kept", next.Records)
	}
	stale, ok := errors.AsType[*StaleMapError](err)
	if !ok {
		t.Fatalf("future-FetchedAt failed refresh error = %v, want *StaleMapError", err)
	}
	if strings.Contains(stale.Error(), "fetched -") {
		t.Errorf("StaleMapError text = %q, want non-negative age", stale.Error())
	}
	attrs := stale.LogAttrs()
	foundAge := false
	for i := 0; i+1 < len(attrs); i += 2 {
		if attrs[i] == "stale_age_seconds" {
			foundAge = true
			if secs, isFloat := attrs[i+1].(float64); !isFloat || secs < 0 {
				t.Errorf("LogAttrs stale_age_seconds = %v, want non-negative float64", attrs[i+1])
			}
		}
	}
	if !foundAge {
		t.Error("LogAttrs carries no stale_age_seconds attribute; the clamp assertion never ran")
	}
}

// TestLoader_refreshCache_zeroRefreshAlwaysRevalidates pins the deployed
// configuration's contract (the app wires DefaultRefresh = 0): a zero
// refresh window disables the fresh-reuse fast path entirely, so even a
// just-fetched cache revalidates against upstream every cycle (an unchanged
// upstream is a cheap 304) instead of being reused until the timestamp ages.
// Guards against the fleet's opposite convention leaking in (scheduler treats
// 0 as "off"; here 0 must mean "always revalidate", never "never refresh").
func TestLoader_refreshCache_zeroRefreshAlwaysRevalidates(t *testing.T) {
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNotModified)
	}))
	defer ts.Close()
	prev := &Cache{
		FetchedAt: time.Now(), // just fetched: any positive window would reuse it
		ETag:      "v1",
		Records:   []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
	}
	l := NewLoader(ts.Client(), ts.URL, "", 0, discardLogger())
	next, err := l.refreshCache(context.Background(), prev)
	if err != nil {
		t.Fatalf("zero-refresh revalidation error: %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("zero-refresh loader made %d upstream requests, want 1 (must revalidate every cycle)", got)
	}
	if len(next.Records) != 1 {
		t.Errorf("zero-refresh 304 kept %d records, want 1", len(next.Records))
	}
}

// TestLoader_refreshCache_unusableCacheFetchFailureErrors pins the
// cache-usability gate on the fetch-outage degradation path: a JSON-valid
// state cache whose records index to nothing (records:[{}] — a zero AniList
// ID buildIndex drops) must NOT enter staleOrFail as a StaleMapError, because
// scout.mapUsable trusts the error type alone and would proceed into
// matching against an empty effective map. It must degrade like no cache at
// all (the no-cache error), so the scout preserves findings.
func TestLoader_refreshCache_unusableCacheFetchFailureErrors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusNotFound)
	}))
	defer ts.Close()

	prev := &Cache{
		FetchedAt: time.Now().Add(-2 * time.Hour),
		Records:   []Record{{}}, // non-empty slice, zero effective index
	}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	_, err := l.refreshCache(context.Background(), prev)
	if err == nil {
		t.Fatal("fetch failure over an unusable cache returned nil error, want a no-cache-available error")
	}
	var stale *StaleMapError
	if errors.As(err, &stale) {
		t.Fatalf("fetch failure over an unusable cache returned %v, want the no-cache error (a StaleMapError would make scout compare against an empty map)", err)
	}
}

// TestLoader_refreshCache_unusableCacheSendsNoValidatorsAndErrorsOn304 pins
// the cache-usability gate on the conditional-GET and 304 paths: an unusable
// non-empty cache (all-zero AniList IDs) must suppress the validators (forcing
// a full 200 download) and, if a 304 arrives anyway, must error rather than
// affirm a map that indexes to nothing.
func TestLoader_refreshCache_unusableCacheSendsNoValidatorsAndErrorsOn304(t *testing.T) {
	var sawValidators atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
			sawValidators.Store(true)
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer ts.Close()

	prev := &Cache{
		FetchedAt:    time.Now().Add(-2 * time.Hour),
		ETag:         "v1",
		LastModified: "Mon, 02 Jan 2006 15:04:05 GMT",
		Records:      []Record{{}}, // non-empty slice, zero effective index
	}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	_, err := l.refreshCache(context.Background(), prev)
	if err == nil {
		t.Fatal("304 over an unusable cache returned nil error, want an error instead of reusing an empty effective map")
	}
	if sawValidators.Load() {
		t.Error("conditional GET sent validators despite an unusable cache; they must be suppressed so the server returns a full 200")
	}
}

// routingFloorPrevCache returns a previously accepted cache with both routing
// populations above the 1% floor: two MOVIE records (TMDB-movie ids) and two
// series records (TVDB ids). Shared by the routing-distribution floor tests.
func routingFloorPrevCache() *Cache {
	return &Cache{
		FetchedAt: time.Now().Add(-2 * time.Hour),
		Records: []Record{
			{AniListID: 1, Type: "MOVIE", TmdbMovies: []int{42}},
			{AniListID: 2, Type: "MOVIE", TmdbMovies: []int{43}},
			{AniListID: 3, Type: "TV", TvdbID: 300},
			{AniListID: 4, Type: "TV", TvdbID: 400},
		},
	}
}

// TestLoader_refreshCache_freshUnusableCacheStillFetches pins the
// cache-usability gate on the fresh-reuse fast path - the first of the four
// cache-state gates cacheUsable documents, and the only one previously
// unpinned: a cache inside the refresh window whose records index to nothing
// (records:[{}] - a zero AniList ID buildIndex drops) must NOT be reused as
// fresh, because serving it would idle a whole refresh window on an empty
// effective map; the loader must fall through to the fetch and accept the
// upstream body.
func TestLoader_refreshCache_freshUnusableCacheStillFetches(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"anilist_id":42,"type":"tv","tvdb_id":100}]`))
	}))
	defer ts.Close()
	prev := &Cache{
		FetchedAt: time.Now(),   // inside the refresh window: freshness alone would reuse it
		Records:   []Record{{}}, // non-empty slice, zero effective index
	}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	next, err := l.refreshCache(context.Background(), prev)
	if err != nil {
		t.Fatalf("refreshCache with a fresh-but-unusable cache error: %v", err)
	}
	if len(next.Records) != 1 || next.Records[0].AniListID != 42 {
		t.Fatalf("fresh-but-unusable cache was reused as fresh: records = %+v, want fetched record id 42", next.Records)
	}
}

// TestLoader_refreshCache_zeroIDIdentifiersDoNotMakeCacheUsable pins the
// population deduplicateRecords hands to cacheUsable: a zero-AniList-ID record
// is dropped by buildIndex, so its arr identifiers must not count toward the
// coverage floor. A cache whose only keyed record carries no arr id, padded by
// a zero-key record with a TVDB id, is NOT usable — the fresh-cache fast path
// must fall through to the fetch instead of serving an effective index whose
// only keyed record cannot resolve.
func TestLoader_refreshCache_zeroIDIdentifiersDoNotMakeCacheUsable(t *testing.T) {
	records := []Record{
		{AniListID: 1, Type: "TV"},
		{AniListID: 0, Type: "TV", TvdbID: 100},
	}
	if cacheUsable(records) {
		t.Fatal("cacheUsable = true, want false: the zero-ID record's TVDB id must not count for the dropped record")
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"anilist_id":42,"type":"tv","tvdb_id":100}]`))
	}))
	defer ts.Close()
	prev := &Cache{
		FetchedAt: time.Now(), // inside the refresh window: freshness alone would reuse it
		Records:   records,
	}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	next, err := l.refreshCache(context.Background(), prev)
	if err != nil {
		t.Fatalf("refreshCache with a fresh-but-unusable cache error: %v", err)
	}
	if len(next.Records) != 1 || next.Records[0].AniListID != 42 {
		t.Fatalf("fresh-but-unusable cache was reused as fresh: records = %+v, want fetched record id 42", next.Records)
	}
}

// TestLoader_refreshCache_boundsPersistedValidators pins the validator size
// bound the app's state.json bloat protection rides on: an at-limit validator
// is retained while an over-limit one is dropped, so an upstream-controlled
// header cannot inflate the persisted state. The guard itself lives in
// httpx.DoConditional (capture-side hygiene); this is the consumer-side
// contract pin, with the limit mirroring httpx's documented 1 KiB cap.
func TestLoader_refreshCache_boundsPersistedValidators(t *testing.T) {
	const httpxValidatorCap = 1 << 10
	atLimit := strings.Repeat("v", httpxValidatorCap)
	tests := []struct {
		name      string
		validator string
		want      string
	}{
		{name: "at limit retained", validator: atLimit, want: atLimit},
		{name: "over limit dropped", validator: atLimit + "x", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("ETag", tc.validator)
				_, _ = w.Write([]byte(`[{"anilist_id":42,"type":"tv","tvdb_id":100}]`))
			}))
			defer ts.Close()

			loader := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
			next, err := loader.refreshCache(t.Context(), &Cache{})
			if err != nil {
				t.Fatalf("refreshCache error: %v", err)
			}
			if next.ETag != tc.want {
				t.Errorf("ETag length %d persisted as length %d, want length %d", len(tc.validator), len(next.ETag), len(tc.want))
			}
		})
	}
}

// TestLoader_refreshCache_sanitizesPersistedValidators pins the app's
// self-heal contract for a poisoned validator already persisted in the
// previous Cache (it predates the hygiene, or the state file was tampered
// with): it must never be sent as a request header (httpx.DoConditional's
// replay-side hygiene skips it, so the refresh degrades to an unconditional
// GET instead of failing net/http's request-write validation forever), and a
// successful refresh must return a Cache that no longer carries it.
func TestLoader_refreshCache_sanitizesPersistedValidators(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("If-None-Match"); got != "" {
			t.Errorf("If-None-Match = %q, want the poisoned persisted ETag dropped before the request", got)
		}
		if got := r.Header.Get("If-Modified-Since"); got != "" {
			t.Errorf("If-Modified-Since = %q, want empty (no Last-Modified was cached)", got)
		}
		_, _ = w.Write([]byte(`[{"anilist_id":42,"type":"tv","tvdb_id":100}]`))
	}))
	defer ts.Close()

	prev := &Cache{
		FetchedAt: time.Now().Add(-2 * time.Hour),
		ETag:      "\"et\x01ag\"",
		Records:   []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
	}
	loader := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	next, err := loader.refreshCache(t.Context(), prev)
	if err != nil {
		t.Fatalf("refreshCache error: %v", err)
	}
	if next.ETag != "" {
		t.Errorf("returned ETag = %q, want the poisoned persisted validator gone", next.ETag)
	}
	if len(next.Records) != 1 || next.Records[0].AniListID != 42 {
		t.Errorf("records = %+v, want the refreshed record id 42", next.Records)
	}
}

// TestLoader_refreshCache_304KeepsValidSkipsPoisonedValidator pins the mixed
// case on the 304 path: with one valid and one poisoned persisted validator,
// the valid one still rides the conditional request (a 304 stays possible)
// while the poisoned one is never sent (httpx.DoConditional's replay-side
// hygiene skips it). The 304 return re-persists the poisoned value - it is
// inert (skipped again on every replay) and stays until the next accepted
// 200's pre-sanitized capture replaces it.
func TestLoader_refreshCache_304KeepsValidSkipsPoisonedValidator(t *testing.T) {
	const lastModified = "Wed, 21 Oct 2015 07:28:00 GMT"
	const poisoned = "\"et\x01ag\""
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("If-None-Match"); got != "" {
			t.Errorf("If-None-Match = %q, want the poisoned persisted ETag skipped at replay", got)
		}
		if got := r.Header.Get("If-Modified-Since"); got != lastModified {
			t.Errorf("If-Modified-Since = %q, want the valid persisted validator %q", got, lastModified)
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer ts.Close()

	prev := &Cache{
		FetchedAt:    time.Now().Add(-2 * time.Hour),
		ETag:         poisoned,
		LastModified: lastModified,
		Records:      []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
	}
	loader := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	next, err := loader.refreshCache(t.Context(), prev)
	if err != nil {
		t.Fatalf("refreshCache error: %v", err)
	}
	if next.ETag != poisoned {
		t.Errorf("returned ETag = %q, want the poisoned validator retained inert on the 304 path (replaced only by an accepted 200)", next.ETag)
	}
	if next.LastModified != lastModified {
		t.Errorf("returned LastModified = %q, want the valid validator %q retained", next.LastModified, lastModified)
	}
	if len(next.Records) != 1 || next.Records[0].AniListID != 1 {
		t.Errorf("records = %+v, want the cached record reused on 304", next.Records)
	}
}

// TestLoader_refreshCache_freshLowCoverageCacheStillFetches pins the second
// arm of cacheUsable on the fresh-reuse fast path: a cache inside the refresh
// window whose records index fine but carry no arr identifier (below the 1%
// coverage floor) must NOT be reused as fresh — serving it would idle a whole
// refresh window on a map no lookup can resolve; the loader must fall through
// to the fetch and accept the upstream body.
func TestLoader_refreshCache_freshLowCoverageCacheStillFetches(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"anilist_id":42,"type":"tv","tvdb_id":100}]`))
	}))
	defer ts.Close()
	prev := &Cache{
		FetchedAt: time.Now(),                           // inside the refresh window
		Records:   []Record{{AniListID: 1, Type: "TV"}}, // keyed, but zero arr-identifier coverage
	}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	next, err := l.refreshCache(context.Background(), prev)
	if err != nil {
		t.Fatalf("refreshCache with a fresh-but-unmappable cache error: %v", err)
	}
	if len(next.Records) != 1 || next.Records[0].AniListID != 42 {
		t.Fatalf("fresh-but-unmappable cache was reused as fresh: records = %+v, want fetched record id 42", next.Records)
	}
}
