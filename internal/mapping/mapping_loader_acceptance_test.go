package mapping

import (
	"errors"
	"strings"
	"testing"
)

// coverage is total entries, the first covered of them on a TVDB series.
func coverage(covered, total int) []entry {
	es := make([]entry, 0, total)
	for i := 1; i <= total; i++ {
		e := entry{AniList: i, Type: "tv"}
		if i <= covered {
			e.TVDB = i
		}
		es = append(es, e)
	}
	return es
}

func TestLoader_refreshCache_emptyRefreshKeepsStale(t *testing.T) {
	next, err := refresh(t, staleCache(""), encode(t, document(nil, nil)))
	if err == nil {
		t.Fatal("empty refresh returned nil error, want degraded error")
	}
	if len(next.Records) != 1 || next.Records[0].AniListID != 1 || next.Records[0].TvdbID != 100 {
		t.Fatalf("empty refresh records = %+v, want stale record id 1 on TVDB 100", next.Records)
	}
}

// TestLoader_refreshCache_noArrIdentifierKeepsStale covers the acceptance
// guard: a document whose entries reach no arr identifier (a wholesale loss
// of the series and film ids) is treated like an empty one and keeps the
// usable stale map.
func TestLoader_refreshCache_noArrIdentifierKeepsStale(t *testing.T) {
	next, err := refresh(t, staleCache(""), entriesBody(t, entry{AniList: 1, Type: "tv"}, entry{AniList: 2, Type: "movie"}))
	if err == nil {
		t.Fatal("refresh with no arr identifiers returned nil error, want degraded error")
	}
	if len(next.Records) != 1 || next.Records[0].TvdbID != 100 {
		t.Fatalf("no-arr-id refresh records = %+v, want stale record id 1 on TVDB 100", next.Records)
	}
	if next.RejectedRefreshes != 1 {
		t.Errorf("no-arr-id refresh RejectedRefreshes = %d, want 1 (the validation floor is an acceptance-guard rejection)", next.RejectedRefreshes)
	}
}

// TestLoader_refreshCache_lowArrIdentifierCoverageKeepsStale covers the
// coverage floor: 1 routed entry of 250 is a wholesale degradation (below
// the 1% floor) and keeps the usable stale map.
func TestLoader_refreshCache_lowArrIdentifierCoverageKeepsStale(t *testing.T) {
	next, err := refresh(t, staleCache(""), entriesBody(t, coverage(1, 250)...))
	if err == nil {
		t.Fatal("refresh with 1/250 arr-identifier coverage returned nil error, want degraded error")
	}
	if len(next.Records) != 1 || next.Records[0].TvdbID != 100 {
		t.Fatalf("low-coverage refresh records = %+v, want stale record id 1 on TVDB 100", next.Records)
	}
}

// TestLoader_refreshCache_coverageFloor pins the ceiling arithmetic of the
// arr-identifier coverage minimum on a first boot: 1 of 100 is exactly the
// floor and accepted; for 199 entries the floor is 2 (ceiling), so 1/199 is
// rejected and 2/199 accepted - floor division would wrongly admit 1/199.
func TestLoader_refreshCache_coverageFloor(t *testing.T) {
	for _, tc := range []struct {
		name       string
		covered    int
		total      int
		wantAccept bool
	}{
		{name: "1 of 100 accepted", covered: 1, total: 100, wantAccept: true},
		{name: "1 of 199 rejected", covered: 1, total: 199},
		{name: "2 of 199 accepted", covered: 2, total: 199, wantAccept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next, err := refresh(t, &Cache{}, entriesBody(t, coverage(tc.covered, tc.total)...))
			if tc.wantAccept {
				if err != nil || len(next.Records) != tc.total {
					t.Fatalf("refresh with %d/%d arr identifiers = %d records, %v, want all %d accepted", tc.covered, tc.total, len(next.Records), err, tc.total)
				}
				return
			}
			if err == nil || len(next.Records) != 0 {
				t.Fatalf("refresh with %d/%d arr identifiers = %d records, %v, want the below-minimum rejection", tc.covered, tc.total, len(next.Records), err)
			}
		})
	}
}

// TestLoader_refreshCache_truncatedRefreshKeepsStale covers truncated-refresh
// rejection: a valid document that shrinks the map to less than half the
// previous record count (1 replacing 4) degrades to the stale cache, here
// through the series-routed collapse guard, which fires before the whole-map
// shrink guard (TestLoader_refreshCache_wholeMapShrinkGuardKeepsStale).
func TestLoader_refreshCache_truncatedRefreshKeepsStale(t *testing.T) {
	prev := fourSeries()
	next, err := refresh(t, prev, entriesBody(t, entry{AniList: 9, Type: "tv", TVDB: 900}))
	if err == nil {
		t.Fatal("truncated refresh (1 record replacing 4) returned nil error, want degraded error")
	}
	if len(next.Records) != 4 || next.Records[3].AniListID != 4 || next.Records[3].TvdbID != 400 {
		t.Fatalf("truncated refresh kept %+v, want the 4 stale records unchanged", next.Records)
	}
}

