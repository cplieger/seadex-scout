package audit

import (
	"strings"
	"testing"

	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

func TestVerdict(t *testing.T) {
	tests := []struct {
		name    string
		want    Verdict
		current []string
		best    []string
		alt     []string
		hasFile bool
	}{
		{"no file is no_file", VerdictNoFile, nil, []string{"a"}, nil, false},
		{"file but no identifiable group is unverified", VerdictUnverified, nil, []string{"a"}, nil, true},
		{"current group is best", VerdictBest, []string{"sam"}, []string{"sam"}, nil, true},
		{"current group is an alt", VerdictAlt, []string{"kh"}, []string{"sam"}, []string{"kh"}, true},
		{"current group is unlisted", VerdictUnlisted, []string{"zzz"}, []string{"sam"}, []string{"kh"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := verdict(tt.hasFile, tt.current, tt.best, tt.alt); got != tt.want {
				t.Errorf("verdict = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAuditNotOnSeaDex(t *testing.T) {
	a := NewAuditor(Config{SeaDexBaseURL: "https://releases.moe"})

	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 1, Title: "Covered", TvdbID: 100, SeasonGroups: map[int][]string{1: {"x"}}, Groups: []string{"x"}, HasFile: true},
		{Arr: library.ArrSonarr, ArrID: 2, Title: "UncoveredCatalogued", TvdbID: 200, Groups: []string{"y"}, HasFile: true},
		{Arr: library.ArrSonarr, ArrID: 3, Title: "UncoveredUncatalogued", TvdbID: 300, Groups: []string{"z"}, HasFile: true},
		{Arr: library.ArrRadarr, ArrID: 4, Title: "UncoveredMovie", TmdbID: 400, HasFile: true},
	}}
	idx := mapping.NewIndex([]mapping.Record{
		{AniListID: 1, Type: "TV", TvdbID: 100},
		{AniListID: 2, Type: "TV", TvdbID: 200},
		{AniListID: 4, Type: "MOVIE", TmdbMovies: []int{400}},
	})
	matches := []match.Match{{
		Item:   &snap.Items[0],
		Arr:    library.ArrSonarr,
		Source: match.SourceID,
		Entry:  seadex.Entry{AniListID: 1},
		Record: mapping.Record{Type: "TV", TvdbID: 100, SeasonTvdb: 1},
	}}

	rep := a.Audit(matches, snap, idx)

	got := map[string]bool{}
	for i := range rep.Rows {
		if rep.Rows[i].Verdict == VerdictNotOnSeaDex {
			got[rep.Rows[i].Title] = true
		}
	}
	if !got["UncoveredCatalogued"] {
		t.Error("expected the uncovered catalogued series in not_on_seadex")
	}
	if !got["UncoveredMovie"] {
		t.Error("expected the uncovered catalogued movie in not_on_seadex")
	}
	if got["UncoveredUncatalogued"] {
		t.Error("an uncovered item absent from Fribb must not be listed")
	}
	if got["Covered"] {
		t.Error("a covered item must not be listed as not_on_seadex")
	}
	if n := rep.Totals[string(VerdictNotOnSeaDex)]; n != 2 {
		t.Errorf("not_on_seadex total = %d, want 2", n)
	}
}

// TestAuditNotOnSeaDexHonorsExcludeSpecials pins the exclude_specials symmetry
// (h-f6): with the filter on, a specials-only library item (its only Fribb
// record is an OVA) must not surface as not_on_seadex — matching the
// matched-rows arm, which drops specials — while a mixed series (a sibling TV
// record sharing the TVDB id) stays catalogued and is still listed.
func TestAuditNotOnSeaDexHonorsExcludeSpecials(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 1, Title: "SpecialsOnly", TvdbID: 500, Groups: []string{"g"}, HasFile: true},
		{Arr: library.ArrSonarr, ArrID: 2, Title: "MixedSeries", TvdbID: 600, Groups: []string{"g"}, HasFile: true},
	}}
	idx := mapping.NewIndex([]mapping.Record{
		{AniListID: 1, Type: "OVA", TvdbID: 500},
		{AniListID: 2, Type: "OVA", TvdbID: 600},
		{AniListID: 3, Type: "TV", TvdbID: 600},
	})

	rowsFor := func(exclude bool) map[string]bool {
		a := NewAuditor(Config{SeaDexBaseURL: "https://releases.moe", ExcludeSpecials: exclude})
		rep := a.Audit(nil, snap, idx)
		got := map[string]bool{}
		for i := range rep.Rows {
			if rep.Rows[i].Verdict == VerdictNotOnSeaDex {
				got[rep.Rows[i].Title] = true
			}
		}
		return got
	}

	on := rowsFor(true)
	if on["SpecialsOnly"] {
		t.Error("exclude_specials on: a specials-only item must not surface as not_on_seadex")
	}
	if !on["MixedSeries"] {
		t.Error("exclude_specials on: a mixed series must stay catalogued via its TV record")
	}
	off := rowsFor(false)
	if !off["SpecialsOnly"] {
		t.Error("exclude_specials off: the specials-only item must be listed")
	}
}

