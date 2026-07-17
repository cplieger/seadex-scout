package align_test

import (
	"reflect"
	"testing"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
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
