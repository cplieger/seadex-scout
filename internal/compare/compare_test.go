package compare

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/seadex-scout/internal/filter"
	"github.com/cplieger/seadex-scout/internal/keyenc"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// TestCandidateStableKeyBoundsOversizedComponents pins the size bound on the
// headline tie-break key: SeaDex admits arbitrarily long URLs (48 MiB pages,
// up to 512 torrents per entry), so each memoized candidate key must reduce an
// oversized component set to a fixed-size hashed identity. Distinct oversized
// candidates must still key differently and select the same headline regardless
// of upstream order.
func TestCandidateStableKeyBoundsOversizedComponents(t *testing.T) {
	oversized := func(tag string) candidate {
		return candidate{
			rel: release.Release{Group: "Grp" + tag, Tracker: "Nyaa", Resolution: "1080p", TrackerType: release.TrackerPublic},
			torrent: seadex.Torrent{
				Tracker:  "Nyaa",
				InfoHash: tag,
				URL:      "https://nyaa.si/view/" + tag + "?pad=" + strings.Repeat("x", keyenc.MaxComponentBytes),
			},
		}
	}
	a, b := oversized("aaa"), oversized("bbb")
	keyA, keyB := candidateStableKey(&a), candidateStableKey(&b)
	if len(keyA) > 128 {
		t.Errorf("candidateStableKey over an oversized URL = %d bytes, want bounded (hashed component)", len(keyA))
	}
	if keyA == keyB {
		t.Error("distinct oversized candidates must not share a stable key")
	}
	forward := []candidate{a, b}
	reversed := []candidate{b, a}
	fwd, rev := representative(forward), representative(reversed)
	if fwd.torrent.InfoHash != rev.torrent.InfoHash {
		t.Errorf("representative over oversized candidates depends on upstream order: forward picked %q, reversed picked %q",
			fwd.torrent.InfoHash, rev.torrent.InfoHash)
	}
}

func TestRepresentativePrefersResolutionThenPublic(t *testing.T) {
	higherRes := []candidate{
		{rel: release.Release{Resolution: "720p", TrackerType: release.TrackerPublic}},
		{rel: release.Release{Resolution: "1080p", TrackerType: release.TrackerPrivate}},
	}
	if rep := representative(higherRes); rep.rel.Resolution != "1080p" {
		t.Errorf("headline resolution = %q, want highest 1080p", rep.rel.Resolution)
	}

	tie := []candidate{
		{rel: release.Release{Resolution: "1080p", TrackerType: release.TrackerPrivate}},
		{rel: release.Release{Resolution: "1080p", TrackerType: release.TrackerPublic}},
	}
	if rep := representative(tie); rep.rel.TrackerType != release.TrackerPublic {
		t.Errorf("on a resolution tie the public tracker must win, got %q", rep.rel.TrackerType)
	}

	// Equal-ranked candidates must select the same representative whatever
	// order the upstream returned them in: the headline's identity enters the
	// dedupe key, so an order-dependent pick would emit a different key (a
	// duplicate alert plus a false resolution) for an unchanged finding.
	forward := []candidate{
		{
			rel:     release.Release{Group: "GrpA", Resolution: "1080p", TrackerType: release.TrackerPublic},
			torrent: seadex.Torrent{Tracker: "Nyaa", InfoHash: "aaa", URL: "https://nyaa.si/view/1"},
		},
		{
			rel:     release.Release{Group: "GrpB", Resolution: "1080p", TrackerType: release.TrackerPublic},
			torrent: seadex.Torrent{Tracker: "Nyaa", InfoHash: "bbb", URL: "https://nyaa.si/view/2"},
		},
	}
	reversed := []candidate{forward[1], forward[0]}
	fwd, rev := representative(forward), representative(reversed)
	if fwd.torrent.InfoHash != rev.torrent.InfoHash || fwd.torrent.URL != rev.torrent.URL {
		t.Errorf("representative depends on upstream order: forward picked %q, reversed picked %q",
			fwd.torrent.InfoHash, rev.torrent.InfoHash)
	}
	findingFor := func(pool []candidate) Finding {
		f := Finding{AniListID: 1}
		fillBest(&f, pool, groupSet(pool))
		f = *finalize(&f, StatusBetter, SevWarn)
		// The obtainable-link SET is what feeds the (order-insensitive)
		// dedupe key notify derives; normalize the pool-ordered slice so the
		// comparison checks set equality like the key does.
		slices.SortFunc(f.Links, func(a, b ReleaseLink) int { return strings.Compare(a.URL, b.URL) })
		return f
	}
	if !reflect.DeepEqual(findingFor(forward), findingFor(reversed)) {
		t.Error("findings built from opposite upstream orders must be identical (they seed the same dedupe key downstream)")
	}
}

