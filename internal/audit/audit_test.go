package audit

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/tagfilter"
)

// TestVerdictFor pins the rendering of the shared decision core's group-ladder
// standing in the report's verdict vocabulary: 1:1 except the unverified standing,
// which the report splits by origin into unverified and unattributed.
func TestVerdictFor(t *testing.T) {
	tests := []struct {
		name          string
		decision      align.Decision
		groupsUnknown bool
		want          Verdict
	}{
		{name: "no file", decision: align.Decision{Standing: align.StandingNoFile}, want: VerdictNoFile},
		{name: "best", decision: align.Decision{Standing: align.StandingBest}, want: VerdictBest},
		{name: "alt", decision: align.Decision{Standing: align.StandingAlt}, want: VerdictAlt},
		{name: "unlisted", decision: align.Decision{Standing: align.StandingUnlisted}, want: VerdictUnlisted},
		// The three origins of an unverified standing the report keeps as
		// unverified: a NOGRP side on a compared scope, a placeholder whose files
		// could not be read, and an offered unit whose files could not be read
		// (byte-identical to the offered case below on the decision alone).
		{name: "unverified nogrp side", decision: align.Decision{Standing: align.StandingUnverified, Kind: align.ScopeSeason}, want: VerdictUnverified},
		{name: "unverified failed placeholder", decision: align.Decision{Standing: align.StandingUnverified, Kind: align.ScopeSeason}, groupsUnknown: true, want: VerdictUnverified},
		{name: "unverified offered placeholder", decision: align.Decision{Standing: align.StandingUnverified, Kind: align.ScopeOffered}, groupsUnknown: true, want: VerdictUnverified},
		// The one origin that is a different verdict: an offered unit whose bucket
		// was read, which the app never compares.
		{name: "unattributed offered comparable", decision: align.Decision{Standing: align.StandingUnverified, Kind: align.ScopeOffered}, want: VerdictUnattributed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := verdictFor(&tt.decision, tt.groupsUnknown); got != tt.want {
				t.Errorf("verdictFor(%+v, %v) = %q, want %q", tt.decision, tt.groupsUnknown, got, tt.want)
			}
		})
	}
}

func TestAuditNotOnSeaDex(t *testing.T) {
	a := New(Config{})

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

// TestAuditRowGroupsDoNotAliasTheSnapshot pins the clone both row-building
// sites document: for a single-unit scope align.Decide returns the library
// snapshot's OWN slice, and uncoveredRows reads the item's Groups directly, so
// an aliased Row.CurrentGroups would hand the report a window into state a
// concurrent daemon cycle owns - a data race whose torn or rewritten group
// column no assertion in this suite would notice. Mutating the row's groups
// must leave the snapshot untouched on BOTH arms (a matched season-scoped row
// and a not_on_seadex row).
func TestAuditRowGroupsDoNotAliasTheSnapshot(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{
			Arr: library.ArrSonarr, ArrID: 1, Title: "Matched", TvdbID: 100,
			SeasonGroups: map[int][]string{1: {"sev"}}, Groups: []string{"sev"}, HasFile: true,
		},
		{
			Arr: library.ArrSonarr, ArrID: 2, Title: "Uncovered", TvdbID: 200,
			SeasonGroups: map[int][]string{1: {"grp"}}, Groups: []string{"grp"}, HasFile: true,
		},
	}}
	idx := mapping.NewIndex([]mapping.Record{
		{AniListID: 1, Type: "TV", TvdbID: 100},
		{AniListID: 2, Type: "TV", TvdbID: 200},
	})
	matches := []match.Match{{
		Item:   &snap.Items[0],
		Arr:    library.ArrSonarr,
		Source: match.SourceID,
		Entry:  seadex.Entry{AniListID: 1},
		Record: mapping.Record{Type: "TV", TvdbID: 100, SeasonTvdb: 1},
	}}

	rep := New(Config{}).Audit(matches, snap, idx, nil)

	if len(rep.Rows) != 2 {
		t.Fatalf("rows = %d, want 2 (the matched row plus the not_on_seadex row)", len(rep.Rows))
	}
	for i := range rep.Rows {
		row := &rep.Rows[i]
		if len(row.CurrentGroups) == 0 {
			t.Fatalf("row %q carries no groups", row.Title)
		}
		row.CurrentGroups[0] = "MUTATED"
	}
	if got := snap.Items[0].SeasonGroups[1][0]; got != "sev" {
		t.Errorf("matched row aliased the snapshot's season groups: %q, want %q", got, "sev")
	}
	if got := snap.Items[1].Groups[0]; got != "grp" {
		t.Errorf("not_on_seadex row aliased the snapshot item's groups: %q, want %q", got, "grp")
	}
}

// TestAuditNotOnSeaDexHonorsExcludeSpecials pins the exclude_specials symmetry:
// with the filter on, a specials-only library item (its only Fribb
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
		a := New(Config{ExcludeSpecials: exclude})
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