// TestLoader_refreshCache_routingCollapseKeepsStale covers the
// routing-distribution floor: a document that keeps every entry routed but
// collapses one route population is refused in favour of the stale map,
// both ways - every film losing its TMDB movie, its published type kept, and
// every series losing its TVDB id.
func TestLoader_refreshCache_routingCollapseKeepsStale(t *testing.T) {
	for name, es := range map[string][]entry{
		"films without their TMDB movie": {
			{AniList: 1, Type: "movie", TVDB: 100},
			{AniList: 2, Type: "movie", TVDB: 101},
			{AniList: 3, Type: "tv", TVDB: 300},
			{AniList: 4, Type: "tv", TVDB: 400},
		},
		"series without their TVDB id": {
			{AniList: 1, Type: "movie", Movies: []int{42}},
			{AniList: 2, Type: "movie", Movies: []int{43}},
			{AniList: 3, Type: "movie", Movies: []int{44}},
			{AniList: 4, Type: "movie", Movies: []int{45}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			prev := routingFloorPrevCache()
			next, err := refresh(t, prev, entriesBody(t, es...))
			if _, ok := errors.AsType[*StaleMapError](err); !ok {
				t.Fatalf("routing-collapse refresh error = %v, want a *StaleMapError guard rejection", err)
			}
			if len(next.Records) != len(prev.Records) || next.RejectedRefreshes != 1 {
				t.Errorf("routing-collapse refresh = %d records, streak %d, want the %d stale records and a streak of 1", len(next.Records),
					next.RejectedRefreshes, len(prev.Records))
			}
		})
	}
}

// routingFloorPrevCache returns a previously accepted cache with both routing
// populations above the 1% floor: two films and two series.
func routingFloorPrevCache() *Cache {
	prev := staleCache("v2026.10.16")
	prev.Records = []Record{
		{AniListID: 1, TmdbMovies: []int{42}},
		{AniListID: 2, TmdbMovies: []int{43}},
		{AniListID: 3, TvdbID: 300},
		{AniListID: 4, TvdbID: 400},
	}
	return prev
}

// TestLoader_refreshCache_additiveUpdateKeepsRoutingFloor pins the accepting
// side of the routing floor: an additive update growing both routing
// populations is accepted and resets the rejection streak.
func TestLoader_refreshCache_additiveUpdateKeepsRoutingFloor(t *testing.T) {
	prev := routingFloorPrevCache()
	prev.RejectedRefreshes = 3
	next, err := refresh(t, prev, entriesBody(t,
		entry{AniList: 1, Type: "movie", Movies: []int{42}}, entry{AniList: 2, Type: "movie", Movies: []int{43}},
		entry{AniList: 3, Type: "tv", TVDB: 300}, entry{AniList: 4, Type: "tv", TVDB: 400},
		entry{AniList: 5, Type: "movie", Movies: []int{44}}, entry{AniList: 6, Type: "tv", TVDB: 600}))
	if err != nil {
		t.Fatalf("additive refresh growing both routing sides returned error %v, want accepted", err)
	}
	if len(next.Records) != 6 || next.RejectedRefreshes != 0 {
		t.Errorf("accepted refresh = %d records, streak %d, want 6 and 0", len(next.Records), next.RejectedRefreshes)
	}
}

// TestLoader_refreshCache_wholeMapShrinkGuardKeepsStale pins the whole-map
// below-half shrink guard on a shape the per-population floors cannot
// intercept: the guarded populations are fully retained while the TOTAL
// record count drops below half (100 -> 40, via loss of bare records). The
// guard rejects with the FIXED "refresh shrank below half of previous"
// reason, carries the live counts as structured stale_returned and
// stale_previous facts, advances the streak and keeps the stale map.
func TestLoader_refreshCache_wholeMapShrinkGuardKeepsStale(t *testing.T) {
	prev := staleCache("")
	prev.Records = nil
	for i := 1; i <= 100; i++ {
		r := Record{AniListID: i}
		if i <= 10 {
			r.TvdbID = i
		}
		prev.Records = append(prev.Records, r)
	}
	es := coverage(10, 40)
	for i := 10; i < 40; i++ {
		es[i].Type = ""
	}
	next, err := refresh(t, prev, entriesBody(t, es...))
	stale, ok := errors.AsType[*StaleMapError](err)
	if !ok {
		t.Fatalf("below-half refresh error = %v, want a *StaleMapError guard rejection", err)
	}
	if len(next.Records) != 100 || next.RejectedRefreshes != 1 {
		t.Errorf("below-half refresh = %d records, streak %d, want the 100 stale records and a streak of 1", len(next.Records), next.RejectedRefreshes)
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
// acceptance boundary shared by the whole-map shrink guard and
// populationCollapsed: both are strictly below-half, so 4 of 8 is accepted
// and resets the streak.
func TestLoader_refreshCache_exactHalfShrinkAccepted(t *testing.T) {
	prev := staleCache("")
	prev.Records = nil
	for i := 1; i <= 8; i++ {
		prev.Records = append(prev.Records, Record{AniListID: i, TvdbID: i})
	}
	prev.RejectedRefreshes = 3
	next, err := refresh(t, prev, entriesBody(t, coverage(4, 4)...))
	if err != nil {
		t.Fatalf("exactly-half refresh (4 of 8) returned error %v, want accepted (the guards are strictly below-half)", err)
	}
	if len(next.Records) != 4 || next.RejectedRefreshes != 0 {
		t.Errorf("accepted refresh = %d records, streak %d, want 4 and 0", len(next.Records), next.RejectedRefreshes)
	}
}
