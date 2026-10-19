package align_test

import (
	"testing"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
)

// TestDecidePlaceholderItemIsUnverifiedNotNoFile pins the placeholder branch: a
// library.Item whose file state could not be READ (library.Item.Failed) has no
// group evidence at all, so the standing is unverified and Groups stays nil.
// StandingNoFile would assert the unit has nothing on disk, the false claim
// Comparable exists to stop. Pinned HERE because the audit report is the consumer
// relying on Decide to handle the placeholder (the daemon's compare pass drops
// these matches beforehand) and no other align test passes Decide a Failed item.
func TestDecidePlaceholderItemIsUnverifiedNotNoFile(t *testing.T) {
	movieRec := mapping.Record{Type: "MOVIE"}
	for name, item := range map[string]library.Item{
		"failed movie":                   {Arr: library.ArrRadarr, Failed: true},
		"failed movie Radarr filed":      {Arr: library.ArrRadarr, Failed: true, HasFile: true},
		"failed series with no groups":   {Arr: library.ArrSonarr, Failed: true},
		"failed series with read groups": {Arr: library.ArrSonarr, Failed: true, SeasonGroups: map[int][]string{1: {"sam"}}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := movieRec
			if item.Arr == library.ArrSonarr {
				rec = mapping.Record{Type: "TV", SeasonTvdb: 1}
			}
			d := align.Decide(&item, &align.Entry{Record: &rec}, &align.Listing{Best: []string{"sam"}})
			if d.Standing != align.StandingUnverified {
				t.Errorf("Standing = %v, want %v (a placeholder's file state is MISSING, not empty)", d.Standing, align.StandingUnverified)
			}
			if d.Outcome != align.OutcomeUnverifiable {
				t.Errorf("Outcome = %v, want %v", d.Outcome, align.OutcomeUnverifiable)
			}
			if len(d.Groups) != 0 {
				t.Errorf("Groups = %v, want empty (no groups were read, so none may be reported)", d.Groups)
			}
			if d.NoBest {
				t.Error("NoBest = true, want false (the best set is non-empty)")
			}
		})
	}
}

// TestDecideSingleUnit pins the file-first group ladder and the outcome
// linearization for the single-unit scopes: no file wins over everything including
// the no-best nudge, a group-less filed unit is unverified (unverifiable rather
// than mixed or diverged), a proven best group aligns however many groups the unit
// spans, and a not-aligned all-known unit is mixed exactly when it spans several.
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
			name:         "filed movie with no identifiable group is unverified and unverifiable",
			item:         library.Item{Arr: library.ArrRadarr, HasFile: true},
			rec:          movieRec,
			best:         []string{"sam"},
			wantStanding: align.StandingUnverified,
			wantOutcome:  align.OutcomeUnverifiable,
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
			d := align.Decide(&tt.item, &align.Entry{Record: &tt.rec}, &align.Listing{Best: tt.best, Alt: tt.alt})
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
	d := align.Decide(&item, &align.Entry{Record: &rec}, &align.Listing{Best: []string{"sam"}})
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

