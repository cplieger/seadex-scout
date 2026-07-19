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

// TestLoader_refreshCache_typeSparsePreviousCacheAcceptsUntypedRefresh pins
// the type floor's relative contract: fribb.go tolerantly decodes an absent
// type as the safe non-movie default, so when the previously accepted cache is
// itself type-sparse (never met the floor), an equally type-sparse but
// otherwise valid refresh is the catalogue's established shape and must be
// accepted — not rejected on an absolute schema requirement the decoder does
// not impose (which would keep the stale map forever and escalate to ERROR).
func TestLoader_refreshCache_typeSparsePreviousCacheAcceptsUntypedRefresh(t *testing.T) {
	const n = 200
	var b strings.Builder
	b.WriteString("[")
	prevRecords := make([]Record, 0, n)
	for i := 1; i <= n; i++ {
		if i > 1 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"anilist_id":%d,"tvdb_id":%d}`, i, i+1000)
		prevRecords = append(prevRecords, Record{AniListID: i, TvdbID: i + 1000})
	}
	b.WriteString("]")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(b.String()))
	}))
	defer ts.Close()

	prev := &Cache{
		FetchedAt:         time.Now().Add(-2 * time.Hour),
		Records:           prevRecords,
		RejectedRefreshes: 3,
	}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	next, err := l.refreshCache(context.Background(), prev)
	if err != nil {
		t.Fatalf("type-sparse refresh over a type-sparse cache returned error %v, want accepted", err)
	}
	if len(next.Records) != n {
		t.Fatalf("accepted refresh records = %d, want %d", len(next.Records), n)
	}
	if next.RejectedRefreshes != 0 {
		t.Errorf("accepted refresh RejectedRefreshes = %d, want 0 (acceptance resets the streak)", next.RejectedRefreshes)
	}
}

// TestLoader_refreshCache_additiveGrowthKeepsTypedFloor pins the type floor's
// loss requirement: a previous cache of 100 records with exactly one typed
// record meets its own 1% floor (minimum 1), and a legitimate additive refresh
// of 101 records that RETAINS that same typed record raises the ceiling-derived
// minimum to 2 without losing any type data. The floor must not fire on growth
// alone — rejecting it would keep the stale map every cycle, advance
// RejectedRefreshes, and escalate to ERROR indefinitely.
func TestLoader_refreshCache_additiveGrowthKeepsTypedFloor(t *testing.T) {
	const prevN = 100
	var b strings.Builder
	b.WriteString(`[{"anilist_id":1,"type":"tv","tvdb_id":1001}`)
	prevRecords := []Record{{AniListID: 1, Type: "TV", TvdbID: 1001}}
	for i := 2; i <= prevN; i++ {
		fmt.Fprintf(&b, `,{"anilist_id":%d,"tvdb_id":%d}`, i, i+1000)
		prevRecords = append(prevRecords, Record{AniListID: i, TvdbID: i + 1000})
	}
	// The candidate retains every previous record (including the one typed
	// record) and adds one valid untyped record: 101 records, 1 typed.
	fmt.Fprintf(&b, `,{"anilist_id":%d,"tvdb_id":%d}`, prevN+1, prevN+1001)
	b.WriteString("]")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(b.String()))
	}))
	defer ts.Close()

	prev := &Cache{
		FetchedAt:         time.Now().Add(-2 * time.Hour),
		Records:           prevRecords,
		RejectedRefreshes: 3,
	}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	next, err := l.refreshCache(context.Background(), prev)
	if err != nil {
		t.Fatalf("additive refresh retaining all typed records returned error %v, want accepted", err)
	}
	if len(next.Records) != prevN+1 {
		t.Fatalf("accepted refresh records = %d, want %d", len(next.Records), prevN+1)
	}
	if next.RejectedRefreshes != 0 {
		t.Errorf("accepted refresh RejectedRefreshes = %d, want 0 (acceptance resets the streak)", next.RejectedRefreshes)
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

// TestLoader_refreshCache_recordCapBreachAdvancesRejectionStreak pins the
// record-cap exception to the "parse failures don't advance the streak" rule:
// an over-cap body is a persistent guard refusal (an over-cap upstream list
// re-downloads and rejects every cycle, never self-healing), so acceptRefresh
// must route it through rejectRefresh — the errors.Is-matchable sentinel
// survives the *StaleMapError wrap, the stale map is kept, and the persisted
// streak advances so ConsecutiveRejections reaches
// RejectionEscalationThreshold (the scout's WARN→ERROR escalation point)
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
		RejectedRefreshes: RejectionEscalationThreshold - 1,
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
	if next.RejectedRefreshes != RejectionEscalationThreshold {
		t.Errorf("cap-breach RejectedRefreshes = %d, want %d (a cap breach advances the streak)", next.RejectedRefreshes, RejectionEscalationThreshold)
	}
	if stale.ConsecutiveRejections() != RejectionEscalationThreshold {
		t.Errorf("cap-breach ConsecutiveRejections = %d, want %d (the scout escalates to ERROR at the threshold)", stale.ConsecutiveRejections(), RejectionEscalationThreshold)
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
	for i := 0; i+1 < len(attrs); i += 2 {
		if attrs[i] == "stale_age_seconds" {
			if secs, isFloat := attrs[i+1].(float64); !isFloat || secs < 0 {
				t.Errorf("LogAttrs stale_age_seconds = %v, want non-negative float64", attrs[i+1])
			}
		}
	}
}

// TestLoader_refreshCache_zeroRefreshAlwaysRevalidates pins the deployed
// configuration's contract (the app wires DefaultMappingRefresh = 0): a zero
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
	var sawValidators bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
			sawValidators = true
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
	if sawValidators {
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

// TestLoader_refreshCache_routingCollapseKeepsStale covers the
// routing-distribution acceptance floor: a fresh body that keeps 100% typed
// coverage but collapses one routing population must be rejected in favour of
// the stale map. Both directions are pinned — every movie type renamed to an
// unrecognized string (FILM: all records route to Sonarr) and every record
// stamped MOVIE (all records route to Radarr) — since either silently sends an
// entire side of the catalogue to the wrong arr while passing the typed and
// arr-identifier floors.
func TestLoader_refreshCache_routingCollapseKeepsStale(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "movie types renamed to FILM",
			body: `[{"anilist_id":1,"type":"film","themoviedb_id":{"movie":[42]}},` +
				`{"anilist_id":2,"type":"film","themoviedb_id":{"movie":[43]}},` +
				`{"anilist_id":3,"type":"tv","tvdb_id":300},` +
				`{"anilist_id":4,"type":"tv","tvdb_id":400}]`,
		},
		{
			name: "every record stamped MOVIE",
			body: `[{"anilist_id":1,"type":"movie","themoviedb_id":{"movie":[42]}},` +
				`{"anilist_id":2,"type":"movie","themoviedb_id":{"movie":[43]}},` +
				`{"anilist_id":3,"type":"movie","themoviedb_id":{"movie":[44]}},` +
				`{"anilist_id":4,"type":"movie","themoviedb_id":{"movie":[45]}}]`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer ts.Close()

			prev := routingFloorPrevCache()
			l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
			next, err := l.refreshCache(context.Background(), prev)
			var stale *StaleMapError
			if !errors.As(err, &stale) {
				t.Fatalf("routing-collapse refresh error = %v, want a *StaleMapError guard rejection", err)
			}
			if len(next.Records) != len(prev.Records) {
				t.Fatalf("routing-collapse refresh kept %d records, want the %d stale records unchanged", len(next.Records), len(prev.Records))
			}
			if next.RejectedRefreshes != 1 {
				t.Errorf("routing-collapse RejectedRefreshes = %d, want 1 (the routing floor is an acceptance-guard rejection)", next.RejectedRefreshes)
			}
		})
	}
}

// TestLoader_refreshCache_additiveUpdateKeepsRoutingFloor pins the accepting
// side of the routing-distribution floor: a normal additive catalogue update
// that grows both routing populations must be accepted (and reset the
// rejection streak), not rejected on growth alone.
func TestLoader_refreshCache_additiveUpdateKeepsRoutingFloor(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"anilist_id":1,"type":"movie","themoviedb_id":{"movie":[42]}},` +
			`{"anilist_id":2,"type":"movie","themoviedb_id":{"movie":[43]}},` +
			`{"anilist_id":3,"type":"tv","tvdb_id":300},` +
			`{"anilist_id":4,"type":"tv","tvdb_id":400},` +
			`{"anilist_id":5,"type":"movie","themoviedb_id":{"movie":[44]}},` +
			`{"anilist_id":6,"type":"tv","tvdb_id":600}]`))
	}))
	defer ts.Close()

	prev := routingFloorPrevCache()
	prev.RejectedRefreshes = 3
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	next, err := l.refreshCache(context.Background(), prev)
	if err != nil {
		t.Fatalf("additive refresh growing both routing sides returned error %v, want accepted", err)
	}
	if len(next.Records) != 6 {
		t.Fatalf("accepted refresh records = %d, want 6", len(next.Records))
	}
	if next.RejectedRefreshes != 0 {
		t.Errorf("accepted refresh RejectedRefreshes = %d, want 0 (acceptance resets the streak)", next.RejectedRefreshes)
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

// TestLoader_refreshCache_boundsPersistedValidators pins the maxValidatorBytes
// guard: an at-limit validator is retained while an over-limit one is dropped,
// so an upstream-controlled header cannot inflate the persisted state.json.
func TestLoader_refreshCache_boundsPersistedValidators(t *testing.T) {
	atLimit := strings.Repeat("v", maxValidatorBytes)
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

// TestLoader_boundedValidator_dropsInvalidHeaderBytes pins the content
// dimension of the persisted-validator guard (the sibling of the size bound
// above): a validator carrying bytes illegal in an HTTP header field value
// (RFC 9110 field-value grammar - control characters other than tab, or DEL)
// is dropped, so a hostile upstream ETag with a CR/LF can never be persisted
// into state.json and replayed as a request header net/http rejects at
// write time, permanently poisoning conditional revalidation. Exercised
// directly on boundedValidator because Go's server-side header writer
// rewrites CR/LF to spaces, so the httptest harness above cannot deliver
// these bytes over the wire.
func TestLoader_boundedValidator_dropsInvalidHeaderBytes(t *testing.T) {
	l := NewLoader(nil, "", "", 0, discardLogger())
	tests := []struct {
		name      string
		validator string
		want      string
	}{
		{name: "crlf dropped", validator: "\"etag\r\nX-Injected: 1\"", want: ""},
		{name: "control byte dropped", validator: "\"et\x01ag\"", want: ""},
		{name: "DEL dropped", validator: "\"et\x7fag\"", want: ""},
		{name: "tab retained", validator: "\"et\tag\"", want: "\"et\tag\""},
		{name: "printable ascii retained", validator: "W/\"abc-123\"", want: "W/\"abc-123\""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := l.boundedValidator("etag", tc.validator); got != tc.want {
				t.Errorf("boundedValidator(%q) = %q, want %q", tc.validator, got, tc.want)
			}
		})
	}
}

// TestLoader_refreshCache_firstBootKeylessBodyRejected pins the AniList-key
// coverage floor's denominator: on first boot (no previous cache, so the
// relative shrink guard cannot fire) a valid 200-element body where 199
// records lack an anilist_id and only one is fully mapped must be rejected as
// wholesale key loss — not reinterpreted as a healthy 1/1 map after the
// parser drops the keyless rows. The floor validates the survivor count
// against the top-level source-element count (parseFribbForRefresh), which
// destructive filtering cannot shrink.
func TestLoader_refreshCache_firstBootKeylessBodyRejected(t *testing.T) {
	var b strings.Builder
	b.WriteString(`[{"anilist_id":1,"type":"tv","tvdb_id":100}`)
	for i := 2; i <= 200; i++ {
		b.WriteString(`,{"type":"tv","tvdb_id":100}`)
	}
	b.WriteByte(']')
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(b.String()))
	}))
	defer ts.Close()

	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	next, err := l.refreshCache(context.Background(), &Cache{})
	if err == nil {
		t.Fatal("first-boot refresh with 1/200 AniList-keyed records returned nil error, want below-minimum rejection")
	}
	if len(next.Records) != 0 {
		t.Errorf("rejected first-boot refresh produced %d records, want 0", len(next.Records))
	}
}

