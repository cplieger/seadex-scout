package mapping

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/animap/mapread"
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
	if err := validateRefreshedRecords(previous, candidate); err == nil {
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
	if err := validateRefreshedRecords(previous, movieMidBand); err == nil {
		t.Error("mid-band movie-routed collapse (200 -> 40, above the 1% floor) returned nil error, want rejection")
	}

	seriesMidBand := make([]Record, len(previous))
	copy(seriesMidBand, previous)
	for i := 1000; i < body; i++ {
		seriesMidBand[i].TvdbID = 0
	}
	if err := validateRefreshedRecords(previous, seriesMidBand); err == nil {
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

	if err := validateRefreshedRecords(previous, candidate); err == nil {
		t.Error("movie-routed collapse 3 -> 1 with the significance floor at 3 returned nil error, want rejection")
	}
}

// refresh downloads body over prev through a release server.
func refresh(t *testing.T, prev *Cache, body []byte) (Cache, error) {
	t.Helper()
	return newRelease(t, testTag, body).loader().refreshCache(t.Context(), prev)
}

// factsCache is a usable cache of 200 series records, AniDB and AniList ids
// equal, with facts for the anime in facts.
func factsCache(facts map[int]Mapping) *Cache {
	prev := &Cache{Mappings: facts, AniListMappings: map[int]Mapping{}}
	for id := 1; id <= 200; id++ {
		prev.Records = append(prev.Records, Record{AniListID: id, AniDBID: id, Type: "TV", TvdbID: id})
	}
	return prev
}

// factsBody is 200 series entries whose episode lands on lands(id), nil
// stating nothing.
func factsBody(t *testing.T, lands func(id int) *[]mapread.Target, extra ...entry) []byte {
	t.Helper()
	es := make([]entry, 0, 200+len(extra))
	for id := 1; id <= 200; id++ {
		es = append(es, entry{AniList: id, Type: "TV", TVDB: id, Lands: lands(id)})
	}
	return entriesBody(t, append(es, extra...)...)
}

// TestRefresh_seasonRangeCollapseRejected pins the guard over the season
// ranges: a document that keeps every record but loses most of its ranges
// would drop every pack label and own-season scope while each record guard
// stays green, so it is refused; one keeping half of them is accepted, and
// ranges on other anime do not stand in for the ones lost.
func TestRefresh_seasonRangeCollapseRejected(t *testing.T) {
	prevFacts := map[int]Mapping{}
	for id := 1; id <= 10; id++ {
		prevFacts[id] = Mapping{Seasons: []SeasonRange{{Season: 1, First: 1, Last: 1}}}
	}
	keep := func(n int) func(int) *[]mapread.Target {
		return func(id int) *[]mapread.Target {
			if id <= n {
				return targets(dflt(1, 1))
			}
			return nil
		}
	}
	next, err := refresh(t, factsCache(prevFacts), factsBody(t, keep(4)))
	stale, ok := errors.AsType[*StaleMapError](err)
	if !ok || !attrsContain(stale.LogAttrs(), "stale_reason", "refresh validation failed") || !strings.Contains(err.Error(), "season-range") {
		t.Fatalf("refresh(4 of 10 ranges kept) error = %v, want the season-range guard's refusal", err)
	}
	if len(next.Mappings) != 10 {
		t.Errorf("refused refresh kept %d facts, want the stale 10", len(next.Mappings))
	}
	next, err = refresh(t, factsCache(prevFacts), factsBody(t, keep(5)))
	if err != nil {
		t.Fatalf("refresh(5 of 10 ranges kept) error = %v, want acceptance", err)
	}
	if len(next.Mappings) != 5 {
		t.Errorf("accepted refresh carries %d facts, want the document's 5", len(next.Mappings))
	}
	moved := func(id int) *[]mapread.Target {
		if id > 190 {
			return targets(dflt(1, 1))
		}
		return nil
	}
	if _, err := refresh(t, factsCache(prevFacts), factsBody(t, moved)); err == nil || !strings.Contains(err.Error(), "season-range") {
		t.Errorf("refresh(ten ranges, none on the anime that had one) error = %v, want the season-range guard's refusal", err)
	}
}

// TestRefresh_aniListKeyedFactsExtinctionRejected pins that the two fact
// populations are guarded apart: a document that keeps every AniDB-keyed
// fact but loses every AniList-keyed one would pass a guard over their sum.
// A link that became an identity keeps its fact under the AniDB key its
// record now joins on, so it is accepted.
func TestRefresh_aniListKeyedFactsExtinctionRejected(t *testing.T) {
	prevFacts := map[int]Mapping{}
	for id := 1; id <= 10; id++ {
		prevFacts[id] = Mapping{Seasons: []SeasonRange{{Season: 1, First: 1, Last: 1}}}
	}
	prev := factsCache(prevFacts)
	prev.Records = append(prev.Records, Record{AniListID: 1000, Type: "OVA", TvdbID: 1})
	prev.AniListMappings[1000] = Mapping{Seasons: []SeasonRange{{Season: 2, First: 1, Last: 1}}}
	const (
		dropped = iota
		linked
		identity
	)
	body := func(link int) []byte {
		doc := document(nil, nil)
		for id := 1; id <= 200; id++ {
			a := mapread.Anime{AniDBID: id, Type: "TV", TVDBID: id, Last: mapread.Last{Regular: 2}}
			if id <= 10 {
				a.Episodes = []mapread.Run{regular(1, 1, targets(dflt(1, 1))), regular(2, 2, targets(at(2, 1)))}
			}
			doc.Anime = append(doc.Anime, a)
			doc.Entries = append(doc.Entries, mapread.Entry{AniListID: id, Type: "TV", Parts: []mapread.Part{{AniDBID: id, Kind: mapread.Regular}}})
		}
		switch link {
		case linked:
			doc.Entries = append(doc.Entries, mapread.Entry{AniListID: 1000, Type: "OVA", Parts: []mapread.Part{{AniDBID: 1, Kind: mapread.Regular, Start: 2, End: 2}}})
		case identity:
			doc.Anime = append(doc.Anime, mapread.Anime{
				AniDBID: 201, Type: "OVA", TVDBID: 1, Last: mapread.Last{Regular: 1},
				Episodes: []mapread.Run{regular(1, 1, targets(at(2, 1)))},
			})
			doc.Entries = append(doc.Entries, mapread.Entry{AniListID: 1000, Type: "OVA", Parts: []mapread.Part{{AniDBID: 201, Kind: mapread.Regular}}})
		}
		return encode(t, doc)
	}
	next, err := refresh(t, prev, body(dropped))
	stale, ok := errors.AsType[*StaleMapError](err)
	if !ok || !strings.Contains(err.Error(), "AniList-keyed season-range") || !attrsContain(stale.LogAttrs(), "stale_reason", "refresh validation failed") {
		t.Fatalf("refresh(every AniList-keyed range lost) error = %v, want the AniList-keyed guard's refusal", err)
	}
	if len(next.AniListMappings) != 1 || len(next.Mappings) != 10 {
		t.Errorf("refused refresh kept %d AniList-keyed and %d AniDB-keyed facts, want the stale 1 and 10", len(next.AniListMappings), len(next.Mappings))
	}
	next, err = refresh(t, prev, body(linked))
	if err != nil {
		t.Fatalf("refresh(AniList-keyed range kept) error = %v, want acceptance", err)
	}
	if got := next.AniListMappings[1000].Seasons; !slices.Equal(got, []SeasonRange{{Season: 2, First: 1, Last: 1}}) {
		t.Errorf("accepted refresh AniListMappings[1000].Seasons = %+v, want the link's season 2", got)
	}
	next, err = refresh(t, prev, body(identity))
	if err != nil {
		t.Fatalf("refresh(link become an identity) error = %v, want acceptance", err)
	}
	if got := next.Mappings[201].Seasons; len(next.AniListMappings) != 0 || !slices.Equal(got, []SeasonRange{{Season: 2, First: 1, Last: 1}}) {
		t.Errorf("accepted refresh Mappings[201].Seasons = %+v with %d AniList-keyed facts, want season 2 under the AniDB key and none",
			got, len(next.AniListMappings))
	}
}

func TestRefresh_placedSpecialsExtinctionRejected(t *testing.T) {
	prevFacts := map[int]Mapping{}
	for id := 11; id <= 20; id++ {
		prevFacts[id] = Mapping{Specials: []int{id}, SpecialsTvdb: id, SpecialsEpisodes: 1}
	}
	lands := func(placed bool) func(int) *[]mapread.Target {
		return func(id int) *[]mapread.Target {
			if id > 10 && id <= 20 && placed {
				return targets(at(0, id))
			}
			return nil
		}
	}
	next, err := refresh(t, factsCache(prevFacts), factsBody(t, lands(false)))
	if stale, ok := errors.AsType[*StaleMapError](err); !ok || !attrsContain(stale.LogAttrs(), "stale_reason", "refresh validation failed") {
		t.Fatalf("refresh(every special placement dropped) error = %v, want the stale map with stale_reason refresh validation failed", err)
	}
	if got := next.Mappings[15].Specials; !slices.Equal(got, []int{15}) {
		t.Errorf("refused refresh kept Mappings[15].Specials = %v, want the stale [15]", got)
	}
	next, err = refresh(t, factsCache(prevFacts), factsBody(t, lands(true)))
	if err != nil {
		t.Fatalf("refresh(placements kept) error = %v, want acceptance", err)
	}
	if got := next.Mappings[15].Specials; !slices.Equal(got, []int{15}) {
		t.Errorf("accepted refresh Mappings[15].Specials = %v, want the document's [15]", got)
	}
}

func TestRefresh_placedEpisodesExtinctionRejected(t *testing.T) {
	prevFacts := map[int]Mapping{}
	for id := 1; id <= 10; id++ {
		prevFacts[id] = Mapping{Episodes: []int{1}, EpisodesTvdb: id, EpisodesSeason: 1}
	}
	lands := func(stated bool) func(int) *[]mapread.Target {
		return func(int) *[]mapread.Target {
			if stated {
				return targets(at(1, 1))
			}
			return targets(dflt(1, 1))
		}
	}
	_, err := refresh(t, factsCache(prevFacts), factsBody(t, lands(false)))
	if stale, ok := errors.AsType[*StaleMapError](err); !ok || !strings.Contains(err.Error(), "season-episode placement") || !attrsContain(stale.LogAttrs(), "stale_reason", "refresh validation failed") {
		t.Fatalf("refresh(every season placement left to a default) error = %v, want the season-episode guard's refusal", err)
	}
	next, err := refresh(t, factsCache(prevFacts), factsBody(t, lands(true)))
	if err != nil || !slices.Equal(next.Mappings[5].Episodes, []int{1}) {
		t.Fatalf("refresh(placements stated) = %+v, %v; want acceptance with Mappings[5].Episodes [1]", next.Mappings[5], err)
	}
}

// specialsBody is specialsDoc encoded, anime 11's episodes landing on one
// target each.
func specialsBody(t *testing.T, lands11 [2]mapread.Target, link bool) []byte {
	t.Helper()
	return encode(t, specialsDoc([2]*[]mapread.Target{targets(lands11[0]), targets(lands11[1])}, link))
}

// specialsDoc is 200 series anime with two regular episodes each, AniList
// and AniDB ids equal; anime 1 to 10 put both on stated specials, and anime
// 11's land by lands11. link adds AniList 1000, anime 1's second episode.
func specialsDoc(lands11 [2]*[]mapread.Target, link bool) *mapread.Document {
	doc := document(nil, nil)
	for id := 1; id <= 200; id++ {
		a := mapread.Anime{AniDBID: id, Type: "TV", TVDBID: id, Last: mapread.Last{Regular: 2}}
		switch {
		case id <= 10:
			a.Episodes = []mapread.Run{regular(1, 2, targets(at(0, 1)))}
		case id == 11:
			a.Episodes = []mapread.Run{regular(1, 1, lands11[0]), regular(2, 2, lands11[1])}
		}
		doc.Anime = append(doc.Anime, a)
		doc.Entries = append(doc.Entries, mapread.Entry{AniListID: id, Type: "TV", Parts: []mapread.Part{{AniDBID: id, Kind: mapread.Regular}}})
	}
	if link {
		doc.Entries = append(doc.Entries, mapread.Entry{AniListID: 1000, Type: "OVA", Parts: []mapread.Part{{AniDBID: 1, Kind: mapread.Regular, Start: 2, End: 2}}})
	}
	return doc
}

// specialsCache is the usable cache specialsBody(stated, true) replaces:
// ten AniDB-keyed special placements, one single special and one
// AniList-keyed special placement.
func specialsCache() *Cache {
	prevFacts := map[int]Mapping{11: {SpecialEpisode: 7, SpecialsTvdb: 11}}
	for id := 1; id <= 10; id++ {
		prevFacts[id] = Mapping{Specials: []int{1, 2}, SpecialsTvdb: id, SpecialsEpisodes: 2}
	}
	prev := factsCache(prevFacts)
	prev.Records = append(prev.Records, Record{AniListID: 1000, Type: "OVA", TvdbID: 1})
	prev.AniListMappings[1000] = Mapping{Specials: []int{2}, SpecialsTvdb: 1, SpecialsEpisodes: 1}
	return prev
}

// TestRefresh_aniListKeyedSpecialsExtinctionRejected pins that the special
// placements are guarded per key space: a document keeping all ten
// AniDB-keyed ones but losing the AniList-keyed one is refused.
func TestRefresh_aniListKeyedSpecialsExtinctionRejected(t *testing.T) {
	stated := [2]mapread.Target{at(0, 7), dflt(0, 7)}
	next, err := refresh(t, specialsCache(), specialsBody(t, stated, false))
	stale, ok := errors.AsType[*StaleMapError](err)
	if !ok || !strings.Contains(err.Error(), "AniList-keyed special placement") || !attrsContain(stale.LogAttrs(), "stale_reason", "refresh validation failed") {
		t.Fatalf("refresh(every AniList-keyed special placement lost) error = %v, want the AniList-keyed special guard's refusal", err)
	}
	if got := next.AniListMappings[1000].Specials; !slices.Equal(got, []int{2}) {
		t.Errorf("refused refresh kept AniListMappings[1000].Specials = %v, want the stale [2]", got)
	}
	next, err = refresh(t, specialsCache(), specialsBody(t, stated, true))
	if err != nil {
		t.Fatalf("refresh(every placement kept) error = %v, want acceptance", err)
	}
	if got := next.AniListMappings[1000].Specials; !slices.Equal(got, []int{2}) {
		t.Errorf("accepted refresh AniListMappings[1000].Specials = %v, want the document's [2]", got)
	}
}

// TestRefresh_singleSpecialExtinctionRejected pins the single-special guard:
// a document keeping every special placement but leaving the one film's
// special to defaults alone loses its series-titled twin, so it is refused;
// one stating the film's every episode there serves it Specials instead, so
// it is accepted.
func TestRefresh_singleSpecialExtinctionRejected(t *testing.T) {
	next, err := refresh(t, specialsCache(), specialsBody(t, [2]mapread.Target{dflt(0, 7), dflt(0, 7)}, true))
	stale, ok := errors.AsType[*StaleMapError](err)
	if !ok || !strings.Contains(err.Error(), "single-special placement") || !attrsContain(stale.LogAttrs(), "stale_reason", "refresh validation failed") {
		t.Fatalf("refresh(every single special lost) error = %v, want the single-special guard's refusal", err)
	}
	if got := next.Mappings[11].SpecialEpisode; got != 7 {
		t.Errorf("refused refresh kept Mappings[11].SpecialEpisode = %d, want the stale 7", got)
	}
	next, err = refresh(t, specialsCache(), specialsBody(t, [2]mapread.Target{at(0, 7), dflt(0, 7)}, true))
	if err != nil {
		t.Fatalf("refresh(single special kept) error = %v, want acceptance", err)
	}
	if got := next.Mappings[11].SpecialEpisode; got != 7 {
		t.Errorf("accepted refresh Mappings[11].SpecialEpisode = %d, want the document's 7", got)
	}
	next, err = refresh(t, specialsCache(), specialsBody(t, [2]mapread.Target{at(0, 7), at(0, 7)}, true))
	if err != nil {
		t.Fatalf("refresh(single special stated as Specials) error = %v, want acceptance", err)
	}
	if got := next.Mappings[11]; !slices.Equal(got.Specials, []int{7}) || got.SpecialsEpisodes != 2 || got.SpecialEpisode != 0 {
		t.Errorf("accepted refresh Mappings[11] = %+v, want Specials [7] for 2 entry episodes and no SpecialEpisode", got)
	}
}

// TestRefresh_fullyStatedEntryKeepsItsFact pins that a previous key whose
// entry the release states in full is no loss: the last single special
// corrected onto positive seasons or onto no TVDB episode is accepted, as is
// the last AniList-keyed special placement corrected to land nowhere, each
// nowhere stated by a listed run's tvdb []. A number no run covers is no
// such statement, since a document that lost its runs reads it the same
// way, so losing runs is refused; so are an anime that lost its TVDB series
// and one with no regular episode.
func TestRefresh_fullyStatedEntryKeepsItsFact(t *testing.T) {
	keep11 := [2]*[]mapread.Target{targets(at(0, 7)), targets(dflt(0, 7))}
	seasonsCache := func() *Cache {
		facts := map[int]Mapping{}
		for id := 1; id <= 200; id++ {
			facts[id] = Mapping{Seasons: []SeasonRange{{Season: 1, First: 1, Last: 1}}}
		}
		return factsCache(facts)
	}
	cases := []struct {
		doc     func() *mapread.Document
		prev    func() *Cache
		check   func(t *testing.T, next Cache)
		name    string
		refused string
	}{
		{
			name: "the single special stated on two positive seasons",
			doc: func() *mapread.Document {
				return specialsDoc([2]*[]mapread.Target{targets(at(1, 1)), targets(at(2, 1))}, true)
			},
			check: func(t *testing.T, next Cache) {
				want := []SeasonRange{{Season: 1, First: 1, Last: 1}, {Season: 2, First: 2, Last: 2}}
				if got := next.Mappings[11]; !slices.Equal(got.Seasons, want) || got.SpecialEpisode != 0 {
					t.Errorf("accepted refresh Mappings[11] = %+v, want Seasons %+v and no SpecialEpisode", got, want)
				}
			},
		},
		{
			name: "the single special landing nowhere",
			doc: func() *mapread.Document {
				doc := specialsDoc(keep11, true)
				nowhere := regular(1, 2, targets())
				nowhere.AniDBEpisodeIDs = []int{1101, 1102}
				doc.Anime[10].Episodes, doc.Anime[10].EpisodesListed = []mapread.Run{nowhere}, true
				return doc
			},
			check: func(t *testing.T, next Cache) {
				if got, ok := next.Mappings[11]; ok {
					t.Errorf("accepted refresh Mappings[11] = %+v, want no facts for an anime on no TVDB episode", got)
				}
			},
		},
		{
			name: "the AniList-keyed special placement landing nowhere",
			doc: func() *mapread.Document {
				doc := specialsDoc(keep11, true)
				nowhere := regular(2, 2, targets())
				nowhere.AniDBEpisodeIDs = []int{102}
				doc.Anime[0].Episodes = []mapread.Run{regular(1, 1, targets(at(0, 1))), nowhere}
				doc.Anime[0].EpisodesListed = true
				return doc
			},
			check: func(t *testing.T, next Cache) {
				if got, ok := next.AniListMappings[1000]; ok {
					t.Errorf("accepted refresh AniListMappings[1000] = %+v, want no facts for a link on no TVDB episode", got)
				}
			},
		},
		{
			name: "the single special's anime losing its TVDB series",
			doc: func() *mapread.Document {
				doc := specialsDoc(keep11, true)
				doc.Anime[10].TVDBID, doc.Anime[10].Episodes = 0, nil
				return doc
			},
			refused: "single-special placement",
		},
		{
			name: "the single special's anime with no regular episode",
			doc: func() *mapread.Document {
				doc := specialsDoc(keep11, true)
				doc.Anime[10].Last.Regular, doc.Anime[10].Episodes = 0, nil
				return doc
			},
			refused: "single-special placement",
		},
		{
			name: "the single special's anime losing its runs",
			doc: func() *mapread.Document {
				doc := specialsDoc(keep11, true)
				doc.Anime[10].Episodes = nil
				return doc
			},
			refused: "single-special placement",
		},
		{
			name: "the single special's listed anime losing its runs",
			doc: func() *mapread.Document {
				doc := specialsDoc(keep11, true)
				doc.Anime[10].Episodes, doc.Anime[10].EpisodesListed = nil, true
				return doc
			},
			refused: "single-special placement",
		},
		{
			name: "ten special placements' anime losing their runs",
			doc: func() *mapread.Document {
				doc := specialsDoc(keep11, true)
				for i := range 10 {
					doc.Anime[i].Episodes = nil
				}
				return doc
			},
			refused: "special placement records went extinct (previous cache carried 10)",
		},
		{
			name: "every anime losing its runs",
			prev: seasonsCache,
			doc: func() *mapread.Document {
				doc := document(nil, nil)
				for id := 1; id <= 200; id++ {
					doc.Anime = append(doc.Anime, mapread.Anime{AniDBID: id, Type: "TV", TVDBID: id, Last: mapread.Last{Regular: 12}})
					doc.Entries = append(doc.Entries, mapread.Entry{AniListID: id, Type: "TV", Parts: []mapread.Part{{AniDBID: id, Kind: mapread.Regular}}})
				}
				return doc
			},
			refused: "season-range",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev := specialsCache
			if tc.prev != nil {
				prev = tc.prev
			}
			next, err := refresh(t, prev(), encode(t, tc.doc()))
			if tc.refused == "" {
				if err != nil {
					t.Fatalf("refresh error = %v, want acceptance", err)
				}
				if next.Tag != testTag {
					t.Errorf("accepted refresh Tag = %q, want %q", next.Tag, testTag)
				}
				tc.check(t, next)
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.refused) {
				t.Fatalf("refresh error = %v, want the %s guard's refusal", err, tc.refused)
			}
			want := prev()
			if !maps.EqualFunc(next.Mappings, want.Mappings, sameMapping) || !maps.EqualFunc(next.AniListMappings, want.AniListMappings, sameMapping) ||
				next.RejectedRefreshes != 1 {
				t.Errorf("refused refresh facts %+v and %+v, streak %d; want the stale cache's and streak 1", next.Mappings, next.AniListMappings, next.RejectedRefreshes)
			}
		})
	}
}
