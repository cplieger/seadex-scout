package indexer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/slogx/capture"
)

// advanceFixture is the seed snapshot the Advance tests start from: a current-schema
// snapshot (so loadPrevious does not baseline) carrying a POPULATED per-entry curation
// ownership fact, one already-published journal item, a harvested title and a harvest
// cursor. Every member is non-empty on purpose - the window pass must not act on
// absence from its own input, and a fixture whose ownership fact were empty could not
// tell a correct carry-through from a blanking. The ownership is attributed to THREE
// different AniList entries, because a window that evaluates entry 7 must replace entry
// 7's contribution and leave entries 8 and 9 alone.
func advanceFixture(firstSeen time.Time) *snapshot {
	return &snapshot{
		Version: currentFeedVersion,
		Owners: mergeOwners(
			ownsBy(7, hashed("nyaa:42", strings.Repeat("a", 40), true)),
			ownsBy(8, ownedRelease{Hash: strings.Repeat("b", 40)}),
			ownsBy(9, keyed("ab:1000", false), keyed("nyaa:99", true)),
		),
		Published:     map[string]bool{"nyaa:42": true},
		Titles:        map[string]string{"nyaa:42": "Harvested Show S01 [Group]"},
		HarvestCursor: "nyaa:7",
		NyaaFeed: []journalItem{{
			Key:       "nyaa:42",
			FirstSeen: firstSeen,
			AniListID: 7,
			Title:     "Harvested Show S01 [Group]",
			// The real journal GUID is the tracker's page URL, and it has to
			// be: both carry arms and the reader's link rebuild gate a
			// carried item on trackerKeyFromURL(GUID) == Key. A bare "nyaa:42"
			// here would be refused by every one of them.
			GUID:    "https://nyaa.si/view/42",
			PubDate: firstSeen,
		}},
		ABFeed: []journalItem{},
	}
}

// rawSnapshotMembers reads the persisted snapshot as raw JSON members, so a
// test can compare a member BYTE-FOR-BYTE across an Advance rather than
// through a decode that would normalize away a difference.
func rawSnapshotMembers(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		t.Fatalf("unmarshal snapshot members: %v", err)
	}
	return members
}

// advanceTestWriter is newTestWriter with the clock pinned to now, which every
// Advance assertion about journaling, aging and ordering needs.
func advanceTestWriter(path string, now time.Time) *FeedWriter {
	w := newTestWriter(path, "", false)
	w.now = func() time.Time { return now }
	return w
}

// TestAdvanceUpsertsWindowCurationWithoutDisturbingTheRest pins the SUPERSET contract
// the search curation index has as a PROJECTION of a per-entry ownership fact written
// by upsert-what-you-evaluated at either scope: the entries this window evaluated have
// their contribution replaced, so a release curated this tick is searchable within one
// tick rather than one reconcile interval (an empty proxied search is indistinguishable
// to an arr from "SeaDex curates nothing for this show"), and every entry the window
// did NOT evaluate keeps its stored contribution exactly, because absence from a window
// is not evidence.
func TestAdvanceUpsertsWindowCurationWithoutDisturbingTheRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	now := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	writeSnapshotFile(t, path, advanceFixture(now.Add(-time.Hour)))
	before := readSnapshotFile(t, path)

	// A window carrying an entirely new entry and identity.
	window := []seadex.Entry{nyaaEntry(88, 77, true, "New Show - S01E01 (1080p) [G].mkv")}
	if err := advanceTestWriter(path, now).Advance(t.Context(), window, nil); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	after := readSnapshotFile(t, path)
	// Every previously-owned entry survives with its contribution intact.
	for id, want := range before.Owners {
		got, still := after.Owners[id]
		if !still {
			t.Errorf("entry %s lost its curation ownership across a window pass; absence from a window is not evidence", id)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("entry %s contribution changed across a window pass:\n before %v\n  after %v", id, want, got)
		}
	}
	// And the derived index still answers for every pre-existing identity.
	for key, wantBest := range byKeyOf(&before) {
		gotBest, ok := byKeyOf(&after)[key]
		if !ok {
			t.Errorf("search key %q disappeared across a window pass", key)
			continue
		}
		if gotBest != wantBest {
			t.Errorf("search key %q best marker = %v, want the stored %v (a window must never lower a marker it cannot speak for)", key, gotBest, wantBest)
		}
	}
	for hash, wantBest := range byHashOf(&before) {
		if gotBest, ok := byHashOf(&after)[hash]; !ok || gotBest != wantBest {
			t.Errorf("search hash %q = (%v, present=%v), want the stored %v", hash, gotBest, ok, wantBest)
		}
	}
	for pair := range byPairOf(&before) {
		if !byPairOf(&after)[pair] {
			t.Errorf("pair relation %q disappeared across a window pass", pair)
		}
	}
	// The window's OWN curation is now searchable, which is the whole point.
	if _, ok := byKeyOf(&after)["nyaa:77"]; !ok {
		t.Errorf("the window's own release nyaa:77 is not searchable: %v", byKeyOf(&after))
	}
	if _, owned := after.Owners[ownerKey(88)]; !owned {
		t.Errorf("the window's own entry 88 did not gain a curation owner: %v", after.Owners)
	}

	// The advance must still have DONE its journal job, or the assertions above
	// would also hold for a no-op.
	if len(after.NyaaFeed) != 2 {
		t.Errorf("nyaa feed = %d items, want 2 (the carried item plus the new one)", len(after.NyaaFeed))
	}
}