func TestGroupSetNormalizesDedupesSorts(t *testing.T) {
	cands := []candidate{
		{rel: release.Release{Group: "SubsPlease"}},
		{rel: release.Release{Group: "subsplease"}},
		{rel: release.Release{Group: "Erai-raws"}},
	}
	got := groupSet(cands)
	want := []string{"erai-raws", "subsplease"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("groupSet() = %v, want %v", got, want)
	}
}

func TestObtainableLinksDedupesAndPrefixesPrivateURL(t *testing.T) {
	cands := []candidate{
		{rel: release.Release{Tracker: "Nyaa"}, torrent: seadex.Torrent{Tracker: "Nyaa", URL: "https://nyaa.si/view/1"}},
		{rel: release.Release{Tracker: "AB"}, torrent: seadex.Torrent{Tracker: "AB", URL: "/torrents.php?id=1"}},
		{rel: release.Release{Tracker: "Nyaa"}, torrent: seadex.Torrent{Tracker: "Nyaa", URL: "https://nyaa.si/view/1"}},
		// A delimiter-bearing pair: with a string-concatenated dedupe key these
		// two distinct (tracker, URL) tuples collide ("Nyaa|https://nyaa.si/a"
		// + "https://nyaa.si/b" == "Nyaa" + "https://nyaa.si/a|https://nyaa.si/b");
		// the struct key keeps both. Both URLs stay on the tracker's canonical
		// host so UsableURL passes them through.
		{rel: release.Release{Tracker: "Nyaa|https://nyaa.si/a"}, torrent: seadex.Torrent{Tracker: "Nyaa", URL: "https://nyaa.si/b"}},
		{rel: release.Release{Tracker: "Nyaa"}, torrent: seadex.Torrent{Tracker: "Nyaa", URL: "https://nyaa.si/a|https://nyaa.si/b"}},
	}
	links := obtainableLinks(cands)
	if len(links) != 4 {
		t.Fatalf("expected 4 distinct links, got %d: %+v", len(links), links)
	}
	if links[1].URL != "https://animebytes.tv/torrents.php?id=1" {
		t.Errorf("AB relative URL not prefixed, got %q", links[1].URL)
	}
	if links[2] == links[3] {
		t.Errorf("delimiter-bearing tuples must stay distinct, both = %+v", links[2])
	}
	if links[2].URL != "https://nyaa.si/b" || links[3].URL != "https://nyaa.si/a|https://nyaa.si/b" {
		t.Errorf("delimiter-bearing tuples mangled: %+v, %+v", links[2], links[3])
	}
}

func comparer(opts filter.Options, excludeSpecials bool) *Comparer {
	return NewComparer(Config{Filter: opts, ExcludeSpecials: excludeSpecials})
}

// abComparer is a comparer with the AnimeBytes tracker toggle enabled (the
// toggle rides Config, not filter.Options, which holds only content filters).
func abComparer() *Comparer {
	return NewComparer(Config{AnimeBytes: true})
}

func TestCompareAlignedProducesNoFinding(t *testing.T) {
	item := &library.Item{Title: "Frieren", Groups: []string{"subsplease"}, SeasonGroups: map[int][]string{1: {"subsplease"}}}
	entry := seadex.Entry{AniListID: 154587, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/1"},
	}}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}
	if got := comparer(filter.Options{}, false).Compare([]match.Match{m}); len(got) != 0 {
		t.Errorf("aligned item must produce no finding, got %+v", got)
	}
}

func TestCompareBetterRelease(t *testing.T) {
	item := &library.Item{Title: "Frieren", Groups: []string{"erai-raws"}, SeasonGroups: map[int][]string{1: {"erai-raws"}}}
	entry := seadex.Entry{AniListID: 154587, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/1"},
	}}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}
	got := comparer(filter.Options{}, false).Compare([]match.Match{m})
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
	if got[0].Status != StatusBetter || got[0].Severity != SevWarn {
		t.Errorf("status/severity = %q/%q, want better_release/warn", got[0].Status, got[0].Severity)
	}
	if got[0].RecommendedGroup != "SubsPlease" {
		t.Errorf("RecommendedGroup = %q, want SubsPlease", got[0].RecommendedGroup)
	}
}

