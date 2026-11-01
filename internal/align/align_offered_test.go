package align_test

import (
	"testing"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
)

// TestRecordSeasonDispatchesOnThePlacement pins that the scope dispatch reads
// the season's PRESENCE and never the routes: an absolute-numbered run is judged
// against the real seasons, a mapped zero against the season-0 bucket, whether
// or not the entry also has a Radarr route.
func TestRecordSeasonDispatchesOnThePlacement(t *testing.T) {
	tests := []struct {
		name       string
		rec        mapping.Record
		wantKind   align.ScopeKind
		wantSeason int
	}{
		{
			name:     "LoGH: absent season, spans the real seasons",
			rec:      mapping.Record{TvdbID: 78964, SeasonKind: mapping.SeasonAbsent},
			wantKind: align.ScopeWholeSeries,
		},
		{
			name:     "Heya Camp: mapped zero, offered",
			rec:      mapping.Record{TvdbID: 344974, SeasonKind: mapping.SeasonPresent},
			wantKind: align.ScopeOffered,
		},
		{
			name:       "a positive season keeps its season and stays comparable",
			rec:        mapping.Record{TvdbID: 424536, SeasonKind: mapping.SeasonPresent, SeasonTvdb: 2},
			wantKind:   align.ScopeSeason,
			wantSeason: 2,
		},
		{
			name:     "a film with a TMDB route and a mapped zero is offered",
			rec:      mapping.Record{TvdbID: 85249, TmdbMovies: []int{80518}, SeasonKind: mapping.SeasonPresent},
			wantKind: align.ScopeOffered,
		},
		{
			name:     "a film with a TMDB route and an absent season is a whole-series comparison",
			rec:      mapping.Record{TvdbID: 348545, TmdbMovies: []int{1311031}, SeasonKind: mapping.SeasonAbsent},
			wantKind: align.ScopeWholeSeries,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, season := align.RecordSeason(&tt.rec)
			if kind != tt.wantKind {
				t.Errorf("RecordSeason(%+v) kind = %v, want %v", tt.rec, kind, tt.wantKind)
			}
			if season != tt.wantSeason {
				t.Errorf("RecordSeason(%+v) season = %d, want %d", tt.rec, season, tt.wantSeason)
			}
		})
	}
}

// TestRecordSeasonUnknownKind pins the arm an override naming no season_kind
// reaches: with no placement to read, a positive season pins that season and
// anything else is the whole series, a film's record included.
func TestRecordSeasonUnknownKind(t *testing.T) {
	tests := []struct {
		name       string
		rec        mapping.Record
		wantKind   align.ScopeKind
		wantSeason int
	}{
		{name: "no season", rec: mapping.Record{TvdbID: 12345}, wantKind: align.ScopeWholeSeries},
		{name: "no season with a TMDB route", rec: mapping.Record{TvdbID: 85249, TmdbMovies: []int{80518}}, wantKind: align.ScopeWholeSeries},
		{name: "a positive season", rec: mapping.Record{TvdbID: 78878, SeasonTvdb: 3}, wantKind: align.ScopeSeason, wantSeason: 3},
		{name: "a positive season with a TMDB route", rec: mapping.Record{TvdbID: 78878, TmdbMovies: []int{80741}, SeasonTvdb: 3}, wantKind: align.ScopeSeason, wantSeason: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, season := align.RecordSeason(&tt.rec)
			if kind != tt.wantKind || season != tt.wantSeason {
				t.Errorf("RecordSeason(%+v) = %v, %d, want %v, %d", tt.rec, kind, season, tt.wantKind, tt.wantSeason)
			}
		})
	}
}

// TestSonarrSpecialIsTheOfferedScope pins, over every season kind, season,
// TVDB id and TMDB route, that the specials rule and the comparison scope
// agree: the Sonarr copy is a special exactly when it is offered on season 0.
func TestSonarrSpecialIsTheOfferedScope(t *testing.T) {
	kinds := []mapping.SeasonKind{"", mapping.SeasonPresent, mapping.SeasonAbsent}
	for _, kind := range kinds {
		for _, season := range []int{0, 3} {
			for _, tvdb := range []int{0, 85249} {
				for _, tmdb := range [][]int{nil, {80518}} {
					rec := mapping.Record{SeasonKind: kind, SeasonTvdb: season, TvdbID: tvdb, TmdbMovies: tmdb}
					scope, _ := align.RecordSeason(&rec)
					want := tvdb > 0 && scope == align.ScopeOffered
					if got := rec.SonarrSpecial(); got != want {
						t.Errorf("SonarrSpecial(%+v) = %v, want %v (RecordSeason %v)", rec, got, want, scope)
					}
				}
			}
		}
	}
}

