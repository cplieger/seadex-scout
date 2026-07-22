package indexer

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/slogx/capture"
)

// TestRebuildPersistsPairRelation pins that the persisted curation set
// carries the hash/key pair relation lookup's cross-torrent gate reads: a
// torrent with both identity signals records its exact pair, and the map is
// persisted non-nil (even when empty) so a freshly written snapshot never
// falls back to the weaker legacy per-signal gate.
func TestRebuildPersistsPairRelation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	entries := []seadex.Entry{{
		AniListID: 9,
		Torrents: []seadex.Torrent{{
			Tracker: "Nyaa", URL: "https://nyaa.si/view/42",
			InfoHash: "abcdef1234567890abcdef1234567890abcdef12", IsBest: true,
			Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
		}},
	}}
	if err := newTestWriter(path, "", false).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if snap.ByPair == nil {
		t.Fatal("by_pair missing from the persisted snapshot (readers would fall back to the legacy per-signal gate)")
	}
	if !snap.ByPair[pairKey("abcdef1234567890abcdef1234567890abcdef12", "nyaa:42")] {
		t.Errorf("by_pair missing the same-torrent hash/key pair: %v", snap.ByPair)
	}
}

// TestRebuildWarnsWhenABPasskeyMissing pins the operator nudge: a rebuild
// journaling AnimeBytes releases with no configured passkey still writes the
// snapshot (Nyaa unaffected) and logs ONE warning carrying the skip count, so
// the operator learns why the AB RSS feed has nothing grabbable. The logger is
// injected via NewFeedWriter, so no slog.Default swap is needed.
func TestRebuildWarnsWhenABPasskeyMissing(t *testing.T) {
	log, rec := capture.New()
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	entries := []seadex.Entry{{
		AniListID: 9,
		Torrents: []seadex.Torrent{
			{
				Tracker: "AB", URL: "/torrents.php?id=1&torrentid=123", IsBest: true,
				Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
			},
			{
				Tracker: "Nyaa", URL: "https://nyaa.si/view/42", IsBest: true,
				Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
			},
		},
	}}
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api", ABTorznabURL: "http://prowlarr/2/api"}}, Deps{Logger: log})
	if err := w.Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if !rec.Contains("ab RSS feed empty of grabbable links") {
		t.Errorf("missing passkey warning not logged; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
	skipped := int64(-1)
	for _, r := range rec.Records() {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "ab_releases_skipped" {
				skipped = a.Value.Int64()
			}
			return true
		})
	}
	if skipped != 1 {
		t.Errorf("warning does not carry ab_releases_skipped=1 (got %d); log output:\n%s", skipped, strings.Join(rec.Messages(), "\n"))
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 1 {
		t.Errorf("nyaa feed = %d items, want 1 (Nyaa unaffected by the AB skip)", len(snap.NyaaFeed))
	}
}

// TestRebuildNoPasskeyWarnWithoutABIntent pins the WARN gate: a deployment with
// no AB Torznab URL (a Nyaa-only operator) skips the missing-passkey nudge even
// though newly curated AB releases were skipped, so the per-cycle log does not
// nag about a tracker the operator opted out of.
func TestRebuildNoPasskeyWarnWithoutABIntent(t *testing.T) {
	log, rec := capture.New()
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	entries := []seadex.Entry{{
		AniListID: 9,
		Torrents: []seadex.Torrent{{
			Tracker: "AB", URL: "/torrents.php?id=1&torrentid=123", IsBest: true,
			Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
		}},
	}}
	if err := NewFeedWriter(&FeedWriterConfig{Path: path}, Deps{Logger: log}).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if rec.Contains("ab RSS feed empty of grabbable links") {
		t.Errorf("passkey warning logged without AB intent; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestRebuildUnconfiguredABPersistsNoABFeed pins the write side of the
// README's per-tracker off switch: with AnimeBytes unconfigured (an empty
// ab_torznab_url) but a passkey still set, a rebuild must persist NO
// AnimeBytes feed - the passkey must not land on disk in synthesized download
// links for a tracker the operator turned off - while the curation set and the
// Nyaa feed are unaffected. The construction-time WARN names the mismatched
// fields so the half-configured intent surfaces.
func TestRebuildUnconfiguredABPersistsNoABFeed(t *testing.T) {
	log, rec := capture.New()
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	entries := []seadex.Entry{{
		AniListID: 9,
		Torrents: []seadex.Torrent{
			{
				Tracker: "AB", URL: "/torrents.php?id=1&torrentid=123", IsBest: true,
				Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
			},
			{
				Tracker: "Nyaa", URL: "https://nyaa.si/view/42", IsBest: true,
				Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
			},
		},
	}}
	if err := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api", ABPasskey: "SECRETPASSKEY"}}, Deps{Logger: log}).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if bytes.Contains(data, []byte("SECRETPASSKEY")) {
		t.Error("snapshot persists the passkey for an unconfigured AB tracker")
	}
	snap := readSnapshotFile(t, path)
	if len(snap.ABFeed) != 0 {
		t.Errorf("ab_feed = %d items, want 0 (unconfigured tracker's feed must not be built)", len(snap.ABFeed))
	}
	if len(snap.NyaaFeed) != 1 {
		t.Errorf("nyaa_feed = %d items, want 1 (the configured tracker is unaffected)", len(snap.NyaaFeed))
	}
	if len(snap.ByKey) == 0 {
		t.Error("curation set empty: the search index must still cover AB releases (search rides Prowlarr, no passkey)")
	}
	if !snap.Seen["ab:123"] {
		t.Errorf("seen ledger missing the skipped AB identity (it must not journal later as new): %v", snap.Seen)
	}
	if !rec.Contains("indexer.ab_passkey is set but indexer.ab_torznab_url is empty") {
		t.Errorf("half-configured AB intent not warned at construction; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestRebuildPersistsABItemsGUIDOnly pins the at-rest credential contract: a
// rebuild with a CONFIGURED AnimeBytes passkey journals AB releases yet
// persists them GUID-only - the raw feed.json bytes contain ZERO occurrences
// of the passkey and BOTH stored items have an empty download URL (the
// snapshot is never authoritative for fetch targets; the reader re-derives
// Nyaa links too, see rebuildNyaaDownloadURLs) - while a server loading that
// snapshot with the same passkey still serves the AB item with its correct
// derived download link (rebuildABDownloadURLs), so keeping the credential
// off disk costs the served feed nothing.
func TestRebuildPersistsABItemsGUIDOnly(t *testing.T) {
	const passkey = "SUPERSECRETPASSKEY123"
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	entries := []seadex.Entry{{
		AniListID: 154587,
		Torrents: []seadex.Torrent{
			{
				Tracker: "AB", URL: "/torrents.php?id=86576&torrentid=1167293", IsBest: true,
				Files: []seadex.File{{Length: 1, Name: "Frieren - S01E01 (BD Remux 1080p) [PMR].mkv"}},
			},
			{
				Tracker: "Nyaa", URL: "https://nyaa.si/view/1961373", IsBest: true,
				Files: []seadex.File{{Length: 1, Name: "Frieren - S01E01 (BD Remux 1080p) [PMR].mkv"}},
			},
		},
	}}
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api", ABPasskey: passkey, ABTorznabURL: "http://prowlarr/2/api"}}, Deps{})
	if err := w.Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted snapshot: %v", err)
	}
	if n := bytes.Count(data, []byte(passkey)); n != 0 {
		t.Fatalf("persisted feed.json contains the AB passkey %d times, want 0", n)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.ABFeed) != 1 || len(snap.NyaaFeed) != 1 {
		t.Fatalf("feeds: ab=%d nyaa=%d, want 1 and 1", len(snap.ABFeed), len(snap.NyaaFeed))
	}
	if got := snap.ABFeed[0].DownloadURL; got != "" {
		t.Errorf("persisted AB download URL = %q, want empty (GUID-only)", got)
	}
	if got, want := snap.ABFeed[0].GUID, "https://animebytes.tv/torrents.php?id=86576&torrentid=1167293"; got != want {
		t.Errorf("persisted AB GUID = %q, want %q (the reader derives the link from it)", got, want)
	}
	if got := snap.NyaaFeed[0].DownloadURL; got != "" {
		t.Errorf("persisted Nyaa download URL = %q, want empty (GUID-only; the reader re-derives the public link)", got)
	}

	// The reader derives the served AB link from the GUID and its own
	// configured passkey on load, so the feed serves grabbable links even
	// though the snapshot holds none.
	ix := New(&Config{APIKey: "k", UpstreamConfig: UpstreamConfig{ABTorznabURL: "http://prowlarr/2/api", ABPasskey: passkey}}, Deps{}, path)
	served := ix.feedFor(upstreamAB)
	if len(served) != 1 {
		t.Fatalf("served ab feed = %d items, want 1", len(served))
	}
	if want := "https://animebytes.tv/torrent/1167293/download/" + passkey; served[0].DownloadURL != want {
		t.Errorf("served ab download = %q, want %q (derived from GUID + configured passkey)", served[0].DownloadURL, want)
	}
}

// TestRebuildPersistScrubsABScopedItemCarriedInNyaaFeed pins the misplaced-item
// arm of the passkey-at-rest invariant: the secret is attached per item by KEY
// scope - an ab:-keyed item that a legacy or corrupted snapshot placed in
// nyaa_feed is re-rendered by carryJournal with a passkey-bearing AB download
// link and appended to the nyaa slice, where the AB-feed strip never looks.
// The persist-time Nyaa-feed strip (stripDownloadURLs blanks every item's
// download URL) must catch it, so the persisted file can never hold the
// passkey regardless of which feed slice the item rode in on.
func TestRebuildPersistScrubsABScopedItemCarriedInNyaaFeed(t *testing.T) {
	const passkey = "SUPERSECRETPASSKEY123"
	path := filepath.Join(t.TempDir(), "feed.json")
	first := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	writeSnapshotFile(t, path, &snapshot{
		ByHash: map[string]bool{},
		ByKey:  map[string]bool{},
		Seen:   map[string]bool{"ab:1167293": true},
		// The ab:-scoped item sits in the WRONG feed slice (nyaa_feed), the
		// scope/feed mismatch loadPrevious does not validate.
		NyaaFeed: []journalItem{
			{item: item{Title: "Frieren - S01 (BD Remux 1080p) [PMR]", GUID: "https://animebytes.tv/torrents.php?id=86576&torrentid=1167293", PubDate: first}, Key: "ab:1167293", AniListID: 154587, FirstSeen: first},
		},
	})
	entries := []seadex.Entry{{
		AniListID: 154587,
		Torrents: []seadex.Torrent{{
			Tracker: "AB", URL: "/torrents.php?id=86576&torrentid=1167293", IsBest: true,
			Files: []seadex.File{{Length: 1, Name: "Frieren - S01E01 (BD Remux 1080p) [PMR].mkv"}},
		}},
	}}
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api", ABPasskey: passkey, ABTorznabURL: "http://prowlarr/2/api"}}, Deps{})
	if err := w.Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted snapshot: %v", err)
	}
	if n := bytes.Count(data, []byte(passkey)); n != 0 {
		t.Errorf("persisted feed.json contains the AB passkey %d times, want 0 (ab:-scoped item in nyaa_feed must be scrubbed at persist)", n)
	}
}

// TestRebuildReportsWriteError pins the write-failure path: when the snapshot
// cannot be persisted (here the target's parent is a regular file, a root-safe
// ENOTDIR injection - which the previous-snapshot read classifies as absent,
// so the failure surfaces at the write), Rebuild returns a wrapped error
// naming the path rather than logging success.
func TestRebuildReportsWriteError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	path := filepath.Join(blocker, "feed.json")
	err := NewFeedWriter(&FeedWriterConfig{Path: path}, Deps{}).Rebuild(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("Rebuild with an unwritable path returned nil, want error")
	}
	if !strings.Contains(err.Error(), "write feed snapshot") || !strings.Contains(err.Error(), path) {
		t.Errorf("error = %q, want it wrapped as a feed snapshot write failure naming %q", err, path)
	}
}

// TestRebuildFailsOnUnreadablePreviousSnapshot pins the transient-read
// posture: a previous snapshot that stats fine but cannot be read (here a
// directory, a root-safe EISDIR injection) must FAIL the rebuild - never
// re-baseline and blank a live journal over a transient fault - so the
// last-good snapshot stays served and the next cycle retries.
func TestRebuildFailsOnUnreadablePreviousSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir over snapshot path: %v", err)
	}
	err := NewFeedWriter(&FeedWriterConfig{Path: path}, Deps{}).Rebuild(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("Rebuild with an unreadable previous snapshot returned nil, want error")
	}
	if !strings.Contains(err.Error(), "read previous feed snapshot") {
		t.Errorf("error = %q, want it wrapped as a previous-snapshot read failure", err)
	}
}

// TestRebuildDropsOversizedItem pins the shared persisted-item limits at the
// creation choke point (h-f10): a torrent whose synthesized field blows
// maxPersistedFieldBytes (here a file-less torrent whose feed title falls
// back to an oversized release group) is dropped as unresolvable instead of
// being persisted - one such value could otherwise pass the whole-snapshot
// size bound and OOM the reader's XML render - while its identity is still
// recorded in the seen ledger so it can never re-enter the journal as new.
func TestRebuildDropsOversizedItem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	entries := []seadex.Entry{{
		AniListID: 9,
		Torrents: []seadex.Torrent{{
			Tracker: "Nyaa", URL: "https://nyaa.si/view/42", IsBest: true,
			ReleaseGroup: strings.Repeat("a", maxPersistedFieldBytes+1),
		}},
	}}
	if err := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{}).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 0 {
		t.Errorf("nyaa_feed has %d items, want the oversized item dropped as unresolvable", len(snap.NyaaFeed))
	}
	if !snap.Seen["nyaa:42"] {
		t.Error("seen ledger missing the dropped torrent's identity; it could re-enter the journal as new")
	}
}

