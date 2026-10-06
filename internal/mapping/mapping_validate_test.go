package mapping

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cplieger/httpx/v5"
)

// TestValidateRefreshedRecordsOneArrIdentifierCollapseRejected pins the
// per-side resolvability of the routing floor: a candidate that keeps every
// type label and every TVDB id but loses all movie TMDB/IMDb ids preserves the
// global arr-identifier floor and the type-label routing counts, yet the
// matcher could then resolve no Radarr entry at all. censusOf must count
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

// TestValidateRefreshedRecordsRoutingMidBandCollapseRejected pins the routing
// floor's per-population shrink guards (populationCollapsed): the mid-band -
// most of one resolvable routing side gutted while the survivors still clear
// the 1% floor - is what the extinction guard cannot see. Movie-routed
// 200 -> 40 (TMDB ids stripped, types intact) and series-routed 1800 -> 800
// (TVDB ids zeroed) each keep every other floor green yet must be rejected in
// favour of the stale map.
func TestValidateRefreshedRecordsRoutingMidBandCollapseRejected(t *testing.T) {
	const body = 2000
	previous := make([]Record, 0, body)
	for id := 1; id <= 200; id++ {
		previous = append(previous, Record{AniListID: id, Type: "MOVIE", TmdbMovies: []int{id}})
	}
	for id := 201; id <= body; id++ {
		previous = append(previous, Record{AniListID: id, Type: "TV", TvdbID: id})
	}

	movieMidBand := make([]Record, len(previous))
	copy(movieMidBand, previous)
	for i := 40; i < 200; i++ {
		movieMidBand[i].TmdbMovies = nil
	}
	if err := validateRefreshedRecords(previous, movieMidBand, len(movieMidBand)); err == nil {
		t.Error("mid-band movie-routed collapse (200 -> 40, above the 1% floor) returned nil error, want rejection")
	}

	seriesMidBand := make([]Record, len(previous))
	copy(seriesMidBand, previous)
	for i := 1000; i < body; i++ {
		seriesMidBand[i].TvdbID = 0
	}
	if err := validateRefreshedRecords(previous, seriesMidBand, len(seriesMidBand)); err == nil {
		t.Error("mid-band series-routed collapse (1800 -> 800, above the 1% floor) returned nil error, want rejection")
	}
}

// TestValidateRefreshedRecordsCollapseExactlyAtTheSignificanceFloorRejected pins
// the inclusive end of the shrink guard's significance gate. The gate exists so a
// SPARSE population keeps its exemption, and a population sitting exactly on the
// previously accepted cache's own 1% floor is not sparse - it is the smallest
// population the guard is defined over, so a below-half collapse of it must still
// be refused. Movie-routed 3 -> 1 over a 300-record cache (floor 3) is that
// boundary: one record short of the floor the same collapse is exempt, so an
// exclusive gate would silently give the smallest guarded population away.
func TestValidateRefreshedRecordsCollapseExactlyAtTheSignificanceFloorRejected(t *testing.T) {
	const body = 300
	previous := make([]Record, 0, body)
	for id := 1; id <= 3; id++ {
		previous = append(previous, Record{AniListID: id, Type: "MOVIE", TmdbMovies: []int{id}})
	}
	for id := 4; id <= body; id++ {
		previous = append(previous, Record{AniListID: id, Type: "TV", TvdbID: id})
	}

	candidate := make([]Record, len(previous))
	copy(candidate, previous)
	candidate[1].TmdbMovies = nil
	candidate[2].TmdbMovies = nil

	if err := validateRefreshedRecords(previous, candidate, len(candidate)); err == nil {
		t.Error("movie-routed collapse 3 -> 1 with the significance floor at 3 returned nil error, want rejection")
	}
}

