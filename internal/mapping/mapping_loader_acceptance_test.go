package mapping

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Acceptance-guard tests for Loader.refreshCache, split from
// mapping_loader_test.go: the refresh-rejection guards (empty/keyless/
// low-coverage/collapse/shrink bodies keep the stale cache) and their
// acceptance-floor counterparts.

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

// TestLoader_refreshCache_truncatedRefreshKeepsStale covers truncated-refresh
// rejection: a syntactically valid refresh that shrinks the map to less than
// half the previous record count (here 1 valid mapped record replacing 4) must
// degrade to the stale cache with an error, not replace it. For this all-typed
// shape the typed population-collapse floor (validateTypeCoverage) fires before
// the whole-map below-half shrink guard, which is pinned separately by
// TestLoader_refreshCache_wholeMapShrinkGuardKeepsStale.
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
// single effective record, which the acceptance floors must reject against a
// 4-record stale cache (the typed population-collapse floor fires first for
// this all-typed shape) instead of persisting a refresh that indexes to
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

// TestLoader_refreshCache_negativeOnlyCacheNotUsable pins the positive-ID
// half of the effective-record contract on the persisted-cache path: a
// state.json record set whose only keys are unique negative AniList IDs
// (which real SeaDex lookups can never resolve) must not make the cache
// usable, so the fresh-cache fast path falls through to the fetch instead of
// serving an index that cannot resolve any entry.
func TestLoader_refreshCache_negativeOnlyCacheNotUsable(t *testing.T) {
	records := []Record{
		{AniListID: -1, Type: "TV", TvdbID: 100},
		{AniListID: -2, Type: "TV", TvdbID: 200},
	}
	if cacheUsable(records) {
		t.Fatal("cacheUsable = true, want false: negative AniList IDs can never resolve a SeaDex lookup")
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

// TestLoader_refreshCache_firstBootNegativeIDBodyRejected pins that the
// positive-ID rule holds at the acceptance boundary too: a first-boot body of
// 200 rows with unique NEGATIVE AniList IDs and valid arr identifiers must be
// rejected (the parser drops the rows as keyless, so the AniList-key floor
// fires against the 200-element source count) instead of being accepted as a
// map no real SeaDex lookup could ever resolve against.
func TestLoader_refreshCache_firstBootNegativeIDBodyRejected(t *testing.T) {
	var b strings.Builder
	b.WriteByte('[')
	for i := 1; i <= 200; i++ {
		if i > 1 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"anilist_id":%d,"type":"tv","tvdb_id":%d}`, -i, i)
	}
	b.WriteByte(']')
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(b.String()))
	}))
	defer ts.Close()

	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	next, err := l.refreshCache(context.Background(), &Cache{})
	if err == nil {
		t.Fatal("first-boot refresh of 200 negative-ID records returned nil error, want rejection")
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
	if rec9 := next.Records[1]; rec9.AniListID != 9 || rec9.TvdbID != 901 {
		t.Errorf("accepted refresh record[1] = %+v, want the duplicated id 9 carrying the LAST record's TvdbID 901 (matching buildIndex)", rec9)
	}
}

// TestLoader_refreshCache_wholeMapShrinkGuardKeepsStale pins acceptRefresh's
// whole-map below-half shrink guard on a shape the per-population floors
// cannot intercept: the guarded populations (typed, season, special, both
// routing sides) are fully retained while the TOTAL record count drops below
// half (100 -> 40, via loss of bare id-only records). The guard must reject
// with the FIXED "refresh shrank below half of previous" reason (a
// Loki-queryable class discriminator), carry the live counts as structured
// stale_returned/stale_previous facts, advance the persisted rejection
// streak, and keep the stale map unchanged.
func TestLoader_refreshCache_wholeMapShrinkGuardKeepsStale(t *testing.T) {
	var b strings.Builder
	b.WriteByte('[')
	for i := 1; i <= 10; i++ {
		if i > 1 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"anilist_id":%d,"type":"tv","tvdb_id":%d}`, i, i)
	}
	for i := 11; i <= 40; i++ {
		fmt.Fprintf(&b, `,{"anilist_id":%d}`, i)
	}
	b.WriteByte(']')
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(b.String()))
	}))
	defer ts.Close()

	prevRecords := make([]Record, 0, 100)
	for i := 1; i <= 10; i++ {
		prevRecords = append(prevRecords, Record{AniListID: i, Type: "TV", TvdbID: i})
	}
	for i := 11; i <= 100; i++ {
		prevRecords = append(prevRecords, Record{AniListID: i})
	}
	prev := &Cache{
		FetchedAt: time.Now().Add(-2 * time.Hour),
		Records:   prevRecords,
	}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	next, err := l.refreshCache(context.Background(), prev)
	stale, ok := errors.AsType[*StaleMapError](err)
	if !ok {
		t.Fatalf("below-half refresh error = %v, want a *StaleMapError guard rejection", err)
	}
	if len(next.Records) != len(prevRecords) {
		t.Fatalf("below-half refresh kept %d records, want the %d stale records unchanged", len(next.Records), len(prevRecords))
	}
	if next.RejectedRefreshes != 1 {
		t.Errorf("below-half refresh RejectedRefreshes = %d, want 1 (the shrink guard is an acceptance-guard rejection)", next.RejectedRefreshes)
	}
	if !strings.Contains(stale.Error(), "refresh shrank below half of previous (returned 40, previous 100)") {
		t.Errorf("StaleMapError text = %q, want the fixed reason with the shrink counts parenthetical", stale.Error())
	}
	attrs := stale.LogAttrs()
	var gotReturned, gotPrevious any
	for i := 0; i+1 < len(attrs); i += 2 {
		switch attrs[i] {
		case "stale_returned":
			gotReturned = attrs[i+1]
		case "stale_previous":
			gotPrevious = attrs[i+1]
		}
	}
	if gotReturned != 40 || gotPrevious != 100 {
		t.Errorf("LogAttrs stale_returned=%v stale_previous=%v, want 40 and 100", gotReturned, gotPrevious)
	}
}