// TestCompareUnverifiableEvidenceIsInfo pins the tri-state evidence model on
// the findings path: unknown group evidence (the release.NoGroup sentinel) on
// either side of the comparison yields ONE informational `unverifiable`
// finding - never a silent aligned suppression (the former sentinel==sentinel
// defect) and never a warn-level better_release (the live 26-NOGRP-best-
// torrents class: SeaDex side unknown, library known). The finding carries
// the recommendation fields for the manual review, and its dedupe key is
// stable across cycles so the normal cross-cycle dedupe emits it once per
// identity.
func TestCompareUnverifiableEvidenceIsInfo(t *testing.T) {
	tests := []struct {
		name      string
		diskGroup string
		bestGroup string // "" classifies to the NoGroup sentinel
	}{
		{name: "unknown library evidence against a known best", diskGroup: "nogrp", bestGroup: "SubsPlease"},
		{name: "known library group against a NOGRP-only best", diskGroup: "erai-raws", bestGroup: ""},
		{name: "sentinel on both sides is not alignment proof", diskGroup: "nogrp", bestGroup: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := &library.Item{Title: "Unknown Evidence", Groups: []string{tt.diskGroup}, SeasonGroups: map[int][]string{1: {tt.diskGroup}}}
			entry := seadex.Entry{AniListID: 900, Torrents: []seadex.Torrent{
				{IsBest: true, ReleaseGroup: tt.bestGroup, Tracker: "Nyaa", URL: "https://nyaa.si/view/900"},
			}}
			m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}

			got := comparer(filter.Options{}, false).Compare([]match.Match{m})
			if len(got) != 1 {
				t.Fatalf("expected 1 unverifiable finding, got %d: %+v", len(got), got)
			}
			f := got[0]
			if f.Status != StatusUnverifiable || f.Severity != SevInfo {
				t.Errorf("status/severity = %q/%q, want unverifiable/info", f.Status, f.Severity)
			}
			if f.RecommendedGroups == nil || f.ReleaseURL == "" {
				t.Errorf("recommendation fields must be filled for the manual review, got %+v", f)
			}
			if f.CurrentGroup != tt.diskGroup {
				t.Errorf("CurrentGroup = %q, want the scoped on-disk group %q", f.CurrentGroup, tt.diskGroup)
			}

			// A second cycle over the same state produces a byte-identical
			// finding, so the dedupe key notify derives from it is stable and
			// the reporter's cross-cycle dedupe suppresses re-emission
			// exactly like every other finding.
			again := comparer(filter.Options{}, false).Compare([]match.Match{m})
			if len(again) != 1 || !reflect.DeepEqual(again[0], f) {
				t.Errorf("finding not stable across cycles: %+v vs %+v", f, again[0])
			}
		})
	}
}

func TestCompareSeasonScopedFindingSeed(t *testing.T) {
	// A series whose seasons carry different groups: season 1 SubsPlease, season 2
	// Erai-raws, and item.Groups holds both. A SeaDex season-2 best for SubsPlease
	// must seed the finding from ONLY season 2's group (erai-raws), not the
	// whole-series set, in both CurrentGroup and the dedupe key.
	item := &library.Item{
		Title:        "Two Cour Show",
		Groups:       []string{"subsplease", "erai-raws"},
		SeasonGroups: map[int][]string{1: {"subsplease"}, 2: {"erai-raws"}},
	}
	entry := seadex.Entry{AniListID: 200, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/200"},
	}}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 2}}
	got := comparer(filter.Options{}, false).Compare([]match.Match{m})
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
	if got[0].CurrentGroup != "erai-raws" {
		t.Errorf("CurrentGroup = %q, want season-scoped %q (not whole-series subsplease,erai-raws)", got[0].CurrentGroup, "erai-raws")
	}
	if !reflect.DeepEqual(got[0].CurrentGroups, []string{"erai-raws"}) {
		t.Errorf("CurrentGroups = %v, want the season-scoped [erai-raws] (this structured set seeds notify's dedupe key)", got[0].CurrentGroups)
	}
}