// TestRebuildBaselinesOversizedCachedTitle pins the titles-cache ingress of
// the shared persisted-item limits (h-f10): a previous snapshot whose feed
// items are all bounded but whose harvested-title cache carries an over-limit
// value must warn and re-baseline as malformed - applyTitles overwrites a
// carried item's title AFTER renderJournalItem's creation-time check, so
// accepting the cache would let one rebuild persist a snapshot the server's
// reload rejects and degrade the feed for a full rebuild interval.
func TestRebuildBaselinesOversizedCachedTitle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	t0 := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	prev := snapshot{
		ByHash: map[string]bool{}, ByKey: map[string]bool{"nyaa:42": true},
		ByPair: map[string]bool{},
		Seen:   map[string]bool{"nyaa:42": true},
		Titles: map[string]string{"nyaa:42": strings.Repeat("a", maxPersistedFieldBytes+1)},
		NyaaFeed: []journalItem{
			{item: item{PubDate: t0, Title: "Show - S01 (1080p) [G]", GUID: "https://nyaa.si/view/42"}, FirstSeen: t0, Key: "nyaa:42"},
		},
	}
	writeSnapshotFile(t, path, &prev)
	log, rec := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{Logger: log})
	w.now = func() time.Time { return t0.Add(time.Hour) }
	entries := []seadex.Entry{nyaaEntry(7, 42, true, "Show - S01E01 (1080p) [G].mkv")}
	if err := w.Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 0 {
		t.Errorf("feed = %d items, want 0 (an oversized cached title must re-baseline, not be applied to a carried item)", len(snap.NyaaFeed))
	}
	if len(snap.Titles) != 0 {
		t.Errorf("titles after re-baseline = %v, want empty", snap.Titles)
	}
	if !snap.Seen["nyaa:42"] {
		t.Errorf("seen ledger missing the curated identity after re-baseline: %v", snap.Seen)
	}
	if !rec.Contains("previous feed snapshot malformed; re-baselining the feed journal") {
		t.Errorf("oversized cached title not warned; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestPersistRejectsOversizedSnapshot pins the write-side size bound: a
// snapshot that marshals past maxFeedBytes (which Indexer.reload would refuse)
// is rejected BEFORE the atomic write, returning a size error naming actual and
// maximum bytes, and the previous last-good snapshot stays in place readable.
// Exercised on persist directly: since renderJournalItem drops over-limit items
// at creation (TestRebuildDropsOversizedItem), no single item can inflate a
// rebuilt snapshot past the bound anymore - the bound now guards aggregate
// growth (e.g. an enormous seen ledger or title cache).
func TestPersistRejectsOversizedSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	previous := []byte(`{"by_hash":{},"by_key":{},"seen":{},"nyaa_feed":[],"ab_feed":[]}`)
	if err := os.WriteFile(path, previous, 0o600); err != nil {
		t.Fatalf("seed previous snapshot: %v", err)
	}
	snap := &snapshot{
		ByHash: map[string]bool{}, ByKey: map[string]bool{}, Seen: map[string]bool{},
		Titles: map[string]string{"nyaa:42": strings.Repeat("a", maxFeedBytes+1)},
	}
	err := NewFeedWriter(&FeedWriterConfig{Path: path}, Deps{}).persist(context.Background(), snap)
	if err == nil {
		t.Fatal("persist with an oversized snapshot returned nil, want size error")
	}
	if !strings.Contains(err.Error(), "exceeds max") {
		t.Errorf("error = %q, want a size-cap error naming the max", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("previous snapshot unreadable after rejection: %v", readErr)
	}
	if !bytes.Equal(got, previous) {
		t.Error("previous snapshot replaced despite size rejection")
	}
}

// TestRebuildExcludesCurationWarnedTorrents pins the feed-side curation gate:
// a torrent SeaDex tags Broken/Incomplete is excluded from the search
// curation set (a Prowlarr result matching it is purged as uncurated), never
// journaled onto RSS, and deliberately NOT recorded in the seen ledger - so a
// later rebuild with the warning lifted journals it as newly grabbable
// curation - while an unwarned sibling flows through untouched and the
// snapshot log line counts the exclusion.
func TestRebuildExcludesCurationWarnedTorrents(t *testing.T) {
	log, rec := capture.New()
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	warnedTorrent := seadex.Torrent{
		Tracker: "Nyaa", URL: "https://nyaa.si/view/41", IsBest: true,
		InfoHash: strings.Repeat("a", 40),
		Tags:     []string{"dual", "Broken"},
		Files:    []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [W].mkv"}},
	}
	entries := []seadex.Entry{{
		AniListID: 7,
		Torrents: []seadex.Torrent{
			warnedTorrent,
			{
				Tracker: "Nyaa", URL: "https://nyaa.si/view/42", IsBest: true,
				Files: []seadex.File{{Length: 1, Name: "Show - S01E02 (1080p) [G].mkv"}},
			},
		},
	}}
	if err := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{Logger: log}).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if _, ok := snap.ByKey["nyaa:41"]; ok {
		t.Error("curation set contains the warned torrent's key (searches would serve it)")
	}
	if _, ok := snap.ByHash[warnedTorrent.InfoHash]; ok {
		t.Error("curation set contains the warned torrent's info hash (searches would serve it)")
	}
	if _, ok := snap.ByKey["nyaa:42"]; !ok {
		t.Error("curation set lost the unwarned sibling")
	}
	if len(snap.NyaaFeed) != 1 || snap.NyaaFeed[0].Key != "nyaa:42" {
		t.Errorf("nyaa feed = %+v, want only the unwarned nyaa:42", snap.NyaaFeed)
	}
	if snap.Seen["nyaa:41"] || snap.Seen[warnedTorrent.InfoHash] {
		t.Errorf("seen ledger recorded the warned torrent (un-warning could never journal it): %v", snap.Seen)
	}
	warnedExcluded := int64(-1)
	for _, r := range rec.Records() {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "warned_excluded" {
				warnedExcluded = a.Value.Int64()
			}
			return true
		})
	}
	if warnedExcluded != 1 {
		t.Errorf("snapshot log line warned_excluded = %d, want 1; log output:\n%s", warnedExcluded, strings.Join(rec.Messages(), "\n"))
	}

	// The warning is lifted: the torrent was never folded into the seen
	// ledger, so it now journals as NEW - the moment it first became
	// grabbable curation is when the arrs should see it on RSS.
	entries[0].Torrents[0].Tags = []string{"dual"}
	if err := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{}).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("second Rebuild: %v", err)
	}
	snap = readSnapshotFile(t, path)
	if _, ok := snap.ByKey["nyaa:41"]; !ok {
		t.Error("curation set missing the un-warned torrent after the warning lifted")
	}
	keys := make([]string, 0, len(snap.NyaaFeed))
	for i := range snap.NyaaFeed {
		keys = append(keys, snap.NyaaFeed[i].Key)
	}
	if !slices.Contains(keys, "nyaa:41") {
		t.Errorf("nyaa feed after un-warning = %v, want it to journal nyaa:41 as new", keys)
	}
}

