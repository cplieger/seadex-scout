package align_test

import (
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"pgregory.net/rapid"
)

// wholeRec is the record shape that classifies as a whole-series comparison:
// a Sonarr series with no positive Fribb TVDB season and not a special.
var wholeRec = mapping.Record{Type: "TV", SeasonTvdb: 0}

// decideWhole runs the shared decision for a whole-series item over the given
// per-season groups.
func decideWhole(seasons map[int][]string, best, alt []string) align.Decision {
	item := &library.Item{Arr: library.ArrSonarr, SeasonGroups: seasons, HasFile: true}
	return align.Decide(item, &wholeRec, best, alt, nil, nil)
}

// TestDecideWholeSeriesConservative pins the conservative per-real-season
// aggregation (ported from the audit's former wholeSeriesVerdict table, which
// the shared core replaced): best only when every filed real season provenly
// carries a best group, downgrading to alt then unlisted otherwise, season 0
// excluded, no filed real season reading as no-file, and the approximation
// flag set exactly when the aggregate spans more than one season or group.
func TestDecideWholeSeriesConservative(t *testing.T) {
	best := []string{"a&c"}
	alt := []string{"kh"}
	tests := []struct {
		name    string
		seasons map[int][]string
		want    align.Standing
		approx  bool
	}{
		{"all seasons best", map[int][]string{1: {"a&c"}, 2: {"a&c"}}, align.StandingBest, true},
		{"best plus unlisted downgrades to unlisted", map[int][]string{1: {"a&c"}, 2: {"kitsune"}}, align.StandingUnlisted, true},
		{"best plus alt downgrades to alt", map[int][]string{1: {"a&c"}, 2: {"kh"}}, align.StandingAlt, true},
		{"season 0 is excluded", map[int][]string{0: {"kitsune"}, 1: {"a&c"}}, align.StandingBest, false},
		{"single season is not approx", map[int][]string{1: {"a&c"}}, align.StandingBest, false},
		{"single season spanning two groups is approx", map[int][]string{1: {"a&c", "kh"}}, align.StandingBest, true},
		{"an empty season is neither counted nor approx", map[int][]string{1: {"a&c"}, 2: {}}, align.StandingBest, false},
		{"only season 0 on disk is no-file", map[int][]string{0: {"a&c"}}, align.StandingNoFile, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := decideWhole(tt.seasons, best, alt)
			if d.Standing != tt.want {
				t.Errorf("Standing = %v, want %v", d.Standing, tt.want)
			}
			if d.Approx != tt.approx {
				t.Errorf("Approx = %v, want %v", d.Approx, tt.approx)
			}
			if d.Kind != align.ScopeWholeSeries {
				t.Errorf("Kind = %v, want ScopeWholeSeries", d.Kind)
			}
		})
	}
}

// TestDecideWholeSeriesGroupsUnion pins the aggregate group set the decision
// carries for display and dedupe keys: the sorted, per-season-deduped union of
// every filed real season's groups, season 0 excluded, and nil when no real
// season is filed.
func TestDecideWholeSeriesGroupsUnion(t *testing.T) {
	tests := []struct {
		name    string
		seasons map[int][]string
		want    []string
	}{
		{"unions and sorts across seasons, season 0 excluded", map[int][]string{0: {"specialgrp"}, 1: {"a&c"}, 2: {"kh"}}, []string{"a&c", "kh"}},
		{"deduplicates a group shared across seasons", map[int][]string{1: {"shared", "alpha"}, 2: {"shared", "beta"}}, []string{"alpha", "beta", "shared"}},
		{"no filed real season carries no groups", map[int][]string{0: {"a&c"}}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := decideWhole(tt.seasons, []string{"a&c"}, nil)
			if !reflect.DeepEqual(d.Groups, tt.want) {
				t.Errorf("Groups = %v, want %v", d.Groups, tt.want)
			}
		})
	}
}

