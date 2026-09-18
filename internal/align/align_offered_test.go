package align_test

import (
	"testing"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
)

// TestRecordSeasonDispatchesOnTheSeasonNotTheType pins that the scope dispatch
// reads the season's PRESENCE: it fails if IsSpecial or IsMovie is a scope input
// on a record whose season kind is KNOWN. The three fixtures are the live shapes
// a type label gets wrong in both directions - an OVA-typed absolute-numbered
// run judged against a season-0 bucket, and two TV-typed specials entries judged
// against every season.
func TestRecordSeasonDispatchesOnTheSeasonNotTheType(t *testing.T) {
	tests := []struct {
		name       string
		rec        mapping.Record
		wantKind   align.ScopeKind
		wantSeason int
	}{
		{
			name:     "LoGH: OVA-typed, absent season, spans the real seasons",
			rec:      mapping.Record{Type: "OVA", TvdbID: 78964, SeasonKind: mapping.SeasonAbsent},
			wantKind: align.ScopeWholeSeries,
		},
		{
			name:     "Heya Camp: TV-typed, mapped zero, offered",
			rec:      mapping.Record{Type: "TV", TvdbID: 344974, SeasonKind: mapping.SeasonPresent},
			wantKind: align.ScopeOffered,
		},
		{
			name:     "Seisen no Shirushi: TV-typed, mapped zero, offered",
			rec:      mapping.Record{Type: "TV", TvdbID: 79151, SeasonKind: mapping.SeasonPresent},
			wantKind: align.ScopeOffered,
		},
		{
			name:       "a positive season keeps its season and stays comparable",
			rec:        mapping.Record{Type: "TV", TvdbID: 424536, SeasonKind: mapping.SeasonPresent, SeasonTvdb: 2},
			wantKind:   align.ScopeSeason,
			wantSeason: 2,
		},
		{
			name:     "a MOVIE record with a mapped zero is offered",
			rec:      mapping.Record{Type: "MOVIE", TvdbID: 78964, SeasonKind: mapping.SeasonPresent},
			wantKind: align.ScopeOffered,
		},
		{
			// The union arm costs nothing here, and no live record is in this state
			// (0 of the 919 tvdb-carrying MOVIE records), so this pins it against a
			// future upstream that is.
			name:     "a MOVIE record with an ABSENT season is a whole-series comparison",
			rec:      mapping.Record{Type: "MOVIE", TvdbID: 78964, SeasonKind: mapping.SeasonAbsent},
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

// TestRecordSeasonUnknownKindIsAUnion pins the transition arm on BOTH populations,
// because every revision of this dispatch fixed one and broke the other. Through a
// 304 window - up to the week Fribb takes to regenerate - every persisted record
// reads unknown, so the 48 films must still scope offered via IsMovie AND the 66
// mapped-zero specials via IsSpecial. Stranding the specials costs ~10 false WARNs
// plus 32 false info rows for a week.
func TestRecordSeasonUnknownKindIsAUnion(t *testing.T) {
	t.Run("the 48 films reach the offered kind via IsMovie", func(t *testing.T) {
		rec := mapping.Record{Type: "MOVIE", TvdbID: 78964}
		if kind, _ := align.RecordSeason(&rec); kind != align.ScopeOffered {
			t.Errorf("kind = %v, want %v (a film in a season-0 bucket must not be compared against every real season)", kind, align.ScopeOffered)
		}
	})
	t.Run("the 66 mapped-zero specials reach the offered kind via IsSpecial", func(t *testing.T) {
		for _, typ := range []string{"OVA", "ONA", "SPECIAL"} {
			rec := mapping.Record{Type: typ, TvdbID: 344974}
			if kind, _ := align.RecordSeason(&rec); kind != align.ScopeOffered {
				t.Errorf("%s kind = %v, want %v", typ, kind, align.ScopeOffered)
			}
		}
	})
	t.Run("a positive season still wins the union arm", func(t *testing.T) {
		rec := mapping.Record{Type: "MOVIE", TvdbID: 78878, SeasonTvdb: 3}
		kind, season := align.RecordSeason(&rec)
		if kind != align.ScopeSeason || season != 3 {
			t.Errorf("kind, season = %v, %d, want %v, 3", kind, season, align.ScopeSeason)
		}
	})
	t.Run("a plain TV record still reads whole-series", func(t *testing.T) {
		rec := mapping.Record{Type: "TV", TvdbID: 12345}
		if kind, _ := align.RecordSeason(&rec); kind != align.ScopeWholeSeries {
			t.Errorf("kind = %v, want %v", kind, align.ScopeWholeSeries)
		}
	})
}

// TestDecideOfferedStanding pins the two standings the offered kind produces and
// why the empty bucket is not one of them: absence is PROVABLE without
// attributing anything, so an empty season-0 bucket keeps its no_file (11 live
// rows, 4 special and 7 film, that an unconditional unverified would have
// downgraded), while a populated one takes the standing a placeholder whose file
// state could not be read already takes.
func TestDecideOfferedStanding(t *testing.T) {
	rec := mapping.Record{Type: "MOVIE", TvdbID: 78964, SeasonKind: mapping.SeasonPresent}
	t.Run("empty bucket is no_file", func(t *testing.T) {
		item := library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{1: {"koala"}}}
		d := align.Decide(&item, &rec, []string{"subsplease"}, nil, nil, nil)
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
		d := align.Decide(&item, &rec, []string{"subsplease"}, nil, nil, nil)
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
		d := align.Decide(&item, &rec, []string{"subsplease"}, nil, nil, nil)
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
	mappedZeroFilm := mapping.Record{Type: "MOVIE", TvdbID: 78964, SeasonKind: mapping.SeasonPresent}
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
			rec:  mapping.Record{Type: "TV", SeasonKind: mapping.SeasonPresent, SeasonTvdb: 1},
			want: true,
		},
		{
			name: "an absent-season special claims coverage: it is compared, LoGH's shape",
			item: library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{1: {"koala"}}},
			rec:  mapping.Record{Type: "OVA", SeasonKind: mapping.SeasonAbsent},
			want: true,
		},
		{
			name: "a Failed placeholder claims coverage: unreadable is not unattributable",
			item: library.Item{Arr: library.ArrSonarr, Failed: true},
			rec:  mapping.Record{Type: "TV", SeasonKind: mapping.SeasonPresent, SeasonTvdb: 1},
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
