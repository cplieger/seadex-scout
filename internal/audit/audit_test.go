package audit

import (
	"reflect"
	"strings"
	"testing"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// TestVerdictFor pins the 1:1 rendering of the shared decision core's
// group-ladder standing in the report's verdict vocabulary.
func TestVerdictFor(t *testing.T) {
	tests := []struct {
		name     string
		standing align.Standing
		want     Verdict
	}{
		{"no file", align.StandingNoFile, VerdictNoFile},
		{"unverified", align.StandingUnverified, VerdictUnverified},
		{"best", align.StandingBest, VerdictBest},
		{"alt", align.StandingAlt, VerdictAlt},
		{"unlisted", align.StandingUnlisted, VerdictUnlisted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := verdictFor(tt.standing); got != tt.want {
				t.Errorf("verdictFor(%v) = %q, want %q", tt.standing, got, tt.want)
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

	rep := a.Audit(matches, snap, idx, nil)

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
		rep := a.Audit(nil, snap, idx, nil)
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

// TestAuditUnknownGroupEvidenceIsUnverified pins the tri-state evidence model
// end to end through the audit (deliberately INVERTING the former
// TestAuditNoGroupMatchesBest, which pinned the sentinel-identity defect): the
// NoGroup sentinel is unknown evidence, never an identity token, so a
// group-less on-disk release against a group-less SeaDex best reads
// unverified - "we could not verify either side" - rather than have_best, and
// unknown evidence on EITHER side alone (a NOGRP-only library item against a
// known best, or a known library group against a NOGRP-only best torrent)
// yields the same unverified verdict instead of have_unlisted.
func TestAuditUnknownGroupEvidenceIsUnverified(t *testing.T) {
	a := NewAuditor(Config{SeaDexBaseURL: "https://releases.moe"})
	tests := []struct {
		name      string
		diskGroup string
		bestGroup string // "" classifies to the NoGroup sentinel
	}{
		{name: "sentinel on both sides is not alignment proof", diskGroup: "nogrp", bestGroup: ""},
		{name: "NOGRP-only library item against a known best", diskGroup: "nogrp", bestGroup: "SEV"},
		{name: "known library group against a NOGRP-only best", diskGroup: "sev", bestGroup: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := &library.Snapshot{Items: []library.Item{{
				Arr: library.ArrSonarr, ArrID: 9, Title: "Groupless", TvdbID: 900,
				SeasonGroups: map[int][]string{1: {tt.diskGroup}}, Groups: []string{tt.diskGroup}, HasFile: true,
			}}}
			idx := mapping.NewIndex([]mapping.Record{{AniListID: 9, Type: "TV", TvdbID: 900}})
			matches := []match.Match{{
				Item:   &snap.Items[0],
				Arr:    library.ArrSonarr,
				Source: match.SourceID,
				Entry:  seadex.Entry{AniListID: 9, Torrents: []seadex.Torrent{{Tracker: "Nyaa", ReleaseGroup: tt.bestGroup, IsBest: true, URL: "https://nyaa.si/view/9"}}},
				Record: mapping.Record{Type: "TV", TvdbID: 900, SeasonTvdb: 1},
			}}

			rep := a.Audit(matches, snap, idx, nil)

			var row *Row
			for i := range rep.Rows {
				if rep.Rows[i].AniListID == 9 {
					row = &rep.Rows[i]
				}
			}
			if row == nil {
				t.Fatal("expected a row for the matched entry")
			}
			if row.Verdict != VerdictUnverified {
				t.Errorf("verdict = %q, want %q (unknown evidence proves neither alignment nor divergence)", row.Verdict, VerdictUnverified)
			}
			if row.Qualifier != "" {
				t.Errorf("qualifier = %q, want none (the unverified verdict itself carries the story)", row.Qualifier)
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
			Entry:  seadex.Entry{AniListID: 1, Torrents: []seadex.Torrent{{Tracker: "Nyaa", ReleaseGroup: "A&C", IsBest: true, URL: "https://nyaa.si/view/1"}}},
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

	rep := a.Audit(matches, nil, nil, nil)

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
// a torrent whose untrusted tracker label says "Nyaa" but whose URL carries
// DEFINITIVE animebytes.tv host evidence - absolute or schemeless - must be
// dropped from the report's releases while the AnimeBytes toggle is off,
// exactly like a correctly labeled AB torrent (the guard reads the RAW
// upstream URL). The host:port form hides its host evidence (net/url parses
// the host as an opaque scheme), so it is NOT definitive: its row stays
// LISTED - link dropped, annotated unobtainable - rather than erased, while
// the AB link itself still never surfaces.
func TestAuditMislabeledAnimeBytesURLHiddenWhenOff(t *testing.T) {
	for _, tc := range []struct {
		sneakyURL  string
		definitive bool
	}{
		{"https://animebytes.tv/torrents.php?id=9&torrentid=10", true},
		{"animebytes.tv/torrents.php?id=9&torrentid=10", true},
		{"animebytes.tv:443/torrents.php?id=9&torrentid=10", false},
	} {
		entry := seadex.Entry{AniListID: 11, Torrents: []seadex.Torrent{
			{Tracker: "Nyaa", URL: tc.sneakyURL, ReleaseGroup: "Sneaky", IsBest: true},
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
			{"AB off", false, !tc.definitive},
			{"AB on keeps it", true, true},
		} {
			t.Run(tc.sneakyURL+" "+tt.name, func(t *testing.T) {
				a := NewAuditor(Config{SeaDexBaseURL: "https://releases.moe", AnimeBytes: tt.animeBytes})
				rep := a.Audit(matches, snap, mapping.NewIndex(nil), nil)
				var row *Row
				for i := range rep.Rows {
					if rep.Rows[i].AniListID == 11 {
						row = &rep.Rows[i]
					}
				}
				if row == nil {
					t.Fatal("expected a row for the matched entry")
				}
				var sneaky *Release
				for i := range row.Releases {
					if row.Releases[i].Group == "Sneaky" {
						sneaky = &row.Releases[i]
					}
				}
				if got := sneaky != nil; got != tt.wantSneaky {
					t.Errorf("mislabeled AB-URL release present = %v, want %v (releases: %+v)", got, tt.wantSneaky, row.Releases)
				}
				if !tt.animeBytes {
					// Whatever the row visibility, the AB link must never
					// surface while the toggle is off.
					for _, r := range row.Releases {
						if strings.Contains(r.URL, "animebytes.tv") {
							t.Errorf("AB link surfaced with the toggle off: %q", r.URL)
						}
					}
					if sneaky != nil {
						if !sneaky.Unobtainable {
							t.Error("ambiguous-evidence release listed but not marked unobtainable")
						}
						if sneaky.URL != "" {
							t.Errorf("ambiguous-evidence release URL = %q, want empty", sneaky.URL)
						}
					}
				}
			})
		}
	}
}

// TestAuditMalformedPublicURLListedUnobtainable pins the report contract for
// a public-labeled release with MALFORMED URL evidence: the fail-closed
// verdict gate (classify.ABVisible) cannot prove it is AnimeBytes, so with
// the toggle off the row must remain LISTED with an empty URL and
// Unobtainable=true - the operator sees why it did not affect the verdict -
// while a definite AB release in the same entry stays hidden. Regression
// test: classifyReleases previously used ABVisible as the row-visibility
// gate, silently erasing such rows.
func TestAuditMalformedPublicURLListedUnobtainable(t *testing.T) {
	entry := seadex.Entry{AniListID: 12, Torrents: []seadex.Torrent{
		{Tracker: "Nyaa", URL: "https://nyaa.si/\x7f", ReleaseGroup: "Mangled", IsBest: true},
		{Tracker: "AB", URL: "/torrents.php?id=9&torrentid=10", ReleaseGroup: "Private", IsBest: true},
	}}
	snap := &library.Snapshot{Items: []library.Item{{
		Arr: library.ArrSonarr, ArrID: 12, Title: "Mangled Link", TvdbID: 1200,
		SeasonGroups: map[int][]string{1: {"other"}}, Groups: []string{"other"}, HasFile: true,
	}}}
	matches := []match.Match{{
		Item:   &snap.Items[0],
		Arr:    library.ArrSonarr,
		Source: match.SourceID,
		Entry:  entry,
		Record: mapping.Record{Type: "TV", TvdbID: 1200, SeasonTvdb: 1},
	}}

	a := NewAuditor(Config{SeaDexBaseURL: "https://releases.moe"})
	rep := a.Audit(matches, snap, mapping.NewIndex(nil), nil)
	var row *Row
	for i := range rep.Rows {
		if rep.Rows[i].AniListID == 12 {
			row = &rep.Rows[i]
		}
	}
	if row == nil {
		t.Fatal("expected a row for the matched entry")
	}
	var mangled *Release
	for i := range row.Releases {
		switch row.Releases[i].Group {
		case "Mangled":
			mangled = &row.Releases[i]
		case "Private":
			t.Errorf("definite AB release listed with the toggle off: %+v", row.Releases[i])
		}
	}
	if mangled == nil {
		t.Fatalf("malformed-URL public release missing; want listed and unobtainable (releases: %+v)", row.Releases)
	}
	if !mangled.Unobtainable {
		t.Error("malformed-URL public release not marked unobtainable")
	}
	if mangled.URL != "" {
		t.Errorf("malformed-URL public release URL = %q, want empty", mangled.URL)
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

// TestAuditIncompleteMappings pins the incomplete-mapping section's data
// shape: the transiently-unresolved AniList ids render as IncompleteEntry
// rows sorted by id, each carrying its releases.moe link, and a fully
// resolved run (nil or empty set) carries none - so the section (and the
// JSON key, via omitempty) only ever appears when something actually failed.
func TestAuditIncompleteMappings(t *testing.T) {
	a := NewAuditor(Config{SeaDexBaseURL: "https://releases.moe"})

	rep := a.Audit(nil, nil, nil, map[int]struct{}{99: {}, 7: {}})

	want := []IncompleteEntry{
		{SeaDexURL: "https://releases.moe/7", AniListID: 7},
		{SeaDexURL: "https://releases.moe/99", AniListID: 99},
	}
	if !reflect.DeepEqual(rep.Incomplete, want) {
		t.Errorf("Incomplete = %+v, want %+v (sorted by AniList id with releases.moe links)", rep.Incomplete, want)
	}

	if got := a.Audit(nil, nil, nil, nil).Incomplete; got != nil {
		t.Errorf("Incomplete on a fully resolved run = %+v, want nil", got)
	}
	if got := a.Audit(nil, nil, nil, map[int]struct{}{}).Incomplete; got != nil {
		t.Errorf("Incomplete on an empty set = %+v, want nil", got)
	}
}

// TestRowQualifier pins the daemon-vocabulary qualifier over the shared
// decision: theoretical/incomplete when SeaDex lists no best at all
// (theoretical taking precedence, the classify.Fallback order shared with the
// daemon's emptyResult, annotated even on a no-file row the daemon silences),
// mixed only on a not-aligned multi-group row, incomplete on a diverged row
// of an incomplete entry, and empty everywhere else (an aligned row is never
// mixed - alignment wins). Decisions are built through align.Decide from real
// season/record inputs, so the qualifier is pinned against decisions the
// production path can actually produce.
func TestRowQualifier(t *testing.T) {
	tests := []struct {
		name    string
		entry   seadex.Entry
		seasons map[int][]string
		best    []string
		alt     []string
		want    Qualifier
	}{
		{"theoretical-only entry", seadex.Entry{TheoreticalBest: "remux"}, map[int][]string{1: {"a"}}, nil, nil, QualifierTheoretical},
		{"theoretical wins over incomplete", seadex.Entry{TheoreticalBest: "remux", Incomplete: true}, map[int][]string{1: {"a"}}, nil, nil, QualifierTheoretical},
		{"incomplete with nothing recommended", seadex.Entry{Incomplete: true}, map[int][]string{1: {"a"}}, nil, nil, QualifierIncomplete},
		{"no best and neither flag is unqualified", seadex.Entry{}, map[int][]string{1: {"a"}}, nil, nil, ""},
		{"no best on a no-file row still annotates the entry state", seadex.Entry{TheoreticalBest: "remux"}, map[int][]string{2: {"a"}}, nil, nil, QualifierTheoretical},
		{"not-aligned multi-group is mixed", seadex.Entry{}, map[int][]string{1: {"a", "b"}}, []string{"sam"}, nil, QualifierMixed},
		{"not-aligned alt multi-group is mixed", seadex.Entry{}, map[int][]string{1: {"a", "b"}}, []string{"sam"}, []string{"a"}, QualifierMixed},
		{"aligned multi-group is not mixed", seadex.Entry{}, map[int][]string{1: {"a", "b"}}, []string{"a"}, nil, ""},
		{"not-aligned single group is not mixed", seadex.Entry{}, map[int][]string{1: {"a"}}, []string{"sam"}, nil, ""},
		{"diverged single group of an incomplete entry is incomplete", seadex.Entry{Incomplete: true}, map[int][]string{1: {"a"}}, []string{"sam"}, nil, QualifierIncomplete},
		{"no_file with best listed is unqualified", seadex.Entry{}, map[int][]string{2: {"a"}}, []string{"sam"}, nil, ""},
		{"unverifiable row is unqualified", seadex.Entry{}, map[int][]string{1: {"nogrp"}}, []string{"sam"}, nil, ""},
		{"unverifiable row of an incomplete entry is still unqualified", seadex.Entry{Incomplete: true}, map[int][]string{1: {"nogrp"}}, []string{"sam"}, nil, ""},
	}
	rec := mapping.Record{Type: "TV", SeasonTvdb: 1}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := &library.Item{Arr: library.ArrSonarr, SeasonGroups: tt.seasons, HasFile: true}
			d := align.Decide(item, &rec, tt.best, tt.alt)
			if got := rowQualifier(&tt.entry, &d); got != tt.want {
				t.Errorf("rowQualifier() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestAuditCurationWarnedReleaseAnnotatedNotCounted pins the report-path
// curation-warning contract: a warned release stays LISTED (the report
// enumerates raw SeaDex data) carrying its canonical warning tags, but it
// counts as neither best nor alt for the verdict - an on-disk group matching
// only a Broken best reads have_unlisted, never have_best, mirroring the
// daemon's exclusion - while an unwarned best still classifies as usual.
func TestAuditCurationWarnedReleaseAnnotatedNotCounted(t *testing.T) {
	a := NewAuditor(Config{SeaDexBaseURL: "https://releases.moe"})
	rowFor := func(t *testing.T, torrents []seadex.Torrent) Row {
		t.Helper()
		item := &library.Item{
			Arr: library.ArrSonarr, ArrID: 1, Title: "Warned", TvdbID: 100,
			SeasonGroups: map[int][]string{1: {"pmr"}}, Groups: []string{"pmr"}, HasFile: true,
		}
		matches := []match.Match{{
			Item:   item,
			Arr:    library.ArrSonarr,
			Source: match.SourceID,
			Entry:  seadex.Entry{AniListID: 10, Torrents: torrents},
			Record: mapping.Record{Type: "TV", TvdbID: 100, SeasonTvdb: 1},
		}}
		rep := a.Audit(matches, nil, nil, nil)
		if len(rep.Rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rep.Rows))
		}
		return rep.Rows[0]
	}

	t.Run("warned best neither aligns nor recommends", func(t *testing.T) {
		row := rowFor(t, []seadex.Torrent{{
			Tracker: "Nyaa", ReleaseGroup: "PMR", URL: "https://nyaa.si/view/1",
			IsBest: true, Tags: []string{"Broken"},
		}})
		if row.Verdict != VerdictUnlisted {
			t.Errorf("verdict = %q, want %q (a Broken best must not count as best)", row.Verdict, VerdictUnlisted)
		}
		if len(row.Releases) != 1 {
			t.Fatalf("releases = %d, want 1 (a warned release stays listed)", len(row.Releases))
		}
		if got := row.Releases[0].Warnings; !reflect.DeepEqual(got, []string{"broken"}) {
			t.Errorf("release warnings = %v, want the canonical [broken]", got)
		}
	})

	t.Run("warned alt does not classify as alt", func(t *testing.T) {
		row := rowFor(t, []seadex.Torrent{
			{Tracker: "Nyaa", ReleaseGroup: "PMR", URL: "https://nyaa.si/view/2", Tags: []string{"Incomplete"}},
			{Tracker: "Nyaa", ReleaseGroup: "SEV", URL: "https://nyaa.si/view/3", IsBest: true},
		})
		if row.Verdict != VerdictUnlisted {
			t.Errorf("verdict = %q, want %q (a warned alt must not count as alt)", row.Verdict, VerdictUnlisted)
		}
	})

	t.Run("unwarned best still classifies", func(t *testing.T) {
		row := rowFor(t, []seadex.Torrent{{
			Tracker: "Nyaa", ReleaseGroup: "PMR", URL: "https://nyaa.si/view/4", IsBest: true,
		}})
		if row.Verdict != VerdictBest {
			t.Errorf("verdict = %q, want %q (an unwarned best is unaffected)", row.Verdict, VerdictBest)
		}
		if len(row.Releases) != 1 || row.Releases[0].Warnings != nil {
			t.Errorf("releases = %+v, want one unwarned release with nil warnings", row.Releases)
		}
	})
}

// TestAuditUnobtainableBestAnnotatedNotCounted pins the report-path
// obtainability contract (option (b) of review finding h-f15): a SeaDex best
// the daemon's filter.Obtainable rule rejects (here: no usable URL) stays
// LISTED, carrying an explicit Unobtainable marker, but counts as neither
// best nor alt for the verdict - an on-disk group matching only an
// unobtainable best reads have_unlisted, never have_best, mirroring the
// daemon's exclusion - so the rendered facts and the decision inputs no
// longer silently diverge. An obtainable best on the same entry still
// classifies as usual and carries no marker.
func TestAuditUnobtainableBestAnnotatedNotCounted(t *testing.T) {
	a := NewAuditor(Config{SeaDexBaseURL: "https://releases.moe"})
	rowFor := func(t *testing.T, torrents []seadex.Torrent) Row {
		t.Helper()
		item := &library.Item{
			Arr: library.ArrSonarr, ArrID: 1, Title: "Unobtainable", TvdbID: 100,
			SeasonGroups: map[int][]string{1: {"pmr"}}, Groups: []string{"pmr"}, HasFile: true,
		}
		matches := []match.Match{{
			Item:   item,
			Arr:    library.ArrSonarr,
			Source: match.SourceID,
			Entry:  seadex.Entry{AniListID: 10, Torrents: torrents},
			Record: mapping.Record{Type: "TV", TvdbID: 100, SeasonTvdb: 1},
		}}
		rep := a.Audit(matches, nil, nil, nil)
		if len(rep.Rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rep.Rows))
		}
		return rep.Rows[0]
	}

	t.Run("unobtainable best neither aligns nor recommends", func(t *testing.T) {
		row := rowFor(t, []seadex.Torrent{{
			Tracker: "Nyaa", ReleaseGroup: "PMR", IsBest: true,
		}})
		if row.Verdict != VerdictUnlisted {
			t.Errorf("verdict = %q, want %q (an unobtainable best must not count as best)", row.Verdict, VerdictUnlisted)
		}
		if len(row.Releases) != 1 {
			t.Fatalf("releases = %d, want 1 (an unobtainable release stays listed)", len(row.Releases))
		}
		if !row.Releases[0].Unobtainable {
			t.Error("release Unobtainable = false, want true (the marker explains the ignored best)")
		}
	})

	t.Run("obtainable best still classifies unmarked", func(t *testing.T) {
		row := rowFor(t, []seadex.Torrent{{
			Tracker: "Nyaa", ReleaseGroup: "PMR", URL: "https://nyaa.si/view/5", IsBest: true,
		}})
		if row.Verdict != VerdictBest {
			t.Errorf("verdict = %q, want %q (an obtainable best is unaffected)", row.Verdict, VerdictBest)
		}
		if len(row.Releases) != 1 || row.Releases[0].Unobtainable {
			t.Errorf("releases = %+v, want one obtainable release without the marker", row.Releases)
		}
	})
}

// TestAuditExcludedSpecialMatchStillCoversItem pins the covered-mark ordering
// in Audit's row loop: an item whose only SeaDex match is a special dropped
// by exclude_specials is still marked covered BEFORE the specials filter
// fires, so it never resurfaces as not_on_seadex - the item IS on SeaDex
// (via the special entry), so a not_on_seadex row would be wrong even though
// its verdict row is filtered out. The sibling TV record keeps the item
// catalogued, so this test fails if the covered mark ever moves below the
// specials filter.
func TestAuditExcludedSpecialMatchStillCoversItem(t *testing.T) {
	a := NewAuditor(Config{SeaDexBaseURL: "https://releases.moe", ExcludeSpecials: true})
	snap := &library.Snapshot{Items: []library.Item{{
		Arr: library.ArrSonarr, ArrID: 1, Title: "SpecialOnly", TvdbID: 700,
		Groups: []string{"g"}, HasFile: true,
	}}}
	idx := mapping.NewIndex([]mapping.Record{
		{AniListID: 5, Type: "OVA", TvdbID: 700},
		{AniListID: 6, Type: "TV", TvdbID: 700},
	})
	matches := []match.Match{{
		Item:   &snap.Items[0],
		Arr:    library.ArrSonarr,
		Source: match.SourceID,
		Entry:  seadex.Entry{AniListID: 5},
		Record: mapping.Record{Type: "OVA", TvdbID: 700},
	}}

	rep := a.Audit(matches, snap, idx, nil)

	if len(rep.Rows) != 0 {
		t.Errorf("rows = %+v, want none (the excluded-special match still covers its item, which must not resurface as not_on_seadex)", rep.Rows)
	}
	if n := rep.Totals[string(VerdictNotOnSeaDex)]; n != 0 {
		t.Errorf("not_on_seadex total = %d, want 0", n)
	}
}

// TestAssessClampsNegativeSeason pins the Season clamp in assess: a Fribb
// record whose season.tvdb is negative (the -1 convention for an
// absolute-numbered run) yields Season 0 on the row, so a negative season can
// never reach the JSON wire shape (omitempty then drops the zero).
func TestAssessClampsNegativeSeason(t *testing.T) {
	a := NewAuditor(Config{SeaDexBaseURL: "https://releases.moe"})
	item := &library.Item{
		Arr: library.ArrSonarr, ArrID: 1, Title: "Absolute", TvdbID: 100,
		SeasonGroups: map[int][]string{1: {"g"}}, Groups: []string{"g"}, HasFile: true,
	}

	row := a.assess(&match.Match{
		Item:   item,
		Arr:    library.ArrSonarr,
		Source: match.SourceID,
		Entry:  seadex.Entry{AniListID: 1},
		Record: mapping.Record{Type: "TV", TvdbID: 100, SeasonTvdb: -1},
	})

	if row.Season != 0 {
		t.Errorf("Season = %d, want 0 (negative Fribb season.tvdb must clamp to zero)", row.Season)
	}
}

func TestAuditNotOnSeaDexRowScopeAndEmptyCells(t *testing.T) {
	a := NewAuditor(Config{SeaDexBaseURL: "https://releases.moe"})
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrRadarr, ArrID: 1, Title: "UncoveredMovie", TmdbID: 400, HasFile: true},
		{Arr: library.ArrSonarr, ArrID: 2, Title: "UncoveredSeries", TvdbID: 200, Groups: []string{"grp"}, HasFile: true},
	}}
	idx := mapping.NewIndex([]mapping.Record{
		{AniListID: 1, Type: "MOVIE", TmdbMovies: []int{400}},
		{AniListID: 2, Type: "TV", TvdbID: 200},
	})

	rep := a.Audit(nil, snap, idx, nil)
	md := renderMarkdown(&rep)

	if !strings.Contains(md, "| UncoveredMovie | movie | - | - | - |") {
		t.Errorf("markdown missing the movie-scoped not_on_seadex row with empty-cell placeholders:\n%s", md)
	}
	if !strings.Contains(md, "| UncoveredSeries | series | grp | - | - |") {
		t.Errorf("markdown missing the series-scoped not_on_seadex row:\n%s", md)
	}
}

func TestAssessCarriesEntryStateFlags(t *testing.T) {
	a := NewAuditor(Config{SeaDexBaseURL: "https://releases.moe"})
	item := &library.Item{
		Arr: library.ArrSonarr, ArrID: 1, Title: "Flagged", TvdbID: 100,
		SeasonGroups: map[int][]string{0: {"g"}}, Groups: []string{"g"}, HasFile: true,
	}

	row := a.assess(&match.Match{
		Item:   item,
		Arr:    library.ArrSonarr,
		Source: match.SourceID,
		Entry:  seadex.Entry{AniListID: 1, Incomplete: true},
		Record: mapping.Record{Type: "OVA", TvdbID: 100},
	})

	if !row.Incomplete {
		t.Error("row.Incomplete = false, want true (copied from the SeaDex entry for the JSON wire shape)")
	}
	if !row.Special {
		t.Error("row.Special = false, want true (an OVA record marks the row special)")
	}
}

func TestGroupSets(t *testing.T) {
	rels := []Release{
		{Group: "SubsPlease", Best: true, URL: "https://nyaa.si/view/1"},
		{Group: "subsplease", Best: true, URL: "https://nyaa.si/view/2"},
		{Group: "Erai", Best: false, URL: "https://nyaa.si/view/3"},
		// An Unobtainable release (the daemon's filter.Obtainable rule
		// rejected it: no usable link, or a tracker the operator cannot use)
		// is raw-catalogue visibility only: it must drive neither the best
		// nor the alt set - the eligibility IS the daemon's obtainability
		// rule.
		{Group: "LinklessBest", Best: true, Unobtainable: true},
		{Group: "LinklessAlt", Best: false, Unobtainable: true},
	}
	best, alt := groupSets(rels)
	if !reflect.DeepEqual(best, []string{"subsplease"}) {
		t.Errorf("best = %v, want [subsplease]", best)
	}
	if !reflect.DeepEqual(alt, []string{"erai"}) {
		t.Errorf("alt = %v, want [erai]", alt)
	}
}

func TestClassifyReleasesGatesAnimeBytes(t *testing.T) {
	entry := &seadex.Entry{Torrents: []seadex.Torrent{
		{Tracker: "Nyaa", ReleaseGroup: "SubsPlease", IsBest: true, URL: "https://nyaa.si/view/1"},
		{Tracker: "AB", ReleaseGroup: "Commie", IsBest: false, URL: "/torrents.php?id=1"},
	}}

	off := NewAuditor(Config{}).classifyReleases(entry)
	if len(off) != 1 || off[0].Tracker != "Nyaa" {
		t.Errorf("with AnimeBytes off only the Nyaa release should survive, got %+v", off)
	}

	on := NewAuditor(Config{AnimeBytes: true}).classifyReleases(entry)
	if len(on) != 2 {
		t.Errorf("with AnimeBytes on both releases should be present, got %d", len(on))
	}
}