// TestDecideWholeSeriesNilAlt pins the daemon-shaped inputs: with a nil alt
// set, a filed season provenly lacking a best group reads unlisted (never
// alt), so "aligned" is exactly "no season is unlisted or unverifiable".
func TestDecideWholeSeriesNilAlt(t *testing.T) {
	d := decideWhole(map[int][]string{1: {"a&c"}, 2: {"kh"}}, []string{"a&c"}, nil)
	if d.Standing != align.StandingUnlisted {
		t.Errorf("Standing = %v, want StandingUnlisted (nil alt: a best-less season is unlisted)", d.Standing)
	}
	if d.Outcome != align.OutcomeMixed {
		t.Errorf("Outcome = %v, want OutcomeMixed (not aligned, two-group aggregate)", d.Outcome)
	}
}

// TestDecideWholeSeriesUnknownEvidence pins the conservative propagation of
// unverifiability: a season with unknown group evidence (release.NoGroup on either
// side) blocks the have-best claim, while a PROVEN downgrade in another season
// still outranks the unknown, since the proof stands regardless of what the
// unknown season holds.
func TestDecideWholeSeriesUnknownEvidence(t *testing.T) {
	best := []string{"a&c"}
	alt := []string{"kh"}
	tests := []struct {
		name    string
		seasons map[int][]string
		best    []string
		want    align.Standing
		outcome align.Outcome
	}{
		{
			name:    "an unknown season blocks best: series is unverified",
			seasons: map[int][]string{1: {"a&c"}, 2: {"nogrp"}},
			best:    best, want: align.StandingUnverified, outcome: align.OutcomeUnverifiable,
		},
		{
			name:    "sentinel-only series is unverified",
			seasons: map[int][]string{1: {"nogrp"}},
			best:    best, want: align.StandingUnverified, outcome: align.OutcomeUnverifiable,
		},
		{
			name:    "an unknown-only best set makes every filed season unverifiable",
			seasons: map[int][]string{1: {"a&c"}, 2: {"kh"}},
			best:    []string{"nogrp"}, want: align.StandingUnverified, outcome: align.OutcomeUnverifiable,
		},
		{
			name:    "a proven unlisted season outranks an unknown one",
			seasons: map[int][]string{1: {"nogrp"}, 2: {"kitsune"}},
			best:    best, want: align.StandingUnlisted, outcome: align.OutcomeMixed,
		},
		{
			name:    "a proven alt season outranks an unknown one",
			seasons: map[int][]string{1: {"nogrp"}, 2: {"kh"}},
			best:    best, want: align.StandingAlt, outcome: align.OutcomeMixed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := decideWhole(tt.seasons, tt.best, alt)
			if d.Standing != tt.want {
				t.Errorf("Standing = %v, want %v", d.Standing, tt.want)
			}
			if d.Outcome != tt.outcome {
				t.Errorf("Outcome = %v, want %v", d.Outcome, tt.outcome)
			}
		})
	}
}

// TestDecideWholeSeriesOutcomes pins the outcome linearization over the
// aggregate: no filed real season beats the no-best nudge, full alignment is
// silent however many groups the union spans, and a not-aligned single-group
// aggregate diverges.
func TestDecideWholeSeriesOutcomes(t *testing.T) {
	tests := []struct {
		name    string
		seasons map[int][]string
		best    []string
		want    align.Outcome
	}{
		{"no filed real season wins over no-best", map[int][]string{0: {"x"}}, nil, align.OutcomeNoFile},
		{"no-best with a filed season", map[int][]string{1: {"a"}}, nil, align.OutcomeNoBest},
		{"aligned multi-group aggregate is aligned", map[int][]string{1: {"a&c"}, 2: {"a&c", "kh"}}, []string{"a&c"}, align.OutcomeAligned},
		{"not-aligned single-group aggregate diverges", map[int][]string{1: {"kh"}, 2: {"kh"}}, []string{"a&c"}, align.OutcomeDiverged},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if d := decideWhole(tt.seasons, tt.best, nil); d.Outcome != tt.want {
				t.Errorf("Outcome = %v, want %v", d.Outcome, tt.want)
			}
		})
	}
}