// TestRebuildWarnedTorrentIdentityWinsAcrossEntries pins the identity-level
// warning policy: a torrent attached to several SeaDex entries where only ONE
// occurrence carries the Broken/Incomplete tag is excluded everywhere - the
// search curation set (proxied searches would otherwise serve and mark the
// unwarned duplicate) and the RSS journal alike (carryJournal consumes the
// any-occurrence key set) - so the two indexer paths can never disagree about
// whether the release is grabbable. The unwarned duplicate deliberately
// carries a DIFFERENT journal key and shares only the info hash, so the test
// fails if the warned-identity collector regresses to key-only matching. The
// duplicate is also seeded as a PREVIOUSLY JOURNALED item, so the test fails
// if the carry-drop key set regresses to direct-warning keys only (the
// carried nyaa:99 would then keep serving warned bytes on RSS while search
// suppresses them).
func TestRebuildWarnedTorrentIdentityWinsAcrossEntries(t *testing.T) {
	log, rec := capture.New()
	path := filepath.Join(t.TempDir(), "feed.json")
	const warnedURL = "https://nyaa.si/view/41"
	const duplicateURL = "https://nyaa.si/view/99"
	hash := strings.Repeat("a", 40)
	// The duplicate was journaled BEFORE its sibling occurrence was warned:
	// its carried item must be retracted through the carry-drop key set even
	// though its own occurrence never carries the Broken tag.
	writeSnapshotFile(t, path, &snapshot{
		ByHash: map[string]bool{},
		ByKey:  map[string]bool{"nyaa:99": true},
		Seen:   map[string]bool{},
		NyaaFeed: []journalItem{
			{item: item{Title: "Show - S01 (1080p) [W]", GUID: duplicateURL, DownloadURL: "https://nyaa.si/download/99.torrent", PubDate: time.Now().UTC()}, Key: "nyaa:99", AniListID: 8, FirstSeen: time.Now().UTC()},
		},
	})
	entries := []seadex.Entry{
		{
			AniListID: 7,
			Torrents: []seadex.Torrent{{
				Tracker: "Nyaa", URL: warnedURL, IsBest: true, InfoHash: hash,
				Tags:  []string{"dual", "Broken"},
				Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [W].mkv"}},
			}},
		},
		{
			AniListID: 8,
			Torrents: []seadex.Torrent{{
				Tracker: "Nyaa", URL: duplicateURL, IsBest: true, InfoHash: hash,
				Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [W].mkv"}},
			}},
		},
	}
	if err := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{Logger: log}).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if _, ok := snap.ByKey["nyaa:41"]; ok {
		t.Error("curation set marks the warned identity via its unwarned duplicate (searches would serve it)")
	}
	if _, ok := snap.ByKey["nyaa:99"]; ok {
		t.Error("curation set marks the warned bytes through a different-key duplicate")
	}
	if _, ok := snap.ByHash[hash]; ok {
		t.Error("curation set marks the warned identity's info hash via its unwarned duplicate")
	}
	if len(snap.NyaaFeed) != 0 {
		t.Errorf("nyaa feed = %+v, want empty (a warned identity must not journal, and the carried duplicate must be retracted)", snap.NyaaFeed)
	}
	if snap.Seen["nyaa:41"] || snap.Seen["nyaa:99"] || snap.Seen[hash] {
		t.Errorf("seen ledger recorded the warned identity (un-warning could never journal it): %v", snap.Seen)
	}
	warnedDropped := int64(-1)
	for _, r := range rec.Records() {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "journal_warned_dropped" {
				warnedDropped = a.Value.Int64()
			}
			return true
		})
	}
	if warnedDropped != 1 {
		t.Errorf("snapshot log line journal_warned_dropped = %d, want 1 (the carried duplicate); log output:\n%s", warnedDropped, strings.Join(rec.Messages(), "\n"))
	}
}

