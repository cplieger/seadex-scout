package align_test

import (
	"slices"
	"testing"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/release"
)

var (
	none1 = release.Revision{Version: 1, Marker: release.RevisionNone}
	v2    = release.Revision{Version: 2, Marker: release.RevisionVersion}
	v3    = release.Revision{Version: 3, Marker: release.RevisionVersion}
)

type revisionCase struct {
	desc       string
	held       map[string]release.Revision
	listing    align.Listing
	groups     []string
	want       align.Outcome
	superseded []string
}

func revisionCases() []revisionCase {
	return []revisionCase{
		{
			desc:       "original against listed v2",
			held:       map[string]release.Revision{"g": none1},
			listing:    align.Listing{Best: []string{"G"}, BestRevisions: map[string]release.Revision{"g": v2}},
			groups:     []string{"g"},
			want:       align.OutcomeSuperseded,
			superseded: []string{"g"},
		},
		{
			desc:    "held v2, listed v2",
			held:    map[string]release.Revision{"g": v2},
			listing: align.Listing{Best: []string{"G"}, BestRevisions: map[string]release.Revision{"g": v2}},
			groups:  []string{"g"},
			want:    align.OutcomeAligned,
		},
		{
			desc:    "held v3, listed v2",
			held:    map[string]release.Revision{"g": v3},
			listing: align.Listing{Best: []string{"G"}, BestRevisions: map[string]release.Revision{"g": v2}},
			groups:  []string{"g"},
			want:    align.OutcomeAligned,
		},
		{
			desc:    "held unknown",
			listing: align.Listing{Best: []string{"G"}, BestRevisions: map[string]release.Revision{"g": v2}},
			groups:  []string{"g"},
			want:    align.OutcomeAligned,
		},
		{
			desc:    "unversioned listing",
			held:    map[string]release.Revision{"g": none1},
			listing: align.Listing{Best: []string{"G"}, BestRevisions: map[string]release.Revision{"g": none1}},
			groups:  []string{"g"},
			want:    align.OutcomeAligned,
		},
		{
			desc:    "group missing from the listed revisions",
			held:    map[string]release.Revision{"g": none1},
			listing: align.Listing{Best: []string{"G"}},
			groups:  []string{"g"},
			want:    align.OutcomeAligned,
		},
		{
			desc:    "another best group is current",
			held:    map[string]release.Revision{"g": none1, "k": none1},
			listing: align.Listing{Best: []string{"G", "K"}, BestRevisions: map[string]release.Revision{"g": v2, "k": none1}},
			groups:  []string{"g", "k"},
			want:    align.OutcomeAligned,
		},
		{
			desc:       "behind best group beside an unlisted one",
			held:       map[string]release.Revision{"g": none1, "h": none1},
			listing:    align.Listing{Best: []string{"G"}, BestRevisions: map[string]release.Revision{"g": v2}},
			groups:     []string{"g", "h"},
			want:       align.OutcomeSuperseded,
			superseded: []string{"g"},
		},
		{
			desc:       "another best group absent from disk",
			held:       map[string]release.Revision{"g": none1},
			listing:    align.Listing{Best: []string{"G", "K"}, BestRevisions: map[string]release.Revision{"g": v2, "k": v2}},
			groups:     []string{"g"},
			want:       align.OutcomeSuperseded,
			superseded: []string{"g"},
		},
		{
			desc:    "no best listed",
			held:    map[string]release.Revision{"g": none1},
			listing: align.Listing{BestRevisions: map[string]release.Revision{"g": v2}},
			groups:  []string{"g"},
			want:    align.OutcomeNoBest,
		},
		{
			desc:       "listing keys compare normalized",
			held:       map[string]release.Revision{"udf": none1},
			listing:    align.Listing{Best: []string{"UDF"}, BestRevisions: map[string]release.Revision{"udf": v2}},
			groups:     []string{"UDF"},
			want:       align.OutcomeSuperseded,
			superseded: []string{"udf"},
		},
	}
}