// TestLoader_refreshCache_firstBootDuplicateAmplificationRejected pins that
// the AniList-key floor's denominator survives deduplication too: a
// first-boot body of 200 rows all repeating one valid AniList ID collapses to
// a single effective record, which must be rejected against the original
// 200-element source count instead of passing every floor as a 1/1 map.
func TestLoader_refreshCache_firstBootDuplicateAmplificationRejected(t *testing.T) {
	var b strings.Builder
	b.WriteByte('[')
	for i := 1; i <= 200; i++ {
		if i > 1 {
			b.WriteByte(',')
		}
		b.WriteString(`{"anilist_id":9,"type":"tv","tvdb_id":900}`)
	}
	b.WriteByte(']')
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(b.String()))
	}))
	defer ts.Close()

	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	next, err := l.refreshCache(context.Background(), &Cache{})
	if err == nil {
		t.Fatal("first-boot refresh of 200 duplicates of one AniList ID returned nil error, want below-minimum rejection")
	}
	if len(next.Records) != 0 {
		t.Errorf("rejected first-boot refresh produced %d records, want 0", len(next.Records))
	}
}

// TestLoader_refreshCache_acceptedDuplicateKeepsLastRecord pins
// deduplicateRecords' documented last-record-wins and stable-order semantics
// on an ACCEPTED refresh: the persisted Cache.Records (and hence the served
// index) must carry the LAST duplicate's data at the last-occurrence
// position, matching buildIndex's map-overwrite semantics. The existing
// duplicate-ID tests only exercise REJECTED refreshes, where which duplicate
// survives is never observable.
func TestLoader_refreshCache_acceptedDuplicateKeepsLastRecord(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[` +
			`{"anilist_id":9,"type":"tv","tvdb_id":900},` +
			`{"anilist_id":1,"type":"tv","tvdb_id":100},` +
			`{"anilist_id":9,"type":"tv","tvdb_id":901},` +
			`{"anilist_id":2,"type":"tv","tvdb_id":200},` +
			`{"anilist_id":3,"type":"tv","tvdb_id":300}]`))
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
	next, err := l.refreshCache(context.Background(), prev)
	if err != nil {
		t.Fatalf("refresh with one duplicated ID returned error %v, want accepted (4 effective records against 4 stale)", err)
	}
	if len(next.Records) != 4 {
		t.Fatalf("accepted refresh kept %d records, want 4 (duplicate collapsed to one effective record)", len(next.Records))
	}
	wantIDs := []int{1, 9, 2, 3}
	for i, want := range wantIDs {
		if next.Records[i].AniListID != want {
			t.Errorf("accepted refresh record[%d].AniListID = %d, want %d (stable last-occurrence order)", i, next.Records[i].AniListID, want)
		}
	}
	rec9 := next.Records[1]
	if rec9.AniListID == 9 && rec9.TvdbID != 901 {
		t.Errorf("duplicated record persisted TvdbID = %d, want 901 (last record wins, matching buildIndex)", rec9.TvdbID)
	}
}

// TestValidateRefreshedRecordsOneArrIdentifierCollapseRejected pins the
// per-side resolvability of the routing floor: a candidate that keeps every
// type label and every TVDB id but loses all movie TMDB/IMDb ids preserves the
// global arr-identifier floor and the type-label routing counts, yet the
// matcher could then resolve no Radarr entry at all. routingCounts must count
// records that can actually resolve in their routed arr (HasArrIdentifier),
// so a collapse of one arr's resolvable population is rejected in favour of
// the stale map.
func TestValidateRefreshedRecordsOneArrIdentifierCollapseRejected(t *testing.T) {
	previous := make([]Record, 0, 200)
	candidate := make([]Record, 0, 200)
	for id := 1; id <= 100; id++ {
		previous = append(previous, Record{AniListID: id, Type: "MOVIE", TmdbMovies: []int{id}})
		candidate = append(candidate, Record{AniListID: id, Type: "MOVIE"})
	}
	for id := 101; id <= 200; id++ {
		previous = append(previous, Record{AniListID: id, Type: "TV", TvdbID: id})
		candidate = append(candidate, Record{AniListID: id, Type: "TV", TvdbID: id})
	}
	if err := validateRefreshedRecords(previous, candidate, len(candidate)); err == nil {
		t.Fatal("refresh that lost every movie identifier returned nil error, want rejection")
	}
}

// TestValidateRefreshedRecordsScopeCollapseRejected pins the scope-coverage
// floor: a candidate that keeps 200 valid AniList IDs, TVDB ids, non-empty
// types, and unchanged routing counts — but wholesale zeroes every positive
// SeasonTvdb, or relabels every special as TV — silently degrades comparison
// scope (whole-series instead of the mapped season; specials bypassing
// exclude_specials and the season-0 bucket) and must be rejected in favour of
// the stale map.
func TestValidateRefreshedRecordsScopeCollapseRejected(t *testing.T) {
	previous := make([]Record, 0, 200)
	for id := 1; id <= 100; id++ {
		previous = append(previous, Record{AniListID: id, Type: "TV", TvdbID: id, SeasonTvdb: 1})
	}
	for id := 101; id <= 200; id++ {
		previous = append(previous, Record{AniListID: id, Type: "OVA", TvdbID: id, SeasonTvdb: 1})
	}
	tests := []struct {
		name   string
		mutate func(r *Record)
	}{
		{"every positive season zeroed", func(r *Record) { r.SeasonTvdb = 0 }},
		{"every special relabeled TV", func(r *Record) { r.Type = "TV" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			candidate := make([]Record, len(previous))
			copy(candidate, previous)
			for i := range candidate {
				tc.mutate(&candidate[i])
			}
			if err := validateRefreshedRecords(previous, candidate, len(candidate)); err == nil {
				t.Error("scope-collapsing refresh returned nil error, want rejection")
			}
		})
	}
}

// TestValidateRefreshedRecordsScopeAdditiveGrowthAccepted pins the accepting
// side of the scope floor's loss requirement: an additive refresh that grows
// the record count (raising the ceiling-derived minimum) while RETAINING every
// season-scoped and special record must be accepted — the floor fires only on
// a genuine loss, never on catalogue growth.
func TestValidateRefreshedRecordsScopeAdditiveGrowthAccepted(t *testing.T) {
	previous := make([]Record, 0, 100)
	previous = append(previous,
		Record{AniListID: 1, Type: "TV", TvdbID: 1, SeasonTvdb: 1},
		Record{AniListID: 2, Type: "OVA", TvdbID: 2},
	)
	for id := 3; id <= 100; id++ {
		previous = append(previous, Record{AniListID: id, Type: "TV", TvdbID: id})
	}
	candidate := make([]Record, len(previous), len(previous)+101)
	copy(candidate, previous)
	for id := 101; id <= 201; id++ {
		candidate = append(candidate, Record{AniListID: id, Type: "TV", TvdbID: id})
	}
	if err := validateRefreshedRecords(previous, candidate, len(candidate)); err != nil {
		t.Errorf("additive refresh retaining all season-scoped and special records returned error %v, want accepted", err)
	}
}

// TestValidateRefreshedRecordsScopeSparsePreviousAccepted pins the scope
// floor's previous-cache gate: when the previously accepted cache is itself
// scope-sparse (no season-scoped and no special records, so it never met the
// floor), an equally scope-sparse but otherwise valid refresh is the
// catalogue's established shape and must be accepted — not rejected on an
// absolute requirement the tolerant decoders do not impose.
func TestValidateRefreshedRecordsScopeSparsePreviousAccepted(t *testing.T) {
	previous := make([]Record, 0, 200)
	candidate := make([]Record, 0, 200)
	for id := 1; id <= 200; id++ {
		previous = append(previous, Record{AniListID: id, Type: "TV", TvdbID: id})
		candidate = append(candidate, Record{AniListID: id, Type: "TV", TvdbID: id})
	}
	if err := validateRefreshedRecords(previous, candidate, len(candidate)); err != nil {
		t.Errorf("scope-sparse refresh over a scope-sparse cache returned error %v, want accepted", err)
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