// TestAdvanceRefusesSearchAdmissionUnderATagPolicy closes the one genuine regression
// risk in windowing the search index. The catalogue pass filters catalogue-wide
// (splitCurationWarned closing over shared info hashes) BEFORE building the index; a
// window closes only over itself, so a warned identity reachable only through an entry
// OUTSIDE the window is invisible to it, and admitting window keys on that evidence
// marks a release the operator explicitly excluded as curated for up to one reconcile
// interval. The gate is complete rather than mitigating: with any tag exclusion
// configured the window writes NO ownership at all.
func TestAdvanceRefusesSearchAdmissionUnderATagPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	now := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	writeSnapshotFile(t, path, advanceFixture(now.Add(-time.Hour)))
	before := readSnapshotFile(t, path)

	w := newExcludingTestWriter(path)
	w.now = func() time.Time { return now }

	// Nothing in this window is tagged, so a per-torrent filter would admit it.
	// It is refused anyway, because the window cannot prove the identity is not
	// warned through an entry it never saw.
	window := []seadex.Entry{nyaaEntry(88, 77, true, "Clean Show - S01E01 (1080p) [G].mkv")}
	if err := w.Advance(t.Context(), window, nil); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	after := readSnapshotFile(t, path)
	if !reflect.DeepEqual(after.Owners, before.Owners) {
		t.Errorf("a window pass wrote curation ownership under a tag policy:\n before %v\n  after %v", before.Owners, after.Owners)
	}
	if _, ok := byKeyOf(&after)["nyaa:77"]; ok {
		t.Error("the window admitted a key into the search index while a tag policy was configured; its warned closure is only window-wide")
	}
	// The RSS half still ran: the journal is not gated on the tag closure being
	// complete, because a warned torrent IN the window is filtered directly.
	if !slices.Contains(feedKeys(after.NyaaFeed), "nyaa:77") {
		t.Errorf("nyaa feed = %v, want the release still journaled", feedKeys(after.NyaaFeed))
	}
}

// TestAdvancePreservesTitlesAndHarvestCursorVerbatim covers the other two
// accumulating members a window cannot re-derive. Titles is the harvested
// real-release-name cache (built from Prowlarr over many rebuilds, and the
// reason the feed carries parseable titles at all) and HarvestCursor is the
// rotation position that keeps a deep show from starving its successors. Both
// would be lost by a pass that rewrote the snapshot from its argument, and
// neither loss is observable in the feed's item count.
func TestAdvancePreservesTitlesAndHarvestCursorVerbatim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	now := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	writeSnapshotFile(t, path, advanceFixture(now.Add(-time.Hour)))
	before := rawSnapshotMembers(t, path)

	window := []seadex.Entry{nyaaEntry(8, 77, true, "New Show - S01E01 (1080p) [G].mkv")}
	if err := advanceTestWriter(path, now).Advance(t.Context(), window, nil); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	after := rawSnapshotMembers(t, path)
	for _, member := range []string{"titles", "harvest_cursor"} {
		if len(after[member]) == 0 {
			t.Errorf("%s missing from the advanced snapshot; Advance must carry it through", member)
		}
		if string(after[member]) != string(before[member]) {
			t.Errorf("%s changed across Advance: before %s, after %s", member, before[member], after[member])
		}
	}
}