func TestDecideSeasonRevision(t *testing.T) {
	rec := mapping.Record{Type: "TV", SeasonTvdb: 1}
	for _, tc := range revisionCases() {
		item := library.Item{
			Arr: library.ArrSonarr, HasFile: true,
			SeasonGroups:    map[int][]string{1: tc.groups},
			SeasonRevisions: map[int]map[string]release.Revision{1: tc.held},
		}
		checkRevisionDecision(t, "season", &tc, align.Decide(&item, &align.Entry{Record: &rec}, &tc.listing))
	}
}

func TestDecideMovieRevision(t *testing.T) {
	rec := mapping.Record{Type: "MOVIE"}
	for _, tc := range revisionCases() {
		item := library.Item{Arr: library.ArrRadarr, HasFile: true, Groups: tc.groups, Revisions: tc.held}
		checkRevisionDecision(t, "movie", &tc, align.Decide(&item, &align.Entry{Record: &rec}, &tc.listing))
	}
}

func checkRevisionDecision(t *testing.T, scope string, tc *revisionCase, d align.Decision) {
	t.Helper()
	if d.Outcome != tc.want {
		t.Errorf("Decide(%s, %s).Outcome = %v, want %v", scope, tc.desc, d.Outcome, tc.want)
	}
	if !slices.Equal(d.SupersededGroups, tc.superseded) {
		t.Errorf("Decide(%s, %s).SupersededGroups = %v, want %v", scope, tc.desc, d.SupersededGroups, tc.superseded)
	}
	if tc.want != align.OutcomeSuperseded {
		if d.HeldRevision.Known() || d.ListedRevision.Known() {
			t.Errorf("Decide(%s, %s) revisions = %+v/%+v, want none off the superseded standing", scope, tc.desc, d.HeldRevision, d.ListedRevision)
		}
		return
	}
	if d.Standing != align.StandingBestSuperseded || d.HeldRevision != none1 || d.ListedRevision != v2 {
		t.Errorf("Decide(%s, %s) = standing %v, held %+v, listed %+v; want superseded v1 -> v2", scope, tc.desc, d.Standing, d.HeldRevision, d.ListedRevision)
	}
}

func TestDecideRevisionNeverOverridesPlaceholderOrOffered(t *testing.T) {
	listing := align.Listing{Best: []string{"G"}, BestRevisions: map[string]release.Revision{"g": v2}}
	held := map[int]map[string]release.Revision{0: {"g": none1}, 1: {"g": none1}}
	failed := library.Item{Arr: library.ArrSonarr, Failed: true, SeasonGroups: map[int][]string{1: {"g"}}, SeasonRevisions: held}
	seasonRec := mapping.Record{Type: "TV", SeasonTvdb: 1}
	if d := align.Decide(&failed, &align.Entry{Record: &seasonRec}, &listing); d.Outcome != align.OutcomeUnverifiable {
		t.Errorf("Decide(placeholder).Outcome = %v, want unverifiable", d.Outcome)
	}
	offered := library.Item{Arr: library.ArrSonarr, HasFile: true, SeasonGroups: map[int][]string{0: {"g"}}, SeasonRevisions: held}
	offeredRec := mapping.Record{Type: "MOVIE", TvdbID: 1, SeasonKind: mapping.SeasonPresent}
	if d := align.Decide(&offered, &align.Entry{Record: &offeredRec}, &listing); d.Standing != align.StandingUnverified || d.Kind != align.ScopeOffered {
		t.Errorf("Decide(offered bucket) = %v/%v, want unverified offered", d.Kind, d.Standing)
	}
}

