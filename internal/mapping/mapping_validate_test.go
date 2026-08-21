package mapping

import (
	"testing"
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

// TestValidateRefreshedRecordsCollapseExactlyAtTheSignificanceFloorRejected
// pins the inclusive end of the shrink guard's significance gate. The gate
// exists so a SPARSE population keeps its exemption, and a population sitting
// exactly on the previously accepted cache's own 1% floor is not sparse - it is
// the smallest population the guard is defined over, so a below-half collapse
// of it must still be refused. Movie-routed 3 -> 1 over a 300-record cache
// (floor 3) is that boundary: one record short of the floor the same collapse
// is exempt, so an exclusive gate would silently give the smallest guarded
// population away.
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