// TestDecideWholeSeriesUnknownAltEvidence pins the alt-rung unverifiability
// propagation through the whole-series aggregation: a filed season whose known
// groups provenly lack the best but whose alt comparison is indeterminate (an
// unknown-only alt set) marks the season unverifiable, so the series reads
// unverified - never the confident unlisted divergence the evidence cannot
// prove.
func TestDecideWholeSeriesUnknownAltEvidence(t *testing.T) {
	d := decideWhole(map[int][]string{1: {"kitsune"}}, []string{"a&c"}, []string{"nogrp"})
	if d.Standing != align.StandingUnverified {
		t.Errorf("Standing = %v, want StandingUnverified (unknown-only alt: the divergence is unproven)", d.Standing)
	}
	if d.Outcome != align.OutcomeUnverifiable {
		t.Errorf("Outcome = %v, want OutcomeUnverifiable", d.Outcome)
	}
}

// standingConservativeness ranks standings by conservativeness for the
// whole-series aggregation properties: Best < Unverified < Alt < Unlisted.
var standingConservativeness = map[align.Standing]int{
	align.StandingBest:       0,
	align.StandingUnverified: 1,
	align.StandingAlt:        2,
	align.StandingUnlisted:   3,
}

// TestDecideWholeSeriesMonotoneDowngrade property-checks the aggregation's core
// invariant: growing a whole-series item by one more filed real season can only
// hold or downgrade the standing (Best -> Unverified -> Alt -> Unlisted), never
// upgrade it, so an already-proven downgrade cannot be washed out by adding
// evidence. A violation means one season's verdict masked another's.
func TestDecideWholeSeriesMonotoneDowngrade(t *testing.T) {
	groupPool := []string{"a&c", "kh", "kitsune", "nogrp", "sam"}
	best := []string{"a&c"}
	alt := []string{"kh"}
	rapid.Check(t, func(t *rapid.T) {
		groupsGen := rapid.SliceOfN(rapid.SampledFrom(groupPool), 1, 3)
		seasons := rapid.MapOfN(rapid.IntRange(1, 6), groupsGen, 1, 4).Draw(t, "seasons")
		before := decideWhole(seasons, best, alt)

		grown := maps.Clone(seasons)
		grown[rapid.IntRange(7, 9).Draw(t, "extra_season")] = groupsGen.Draw(t, "extra_groups")
		after := decideWhole(grown, best, alt)

		if standingConservativeness[after.Standing] < standingConservativeness[before.Standing] {
			t.Fatalf("adding a season upgraded the standing: %v -> %v (seasons %v, grown %v)",
				before.Standing, after.Standing, seasons, grown)
		}
	})
}

// TestDecideWholeSeriesMatchesMostConservativeSeason property-checks the
// whole-series aggregation against an oracle built from the package's OWN
// single-unit path: the aggregate standing must equal the most conservative
// (Best < Unverified < Alt < Unlisted) of the standings Decide produces when each
// filed real season is judged alone as a mapped single season, and no-file exactly
// when no real season carries files. So a drift between summarizeWholeSeries's
// per-season ladder and unitStanding's fails the property.
func TestDecideWholeSeriesMatchesMostConservativeSeason(t *testing.T) {
	groupPool := []string{"a&c", "kh", "kitsune", "nogrp", "sam"}
	rapid.Check(t, func(t *rapid.T) {
		groupsGen := rapid.SliceOfN(rapid.SampledFrom(groupPool), 0, 3)
		seasons := rapid.MapOfN(rapid.IntRange(0, 6), groupsGen, 1, 5).Draw(t, "seasons")
		best := rapid.SliceOfN(rapid.SampledFrom(groupPool), 1, 2).Draw(t, "best")
		alt := rapid.SliceOfN(rapid.SampledFrom(groupPool), 0, 2).Draw(t, "alt")

		whole := decideWhole(seasons, best, alt)

		want := align.StandingNoFile
		filed := false
		for season, groups := range seasons {
			if season == 0 || len(groups) == 0 {
				continue
			}
			item := &library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{season: groups}}
			rec := mapping.Record{Type: "TV", SeasonTvdb: season}
			single := align.Decide(item, &rec, best, alt, nil, nil)
			if !filed || standingConservativeness[single.Standing] > standingConservativeness[want] {
				want = single.Standing
			}
			filed = true
		}

		if whole.Standing != want {
			t.Fatalf("whole-series Standing = %v, want the most conservative per-season standing %v (seasons %v, best %v, alt %v)",
				whole.Standing, want, seasons, best, alt)
		}
	})
}