// TestAdvanceJournalsNewExpiresOldAndNeverReadmits pins Advance's three journal
// transitions in one pass, because they are one decision: what the window may change.
// A genuinely new torrent (absent from the publication log) is admitted, stamped now,
// and its identity recorded; a torrent already in the ledger is NOT re-admitted however
// the window presents it, since the never-pruned ledger would re-broadcast an old
// release as new on every tick; and a carried item past feedJournalMaxAge leaves, so
// the journal stays a recent-additions window rather than growing without bound between
// reconciles.
func TestAdvanceJournalsNewExpiresOldAndNeverReadmits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	now := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)

	fixture := advanceFixture(now.Add(-time.Hour))
	// A second carried item, already past the journal window.
	fixture.NyaaFeed = append(fixture.NyaaFeed, journalItem{
		Key:       "nyaa:99",
		FirstSeen: now.Add(-feedJournalMaxAge - time.Minute),
		AniListID: 9,
		Title:     "Aged Out S01", GUID: "nyaa:99", PubDate: now.Add(-feedJournalMaxAge - time.Minute),
	})
	fixture.Published["nyaa:99"] = true
	writeSnapshotFile(t, path, fixture)

	// The window re-presents the already-seen nyaa:42 AND a genuinely new
	// nyaa:77.
	window := []seadex.Entry{
		nyaaEntry(7, 42, true, "Harvested Show - S01E01 (1080p) [G].mkv"),
		nyaaEntry(8, 77, true, "New Show - S01E01 (1080p) [G].mkv"),
	}
	if err := advanceTestWriter(path, now).Advance(t.Context(), window, nil); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	snap := readSnapshotFile(t, path)
	keys := feedKeys(snap.NyaaFeed)
	if len(keys) != 2 {
		t.Fatalf("nyaa feed keys = %v, want exactly [nyaa:77 nyaa:42] in some order", keys)
	}
	counts := map[string]int{}
	for _, k := range keys {
		counts[k]++
	}
	if counts["nyaa:42"] != 1 {
		t.Errorf("nyaa:42 appears %d times, want 1 (carried once, never re-admitted from the window)", counts["nyaa:42"])
	}
	if counts["nyaa:77"] != 1 {
		t.Errorf("nyaa:77 appears %d times, want 1 (the genuinely new torrent)", counts["nyaa:77"])
	}
	if counts["nyaa:99"] != 0 {
		t.Errorf("nyaa:99 is still journaled, want it expired past feedJournalMaxAge")
	}
	if !snap.Published["nyaa:77"] {
		t.Errorf("publication log missing nyaa:77 after admission: %v", snap.Published)
	}
	// An expired item's identity stays in the ledger; that is what stops it
	// ever re-entering as new.
	if !snap.Published["nyaa:99"] {
		t.Errorf("publication log dropped the expired nyaa:99: %v (it could then re-enter as new)", snap.Published)
	}
	for i := range snap.NyaaFeed {
		if snap.NyaaFeed[i].Key != "nyaa:77" {
			continue
		}
		if !snap.NyaaFeed[i].FirstSeen.Equal(now) {
			t.Errorf("new item FirstSeen = %v, want %v", snap.NyaaFeed[i].FirstSeen, now)
		}
	}
	// The carried item keeps its original timestamp: a bumped FirstSeen would
	// silently extend its journal lifetime on every tick.
	for i := range snap.NyaaFeed {
		if snap.NyaaFeed[i].Key == "nyaa:42" && !snap.NyaaFeed[i].FirstSeen.Equal(now.Add(-time.Hour)) {
			t.Errorf("carried FirstSeen = %v, want the original %v", snap.NyaaFeed[i].FirstSeen, now.Add(-time.Hour))
		}
	}
}

// TestAdvanceLeavesBothFeedsSortedNewestFirst pins the sort, on BOTH feeds, for
// the reason the sortFeed call site in pass.go carries. Dropping it is silent
// and total: a newly admitted item appended at the TAIL, behind older carried
// items, is never reached, so the feed looks correct in every count assertion
// and delivers nothing.
func TestAdvanceLeavesBothFeedsSortedNewestFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	now := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)

	fixture := advanceFixture(now.Add(-2 * time.Hour))
	fixture.ABFeed = []journalItem{{
		Key:       "ab:1000",
		FirstSeen: now.Add(-3 * time.Hour),
		AniListID: 10,
		Title:     "Old AB S01",
		// A real AB journal GUID is the torrent permalink; the carry gates
		// key off it (see advanceFixture).
		GUID:    "https://animebytes.tv/torrents.php?id=86576&torrentid=1000",
		PubDate: now.Add(-3 * time.Hour),
	}}
	fixture.Published["ab:1000"] = true
	writeSnapshotFile(t, path, fixture)

	// AnimeBytes must be configured AND passkeyed for its journal to grow.
	w := NewFeedWriter(&FeedWriterConfig{
		Path:           path,
		NyaaTorznabURL: "http://prowlarr/1/api",
		ABTorznabURL:   "http://prowlarr/2/api",
		ABPasskey:      strings.Repeat("p", 32),
	}, nil, nil)
	w.now = func() time.Time { return now }

	window := []seadex.Entry{
		nyaaEntry(8, 77, true, "New Nyaa - S01E01 (1080p) [G].mkv"),
		{
			AniListID: 11,
			Torrents: []seadex.Torrent{{
				Tracker: "AB", URL: "/torrents.php?id=500&torrentid=2000", IsBest: true,
				Files: []seadex.File{{Name: "New AB - S01E01 (1080p) [G].mkv", Length: 1}},
			}},
		},
	}
	if err := w.Advance(t.Context(), window, nil); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	snap := readSnapshotFile(t, path)
	for _, tc := range []struct {
		name    string
		feed    []journalItem
		wantNew string
	}{
		{"nyaa", snap.NyaaFeed, "nyaa:77"},
		{"ab", snap.ABFeed, "ab:2000"},
	} {
		if len(tc.feed) != 2 {
			t.Errorf("%s feed = %d items, want 2 (one carried, one new): %v", tc.name, len(tc.feed), feedKeys(tc.feed))
			continue
		}
		for i := 1; i < len(tc.feed); i++ {
			if tc.feed[i-1].FirstSeen.Before(tc.feed[i].FirstSeen) {
				t.Errorf("%s feed is not newest-first: item %d (%v) is older than item %d (%v)",
					tc.name, i-1, tc.feed[i-1].FirstSeen, i, tc.feed[i].FirstSeen)
			}
		}
		if tc.feed[0].Key != tc.wantNew {
			t.Errorf("%s feed head = %q, want the newly admitted %q (an arr stops reading at the first stale item, so a new item at the tail is unreachable)",
				tc.name, tc.feed[0].Key, tc.wantNew)
		}
	}
}

