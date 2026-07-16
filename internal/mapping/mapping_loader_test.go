package mapping

import (
	"context"
	"errors"
	"fmt"
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
		Records:   []Record{{AniListID: 1, Type: "TV"}},
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
		Records:   []Record{{AniListID: 1, Type: "TV"}},
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

func TestLoader_refreshCache_emptyRefreshKeepsStale(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer ts.Close()

	prev := &Cache{
		FetchedAt: time.Now().Add(-2 * time.Hour),
		Records:   []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
	}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
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

// TestLoader_refreshCache_noArrIdentifierKeepsStale covers the acceptance guard:
// a refresh whose records carry only anilist_id/type (a wholesale upstream loss
// of the arr-ID fields, which the tolerant decoders zero rather than reject)
// must be treated like the zero-record branch and retain the usable stale map.
func TestLoader_refreshCache_noArrIdentifierKeepsStale(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"anilist_id":1,"type":"tv"},{"anilist_id":2,"type":"movie"}]`))
	}))
	defer ts.Close()

	prev := &Cache{
		FetchedAt: time.Now().Add(-2 * time.Hour),
		Records:   []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
	}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	next, err := l.refreshCache(context.Background(), prev)
	if err == nil {
		t.Fatal("refresh with no arr identifiers returned nil error, want degraded error")
	}
	if len(next.Records) != 1 || next.Records[0].AniListID != 1 {
		t.Fatalf("no-arr-id refresh records = %+v, want stale record id 1", next.Records)
	}
	if next.Records[0].TvdbID != 100 {
		t.Errorf("no-arr-id refresh stale TvdbID = %d, want 100", next.Records[0].TvdbID)
	}
	if next.RejectedRefreshes != 1 {
		t.Errorf("no-arr-id refresh RejectedRefreshes = %d, want 1 (the validation floor is an acceptance-guard rejection)", next.RejectedRefreshes)
	}
}

// TestLoader_refreshCache_noTypeKeepsStale covers the type-coverage floor: a
// refresh whose records kept their arr ids but wholesale lost the type field
// (an upstream shape change flexString tolerantly zeroes per record) would
// mis-route every MOVIE record to Sonarr while passing the arr-identifier
// floor, so it must be rejected in favour of the usable stale map.
func TestLoader_refreshCache_noTypeKeepsStale(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"anilist_id":1,"type":1,"tvdb_id":100},{"anilist_id":2,"type":2,"tvdb_id":200}]`))
	}))
	defer ts.Close()

	prev := &Cache{
		FetchedAt: time.Now().Add(-2 * time.Hour),
		Records:   []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
	}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	next, err := l.refreshCache(context.Background(), prev)
	if err == nil {
		t.Fatal("refresh with no typed records returned nil error, want degraded error")
	}
	if len(next.Records) != 1 || next.Records[0].AniListID != 1 {
		t.Fatalf("no-type refresh records = %+v, want stale record id 1", next.Records)
	}
	if next.Records[0].Type != "TV" {
		t.Errorf("no-type refresh stale Type = %q, want %q", next.Records[0].Type, "TV")
	}
	if next.RejectedRefreshes != 1 {
		t.Errorf("no-type refresh RejectedRefreshes = %d, want 1 (the type floor is an acceptance-guard rejection)", next.RejectedRefreshes)
	}
}

// TestLoader_refreshCache_lowArrIdentifierCoverageKeepsStale covers the
// coverage floor: a refresh where only 1 of 200+ records retains an arr
// identifier is a wholesale degradation (below the 1% floor) and must keep the
// usable stale map rather than accepting the near-useless record set.
func TestLoader_refreshCache_lowArrIdentifierCoverageKeepsStale(t *testing.T) {
	var b strings.Builder
	b.WriteString(`[{"anilist_id":1,"type":"tv","tvdb_id":100}`)
	for i := 2; i <= 250; i++ {
		fmt.Fprintf(&b, `,{"anilist_id":%d,"type":"tv"}`, i)
	}
	b.WriteByte(']')
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(b.String()))
	}))
	defer ts.Close()

	prev := &Cache{
		FetchedAt: time.Now().Add(-2 * time.Hour),
		Records:   []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
	}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	next, err := l.refreshCache(context.Background(), prev)
	if err == nil {
		t.Fatal("refresh with 1/250 arr-identifier coverage returned nil error, want degraded error")
	}
	if len(next.Records) != 1 || next.Records[0].AniListID != 1 {
		t.Fatalf("low-coverage refresh records = %+v, want stale record id 1", next.Records)
	}
	if next.Records[0].TvdbID != 100 {
		t.Errorf("low-coverage refresh stale TvdbID = %d, want 100", next.Records[0].TvdbID)
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
	var sawValidators bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
			sawValidators = true
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

// TestLoader_refreshCache_acceptsArrIdentifierCoverageFloor pins the accepting
// side of the arr-identifier coverage guard: a first boot whose body carries
// exactly max(1, len(records)/100) records with an arr identifier (1 of 100)
// must be accepted, not rejected with the no-cache error.
func TestLoader_refreshCache_acceptsArrIdentifierCoverageFloor(t *testing.T) {
	var bodyBuilder strings.Builder
	bodyBuilder.WriteString(`[{"anilist_id":1,"type":"tv","tvdb_id":100}`)
	for i := 2; i <= 100; i++ {
		fmt.Fprintf(&bodyBuilder, `,{"anilist_id":%d,"type":"tv"}`, i)
	}
	bodyBuilder.WriteByte(']')
	body := bodyBuilder.String()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()

	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	next, err := l.refreshCache(context.Background(), &Cache{})
	if err != nil {
		t.Fatalf("refresh with exactly 1/100 arr identifiers returned error: %v", err)
	}
	if len(next.Records) != 100 {
		t.Errorf("refresh with exactly 1/100 arr identifiers kept %d records, want 100", len(next.Records))
	}
}

// TestLoader_refreshCache_coverageFloorCeiling pins the ceiling arithmetic of
// the arr-identifier coverage minimum: for 199 records the documented 1% floor
// is 2 (ceiling), so 1/199 must be rejected while 2/199 is accepted — floor
// division would wrongly admit 1/199.
func TestLoader_refreshCache_coverageFloorCeiling(t *testing.T) {
	tests := []struct {
		name       string
		covered    int
		total      int
		wantAccept bool
	}{
		{name: "1 of 199 rejected", covered: 1, total: 199, wantAccept: false},
		{name: "2 of 199 accepted", covered: 2, total: 199, wantAccept: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			b.WriteByte('[')
			for i := 1; i <= tc.total; i++ {
				if i > 1 {
					b.WriteByte(',')
				}
				if i <= tc.covered {
					fmt.Fprintf(&b, `{"anilist_id":%d,"type":"tv","tvdb_id":%d}`, i, i)
				} else {
					fmt.Fprintf(&b, `{"anilist_id":%d,"type":"tv"}`, i)
				}
			}
			b.WriteByte(']')
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(b.String()))
			}))
			defer ts.Close()

			l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
			next, err := l.refreshCache(context.Background(), &Cache{})
			if tc.wantAccept {
				if err != nil {
					t.Fatalf("refresh with %d/%d arr identifiers returned error: %v", tc.covered, tc.total, err)
				}
				if len(next.Records) != tc.total {
					t.Errorf("refresh with %d/%d arr identifiers kept %d records, want %d", tc.covered, tc.total, len(next.Records), tc.total)
				}
				return
			}
			if err == nil {
				t.Fatalf("refresh with %d/%d arr identifiers returned nil error, want below-minimum rejection", tc.covered, tc.total)
			}
			if len(next.Records) != 0 {
				t.Errorf("rejected refresh with no prior cache produced %d records, want 0", len(next.Records))
			}
		})
	}
}