// TestDecideWholeSeriesDropsSiblingMappedSeasons is the sibling ruling's shipped
// half: a real season a SIBLING record maps positively belongs to that sibling's
// own comparison, so folding it into this entry's aggregate contaminates the
// verdict with a run the entry does not cover. Both live false findings go silent
// here, Gintama and Bleach.
//
// The last case is the reachable edge: an item whose every filed season belongs to
// siblings summarizes to zero seasons, which is StandingNoFile.
func TestDecideWholeSeriesDropsSiblingMappedSeasons(t *testing.T) {
	best := []string{"cbt"}
	tests := []struct {
		name     string
		seasons  map[int][]string
		siblings []int
		want     align.Standing
	}{
		{
			name:     "Gintama: the sibling-owned seasons stop contaminating the aggregate",
			seasons:  map[int][]string{1: {"cbt"}, 2: {"cbt"}, 3: {"cbt"}, 4: {"cbt"}, 5: {"kh"}, 6: {"kh"}, 7: {"kh"}, 8: {"kh"}, 10: {"kh"}},
			siblings: []int{5, 6, 7, 8, 10},
			want:     align.StandingBest,
		},
		{
			name:     "Bleach: one sibling-owned season dropped",
			seasons:  map[int][]string{1: {"cbt"}, 17: {"kitsune"}},
			siblings: []int{17},
			want:     align.StandingBest,
		},
		{
			name:     "without the summary the same item is contaminated",
			seasons:  map[int][]string{1: {"cbt"}, 17: {"kitsune"}},
			siblings: nil,
			want:     align.StandingUnlisted,
		},
		{
			name:     "every filed season belongs to siblings, so nothing of the entry's own is on disk",
			seasons:  map[int][]string{5: {"kh"}, 6: {"kh"}},
			siblings: []int{5, 6},
			want:     align.StandingNoFile,
		},
		{
			name:     "a sibling season with no files on disk changes nothing",
			seasons:  map[int][]string{1: {"cbt"}},
			siblings: []int{2, 3},
			want:     align.StandingBest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := &library.Item{Arr: library.ArrSonarr, SeasonGroups: tt.seasons, HasFile: true}
			d := align.Decide(item, &wholeRec, best, nil, tt.siblings, nil)
			if d.Standing != tt.want {
				t.Errorf("Standing = %v, want %v (groups %v)", d.Standing, tt.want, d.Groups)
			}
			for _, season := range tt.siblings {
				for _, group := range tt.seasons[season] {
					if slices.Contains(d.Groups, group) {
						t.Errorf("Groups = %v, want no group from the sibling-owned season %d", d.Groups, season)
					}
				}
			}
		})
	}
}

// TestDecideSiblingSeasonsOnlyReachTheWholeSeriesAggregate pins the parameter's
// scope: every other comparison is already exact about its unit, so a sibling
// season must not remove a season-scoped entry's own season or touch a movie.
func TestDecideSiblingSeasonsOnlyReachTheWholeSeriesAggregate(t *testing.T) {
	best := []string{"cbt"}
	seasonItem := &library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{3: {"cbt"}}}
	seasonRec := mapping.Record{Type: "TV", SeasonKind: mapping.SeasonPresent, SeasonTvdb: 3}
	if d := align.Decide(seasonItem, &seasonRec, best, nil, []int{3}, nil); d.Standing != align.StandingBest {
		t.Errorf("season-scoped Standing = %v, want %v (a cour-split sibling must not blank the entry's own season)", d.Standing, align.StandingBest)
	}
	movieItem := &library.Item{Arr: library.ArrRadarr, Groups: []string{"cbt"}, HasFile: true}
	movieRec := mapping.Record{Type: "MOVIE", SeasonKind: mapping.SeasonPresent}
	if d := align.Decide(movieItem, &movieRec, best, nil, []int{1, 2}, nil); d.Standing != align.StandingBest {
		t.Errorf("movie Standing = %v, want %v", d.Standing, align.StandingBest)
	}
}