// TestAuditUnknownGroupEvidenceIsUnverified pins the tri-state evidence model end to
// end through the audit: the NoGroup sentinel is unknown evidence, never an identity
// token, so a group-less on-disk release against a group-less SeaDex best reads
// unverified - "we could not verify either side" - rather than have_best, and unknown
// evidence on EITHER side alone (a NOGRP-only library item against a known best, or a
// known library group against a NOGRP-only best torrent) yields the same unverified
// verdict instead of have_unlisted.
func TestAuditUnknownGroupEvidenceIsUnverified(t *testing.T) {
	a := New(Config{})
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
	a := New(Config{ExcludeSpecials: true})
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

// TestAuditMislabeledAnimeBytesURLHiddenWhenOff proves the URL-aware AB guard: a
// torrent whose untrusted tracker label says "Nyaa" but whose URL carries DEFINITIVE
// animebytes.tv host evidence - absolute or schemeless - must be dropped from the
// report's releases while the AnimeBytes toggle is off, exactly like a correctly
// labeled AB torrent (the guard reads the RAW upstream URL). The host:port form hides
// its host evidence (net/url parses the host as an opaque scheme), so it is NOT
// definitive: its row stays LISTED - link dropped, annotated unobtainable - rather
// than erased, while the AB link itself still never surfaces.
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
				a := New(Config{AnimeBytes: tt.animeBytes})
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
// verdict gate (filter.ABVisible) cannot prove it is AnimeBytes, so with
// the toggle off the row must remain LISTED with an empty URL and
// Unobtainable=true - the operator sees why it did not affect the verdict -
// while a definite AB release in the same entry stays hidden. Reading ABVisible
// as the row-visibility gate silently erases such rows.
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

	a := New(Config{})
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

// TestSortRowsOrdersByVerdictTitleSeasonAniListID pins the report's row
// ordering: rows group by verdict actionability (verdictOrder: unlisted, alt,
// unverified, no_file, best, not_on_seadex); within a verdict they sort by
// title case-insensitively; same-title rows sort by season ascending; and
// same-season rows tie-break on AniList id ascending. Pinned directly because
// no other test exercises sortRows' comparator: inverting either the rank or
// the title comparison must fail here.
func TestSortRowsOrdersByVerdictTitleSeasonAniListID(t *testing.T) {
	rows := []Row{
		{Title: "zeta", Verdict: VerdictBest},
		{Title: "Beta", Verdict: VerdictUnlisted},
		{Title: "gamma", Verdict: VerdictNotOnSeaDex},
		{Title: "alpha", Verdict: VerdictBest},
		{Title: "delta", Verdict: VerdictNoFile},
		{Title: "epsilon", Verdict: VerdictUnverified},
		{Title: "omega", Verdict: VerdictAlt},
		{Title: "ALPHA2", Verdict: VerdictUnlisted},
		// Same verdict + title: season ascending, then AniList id ascending
		// within an equal season (the tie-breaks the title-only ordering
		// left uncovered).
		{Title: "shared", Verdict: VerdictAlt, Season: 2, AniListID: 30},
		{Title: "shared", Verdict: VerdictAlt, Season: 1, AniListID: 20},
		{Title: "shared", Verdict: VerdictAlt, Season: 1, AniListID: 10},
	}

	sortRows(rows)

	want := []struct {
		title   string
		verdict Verdict
		season  int
		alID    int
	}{
		{"ALPHA2", VerdictUnlisted, 0, 0}, // case-insensitive: "alpha2" < "beta"
		{"Beta", VerdictUnlisted, 0, 0},
		{"omega", VerdictAlt, 0, 0},
		{"shared", VerdictAlt, 1, 10}, // same title: season first, then AniList id
		{"shared", VerdictAlt, 1, 20},
		{"shared", VerdictAlt, 2, 30},
		{"epsilon", VerdictUnverified, 0, 0},
		{"delta", VerdictNoFile, 0, 0},
		{"alpha", VerdictBest, 0, 0},
		{"zeta", VerdictBest, 0, 0},
		{"gamma", VerdictNotOnSeaDex, 0, 0},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %d, want %d", len(rows), len(want))
	}
	for i, w := range want {
		if rows[i].Title != w.title || rows[i].Verdict != w.verdict || rows[i].Season != w.season || rows[i].AniListID != w.alID {
			t.Errorf("rows[%d] = %q/%q/S%d/al%d, want %q/%q/S%d/al%d",
				i, rows[i].Title, rows[i].Verdict, rows[i].Season, rows[i].AniListID, w.title, w.verdict, w.season, w.alID)
		}
	}
}

// TestAuditIncompleteMappings pins the incomplete-mapping section's data
// shape: the transiently-unresolved AniList ids render as IncompleteEntry
// rows sorted by id, each carrying its releases.moe link, and a fully
// resolved run (nil or empty set) carries none - so the section (and the
// JSON key, via omitempty) only ever appears when something actually failed.
func TestAuditIncompleteMappings(t *testing.T) {
	a := New(Config{})

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

// TestRowQualifier pins the daemon-vocabulary qualifier over the shared decision:
// theoretical/incomplete when SeaDex lists no best at all (theoretical taking
// precedence, the classify.Fallback order shared with the daemon's emptyResult,
// annotated even on a no-file row the daemon silences), mixed only on a not-aligned
// multi-group row, incomplete on a diverged row of an incomplete entry, and empty
// everywhere else (an aligned row is never mixed - alignment wins). Decisions are
// built through align.Decide from real season/record inputs, so the qualifier is
// pinned against decisions the production path can actually produce.
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
			d := align.Decide(item, &rec, tt.best, tt.alt, nil, nil)
			if got := rowQualifier(&tt.entry, &d); got != tt.want {
				t.Errorf("rowQualifier() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestAuditBrokenBestCountedAndAnnotatedByDefault pins the report-path DEFAULT under
// filters.exclude_tags: with no exclusions configured a curation-warned release is
// LISTED, ANNOTATED with its canonical warning tags, AND counted as BEST evidence, so
// an on-disk group matching a Broken best reads have_best. The excluded case lives in
// TestAuditExcludedTagBestNotCounted, which configures `broken: [report]` explicitly.
func TestAuditBrokenBestCountedAndAnnotatedByDefault(t *testing.T) {
	rowFor := auditRowFixture(New(Config{}))

	t.Run("warned best counts and is annotated", func(t *testing.T) {
		row := rowFor(t, []seadex.Torrent{{
			Tracker: "Nyaa", ReleaseGroup: "PMR", URL: "https://nyaa.si/view/1",
			IsBest: true, Tags: []string{"Broken"},
		}})
		if row.Verdict != VerdictBest {
			t.Errorf("verdict = %q, want %q (nothing is filtered by default, so a Broken best counts)", row.Verdict, VerdictBest)
		}
		if len(row.Releases) != 1 {
			t.Fatalf("releases = %d, want 1 (a warned release stays listed)", len(row.Releases))
		}
		rel := row.Releases[0]
		if !reflect.DeepEqual(rel.Warnings, []string{"broken"}) {
			t.Errorf("release warnings = %v, want the canonical [broken] (display is not config-driven)", rel.Warnings)
		}
		if rel.Filtered {
			t.Error("release Filtered = true with an empty exclude_tags policy")
		}
	})

	t.Run("a feed-only exclusion leaves the report alone", func(t *testing.T) {
		feedOnly := auditRowFixture(New(Config{TagFilter: tagfilter.New(map[string][]tagfilter.Surface{
			"broken": {tagfilter.SurfaceFeed},
		})}))
		row := feedOnly(t, []seadex.Torrent{{
			Tracker: "Nyaa", ReleaseGroup: "PMR", URL: "https://nyaa.si/view/1",
			IsBest: true, Tags: []string{"Broken"},
		}})
		if row.Verdict != VerdictBest {
			t.Errorf("verdict = %q, want %q (broken:[feed] must not affect the report)", row.Verdict, VerdictBest)
		}
		if row.Releases[0].Filtered {
			t.Error("release Filtered = true under a feed-only exclusion")
		}
	})

	t.Run("warned alt still classifies as alt", func(t *testing.T) {
		row := rowFor(t, []seadex.Torrent{
			{Tracker: "Nyaa", ReleaseGroup: "PMR", URL: "https://nyaa.si/view/2", Tags: []string{"Incomplete"}},
			{Tracker: "Nyaa", ReleaseGroup: "SEV", URL: "https://nyaa.si/view/3", IsBest: true},
		})
		// The alt rung is descriptive ("is what I have something SeaDex
		// lists?"), and a curation warning does not change that answer -
		// have_unlisted would claim SeaDex lists the on-disk group neither
		// as best nor as alt, which is false here.
		if row.Verdict != VerdictAlt {
			t.Errorf("verdict = %q, want %q (SeaDex lists the on-disk group as an alt, warned or not)", row.Verdict, VerdictAlt)
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

// TestAuditExcludedTagBestNotCounted pins the report surface under a CONFIGURED
// exclusion (`broken: [report]`): the excluded best stays LISTED and ANNOTATED
// (display never depends on the policy) but forfeits BEST evidence, so an
// on-disk group matching only it reads have_unlisted - the pre-config behaviour,
// now the operator's choice. Matching stays exact and case-insensitive, so a
// substring near-miss keeps counting.
func TestAuditExcludedTagBestNotCounted(t *testing.T) {
	rowFor := auditRowFixture(New(Config{TagFilter: tagfilter.New(map[string][]tagfilter.Surface{
		"broken": {tagfilter.SurfaceReport},
	})}))

	t.Run("excluded best is listed, annotated and not counted", func(t *testing.T) {
		row := rowFor(t, []seadex.Torrent{{
			Tracker: "Nyaa", ReleaseGroup: "PMR", URL: "https://nyaa.si/view/1",
			IsBest: true, Tags: []string{"BROKEN"},
		}})
		if row.Verdict != VerdictUnlisted {
			t.Errorf("verdict = %q, want %q (an excluded best must not count as best)", row.Verdict, VerdictUnlisted)
		}
		if len(row.Releases) != 1 {
			t.Fatalf("releases = %d, want 1 (an excluded release stays listed)", len(row.Releases))
		}
		rel := row.Releases[0]
		if !rel.Filtered {
			t.Error("release Filtered = false, want true under broken:[report]")
		}
		if !reflect.DeepEqual(rel.Warnings, []string{"broken"}) {
			t.Errorf("release warnings = %v, want the canonical [broken] even when filtered", rel.Warnings)
		}
	})

	t.Run("a substring near-miss still counts", func(t *testing.T) {
		row := rowFor(t, []seadex.Torrent{{
			Tracker: "Nyaa", ReleaseGroup: "PMR", URL: "https://nyaa.si/view/5",
			IsBest: true, Tags: []string{"brokenish"},
		}})
		if row.Verdict != VerdictBest {
			t.Errorf("verdict = %q, want %q (a tag merely containing an excluded tag is not excluded)", row.Verdict, VerdictBest)
		}
	})
}

// auditRowFixture returns a helper producing the single report Row for one
// entry's torrents against a fixed on-disk item (Sonarr series, TVDB 100,
// season 1, group pmr). Shared by the default-behaviour and configured-exclusion
// tests so both read the same fixture.
func auditRowFixture(a *Auditor) func(*testing.T, []seadex.Torrent) Row {
	return func(t *testing.T, torrents []seadex.Torrent) Row {
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
}

// TestAuditUnobtainableBestAnnotatedNotCounted pins the report-path obtainability
// contract: a SeaDex best the daemon's filter.Obtainable rule rejects (here: no usable
// URL) stays LISTED carrying an explicit Unobtainable marker, but counts as no best for
// the verdict - an on-disk group matching only an unobtainable best reads have_unlisted,
// never have_best, mirroring the daemon's exclusion. An obtainable best on the same
// entry still classifies as usual and carries no marker.
func TestAuditUnobtainableBestAnnotatedNotCounted(t *testing.T) {
	a := New(Config{})
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

// TestAuditExcludedSpecialMatchStillCoversItem pins the covered-mark ordering in
// Audit's row loop: an item whose only SeaDex match is a special dropped by
// exclude_specials is marked covered BEFORE the filter fires, so it never
// resurfaces as not_on_seadex when the item IS on SeaDex via that entry. The
// sibling TV record keeps it catalogued, so this fails if the mark moves below the
// filter. The special's season is ABSENT, so it is comparable, which is the only
// shape that still isolates the ordering now that an offered special claims none.
func TestAuditExcludedSpecialMatchStillCoversItem(t *testing.T) {
	a := New(Config{ExcludeSpecials: true})
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
		Record: mapping.Record{Type: "OVA", TvdbID: 700, SeasonKind: mapping.SeasonAbsent},
	}}

	rep := a.Audit(matches, snap, idx, nil)

	if len(rep.Rows) != 0 {
		t.Errorf("rows = %+v, want none (the excluded-special match still covers its item, which must not resurface as not_on_seadex)", rep.Rows)
	}
	if n := rep.Totals[string(VerdictNotOnSeaDex)]; n != 0 {
		t.Errorf("not_on_seadex total = %d, want 0", n)
	}
}

// TestAuditCoverageFollowsComparability pins that coverage is a property of the
// COMPARISON, not of the link. The alternative was measured - resolving a film to
// its parent series marks the series covered, which silently deletes the two
// truthful not_on_seadex rows the report exists to produce (Macross Plus's season
// 1 holds four files belonging to an OVA SeaDex does not list). Both directions
// are asserted here, because keying on the item alone loses the first and keying
// on the RECORD alone loses the second: 54 live rows are Radarr-owned films with
// a mapped zero, fully comparable as movies.
func TestAuditCoverageFollowsComparability(t *testing.T) {
	tests := []struct {
		name          string
		record        mapping.Record
		item          library.Item
		wantUncovered bool
	}{
		{
			name:          "an offered film on a Sonarr series claims no coverage",
			record:        mapping.Record{Type: "MOVIE", TvdbID: 700, SeasonKind: mapping.SeasonPresent},
			item:          library.Item{Arr: library.ArrSonarr, ArrID: 1, Title: "Macross", TvdbID: 700, Groups: []string{"g"}, HasFile: true, SeasonGroups: map[int][]string{1: {"g"}}},
			wantUncovered: true,
		},
		{
			name:   "a comparable season entry claims coverage",
			record: mapping.Record{Type: "TV", TvdbID: 700, SeasonKind: mapping.SeasonPresent, SeasonTvdb: 1},
			item:   library.Item{Arr: library.ArrSonarr, ArrID: 1, Title: "Macross", TvdbID: 700, Groups: []string{"g"}, HasFile: true, SeasonGroups: map[int][]string{1: {"g"}}},
		},
		{
			// The same mapped-zero record on the arr that CAN identify the file:
			// scope's Radarr early return keeps it a movie, so it still covers.
			name:   "a Radarr-owned film with a mapped zero keeps its coverage",
			record: mapping.Record{Type: "MOVIE", TmdbMovies: []int{635302}, SeasonKind: mapping.SeasonPresent},
			item:   library.Item{Arr: library.ArrRadarr, ArrID: 2, Title: "Mugen Train", TmdbID: 635302, Groups: []string{"g"}, HasFile: true},
		},
		{
			// LoGH: a SPECIAL-typed record whose season is ABSENT compares against
			// the real seasons, so it keeps coverage. Fails if the dispatch reads
			// the type label.
			name:   "an absent-season special is comparable and keeps its coverage",
			record: mapping.Record{Type: "OVA", TvdbID: 700, SeasonKind: mapping.SeasonAbsent},
			item:   library.Item{Arr: library.ArrSonarr, ArrID: 1, Title: "LoGH", TvdbID: 700, Groups: []string{"koala"}, HasFile: true, SeasonGroups: map[int][]string{1: {"koala"}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := New(Config{})
			snap := &library.Snapshot{Items: []library.Item{tt.item}}
			indexed := tt.record
			indexed.AniListID = 5
			records := []mapping.Record{indexed}
			if tt.item.Arr == library.ArrSonarr {
				// The reverse catalogue is what makes an uncovered item VISIBLE, and
				// RoutedIDs deliberately still drops a MOVIE record's tvdb id (only
				// FindByID reads the type-blind AllIDs). A sibling series record is
				// what catalogues the item live - Macross Plus's own shape - so every
				// negative assertion here is against an item the catalogue can see.
				records = append(records, mapping.Record{AniListID: 9, Type: "TV", TvdbID: tt.item.TvdbID})
			}
			idx := mapping.NewIndex(records)
			matches := []match.Match{{
				Item:   &snap.Items[0],
				Arr:    tt.item.Arr,
				Source: match.SourceID,
				Entry:  seadex.Entry{AniListID: 5},
				Record: tt.record,
			}}

			rep := a.Audit(matches, snap, idx, nil)

			uncovered := rep.Totals[string(VerdictNotOnSeaDex)]
			if tt.wantUncovered && uncovered != 1 {
				t.Errorf("not_on_seadex rows = %d, want 1 (an entry the app cannot compare covers nothing): %+v", uncovered, rep.Rows)
			}
			if !tt.wantUncovered && uncovered != 0 {
				t.Errorf("not_on_seadex rows = %d, want 0 (a comparable entry covers its item): %+v", uncovered, rep.Rows)
			}
		})
	}
}

// TestAuditOfferedRowIsHonest pins what the REPORT says where the daemon says
// nothing, and asserts the verdict MOVES. The row keeps the bucket's groups in its
// current-groups column with the approximation marker, so the operator sees what
// IS on disk and that the app is not claiming to match it.
//
// The two subtests run the same assertions on a film and on a SPECIAL record, so
// the existing special rows are covered by construction rather than by intention.
func TestAuditOfferedRowIsHonest(t *testing.T) {
	entry := seadex.Entry{AniListID: 5, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/5"},
	}}
	for _, typ := range []string{"MOVIE", "SPECIAL"} {
		t.Run(typ, func(t *testing.T) {
			a := New(Config{})
			snap := &library.Snapshot{Items: []library.Item{{
				Arr: library.ArrSonarr, ArrID: 1, Title: "Bucketed", TvdbID: 700,
				Groups: []string{"erai-raws"}, HasFile: true, SeasonGroups: map[int][]string{0: {"erai-raws"}},
			}}}
			rec := mapping.Record{Type: typ, TvdbID: 700, SeasonKind: mapping.SeasonPresent}
			matches := []match.Match{{
				Item: &snap.Items[0], Arr: library.ArrSonarr, Source: match.SourceID, Entry: entry, Record: rec,
			}}

			rep := a.Audit(matches, nil, nil, nil)

			if len(rep.Rows) != 1 {
				t.Fatalf("rows = %+v, want exactly the offered row", rep.Rows)
			}
			row := rep.Rows[0]
			if row.Verdict != VerdictUnattributed {
				t.Errorf("verdict = %q, want %q (the verdict must MOVE: the app never compares this one)", row.Verdict, VerdictUnattributed)
			}
			if row.Scope != align.ScopeOffered {
				t.Errorf("scope = %v, want %v", row.Scope, align.ScopeOffered)
			}
			if len(row.CurrentGroups) != 1 || row.CurrentGroups[0] != "erai-raws" {
				t.Errorf("CurrentGroups = %v, want the bucket's groups (what IS on disk)", row.CurrentGroups)
			}
			if !row.Approx {
				t.Error("Approx = false, want true (on an offered row the marker means the bucket was never attributed at all)")
			}
		})
	}
}

// TestAuditOfferedEmptyBucketKeepsNoFile is the other standing: an offered entry
// whose season-0 bucket holds no file renders no_file rather than unverified,
// because absence is proven without attributing anything.
func TestAuditOfferedEmptyBucketKeepsNoFile(t *testing.T) {
	a := New(Config{})
	snap := &library.Snapshot{Items: []library.Item{{
		Arr: library.ArrSonarr, ArrID: 1, Title: "Bucketed", TvdbID: 700,
		Groups: []string{"koala"}, HasFile: true, SeasonGroups: map[int][]string{1: {"koala"}},
	}}}
	rec := mapping.Record{Type: "MOVIE", TvdbID: 700, SeasonKind: mapping.SeasonPresent}
	matches := []match.Match{{
		Item: &snap.Items[0], Arr: library.ArrSonarr, Source: match.SourceID,
		Entry: seadex.Entry{AniListID: 5, Torrents: []seadex.Torrent{
			{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/5"},
		}},
		Record: rec,
	}}

	rep := a.Audit(matches, nil, nil, nil)

	if len(rep.Rows) != 1 {
		t.Fatalf("rows = %+v, want exactly the offered row", rep.Rows)
	}
	if got := rep.Rows[0].Verdict; got != VerdictNoFile {
		t.Errorf("verdict = %q, want %q (an empty bucket proves absence)", got, VerdictNoFile)
	}
}

// TestAuditWholeSeriesSiblingSeasonsAndFairyTail pins both sides of the sibling
// ruling in the report. Gintama, the half that ships: the aggregate drops the
// seasons its siblings map, reads have_best, and keeps a comparable row.
//
// Fairy Tail, the half that is declined: three seasonless entries share one
// series whose only other row is a special the offered kind already takes, so
// routing them there too would leave the item with NO comparable row and emit a
// false not_on_seadex. That subtest fails if the declined half is implemented.
func TestAuditWholeSeriesSiblingSeasonsAndFairyTail(t *testing.T) {
	best := seadex.Entry{AniListID: 918, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "CBT", Tracker: "Nyaa", URL: "https://nyaa.si/view/918"},
	}}

	t.Run("Gintama keeps its coverage and reads have_best", func(t *testing.T) {
		a := New(Config{})
		snap := &library.Snapshot{Items: []library.Item{{
			Arr: library.ArrSonarr, ArrID: 1, Title: "Gintama", TvdbID: 79895, HasFile: true,
			Groups:       []string{"cbt", "kh"},
			SeasonGroups: map[int][]string{1: {"cbt"}, 5: {"kh"}, 10: {"kh"}},
		}}}
		idx := mapping.NewIndex([]mapping.Record{
			{AniListID: 918, Type: "TV", TvdbID: 79895, SeasonKind: mapping.SeasonAbsent},
			{AniListID: 100, Type: "TV", TvdbID: 79895, SeasonKind: mapping.SeasonPresent, SeasonTvdb: 5},
			{AniListID: 101, Type: "TV", TvdbID: 79895, SeasonKind: mapping.SeasonPresent, SeasonTvdb: 10},
		})
		rec, _ := idx.Lookup(918)
		matches := []match.Match{{
			Item: &snap.Items[0], SiblingSeasons: idx.SiblingSeasons(&rec), Arr: library.ArrSonarr,
			Source: match.SourceID, Entry: best, Record: rec,
		}}

		rep := a.Audit(matches, snap, idx, nil)

		if len(rep.Rows) != 1 {
			t.Fatalf("rows = %+v, want exactly the one verdict row (no not_on_seadex row: the item keeps a comparable row)", rep.Rows)
		}
		if rep.Rows[0].Verdict != VerdictBest {
			t.Errorf("verdict = %q, want %q (seasons 5 and 10 belong to the siblings)", rep.Rows[0].Verdict, VerdictBest)
		}
	})

	t.Run("Fairy Tail's seasonless siblings keep their contaminated verdicts", func(t *testing.T) {
		a := New(Config{})
		snap := &library.Snapshot{Items: []library.Item{{
			Arr: library.ArrSonarr, ArrID: 1, Title: "Fairy Tail", TvdbID: 114701, HasFile: true,
			Groups:       []string{"kitsune"},
			SeasonGroups: map[int][]string{1: {"kitsune"}, 2: {"kitsune"}},
		}}}
		records := []mapping.Record{
			{AniListID: 6702, Type: "TV", TvdbID: 114701, SeasonKind: mapping.SeasonAbsent},
			{AniListID: 20626, Type: "TV", TvdbID: 114701, SeasonKind: mapping.SeasonAbsent},
			{AniListID: 99749, Type: "TV", TvdbID: 114701, SeasonKind: mapping.SeasonAbsent},
			{AniListID: 9982, Type: "SPECIAL", TvdbID: 114701, SeasonKind: mapping.SeasonPresent},
		}
		idx := mapping.NewIndex(records)
		var matches []match.Match
		for _, id := range []int{6702, 20626, 99749} {
			rec, _ := idx.Lookup(id)
			matches = append(matches, match.Match{
				Item: &snap.Items[0], SiblingSeasons: idx.SiblingSeasons(&rec), Arr: library.ArrSonarr,
				Source: match.SourceID, Entry: seadex.Entry{AniListID: id, Torrents: best.Torrents}, Record: rec,
			})
		}

		rep := a.Audit(matches, snap, idx, nil)

		if len(rep.Rows) != 3 {
			t.Fatalf("rows = %+v, want exactly the three verdict rows and NO not_on_seadex row", rep.Rows)
		}
		for _, row := range rep.Rows {
			if row.Scope != align.ScopeWholeSeries {
				t.Errorf("alID %d scope = %v, want %v (half two is declined: a seasonless sibling must not route to the offered kind)", row.AniListID, row.Scope, align.ScopeWholeSeries)
			}
			if row.Verdict != VerdictUnlisted {
				t.Errorf("alID %d verdict = %q, want %q (the contamination is the accepted residue)", row.AniListID, row.Verdict, VerdictUnlisted)
			}
		}
	})
}

// TestAuditFairyTailOwnSeasonsAreTruthful is what the declined half above wanted
// and the Anime-Lists mapping-list delivers: with each entry's OWN TVDB seasons
// on its Match, the three Fairy Tail entries stay whole-series and comparable
// (no not_on_seadex row) and each verdict reads its own seasons - S1-S4 best,
// S5-S7 alt, S8 unlisted - instead of three contaminated copies of one.
func TestAuditFairyTailOwnSeasonsAreTruthful(t *testing.T) {
	a := New(Config{})
	snap := &library.Snapshot{Items: []library.Item{{
		Arr: library.ArrSonarr, ArrID: 1, Title: "Fairy Tail", TvdbID: 114801, HasFile: true,
		Groups: []string{"cbt", "kh", "erai"},
		SeasonGroups: map[int][]string{
			1: {"cbt"}, 2: {"cbt"}, 3: {"cbt"}, 4: {"cbt"},
			5: {"kh"}, 6: {"kh"}, 7: {"kh"},
			8: {"erai"},
		},
	}}}
	idx := mapping.NewIndexWithMappings([]mapping.Record{
		{AniListID: 6702, Type: "TV", TvdbID: 114801, AniDBID: 6662, SeasonKind: mapping.SeasonAbsent},
		{AniListID: 20626, Type: "TV", TvdbID: 114801, AniDBID: 9980, SeasonKind: mapping.SeasonAbsent},
		{AniListID: 99749, Type: "TV", TvdbID: 114801, AniDBID: 13295, SeasonKind: mapping.SeasonAbsent},
	}, map[int]mapping.Mapping{
		6662:  {Seasons: []mapping.SeasonRange{{Season: 1, First: 1, Last: 48}, {Season: 2, First: 49, Last: 96}, {Season: 3, First: 97, Last: 150}, {Season: 4, First: 151, Last: 175}}},
		9980:  {Seasons: []mapping.SeasonRange{{Season: 5, First: 1, Last: 51}, {Season: 6, First: 52, Last: 90}, {Season: 7, First: 91, Last: 102}}},
		13295: {Seasons: []mapping.SeasonRange{{Season: 8, First: 1, Last: 51}}},
	})
	torrents := []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "CBT", Tracker: "Nyaa", URL: "https://nyaa.si/view/1"},
		{ReleaseGroup: "KH", Tracker: "Nyaa", URL: "https://nyaa.si/view/2"},
	}
	var matches []match.Match
	for _, id := range []int{6702, 20626, 99749} {
		rec, _ := idx.Lookup(id)
		m, _ := idx.MappingFor(&rec)
		matches = append(matches, match.Match{
			Item: &snap.Items[0], SiblingSeasons: idx.SiblingSeasons(&rec), Seasons: m.Seasons, Arr: library.ArrSonarr,
			Source: match.SourceID, Entry: seadex.Entry{AniListID: id, Torrents: torrents}, Record: rec,
		})
	}

	rep := a.Audit(matches, snap, idx, nil)

	if len(rep.Rows) != 3 {
		t.Fatalf("rows = %+v, want exactly the three verdict rows and NO not_on_seadex row (coverage untouched)", rep.Rows)
	}
	want := map[int]struct {
		verdict Verdict
		groups  []string
	}{
		6702:  {VerdictBest, []string{"cbt"}},
		20626: {VerdictAlt, []string{"kh"}},
		99749: {VerdictUnlisted, []string{"erai"}},
	}
	for _, row := range rep.Rows {
		w := want[row.AniListID]
		if row.Scope != align.ScopeWholeSeries {
			t.Errorf("alID %d scope = %v, want %v (the entries stay comparable)", row.AniListID, row.Scope, align.ScopeWholeSeries)
		}
		if row.Verdict != w.verdict {
			t.Errorf("alID %d verdict = %q, want %q (its own seasons, not the whole series)", row.AniListID, row.Verdict, w.verdict)
		}
		if !slices.Equal(row.CurrentGroups, w.groups) {
			t.Errorf("alID %d CurrentGroups = %v, want %v", row.AniListID, row.CurrentGroups, w.groups)
		}
	}
}

// TestAuditPartialWalkKeepsCoverage pins the half of the coverage rule a
// file-presence predicate would have broken: a Failed placeholder's file state
// could not be READ, which is not the same as being unattributable, so it still
// claims coverage and a partial walk adds no rows.
func TestAuditPartialWalkKeepsCoverage(t *testing.T) {
	a := New(Config{})
	snap := &library.Snapshot{Items: []library.Item{{
		Arr: library.ArrSonarr, ArrID: 1, Title: "Frieren", TvdbID: 700, Failed: true,
	}}}
	idx := mapping.NewIndex([]mapping.Record{{AniListID: 5, Type: "TV", TvdbID: 700, SeasonKind: mapping.SeasonPresent, SeasonTvdb: 1}})
	matches := []match.Match{{
		Item:   &snap.Items[0],
		Arr:    library.ArrSonarr,
		Source: match.SourceID,
		Entry:  seadex.Entry{AniListID: 5},
		Record: mapping.Record{Type: "TV", TvdbID: 700, SeasonKind: mapping.SeasonPresent, SeasonTvdb: 1},
	}}

	rep := a.Audit(matches, snap, idx, nil)

	if n := rep.Totals[string(VerdictNotOnSeaDex)]; n != 0 {
		t.Errorf("not_on_seadex rows = %d, want 0 (a placeholder whose walk failed still claims coverage): %+v", n, rep.Rows)
	}
}

// TestAssessClampsNegativeSeason pins the Season clamp in assess: a Fribb
// record whose season.tvdb is negative (the -1 convention for an
// absolute-numbered run) yields Season 0 on the row, so a negative season can
// never reach the JSON wire shape (omitempty then drops the zero).
func TestAssessClampsNegativeSeason(t *testing.T) {
	a := New(Config{})
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
	a := New(Config{})
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
	a := New(Config{})
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
		// forfeits the PRESCRIPTIVE best rung - the eligibility there IS the
		// daemon's obtainability rule - but still counts on the DESCRIPTIVE
		// alt rung, which only asks whether SeaDex lists what is on disk.
		{Group: "LinklessBest", Best: true, Unobtainable: true},
		{Group: "LinklessAlt", Best: false, Unobtainable: true},
	}
	best, alt := groupSets(rels)
	if !reflect.DeepEqual(best, []string{"subsplease"}) {
		t.Errorf("best = %v, want [subsplease]", best)
	}
	if !reflect.DeepEqual(alt, []string{"erai", "linklessalt"}) {
		t.Errorf("alt = %v, want [erai linklessalt]", alt)
	}
}

func TestClassifyReleasesGatesAnimeBytes(t *testing.T) {
	entry := &seadex.Entry{Torrents: []seadex.Torrent{
		{Tracker: "Nyaa", ReleaseGroup: "SubsPlease", IsBest: true, URL: "https://nyaa.si/view/1"},
		{Tracker: "AB", ReleaseGroup: "Commie", IsBest: false, URL: "/torrents.php?id=1"},
	}}

	off := New(Config{}).classifyReleases(entry)
	if len(off) != 1 || off[0].Tracker != "Nyaa" {
		t.Errorf("with AnimeBytes off only the Nyaa release should survive, got %+v", off)
	}

	on := New(Config{AnimeBytes: true}).classifyReleases(entry)
	if len(on) != 2 {
		t.Errorf("with AnimeBytes on both releases should be present, got %d", len(on))
	}
}

// TestBestCellMarksOnlyHiddenBests pins the best column's hidden-AnimeBytes
// marker against the fact it claims: the marker asserts a best exists on a
// tracker the operator disabled, so it may only count withheld releases SeaDex
// marks BEST. A withheld ALT says nothing about whether a best exists - for an
// entry SeaDex lists no best for, annotating the empty best cell from the
// all-releases count would tell the reader the opposite of the truth. Built
// through assess so the counting and the projection are pinned together.
func TestBestCellMarksOnlyHiddenBests(t *testing.T) {
	item := &library.Item{
		Arr: library.ArrSonarr, ArrID: 1, Title: "Hidden", TvdbID: 100,
		SeasonGroups: map[int][]string{1: {"mine"}}, Groups: []string{"mine"}, HasFile: true,
	}
	record := mapping.Record{Type: "TV", TvdbID: 100, SeasonTvdb: 1}

	tests := []struct {
		name       string
		torrents   []seadex.Torrent
		wantCell   string
		wantHidden int
	}{
		{
			name: "hidden alt with no best carries no marker",
			torrents: []seadex.Torrent{
				{Tracker: "AB", ReleaseGroup: "Commie", URL: "/torrents.php?id=1&torrentid=2"},
			},
			wantCell:   "-",
			wantHidden: 1,
		},
		{
			name: "hidden bests are counted",
			torrents: []seadex.Torrent{
				{Tracker: "AB", ReleaseGroup: "Commie", IsBest: true, URL: "/torrents.php?id=1&torrentid=2"},
				{Tracker: "AB", ReleaseGroup: "PMR", IsBest: true, URL: "/torrents.php?id=1&torrentid=3"},
				{Tracker: "AB", ReleaseGroup: "LostYears", URL: "/torrents.php?id=1&torrentid=4"},
			},
			wantCell:   "- (2 best hidden: animebytes)",
			wantHidden: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// AnimeBytes off: every AB release above is withheld.
			row := New(Config{}).assess(&match.Match{
				Item:   item,
				Arr:    library.ArrSonarr,
				Source: match.SourceID,
				Entry:  seadex.Entry{AniListID: 1, Torrents: tt.torrents},
				Record: record,
			})

			if got := bestCell(&row); got != tt.wantCell {
				t.Errorf("bestCell() = %q, want %q", got, tt.wantCell)
			}
			// The exported count keeps its all-releases meaning: the
			// hidden_animebytes JSON key and slog attribute are unchanged.
			if row.HiddenAnimeBytes != tt.wantHidden {
				t.Errorf("HiddenAnimeBytes = %d, want %d (the total withheld count, best or not)",
					row.HiddenAnimeBytes, tt.wantHidden)
			}
		})
	}
}

// TestAuditGroupsUnknownMarksPlaceholders pins the group-evidence marker: a library
// item whose file data the walk could not establish carries NO group evidence, and both
// row producers must say so rather than publishing an empty group set that reads as
// "nothing identifiable is on disk". The two producers reach it by different routes and
// both matter: the matched row goes through align.Decide (which answers a placeholder
// with StandingUnverified), while uncoveredRows never calls Decide at all and its
// not_on_seadex verdict stays TRUE, so the marker is that row's only way to qualify its
// own groups column.
func TestAuditGroupsUnknownMarksPlaceholders(t *testing.T) {
	a := New(Config{})

	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 1, Title: "MatchedPlaceholder", TvdbID: 100, Failed: true},
		{Arr: library.ArrSonarr, ArrID: 2, Title: "UncoveredPlaceholder", TvdbID: 200, Failed: true},
		{Arr: library.ArrSonarr, ArrID: 3, Title: "UncoveredHealthy", TvdbID: 300, Groups: []string{"erai"}, HasFile: true},
	}}
	idx := mapping.NewIndex([]mapping.Record{
		{AniListID: 1, Type: "TV", TvdbID: 100},
		{AniListID: 2, Type: "TV", TvdbID: 200},
		{AniListID: 3, Type: "TV", TvdbID: 300},
	})
	matches := []match.Match{{
		Item:   &snap.Items[0],
		Arr:    library.ArrSonarr,
		Source: match.SourceID,
		Entry:  seadex.Entry{AniListID: 1},
		Record: mapping.Record{Type: "TV", TvdbID: 100, SeasonTvdb: 1},
	}}

	rep := a.Audit(matches, snap, idx, nil)

	byTitle := map[string]*Row{}
	for i := range rep.Rows {
		byTitle[rep.Rows[i].Title] = &rep.Rows[i]
	}
	for _, tc := range []struct {
		title string
		want  bool
	}{
		{"MatchedPlaceholder", true},
		{"UncoveredPlaceholder", true},
		{"UncoveredHealthy", false},
	} {
		row, ok := byTitle[tc.title]
		if !ok {
			t.Fatalf("row %q missing from the report", tc.title)
		}
		if row.GroupsUnknown != tc.want {
			t.Errorf("%s: GroupsUnknown = %v, want %v", tc.title, row.GroupsUnknown, tc.want)
		}
		if got := groupsCell(row); tc.want && got != unknownCell {
			t.Errorf("%s: groups cell = %q, want %q", tc.title, got, unknownCell)
		} else if !tc.want && got == unknownCell {
			t.Errorf("%s: groups cell must not read %q for an item with real evidence", tc.title, unknownCell)
		}
	}
}