// TestAuditNoGroupMatchesBest proves the NoGroup fallback end to end: a
// group-less on-disk release compares equal to a group-less SeaDex best (both
// resolve to NOGRP), yielding have_best rather than an unresolved row.
func TestAuditNoGroupMatchesBest(t *testing.T) {
	a := NewAuditor(Config{SeaDexBaseURL: "https://releases.moe"})
	snap := &library.Snapshot{Items: []library.Item{{
		Arr: library.ArrSonarr, ArrID: 9, Title: "Groupless", TvdbID: 900,
		SeasonGroups: map[int][]string{1: {"nogrp"}}, Groups: []string{"nogrp"}, HasFile: true,
	}}}
	idx := mapping.NewIndex([]mapping.Record{{AniListID: 9, Type: "TV", TvdbID: 900}})
	matches := []match.Match{{
		Item:   &snap.Items[0],
		Arr:    library.ArrSonarr,
		Source: match.SourceID,
		Entry:  seadex.Entry{AniListID: 9, Torrents: []seadex.Torrent{{Tracker: "Nyaa", IsBest: true}}},
		Record: mapping.Record{Type: "TV", TvdbID: 900, SeasonTvdb: 1},
	}}

	rep := a.Audit(matches, snap, idx)

	var got Verdict
	for i := range rep.Rows {
		if rep.Rows[i].AniListID == 9 {
			got = rep.Rows[i].Verdict
		}
	}
	if got != VerdictBest {
		t.Errorf("group-less item vs group-less SeaDex best = %q, want %q", got, VerdictBest)
	}
}

// TestWholeSeriesVerdict covers the conservative per-season aggregation for an
// absolute-numbered / whole-series entry: have_best only when every real season
// carries a best group, downgrading otherwise, with season 0 excluded.
func TestWholeSeriesVerdict(t *testing.T) {
	best := []string{"a&c"}
	alt := []string{"kh"}
	tests := []struct {
		name    string
		seasons map[int][]string
		want    Verdict
		approx  bool
	}{
		{"all seasons best", map[int][]string{1: {"a&c"}, 2: {"a&c"}}, VerdictBest, true},
		{"best plus unlisted downgrades to unlisted", map[int][]string{1: {"a&c"}, 2: {"kitsune"}}, VerdictUnlisted, true},
		{"best plus alt downgrades to alt", map[int][]string{1: {"a&c"}, 2: {"kh"}}, VerdictAlt, true},
		{"season 0 is excluded", map[int][]string{0: {"kitsune"}, 1: {"a&c"}}, VerdictBest, false},
		{"single season is not approx", map[int][]string{1: {"a&c"}}, VerdictBest, false},
		{"single season spanning two groups is approx", map[int][]string{1: {"a&c", "kh"}}, VerdictBest, true},
		{"only season 0 on disk is no_file", map[int][]string{0: {"a&c"}}, VerdictNoFile, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := &library.Item{Arr: library.ArrSonarr, SeasonGroups: tt.seasons, HasFile: true}
			got, _, approx := wholeSeriesVerdict(item, best, alt)
			if got != tt.want {
				t.Errorf("verdict = %q, want %q", got, tt.want)
			}
			if approx != tt.approx {
				t.Errorf("approx = %v, want %v", approx, tt.approx)
			}
		})
	}
}