// TestAdvanceDefersOverUnusableSnapshot pins the recovery boundary: a missing or
// malformed snapshot is the FULL pass's problem, and Advance must return nil while
// changing nothing. Baselining from a window would be the damaging alternative - the
// baseline path records "everything currently curated" in the publication log, and that
// ledger is never pruned, so from a window it would burn the window's identities as
// already-served (they could then never appear on RSS) and discard the entire journal
// in the same write.
func TestAdvanceDefersOverUnusableSnapshot(t *testing.T) {
	window := []seadex.Entry{nyaaEntry(8, 77, true, "New Show - S01E01 (1080p) [G].mkv")}
	tests := map[string]struct {
		// seed writes the pre-state, returning the exact bytes on disk (or "",
		// meaning no file at all).
		seed     func(t *testing.T, path string) string
		wantWarn bool
	}{
		"missing snapshot file": {
			seed:     func(*testing.T, string) string { return "" },
			wantWarn: true,
		},
		"malformed snapshot": {
			seed: func(t *testing.T, path string) string {
				t.Helper()
				const bad = `{"published":`
				if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
					t.Fatalf("write malformed snapshot: %v", err)
				}
				return bad
			},
			wantWarn: true,
		},
		"unsupported schema version": {
			// A snapshot at any other version re-baselines (settled
			// no-rollback-no-migration), which loadPrevious also reports as
			// baseline: the window must not migrate it either.
			seed: func(t *testing.T, path string) string {
				t.Helper()
				const old = `{"version":1,"owners":{},"published":{},"nyaa_feed":[],"ab_feed":[]}`
				if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
					t.Fatalf("write foreign-version snapshot: %v", err)
				}
				return old
			},
			wantWarn: true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "feed.json")
			want := tc.seed(t, path)
			log, rec := capture.New()
			w := NewFeedWriter(&FeedWriterConfig{
				Path:           path,
				NyaaTorznabURL: "http://prowlarr/1/api",
			}, log, nil)

			if err := w.Advance(t.Context(), window, nil); err != nil {
				t.Fatalf("Advance = %v, want nil (deferring to the next full rebuild is not an error)", err)
			}

			got, err := os.ReadFile(path)
			switch {
			case want == "":
				if err == nil {
					t.Errorf("Advance created %s (%d bytes); a window must not baseline a fresh install", path, len(got))
				}
			case err != nil:
				t.Fatalf("read snapshot after Advance: %v", err)
			case string(got) != want:
				t.Errorf("snapshot changed across a deferred Advance:\n before %s\n  after %s", want, got)
			}
			if tc.wantWarn && !rec.Contains("indexer feed snapshot unusable; deferring to the next full rebuild") {
				t.Errorf("deferral not warned; log output:\n%s", strings.Join(rec.Messages(), "\n"))
			}
		})
	}
}

// TestAdvanceHonoursExcludeTags pins the operator's tag policy on the growth path, in
// BOTH of its consequences. The feed one is obvious: an excluded release must not be
// journaled, or it is servable to the arrs for up to a full reconcile interval. The
// ledger one is why this is not just a count assertion - folding an excluded identity
// into the never-pruned publication log would make the exclusion PERMANENT, so an
// operator who later removed the tag from filters.exclude_tags could never get that
// release back on RSS.
func TestAdvanceHonoursExcludeTags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	now := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	writeSnapshotFile(t, path, advanceFixture(now.Add(-time.Hour)))

	w := newExcludingTestWriter(path)
	w.now = func() time.Time { return now }

	excluded := nyaaEntry(8, 77, true, "Broken Show - S01E01 (1080p) [G].mkv")
	excluded.Torrents[0].Tags = []string{"broken"}
	admitted := nyaaEntry(9, 78, true, "Clean Show - S01E01 (1080p) [G].mkv")

	if err := w.Advance(t.Context(), []seadex.Entry{excluded, admitted}, nil); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	snap := readSnapshotFile(t, path)
	keys := feedKeys(snap.NyaaFeed)
	if slices.Contains(keys, "nyaa:77") {
		t.Errorf("excluded release nyaa:77 was journaled: %v", keys)
	}
	if !slices.Contains(keys, "nyaa:78") {
		t.Errorf("nyaa feed = %v, want the unexcluded nyaa:78 admitted (the exclusion must be per-torrent, not per-window)", keys)
	}
	if snap.Published["nyaa:77"] {
		t.Errorf("excluded release nyaa:77 entered the never-pruned publication log: %v; "+
			"un-excluding the tag could then never restore it", snap.Published)
	}
}