// TestRebuildDropsCarriedJournalItemBecomingWarned pins the carry-side gate:
// a previously journaled item whose torrent has SINCE been tagged
// Broken/Incomplete is dropped from the journal - unlike a
// curated-then-replaced torrent, which keeps its stored render - so the arrs
// cannot grab a release the curators now warn against, and the drop is
// counted on the snapshot log line.
func TestRebuildDropsCarriedJournalItemBecomingWarned(t *testing.T) {
	log, rec := capture.New()
	path := filepath.Join(t.TempDir(), "feed.json")
	writeSnapshotFile(t, path, &snapshot{
		ByHash: map[string]bool{},
		ByKey:  map[string]bool{"nyaa:42": true},
		Seen:   map[string]bool{"nyaa:42": true},
		NyaaFeed: []journalItem{
			{item: item{Title: "Show - S01 (1080p) [G]", GUID: "https://nyaa.si/view/42", DownloadURL: "https://nyaa.si/download/42.torrent", PubDate: time.Now().UTC()}, Key: "nyaa:42", AniListID: 7, FirstSeen: time.Now().UTC()},
		},
	})
	entries := []seadex.Entry{{
		AniListID: 7,
		Torrents: []seadex.Torrent{{
			Tracker: "Nyaa", URL: "https://nyaa.si/view/42", IsBest: true,
			Tags:  []string{"Incomplete"},
			Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
		}},
	}}
	if err := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{Logger: log}).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 0 {
		t.Errorf("nyaa feed = %+v, want empty (the carried item's torrent is now warned)", snap.NyaaFeed)
	}
	if _, ok := snap.ByKey["nyaa:42"]; ok {
		t.Error("curation set still marks the now-warned torrent")
	}
	warnedDropped := int64(-1)
	for _, r := range rec.Records() {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "journal_warned_dropped" {
				warnedDropped = a.Value.Int64()
			}
			return true
		})
	}
	if warnedDropped != 1 {
		t.Errorf("snapshot log line journal_warned_dropped = %d, want 1; log output:\n%s", warnedDropped, strings.Join(rec.Messages(), "\n"))
	}
}