// TestLoader_refreshCache_truncatedRefreshKeepsStale covers the below-half-size
// acceptance guard: a syntactically valid refresh that shrinks the map to less
// than half the previous record count (here 1 valid mapped record replacing 4)
// must degrade to the stale cache with an error, not replace it.
func TestLoader_refreshCache_truncatedRefreshKeepsStale(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"anilist_id":9,"type":"tv","tvdb_id":900}]`))
	}))
	defer ts.Close()

	prevRecords := []Record{
		{AniListID: 1, Type: "TV", TvdbID: 100},
		{AniListID: 2, Type: "TV", TvdbID: 200},
		{AniListID: 3, Type: "TV", TvdbID: 300},
		{AniListID: 4, Type: "TV", TvdbID: 400},
	}
	prev := &Cache{
		FetchedAt: time.Now().Add(-2 * time.Hour),
		Records:   prevRecords,
	}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	next, err := l.refreshCache(context.Background(), prev)
	if err == nil {
		t.Fatal("truncated refresh (1 record replacing 4) returned nil error, want degraded error")
	}
	if len(next.Records) != len(prevRecords) {
		t.Fatalf("truncated refresh kept %d records, want the %d stale records unchanged", len(next.Records), len(prevRecords))
	}
	for i, want := range prevRecords {
		got := next.Records[i]
		if got.AniListID != want.AniListID || got.TvdbID != want.TvdbID || got.Type != want.Type {
			t.Errorf("truncated refresh record[%d] = %+v, want unchanged %+v", i, got, want)
		}
	}
}

// TestLoader_refreshCache_duplicateIDCollapseKeepsStale pins that cache
// acceptance measures the effective AniList-keyed dataset, not the transport
// row count: a 200 whose mapped rows all repeat one AniList ID collapses to a
// single effective record, which the below-half-size guard must reject against
// a 4-record stale cache instead of persisting a refresh that indexes to
// length one.
func TestLoader_refreshCache_duplicateIDCollapseKeepsStale(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[` +
			`{"anilist_id":9,"type":"tv","tvdb_id":900},` +
			`{"anilist_id":9,"type":"tv","tvdb_id":901},` +
			`{"anilist_id":9,"type":"tv","tvdb_id":902},` +
			`{"anilist_id":9,"type":"tv","tvdb_id":903}]`))
	}))
	defer ts.Close()

	prevRecords := []Record{
		{AniListID: 1, Type: "TV", TvdbID: 100},
		{AniListID: 2, Type: "TV", TvdbID: 200},
		{AniListID: 3, Type: "TV", TvdbID: 300},
		{AniListID: 4, Type: "TV", TvdbID: 400},
	}
	prev := &Cache{FetchedAt: time.Now().Add(-2 * time.Hour), Records: prevRecords}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	next, err := l.refreshCache(context.Background(), prev)
	var stale *StaleMapError
	if !errors.As(err, &stale) {
		t.Fatalf("duplicate-collapse refresh error = %v, want a *StaleMapError", err)
	}
	if len(next.Records) != len(prevRecords) {
		t.Fatalf("duplicate-collapse refresh kept %d records, want the %d stale records unchanged", len(next.Records), len(prevRecords))
	}
	for i, want := range prevRecords {
		got := next.Records[i]
		if got.AniListID != want.AniListID || got.TvdbID != want.TvdbID {
			t.Errorf("duplicate-collapse refresh record[%d] = %+v, want unchanged %+v", i, got, want)
		}
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
	for i := 1; i <= RejectionEscalationThreshold; i++ {
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
