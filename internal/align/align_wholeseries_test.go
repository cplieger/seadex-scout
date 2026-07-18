package align_test

import (
	"maps"
	"reflect"
	"testing"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"pgregory.net/rapid"
)

// wholeRec is the record shape that classifies as a whole-series comparison:
// a Sonarr series with no positive Fribb TVDB season and not a special.
var wholeRec = mapping.Record{Type: "TV", SeasonTvdb: 0}

func TestWholeSeries(t *testing.T) {
	tests := []struct {
		name string
		rec  mapping.Record
		item library.Item
		want bool
	}{
		{"sonarr seasonless non-special is whole-series", mapping.Record{Type: "TV", SeasonTvdb: 0}, library.Item{Arr: library.ArrSonarr}, true},
		{"sonarr with a positive season is not whole-series", mapping.Record{Type: "TV", SeasonTvdb: 2}, library.Item{Arr: library.ArrSonarr}, false},
		{"sonarr special is not whole-series", mapping.Record{Type: "OVA", SeasonTvdb: 0}, library.Item{Arr: library.ArrSonarr}, false},
		{"radarr is never whole-series", mapping.Record{Type: "MOVIE", SeasonTvdb: 0}, library.Item{Arr: library.ArrRadarr}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := align.Scope(&tt.item, &tt.rec).Kind == align.ScopeWholeSeries
			if got != tt.want {
				t.Errorf("Scope().Kind == ScopeWholeSeries = %v, want %v", got, tt.want)
			}
		})
	}
}

// decideWhole runs the shared decision for a whole-series item over the given
// per-season groups.
func decideWhole(seasons map[int][]string, best, alt []string) align.Decision {
	item := &library.Item{Arr: library.ArrSonarr, SeasonGroups: seasons, HasFile: true}
	return align.Decide(item, &wholeRec, best, alt)
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
// unverifiability through the whole-series aggregation: a season with unknown
// group evidence (the release.NoGroup sentinel, on either side of its
// comparison) blocks the have-best claim - the series reads unverified, never
// best - while a PROVEN downgrade in another season (unlisted or alt) still
// outranks the unknown: the proof stands regardless of what the unknown
// season might hold, so the actionable verdict is not hidden behind
// unverifiability.
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

// TestDecideWholeSeriesMonotoneDowngrade property-checks the conservative
// aggregation's core invariant: growing a whole-series item by one more filed
// real season can only hold or downgrade the standing (Best -> Unverified ->
// Alt -> Unlisted), never upgrade it - the per-season flags only accumulate,
// so an already-proven downgrade or unverifiability cannot be washed out by
// adding evidence. A violation would mean one season's verdict masked
// another's, the exact bug the conservative aggregation exists to prevent.
func TestDecideWholeSeriesMonotoneDowngrade(t *testing.T) {
	conservativeness := map[align.Standing]int{
		align.StandingBest:       0,
		align.StandingUnverified: 1,
		align.StandingAlt:        2,
		align.StandingUnlisted:   3,
	}
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

		if conservativeness[after.Standing] < conservativeness[before.Standing] {
			t.Fatalf("adding a season upgraded the standing: %v -> %v (seasons %v, grown %v)",
				before.Standing, after.Standing, seasons, grown)
		}
	})
}

// TestDecideWholeSeriesMatchesMostConservativeSeason property-checks the
// whole-series aggregation against an oracle built from the package's OWN
// single-unit path: the aggregate standing must equal the most conservative
// (Best < Unverified < Alt < Unlisted) of the standings Decide produces when
// each filed real season is judged alone as a mapped single season, and
// no-file exactly when no real season carries files. This is the documented
// contract ("the most conservative verdict") expressed as a cross-path
// consistency check, so a drift between summarizeWholeSeries's per-season
// ladder and unitStanding's - the divergence class the shared package exists
// to prevent - fails the property.
func TestDecideWholeSeriesMatchesMostConservativeSeason(t *testing.T) {
	conservativeness := map[align.Standing]int{
		align.StandingBest:       0,
		align.StandingUnverified: 1,
		align.StandingAlt:        2,
		align.StandingUnlisted:   3,
	}
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
			single := align.Decide(item, &rec, best, alt)
			if !filed || conservativeness[single.Standing] > conservativeness[want] {
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