func TestCompareSeasonScopedSingleGroupNotMixed(t *testing.T) {
	// A series that spans two groups across its seasons (item.Groups = lostyears,
	// pmr) but whose season 1 carries a single group (pmr). A season-1 SeaDex best
	// PMR release is aligned; the whole-series mixed-group flag must NOT trigger a
	// spurious mixed_group_manual finding for a season that is already aligned.
	item := &library.Item{
		Title:        "Split Group Show",
		Groups:       []string{"lostyears", "pmr"},
		SeasonGroups: map[int][]string{1: {"pmr"}},
	}
	entry := seadex.Entry{AniListID: 201, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "PMR", Tracker: "Nyaa", URL: "https://nyaa.si/view/201"},
	}}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}
	if got := comparer(filter.Options{}, false).Compare([]match.Match{m}); len(got) != 0 {
		t.Errorf("season-1 aligned item must produce no finding (not mixed_group_manual), got %+v", got)
	}
}

func TestCompareSeasonScopedRecommendationNotMaskedByOtherSeason(t *testing.T) {
	// Season 2 lacks a recommended group even though season 1 has one: a finding
	// for season 2 must still be produced (the whole-series group set must not
	// mask a later season that still needs the release).
	item := &library.Item{
		Title:        "Two Cour Show",
		Groups:       []string{"subsplease", "erai-raws"},
		SeasonGroups: map[int][]string{1: {"subsplease"}, 2: {"erai-raws"}},
	}
	entry := seadex.Entry{AniListID: 202, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/202"},
	}}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 2}}
	got := comparer(filter.Options{}, false).Compare([]match.Match{m})
	if len(got) != 1 {
		t.Fatalf("season 2 missing a recommended group must produce a finding even when another season has it, got %d", len(got))
	}
	if got[0].Status != StatusBetter {
		t.Errorf("status = %q, want %q", got[0].Status, StatusBetter)
	}
}

func TestCompareTheoreticalBestIsInfo(t *testing.T) {
	// A real season must be on disk: file presence is checked before the
	// recommendation-emptiness nudge, so a fileless item stays silent.
	item := &library.Item{Title: "X", Arr: library.ArrSonarr, Groups: []string{"whatever"}, SeasonGroups: map[int][]string{1: {"whatever"}}}
	entry := seadex.Entry{AniListID: 1, TheoreticalBest: "a stated remux"}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{}}
	got := comparer(filter.Options{}, false).Compare([]match.Match{m})
	if len(got) != 1 || got[0].Status != StatusTheoretical || got[0].Severity != SevInfo {
		t.Fatalf("expected one theoretical_best/info finding, got %+v", got)
	}
}

func TestCompareSeasonNotOnDiskTheoreticalIsSilent(t *testing.T) {
	// File presence comes before the recommendation-emptiness check: a
	// theoretical-best-only entry whose mapped season is not on disk has
	// nothing the nudge could apply to, so the daemon stays quiet (the audit
	// records the same scope as no_file).
	item := &library.Item{Title: "Short", Groups: []string{"a"}, SeasonGroups: map[int][]string{1: {"a"}}}
	entry := seadex.Entry{AniListID: 404, TheoreticalBest: "a stated remux"}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 3}}
	if got := comparer(filter.Options{}, false).Compare([]match.Match{m}); len(got) != 0 {
		t.Errorf("a theoretical-only entry for a season with no file must be silent, got %+v", got)
	}
}

func TestCompareSkipsNotInLibraryAndSpecials(t *testing.T) {
	notInLib := match.Match{Arr: library.ArrSonarr, Entry: seadex.Entry{AniListID: 1}}
	special := match.Match{
		Item:   &library.Item{Title: "OVA", Groups: []string{"x"}},
		Arr:    library.ArrSonarr,
		Entry:  seadex.Entry{AniListID: 2, Torrents: []seadex.Torrent{{IsBest: true, ReleaseGroup: "Y", Tracker: "Nyaa", URL: "https://nyaa.si/view/2"}}},
		Record: mapping.Record{Type: "OVA"},
	}
	got := comparer(filter.Options{}, true).Compare([]match.Match{notInLib, special})
	if len(got) != 0 {
		t.Errorf("not-in-library and excluded specials must be skipped, got %+v", got)
	}
}

