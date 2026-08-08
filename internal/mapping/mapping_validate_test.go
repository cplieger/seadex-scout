package mapping

import (
	"testing"
)

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

// TestValidateRefreshedRecordsMidBandPopulationCollapseRejected pins the
// per-population shrink guards (populationCollapsed): a refresh that keeps
// the record count and every 1%-of-body floor green while gutting MOST of one
// population - typed records dropping 2000 -> 40 in a 2000-record body, where
// 40 still clears the 1% floor of 20 - must be rejected in favour of the
// stale map. A drop that retains at least half of the previous population is
// accepted (the guard mirrors the whole-map below-half shrink guard), and a
// PARTIAL drop of a population below the significance gate (under 1% of the
// previous body) is never shrink-guarded, so tiny populations cannot reject
// noisily. Total extinction of such a population IS rejected, by the separate
// gate-free rule pinned in
// TestValidateRefreshedRecordsPopulationExtinctionRejected.
func TestValidateRefreshedRecordsMidBandPopulationCollapseRejected(t *testing.T) {
	const body = 2000
	previous := make([]Record, 0, body)
	for id := 1; id <= body; id++ {
		previous = append(previous, Record{AniListID: id, Type: "TV", TvdbID: id})
	}

	midBand := make([]Record, len(previous))
	copy(midBand, previous)
	for i := 40; i < len(midBand); i++ {
		midBand[i].Type = "" // routing (TvdbID) intact: only the typed population collapses
	}
	if err := validateRefreshedRecords(previous, midBand, len(midBand)); err == nil {
		t.Error("mid-band typed collapse (2000 -> 40, above the 1% floor) returned nil error, want rejection")
	}

	aboveHalf := make([]Record, len(previous))
	copy(aboveHalf, previous)
	for i := 1001; i < len(aboveHalf); i++ {
		aboveHalf[i].Type = ""
	}
	if err := validateRefreshedRecords(previous, aboveHalf, len(aboveHalf)); err != nil {
		t.Errorf("at-least-half typed retention rejected: %v (the guard is below-half-of-previous)", err)
	}

	sparsePrev := make([]Record, 0, body)
	for id := 1; id <= body; id++ {
		r := Record{AniListID: id, Type: "TV", TvdbID: id}
		if id <= 10 {
			r.Type = "SPECIAL" // a 10-record population, below the 1% significance gate (20)
		}
		sparsePrev = append(sparsePrev, r)
	}
	noSpecials := make([]Record, len(sparsePrev))
	copy(noSpecials, sparsePrev)
	for i := range noSpecials {
		// A PARTIAL drop (10 -> 4): below half of the previous population, but
		// under the significance gate, so neither shrink guard fires. Total
		// extinction is a different rule with no gate at all
		// (populationExtinct, pinned by
		// TestValidateRefreshedRecordsPopulationExtinctionRejected), so this
		// case deliberately keeps four specials.
		if noSpecials[i].Type == "SPECIAL" && noSpecials[i].AniListID > 4 {
			noSpecials[i].Type = "TV"
		}
	}
	if err := validateRefreshedRecords(sparsePrev, noSpecials, len(noSpecials)); err != nil {
		t.Errorf("sparse-population partial drop rejected: %v (populations under the significance gate are not shrink-guarded)", err)
	}
}