// TestDecideTriStateEvidence pins the three-valued evidence model: unknown group
// evidence (release.NoGroup) on ONE side yields StandingUnverified and
// OutcomeUnverifiable, never a confident alignment and never a divergence, while
// NoGroup on both sides aligns and a known-known best match wins outright even
// beside unknown members.
// Unverifiability of the best comparison short-circuits BEFORE the alt rung: when
// "do you have the best?" is unanswerable, a proven alt must not imply you lack it.
func TestDecideTriStateEvidence(t *testing.T) {
	seasonRec := mapping.Record{Type: "TV", SeasonTvdb: 1}
	tests := []struct {
		name         string
		item         library.Item
		best         []string
		alt          []string
		wantStanding align.Standing
		wantOutcome  align.Outcome
		wantNoBest   bool
	}{
		{
			name:         "unknown library evidence against a known best is unverifiable",
			item:         library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{1: {"nogrp"}}},
			best:         []string{"sam"},
			wantStanding: align.StandingUnverified,
			wantOutcome:  align.OutcomeUnverifiable,
		},
		{
			name:         "known library evidence against an unknown-only best is unverifiable",
			item:         library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{1: {"sam"}}},
			best:         []string{"nogrp"},
			wantStanding: align.StandingUnverified,
			wantOutcome:  align.OutcomeUnverifiable,
		},
		{
			name:         "sentinel on both sides aligns",
			item:         library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{1: {"nogrp"}}},
			best:         []string{"nogrp"},
			wantStanding: align.StandingBest,
			wantOutcome:  align.OutcomeAligned,
		},
		{
			name:         "unknown member beside a known best match still aligns",
			item:         library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{1: {"sam", "nogrp"}}},
			best:         []string{"sam"},
			wantStanding: align.StandingBest,
			wantOutcome:  align.OutcomeAligned,
		},
		{
			name:         "unknown member beside a known miss is unverifiable, not mixed or diverged",
			item:         library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{1: {"kh", "nogrp"}}},
			best:         []string{"sam"},
			wantStanding: align.StandingUnverified,
			wantOutcome:  align.OutcomeUnverifiable,
		},
		{
			name:         "unverifiable best comparison short-circuits a proven alt",
			item:         library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{1: {"kh", "nogrp"}}},
			best:         []string{"sam"},
			alt:          []string{"kh"},
			wantStanding: align.StandingUnverified,
			wantOutcome:  align.OutcomeUnverifiable,
		},
		{
			// The deliberately conservative sub-case: best-divergence is PROVEN
			// (current "kh" vs best "sam", all evidence known) and only the alt
			// placement is indeterminate, so the row says "check this" rather than
			// "you have an unlisted release".
			name:         "proven-divergent best with an unknown-only alt is unverified",
			item:         library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{1: {"kh"}}},
			best:         []string{"sam"},
			alt:          []string{"nogrp"},
			wantStanding: align.StandingUnverified,
			wantOutcome:  align.OutcomeUnverifiable,
		},
		{
			name:         "no-best nudge outranks an indeterminate alt comparison",
			item:         library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{1: {"nogrp"}}},
			alt:          []string{"kh"},
			wantStanding: align.StandingUnverified,
			wantOutcome:  align.OutcomeNoBest,
			wantNoBest:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := align.Decide(&tt.item, &align.Entry{Record: &seasonRec}, &align.Listing{Best: tt.best, Alt: tt.alt})
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

// TestDecideSeasonLabel pins the shared non-negative season label the
// consumers stamp on their output (Decision.Season): a positive mapped TVDB
// season passes through, and a zero or negative mapping (a negative value is
// only ever an override's) never reaches the season scope at all, so the
// label stays 0 instead of leaking a negative number into the daemon
// findings and audit rows.
func TestDecideSeasonLabel(t *testing.T) {
	tests := []struct {
		name       string
		recType    string
		arr        string
		seasonTvdb int
		want       int
	}{
		{"positive season passes through", "TV", library.ArrSonarr, 2, 2},
		{"negative absolute-numbered mapping scopes whole-series and carries 0", "TV", library.ArrSonarr, -1, 0},
		{"movie carries 0", "MOVIE", library.ArrRadarr, 0, 0},
		{"special carries 0", "OVA", library.ArrSonarr, 0, 0},
		{"movie ignores irrelevant positive season metadata", "MOVIE", library.ArrRadarr, 2, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := library.Item{Arr: tt.arr, HasFile: true, SeasonGroups: map[int][]string{2: {"sam"}}}
			rec := mapping.Record{Type: tt.recType, SeasonTvdb: tt.seasonTvdb}
			if d := align.Decide(&item, &align.Entry{Record: &rec}, &align.Listing{Best: []string{"sam"}}); d.Season != tt.want {
				t.Errorf("Season = %d, want %d", d.Season, tt.want)
			}
		})
	}
}

// TestDecideSingleUnitApproxPassThrough pins that Decide carries the scope
// layer's approximation flag for a single-unit comparison: a multi-group
// season-0 specials bucket stays approximate through the shared decision, so
// a dropped pass-through in Decide cannot silently render every coarse audit
// row exact.
func TestDecideSingleUnitApproxPassThrough(t *testing.T) {
	item := library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{0: {"cait-sidhe", "sallysubs"}}}
	rec := mapping.Record{Type: "OVA"}
	d := align.Decide(&item, &align.Entry{Record: &rec}, &align.Listing{Best: []string{"cait-sidhe"}})
	if !d.Approx {
		t.Error("Approx = false, want true (multi-group specials bucket is approximate)")
	}
	if d.Kind != align.ScopeOffered {
		t.Errorf("Kind = %v, want ScopeOffered", d.Kind)
	}
}