func TestCompareAnimeBytesRecommendationRequiresOptIn(t *testing.T) {
	item := &library.Item{Title: "Private Tracker Show", Groups: []string{"erai-raws"}, SeasonGroups: map[int][]string{1: {"erai-raws"}}}
	entry := seadex.Entry{AniListID: 303, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "AB", URL: "/torrents.php?id=9&torrentid=10"},
	}}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}

	if got := comparer(filter.Options{}, false).Compare([]match.Match{m}); len(got) != 0 {
		t.Fatalf("AnimeBytes off must make AB-only recommendations silent, got %+v", got)
	}

	got := abComparer().Compare([]match.Match{m})
	if len(got) != 1 {
		t.Fatalf("AnimeBytes on should surface the AB recommendation, got %d", len(got))
	}
	if got[0].Status != StatusBetter || got[0].Severity != SevWarn {
		t.Errorf("status/severity = %q/%q, want better_release/warn", got[0].Status, got[0].Severity)
	}
	if got[0].Tracker != "AB" {
		t.Errorf("Tracker = %q, want AB", got[0].Tracker)
	}
	if got[0].ReleaseURL != "https://animebytes.tv/torrents.php?id=9&torrentid=10" {
		t.Errorf("ReleaseURL = %q, want AnimeBytes absolute URL", got[0].ReleaseURL)
	}
	if len(got[0].Links) != 1 || got[0].Links[0].URL != got[0].ReleaseURL {
		t.Errorf("Links = %+v, want the same obtainable AB release URL", got[0].Links)
	}
}

func TestCompareMixedGroupSeasonIsInfoNudge(t *testing.T) {
	// A mapped season whose episodes span two groups: the daemon cannot attribute
	// one current group, so it emits a mixed_group_manual info nudge with the
	// recommended fields filled for the manual review.
	item := &library.Item{Title: "Mixed", Groups: []string{"a", "b"}, SeasonGroups: map[int][]string{1: {"a", "b"}}}
	entry := seadex.Entry{AniListID: 400, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/400"},
	}}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}
	got := comparer(filter.Options{}, false).Compare([]match.Match{m})
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(got), got)
	}
	if got[0].Status != StatusMixedGroup || got[0].Severity != SevInfo {
		t.Errorf("status/severity = %q/%q, want mixed_group_manual/info", got[0].Status, got[0].Severity)
	}
	if got[0].RecommendedGroup != "SubsPlease" {
		t.Errorf("RecommendedGroup = %q, want SubsPlease (fillBest must run for the nudge)", got[0].RecommendedGroup)
	}
}

func TestCompareAlignedMixedGroupSeasonIsSilent(t *testing.T) {
	// Alignment wins over the mixed-group nudge: a season that spans two groups
	// but already carries a recommended one is aligned, so it must not nag as
	// mixed_group_manual (the audit reports the same row as have_best).
	item := &library.Item{Title: "Aligned Mixed", Groups: []string{"subsplease", "erai-raws"}, SeasonGroups: map[int][]string{1: {"subsplease", "erai-raws"}}}
	entry := seadex.Entry{AniListID: 405, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/405"},
	}}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}
	if got := comparer(filter.Options{}, false).Compare([]match.Match{m}); len(got) != 0 {
		t.Errorf("an aligned multi-group season must produce no finding, got %+v", got)
	}
}

func TestCompareSeasonNotOnDiskIsSilent(t *testing.T) {
	// SeaDex maps the entry to season 3 but only season 1 is on disk: there is
	// nothing on disk a better release would replace, so the daemon stays quiet
	// (the audit report records this as no_file).
	item := &library.Item{Title: "Short", Groups: []string{"a"}, SeasonGroups: map[int][]string{1: {"a"}}}
	entry := seadex.Entry{AniListID: 401, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/401"},
	}}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 3}}
	if got := comparer(filter.Options{}, false).Compare([]match.Match{m}); len(got) != 0 {
		t.Errorf("a mapped season with no file must produce no finding, got %+v", got)
	}
}

func TestCompareIncompleteSeasonEntryIsInfo(t *testing.T) {
	// Season-scoped path (not whole-series): an incomplete SeaDex entry whose
	// recommendation the item lacks downgrades better_release to incomplete/info.
	item := &library.Item{Title: "Incomplete", Groups: []string{"erai-raws"}, SeasonGroups: map[int][]string{1: {"erai-raws"}}}
	entry := seadex.Entry{AniListID: 402, Incomplete: true, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/402"},
	}}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}
	got := comparer(filter.Options{}, false).Compare([]match.Match{m})
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
	if got[0].Status != StatusIncomplete || got[0].Severity != SevInfo {
		t.Errorf("status/severity = %q/%q, want incomplete/info", got[0].Status, got[0].Severity)
	}
}