// TestAdvanceExcludesAWarnedIdentityWithinTheWindow pins the identity CLOSURE at
// window scope, which the direct per-torrent filter alone does not give. SeaDex
// routinely lists one release on two trackers with a shared info hash and the warning
// tag on one occurrence only. Filtering torrent by torrent admits the untagged twin,
// journals it, and records its identity in the never-pruned seen ledger - after which
// the next full pass (whose graph DOES propagate the exclusion) retracts it and can
// never re-admit it, a permanent omission the feed's non-filtering stance rules out.
// The full pass still owns the exclusion only reachable outside the window.
func TestAdvanceExcludesAWarnedIdentityWithinTheWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	now := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	writeSnapshotFile(t, path, advanceFixture(now.Add(-time.Hour)))

	w := newExcludingTestWriter(path)
	w.now = func() time.Time { return now }

	// One release, two occurrences, one shared info hash. Only the Nyaa
	// occurrence carries the tag.
	const sharedHash = "aaaabbbbccccddddeeeeffff00001111222233ff"
	warned := nyaaEntry(8, 77, true, "Broken Show - S01E01 (1080p) [G].mkv")
	warned.Torrents[0].Tags = []string{"broken"}
	warned.Torrents[0].InfoHash = sharedHash
	twin := nyaaEntry(9, 79, true, "Broken Show - S01E01 (1080p) [G].mkv")
	twin.Torrents[0].InfoHash = sharedHash

	if err := w.Advance(t.Context(), []seadex.Entry{warned, twin}, nil); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	snap := readSnapshotFile(t, path)
	keys := feedKeys(snap.NyaaFeed)
	for _, key := range []string{"nyaa:77", "nyaa:79"} {
		if slices.Contains(keys, key) {
			t.Errorf("%s was journaled: %v; it shares an info hash with a warned occurrence, "+
				"so the full pass will retract it while the ledger keeps it out forever", key, keys)
		}
		if snap.Published[key] {
			t.Errorf("%s entered the never-pruned publication log: %v", key, snap.Published)
		}
	}
}

// TestAdvanceDoesNotRerenderCarriedItems pins the deliberate LIMIT of Advance, which
// is as much a contract as its capabilities. A carried item's stored form is left
// exactly as persisted, even when the window carries the same torrent and a re-render
// would produce a different title, so a title corrected upstream keeps its old form
// until the next full pass: re-rendering from a window is unsound in the general case
// (the render folds every entry a torrent is attached to, and a window sees only some
// of them). Without this test the carry path is free to drift into a partial re-render,
// which reads as an improvement and is a correctness loss.
func TestAdvanceDoesNotRerenderCarriedItems(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	now := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)

	fixture := advanceFixture(now.Add(-time.Hour))
	const storedTitle = "Deliberately Stale Stored Title S01 [Harvested]"
	fixture.NyaaFeed[0].Title = storedTitle
	fixture.Titles = map[string]string{}
	writeSnapshotFile(t, path, fixture)

	// The window re-presents nyaa:42 with file names that would render a
	// COMPLETELY different synthesized title.
	window := []seadex.Entry{nyaaEntry(7, 42, true, "Totally Different Name - S05E09 (2160p) [Other].mkv")}
	if err := advanceTestWriter(path, now).Advance(t.Context(), window, nil); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 1 {
		t.Fatalf("nyaa feed = %d items, want the 1 carried item: %v", len(snap.NyaaFeed), feedKeys(snap.NyaaFeed))
	}
	if got := snap.NyaaFeed[0].Title; got != storedTitle {
		t.Errorf("carried title = %q, want the stored %q unchanged; re-rendering from a window is the full pass's job",
			got, storedTitle)
	}
}

// TestAdvanceEmptyWindowIsANoOpWrite pins the quiet case. Advance is reached
// only when the tick found something, but an all-excluded or all-already-seen
// window reduces to nothing, and that must leave every accumulating member
// intact rather than writing a blanked snapshot.
func TestAdvanceEmptyWindowIsANoOpWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	now := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	writeSnapshotFile(t, path, advanceFixture(now.Add(-time.Hour)))
	before := rawSnapshotMembers(t, path)

	if err := advanceTestWriter(path, now).Advance(t.Context(), nil, nil); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	after := rawSnapshotMembers(t, path)
	for _, member := range []string{"owners", "published", "titles", "harvest_cursor", "nyaa_feed"} {
		if string(after[member]) != string(before[member]) {
			t.Errorf("%s changed across an empty Advance:\n before %s\n  after %s", member, before[member], after[member])
		}
	}
}

// TestAdvanceDropsForeignScopedCarriedItems pins expireCarried's scope gate: a
// journal item whose key does not belong to the feed it sits in is corruption
// (a hand-edited or cross-wired snapshot), and serving it would hand an arr a
// download link built for the wrong tracker.
func TestAdvanceDropsForeignScopedCarriedItems(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	now := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)

	fixture := advanceFixture(now.Add(-time.Hour))
	// An AB-keyed item sitting in the Nyaa feed.
	fixture.NyaaFeed = append(fixture.NyaaFeed, journalItem{
		Key:       "ab:1000",
		FirstSeen: now.Add(-30 * time.Minute),
		AniListID: 10,
		Title:     "Misfiled AB S01", GUID: "ab:1000", PubDate: now.Add(-30 * time.Minute),
	})
	writeSnapshotFile(t, path, fixture)

	window := []seadex.Entry{nyaaEntry(8, 77, true, "New Show - S01E01 (1080p) [G].mkv")}
	if err := advanceTestWriter(path, now).Advance(t.Context(), window, nil); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	keys := feedKeys(readSnapshotFile(t, path).NyaaFeed)
	if slices.Contains(keys, "ab:1000") {
		t.Errorf("nyaa feed = %v, want the AB-scoped item dropped", keys)
	}
	if !slices.Contains(keys, "nyaa:42") || !slices.Contains(keys, "nyaa:77") {
		t.Errorf("nyaa feed = %v, want the correctly-scoped carried and new items kept", keys)
	}
}