// TestDecideWithholdsANonAiredOrderSeries pins Decide's first guard: a
// Sonarr series numbered in another order than TheTVDB's aired one cannot be
// mapped to the TVDB seasons SeaDex entries are placed in, so every comparison
// on it is withheld for the order, whatever its files hold or whether they
// were read, with the record's scope kind and season; the same item in aired
// order is compared as before.
func TestDecideWithholdsANonAiredOrderSeries(t *testing.T) {
	bookOrder := &library.Order{Type: "alternate", Name: "Book Order"}
	monogatari := func() library.Item {
		return library.Item{
			Arr: library.ArrSonarr, ArrID: 2189, Title: "Monogatari", HasFile: true,
			SeasonGroups: map[int][]string{0: {"sp"}, 1: {"wrong"}, 2: {"wrong"}},
			Specials:     map[int]library.ListedEpisode{3: {Group: "sp", HasFile: true}},
			Order:        bookOrder,
		}
	}
	listing := &align.Listing{Best: []string{"best"}}
	tests := []struct {
		name       string
		mutate     func(*library.Item)
		entry      align.Entry
		wantKind   align.ScopeKind
		wantSeason int
	}{
		{name: "mapped season", entry: align.Entry{Record: &mapping.Record{Type: "TV", SeasonTvdb: 2, SeasonKind: mapping.SeasonPresent}}, wantKind: align.ScopeSeason, wantSeason: 2},
		{name: "whole series", entry: align.Entry{Record: &mapping.Record{Type: "TV", SeasonKind: mapping.SeasonAbsent}}, wantKind: align.ScopeWholeSeries},
		{name: "placed special", entry: align.Entry{Record: &mapping.Record{Type: "OVA", SeasonKind: mapping.SeasonPresent}, Specials: []int{3}}, wantKind: align.ScopeOffered},
		{name: "season with no file", mutate: func(it *library.Item) { it.SeasonGroups = nil; it.HasFile = false }, entry: align.Entry{Record: &mapping.Record{Type: "TV", SeasonTvdb: 2, SeasonKind: mapping.SeasonPresent}}, wantKind: align.ScopeSeason, wantSeason: 2},
		{name: "failed placeholder", mutate: func(it *library.Item) { *it = library.Item{Arr: library.ArrSonarr, Failed: true, Order: bookOrder} }, entry: align.Entry{Record: &mapping.Record{Type: "TV", SeasonTvdb: 1, SeasonKind: mapping.SeasonPresent}}, wantKind: align.ScopeSeason, wantSeason: 1},
		// arrapi maps an empty Sonarr v5 seasonType to Order{}, an unknown order,
		// never aired (arrapi sonarr/engine_sonarr_v5_api_v5.go, sonarrV5Series.order).
		{name: "unknown order", mutate: func(it *library.Item) { it.Order = &library.Order{} }, entry: align.Entry{Record: &mapping.Record{Type: "TV", SeasonTvdb: 2, SeasonKind: mapping.SeasonPresent}}, wantKind: align.ScopeSeason, wantSeason: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := monogatari()
			if tt.mutate != nil {
				tt.mutate(&item)
			}
			d := align.Decide(&item, &tt.entry, listing)
			if d.Outcome != align.OutcomeWithheld || d.Withheld != align.WithheldOrder {
				t.Errorf("Outcome, Withheld = %v, %v, want %v, %v", d.Outcome, d.Withheld, align.OutcomeWithheld, align.WithheldOrder)
			}
			if d.Kind != tt.wantKind || d.Season != tt.wantSeason {
				t.Errorf("Kind, Season = %v, %d, want %v, %d (the record's)", d.Kind, d.Season, tt.wantKind, tt.wantSeason)
			}
			if len(d.Groups) != 0 || len(d.Episodes) != 0 {
				t.Errorf("Groups, Episodes = %v, %v, want none: nothing was judged", d.Groups, d.Episodes)
			}
		})
	}
	t.Run("aired order compares", func(t *testing.T) {
		item := monogatari()
		item.Order = nil
		d := align.Decide(&item, &align.Entry{Record: &mapping.Record{Type: "TV", SeasonTvdb: 2, SeasonKind: mapping.SeasonPresent}}, listing)
		if d.Outcome != align.OutcomeDiverged || d.Withheld != align.WithheldNone {
			t.Errorf("Outcome, Withheld = %v, %v, want %v, %v", d.Outcome, d.Withheld, align.OutcomeDiverged, align.WithheldNone)
		}
	})
}