func TestRecommendedSkipsNonBestAndContentFiltered(t *testing.T) {
	// A non-best torrent is never a recommendation, and a remux best is dropped
	// by the content filter when exclude_remux is on. With nothing surviving and
	// the entry neither incomplete nor theoretical-best, the daemon is silent (a
	// release the operator filtered out is absent, never a finding).
	item := &library.Item{Title: "Filtered", Groups: []string{"erai-raws"}, SeasonGroups: map[int][]string{1: {"erai-raws"}}}
	entry := seadex.Entry{AniListID: 403, Notes: "BD remux", Torrents: []seadex.Torrent{
		{IsBest: false, ReleaseGroup: "AltGrp", Tracker: "Nyaa", URL: "https://nyaa.si/view/403"},
		{IsBest: true, ReleaseGroup: "RemuxGrp", Tracker: "Nyaa", URL: "https://nyaa.si/view/404"},
	}}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}
	got := comparer(filter.Options{ExcludeRemux: true}, false).Compare([]match.Match{m})
	if len(got) != 0 {
		t.Errorf("non-best and remux-filtered releases must yield no finding, got %+v", got)
	}
}

func TestCompareMislabeledAnimeBytesURLRequiresOptIn(t *testing.T) {
	// The tracker label is untrusted upstream data: a torrent claiming "Nyaa"
	// but carrying an animebytes.tv URL - absolute, schemeless, or host:port -
	// must be invisible while the AnimeBytes toggle is off (URL-aware guard on
	// the RAW upstream URL), and surface only when it is on AND the URL still
	// yields a usable link. Both the absolute and the schemeless forms must
	// then publish the ANIMEBYTES URL (UsableURL recovers the schemeless
	// form's canonical host rather than base-prefixing it under the wrong
	// label). The host:port form hides its host from UsableURL
	// (hidden-host: no followable link can be published), so it stays absent
	// even with the toggle on - an unusable URL is never obtainable evidence.
	const absURL = "https://animebytes.tv/torrents.php?id=9&torrentid=10"
	for _, tc := range []struct {
		sneakyURL string
		wantOn    int
	}{
		{absURL, 1},
		{"animebytes.tv/torrents.php?id=9&torrentid=10", 1},
		{"animebytes.tv:443/torrents.php?id=9&torrentid=10", 0},
	} {
		t.Run(tc.sneakyURL, func(t *testing.T) {
			item := &library.Item{Title: "Mislabeled", Groups: []string{"erai-raws"}, SeasonGroups: map[int][]string{1: {"erai-raws"}}}
			entry := seadex.Entry{AniListID: 500, Torrents: []seadex.Torrent{
				{IsBest: true, ReleaseGroup: "Sneaky", Tracker: "Nyaa", URL: tc.sneakyURL},
			}}
			m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}

			if got := comparer(filter.Options{}, false).Compare([]match.Match{m}); len(got) != 0 {
				t.Fatalf("AnimeBytes off must hide a mislabeled AB-URL recommendation, got %+v", got)
			}
			got := abComparer().Compare([]match.Match{m})
			if len(got) != tc.wantOn {
				t.Fatalf("AnimeBytes on: got %d findings, want %d", len(got), tc.wantOn)
			}
			if tc.wantOn == 1 && got[0].ReleaseURL != absURL {
				t.Errorf("ReleaseURL = %q, want the AB URL %q", got[0].ReleaseURL, absURL)
			}
		})
	}
}