// TestDecideOfferedStanding pins the two standings the offered kind produces and
// why the empty bucket is not one of them: absence is PROVABLE without
// attributing anything, so an empty season-0 bucket keeps its no_file (11 live
// rows, 4 special and 7 film, that an unconditional unverified would have
// downgraded), while a populated one takes the standing a placeholder whose file
// state could not be read already takes.
func TestDecideOfferedStanding(t *testing.T) {
	rec := mapping.Record{TvdbID: 78964, SeasonKind: mapping.SeasonPresent}
	t.Run("empty bucket is no_file", func(t *testing.T) {
		item := library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{1: {"koala"}}}
		d := align.Decide(&item, &align.Entry{Record: &rec}, &align.Listing{Best: []string{"subsplease"}})
		if d.Kind != align.ScopeOffered {
			t.Fatalf("Kind = %v, want %v", d.Kind, align.ScopeOffered)
		}
		if d.Standing != align.StandingNoFile || d.Outcome != align.OutcomeNoFile {
			t.Errorf("Standing, Outcome = %v, %v, want NoFile, NoFile (an empty bucket proves absence)", d.Standing, d.Outcome)
		}
		if d.Approx {
			t.Error("Approx = true, want false (nothing was aggregated: the bucket is empty)")
		}
	})
	t.Run("populated bucket is unverified and approximate", func(t *testing.T) {
		item := library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{0: {"erai-raws"}}}
		d := align.Decide(&item, &align.Entry{Record: &rec}, &align.Listing{Best: []string{"subsplease"}})
		if d.Standing != align.StandingUnverified {
			t.Errorf("Standing = %v, want %v (the verdict MOVES to unverified rather than claiming a divergence)", d.Standing, align.StandingUnverified)
		}
		if len(d.Groups) != 1 || d.Groups[0] != "erai-raws" {
			t.Errorf("Groups = %v, want the bucket's groups, so the operator sees what IS on disk", d.Groups)
		}
		if !d.Approx {
			t.Error("Approx = false, want true (a single-group bucket was never attributed either)")
		}
	})
	t.Run("a populated bucket carrying the recommended group is still unverified", func(t *testing.T) {
		// The group ladder is not consulted at all: a bucket holding the best
		// group does not prove THIS entry's file is the one carrying it.
		item := library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{0: {"subsplease"}}}
		d := align.Decide(&item, &align.Entry{Record: &rec}, &align.Listing{Best: []string{"subsplease"}})
		if d.Standing != align.StandingUnverified {
			t.Errorf("Standing = %v, want %v", d.Standing, align.StandingUnverified)
		}
	})
}

// TestClaimsCoverage pins the exported coverage predicate. It answers over the RESOLVED
// scope rather than the record, and the third case is why: 54 live rows are
// Radarr-owned films with a mapped zero, and keying coverage on RecordSeason
// would strip coverage from every one and manufacture a not_on_seadex row for
// their movies.
func TestClaimsCoverage(t *testing.T) {
	mappedZeroFilm := mapping.Record{TvdbID: 78964, TmdbMovies: []int{1}, SeasonKind: mapping.SeasonPresent}
	tests := []struct {
		name string
		item library.Item
		rec  mapping.Record
		want bool
	}{
		{
			name: "a film offered on a Sonarr bucket claims none",
			item: library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{0: {"erai-raws"}}},
			rec:  mappedZeroFilm,
		},
		{
			name: "the same record on a Radarr movie keeps ScopeMovie and its coverage",
			item: library.Item{Arr: library.ArrRadarr, Groups: []string{"erai-raws"}, HasFile: true},
			rec:  mappedZeroFilm,
			want: true,
		},
		{
			name: "a positive-season series claims coverage",
			item: library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{1: {"erai-raws"}}},
			rec:  mapping.Record{SeasonKind: mapping.SeasonPresent, SeasonTvdb: 1},
			want: true,
		},
		{
			name: "an absent-season special claims coverage: it is compared, LoGH's shape",
			item: library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{1: {"koala"}}},
			rec:  mapping.Record{SeasonKind: mapping.SeasonAbsent},
			want: true,
		},
		{
			name: "a Failed placeholder claims coverage: unreadable is not unattributable",
			item: library.Item{Arr: library.ArrSonarr, Failed: true},
			rec:  mapping.Record{SeasonKind: mapping.SeasonPresent, SeasonTvdb: 1},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := align.ClaimsCoverage(&tt.item, &tt.rec); got != tt.want {
				t.Errorf("ClaimsCoverage() = %v, want %v", got, tt.want)
			}
		})
	}
}
