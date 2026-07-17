package align_test

import (
	"testing"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
)

// TestDecideSingleUnit pins the file-first group ladder and the outcome
// linearization for the single-unit scopes (ported from the audit's former
// verdict table, which the shared core replaced): no file wins over
// everything including the no-best nudge, a group-less filed unit is
// unverified (and diverges rather than reading mixed), a present best group
// aligns no matter how many groups the unit spans, and a not-aligned unit is
// mixed exactly when it spans more than one group.
func TestDecideSingleUnit(t *testing.T) {
	seasonRec := mapping.Record{Type: "TV", SeasonTvdb: 1}
	movieRec := mapping.Record{Type: "MOVIE"}
	tests := []struct {
		name         string
		item         library.Item
		rec          mapping.Record
		best         []string
		alt          []string
		wantStanding align.Standing
		wantOutcome  align.Outcome
		wantNoBest   bool
	}{
		{
			name:         "mapped season not on disk is no-file",
			item:         library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{2: {"a"}}},
			rec:          seasonRec,
			best:         []string{"sam"},
			wantStanding: align.StandingNoFile,
			wantOutcome:  align.OutcomeNoFile,
		},
		{
			name:         "no file wins over the no-best nudge",
			item:         library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{2: {"a"}}},
			rec:          seasonRec,
			wantStanding: align.StandingNoFile,
			wantOutcome:  align.OutcomeNoFile,
			wantNoBest:   true,
		},
		{
			name:         "filed movie with no identifiable group is unverified and diverges",
			item:         library.Item{Arr: library.ArrRadarr, HasFile: true},
			rec:          movieRec,
			best:         []string{"sam"},
			wantStanding: align.StandingUnverified,
			wantOutcome:  align.OutcomeDiverged,
		},
		{
			name:         "current group is best: aligned",
			item:         library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{1: {"sam"}}},
			rec:          seasonRec,
			best:         []string{"sam"},
			wantStanding: align.StandingBest,
			wantOutcome:  align.OutcomeAligned,
		},
		{
			name:         "current group is an alt",
			item:         library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{1: {"kh"}}},
			rec:          seasonRec,
			best:         []string{"sam"},
			alt:          []string{"kh"},
			wantStanding: align.StandingAlt,
			wantOutcome:  align.OutcomeDiverged,
		},
		{
			name:         "current group is unlisted",
			item:         library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{1: {"zzz"}}},
			rec:          seasonRec,
			best:         []string{"sam"},
			alt:          []string{"kh"},
			wantStanding: align.StandingUnlisted,
			wantOutcome:  align.OutcomeDiverged,
		},
		{
			name:         "not-aligned multi-group season is mixed",
			item:         library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{1: {"a", "b"}}},
			rec:          seasonRec,
			best:         []string{"sam"},
			wantStanding: align.StandingUnlisted,
			wantOutcome:  align.OutcomeMixed,
		},
		{
			name:         "alt-matching multi-group season is mixed",
			item:         library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{1: {"a", "b"}}},
			rec:          seasonRec,
			best:         []string{"sam"},
			alt:          []string{"a"},
			wantStanding: align.StandingAlt,
			wantOutcome:  align.OutcomeMixed,
		},
		{
			name:         "aligned multi-group season is aligned, not mixed",
			item:         library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{1: {"a", "b"}}},
			rec:          seasonRec,
			best:         []string{"a"},
			wantStanding: align.StandingBest,
			wantOutcome:  align.OutcomeAligned,
		},
		{
			name:         "empty best set with a filed season is the no-best nudge",
			item:         library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{1: {"a"}}},
			rec:          seasonRec,
			wantStanding: align.StandingUnlisted,
			wantOutcome:  align.OutcomeNoBest,
			wantNoBest:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := align.Decide(&tt.item, &tt.rec, tt.best, tt.alt)
			if d.Standing != tt.wantStanding {
				t.Errorf("Standing = %v, want %v", d.Standing, tt.wantStanding)
			}
			if d.Outcome != tt.wantOutcome {
				t.Errorf("Outcome = %v, want %v", d.Outcome, tt.wantOutcome)
			}
			if d.NoBest != tt.wantNoBest {
				t.Errorf("NoBest = %v, want %v", d.NoBest, tt.wantNoBest)
			}
		})
	}
}

// TestDecideRecordsScopeKindAndGroups pins that the decision carries the
// resolved scope kind and the groups the unit was judged against (the scoped
// set for a single unit, so consumers seed display fields and dedupe keys
// from the decision without re-scoping).
func TestDecideRecordsScopeKindAndGroups(t *testing.T) {
	item := library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{2: {"sam"}}}
	rec := mapping.Record{Type: "TV", SeasonTvdb: 2}
	d := align.Decide(&item, &rec, []string{"sam"}, nil)
	if d.Kind != align.ScopeSeason {
		t.Errorf("Kind = %v, want ScopeSeason", d.Kind)
	}
	if len(d.Groups) != 1 || d.Groups[0] != "sam" {
		t.Errorf("Groups = %v, want [sam]", d.Groups)
	}
	if d.Approx {
		t.Error("a single-group season comparison must not be approximate")
	}
}
