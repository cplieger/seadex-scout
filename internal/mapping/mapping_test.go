package mapping

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/animap/mapread"
	"github.com/cplieger/slogx/capture"
)

// TestRecord_routes pins the user's rule on the record: a TMDB movie id is a
// Radarr route, a TVDB id a Sonarr route, and both are both.
func TestRecord_routes(t *testing.T) {
	tests := []struct {
		name                  string
		rec                   Record
		radarr, sonarr, route bool
	}{
		{name: "tmdb only", rec: Record{TmdbMovies: []int{4}}, radarr: true, route: true},
		{name: "tvdb only", rec: Record{TvdbID: 100}, sonarr: true, route: true},
		{name: "both", rec: Record{TvdbID: 100, TmdbMovies: []int{4}}, radarr: true, sonarr: true, route: true},
		{name: "neither", rec: Record{AniDBID: 7, SeasonKind: SeasonPresent}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rec.RadarrRoute(); got != tt.radarr {
				t.Errorf("RadarrRoute() = %v, want %v", got, tt.radarr)
			}
			if got := tt.rec.SonarrRoute(); got != tt.sonarr {
				t.Errorf("SonarrRoute() = %v, want %v", got, tt.sonarr)
			}
			if got := tt.rec.HasRoute(); got != tt.route {
				t.Errorf("HasRoute() = %v, want %v", got, tt.route)
			}
		})
	}
}