// TestAcceptRefresh_mappingListCollapseRejected pins the guard over the
// mapping-list facts: a body that keeps every record but loses most of its
// mapping lists would drop every film's special episode and every season range
// while each record guard stays green, so it is refused in favour of the stale
// map; a body keeping half of them is accepted.
func TestAcceptRefresh_mappingListCollapseRejected(t *testing.T) {
	const records = 200
	prev := &Cache{Mappings: map[int]Mapping{}}
	for id := 1; id <= records; id++ {
		prev.Records = append(prev.Records, Record{AniListID: id, AniDBID: id, Type: "TV", TvdbID: id})
		if id <= 10 {
			prev.Mappings[id] = Mapping{SpecialEpisode: 1}
		}
	}
	body := func(withFacts int) []byte {
		var b strings.Builder
		b.WriteByte('[')
		for id := 1; id <= records; id++ {
			if id > 1 {
				b.WriteByte(',')
			}
			if id <= withFacts {
				fmt.Fprintf(&b, `{"anilist_id":%d,"anidb_id":%d,"type":"TV","tvdb_id":%d,"tvdb_season":0,`+
					`"mapping_list":[{"anidb_season":1,"tvdb_season":0,"episodes":[[1,1]]}]}`, id, id, id)
				continue
			}
			fmt.Fprintf(&b, `{"anilist_id":%d,"anidb_id":%d,"type":"TV","tvdb_id":%d}`, id, id, id)
		}
		b.WriteByte(']')
		return animapBody(b.String())
	}
	l := &Loader{log: discardLogger()}
	next, err := l.acceptRefresh(prev, httpx.ConditionalResult{Body: body(4)})
	stale, ok := errors.AsType[*StaleMapError](err)
	if !ok || !attrsContain(stale.LogAttrs(), "stale_reason", "refresh validation failed") {
		t.Fatalf("acceptRefresh(4 of 10 mapping lists kept) error = %v, want the stale map with stale_reason refresh validation failed", err)
	}
	if len(next.Mappings) != 10 {
		t.Errorf("refused refresh kept %d mapping-list facts, want the stale 10", len(next.Mappings))
	}
	next, err = l.acceptRefresh(prev, httpx.ConditionalResult{Body: body(5)})
	if err != nil {
		t.Fatalf("acceptRefresh(5 of 10 mapping lists kept) error = %v, want acceptance", err)
	}
	if len(next.Mappings) != 5 {
		t.Errorf("accepted refresh carries %d mapping-list facts, want the body's 5", len(next.Mappings))
	}
}

// TestAcceptRefresh_specialsOfParentFactsExtinctionRejected pins that the two
// mapping-list populations are guarded apart: a body that keeps every
// AniDB-keyed fact but loses every specials-of-parent fact would pass a guard
// over their sum, and silently drop each such special's TVDB episode.
func TestAcceptRefresh_specialsOfParentFactsExtinctionRejected(t *testing.T) {
	const records = 200
	prev := &Cache{Mappings: map[int]Mapping{}, ParentMappings: map[int]Mapping{}}
	for id := 1; id <= records; id++ {
		prev.Records = append(prev.Records, Record{AniListID: id, AniDBID: id, Type: "TV", TvdbID: id})
		if id <= 10 {
			prev.Mappings[id] = Mapping{SpecialEpisode: 1}
		}
	}
	prev.Records = append(prev.Records, Record{AniListID: 1000, Type: "OVA", TvdbID: 1})
	prev.ParentMappings[1000] = Mapping{SpecialEpisode: 2}
	body := func(withParent bool) []byte {
		var b strings.Builder
		b.WriteByte('[')
		for id := 1; id <= records; id++ {
			if id > 1 {
				b.WriteByte(',')
			}
			if id <= 10 {
				fmt.Fprintf(&b, `{"anilist_id":%d,"anidb_id":%d,"type":"TV","tvdb_id":%d,"tvdb_season":0,`+
					`"mapping_list":[{"anidb_season":1,"tvdb_season":0,"episodes":[[1,1]]}]}`, id, id, id)
				continue
			}
			fmt.Fprintf(&b, `{"anilist_id":%d,"anidb_id":%d,"type":"TV","tvdb_id":%d}`, id, id, id)
		}
		parent := `,{"anilist_id":1000,"type":"OVA","tvdb_id":1,"tvdb_season":0`
		if withParent {
			parent += `,"anidb_parent":{"anidb_id":1,"specials":[2]},` +
				`"mapping_list":[{"anidb_season":1,"tvdb_season":0,"episodes":[[1,2]]}]`
		}
		b.WriteString(parent + `}]`)
		return animapBody(b.String())
	}
	l := &Loader{log: discardLogger()}
	next, err := l.acceptRefresh(prev, httpx.ConditionalResult{Body: body(false)})
	stale, ok := errors.AsType[*StaleMapError](err)
	if !ok || !attrsContain(stale.LogAttrs(), "stale_reason", "refresh validation failed") {
		t.Fatalf("acceptRefresh(every specials-of-parent fact lost) error = %v, want the stale map with stale_reason refresh validation failed", err)
	}
	if len(next.ParentMappings) != 1 || len(next.Mappings) != 10 {
		t.Errorf("refused refresh kept %d parent and %d AniDB-keyed facts, want the stale 1 and 10", len(next.ParentMappings), len(next.Mappings))
	}
	next, err = l.acceptRefresh(prev, httpx.ConditionalResult{Body: body(true)})
	if err != nil {
		t.Fatalf("acceptRefresh(specials-of-parent fact kept) error = %v, want acceptance", err)
	}
	if len(next.ParentMappings) != 1 {
		t.Errorf("accepted refresh carries %d specials-of-parent facts, want the body's 1", len(next.ParentMappings))
	}
}