// TestAdvanceRebasesFutureFirstSeen pins the clock-skew arm on the carry path.
// A FirstSeen ahead of the wall clock (a rollback, or a snapshot restored from a
// future-skewed host) makes the max-age check see a negative age, so the item
// would outlive the journal window indefinitely - and its served pubDate would
// advertise a negative release age, which an arr delay profile holds forever.
func TestAdvanceRebasesFutureFirstSeen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	now := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)

	fixture := advanceFixture(now.Add(24 * time.Hour))
	writeSnapshotFile(t, path, fixture)

	window := []seadex.Entry{nyaaEntry(8, 77, true, "New Show - S01E01 (1080p) [G].mkv")}
	if err := advanceTestWriter(path, now).Advance(t.Context(), window, nil); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	snap := readSnapshotFile(t, path)
	for i := range snap.NyaaFeed {
		if snap.NyaaFeed[i].Key != "nyaa:42" {
			continue
		}
		if got := snap.NyaaFeed[i].FirstSeen; !got.Equal(now) {
			t.Errorf("future FirstSeen = %v, want it rebased to %v", got, now)
		}
		if got := snap.NyaaFeed[i].PubDate; !got.Equal(now) {
			t.Errorf("future PubDate = %v, want it rebased to %v", got, now)
		}
		return
	}
	t.Errorf("nyaa:42 was dropped rather than rebased: %v", feedKeys(snap.NyaaFeed))
}

// TestBothPassKindsRenderTheMirrorIdentityIdentically fails when the RSS fold
// reads anything narrower than the UNION of the item's journal key and its bare
// info hash: a reconcile and a tick then render different items for one release.
//
// Four gates keep the fixture from going vacuous. The key holder must carry the
// hash, the holders must agree on the best vote, and the hash must be valid
// 40-hex, or the result is dropped before any id is stamped. The fourth is pass
// ORDER: a window carries a journaled key verbatim, so each pass journals it first.
func TestBothPassKindsRenderTheMirrorIdentityIdentically(t *testing.T) {
	mirrorHash := strings.Repeat("e", 40)
	const keyHolderID, hashHolderID = 2001, 2002
	now := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)

	// The key holder is a series with one tvdb id; the hash-only holder is a film
	// on Radarr with another, so the two disagree on the id AND contribute
	// different categories.
	info := func(alID int) EntryInfo {
		switch alID {
		case keyHolderID:
			return EntryInfo{Title: "Mirror Show", TvdbID: 79525, Target: TargetSonarr}
		case hashHolderID:
			return EntryInfo{Title: "Mirror Film", IsMovie: true, TvdbID: 123456, Target: TargetRadarr}
		}
		return EntryInfo{}
	}
	keyHolder := nyaaEntry(keyHolderID, 777, true, "Mirror Show - S01E01 (1080p) [G].mkv")
	keyHolder.Torrents[0].InfoHash = mirrorHash
	hashOnly := seadex.Entry{AniListID: hashHolderID, Torrents: []seadex.Torrent{{
		Tracker: "AnimeTosho", URL: "https://animetosho.org/view/9", InfoHash: mirrorHash, IsBest: true,
		Files: []seadex.File{{Length: 7, Name: "Mirror Film (1080p) [Other].mkv"}},
	}}}
	// Prior state: neither the item's key nor its scope-namespaced hash is in the
	// publication log, so each pass journals it as new; the hash-only holder IS in
	// the ownership fact, which is the only record a window has of an owner it did
	// not evaluate.
	prior := func() *snapshot {
		return &snapshot{
			Published: map[string]bool{},
			Owners:    ownsBy(hashHolderID, ownedRelease{Hash: mirrorHash, IsBest: true}),
			NyaaFeed:  []journalItem{},
			ABFeed:    []journalItem{},
		}
	}

	reconcilePath := filepath.Join(t.TempDir(), "feed.json")
	writeSnapshotFile(t, reconcilePath, prior())
	if err := advanceTestWriter(reconcilePath, now).Rebuild(t.Context(), []seadex.Entry{keyHolder, hashOnly}, info); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	tickPath := filepath.Join(t.TempDir(), "feed.json")
	writeSnapshotFile(t, tickPath, prior())
	if err := advanceTestWriter(tickPath, now).Advance(t.Context(), []seadex.Entry{keyHolder}, info); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	reconciled := journalItemForKey(t, readSnapshotFile(t, reconcilePath).NyaaFeed, "nyaa:777")
	ticked := journalItemForKey(t, readSnapshotFile(t, tickPath).NyaaFeed, "nyaa:777")
	if reconciled.TvdbID != 0 {
		t.Errorf("reconcile TvdbID = %d, want 0: the two holders of this identity disagree, and a disagreement vetoes",
			reconciled.TvdbID)
	}
	if ticked.TvdbID != 0 {
		t.Errorf("tick TvdbID = %d, want 0: the carried owner disagrees with the occurrence the window holds",
			ticked.TvdbID)
	}
	if !reflect.DeepEqual(reconciled, ticked) {
		t.Errorf("the two pass kinds render different items for one release identity:\n reconcile %+v\n      tick %+v",
			reconciled, ticked)
	}
}