// TestClassifyReleasesMapsPublisherRefusalToItsOwnMarker pins the two report
// diagnostics classifyReleases derives from the publisher's refusal REASON, and the
// implication forfeitsBest rests on. A refused url value and a tracker this build does
// not carry get their OWN marker because the remedies differ (an upstream SeaDex record
// to fix vs an internal/tracker table entry to ship), and BOTH leave the release
// Unobtainable, which is the only thing keeping a refused best out of the verdict's
// BEST set now that forfeitsBest names neither flag. The groupSets assertion is what
// makes that implication falsifiable rather than assumed.
func TestClassifyReleasesMapsPublisherRefusalToItsOwnMarker(t *testing.T) {
	tests := map[string]struct {
		tracker            string
		url                string
		wantURLError       bool
		wantUnknownTracker bool
	}{
		"a structureless url value is an upstream data defect": {
			tracker:      "Nyaa",
			url:          "Chihiro",
			wantURLError: true,
		},
		"a tracker this build does not carry is an app-table gap": {
			tracker:            "beyondhd",
			url:                "https://beyondhd.co/t/1",
			wantUnknownTracker: true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			a := New(Config{})
			entry := seadex.Entry{AniListID: 42, Torrents: []seadex.Torrent{
				{Tracker: tc.tracker, URL: tc.url, ReleaseGroup: "PMR", IsBest: true},
			}}

			rels := a.classifyReleases(&entry)

			if len(rels) != 1 {
				t.Fatalf("classifyReleases() = %+v, want the release listed", rels)
			}
			rel := &rels[0]
			if rel.URLError != tc.wantURLError {
				t.Errorf("URLError = %v, want %v", rel.URLError, tc.wantURLError)
			}
			if rel.UnknownTracker != tc.wantUnknownTracker {
				t.Errorf("UnknownTracker = %v, want %v", rel.UnknownTracker, tc.wantUnknownTracker)
			}
			if rel.URL != "" {
				t.Errorf("URL = %q, want empty: the publisher refused the value", rel.URL)
			}
			if !rel.Unobtainable {
				t.Error("Unobtainable = false: forfeitsBest names neither refusal flag, so a refused best would count as BEST evidence")
			}
			if best, _ := groupSets(rels); len(best) != 0 {
				t.Errorf("groupSets best = %v, want none for a release the publisher refused", best)
			}
		})
	}
}