func TestDecideWholeSeriesRevision(t *testing.T) {
	listing := align.Listing{Best: []string{"G", "K"}, Alt: []string{"A"}, BestRevisions: map[string]release.Revision{"g": v2, "k": v2}}
	tests := []struct {
		desc     string
		groups   map[int][]string
		held     map[int]map[string]release.Revision
		siblings []int
		want     align.Standing
	}{
		{
			"one season reissued, one never reissued",
			map[int][]string{1: {"g"}, 2: {"g"}},
			map[int]map[string]release.Revision{1: {"g": v2}, 2: {"g": none1}},
			nil, align.StandingBest,
		},
		{
			"every season behind",
			map[int][]string{1: {"g"}, 2: {"g"}},
			map[int]map[string]release.Revision{1: {"g": none1}, 2: {"g": none1}},
			nil, align.StandingBestSuperseded,
		},
		{
			"an unknown season poisons the fold",
			map[int][]string{1: {"g"}, 2: {"g"}},
			map[int]map[string]release.Revision{2: {"g": none1}},
			nil, align.StandingBest,
		},
		{
			"behind group in one season, current best group in another",
			map[int][]string{1: {"g"}, 2: {"k"}},
			map[int]map[string]release.Revision{1: {"g": none1}, 2: {"k": v2}},
			nil, align.StandingBest,
		},
		{
			"behind season beside an alt season",
			map[int][]string{2: {"g"}, 3: {"a"}},
			map[int]map[string]release.Revision{2: {"g": none1}, 3: {"a": none1}},
			nil, align.StandingAlt,
		},
		{
			"behind season beside an unverifiable season",
			map[int][]string{2: {"g"}, 3: {"nogrp"}},
			map[int]map[string]release.Revision{2: {"g": none1}, 3: {"nogrp": none1}},
			nil, align.StandingBestSuperseded,
		},
		{
			"every season current",
			map[int][]string{1: {"g"}, 2: {"g"}},
			map[int]map[string]release.Revision{1: {"g": v2}, 2: {"g": v3}},
			nil, align.StandingBest,
		},
		{
			"a sibling's reissued season does not lift the fold",
			map[int][]string{1: {"g"}, 2: {"g"}},
			map[int]map[string]release.Revision{1: {"g": none1}, 2: {"g": v2}},
			[]int{2},
			align.StandingBestSuperseded,
		},
	}
	for _, tc := range tests {
		item := library.Item{Arr: library.ArrSonarr, HasFile: true, SeasonGroups: tc.groups, SeasonRevisions: tc.held}
		d := align.Decide(&item, &align.Entry{Record: &wholeRec, SiblingSeasons: tc.siblings}, &listing)
		if d.Standing != tc.want {
			t.Errorf("Decide(whole series, %s).Standing = %v, want %v", tc.desc, d.Standing, tc.want)
			continue
		}
		superseded := tc.want == align.StandingBestSuperseded
		if superseded != (d.Outcome == align.OutcomeSuperseded) || superseded != slices.Equal(d.SupersededGroups, []string{"g"}) {
			t.Errorf("Decide(whole series, %s) = outcome %v, groups %v; want superseded=%t over [g]", tc.desc, d.Outcome, d.SupersededGroups, superseded)
		}
		if superseded && (d.HeldRevision != none1 || d.ListedRevision != v2) {
			t.Errorf("Decide(whole series, %s) revisions = %+v -> %+v, want v1 -> v2", tc.desc, d.HeldRevision, d.ListedRevision)
		}
	}
}

func TestDecideSupersededReportsNewestOnBothSides(t *testing.T) {
	rec := mapping.Record{Type: "TV", SeasonTvdb: 1}
	item := library.Item{
		Arr: library.ArrSonarr, HasFile: true,
		SeasonGroups:    map[int][]string{1: {"g", "k"}},
		SeasonRevisions: map[int]map[string]release.Revision{1: {"g": none1, "k": v2}},
	}
	listing := align.Listing{Best: []string{"G", "K"}, BestRevisions: map[string]release.Revision{"g": v2, "k": v3}}
	d := align.Decide(&item, &align.Entry{Record: &rec}, &listing)
	if d.Outcome != align.OutcomeSuperseded || !slices.Equal(d.SupersededGroups, []string{"g", "k"}) {
		t.Fatalf("Decide(g v1 under v2, k v2 under v3) = outcome %v, groups %v; want superseded over [g k]", d.Outcome, d.SupersededGroups)
	}
	if d.HeldRevision != v2 || d.ListedRevision != v3 {
		t.Errorf("Decide(g v1 under v2, k v2 under v3) revisions = %+v -> %+v, want v2 -> v3", d.HeldRevision, d.ListedRevision)
	}
}
