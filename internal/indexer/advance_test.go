package indexer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/slogx/capture"
)

// advanceFixture is the seed snapshot the Advance tests start from: a
// journal-schema snapshot (Seen non-nil, so loadPrevious does not baseline)
// carrying a POPULATED search curation index, one already-seen journal item,
// a harvested title and a harvest cursor.
//
// Every member is non-empty on purpose. Advance's whole reason for existing is
// that it must not re-derive from a window what only a whole catalogue can
// speak for, and a fixture whose curation index were empty could not tell a
// correct carry-through from a blanking.
func advanceFixture(firstSeen time.Time) *snapshot {
	return &snapshot{
		ByHash: map[string]bool{
			strings.Repeat("a", 40): true,
			strings.Repeat("b", 40): false,
		},
		ByKey: map[string]bool{
			"nyaa:42": true,
			"ab:1000": false,
			"nyaa:99": true,
		},
		ByPair: map[string]bool{
			strings.Repeat("a", 40) + "|nyaa:42": true,
		},
		Seen:          map[string]bool{"nyaa:42": true},
		Titles:        map[string]string{"nyaa:42": "Harvested Show S01 [Group]"},
		HarvestCursor: "nyaa:7",
		NyaaFeed: []journalItem{{
			Key:       "nyaa:42",
			FirstSeen: firstSeen,
			AniListID: 7,
			item: item{
				Title:   "Harvested Show S01 [Group]",
				GUID:    "nyaa:42",
				PubDate: firstSeen,
			},
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

// TestAdvancePreservesSearchCurationIndexVerbatim is the most load-bearing
// assertion in this file.
//
// Rebuild REPLACES the search curation index from its argument (buildCuration),
// which is correct only against a COMPLETE catalogue. Advance is handed a
// window - a handful of recently-changed entries - so if it went anywhere near
// that code path the index would shrink from the whole catalogue (~8700
// identities) to the window's few, and every Prowlarr search would stop
// matching until the next full pass up to a reconcile interval later. The
// failure would be invisible in the RSS feed, which is the surface these tests
// mostly watch.
//
// The comparison is on the RAW persisted bytes of by_hash, by_key and by_pair,
// not on decoded maps: nothing here should touch those members at all, so the
// strictest available assertion is the right one.
func TestAdvancePreservesSearchCurationIndexVerbatim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	now := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	writeSnapshotFile(t, path, advanceFixture(now.Add(-time.Hour)))
	before := rawSnapshotMembers(t, path)

	// A window carrying an entirely different identity: were the index rebuilt
	// from it, by_key would become {"nyaa:77": true} and both other members
	// would empty.
	window := []seadex.Entry{nyaaEntry(8, 77, true, "New Show - S01E01 (1080p) [G].mkv")}
	if err := advanceTestWriter(path, now).Advance(context.Background(), window, nil); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	after := rawSnapshotMembers(t, path)
	for _, member := range []string{"by_hash", "by_key", "by_pair"} {
		if !json.Valid(after[member]) || len(after[member]) == 0 {
			t.Fatalf("%s missing from the advanced snapshot; Advance must carry the curation index through", member)
		}
		if string(after[member]) != string(before[member]) {
			t.Errorf("%s changed across Advance:\n before %s\n  after %s\n"+
				"a window cannot speak for the whole curation index; only a full Rebuild may rewrite it",
				member, before[member], after[member])
		}
	}

	// The advance must still have DONE its job, or the assertions above would
	// also hold for a no-op.
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 2 {
		t.Errorf("nyaa feed = %d items, want 2 (the carried item plus the new one)", len(snap.NyaaFeed))
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
	if err := advanceTestWriter(path, now).Advance(context.Background(), window, nil); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	after := rawSnapshotMembers(t, path)
	for _, member := range []string{"titles", "harvest_cursor"} {
		if len(after[member]) == 0 {
			t.Fatalf("%s missing from the advanced snapshot; Advance must carry it through", member)
		}
		if string(after[member]) != string(before[member]) {
			t.Errorf("%s changed across Advance: before %s, after %s", member, before[member], after[member])
		}
	}
}

// TestAdvanceJournalsNewExpiresOldAndNeverReadmits pins Advance's three
// journal transitions in one pass, because they are one decision: what the
// window may change.
//
//   - a genuinely new torrent (absent from the seen ledger) is admitted,
//     stamped now, and its identity recorded;
//   - a torrent already in the ledger is NOT re-admitted, however the window
//     presents it - the ledger is never pruned, so a re-admission would
//     re-broadcast an old release as new on every tick;
//   - a carried item past feedJournalMaxAge leaves, so the journal stays a
//     recent-additions window rather than growing without bound between
//     reconciles.
func TestAdvanceJournalsNewExpiresOldAndNeverReadmits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	now := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)

	fixture := advanceFixture(now.Add(-time.Hour))
	// A second carried item, already past the journal window.
	fixture.NyaaFeed = append(fixture.NyaaFeed, journalItem{
		Key:       "nyaa:99",
		FirstSeen: now.Add(-feedJournalMaxAge - time.Minute),
		AniListID: 9,
		item:      item{Title: "Aged Out S01", GUID: "nyaa:99", PubDate: now.Add(-feedJournalMaxAge - time.Minute)},
	})
	fixture.Seen["nyaa:99"] = true
	writeSnapshotFile(t, path, fixture)

	// The window re-presents the already-seen nyaa:42 AND a genuinely new
	// nyaa:77.
	window := []seadex.Entry{
		nyaaEntry(7, 42, true, "Harvested Show - S01E01 (1080p) [G].mkv"),
		nyaaEntry(8, 77, true, "New Show - S01E01 (1080p) [G].mkv"),
	}
	if err := advanceTestWriter(path, now).Advance(context.Background(), window, nil); err != nil {
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
	if !snap.Seen["nyaa:77"] {
		t.Errorf("seen ledger missing nyaa:77 after admission: %v", snap.Seen)
	}
	// An expired item's identity stays in the ledger; that is what stops it
	// ever re-entering as new.
	if !snap.Seen["nyaa:99"] {
		t.Errorf("seen ledger dropped the expired nyaa:99: %v (it could then re-enter as new)", snap.Seen)
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

// TestAdvanceLeavesBothFeedsSortedNewestFirst pins the sort, on BOTH feeds,
// because dropping it is silent and total.
//
// The reader serves the persisted order with no sort of its own, and an arr
// walking an RSS feed stops at the first item older than its last sync - so a
// newly admitted item appended at the TAIL, behind older carried items, is
// simply never reached. The feed would look correct in every count assertion
// and deliver nothing, which is the exact freshness this whole path exists to
// provide.
func TestAdvanceLeavesBothFeedsSortedNewestFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	now := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)

	fixture := advanceFixture(now.Add(-2 * time.Hour))
	fixture.ABFeed = []journalItem{{
		Key:       "ab:1000",
		FirstSeen: now.Add(-3 * time.Hour),
		AniListID: 10,
		item:      item{Title: "Old AB S01", GUID: "ab:1000", PubDate: now.Add(-3 * time.Hour)},
	}}
	fixture.Seen["ab:1000"] = true
	writeSnapshotFile(t, path, fixture)

	// AnimeBytes must be configured AND passkeyed for its journal to grow.
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{
		NyaaTorznabURL: "http://prowlarr/1/api",
		ABTorznabURL:   "http://prowlarr/2/api",
		ABPasskey:      strings.Repeat("p", 32),
	}}, nil, nil)
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
	if err := w.Advance(context.Background(), window, nil); err != nil {
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

// TestAdvanceDefersOverUnusableSnapshot pins the recovery boundary: a missing
// or malformed snapshot is the FULL pass's problem, and Advance must return nil
// while changing nothing.
//
// Baselining from a window would be the damaging alternative. The baseline path
// records "everything currently curated" in the seen ledger, and that ledger is
// never pruned - so from a window it would burn the window's identities as
// already-served (they could then never appear on RSS) and discard the entire
// journal in the same write.
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
				const bad = `{"seen":`
				if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
					t.Fatalf("write malformed snapshot: %v", err)
				}
				return bad
			},
			wantWarn: true,
		},
		"pre-journal-schema snapshot": {
			// A snapshot with no seen ledger is the retired schema, which
			// loadPrevious also reports as baseline: the window must not
			// migrate it either.
			seed: func(t *testing.T, path string) string {
				t.Helper()
				const old = `{"by_hash":{},"by_key":{},"nyaa_feed":[],"ab_feed":[]}`
				if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
					t.Fatalf("write legacy snapshot: %v", err)
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
				UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"},
			}, log, nil)

			if err := w.Advance(context.Background(), window, nil); err != nil {
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

// TestAdvanceHonoursExcludeTags pins the operator's tag policy on the growth
// path, in BOTH of its consequences.
//
// The feed one is obvious: an excluded release must not be journaled, or it is
// servable to the arrs for up to a full reconcile interval. The ledger one is
// the reason this test is not just a count assertion - folding an excluded
// identity into the never-pruned seen ledger would make the exclusion
// PERMANENT, so an operator who later removed the tag from filters.exclude_tags
// could never get that release back on RSS.
func TestAdvanceHonoursExcludeTags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	now := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	writeSnapshotFile(t, path, advanceFixture(now.Add(-time.Hour)))

	w := newExcludingTestWriter(path)
	w.now = func() time.Time { return now }

	excluded := nyaaEntry(8, 77, true, "Broken Show - S01E01 (1080p) [G].mkv")
	excluded.Torrents[0].Tags = []string{"broken"}
	admitted := nyaaEntry(9, 78, true, "Clean Show - S01E01 (1080p) [G].mkv")

	if err := w.Advance(context.Background(), []seadex.Entry{excluded, admitted}, nil); err != nil {
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
	if snap.Seen["nyaa:77"] {
		t.Errorf("excluded release nyaa:77 entered the never-pruned seen ledger: %v; "+
			"un-excluding the tag could then never restore it", snap.Seen)
	}
}

// TestAdvanceDoesNotRerenderCarriedItems pins the deliberate LIMIT of Advance,
// which is as much a contract as its capabilities.
//
// A carried item's stored form is left exactly as persisted, even when the
// window carries the same torrent and a re-render would produce a different
// title. That is why the docstring calls out that a title corrected upstream
// keeps its old form until the next full pass: re-rendering from a window is
// unsound in the general case (the render folds every entry a torrent is
// attached to, and a window sees only some of them), so the full pass owns it.
//
// Without this test the "carry" path would be free to drift into a partial
// re-render, which reads as an improvement and is actually a correctness loss.
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
	if err := advanceTestWriter(path, now).Advance(context.Background(), window, nil); err != nil {
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

	if err := advanceTestWriter(path, now).Advance(context.Background(), nil, nil); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	after := rawSnapshotMembers(t, path)
	for _, member := range []string{"by_hash", "by_key", "by_pair", "seen", "titles", "harvest_cursor", "nyaa_feed"} {
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
		item:      item{Title: "Misfiled AB S01", GUID: "ab:1000", PubDate: now.Add(-30 * time.Minute)},
	})
	writeSnapshotFile(t, path, fixture)

	window := []seadex.Entry{nyaaEntry(8, 77, true, "New Show - S01E01 (1080p) [G].mkv")}
	if err := advanceTestWriter(path, now).Advance(context.Background(), window, nil); err != nil {
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
	if err := advanceTestWriter(path, now).Advance(context.Background(), window, nil); err != nil {
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

// feedKeys lists a feed's journal keys in persisted order.
func feedKeys(feed []journalItem) []string {
	keys := make([]string, 0, len(feed))
	for i := range feed {
		keys = append(keys, feed[i].Key)
	}
	return keys
}

// TestFilterWindowByTagsDropsEmptiedEntries pins the unit under
// TestAdvanceHonoursExcludeTags: an entry whose every torrent is excluded
// leaves the window entirely (rather than surviving with an empty torrent
// list, which would make the entry count lie), while a partially-excluded
// entry survives carrying only its kept torrents.
func TestFilterWindowByTagsDropsEmptiedEntries(t *testing.T) {
	t.Parallel()
	tagged := func(alID, viewID int, tags ...string) seadex.Entry {
		e := nyaaEntry(alID, viewID, true, "Show - S01E01 (1080p) [G].mkv")
		e.Torrents[0].Tags = tags
		return e
	}
	// One entry with two torrents, only one of them excluded.
	mixed := tagged(3, 30, "broken")
	mixed.Torrents = append(mixed.Torrents, seadex.Torrent{
		Tracker: "Nyaa", URL: "https://nyaa.si/view/31", IsBest: true,
		Files: []seadex.File{{Name: "Show - S01E02 (1080p) [G].mkv", Length: 1}},
	})

	window := []seadex.Entry{
		tagged(1, 10),           // untagged, kept whole
		tagged(2, 20, "broken"), // wholly excluded, dropped
		mixed,                   // partially excluded, kept narrowed
	}
	got := filterWindowByTags(window, feedExcludesWarnings())

	wantIDs := []int{1, 3}
	if len(got) != len(wantIDs) {
		t.Fatalf("filterWindowByTags returned %d entries, want %d (%v)", len(got), len(wantIDs), entryIDsOf(got))
	}
	for i, want := range wantIDs {
		if got[i].AniListID != want {
			t.Errorf("entry %d AniList ID = %d, want %d", i, got[i].AniListID, want)
		}
	}
	if n := len(got[1].Torrents); n != 1 {
		t.Errorf("partially-excluded entry kept %d torrents, want 1", n)
	}
	if len(got[1].Torrents) == 1 && !strings.HasSuffix(got[1].Torrents[0].URL, "/31") {
		t.Errorf("kept torrent URL = %q, want the unexcluded /31", got[1].Torrents[0].URL)
	}
	// The caller's slice must be untouched: Advance uses window's own length in
	// its log line and the tick reports on it too.
	if n := len(window[2].Torrents); n != 2 {
		t.Errorf("filterWindowByTags mutated the caller's entry (torrents = %d, want 2)", n)
	}
}

// entryIDsOf renders a window's AniList IDs for a failure message.
func entryIDsOf(entries []seadex.Entry) string {
	ids := make([]string, 0, len(entries))
	for i := range entries {
		ids = append(ids, strconv.Itoa(entries[i].AniListID))
	}
	return "[" + strings.Join(ids, " ") + "]"
}