func TestCompareMislabeledAnimeBytesURLChangesLinkSet(t *testing.T) {
	// The obtainable-link set must classify links by the same toggle boundary
	// the candidate filter uses (URL-aware, label untrusted): a same-group
	// best on animebytes.tv mislabeled "Nyaa" is invisible with AnimeBytes
	// off, and enabling the toggle must CHANGE the finding's link set - the
	// component notify's dedupe key folds in - so the newly obtainable source
	// re-surfaces instead of staying suppressed as already alerted.
	item := &library.Item{Title: "Mislabeled Key", Groups: []string{"erai-raws"}, SeasonGroups: map[int][]string{1: {"erai-raws"}}}
	entry := seadex.Entry{AniListID: 501, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/501"},
		{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://animebytes.tv/torrents.php?id=9&torrentid=501"},
	}}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}

	off := comparer(filter.Options{}, false).Compare([]match.Match{m})
	if len(off) != 1 {
		t.Fatalf("AnimeBytes off should still surface the public recommendation, got %d", len(off))
	}
	on := abComparer().Compare([]match.Match{m})
	if len(on) != 1 {
		t.Fatalf("AnimeBytes on should surface the recommendation, got %d", len(on))
	}
	if reflect.DeepEqual(off[0].Links, on[0].Links) {
		t.Errorf("link set must change when the toggle surfaces a mislabeled AB link, got %+v both ways", on[0].Links)
	}
	if len(on[0].Links) != 2 {
		t.Errorf("Links with AnimeBytes on = %+v, want both the public and the mislabeled AB source", on[0].Links)
	}
}

func TestCompareUnknownTrackerRecommendationIsSilent(t *testing.T) {
	item := &library.Item{Title: "Unknown Tracker Show", Groups: []string{"erai-raws"}, SeasonGroups: map[int][]string{1: {"erai-raws"}}}
	entry := seadex.Entry{AniListID: 600, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "SomePrivateTracker", URL: "https://tracker.example/view/1"},
	}}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}

	if got := abComparer().Compare([]match.Match{m}); len(got) != 0 {
		t.Errorf("an unknown-tracker recommendation is unobtainable and must be silent, got %+v", got)
	}
}

func TestObtainableLinksSkipsEmptyURL(t *testing.T) {
	cands := []candidate{
		{rel: release.Release{Tracker: "Nyaa"}, torrent: seadex.Torrent{Tracker: "Nyaa"}},
		{rel: release.Release{Tracker: "Nyaa"}, torrent: seadex.Torrent{Tracker: "Nyaa", URL: "https://nyaa.si/view/2"}},
	}

	links := obtainableLinks(cands)

	if len(links) != 1 || links[0].URL != "https://nyaa.si/view/2" {
		t.Errorf("obtainableLinks() = %+v, want only the URL-carrying link", links)
	}
}

func TestCompareFindingCarriesClassifiedReleaseFields(t *testing.T) {
	item := &library.Item{Title: "Payload", Groups: []string{"erai-raws"}, SeasonGroups: map[int][]string{1: {"erai-raws"}}}
	entry := seadex.Entry{AniListID: 700, Torrents: []seadex.Torrent{{
		IsBest:       true,
		ReleaseGroup: "SubsPlease",
		Tracker:      "Nyaa",
		URL:          "https://nyaa.si/view/700",
		InfoHash:     "deadbeef",
		DualAudio:    true,
		Files:        []seadex.File{{Name: "[SubsPlease] Payload - 01 [1080p][HEVC].mkv"}},
	}}}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}

	got := comparer(filter.Options{}, false).Compare([]match.Match{m})

	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
	f := got[0]
	if f.Resolution != "1080p" {
		t.Errorf("Resolution = %q, want 1080p", f.Resolution)
	}
	if f.Codec != "x265" {
		t.Errorf("Codec = %q, want x265 (HEVC normalizes to x265)", f.Codec)
	}
	if f.Kind != string(release.KindEncode) {
		t.Errorf("Kind = %q, want %q", f.Kind, release.KindEncode)
	}
	if !f.DualAudio {
		t.Error("DualAudio must carry the SeaDex dual-audio flag onto the finding")
	}
	if f.InfoHash != "deadbeef" {
		t.Errorf("InfoHash = %q, want deadbeef", f.InfoHash)
	}
	if f.Reason == "" {
		t.Error("classification reason must be filled")
	}
}