// TestAdvanceCarriesAPriorOwnersTwinTitleIntoTheVote is holders-agree on the
// film twin across the two records a window pass holds: the occurrence in the
// window and an unevaluated owner's stored SonarrTitle. An agreeing prior yields
// the twin and the MOVIE holder's Anime drop; a disagreeing one vetoes the twin
// and the release serves under both categories.
//
// The fixture is only live while the window holder itself produces a twin title
// and the prior owner's record names the item's journal KEY.
func TestAdvanceCarriesAPriorOwnersTwinTitleIntoTheVote(t *testing.T) {
	const windowHolderID, priorHolderID = 3001, 3002
	const windowTitle = "Minami-ke S00E02 1080p [G]"
	now := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	tests := map[string]struct {
		priorTitle string
		wantTitle  string
		wantCats   []int
	}{
		"prior owner agrees": {
			priorTitle: windowTitle, wantTitle: windowTitle, wantCats: []int{catMovies},
		},
		"prior owner disagrees": {
			priorTitle: "Minami-ke S00E03 1080p [G]", wantTitle: "", wantCats: []int{catMovies, catAnime},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "feed.json")
			writeSnapshotFile(t, path, &snapshot{
				Published: map[string]bool{},
				Owners:    ownsBy(priorHolderID, ownedRelease{Key: "nyaa:777", SonarrTitle: tc.priorTitle, IsBest: true}),
				NyaaFeed:  []journalItem{},
				ABFeed:    []journalItem{},
			})
			window := nyaaEntry(windowHolderID, 777, true, "Minami-ke Special (1080p) [G].mkv")
			window.Torrents[0].ReleaseGroup = "G"
			info := func(int) EntryInfo {
				return EntryInfo{Title: "Minami-ke Special", IsMovie: true, Target: TargetSonarr, TvdbID: 80000, SpecialEpisode: 2, SeriesTitle: "Minami-ke"}
			}
			if err := advanceTestWriter(path, now).Advance(t.Context(), []seadex.Entry{window}, info); err != nil {
				t.Fatalf("Advance: %v", err)
			}

			it := journalItemForKey(t, readSnapshotFile(t, path).NyaaFeed, "nyaa:777")
			if it.SonarrTitle != tc.wantTitle {
				t.Errorf("tick SonarrTitle = %q, want %q (prior owner stored %q)", it.SonarrTitle, tc.wantTitle, tc.priorTitle)
			}
			wantGUID := ""
			if tc.wantTitle != "" {
				wantGUID = twinGUID(it.GUID)
			}
			if it.SonarrGUID != wantGUID {
				t.Errorf("tick SonarrGUID = %q, want %q", it.SonarrGUID, wantGUID)
			}
			if !slices.Equal(it.Categories, tc.wantCats) {
				t.Errorf("tick Categories = %v, want %v", it.Categories, tc.wantCats)
			}
		})
	}
}

// feedKeys lists a feed's journal keys in persisted order.
func feedKeys(feed []journalItem) []string {
	keys := make([]string, 0, len(feed))
	for i := range feed {
		keys = append(keys, feed[i].Key)
	}
	return keys
}

// TestAdvanceKeepsACarriedItemWhoseGUIDLostItsIdentity pins the resolution of a
// genuine DISAGREEMENT between the two passes. Applying the GUID-identity gate to
// every carried item and DROPPING on failure makes the tick's verdict irreversible
// under a never-pruned publication log, so a tick permanently discards an item the
// reconcile would have repaired by a fresh render that self-heals the GUID. Only the
// reconcile holds that evidence (a fresh render needs every occurrence of the key),
// and leaving it alone costs nothing at the serve surface: the reader applies the same
// GUID-to-Key invariant when it rebuilds download links.
func TestAdvanceKeepsACarriedItemWhoseGUIDLostItsIdentity(t *testing.T) {
	for name, guid := range map[string]string{
		"a GUID naming another torrent id": "https://nyaa.si/view/9999",
		"a foreign host":                   "https://evil.example/view/42",
		"a bare key rather than a URL":     "nyaa:42",
		"an empty GUID":                    "",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "feed.json")
			now := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
			fixture := advanceFixture(now.Add(-time.Hour))
			fixture.NyaaFeed[0].GUID = guid
			writeSnapshotFile(t, path, fixture)

			// A window carrying an unrelated new release, so the pass persists.
			window := []seadex.Entry{nyaaEntry(88, 77, true, "New Nyaa - S01E01 (1080p) [G].mkv")}
			if err := advanceTestWriter(path, now).Advance(t.Context(), window, nil); err != nil {
				t.Fatalf("Advance: %v", err)
			}

			keys := feedKeys(readSnapshotFile(t, path).NyaaFeed)
			if guid == "" {
				// An empty GUID is refused by the shared DECODE gate, not by the
				// carry path: validJournalRecord requires a Key and a FirstSeen,
				// and the item's own GUID-less render is dropped at load by
				// pruneJournalFeed only when it also fails those. It survives
				// here for the same reason - the reconcile will re-render it.
				_ = keys
			}
			if !slices.Contains(keys, "nyaa:42") {
				t.Errorf("nyaa feed = %v, want the unproven-GUID nyaa:42 KEPT: only the reconcile can re-render and self-heal it, "+
					"and a window drop is permanent under the never-pruned publication log", keys)
			}
			if !slices.Contains(keys, "nyaa:77") {
				t.Errorf("nyaa feed = %v, want the untouched new item still admitted", keys)
			}
		})
	}
}