// TestRebuildBaselinesSnapshotMissingCurationMaps pins loadPrevious's
// structural-validity gate: a previous snapshot that decodes cleanly and even
// carries a seen ledger, but is missing the required by_hash/by_key curation
// maps (a hand-edited or corrupted file - the writer always persists both,
// even empty), must warn and re-baseline rather than trust the ledger: the
// seen set rebuilds from the current catalogue and the journal starts empty.
func TestRebuildBaselinesSnapshotMissingCurationMaps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	if err := os.WriteFile(path, []byte(`{"seen":{"nyaa:41":true},"nyaa_feed":[],"ab_feed":[]}`), 0o600); err != nil {
		t.Fatalf("seed mapless snapshot: %v", err)
	}
	log, rec := capture.New()
	entries := []seadex.Entry{nyaaEntry(7, 42, true, "Show - S01E01 (1080p) [G].mkv")}
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{Logger: log})
	if err := w.Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 0 {
		t.Errorf("feed = %d items, want 0 (a mapless snapshot must re-baseline, not journal against its stale seen ledger)", len(snap.NyaaFeed))
	}
	if !snap.Seen["nyaa:42"] {
		t.Errorf("seen ledger missing the current catalogue after re-baseline: %v", snap.Seen)
	}
	if !rec.Contains("previous feed snapshot malformed; re-baselining the feed journal") {
		t.Errorf("mapless snapshot not warned as malformed; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestRebuildBaselinesOversizedFeedItem pins the feed-items ingress of the
// shared persisted-item limits - the journal twin of
// TestRebuildBaselinesOversizedCachedTitle: a previous snapshot whose maps and
// titles are bounded but whose persisted journal carries an item past
// maxPersistedFieldBytes must warn and re-baseline as malformed, never carry
// the oversized item forward (the server's readSnapshot rejects the same
// bytes, so trusting them would wedge reader and writer on a poisoned file).
func TestRebuildBaselinesOversizedFeedItem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	t0 := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	writeSnapshotFile(t, path, &snapshot{
		ByHash: map[string]bool{}, ByKey: map[string]bool{"nyaa:42": true},
		ByPair: map[string]bool{},
		Seen:   map[string]bool{"nyaa:42": true},
		NyaaFeed: []journalItem{
			{item: item{PubDate: t0, Title: strings.Repeat("a", maxPersistedFieldBytes+1), GUID: "https://nyaa.si/view/42"}, FirstSeen: t0, Key: "nyaa:42"},
		},
	})
	log, rec := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{Logger: log})
	w.now = func() time.Time { return t0.Add(time.Hour) }
	entries := []seadex.Entry{nyaaEntry(7, 42, true, "Show - S01E01 (1080p) [G].mkv")}
	if err := w.Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 0 {
		t.Errorf("feed = %d items, want 0 (an over-limit journal item must re-baseline, not be carried or re-rendered)", len(snap.NyaaFeed))
	}
	if !snap.Seen["nyaa:42"] {
		t.Errorf("seen ledger missing the curated identity after re-baseline: %v", snap.Seen)
	}
	if !rec.Contains("previous feed snapshot malformed; re-baselining the feed journal") {
		t.Errorf("over-limit journal item not warned; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestRebuildOversizedSnapshotRebaselines pins the deterministic-failure
// classification of an over-cap feed.json: persist enforces the same
// maxFeedBytes cap, so an oversized file can only come from external
// corruption or hand-editing and never shrinks on its own - classifying it
// transient (an error) would wedge every future rebuild on the same file.
// It must re-baseline like malformed JSON, and the rebuild's persist then
// atomically replaces the oversized file (self-healing).
func TestRebuildOversizedSnapshotRebaselines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create oversized snapshot: %v", err)
	}
	// A sparse file over the cap: ReadBounded rejects on size, so no real
	// 64 MiB payload is needed on disk.
	if err := f.Truncate(maxFeedBytes + 1); err != nil {
		t.Fatalf("truncate to over-cap size: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close oversized snapshot: %v", err)
	}
	log, rec := capture.New()
	w := newTestWriter(path, "", false)
	w.log = log
	if err := w.Rebuild(context.Background(), nil, nil); err != nil {
		t.Fatalf("Rebuild over an oversized snapshot: %v (must re-baseline, not error)", err)
	}
	if !rec.Contains("previous feed snapshot exceeds size cap; re-baselining the feed journal") {
		t.Errorf("no oversized-rebaseline warn; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat rewritten snapshot: %v", err)
	}
	if fi.Size() > maxFeedBytes {
		t.Errorf("rewritten snapshot = %d bytes, want under the cap (persist must replace the oversized file)", fi.Size())
	}
	if snap := readSnapshotFile(t, path); snap.Seen == nil {
		t.Error("rewritten snapshot carries no seen ledger, want a baselined journal schema")
	}
}

// TestJournalItemPersistedShapeIsFlat pins the on-disk contract across the
// item/journalItem type split: encoding/json flattens the embedded wire item,
// so a persisted journal record keeps the exact historical FLAT object shape
// - no nested "item" key - and a snapshot written by a pre-split binary
// (flat fields) decodes losslessly into the new shape. The resident daemon
// reads what the poll subcommand writes across binary versions, so this
// shape IS the cross-process contract.
func TestJournalItemPersistedShapeIsFlat(t *testing.T) {
	first := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
	jit := journalItem{
		item: item{
			Title: "Show - S01 (1080p) [G]", GUID: "https://nyaa.si/view/42",
			DownloadURL: "https://nyaa.si/download/42.torrent", PubDate: first,
			Size: 7, Seeders: 1,
		},
		Key: "nyaa:42", AniListID: 9, FirstSeen: first,
	}
	data, err := json.Marshal(&jit)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var flat map[string]json.RawMessage
	if err := json.Unmarshal(data, &flat); err != nil {
		t.Fatalf("unmarshal into map: %v", err)
	}
	if _, nested := flat["item"]; nested {
		t.Fatalf("persisted journal item carries a nested \"item\" object, want the historical flat shape: %s", data)
	}
	for _, key := range []string{"Title", "GUID", "DownloadURL", "PubDate", "Key", "AniListID", "FirstSeen"} {
		if _, ok := flat[key]; !ok {
			t.Errorf("persisted journal item lost flat key %q: %s", key, data)
		}
	}

	legacy := []byte(`{"PubDate":"2026-07-01T00:00:00Z","FirstSeen":"2026-07-01T00:00:00Z","Title":"Show - S01 (1080p) [G]","GUID":"https://nyaa.si/view/42","InfoURL":"","DownloadURL":"https://nyaa.si/download/42.torrent","InfoHash":"","DownloadVolumeFactor":"","Key":"nyaa:42","Categories":null,"Size":7,"AniListID":9,"Seeders":1,"Leechers":0}`)
	var decoded journalItem
	if err := json.Unmarshal(legacy, &decoded); err != nil {
		t.Fatalf("decode pre-split flat snapshot record: %v", err)
	}
	if decoded.Title != jit.Title || decoded.Key != jit.Key || decoded.AniListID != jit.AniListID ||
		!decoded.FirstSeen.Equal(first) || decoded.Size != 7 || decoded.Seeders != 1 {
		t.Errorf("pre-split record decoded lossily: %+v", decoded)
	}
}

// TestValidPersistedItemRejectsNegativeCounts pins the numeric arm of the
// shared persisted-item limits: both producers guarantee non-negative
// size/seeders/leechers (toItem clamps, totalSize floors at 0), so a
// persisted negative value identifies a hand-edited or corrupted snapshot
// and must be rejected at load rather than rendered as an invalid enclosure
// length or peer count.
func TestValidPersistedItemRejectsNegativeCounts(t *testing.T) {
	tests := map[string]journalItem{
		"negative size":     {item: item{Title: "x", Size: -1}},
		"negative seeders":  {item: item{Title: "x", Seeders: -1}},
		"negative leechers": {item: item{Title: "x", Leechers: -1}},
	}
	for name, it := range tests {
		t.Run(name, func(t *testing.T) {
			if validPersistedItem(&it) {
				t.Errorf("validPersistedItem(%s) = true, want false", name)
			}
		})
	}
	ok := journalItem{item: item{Title: "x"}}
	if !validPersistedItem(&ok) {
		t.Error("validPersistedItem(zero counts) = false, want true")
	}
}