// TestValidateRefreshedRecordsPopulationExtinctionRejected pins the extinction
// guard (populationExtinct, l-f68): a population going from N to EXACTLY zero
// is rejected even when N is far below the 1% significance gate the shrink
// guards use, because total loss is never a sampling artifact. The partial
// shrink of the same sparse population stays accepted - that exemption is
// pinned above by TestValidateRefreshedRecordsMidBandPopulationCollapseRejected.
func TestValidateRefreshedRecordsPopulationExtinctionRejected(t *testing.T) {
	const body = 2000 // significance gate = 1% = 20 records
	base := func(mark func(r *Record, id int)) []Record {
		records := make([]Record, 0, body)
		for id := 1; id <= body; id++ {
			r := Record{AniListID: id, Type: "TV", TvdbID: id}
			mark(&r, id)
			records = append(records, r)
		}
		return records
	}

	// A 3-record special population, far below the gate of 20.
	sparsePrev := base(func(r *Record, id int) {
		if id <= 3 {
			r.Type = "SPECIAL"
		}
	})
	extinct := base(func(r *Record, id int) {})
	// Extinction is a guard refusal like any other, so it needs no extra streak
	// wiring: validateRefreshedRecords' verdict reaches the streak through
	// failureValidation, which classifyRefreshFailure grades persistent
	// (TestClassifyRefreshFailure) - the same route the coverage and shrink
	// guards already take.
	if err := validateRefreshedRecords(sparsePrev, extinct, len(extinct)); err == nil {
		t.Error("a sparse population going 3 -> 0 was accepted, want rejection (extinction needs no significance gate)")
	}

	// The same population merely shrinking stays accepted.
	partial := base(func(r *Record, id int) {
		if id <= 1 {
			r.Type = "SPECIAL"
		}
	})
	if err := validateRefreshedRecords(sparsePrev, partial, len(partial)); err != nil {
		t.Errorf("a sparse population going 3 -> 1 was rejected: %v (only extinction bypasses the significance gate)", err)
	}

	// Extinction is guarded on every population, not just the special one: a
	// sparse positive-season population zeroed out must reject too.
	seasonPrev := base(func(r *Record, id int) {
		if id <= 3 {
			r.SeasonTvdb = 1
		}
	})
	if err := validateRefreshedRecords(seasonPrev, extinct, len(extinct)); err == nil {
		t.Error("a sparse season-scoped population going 3 -> 0 was accepted, want rejection")
	}

	// A population that was already empty is not extinction: nothing was lost.
	if err := validateRefreshedRecords(extinct, extinct, len(extinct)); err != nil {
		t.Errorf("an already-empty population rejected: %v (prevCount 0 is not a loss)", err)
	}
}

// TestValidateRefreshedRecordsScopeMidBandCollapseRejected pins the scope
// floor's per-population shrink guards (populationCollapsed), which the
// existing scope tests never reach: zeroing a WHOLE population fires the
// coverageLost branch first, so the mid-band - most of one scope population
// gutted while the survivor count still clears the 1% floor - was unguarded
// by any test. Seasons 2000 -> 40 (SeasonTvdb zeroed above index 40) and
// specials 200 -> 40 (OVA relabeled TV) each keep every other floor green
// yet must be rejected in favour of the stale map.
func TestValidateRefreshedRecordsScopeMidBandCollapseRejected(t *testing.T) {
	const body = 2000
	seasonPrev := make([]Record, 0, body)
	for id := 1; id <= body; id++ {
		seasonPrev = append(seasonPrev, Record{AniListID: id, Type: "TV", TvdbID: id, SeasonTvdb: 1})
	}
	seasonMidBand := make([]Record, len(seasonPrev))
	copy(seasonMidBand, seasonPrev)
	for i := 40; i < len(seasonMidBand); i++ {
		seasonMidBand[i].SeasonTvdb = 0
	}
	if err := validateRefreshedRecords(seasonPrev, seasonMidBand, len(seasonMidBand)); err == nil {
		t.Error("mid-band season collapse (2000 -> 40, above the 1% floor) returned nil error, want rejection")
	}

	specialPrev := make([]Record, 0, body)
	for id := 1; id <= body; id++ {
		r := Record{AniListID: id, Type: "TV", TvdbID: id}
		if id <= 200 {
			r.Type = "OVA"
		}
		specialPrev = append(specialPrev, r)
	}
	specialMidBand := make([]Record, len(specialPrev))
	copy(specialMidBand, specialPrev)
	for i := 40; i < 200; i++ {
		specialMidBand[i].Type = "TV"
	}
	if err := validateRefreshedRecords(specialPrev, specialMidBand, len(specialMidBand)); err == nil {
		t.Error("mid-band special collapse (200 -> 40, above the 1% floor) returned nil error, want rejection")
	}
}