// TestDecideAniListMembershipGuard pins that a season or whole-series
// comparison on a Sonarr series whose AniList ids exclude the entry's is
// withheld for AniList, while a member id or an empty list (Sonarr 4, or a
// series Sonarr links to no AniList entry) compares. Offered and episodes
// scopes are never withheld by it: Sonarr's list names only some of a series'
// films and specials, so one missing from it proves nothing, and an entry
// placed on part of a season is exempt too. The ids are Code Geass's
// aniListIds in Sonarr 5's /api/v5/series answer; 20 is outside them.
func TestDecideAniListMembershipGuard(t *testing.T) {
	codeGeass := []int{1575, 2904, 12685, 97880}
	series := func(ids []int) library.Item {
		return library.Item{
			Arr: library.ArrSonarr, ArrID: 33, Title: "Code Geass", HasFile: true,
			SeasonGroups: map[int][]string{0: {"wrong"}, 1: {"wrong"}, 2: {"wrong"}},
			Specials:     map[int]library.ListedEpisode{3: {Group: "wrong", HasFile: true}},
			Listed: map[int]map[int]library.ListedEpisode{1: {
				1: {Group: "wrong", HasFile: true}, 2: {Group: "wrong", HasFile: true},
			}},
			AniListIDs: ids,
		}
	}
	seasonRec := &mapping.Record{Type: "TV", SeasonTvdb: 2, SeasonKind: mapping.SeasonPresent}
	wholeRec := &mapping.Record{Type: "TV", SeasonKind: mapping.SeasonAbsent}
	listing := &align.Listing{Best: []string{"best"}}
	tests := []struct {
		name         string
		ids          []int
		entry        align.Entry
		wantKind     align.ScopeKind
		wantWithheld bool
	}{
		{name: "season non-member", ids: codeGeass, entry: align.Entry{Record: seasonRec, AniListID: 20}, wantKind: align.ScopeSeason, wantWithheld: true},
		{name: "season member", ids: codeGeass, entry: align.Entry{Record: seasonRec, AniListID: 2904}, wantKind: align.ScopeSeason},
		{name: "season empty list", entry: align.Entry{Record: seasonRec, AniListID: 20}, wantKind: align.ScopeSeason},
		{name: "whole series non-member", ids: codeGeass, entry: align.Entry{Record: wholeRec, AniListID: 20}, wantKind: align.ScopeWholeSeries, wantWithheld: true},
		{name: "whole series member", ids: codeGeass, entry: align.Entry{Record: wholeRec, AniListID: 1575}, wantKind: align.ScopeWholeSeries},
		{name: "whole series empty list", ids: []int{}, entry: align.Entry{Record: wholeRec, AniListID: 20}, wantKind: align.ScopeWholeSeries},
		{name: "offered non-member", ids: codeGeass, entry: align.Entry{Record: &mapping.Record{Type: "OVA", SeasonKind: mapping.SeasonPresent}, AniListID: 20}, wantKind: align.ScopeOffered},
		{name: "special episodes non-member", ids: codeGeass, entry: align.Entry{Record: &mapping.Record{Type: "OVA", SeasonKind: mapping.SeasonPresent}, Specials: []int{3}, AniListID: 20}, wantKind: align.ScopeEpisodes},
		{name: "season episodes non-member", ids: codeGeass, entry: align.Entry{Record: &mapping.Record{Type: "TV", SeasonTvdb: 1, SeasonKind: mapping.SeasonPresent}, Episodes: []int{1}, AniListID: 20}, wantKind: align.ScopeEpisodes},
		// A placement that does not resolve to part of the season (every listed
		// episode, an episode Sonarr does not list, a season whose listing is
		// unknown) is a season comparison, so the guard applies to it.
		{name: "placed on every listed episode non-member", ids: codeGeass, entry: align.Entry{Record: &mapping.Record{Type: "TV", SeasonTvdb: 1, SeasonKind: mapping.SeasonPresent}, Episodes: []int{1, 2}, AniListID: 20}, wantKind: align.ScopeSeason, wantWithheld: true},
		{name: "placed on an unlisted episode non-member", ids: codeGeass, entry: align.Entry{Record: &mapping.Record{Type: "TV", SeasonTvdb: 1, SeasonKind: mapping.SeasonPresent}, Episodes: []int{3}, AniListID: 20}, wantKind: align.ScopeSeason, wantWithheld: true},
		{name: "placed on an unlisted season non-member", ids: codeGeass, entry: align.Entry{Record: seasonRec, Episodes: []int{1}, AniListID: 20}, wantKind: align.ScopeSeason, wantWithheld: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := series(tt.ids)
			d := align.Decide(&item, &tt.entry, listing)
			if d.Kind != tt.wantKind {
				t.Errorf("Kind = %v, want %v", d.Kind, tt.wantKind)
			}
			wantReason := align.WithheldNone
			if tt.wantWithheld {
				wantReason = align.WithheldAniList
			}
			if got := align.Withholding(&item, &tt.entry); got != wantReason {
				t.Errorf("Withholding = %v, want %v", got, wantReason)
			}
			if tt.wantWithheld {
				if d.Outcome != align.OutcomeWithheld || d.Withheld != align.WithheldAniList {
					t.Errorf("Outcome, Withheld = %v, %v, want %v, %v", d.Outcome, d.Withheld, align.OutcomeWithheld, align.WithheldAniList)
				}
				if len(d.Groups) != 0 {
					t.Errorf("Groups = %v, want none: nothing was judged", d.Groups)
				}
				return
			}
			if d.Outcome == align.OutcomeWithheld || d.Withheld != align.WithheldNone {
				t.Errorf("Outcome, Withheld = %v, %v, want a compared outcome and no reason", d.Outcome, d.Withheld)
			}
		})
	}
	t.Run("order outranks AniList", func(t *testing.T) {
		item := series(codeGeass)
		item.Order = &library.Order{Type: "dvd"}
		d := align.Decide(&item, &align.Entry{Record: seasonRec, AniListID: 20}, listing)
		if d.Withheld != align.WithheldOrder {
			t.Errorf("Withheld = %v, want %v (the order guard runs first)", d.Withheld, align.WithheldOrder)
		}
	})
}