// TestRecord_SonarrSpecial pins specials to season 0: a record is a special
// only for a TVDB series it lands on season 0 of, whatever its TMDB link.
func TestRecord_SonarrSpecial(t *testing.T) {
	tests := []struct {
		name string
		rec  Record
		want bool
	}{
		{name: "season 0", rec: Record{TvdbID: 1, SeasonKind: SeasonPresent}, want: true},
		{name: "a film in season 0", rec: Record{TvdbID: 1, TmdbMovies: []int{4}, SeasonKind: SeasonPresent}, want: true},
		{name: "a positive season", rec: Record{TvdbID: 1, SeasonKind: SeasonPresent, SeasonTvdb: 2}},
		{name: "the whole series", rec: Record{TvdbID: 1, SeasonKind: SeasonAbsent}},
		{name: "unknown season", rec: Record{TvdbID: 1}},
		{name: "no series", rec: Record{TmdbMovies: []int{4}, SeasonKind: SeasonPresent}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rec.SonarrSpecial(); got != tt.want {
				t.Errorf("SonarrSpecial() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestRecord_HasMappedSeason pins the season predicate to a POSITIVE
// season. Season 0 is what an unmapped record and a mapping-typed special both
// carry, so admitting it would relabel a pack's season half to S00 on the
// strength of a season the upstream never stated, and would count every
// seasonless record toward the season-scoped population.
func TestRecord_HasMappedSeason(t *testing.T) {
	tests := map[int]bool{-1: false, 0: false, 1: true, 12: true}
	for season, want := range tests {
		if got := (&Record{SeasonTvdb: season}).HasMappedSeason(); got != want {
			t.Errorf("Record{SeasonTvdb: %d}.HasMappedSeason() = %v, want %v", season, got, want)
		}
	}
}

// TestRecord_SeasonPresence pins the three-valued season kind: the ZERO value
// reads unknown rather than absent (the one state a Record persisted before the
// field existed can carry, and reading it as absent would strand every
// mapped-zero special served from the persisted cache), an unrecognized
// string reads unknown too rather than failing the record, and the two named
// spellings survive.
func TestRecord_SeasonPresence(t *testing.T) {
	tests := map[SeasonKind]SeasonKind{
		"":            seasonUnknown,
		SeasonPresent: SeasonPresent,
		SeasonAbsent:  SeasonAbsent,
		"mapped":      seasonUnknown,
		"PRESENT":     seasonUnknown,
	}
	for stored, want := range tests {
		if got := (&Record{SeasonKind: stored}).SeasonPresence(); got != want {
			t.Errorf("Record{SeasonKind: %q}.SeasonPresence() = %q, want %q", stored, got, want)
		}
	}
}

// TestNewIndex_canonicalizesSeasonKind pins the kind's normalization at the
// INDEX boundary, which is what lets the scope dispatch read the field directly:
// NewIndex canonicalizes every record on both the animap and the persisted-cache
// path, so an unrecognized string can never reach a consumer as a fourth state,
// while a record's real kind round-trips untouched.
func TestNewIndex_canonicalizesSeasonKind(t *testing.T) {
	idx := NewIndex(Source{Records: []Record{
		{AniListID: 1, TvdbID: 100, SeasonKind: "mapped-positive", SeasonTvdb: 2},
		{AniListID: 2, TvdbID: 200, SeasonKind: SeasonPresent},
		{AniListID: 3, TvdbID: 300, SeasonKind: SeasonAbsent},
		{AniListID: 4, TvdbID: 400},
	}})
	want := map[int]SeasonKind{1: seasonUnknown, 2: SeasonPresent, 3: SeasonAbsent, 4: seasonUnknown}
	for id, kind := range want {
		rec, ok := idx.Lookup(id)
		if !ok {
			t.Fatalf("Lookup(%d) missing", id)
		}
		if rec.SeasonKind != kind {
			t.Errorf("Lookup(%d).SeasonKind = %q, want %q", id, rec.SeasonKind, kind)
		}
	}
	// The garbage kind normalizes without touching the season NUMBER: the pair
	// (unknown, positive season) is legitimate and takes the union arm.
	if rec, _ := idx.Lookup(1); rec.SeasonTvdb != 2 {
		t.Errorf("Lookup(1).SeasonTvdb = %d, want 2 (normalizing the kind must not clamp the season)", rec.SeasonTvdb)
	}
}

// TestRecord_seasonKindRoundTrips pins the persisted json key: it is additive
// and omitempty, so an unknown kind writes nothing while the two named spellings
// survive a Cache write and read.
func TestRecord_seasonKindRoundTrips(t *testing.T) {
	for _, kind := range []SeasonKind{seasonUnknown, SeasonPresent, SeasonAbsent} {
		encoded, err := json.Marshal(Record{AniListID: 7, SeasonKind: kind})
		if err != nil {
			t.Fatalf("Marshal(%q) error: %v", kind, err)
		}
		if kind == seasonUnknown && strings.Contains(string(encoded), "season_kind") {
			t.Errorf("Marshal(unknown) = %s, want no season_kind key (omitempty)", encoded)
		}
		var back Record
		if err := json.Unmarshal(encoded, &back); err != nil {
			t.Fatalf("Unmarshal(%s) error: %v", encoded, err)
		}
		if back.SeasonKind != kind {
			t.Errorf("round trip of %q = %q", kind, back.SeasonKind)
		}
	}
}

// TestParseOverrides_seasonKind pins the override door season-kind needs: without
// the key an operator cannot place an entry at all, since a hand-written override
// would decode to unknown forever. A mapped zero is what says "this
// title is filed under its parent's specials", and an unrecognized value reads
// unknown while still counting as a RECOGNIZED key.
func TestParseOverrides_seasonKind(t *testing.T) {
	set, err := parseOverrides([]byte(`[
		{"anilist_id":5,"tvdb_id":100,"season_kind":"present","season_tvdb":0},
		{"anilist_id":6,"tvdb_id":200,"season_kind":"absent"},
		{"anilist_id":7,"tvdb_id":300,"season_kind":"maybe"}
	]`))
	if err != nil {
		t.Fatalf("parseOverrides error: %v", err)
	}
	if len(set.records) != 3 {
		t.Fatalf("records = %d, want 3", len(set.records))
	}
	want := []SeasonKind{SeasonPresent, SeasonAbsent, seasonUnknown}
	for i, kind := range want {
		if set.records[i].SeasonKind != kind {
			t.Errorf("records[%d].SeasonKind = %q, want %q", i, set.records[i].SeasonKind, kind)
		}
	}
	if set.records[0].SeasonTvdb != 0 {
		t.Errorf("records[0].SeasonTvdb = %d, want 0 (a mapped zero is the whole point of the key)", set.records[0].SeasonTvdb)
	}
	if set.unknown != 0 {
		t.Errorf("unknown keys = %d, want none (season_kind is canonical; an odd VALUE is not an odd KEY)", set.unknown)
	}
}

// TestParseOverrides_anidbID pins the override door for the map-fact join
// key: an operator names WHICH AniDB anime an entry is (the episode facts
// stay animap's), the key survives canonicalize, a negative value clamps to absent,
// and the key is recognized rather than counted unknown.
func TestParseOverrides_anidbID(t *testing.T) {
	set, err := parseOverrides([]byte(`[
		{"anilist_id":5,"anidb_id":12276},
		{"anilist_id":6,"tvdb_id":200,"anidb_id":-3}
	]`))
	if err != nil {
		t.Fatalf("parseOverrides error: %v", err)
	}
	if len(set.records) != 2 {
		t.Fatalf("records = %d, want 2", len(set.records))
	}
	if got := set.records[0].AniDBID; got != 12276 {
		t.Errorf("records[0].AniDBID = %d, want 12276", got)
	}
	if got := set.records[1].AniDBID; got != 0 {
		t.Errorf("records[1].AniDBID = %d, want 0 (a negative id clamps to absent)", got)
	}
	if set.unknown != 0 {
		t.Errorf("unknown keys = %d, want none (anidb_id is canonical)", set.unknown)
	}
}

// TestRoutedCountCountsEitherRoute pins the coverage floor's count: a record
// with either route counts once, a both-routes record included.
func TestRoutedCountCountsEitherRoute(t *testing.T) {
	records := []Record{
		{AniListID: 1, TmdbMovies: []int{4}},
		{AniListID: 2, TvdbID: 100},
		{AniListID: 3, TvdbID: 100, TmdbMovies: []int{5}},
		{AniListID: 4},
	}
	if got := routedCount(records); got != 3 {
		t.Errorf("routedCount = %d, want 3", got)
	}
}

func TestIndex_nilSafe(t *testing.T) {
	var idx *Index
	if _, ok := idx.Lookup(1); ok {
		t.Error("nil Index Lookup returned ok=true")
	}
	if idx.Len() != 0 {
		t.Error("nil Index Len != 0")
	}
	called := false
	idx.ForEachRecord(func(Record) { called = true })
	if called {
		t.Error("nil Index ForEachRecord invoked fn")
	}
	if _, ok := idx.MappingFor(&Record{AniDBID: 1}); ok {
		t.Error("nil Index MappingFor returned ok=true")
	}
}

// TestIndex_MappingFor pins the AniDB join: a record with
// no AniDB id never joins (0 is absent, not a key), an id the map lacks is
// false, a known id returns the stored value, and the served map is the one
// handed to the index rather than anything derived from Record.
func TestIndex_MappingFor(t *testing.T) {
	want := Mapping{Specials: []int{8}, SpecialsTvdb: 79151, SpecialsEpisodes: 1, Seasons: []SeasonRange{{Season: 1, First: 1, Last: 13}}, SeasonsTvdb: 79151}
	idx := NewIndex(Source{
		Records:  []Record{{AniListID: 1, AniDBID: 12276, TvdbID: 79151}, {AniListID: 2, AniDBID: 999}, {AniListID: 3}},
		Mappings: map[int]Mapping{12276: want, 0: {Seasons: []SeasonRange{{Season: 9, First: 1, Last: 1}}}},
	})
	tests := []struct {
		name    string
		rec     *Record
		want    Mapping
		wantHit bool
	}{
		{name: "nil_record", rec: nil},
		{name: "no_anidb_id_never_joins_the_zero_key", rec: &Record{AniListID: 3}},
		{name: "id_the_map_lacks", rec: &Record{AniListID: 2, AniDBID: 999}},
		{name: "known_id", rec: &Record{AniListID: 1, AniDBID: 12276, TvdbID: 79151}, want: want, wantHit: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := idx.MappingFor(tc.rec)
			if ok != tc.wantHit {
				t.Fatalf("MappingFor(%+v) ok = %v, want %v", tc.rec, ok, tc.wantHit)
			}
			if !sameMapping(got, tc.want) {
				t.Errorf("MappingFor(%+v) = %+v, want %+v", tc.rec, got, tc.want)
			}
		})
	}
	if _, ok := NewIndex(Source{Records: []Record{{AniListID: 1, AniDBID: 12276}}}).MappingFor(&Record{AniDBID: 12276}); ok {
		t.Error("NewIndex (no facts) MappingFor returned ok=true, want false")
	}
}

func TestIndex_MappingForKeepsThePlacementOnItsSeries(t *testing.T) {
	idx := NewIndex(Source{Mappings: map[int]Mapping{
		70: {Specials: []int{9}, SpecialsTvdb: 500, SpecialsEpisodes: 1},
		71: {Specials: []int{3}, SpecialsTvdb: 500, SpecialsEpisodes: 1},
	}})
	for _, tc := range []struct {
		rec          Record
		wantSpecials []int
		wantCount    int
		wantHit      bool
	}{
		{rec: Record{AniDBID: 70, TvdbID: 500}, wantSpecials: []int{9}, wantCount: 1, wantHit: true},
		{rec: Record{AniDBID: 70, TvdbID: 600}},
		{rec: Record{AniDBID: 71, TvdbID: 600}},
	} {
		got, ok := idx.MappingFor(&tc.rec)
		if ok != tc.wantHit || !slices.Equal(got.Specials, tc.wantSpecials) || got.SpecialsEpisodes != tc.wantCount ||
			(!tc.wantHit && got.SpecialsTvdb != 0) {
			t.Errorf("MappingFor(anidb %d, tvdb %d) = %+v, %v, want specials %v (count %d), %v",
				tc.rec.AniDBID, tc.rec.TvdbID, got, ok, tc.wantSpecials, tc.wantCount, tc.wantHit)
		}
	}
}

// TestIndex_MappingForKeepsSeasonRangesOnTheirSeries pins that season
// ranges reach a record only on the series they number, through either
// join key, whether or not an upstream record is the whole anime.
func TestIndex_MappingForKeepsSeasonRangesOnTheirSeries(t *testing.T) {
	ranges := []SeasonRange{{Season: 2, First: 1, Last: 12}}
	idx := NewIndex(Source{
		Records:         []Record{{AniListID: 1, AniDBID: 80, TvdbID: 500}},
		Mappings:        map[int]Mapping{80: {Seasons: ranges, SeasonsTvdb: 500}, 82: {Seasons: ranges, SeasonsTvdb: 500}},
		AniListMappings: map[int]Mapping{7: {Seasons: ranges, SeasonsTvdb: 500}},
	})
	for _, tc := range []struct {
		name string
		want []SeasonRange
		rec  Record
	}{
		{name: "its record's series", rec: Record{AniListID: 1, AniDBID: 80, TvdbID: 500}, want: ranges},
		{name: "an override moving the record", rec: Record{AniListID: 1, AniDBID: 80, TvdbID: 600}},
		{name: "an anime no record is the whole of, on its series", rec: Record{AniListID: 9, AniDBID: 82, TvdbID: 500}, want: ranges},
		{name: "an anime no record is the whole of, on another series", rec: Record{AniListID: 9, AniDBID: 82, TvdbID: 600}},
		{name: "an AniList-keyed entry on its series", rec: Record{AniListID: 7, TvdbID: 500}, want: ranges},
		{name: "an AniList-keyed entry on another series", rec: Record{AniListID: 7, TvdbID: 600}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := idx.MappingFor(&tc.rec)
			wantSeries := 0
			if tc.want != nil {
				wantSeries = 500
			}
			if !slices.Equal(got.Seasons, tc.want) || got.SeasonsTvdb != wantSeries || ok != (tc.want != nil) {
				t.Errorf("MappingFor(%+v) = %+v ok=%v, want seasons %+v on %d", tc.rec, got, ok, tc.want, wantSeries)
			}
		})
	}
}

// TestIndex_MappingForJoinsARecordWithoutAniDBOnItsAniListID pins the second
// join: a record with no AniDB id reads the facts kept under its AniList id,
// and a record with an AniDB id never does, whatever its AniList id.
func TestIndex_MappingForJoinsARecordWithoutAniDBOnItsAniListID(t *testing.T) {
	link := Mapping{Specials: []int{8}, SpecialsTvdb: 303073, SpecialsEpisodes: 1}
	idx := NewIndex(Source{Records: []Record{{AniListID: 21777}}, AniListMappings: map[int]Mapping{21777: link}})
	if got, ok := idx.MappingFor(&Record{AniListID: 21777, TvdbID: 303073}); !ok || !sameMapping(got, link) {
		t.Errorf("MappingFor(no AniDB id, AniList 21777) = %+v ok=%v, want %+v", got, ok, link)
	}
	if got, ok := idx.MappingFor(&Record{AniListID: 21777, AniDBID: 5, TvdbID: 303073}); ok {
		t.Errorf("MappingFor(AniDB 5, AniList 21777) = %+v ok=true, want the AniDB join only", got)
	}
}

// MappingFor keeps a record's positive-season episodes only on the series
// and season they number: an override moving the record drops them.
func TestIndex_MappingForDropsEpisodesOfAnotherSeriesOrSeason(t *testing.T) {
	facts := Mapping{Episodes: []int{1}, EpisodesTvdb: 82099, EpisodesSeason: 1}
	idx := NewIndex(Source{AniListMappings: map[int]Mapping{2593: facts}, Mappings: map[int]Mapping{4932: facts}})
	for _, tc := range []struct {
		rec  Record
		name string
		want []int
	}{
		{Record{AniListID: 2593, TvdbID: 82099, SeasonTvdb: 1}, "its own season, AniList join", []int{1}},
		{Record{AniListID: 1, AniDBID: 4932, TvdbID: 82099, SeasonTvdb: 1}, "its own season, AniDB join", []int{1}},
		{Record{AniListID: 2593, TvdbID: 82099, SeasonTvdb: 2}, "another season", nil},
		{Record{AniListID: 2593, TvdbID: 5, SeasonTvdb: 1}, "another series", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := idx.MappingFor(&tc.rec)
			if !slices.Equal(got.Episodes, tc.want) || ok != (tc.want != nil) {
				t.Errorf("MappingFor(%+v) = %+v ok=%v, want episodes %v", tc.rec, got, ok, tc.want)
			}
			if tc.want == nil && (got.EpisodesTvdb != 0 || got.EpisodesSeason != 0) {
				t.Errorf("MappingFor(%+v) kept the dropped episodes' series and season: %+v", tc.rec, got)
			}
		})
	}
}

// TestLoader_Load_overrideAniDBIDJoinsThatAnime pins the override rule: an
// override that names an anidb_id joins the facts of that anime, one no
// AniList entry names included, so the operator supplies the key and never
// the facts.
func TestLoader_Load_overrideAniDBIDJoinsThatAnime(t *testing.T) {
	overrides := filepath.Join(t.TempDir(), "overrides.json")
	if err := os.WriteFile(overrides, []byte(`[{"anilist_id":42,"tmdb_movies":[7],"tvdb_id":100,"anidb_id":12276}]`), 0o600); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	doc := document([]mapread.Anime{
		{AniDBID: 42, Type: "TV", TVDBID: 100, Last: mapread.Last{Regular: 1}},
		{AniDBID: 12276, Type: "MOVIE", TVDBID: 100, Last: mapread.Last{Regular: 1}, Episodes: []mapread.Run{regular(1, 1, targets(at(0, 8)))}},
	}, []mapread.Entry{{AniListID: 42, Type: "TV", Parts: []mapread.Part{{AniDBID: 42, Kind: mapread.Regular}}}})
	rel := newRelease(t, testTag, encode(t, doc))
	_, idx, err := rel.loader(WithOverridesPath(overrides)).Load(t.Context(), nil)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	rec, ok := idx.Lookup(42)
	if !ok || rec.AniDBID != 12276 || !rec.RadarrRoute() {
		t.Fatalf("Lookup(42) = %+v ok=%v, want the override record carrying anidb_id 12276", rec, ok)
	}
	if m, ok := idx.MappingFor(&rec); !ok || !slices.Equal(m.Specials, []int{8}) || m.SpecialsEpisodes != 1 {
		t.Errorf("MappingFor(override) = %+v ok=%v, want the anime's S00E08", m, ok)
	}
}

// TestLoader_Load_overrideReplacesAnAniListKeyedRecordsFacts pins that the
// facts a record with no AniDB id carries under its AniList id go with the
// record an override replaces: the override reaches facts only through an
// anidb_id it names, exactly as for every other record.
func TestLoader_Load_overrideReplacesAnAniListKeyedRecordsFacts(t *testing.T) {
	doc := document([]mapread.Anime{
		{AniDBID: 2, Type: "TV", TVDBID: 200, Last: mapread.Last{Regular: 1}},
		{
			AniDBID: 11500, Type: "TV", TVDBID: 303073, Last: mapread.Last{Regular: 1, Special: 1},
			Episodes: []mapread.Run{{Kind: mapread.Special, Start: 1, End: 1, TVDB: targets(at(0, 8))}},
		},
	}, []mapread.Entry{
		{AniListID: 2, Type: "TV", Parts: []mapread.Part{{AniDBID: 2, Kind: mapread.Regular}}},
		{AniListID: 21777, Type: "SPECIAL", Parts: []mapread.Part{{AniDBID: 11500, Kind: mapread.Special, Start: 1, End: 1}}},
	})
	rel := newRelease(t, testTag, encode(t, doc))
	cache, idx, err := rel.loader().Load(t.Context(), nil)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	rec, _ := idx.Lookup(21777)
	if m, ok := idx.MappingFor(&rec); !ok || !slices.Equal(m.Specials, []int{8}) {
		t.Fatalf("MappingFor(21777) = %+v ok=%v, want the link's own S00E08", m, ok)
	}

	overrides := filepath.Join(t.TempDir(), "overrides.json")
	if err := os.WriteFile(overrides, []byte(`[{"anilist_id":21777,"type":"special","tvdb_id":303073,"season_tvdb":0,"season_kind":"present"}]`), 0o600); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	cache.FetchedAt = time.Now()
	_, idx, err = unused(WithOverridesPath(overrides), WithRefresh(time.Hour)).Load(t.Context(), &cache)
	if err != nil {
		t.Fatalf("Load(with override) error: %v", err)
	}
	rec, _ = idx.Lookup(21777)
	if m, ok := idx.MappingFor(&rec); ok {
		t.Errorf("MappingFor(overridden 21777) = %+v ok=true, want no facts: the override names no anidb_id", m)
	}
	if len(cache.AniListMappings) != 1 {
		t.Errorf("the override deleted %d persisted AniList-keyed facts, want the cache left intact", 1-len(cache.AniListMappings))
	}
}

func TestIndex_ForEachRecordAndNewIndex(t *testing.T) {
	idx := NewIndex(Source{Records: []Record{{AniListID: 1}, {AniListID: 2}}})
	var got []int
	idx.ForEachRecord(func(r Record) { got = append(got, r.AniListID) })
	slices.Sort(got)
	if !slices.Equal(got, []int{1, 2}) {
		t.Errorf("ForEachRecord visited %v, want [1 2]", got)
	}
}

func TestParseOverrides(t *testing.T) {
	set, err := parseOverrides([]byte(`[{"anilist_id":5,"tmdb_movies":[0,42,-5]}]`))
	if err != nil {
		t.Fatalf("parseOverrides error: %v", err)
	}
	if len(set.records) != 1 || set.records[0].AniListID != 5 {
		t.Fatalf("parseOverrides = %+v, want one record for AniList 5", set.records)
	}
	// TMDB movie ids likewise: non-positive entries are dropped to match the
	// canonical form the animap decoder guarantees, so an
	// override cannot introduce a phantom zero/negative lookup key.
	if got := set.records[0].TmdbMovies; !slices.Equal(got, []int{42}) {
		t.Errorf("TmdbMovies = %v, want [42] (non-positive entries dropped)", got)
	}
	if set.unknown != 0 {
		t.Errorf("unknown keys = %d, want none for a well-formed override", set.unknown)
	}
	if _, err := parseOverrides([]byte(`{bad`)); err == nil {
		t.Error("parseOverrides(malformed) = nil error, want error")
	}
	if _, err := parseOverrides([]byte(`null`)); err == nil {
		t.Error("parseOverrides(null) = nil error, want error (a non-array top level must not read as an empty overlay)")
	}
	if _, err := parseOverrides([]byte(`[] trailing`)); err == nil {
		t.Error("parseOverrides(trailing data) = nil error, want error (json.Unmarshal parity)")
	}
}

// TestParseOverridesReportsUnknownKeys pins the unknown-key detection: an
// operator writing upstream-style field names (imdb_id, themoviedb_id,
// season) instead of the override names gets them counted while the records
// still parse.
func TestParseOverridesReportsUnknownKeys(t *testing.T) {
	data := []byte(`[{"anilist_id":5,"imdb_id":"tt1","season":1},{"anilist_id":6,"imdb_id":"tt2","themoviedb_id":9}]`)
	set, err := parseOverrides(data)
	if err != nil {
		t.Fatalf("parseOverrides error: %v", err)
	}
	if len(set.records) != 2 {
		t.Errorf("records = %d, want 2 (unknown keys do not reject the record)", len(set.records))
	}
	if set.unknown != 4 {
		t.Errorf("unknown keys = %d, want 4 (every non-canonical key counted)", set.unknown)
	}
}

// TestParseOverridesAcceptsCaseVariantKeys pins the diagnostic's key matching
// to encoding/json's: a case-variant canonical key (e.g. "ANILIST_ID", "TVDB_ID")
// is decoded and applied by the typed unmarshal, so it must not be reported as
// unknown and "ignored" - that would tell the operator an accepted field was
// discarded.
func TestParseOverridesAcceptsCaseVariantKeys(t *testing.T) {
	set, err := parseOverrides([]byte(`[{"ANILIST_ID":5,"TVDB_ID":9,"Season_Kind":"absent"}]`))
	if err != nil {
		t.Fatalf("parseOverrides error: %v", err)
	}
	if len(set.records) != 1 || set.records[0].AniListID != 5 || set.records[0].TvdbID != 9 || set.records[0].SeasonKind != SeasonAbsent {
		t.Errorf("parseOverrides = %+v, want one record with AniListID 5, TVDB 9 and an absent season", set.records)
	}
	if set.unknown != 0 {
		t.Errorf("unknown keys = %d, want none for case-variant canonical keys (encoding/json accepts them)", set.unknown)
	}
}

// TestNewIndex_ignoresZeroAndKeepsLastDuplicate pins the public NewIndex
// contract consumers rely on: non-positive AniList IDs are omitted
// (unkeyable; real AniList IDs are positive) and the last duplicate wins, so
// upstream ordering cannot silently retain a stale record.
func TestNewIndex_ignoresZeroAndKeepsLastDuplicate(t *testing.T) {
	idx := NewIndex(Source{Records: []Record{
		{AniListID: 0, TvdbID: 99},
		{AniListID: -7, TvdbID: 77},
		{AniListID: 42, TvdbID: 100},
		{AniListID: 42, TvdbID: 200},
	}})

	if got := idx.Len(); got != 1 {
		t.Errorf("NewIndex length = %d, want 1", got)
	}
	got, ok := idx.Lookup(42)
	if !ok {
		t.Fatal("NewIndex lookup 42 missing")
	}
	if got.TvdbID != 200 {
		t.Errorf("NewIndex duplicate TVDB ID = %d, want last value 200", got.TvdbID)
	}
	if _, ok := idx.Lookup(0); ok {
		t.Error("NewIndex retained zero AniList ID")
	}
	if _, ok := idx.Lookup(-7); ok {
		t.Error("NewIndex retained negative AniList ID")
	}
}

// TestParseOverrides_duplicateIDKeepsLastRecord pins the effective set's
// duplicate rule: a repeated AniList ID replaces its earlier record during the
// stream, so the overlay is deduplicated last-record-wins while applied still
// counts every keyed transport row.
func TestParseOverrides_duplicateIDKeepsLastRecord(t *testing.T) {
	set, err := parseOverrides([]byte(`[
		{"anilist_id":1,"tvdb_id":10},
		{"anilist_id":1,"tvdb_id":11},
		{"anilist_id":1,"tvdb_id":12},
		{"anilist_id":2,"tvdb_id":20},
		{"anilist_id":2,"tvdb_id":21}
	]`))
	if err != nil {
		t.Fatalf("parseOverrides error: %v", err)
	}
	if set.applied != 5 {
		t.Errorf("applied = %d, want 5 (every keyed record applies)", set.applied)
	}
	if len(set.records) != 2 {
		t.Errorf("effective records = %d, want 2 (deduplicated during the stream)", len(set.records))
	}
	idx := NewIndex(Source{Records: set.records})
	if got, ok := idx.Lookup(1); !ok || got.TvdbID != 12 {
		t.Errorf("Lookup(1) = %+v, %v, want last record with TvdbID 12", got, ok)
	}
	if got, ok := idx.Lookup(2); !ok || got.TvdbID != 21 {
		t.Errorf("Lookup(2) = %+v, %v, want last record with TvdbID 21", got, ok)
	}
}

// TestParseOverrides_discardsSemanticallyEmptyRowsDuringStream pins the
// memory-amplification guard: a valid compact array of empty objects fits under
// maxOverrideBytes, so a parser materializing it whole allocates the array three
// times over before discarding every row. A large all-empty-object array must
// parse to an EMPTY effective overlay with the exact skipped count, allocating no
// []Record growth per transport row.
func TestParseOverrides_discardsSemanticallyEmptyRowsDuringStream(t *testing.T) {
	const rows = 100_000
	data := []byte("[" + strings.Repeat("{},", rows-1) + "{}]")
	set, err := parseOverrides(data)
	if err != nil {
		t.Fatalf("parseOverrides error: %v", err)
	}
	if len(set.records) != 0 || cap(set.records) != 0 {
		t.Errorf("effective records len=%d cap=%d, want 0/0 (zero-ID rows discarded during the stream)", len(set.records), cap(set.records))
	}
	if set.skipped != rows {
		t.Errorf("skipped = %d, want the exact discarded row count %d", set.skipped, rows)
	}
	if set.applied != 0 || set.unknown != 0 {
		t.Errorf("applied=%d unknown=%d, want both zero", set.applied, set.unknown)
	}
}

// TestIndex_SiblingSeasons pins the per-tvdb summary the whole-series aggregate
// consumes. "A SIBLING maps this season" has to be exact rather than "any record
// maps it", which is why the accumulator counts holders: a record must not report
// its OWN season back when it is that season's only holder, but must when a
// cour-split sibling shares it (Fire Force season 3 spans two AniList entries in
// one TVDB season).
func TestIndex_SiblingSeasons(t *testing.T) {
	idx := NewIndex(Source{Records: []Record{
		// Gintama's shape: one seasonless entry plus season-scoped siblings.
		{AniListID: 918, TvdbID: 79895, SeasonKind: SeasonAbsent},
		{AniListID: 100, TvdbID: 79895, SeasonKind: SeasonPresent, SeasonTvdb: 5},
		{AniListID: 101, TvdbID: 79895, SeasonKind: SeasonPresent, SeasonTvdb: 6},
		// A cour split: two entries on one season.
		{AniListID: 200, TvdbID: 12345, SeasonKind: SeasonPresent, SeasonTvdb: 3},
		{AniListID: 201, TvdbID: 12345, SeasonKind: SeasonPresent, SeasonTvdb: 3},
		// A sole holder of its season, on its own tvdb id.
		{AniListID: 300, TvdbID: 6789, SeasonKind: SeasonPresent, SeasonTvdb: 1},
		// No tvdb id at all.
		{AniListID: 400, TmdbMovies: []int{7}},
		// Season-scoped records that carry no tvdb id: they share no series.
		{AniListID: 500, SeasonKind: SeasonPresent, SeasonTvdb: 1},
		{AniListID: 501, SeasonKind: SeasonPresent, SeasonTvdb: 2},
	}})
	tests := []struct {
		name string
		id   int
		want []int
	}{
		{name: "a seasonless record reports every sibling season", id: 918, want: []int{5, 6}},
		{name: "a season-scoped record does not report its own sole season", id: 100, want: []int{6}},
		{name: "a cour-split record DOES report its shared season", id: 200, want: []int{3}},
		{name: "a sole holder on its own tvdb id reports nothing", id: 300},
		{name: "a record with no tvdb id reports nil", id: 400},
		{name: "a season-scoped record with no tvdb id reports nil beside others like it", id: 500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, ok := idx.Lookup(tt.id)
			if !ok {
				t.Fatalf("Lookup(%d) missing", tt.id)
			}
			if got := idx.SiblingSeasons(&rec); !slices.Equal(got, tt.want) {
				t.Errorf("SiblingSeasons(%d) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
	var nilIndex *Index
	if got := nilIndex.SiblingSeasons(&Record{TvdbID: 1}); got != nil {
		t.Errorf("nil index SiblingSeasons = %v, want nil", got)
	}
}

// TestNewIndexCanonicalizesRecords pins the boundary that OWNS the id-usability
// rule: the accessors do not filter on read, so a record reaching NewIndex
// through plain encoding/json - the persisted-cache and stale-on-error paths -
// must be canonicalized on insertion, which is what
// makes the route predicates' presence check sound.
func TestNewIndexCanonicalizesRecords(t *testing.T) {
	idx := NewIndex(Source{Records: []Record{{
		AniListID:  7,
		TvdbID:     -3,
		SeasonTvdb: -1,
		TmdbMovies: []int{0, -1, 42},
	}}})
	rec, ok := idx.Lookup(7)
	if !ok {
		t.Fatal("Lookup(7) = not found, want the indexed record")
	}
	if !slices.Equal(rec.TmdbMovies, []int{42}) {
		t.Errorf("TmdbMovies = %v, want [42] (canonicalized on insertion)", rec.TmdbMovies)
	}
	if rec.TvdbID != 0 || rec.SeasonTvdb != 0 {
		t.Errorf("TvdbID/SeasonTvdb = %d/%d, want 0/0 (canonicalized on insertion)", rec.TvdbID, rec.SeasonTvdb)
	}
	if !rec.RadarrRoute() || rec.SonarrRoute() {
		t.Errorf("routes = Radarr %v, Sonarr %v, want Radarr only over the canonical record", rec.RadarrRoute(), rec.SonarrRoute())
	}
}

// TestRefresh_staleReasonClassVocabulary pins stale_reason as the
// fixed-cardinality degradation CLASS the operator queries in Loki for the
// classes no other test names: the acceptance floor and an unchanged refused
// release. Each refusal class mapread names is pinned beside its arm in
// TestLoader_refreshCache_refusalsKeepTheCacheAndAdvanceTheStreak.
func TestRefresh_staleReasonClassVocabulary(t *testing.T) {
	t.Run("validation floor", func(t *testing.T) {
		_, err := refresh(t, staleCache(""), entriesBody(t, entry{AniList: 1, Type: "tv"}))
		if stale, ok := errors.AsType[*StaleMapError](err); !ok || !attrsContain(stale.LogAttrs(), "stale_reason", "refresh validation failed") {
			t.Errorf("refresh error = %v, want stale_reason \"refresh validation failed\"", err)
		}
	})
	t.Run("refused release unchanged", func(t *testing.T) {
		prev := staleCache("v2026.10.16")
		prev.RefusedTag = testTag
		_, err := newRelease(t, testTag, nil).loader().refreshCache(t.Context(), prev)
		if stale, ok := errors.AsType[*StaleMapError](err); !ok || !attrsContain(stale.LogAttrs(), "stale_reason", "refused body unchanged") {
			t.Errorf("refresh error = %v, want stale_reason \"refused body unchanged\"", err)
		}
	})
}

// approachingSizeCapMessage is the fixed WARN message a refresh emits before
// the download size cap becomes a hard refusal. It is a Loki-queryable log
// contract (the class is the message, the facts ride as structured fields),
// so the test pins it verbatim.
const approachingSizeCapMessage = "mapping: animap body approaching the download size cap; " +
	"a body past it refuses every refresh and freezes the map stale"

// TestRefresh_approachingDownloadSizeCapWarns pins the operator's only
// advance notice before maxMapBytes becomes a permanent refusal: a release
// whose body reaches the threshold warns once (the shared guard is
// inclusive); one byte below it stays silent.
func TestRefresh_approachingDownloadSizeCapWarns(t *testing.T) {
	// The threshold is hardcoded rather than recomputed from the shared fraction:
	// a fixture derived from the expression under test moves with it.
	// 26843544 is maxMapBytes/10*8 (32 MiB), truncated down.
	const warnThresholdBytes = 26_843_544
	doc := entriesBody(t, seriesEntries(1)...)
	padded := func(total int) []byte {
		return append(doc, bytes.Repeat([]byte(" "), total-len(doc))...)
	}
	for name, tc := range map[string]struct {
		total int
		warns int
	}{
		"at the threshold":  {total: warnThresholdBytes, warns: 1},
		"one byte below it": {total: warnThresholdBytes - 1},
	} {
		t.Run(name, func(t *testing.T) {
			rel := newRelease(t, testTag, padded(tc.total))
			logger, rec := capture.New()
			next, err := NewLoader(rel.srv.Client(), rel.source(), WithLogger(logger)).refreshCache(t.Context(), staleCache(""))
			if err != nil || next.Tag != testTag {
				t.Fatalf("refresh = %+v, %v, want the padded release accepted", next, err)
			}
			if n := rec.CountLevel(slog.LevelWarn, approachingSizeCapMessage); n != tc.warns {
				t.Fatalf("%d bytes warned %d times at WARN, want %d: %v", tc.total, n, tc.warns, rec.Messages())
			}
			if tc.warns > 0 && (!rec.HasAttr(approachingSizeCapMessage, "bytes", strconv.Itoa(tc.total)) ||
				!rec.HasAttr(approachingSizeCapMessage, "cap", strconv.Itoa(maxMapBytes))) {
				t.Errorf("approaching-cap log = %v, want bytes=%d and cap=%d", rec.Messages(), tc.total, maxMapBytes)
			}
		})
	}
}

// approachingProjectionCapMessage is the fixed WARN message a refresh emits
// before the projection budget becomes a hard refusal.
const approachingProjectionCapMessage = "mapping: animap anime episodes approaching the projection cap; " +
	"a document past it refuses every refresh and freezes the map stale"

// TestRefresh_approachingProjectionCapWarns pins the advance notice before
// maxAnimeEpisodes refuses every refresh: a document whose anime on TVDB
// place the threshold's regular episodes warns once; one fewer stays silent.
func TestRefresh_approachingProjectionCapWarns(t *testing.T) {
	// 1677720 is maxAnimeEpisodes/10*8 (2^21), truncated down.
	const warnThreshold = 1_677_720
	for name, tc := range map[string]struct {
		episodes int
		warns    int
	}{
		"at the threshold":     {episodes: warnThreshold, warns: 1},
		"one episode below it": {episodes: warnThreshold - 1},
	} {
		t.Run(name, func(t *testing.T) {
			anime := []mapread.Anime{{AniDBID: 1, Type: "TV", TVDBID: 1, Last: mapread.Last{Regular: 1}, Episodes: []mapread.Run{regular(1, 1, targets(at(1, 1)))}}}
			for aid, left := 2, tc.episodes-1; left > 0; aid++ {
				n := min(left, 1<<20)
				anime = append(anime, mapread.Anime{AniDBID: aid, Type: "TV", TVDBID: aid, Last: mapread.Last{Regular: n}})
				left -= n
			}
			doc := document(anime, []mapread.Entry{{AniListID: 1, Type: "TV", Parts: []mapread.Part{{AniDBID: 1, Kind: mapread.Regular}}}})
			logger, rec := capture.New()
			rel := newRelease(t, testTag, encode(t, doc))
			next, err := NewLoader(rel.srv.Client(), rel.source(), WithLogger(logger)).refreshCache(t.Context(), staleCache(""))
			if err != nil || next.Tag != testTag {
				t.Fatalf("refresh = %+v, %v, want the document accepted", next, err)
			}
			if n := rec.CountLevel(slog.LevelWarn, approachingProjectionCapMessage); n != tc.warns {
				t.Fatalf("%d episodes warned %d times at WARN, want %d: %v", tc.episodes, n, tc.warns, rec.Messages())
			}
			if tc.warns > 0 && (!rec.HasAttr(approachingProjectionCapMessage, "episodes", strconv.Itoa(tc.episodes)) ||
				!rec.HasAttr(approachingProjectionCapMessage, "cap", strconv.Itoa(maxAnimeEpisodes))) {
				t.Errorf("approaching-cap log = %v, want episodes=%d and cap=%d", rec.Messages(), tc.episodes, maxAnimeEpisodes)
			}
		})
	}
}

// TestRefresh_logsTheCandidateCensus pins the population census the
// accepted-refresh line carries. Each guard refuses only a below-half
// collapse, so a shallower routing or type loss is accepted, and these counts
// are the ONLY operator-visible record that it happened. The entries'
// populations are all distinct, so a count attributed to the wrong one shows.
func TestRefresh_logsTheCandidateCensus(t *testing.T) {
	doc, err := mapread.Decode(bytes.NewReader(entriesBody(t,
		entry{AniList: 10, Type: "tv", TVDB: 200},
		entry{AniList: 11, Type: "movie", Movies: []int{5}},
		entry{AniList: 12, Type: "ova", TVDB: 300, Lands: targets(at(2, 1))},
		entry{AniList: 13},
		entry{AniList: 14, Type: "tv", TVDB: 400, Lands: targets(at(0, 3)), Movies: []int{6}},
		entry{AniList: 15, Type: "tv", TVDB: 410, Lands: targets(at(0, 4))},
	)), mapread.DefaultLimits())
	if err != nil {
		t.Fatalf("decoding the census fixture: %v", err)
	}
	// AniList 16's two episodes land on S00E7, the first by a row and the
	// second by default: a single special and no placed specials.
	doc.Anime = append(doc.Anime, mapread.Anime{
		AniDBID: 16, Type: "TV", TVDBID: 500, Last: mapread.Last{Regular: 2},
		Episodes: []mapread.Run{regular(1, 1, targets(at(0, 7))), regular(2, 2, targets(dflt(0, 7)))},
	})
	doc.Entries = append(doc.Entries, mapread.Entry{AniListID: 16, Type: "TV", Parts: []mapread.Part{{AniDBID: 16, Kind: mapread.Regular}}})
	body := encode(t, document(doc.Anime, doc.Entries))
	logger, rec := capture.New()
	rel := newRelease(t, testTag, body)
	if _, err := NewLoader(rel.srv.Client(), rel.source(), WithLogger(logger)).refreshCache(t.Context(), staleCache("")); err != nil {
		t.Fatalf("refresh(census document) = %v, want the refresh accepted", err)
	}
	if n := rec.CountExact("mapping: refreshed"); n != 1 {
		t.Fatalf("accepted refresh logged %d refreshed lines, want 1: %v", n, rec.Messages())
	}
	for key, want := range map[string]string{
		"tag":                    testTag,
		"records":                "7",
		"season_scoped_records":  "1",
		"special_records":        "3",
		"routed_identifiers":     "6",
		"radarr_routed":          "2",
		"sonarr_routed":          "5",
		"season_range_records":   "1",
		"placed_special_records": "2",
		"placed_episode_records": "1",
		"single_special_records": "1",
	} {
		got, ok := rec.AttrValueExact("mapping: refreshed", key)
		if !ok {
			t.Errorf("refreshed log carries no %s attribute; logs = %v", key, rec.Messages())
			continue
		}
		if got != want {
			t.Errorf("refreshed log %s = %s, want %s", key, got, want)
		}
	}
}

// TestCensusOfCountsByRoute pins the census to the routes: a both-routes
// record counts on each side and once in routed, and a special is a record
// offered on season 0 of its series.
func TestCensusOfCountsByRoute(t *testing.T) {
	got := censusOf([]Record{
		{AniListID: 1, TmdbMovies: []int{4}, SeasonKind: SeasonAbsent},
		{AniListID: 2, TvdbID: 100, SeasonKind: SeasonPresent, SeasonTvdb: 2},
		{AniListID: 3, TvdbID: 200, TmdbMovies: []int{5}, SeasonKind: SeasonPresent},
		{AniListID: 4, SeasonKind: SeasonAbsent},
	})
	want := populations{radarrRouted: 2, sonarrRouted: 2, routed: 3, special: 1, positiveSeason: 1}
	if got != want {
		t.Errorf("censusOf = %+v, want %+v", got, want)
	}
}

// TestValidateRefreshedRecordsAcceptsTheRecordsOfTheRetiredShape pins the
// transition the user's cache makes: records persisted with a type and IMDb
// ids decode into this build's Record and accept the same release's records.
func TestValidateRefreshedRecordsAcceptsTheRecordsOfTheRetiredShape(t *testing.T) {
	var previous []Record
	data := `[{"type":"MOVIE","season_kind":"present","imdb_ids":["tt1"],"tmdb_movies":[42],"anilist_id":1,"tvdb_id":100},` +
		`{"type":"TV","season_kind":"present","anilist_id":2,"tvdb_id":200,"season_tvdb":1},` +
		`{"type":"MOVIE","season_kind":"absent","imdb_ids":["tt2"],"anilist_id":3}]`
	if err := json.Unmarshal([]byte(data), &previous); err != nil {
		t.Fatalf("decode retired-shape records: %v", err)
	}
	candidate := []Record{
		{AniListID: 1, TvdbID: 100, TmdbMovies: []int{42}, SeasonKind: SeasonPresent},
		{AniListID: 2, TvdbID: 200, SeasonKind: SeasonPresent, SeasonTvdb: 1},
		{AniListID: 3, SeasonKind: SeasonAbsent},
	}
	if err := validateRefreshedRecords(previous, candidate); err != nil {
		t.Errorf("validateRefreshedRecords(retired shape, same release) = %v, want accepted", err)
	}
}