// TestCompareCurationWarnedBestExcluded pins the curation-warning gate on the
// findings path: a SeaDex best tagged Broken/Incomplete (case-insensitive,
// exact) is never recommended - an entry whose only best is warned emits
// nothing (or its theoretical-best nudge, unchanged), and a warned best
// beside an unwarned one recommends only the unwarned release.
func TestCompareCurationWarnedBestExcluded(t *testing.T) {
	newItem := func() *library.Item {
		return &library.Item{Title: "Warned", Groups: []string{"erai-raws"}, SeasonGroups: map[int][]string{1: {"erai-raws"}}}
	}

	t.Run("warned-only best is silent", func(t *testing.T) {
		for _, tag := range []string{"Broken", "BROKEN", "Incomplete"} {
			entry := seadex.Entry{AniListID: 800, Torrents: []seadex.Torrent{
				{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/800", Tags: []string{"dual", tag}},
			}}
			m := match.Match{Item: newItem(), Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}
			if got := comparer(filter.Options{}, false).Compare([]match.Match{m}); len(got) != 0 {
				t.Errorf("tag %q: a warned-only best must produce no finding, got %+v", tag, got)
			}
		}
	})

	t.Run("warned-only best keeps theoretical fallback", func(t *testing.T) {
		entry := seadex.Entry{AniListID: 801, TheoreticalBest: "a stated remux", Torrents: []seadex.Torrent{
			{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/801", Tags: []string{"Broken"}},
		}}
		m := match.Match{Item: newItem(), Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}
		got := comparer(filter.Options{}, false).Compare([]match.Match{m})
		if len(got) != 1 || got[0].Status != StatusTheoretical || got[0].Severity != SevInfo {
			t.Fatalf("expected the theoretical_best/info nudge with every best warned, got %+v", got)
		}
	})

	t.Run("unwarned best beside a warned one is recommended alone", func(t *testing.T) {
		entry := seadex.Entry{AniListID: 802, Torrents: []seadex.Torrent{
			{IsBest: true, ReleaseGroup: "BrokenGrp", Tracker: "Nyaa", URL: "https://nyaa.si/view/802", Tags: []string{"Broken"}},
			{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/803"},
		}}
		m := match.Match{Item: newItem(), Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}
		got := comparer(filter.Options{}, false).Compare([]match.Match{m})
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
		if !reflect.DeepEqual(got[0].RecommendedGroups, []string{"subsplease"}) {
			t.Errorf("RecommendedGroups = %v, want only the unwarned [subsplease]", got[0].RecommendedGroups)
		}
		if len(got[0].Links) != 1 || got[0].Links[0].URL != "https://nyaa.si/view/803" {
			t.Errorf("Links = %+v, want only the unwarned release's link", got[0].Links)
		}
	})

	t.Run("warned group already on disk is not aligned", func(t *testing.T) {
		// The library holds the warned best's own group: with the warned best
		// excluded there is no recommendation at all, so the daemon stays
		// silent (report-by-exception) rather than reading the item aligned.
		item := &library.Item{Title: "HasWarned", Groups: []string{"brokengrp"}, SeasonGroups: map[int][]string{1: {"brokengrp"}}}
		entry := seadex.Entry{AniListID: 804, Torrents: []seadex.Torrent{
			{IsBest: true, ReleaseGroup: "BrokenGrp", Tracker: "Nyaa", URL: "https://nyaa.si/view/804", Tags: []string{"Broken"}},
		}}
		m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}
		if got := comparer(filter.Options{}, false).Compare([]match.Match{m}); len(got) != 0 {
			t.Errorf("an entry with only a warned best must stay silent, got %+v", got)
		}
	})
}

func TestCompareFindingSeasonField(t *testing.T) {
	entry := seadex.Entry{AniListID: 210, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/210"},
	}}

	t.Run("season-scoped finding carries the mapped TVDB season", func(t *testing.T) {
		item := &library.Item{Title: "Seasoned", Groups: []string{"erai-raws"}, SeasonGroups: map[int][]string{2: {"erai-raws"}}}
		m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 2}}
		got := comparer(filter.Options{}, false).Compare([]match.Match{m})
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
		if got[0].Season != 2 {
			t.Errorf("Season = %d, want the mapped TVDB season 2", got[0].Season)
		}
	})

	t.Run("negative Fribb season clamps to 0", func(t *testing.T) {
		item := &library.Item{Title: "Negative Season", Arr: library.ArrSonarr, Groups: []string{"erai-raws"}, SeasonGroups: map[int][]string{1: {"erai-raws"}}}
		m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{Type: "TV", SeasonTvdb: -1}}
		got := comparer(filter.Options{}, false).Compare([]match.Match{m})
		if len(got) != 1 {
			t.Fatalf("expected 1 whole-series finding, got %d", len(got))
		}
		if got[0].Season != 0 {
			t.Errorf("Season = %d, want 0 (a negative season.tvdb must clamp, not leak into the slog field)", got[0].Season)
		}
	})
}