// TestAdvanceCompletesAPartialRenderFromStoredOwnerVotes pins the half of a window's
// render that its own evidence cannot supply. A window holds the occurrences of the
// entries a curator happened to touch, never a journal key's whole owner set (~4.4% of
// curated torrents are attached to several entries), so its render is authorized only
// as PARTIAL and is completed by carrying the stored votes of the owners it did NOT
// evaluate: best-wins on the download-volume-factor marker and a category union. Both
// are load-bearing - a best release would otherwise publish as an alt for a whole
// reconcile interval, and a film owned only by an unevaluated entry never reaches Radarr.
func TestAdvanceCompletesAPartialRenderFromStoredOwnerVotes(t *testing.T) {
	// A 40-hex info hash the window's own occurrence carries, so the stored
	// hash-side identity test has something to match.
	const sharedHash = "aaaabbbbccccddddeeeeffff0000111122223333"
	// movieOwner types entry 8 as a film and every other entry as a series, so a
	// carried category vote is visible as a second category on the item.
	movieOwner := func(alID int) EntryInfo { return EntryInfo{IsMovie: alID == 8} }

	tests := map[string]struct {
		desc string
		// prior is the ownership fact the PREVIOUS snapshot holds.
		prior map[string][]ownedRelease
		// windowHash is the info hash the window's own occurrence of nyaa:77
		// carries ("" for the hashless AnimeBytes-like shape).
		windowHash string
		wantMarker string
		wantCats   []int
	}{
		"unevaluated_owner_votes_best_by_key": {
			desc: "entry 8 is outside the window and its stored contribution names this key, " +
				"so its best vote and its movie typing complete the render",
			prior:      ownsBy(8, keyed("nyaa:77", true)),
			wantMarker: dvfBest,
			wantCats:   []int{catMovies, catAnime},
		},
		"unevaluated_owner_votes_best_by_hash": {
			desc: "entry 8's stored release names another key but the SAME info hash, " +
				"which is the snapshot's other identity form",
			prior:      ownsBy(8, hashed("nyaa:9999", sharedHash, true)),
			windowHash: sharedHash,
			wantMarker: dvfBest,
			wantCats:   []int{catMovies, catAnime},
		},
		"hashless_stored_release_under_another_key_owns_nothing": {
			desc: "a stored release with no hash and a different key matches nothing, " +
				"even though the item it is compared against also has no hash",
			prior:      ownsBy(8, keyed("nyaa:9999", true)),
			wantMarker: dvfAlt,
			wantCats:   []int{catAnime},
		},
		"replaced_owner_stale_best_vote_is_not_carried": {
			desc: "entry 9 IS this window's own entry, so its stored best vote was " +
				"replaced wholesale by the fresh evidence that demotes the release",
			prior:      ownsBy(9, keyed("nyaa:77", true)),
			wantMarker: dvfAlt,
			wantCats:   []int{catAnime},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "feed.json")
			now := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
			fixture := advanceFixture(now.Add(-time.Hour))
			fixture.Owners = tc.prior
			writeSnapshotFile(t, path, fixture)

			// The window evaluates entry 9 only, and its occurrence is NOT best:
			// every best marker below can only have come from a carried vote.
			entry := nyaaEntry(9, 77, false, "New Show - S01E01 (1080p) [G].mkv")
			entry.Torrents[0].InfoHash = tc.windowHash
			if err := advanceTestWriter(path, now).Advance(t.Context(), []seadex.Entry{entry}, movieOwner); err != nil {
				t.Fatalf("Advance: %v", err)
			}

			snap := readSnapshotFile(t, path)
			var got journalItem
			for i := range snap.NyaaFeed {
				if snap.NyaaFeed[i].Key == "nyaa:77" {
					got = snap.NyaaFeed[i]
				}
			}
			if got.Key == "" {
				t.Fatalf("nyaa:77 was not journaled (%s): feed = %v", tc.desc, feedKeys(snap.NyaaFeed))
			}
			if got.DownloadVolumeFactor != tc.wantMarker {
				t.Errorf("Advance(prior %v) nyaa:77 marker = %q, want %q (%s)",
					tc.prior, got.DownloadVolumeFactor, tc.wantMarker, tc.desc)
			}
			if !slices.Equal(got.Categories, tc.wantCats) {
				t.Errorf("Advance(prior %v) nyaa:77 categories = %v, want %v ascending (%s)",
					tc.prior, got.Categories, tc.wantCats, tc.desc)
			}
		})
	}
}