// TestLoader_refreshCache_exactHalfShrinkAccepted pins the exact-half
// acceptance boundary shared by acceptRefresh's whole-map shrink guard and
// populationCollapsed: both use a strict below-half comparison
// (degradation.Shrunk's count*factor < prevCount), so a refresh retaining
// EXACTLY half of the previous records (4 of 8, every guarded population
// halved together) must be accepted and reset the rejection streak. The
// existing shrink tests only pin below-half rejection and above-half
// acceptance (1001 of 2000), leaving the <= boundary mutant alive in both
// guards.
func TestLoader_refreshCache_exactHalfShrinkAccepted(t *testing.T) {
	var b strings.Builder
	b.WriteByte('[')
	for i := 1; i <= 4; i++ {
		if i > 1 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"anilist_id":%d,"type":"tv","tvdb_id":%d}`, i, i)
	}
	b.WriteByte(']')
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(b.String()))
	}))
	defer ts.Close()

	prevRecords := make([]Record, 0, 8)
	for i := 1; i <= 8; i++ {
		prevRecords = append(prevRecords, Record{AniListID: i, Type: "TV", TvdbID: i})
	}
	prev := &Cache{
		FetchedAt:         time.Now().Add(-2 * time.Hour),
		Records:           prevRecords,
		RejectedRefreshes: 3,
	}
	l := NewLoader(ts.Client(), ts.URL, "", time.Hour, discardLogger())
	next, err := l.refreshCache(context.Background(), prev)
	if err != nil {
		t.Fatalf("exactly-half refresh (4 of 8) returned error %v, want accepted (the guards are strictly below-half)", err)
	}
	if len(next.Records) != 4 {
		t.Fatalf("accepted refresh records = %d, want 4", len(next.Records))
	}
	if next.RejectedRefreshes != 0 {
		t.Errorf("accepted refresh RejectedRefreshes = %d, want 0 (acceptance resets the streak)", next.RejectedRefreshes)
	}
}