// TestCatalogueHas covers the reverse-catalogue predicate directly, exercising
// every id path: the Sonarr TVDB match and zero-TVDB short-circuit, and the
// Radarr TMDB-match plus IMDb fallback. Audit only ever reaches has through the
// TMDB/TVDB paths, so the IMDb fallback and the zero-TVDB guard are otherwise
// untested.
func TestCatalogueHas(t *testing.T) {
	cat := newCatalogue(mapping.NewIndex([]mapping.Record{
		{AniListID: 1, Type: "TV", TvdbID: 100},
		{AniListID: 2, Type: "MOVIE", TmdbMovies: []int{400}, IMDbIDs: []string{"tt777"}},
		// Wrong-arm identifiers must not be catalogued (the HasArrIdentifier
		// contract): a MOVIE record's stray TVDB id must not recognize a
		// Sonarr item, nor a series record's movie ids a Radarr item.
		{AniListID: 3, Type: "MOVIE", TvdbID: 555},
		{AniListID: 4, Type: "TV", TmdbMovies: []int{600}, IMDbIDs: []string{"tt888"}},
	}), false)
	tests := []struct {
		name string
		item library.Item
		want bool
	}{
		{"sonarr tvdb matches", library.Item{Arr: library.ArrSonarr, TvdbID: 100}, true},
		{"sonarr tvdb absent", library.Item{Arr: library.ArrSonarr, TvdbID: 999}, false},
		{"sonarr tvdb zero is not catalogued", library.Item{Arr: library.ArrSonarr, TvdbID: 0}, false},
		{"radarr tmdb matches", library.Item{Arr: library.ArrRadarr, TmdbID: 400}, true},
		{"radarr tmdb miss falls through to imdb match", library.Item{Arr: library.ArrRadarr, TmdbID: 401, ImdbID: "tt777"}, true},
		{"radarr imdb only matches", library.Item{Arr: library.ArrRadarr, ImdbID: "tt777"}, true},
		{"radarr neither id matches", library.Item{Arr: library.ArrRadarr, TmdbID: 402, ImdbID: "tt000"}, false},
		{"radarr no ids is not catalogued", library.Item{Arr: library.ArrRadarr}, false},
		{"sonarr not catalogued via a movie record's tvdb id", library.Item{Arr: library.ArrSonarr, TvdbID: 555}, false},
		{"radarr not catalogued via a series record's movie ids", library.Item{Arr: library.ArrRadarr, TmdbID: 600, ImdbID: "tt888"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			it := tt.item
			if got := cat.has(&it); got != tt.want {
				t.Errorf("has() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAuditRoutesWholeSeriesAndSkips exercises Audit's row loop end to end: a
// seasonless non-special Sonarr match routes through the whole-series verdict
// (conservative, approximate over two seasons), a match not in the library is
// skipped, an excluded special is skipped, and a nil snapshot/index adds no
// not_on_seadex rows.
func TestAuditRoutesWholeSeriesAndSkips(t *testing.T) {
	a := NewAuditor(Config{SeaDexBaseURL: "https://releases.moe", ExcludeSpecials: true})
	inLib := library.Item{
		Arr: library.ArrSonarr, ArrID: 1, Title: "Absolute Run", TvdbID: 100,
		SeasonGroups: map[int][]string{1: {"a&c"}, 2: {"zzz"}},
		Groups:       []string{"a&c", "zzz"}, HasFile: true,
	}
	matches := []match.Match{
		{ // seasonless non-special: routed through the whole-series verdict
			Item: &inLib, Arr: library.ArrSonarr, Source: match.SourceID,
			Entry:  seadex.Entry{AniListID: 1, Torrents: []seadex.Torrent{{Tracker: "Nyaa", ReleaseGroup: "A&C", IsBest: true}}},
			Record: mapping.Record{Type: "TV", TvdbID: 100},
		},
		{ // not in the library: skipped entirely
			Arr: library.ArrSonarr, Entry: seadex.Entry{AniListID: 2}, Record: mapping.Record{Type: "TV"},
		},
		{ // special with exclude_specials on: skipped
			Item: &inLib, Arr: library.ArrSonarr, Source: match.SourceID,
			Entry:  seadex.Entry{AniListID: 3},
			Record: mapping.Record{Type: "OVA", TvdbID: 100},
		},
	}

	rep := a.Audit(matches, nil, nil)

	if len(rep.Rows) != 1 {
		t.Fatalf("rows = %d, want 1 (not-in-library and excluded special skipped; nil snapshot adds nothing)", len(rep.Rows))
	}
	row := rep.Rows[0]
	if row.Verdict != VerdictUnlisted {
		t.Errorf("whole-series verdict = %q, want have_unlisted (season 2 carries an unlisted group)", row.Verdict)
	}
	if !row.Approx {
		t.Error("a two-season whole-series comparison must be marked approximate")
	}
	if rep.Totals[string(VerdictUnlisted)] != 1 {
		t.Errorf("have_unlisted total = %d, want 1", rep.Totals[string(VerdictUnlisted)])
	}
}

// TestAuditMislabeledAnimeBytesURLHiddenWhenOff proves the URL-aware AB guard:
// a torrent whose untrusted tracker label says "Nyaa" but whose URL points at
// animebytes.tv - absolute, schemeless, or host:port - must be dropped from
// the report's releases while the AnimeBytes toggle is off, exactly like a
// correctly labeled AB torrent (the guard reads the RAW upstream URL).
func TestAuditMislabeledAnimeBytesURLHiddenWhenOff(t *testing.T) {
	for _, sneakyURL := range []string{
		"https://animebytes.tv/torrents.php?id=9&torrentid=10",
		"animebytes.tv/torrents.php?id=9&torrentid=10",
		"animebytes.tv:443/torrents.php?id=9&torrentid=10",
	} {
		entry := seadex.Entry{AniListID: 11, Torrents: []seadex.Torrent{
			{Tracker: "Nyaa", URL: sneakyURL, ReleaseGroup: "Sneaky", IsBest: true},
			{Tracker: "Nyaa", URL: "https://nyaa.si/view/11", ReleaseGroup: "Honest", IsBest: true},
		}}
		snap := &library.Snapshot{Items: []library.Item{{
			Arr: library.ArrSonarr, ArrID: 11, Title: "Mislabeled", TvdbID: 1100,
			SeasonGroups: map[int][]string{1: {"honest"}}, Groups: []string{"honest"}, HasFile: true,
		}}}
		matches := []match.Match{{
			Item:   &snap.Items[0],
			Arr:    library.ArrSonarr,
			Source: match.SourceID,
			Entry:  entry,
			Record: mapping.Record{Type: "TV", TvdbID: 1100, SeasonTvdb: 1},
		}}

		for _, tt := range []struct {
			name       string
			animeBytes bool
			wantSneaky bool
		}{
			{"AB off omits the mislabeled release", false, false},
			{"AB on keeps it", true, true},
		} {
			t.Run(sneakyURL+" "+tt.name, func(t *testing.T) {
				a := NewAuditor(Config{SeaDexBaseURL: "https://releases.moe", AnimeBytes: tt.animeBytes})
				rep := a.Audit(matches, snap, mapping.NewIndex(nil))
				var row *Row
				for i := range rep.Rows {
					if rep.Rows[i].AniListID == 11 {
						row = &rep.Rows[i]
					}
				}
				if row == nil {
					t.Fatal("expected a row for the matched entry")
				}
				gotSneaky := false
				for _, r := range row.Releases {
					if r.Group == "Sneaky" || strings.Contains(r.URL, "animebytes.tv") {
						gotSneaky = true
					}
				}
				if gotSneaky != tt.wantSneaky {
					t.Errorf("mislabeled AB-URL release present = %v, want %v (releases: %+v)", gotSneaky, tt.wantSneaky, row.Releases)
				}
			})
		}
	}
}

// TestSortRowsOrdersByVerdictThenTitle pins the report's row ordering: rows
// group by verdict actionability (verdictOrder: unlisted, alt, unverified,
// no_file, best, not_on_seadex) and, within a verdict, sort by title
// case-insensitively. The 2026-07-13 gremlins tracker confirmed sortRows'
// comparator had no killing test (CONDITIONALS_NEGATION mutants LIVED in all
// 3 runs on both the rank and the title comparisons).
func TestSortRowsOrdersByVerdictThenTitle(t *testing.T) {
	rows := []Row{
		{Title: "zeta", Verdict: VerdictBest},
		{Title: "Beta", Verdict: VerdictUnlisted},
		{Title: "gamma", Verdict: VerdictNotOnSeaDex},
		{Title: "alpha", Verdict: VerdictBest},
		{Title: "delta", Verdict: VerdictNoFile},
		{Title: "epsilon", Verdict: VerdictUnverified},
		{Title: "omega", Verdict: VerdictAlt},
		{Title: "ALPHA2", Verdict: VerdictUnlisted},
	}

	sortRows(rows)

	want := []struct {
		title   string
		verdict Verdict
	}{
		{"ALPHA2", VerdictUnlisted}, // case-insensitive: "alpha2" < "beta"
		{"Beta", VerdictUnlisted},
		{"omega", VerdictAlt},
		{"epsilon", VerdictUnverified},
		{"delta", VerdictNoFile},
		{"alpha", VerdictBest},
		{"zeta", VerdictBest},
		{"gamma", VerdictNotOnSeaDex},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %d, want %d", len(rows), len(want))
	}
	for i, w := range want {
		if rows[i].Title != w.title || rows[i].Verdict != w.verdict {
			t.Errorf("rows[%d] = %q/%q, want %q/%q", i, rows[i].Title, rows[i].Verdict, w.title, w.verdict)
		}
	}
}

// TestRowQualifier pins the daemon-vocabulary qualifier: theoretical/incomplete
// when SeaDex lists no best at all (theoretical taking precedence, mirroring
// the daemon's emptyResult), mixed only on a not-aligned multi-group row, and
// empty everywhere else (an aligned row is never mixed - alignment wins).
func TestRowQualifier(t *testing.T) {
	tests := []struct {
		name    string
		entry   seadex.Entry
		best    []string
		verdict Verdict
		current []string
		want    Qualifier
	}{
		{"theoretical-only entry", seadex.Entry{TheoreticalBest: "remux"}, nil, VerdictUnlisted, []string{"a"}, QualifierTheoretical},
		{"theoretical wins over incomplete", seadex.Entry{TheoreticalBest: "remux", Incomplete: true}, nil, VerdictUnlisted, []string{"a"}, QualifierTheoretical},
		{"incomplete with nothing recommended", seadex.Entry{Incomplete: true}, nil, VerdictUnlisted, []string{"a"}, QualifierIncomplete},
		{"no best and neither flag is unqualified", seadex.Entry{}, nil, VerdictUnlisted, []string{"a"}, ""},
		{"not-aligned multi-group is mixed", seadex.Entry{}, []string{"sam"}, VerdictUnlisted, []string{"a", "b"}, QualifierMixed},
		{"not-aligned alt multi-group is mixed", seadex.Entry{}, []string{"sam"}, VerdictAlt, []string{"a", "b"}, QualifierMixed},
		{"aligned multi-group is not mixed", seadex.Entry{}, []string{"a"}, VerdictBest, []string{"a", "b"}, ""},
		{"not-aligned single group is not mixed", seadex.Entry{}, []string{"sam"}, VerdictUnlisted, []string{"a"}, ""},
		{"no_file with best listed is unqualified", seadex.Entry{}, []string{"sam"}, VerdictNoFile, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rowQualifier(&tt.entry, tt.best, tt.verdict, tt.current); got != tt.want {
				t.Errorf("rowQualifier() = %q, want %q", got, tt.want)
			}
		})
	}
}