// TestDecideAniListGuardOnAFailedPlaceholder pins that the guard reads no
// file evidence: on a series whose file read failed it withholds a season or
// whole-series entry the ids exclude, as on a walked one. An entry the map
// places on episodes of its season is not withheld there, because the episode
// listing the placeholder lacks could place it on part of the season, where
// the guard does not apply; its file state is unknown, so it is unverifiable.
func TestDecideAniListGuardOnAFailedPlaceholder(t *testing.T) {
	failed := library.Item{Arr: library.ArrSonarr, ArrID: 33, Title: "Code Geass", Failed: true, AniListIDs: []int{1575, 2904, 12685, 97880}}
	listing := &align.Listing{Best: []string{"best"}}
	seasonRec := &mapping.Record{Type: "TV", SeasonTvdb: 1, SeasonKind: mapping.SeasonPresent}
	tests := []struct {
		name         string
		entry        align.Entry
		wantWithheld align.WithholdReason
		wantOutcome  align.Outcome
	}{
		{name: "season non-member", entry: align.Entry{Record: seasonRec, AniListID: 20}, wantWithheld: align.WithheldAniList, wantOutcome: align.OutcomeWithheld},
		{name: "whole series non-member", entry: align.Entry{Record: &mapping.Record{Type: "TV", SeasonKind: mapping.SeasonAbsent}, AniListID: 20}, wantWithheld: align.WithheldAniList, wantOutcome: align.OutcomeWithheld},
		{name: "season member", entry: align.Entry{Record: seasonRec, AniListID: 2904}, wantOutcome: align.OutcomeUnverifiable},
		{name: "placed on season episodes non-member", entry: align.Entry{Record: seasonRec, Episodes: []int{1}, AniListID: 20}, wantOutcome: align.OutcomeUnverifiable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := failed
			d := align.Decide(&item, &tt.entry, listing)
			if d.Outcome != tt.wantOutcome || d.Withheld != tt.wantWithheld {
				t.Errorf("Outcome, Withheld = %v, %v, want %v, %v", d.Outcome, d.Withheld, tt.wantOutcome, tt.wantWithheld)
			}
			if got := align.Withholding(&item, &tt.entry); got != tt.wantWithheld {
				t.Errorf("Withholding = %v, want %v, Decide's reason", got, tt.wantWithheld)
			}
		})
	}
}