// TestDecideOwnSeasonsJudgeASplitShowPerEntry pins the report rule: when the
// Anime-Lists mapping-list names an entry's own TVDB seasons, a whole-series
// comparison judges exactly those. Fairy Tail's three entries share one eight-season
// Sonarr series, and with the verdicts arranged to differ (S1-S4 best, S5-S7 alt,
// S8 unlisted) each Groups is its own seasons' union, where the sibling rule alone
// judges each against all eight and returns the same contaminated unlisted thrice.
func TestDecideOwnSeasonsJudgeASplitShowPerEntry(t *testing.T) {
	best, alt := []string{"cbt"}, []string{"kh"}
	item := &library.Item{Arr: library.ArrSonarr, HasFile: true, SeasonGroups: map[int][]string{
		1: {"cbt"}, 2: {"cbt"}, 3: {"cbt"}, 4: {"cbt"},
		5: {"kh"}, 6: {"kh"}, 7: {"kh"},
		8: {"erai"},
	}}
	tests := []struct {
		name       string
		seasons    []mapping.SeasonRange
		wantGroups []string
		want       align.Standing
	}{
		{
			name: "6662 S1-S4", seasons: []mapping.SeasonRange{{Season: 1, First: 1, Last: 48}, {Season: 2, First: 49, Last: 96}, {Season: 3, First: 97, Last: 150}, {Season: 4, First: 151, Last: 175}},
			wantGroups: []string{"cbt"}, want: align.StandingBest,
		},
		{
			name: "9980 S5-S7", seasons: []mapping.SeasonRange{{Season: 5, First: 1, Last: 51}, {Season: 6, First: 52, Last: 90}, {Season: 7, First: 91, Last: 102}},
			wantGroups: []string{"kh"}, want: align.StandingAlt,
		},
		{
			name: "13295 S8", seasons: []mapping.SeasonRange{{Season: 8, First: 1, Last: 51}},
			wantGroups: []string{"erai"}, want: align.StandingUnlisted,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := align.Decide(item, &wholeRec, best, alt, nil, tt.seasons)
			if d.Kind != align.ScopeWholeSeries {
				t.Fatalf("Kind = %v, want %v", d.Kind, align.ScopeWholeSeries)
			}
			if d.Standing != tt.want {
				t.Errorf("Standing = %v, want %v (groups %v)", d.Standing, tt.want, d.Groups)
			}
			if !slices.Equal(d.Groups, tt.wantGroups) {
				t.Errorf("Groups = %v, want exactly the entry's own seasons' union %v", d.Groups, tt.wantGroups)
			}
		})
	}
	// Without the ranges the same item is contaminated for every entry: the
	// sibling rule has nothing to drop, and every season votes.
	if d := align.Decide(item, &wholeRec, best, alt, nil, nil); d.Standing != align.StandingUnlisted || len(d.Groups) != 3 {
		t.Errorf("without ranges Standing = %v groups %v, want %v over all three groups", d.Standing, d.Groups, align.StandingUnlisted)
	}
	// Ranges win over the sibling set when both are present: a season a range
	// names is judged even if a sibling also maps it.
	if d := align.Decide(item, &wholeRec, best, alt, []int{1, 2, 3, 4}, tests[0].seasons); d.Standing != align.StandingBest {
		t.Errorf("ranges plus siblings Standing = %v, want %v (the ranges are the one source)", d.Standing, align.StandingBest)
	}
}