// TestValidateRefreshedRecordsRoutingMidBandCollapseRejected pins the routing
// floor's per-population shrink guards (populationCollapsed), which the
// existing routing tests never reach: collapsing a WHOLE side fires the
// coverageLost branch first, so the mid-band - most of one resolvable routing
// side gutted while the survivors still clear the 1% floor - was unguarded by
// any test. Movie-routed 200 -> 40 (TMDB ids stripped, types intact) and
// series-routed 1800 -> 800 (TVDB ids zeroed) each keep every other floor
// green yet must be rejected in favour of the stale map.
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

// TestValidateRefreshedRecordsTypedAtFloorAccepted pins the at-floor
// acceptance boundary of coverageLost: the floor comparison is strictly
// below-minimum (count < minimum), so a typed population sitting EXACTLY at
// the ceiling-derived 1% floor (2 typed of 200 records, floor 2) after a
// small legal loss (previous carried 3) must be accepted. The existing floor
// tests pin below-floor rejection and growth acceptance, leaving the
// count <= minimum boundary mutant alive.
func TestValidateRefreshedRecordsTypedAtFloorAccepted(t *testing.T) {
	previous := make([]Record, 0, 200)
	candidate := make([]Record, 0, 200)
	for id := 1; id <= 200; id++ {
		p := Record{AniListID: id, TvdbID: id}
		c := Record{AniListID: id, TvdbID: id}
		if id <= 3 {
			p.Type = "TV"
		}
		if id <= 2 {
			c.Type = "TV"
		}
		previous = append(previous, p)
		candidate = append(candidate, c)
	}
	if err := validateRefreshedRecords(previous, candidate, len(candidate)); err != nil {
		t.Errorf("typed count at the exact floor (2 of 200, floor 2) returned error %v, want accepted (the floor is strictly below-minimum)", err)
	}
}

// TestValidateRefreshedRecordsPreviousPopulationAtSignificanceGateRejected
// pins the significance gate shared by coverageLost and populationCollapsed
// (prevCount >= previousMinimum): a previous population sitting EXACTLY at the
// ceiling-derived 1% floor still counts as meaningful, so a candidate that
// wholesale loses it must be rejected. Every other floor test carries a
// previous population far above the gate (100 of 200, 2000 of 2000) or far
// below it (10 of 2000), so the gate's boundary itself was unpinned.
func TestValidateRefreshedRecordsPreviousPopulationAtSignificanceGateRejected(t *testing.T) {
	const body = 200
	previous := make([]Record, 0, body)
	candidate := make([]Record, 0, body)
	for id := 1; id <= body; id++ {
		p := Record{AniListID: id, TvdbID: id}
		c := Record{AniListID: id, TvdbID: id}
		if id <= 2 {
			p.Type = "TV" // typed population == coverageFloor(200) == 2
		}
		if id <= 1 {
			c.Type = "TV" // one survivor: below the candidate floor, not extinct
		}
		previous = append(previous, p)
		candidate = append(candidate, c)
	}
	if got, want := typedRecordCount(previous), coverageFloor(len(previous)); got != want {
		t.Fatalf("previous typed population = %d, want exactly the significance gate %d", got, want)
	}
	err := validateRefreshedRecords(previous, candidate, len(candidate))
	if err == nil {
		t.Fatal("typed loss against a previous population exactly at the significance gate returned nil error, want rejection")
	}
	const wantMsg = "type coverage 1/200 is below minimum 2 (previous cache carried 2 typed records)"
	if err.Error() != wantMsg {
		t.Errorf("rejection = %q, want the loss-relative coverage floor %q (an extinction message means the gate is still unexercised)", err, wantMsg)
	}
}
