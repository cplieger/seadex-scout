package indexer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cplieger/atomicfile/v3"
	"github.com/cplieger/jsonx/bounded"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/tagfilter"
	"github.com/cplieger/slogx/capture"
)

// Abort vs report in this file: a t.Fatal* that reports a VALUE MISMATCH is a
// t.Errorf, so one run names every wrong value in the cluster instead of stopping
// at the first. It stays a t.Fatal* when (a) a later line indexes or dereferences
// what the check guards, so converting would trade a named failure for a panic;
// (b) the check establishes the object its siblings read, so continuing asserts
// against a known-bad fixture; (c) a sibling would pass VACUOUSLY once it fails;
// (d) the body is a rapid property or a fuzz target, whose harness re-runs it
// while shrinking; or (e) continuing risks a synctest deadlock or a blocked send.

// TestRebuildPersistsPairRelation pins that the persisted curation set
// carries the hash/key pair relation lookup's cross-torrent gate reads: a
// torrent with both identity signals records its exact pair, and the map is
// persisted non-nil (even when empty) so a freshly written snapshot never
// falls back to the weaker legacy per-signal gate.
func TestRebuildPersistsPairRelation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	entries := []seadex.Entry{{
		AniListID: 9,
		Torrents: []seadex.Torrent{{
			Tracker: "Nyaa", URL: "https://nyaa.si/view/42",
			InfoHash: "abcdef1234567890abcdef1234567890abcdef12", IsBest: true,
			Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
		}},
	}}
	if err := newTestWriter(path, "passkey", false).Rebuild(t.Context(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if byPairOf(&snap) == nil {
		t.Fatal("by_pair missing from the persisted snapshot (readers would fall back to the legacy per-signal gate)")
	}
	if !byPairOf(&snap)[pairKey("abcdef1234567890abcdef1234567890abcdef12", "nyaa:42")] {
		t.Errorf("by_pair missing the same-torrent hash/key pair: %v", byPairOf(&snap))
	}
}

// TestRebuildWarnsWhenABPasskeyMissing pins the operator nudge: a rebuild that
// meets AnimeBytes releases with no configured passkey still writes the snapshot
// (Nyaa unaffected) and logs ONE warning carrying the count of AB releases it
// could not turn into a grabbable link, so the operator learns why the AB RSS
// feed is empty. Those releases are neither journaled nor published (see
// TestRebuildDefersNewABItemUntilPasskeyArrives), which is what makes the
// nudge's implied recovery real. The logger is
// injected via NewFeedWriter, so no slog.Default swap is needed.
func TestRebuildWarnsWhenABPasskeyMissing(t *testing.T) {
	log, rec := capture.New()
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
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
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api", ABTorznabURL: "http://prowlarr/2/api"}}, log, nil)
	if err := w.Rebuild(t.Context(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if !rec.Contains("ab RSS feed empty of grabbable links") {
		t.Errorf("missing passkey warning not logged; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
	if v, ok := rec.AttrValue("ab RSS feed empty of grabbable links", "ab_releases_skipped"); !ok || v != "1" {
		t.Errorf("warning does not carry ab_releases_skipped=1 (got %q, found=%v); log output:\n%s", v, ok, strings.Join(rec.Messages(), "\n"))
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
	seedEmptyFeed(t, path)
	entries := []seadex.Entry{{
		AniListID: 9,
		Torrents: []seadex.Torrent{{
			Tracker: "AB", URL: "/torrents.php?id=1&torrentid=123", IsBest: true,
			Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
		}},
	}}
	if err := NewFeedWriter(&FeedWriterConfig{Path: path}, log, nil).Rebuild(t.Context(), entries, nil); err != nil {
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
// Nyaa feed are unaffected. Construction stays SILENT about the half-configured
// intent: internal/config owns that diagnostic (see the comment on the assertion
// below, l-f13).
func TestRebuildUnconfiguredABPersistsNoABFeed(t *testing.T) {
	log, rec := capture.New()
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
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
	if err := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api", ABPasskey: "SECRETPASSKEY"}}, log, nil).Rebuild(t.Context(), entries, nil); err != nil {
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
	if len(byKeyOf(&snap)) == 0 {
		t.Error("curation set empty: the search index must still cover AB releases (search rides Prowlarr, no passkey)")
	}
	if !snap.Published["ab:123"] {
		t.Errorf("publication log missing the skipped AB identity (it must not journal later as new): %v", snap.Published)
	}
	// The half-configured AB intent is internal/config's diagnostic, not the
	// writer's: config validation runs in every mode and deliberately reports it
	// at INFO ("a deliberately parked passkey must not raise Loki alert noise").
	// The writer used to re-evaluate the same condition at WARN, so a configured
	// feed emitted both lines at boot and the WARN re-fired on every `poll` run
	// (l-f13). Construction must stay silent about it.
	if rec.Contains("indexer.ab_passkey is set but indexer.ab_torznab_url is empty") {
		t.Errorf("the writer re-reported config's half-configuration diagnostic; log output:\n%s",
			strings.Join(rec.Messages(), "\n"))
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
	seedEmptyFeed(t, path)
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
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api", ABPasskey: passkey, ABTorznabURL: "http://prowlarr/2/api"}}, nil, nil)
	if err := w.Rebuild(t.Context(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted snapshot: %v", err)
	}
	if n := bytes.Count(data, []byte(passkey)); n != 0 {
		t.Errorf("persisted feed.json contains the AB passkey %d times, want 0", n)
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
	ix := warmedIndexer(&Config{APIKey: "k", SnapshotPath: path, UpstreamConfig: UpstreamConfig{ABTorznabURL: "http://prowlarr/2/api", ABPasskey: passkey}}, nil, nil)
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
// scope, so an ab:-keyed item that a legacy or corrupted snapshot placed in
// nyaa_feed must never reach the persisted file with a passkey-bearing AB
// download link. Two layers hold that: carryItem's scope gate drops the
// cross-scope item at carry admission, and the persist-time Nyaa-feed strip
// (stripDownloadURLs blanks every item's download URL) catches anything that
// still rides in on the wrong slice - so the persisted file can never hold the
// passkey regardless of which feed slice the item came from.
func TestRebuildPersistScrubsABScopedItemCarriedInNyaaFeed(t *testing.T) {
	const passkey = "SUPERSECRETPASSKEY123"
	path := filepath.Join(t.TempDir(), "feed.json")
	first := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	writeSnapshotFile(t, path, &snapshot{
		Owners:    owns(),
		Published: map[string]bool{"ab:1167293": true},
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
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api", ABPasskey: passkey, ABTorznabURL: "http://prowlarr/2/api"}}, nil, nil)
	if err := w.Rebuild(t.Context(), entries, nil); err != nil {
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
	err := NewFeedWriter(&FeedWriterConfig{Path: path}, nil, nil).Rebuild(t.Context(), nil, nil)
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
	err := NewFeedWriter(&FeedWriterConfig{Path: path}, nil, nil).Rebuild(t.Context(), nil, nil)
	if err == nil {
		t.Fatal("Rebuild with an unreadable previous snapshot returned nil, want error")
	}
	if !strings.Contains(err.Error(), "read previous feed snapshot") {
		t.Errorf("error = %q, want it wrapped as a previous-snapshot read failure", err)
	}
}

// TestRebuildDropsOversizedItem pins the shared persisted-item limits at the
// creation choke point (h-f10), and its ledger claim is INVERTED from what it
// used to assert.
//
// A torrent whose synthesized field blows maxPersistedFieldBytes (here a
// file-less torrent whose feed title falls back to an oversized release group) is
// dropped as unresolvable instead of being persisted - one such value could
// otherwise pass the whole-snapshot size bound and OOM the reader's XML render.
//
// Nothing was published, so nothing is recorded. An over-limit field is an
// upstream DATA property, and the publication log is never pruned, so recording
// it would deny the corrected record its RSS exposure forever - the permanent
// omission settled feed-rss-filtering rules out.
func TestRebuildDropsOversizedItem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	entries := []seadex.Entry{{
		AniListID: 9,
		Torrents: []seadex.Torrent{{
			Tracker: "Nyaa", URL: "https://nyaa.si/view/42", IsBest: true,
			ReleaseGroup: strings.Repeat("a", maxPersistedFieldBytes+1),
		}},
	}}
	if err := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, nil, nil).Rebuild(t.Context(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 0 {
		t.Errorf("nyaa_feed has %d items, want the oversized item dropped as unresolvable", len(snap.NyaaFeed))
	}
	if snap.Published["nyaa:42"] {
		t.Errorf("publication log recorded the dropped torrent though nothing was served: %v; "+
			"a corrected upstream record must still be able to journal as new", snap.Published)
	}
}

// TestRebuildDropsOversizedCachedTitle pins the titles-cache ingress of the
// shared persisted-item limits (h-f10, l-f60): a previous snapshot whose feed
// items are all bounded but whose harvested-title cache carries an over-limit
// value must DROP that entry, warn, and keep the journal - the carried item
// stays in the feed under its synthesized title. Accepting the cache would let
// one rebuild persist a snapshot the server's reload prunes (applyTitles
// overwrites a carried item's title AFTER renderJournalItem's creation-time
// check), but re-baselining over it cost the whole journal window: seen is
// rebuilt from the current catalogue, so every release then inside
// feedJournalMaxAge is marked seen without ever being served and can never
// reach RSS again.
func TestRebuildDropsOversizedCachedTitle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	t0 := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	prev := snapshot{
		Owners:    owns(keyed("nyaa:42", true)),
		Published: map[string]bool{"nyaa:42": true},
		Titles:    map[string]string{"nyaa:42": strings.Repeat("a", maxPersistedFieldBytes+1)},
		NyaaFeed: []journalItem{
			{item: item{PubDate: t0, Title: "Show - S01 (1080p) [G]", GUID: "https://nyaa.si/view/42"}, FirstSeen: t0, Key: "nyaa:42"},
		},
	}
	writeSnapshotFile(t, path, &prev)
	log, rec := capture.New()
	w := newLoggedTestWriter(path, log)
	w.now = func() time.Time { return t0.Add(time.Hour) }
	entries := []seadex.Entry{nyaaEntry(7, 42, true, "Show - S01E01 (1080p) [G].mkv")}
	if err := w.Rebuild(t.Context(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 1 {
		t.Fatalf("feed = %d items, want the carried item kept (an oversized cached title costs one re-harvest, not the journal window)", len(snap.NyaaFeed))
	}
	if got := snap.NyaaFeed[0].Title; got != "Show - S01E01 (1080p) [G]" {
		t.Errorf("carried item title = %q, want the synthesized title (the over-limit cached title must not be applied)", got)
	}
	if len(snap.Titles) != 0 {
		t.Errorf("titles after the drop = %v, want empty", snap.Titles)
	}
	if !snap.Published["nyaa:42"] {
		t.Errorf("publication log missing the curated identity: %v", snap.Published)
	}
	if rec.Contains(msgSnapshotMalformed) {
		t.Errorf("oversized cached title re-baselined the journal; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
	if !rec.Contains("previous feed snapshot dropped over-limit cached titles; the harvest re-earns them") {
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
// growth (e.g. an enormous publication log or title cache).
func TestPersistRejectsOversizedSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	previous := []byte(`{"version":2,"owners":{},"published":{},"nyaa_feed":[],"ab_feed":[]}`)
	if err := os.WriteFile(path, previous, 0o600); err != nil {
		t.Fatalf("seed previous snapshot: %v", err)
	}
	snap := &snapshot{
		Owners: owns(), Published: map[string]bool{},
		Titles: map[string]string{"nyaa:42": strings.Repeat("a", maxFeedBytes+1)},
	}
	err := NewFeedWriter(&FeedWriterConfig{Path: path}, nil, nil).persist(t.Context(), snap)
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

// TestRebuildExcludesCurationWarnedTorrents pins the feed-side exclusion gate
// under a CONFIGURED policy (`broken`/`incomplete`: [feed], see
// feedExcludesWarnings): an excluded torrent is dropped from the search
// curation set (a Prowlarr result matching it is purged as uncurated), never
// journaled onto RSS, and deliberately NOT recorded in the publication log - so a
// later rebuild with the tag gone journals it as newly grabbable
// curation - while a kept sibling flows through untouched and the
// snapshot log line counts the exclusion.
//
// The policy argument is what changed with filters.exclude_tags: the exclusion
// used to be hardcoded, so this test needed no configuration. The default is now
// to exclude nothing (TestRebuildKeepsCurationWarnedTorrentsByDefault).
func TestRebuildExcludesCurationWarnedTorrents(t *testing.T) {
	log, rec := capture.New()
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
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
	if err := newLoggedExcludingTestWriter(path, log).Rebuild(t.Context(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if _, ok := byKeyOf(&snap)["nyaa:41"]; ok {
		t.Error("curation set contains the warned torrent's key (searches would serve it)")
	}
	if _, ok := byHashOf(&snap)[warnedTorrent.InfoHash]; ok {
		t.Error("curation set contains the warned torrent's info hash (searches would serve it)")
	}
	if _, ok := byKeyOf(&snap)["nyaa:42"]; !ok {
		t.Error("curation set lost the unwarned sibling")
	}
	if len(snap.NyaaFeed) != 1 || snap.NyaaFeed[0].Key != "nyaa:42" {
		t.Errorf("nyaa feed = %+v, want only the unwarned nyaa:42", snap.NyaaFeed)
	}
	if snap.Published["nyaa:41"] || snap.Published[warnedTorrent.InfoHash] {
		t.Errorf("publication log recorded the warned torrent (un-warning could never journal it): %v", snap.Published)
	}
	if v, ok := rec.AttrValue("indexer feed snapshot written", "warned_excluded"); !ok || v != "1" {
		t.Errorf("snapshot log line warned_excluded = %q (found=%v), want \"1\"; log output:\n%s", v, ok, strings.Join(rec.Messages(), "\n"))
	}

	// The tag is gone upstream: the torrent was never folded into the seen
	// ledger, so it now journals as NEW - the moment it first became
	// grabbable curation is when the arrs should see it on RSS.
	entries[0].Torrents[0].Tags = []string{"dual"}
	if err := newExcludingTestWriter(path).Rebuild(t.Context(), entries, nil); err != nil {
		t.Fatalf("second Rebuild: %v", err)
	}
	snap = readSnapshotFile(t, path)
	if _, ok := byKeyOf(&snap)["nyaa:41"]; !ok {
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

// TestRebuildKeepsCurationWarnedTorrentsByDefault pins the feed surface's
// DEFAULT after filters.exclude_tags: with no exclusions configured (the
// shipped default, and the operator's explicit choice) a torrent SeaDex tags
// Broken IS curated, IS journaled onto RSS, and IS recorded in the publication log,
// so Sonarr/Radarr see it and decide for themselves.
//
// This INVERTS what TestRebuildExcludesCurationWarnedTorrents used to assert
// for an unconfigured writer: the exclusion did not disappear, it moved into
// filters.exclude_tags (that test now configures it). A report-only exclusion is
// asserted here too, so a `broken: [report]` policy provably leaves the feed
// alone - the three surfaces are independent.
func TestRebuildKeepsCurationWarnedTorrentsByDefault(t *testing.T) {
	warnedEntries := func() []seadex.Entry {
		return []seadex.Entry{{
			AniListID: 7,
			Torrents: []seadex.Torrent{{
				Tracker: "Nyaa", URL: "https://nyaa.si/view/41", IsBest: true,
				InfoHash: strings.Repeat("a", 40),
				Tags:     []string{"dual", "Broken"},
				Files:    []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [W].mkv"}},
			}},
		}}
	}
	assertServed := func(t *testing.T, w *FeedWriter, path string) {
		t.Helper()
		if err := w.Rebuild(t.Context(), warnedEntries(), nil); err != nil {
			t.Fatalf("Rebuild: %v", err)
		}
		snap := readSnapshotFile(t, path)
		if _, ok := byKeyOf(&snap)["nyaa:41"]; !ok {
			t.Error("curation set is missing the Broken torrent; searches must serve it when nothing filters the feed")
		}
		if _, ok := byHashOf(&snap)[strings.Repeat("a", 40)]; !ok {
			t.Error("curation set is missing the Broken torrent's info hash")
		}
		if len(snap.NyaaFeed) != 1 || snap.NyaaFeed[0].Key != "nyaa:41" {
			t.Errorf("nyaa feed = %+v, want the Broken torrent journaled onto RSS", snap.NyaaFeed)
		}
		if !snap.Published["nyaa:41"] {
			t.Errorf("publication log = %v, want the served torrent recorded", snap.Published)
		}
	}

	t.Run("no exclude_tags configured", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "feed.json")
		seedEmptyFeed(t, path)
		assertServed(t, newTestWriter(path, "", false), path)
	})

	t.Run("a report-only exclusion leaves the feed alone", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "feed.json")
		seedEmptyFeed(t, path)
		w := NewFeedWriter(&FeedWriterConfig{
			Path: path,
			TagFilter: tagfilter.New(map[string][]tagfilter.Surface{
				"broken": {tagfilter.SurfaceReport},
			}),
			UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"},
		}, nil, nil)
		assertServed(t, w, path)
	})
}

// TestRebuildWarnedTorrentIdentityWinsAcrossEntries pins the identity-level
// exclusion scope under a CONFIGURED policy (feedExcludesWarnings): a torrent
// attached to several SeaDex entries where only ONE
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
		Owners:    owns(keyed("nyaa:99", true)),
		Published: map[string]bool{},
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
	if err := newLoggedExcludingTestWriter(path, log).Rebuild(t.Context(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if _, ok := byKeyOf(&snap)["nyaa:41"]; ok {
		t.Error("curation set marks the warned identity via its unwarned duplicate (searches would serve it)")
	}
	if _, ok := byKeyOf(&snap)["nyaa:99"]; ok {
		t.Error("curation set marks the warned bytes through a different-key duplicate")
	}
	if _, ok := byHashOf(&snap)[hash]; ok {
		t.Error("curation set marks the warned identity's info hash via its unwarned duplicate")
	}
	if len(snap.NyaaFeed) != 0 {
		t.Errorf("nyaa feed = %+v, want empty (a warned identity must not journal, and the carried duplicate must be retracted)", snap.NyaaFeed)
	}
	if snap.Published["nyaa:41"] || snap.Published["nyaa:99"] || snap.Published[hash] {
		t.Errorf("publication log recorded the warned identity (un-warning could never journal it): %v", snap.Published)
	}
	if v, ok := rec.AttrValue("indexer feed snapshot written", "journal_warned_dropped"); !ok || v != "1" {
		t.Errorf("snapshot log line journal_warned_dropped = %q (found=%v), want \"1\" (the carried duplicate); log output:\n%s", v, ok, strings.Join(rec.Messages(), "\n"))
	}
}

// TestRebuildDropsCarriedJournalItemBecomingWarned pins the carry-side gate
// under a CONFIGURED policy (feedExcludesWarnings):
// a previously journaled item whose torrent has SINCE been tagged
// Broken/Incomplete is dropped from the journal - unlike a
// curated-then-replaced torrent, which keeps its stored render - so the arrs
// cannot grab a release the curators now warn against, and the drop is
// counted on the snapshot log line.
func TestRebuildDropsCarriedJournalItemBecomingWarned(t *testing.T) {
	log, rec := capture.New()
	path := filepath.Join(t.TempDir(), "feed.json")
	writeSnapshotFile(t, path, &snapshot{
		Owners:    owns(keyed("nyaa:42", true)),
		Published: map[string]bool{"nyaa:42": true},
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
	if err := newLoggedExcludingTestWriter(path, log).Rebuild(t.Context(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 0 {
		t.Errorf("nyaa feed = %+v, want empty (the carried item's torrent is now warned)", snap.NyaaFeed)
	}
	if _, ok := byKeyOf(&snap)["nyaa:42"]; ok {
		t.Error("curation set still marks the now-warned torrent")
	}
	if v, ok := rec.AttrValue("indexer feed snapshot written", "journal_warned_dropped"); !ok || v != "1" {
		t.Errorf("snapshot log line journal_warned_dropped = %q (found=%v), want \"1\"; log output:\n%s", v, ok, strings.Join(rec.Messages(), "\n"))
	}
}

// TestRebuildBaselinesSnapshotMissingAFact pins loadPrevious's
// structural-validity gate on the new contract: a previous snapshot that decodes
// cleanly at the CURRENT version and even carries a publication log, but is
// missing the curation ownership fact (a hand-edited or corrupted file - the
// writer always persists both, even empty), must warn and re-baseline rather than
// trust the log. The two facts ARE the contract, so one without the other is not
// a partial snapshot to salvage.
func TestRebuildBaselinesSnapshotMissingAFact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	if err := os.WriteFile(path, []byte(`{"version":2,"published":{"nyaa:41":true},"nyaa_feed":[],"ab_feed":[]}`), 0o600); err != nil {
		t.Fatalf("seed factless snapshot: %v", err)
	}
	log, rec := capture.New()
	entries := []seadex.Entry{nyaaEntry(7, 42, true, "Show - S01E01 (1080p) [G].mkv")}
	w := newLoggedTestWriter(path, log)
	if err := w.Rebuild(t.Context(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 0 {
		t.Errorf("feed = %d items, want 0 (a factless snapshot must re-baseline, not journal against its stale publication log)", len(snap.NyaaFeed))
	}
	if !snap.Published["nyaa:42"] {
		t.Errorf("publication log missing the forfeited catalogue after re-baseline: %v", snap.Published)
	}
	if !rec.Contains("previous feed snapshot malformed; re-baselining the feed journal") {
		t.Errorf("factless snapshot not warned as malformed; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestRebuildDropsOversizedFeedItem pins the feed-items ingress of the shared
// persisted-item limits - the journal twin of
// TestRebuildDropsOversizedCachedTitle: a previous snapshot whose maps and
// titles are bounded but whose persisted journal carries an item past
// maxPersistedFieldBytes must drop THAT item and keep the rest of the journal.
// The over-limit item is never carried or re-rendered (the server's readSnapshot
// prunes the same bytes, so trusting them would wedge reader and writer on a
// poisoned file), while its bounded sibling survives - one corrupted item out of
// thousands must not cost the whole journal window (l-f45).
func TestRebuildDropsOversizedFeedItem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	t0 := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	writeSnapshotFile(t, path, &snapshot{
		Owners:    owns(keyed("nyaa:42", true), keyed("nyaa:43", true)),
		Published: map[string]bool{"nyaa:42": true, "nyaa:43": true},
		NyaaFeed: []journalItem{
			{item: item{PubDate: t0, Title: strings.Repeat("a", maxPersistedFieldBytes+1), GUID: "https://nyaa.si/view/42"}, FirstSeen: t0, Key: "nyaa:42"},
			{item: item{PubDate: t0, Title: "Sibling - S01 (1080p) [G]", GUID: "https://nyaa.si/view/43"}, FirstSeen: t0, Key: "nyaa:43"},
		},
	})
	log, rec := capture.New()
	w := newLoggedTestWriter(path, log)
	w.now = func() time.Time { return t0.Add(time.Hour) }
	entries := []seadex.Entry{nyaaEntry(7, 42, true, "Show - S01E01 (1080p) [G].mkv")}
	if err := w.Rebuild(t.Context(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	keys := make([]string, 0, len(snap.NyaaFeed))
	for _, it := range snap.NyaaFeed {
		keys = append(keys, it.Key)
	}
	if !slices.Equal(keys, []string{"nyaa:43"}) {
		t.Errorf("feed keys = %v, want only the bounded sibling nyaa:43 (the over-limit item is dropped, the journal is kept)", keys)
	}
	if !snap.Published["nyaa:42"] || !snap.Published["nyaa:43"] {
		t.Errorf("publication log lost a carried identity: %v", snap.Published)
	}
	if rec.Contains(msgSnapshotMalformed) {
		t.Errorf("an over-limit journal item re-baselined the whole journal; log output:\n%s", strings.Join(rec.Messages(), "\n"))
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
	// multi-MiB payload is needed on disk.
	if err := f.Truncate(maxFeedBytes + 1); err != nil {
		t.Fatalf("truncate to over-cap size: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close oversized snapshot: %v", err)
	}
	log, rec := capture.New()
	w := newTestWriter(path, "", false)
	w.log = log
	if err := w.Rebuild(t.Context(), nil, nil); err != nil {
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
	if snap := readSnapshotFile(t, path); snap.Published == nil {
		t.Error("rewritten snapshot carries no publication log, want a baselined journal schema")
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

// TestValidPersistedItemRejectsNonPositiveCategories pins that the
// persisted-item gate rejects a category list carrying a non-positive
// Torznab id: both producers only ever union positive ids (catTV/catAnime/
// catMovies), so a zero or negative entry identifies a hand-edited or
// corrupted snapshot. Accepting it would let a non-empty-but-all-invalid
// list disable filterByCats' uncategorized fallback and turn a real release
// into a false no-match instead of re-baselining the snapshot.
func TestValidPersistedItemRejectsNonPositiveCategories(t *testing.T) {
	for name, categories := range map[string][]int{
		"zero":     {0},
		"negative": {-1},
		"mixed":    {catAnime, 0},
	} {
		t.Run(name, func(t *testing.T) {
			it := journalItem{item: item{Title: "x", Categories: categories}}
			if validPersistedItem(&it) {
				t.Errorf("validPersistedItem(Categories=%v) = true, want false", categories)
			}
		})
	}
}

// TestValidPersistedItemRejectsOversizedCategoryList pins the list-length arm
// of the shared persisted-item limits: both producers union at most the three
// Torznab ids the feed uses (catTV/catAnime/catMovies), so a category list
// past maxPersistedCategories identifies a hand-edited or corrupted snapshot
// and must be rejected at load. A list at the limit stays accepted (the bound
// rejects strictly above, not at).
func TestValidPersistedItemRejectsOversizedCategoryList(t *testing.T) {
	over := make([]int, maxPersistedCategories+1)
	for i := range over {
		over[i] = catAnime
	}
	it := journalItem{item: item{Title: "x", Categories: over}}
	if validPersistedItem(&it) {
		t.Errorf("validPersistedItem(%d categories) = true, want false", len(over))
	}
	atLimit := journalItem{item: item{Title: "x", Categories: over[:maxPersistedCategories]}}
	if !validPersistedItem(&atLimit) {
		t.Errorf("validPersistedItem(%d categories) = false, want true (an at-limit list is valid)", maxPersistedCategories)
	}
}

// TestValidPersistedItemAcceptsMaxFieldLength pins the inclusive endpoint of
// the persisted string-field cap: the documented contract rejects only values
// PAST maxPersistedFieldBytes, so an exactly-at-limit field stays valid. The
// oversized-field tests alone leave a boundary slip from `>` to `>=` undetected,
// which would reject every at-limit title the harvest can legitimately produce.
func TestValidPersistedItemAcceptsMaxFieldLength(t *testing.T) {
	atLimit := strings.Repeat("x", maxPersistedFieldBytes)
	it := journalItem{item: item{Title: atLimit}}
	if !validPersistedItem(&it) {
		t.Errorf("validPersistedItem(Title length %d) = false, want true", len(atLimit))
	}
}

// TestRebuildWarnedIdentityPropagatesTransitively pins the transitive closure in
// collectWarnedIdentities across MORE than one hop: A (Broken, nyaa:1+H1)
// links B (nyaa:2+H1) by hash, and B links C (a nyaa:2 occurrence carrying
// H2) by key, so H2 is only reachable through B (entries are ordered so C is
// scanned before B, which is what used to require a second sweep). A previously
// journaled item whose stored info hash is H2 must be retracted through ws.ids;
// a single-hop regression (a traversal that stops expanding after the directly
// warned nodes) would leave it serving warned bytes on RSS while
// every existing warned-exclusion test still passes.
func TestRebuildWarnedIdentityPropagatesTransitively(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	h1 := strings.Repeat("a", 40)
	h2 := strings.Repeat("b", 40)
	now := time.Now().UTC().Truncate(time.Second)
	writeSnapshotFile(t, path, &snapshot{
		Owners:    owns(),
		Published: map[string]bool{"nyaa:9": true, h2: true},
		NyaaFeed: []journalItem{
			{item: item{Title: "Show - S01 (1080p) [G]", GUID: "https://nyaa.si/view/9", InfoHash: h2, PubDate: now}, Key: "nyaa:9", AniListID: 5, FirstSeen: now},
		},
	})
	mkv := []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}}
	entries := []seadex.Entry{
		// C first: its shared key nyaa:2 is only warned after B is folded,
		// so folding C's H2 requires a second propagation sweep.
		{AniListID: 3, Torrents: []seadex.Torrent{{Tracker: "Nyaa", URL: "https://nyaa.si/view/2", InfoHash: h2, IsBest: true, Files: mkv}}},
		{AniListID: 2, Torrents: []seadex.Torrent{{Tracker: "Nyaa", URL: "https://nyaa.si/view/2", InfoHash: h1, IsBest: true, Files: mkv}}},
		{AniListID: 1, Torrents: []seadex.Torrent{{Tracker: "Nyaa", URL: "https://nyaa.si/view/1", InfoHash: h1, IsBest: true, Tags: []string{"Broken"}, Files: mkv}}},
	}
	if err := newExcludingTestWriter(path).Rebuild(t.Context(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 0 {
		t.Errorf("nyaa feed = %+v, want empty (the carried nyaa:9 item stores hash %s, warned only through the key-then-hash chain)", snap.NyaaFeed, h2)
	}
	if _, ok := byHashOf(&snap)[h2]; ok {
		t.Error("curation set marks the transitively warned hash (searches would serve it)")
	}
	if _, ok := byKeyOf(&snap)["nyaa:2"]; ok {
		t.Error("curation set marks the transitively warned key")
	}
}

// TestRebuildBaselinesOversizedSeenLedgerKey pins the publication-log ingress of
// the shared persisted-item limits: the ledger is carried forward verbatim
// and never pruned, so an over-limit identity key from a hand-edited
// snapshot must warn and re-baseline as malformed rather than persist in
// every future snapshot.
func TestRebuildBaselinesOversizedSeenLedgerKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	oversized := strings.Repeat("k", maxPersistedFieldBytes+1)
	writeSnapshotFile(t, path, &snapshot{
		Owners:    owns(),
		Published: map[string]bool{oversized: true},
	})
	log, rec := capture.New()
	w := newLoggedTestWriter(path, log)
	entries := []seadex.Entry{nyaaEntry(7, 42, true, "Show - S01E01 (1080p) [G].mkv")}
	if err := w.Rebuild(t.Context(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if snap.Published[oversized] {
		t.Error("oversized publication-log key survived the rebuild (it would persist in every future snapshot)")
	}
	if !snap.Published["nyaa:42"] {
		t.Errorf("publication log missing the curated identity after re-baseline: %v", snap.Published)
	}
	if !rec.Contains(msgSnapshotMalformed) {
		t.Errorf("oversized publication-log key not warned; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestRebuildCanonicalizesStoredHashBeforeWarningRetraction pins that the
// shared snapshot decode canonicalizes persisted identity fields BEFORE the
// writer compares them: a carried item whose at-rest InfoHash is uppercase
// must still match the current catalogue's canonical warned hash, so a
// curator-warned release (Broken) cannot keep riding the RSS journal under a
// stale tracker key while search suppresses it.
func TestRebuildCanonicalizesStoredHashBeforeWarningRetraction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	hash := strings.Repeat("a", 40)
	now := time.Now().UTC().Truncate(time.Second)
	writeSnapshotFile(t, path, &snapshot{
		Owners:    owns(keyed("nyaa:99", true)),
		Published: map[string]bool{"nyaa:99": true},
		NyaaFeed: []journalItem{{
			item:      item{Title: "Show - S01 (1080p) [W]", GUID: "https://nyaa.si/view/99", InfoHash: strings.ToUpper(hash), PubDate: now},
			Key:       "nyaa:99",
			AniListID: 8,
			FirstSeen: now,
		}},
	})
	entries := []seadex.Entry{{AniListID: 7, Torrents: []seadex.Torrent{{
		Tracker: "Nyaa", URL: "https://nyaa.si/view/41", InfoHash: hash, IsBest: true,
		Tags:  []string{"Broken"},
		Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [W].mkv"}},
	}}}}
	w := newExcludingTestWriter(path)
	if err := w.Rebuild(t.Context(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if got := readSnapshotFile(t, path).NyaaFeed; len(got) != 0 {
		t.Errorf("nyaa feed = %+v, want empty after canonical stored-hash warning retraction", got)
	}
}

// TestValidPersistedItemRejectsOversizedFields pins the string-field arm of
// the shared persisted-item limits across EVERY field the gate walks, not
// just Title: a hand-edited or corrupted snapshot can oversize any of them,
// and each one is either rendered into the served Torznab item (whose XML
// escaping expands an ampersand-heavy value ~5x) or compared as identity by
// the writer's carry gates, so dropping any single field from the check would
// let one over-limit value reach renderFeed.
func TestValidPersistedItemRejectsOversizedFields(t *testing.T) {
	over := strings.Repeat("x", maxPersistedFieldBytes+1)
	tests := map[string]journalItem{
		"title":                  {item: item{Title: over}},
		"guid":                   {item: item{Title: "x", GUID: over}},
		"info url":               {item: item{Title: "x", InfoURL: over}},
		"download url":           {item: item{Title: "x", DownloadURL: over}},
		"info hash":              {item: item{Title: "x", InfoHash: over}},
		"download volume factor": {item: item{Title: "x", DownloadVolumeFactor: over}},
		"journal key":            {item: item{Title: "x"}, Key: over},
	}
	for name, it := range tests {
		t.Run(name, func(t *testing.T) {
			if validPersistedItem(&it) {
				t.Errorf("validPersistedItem(oversized %s, %d bytes) = true, want false", name, len(over))
			}
		})
	}
}

// TestRebuildDropsOversizedABFeedItem is the AnimeBytes twin of
// TestRebuildDropsOversizedFeedItem: the shared decode gate prunes BOTH
// persisted journals, so a previous snapshot whose ab_feed carries an item past
// maxPersistedFieldBytes must drop it exactly like its nyaa_feed counterpart.
// Without this case the ab_feed argument of decodeSnapshot's per-item prune is
// unpinned - the writer would carry the over-limit AB item forward while the
// server's readSnapshot drops the same bytes, so the two ends would serve
// different feeds from one file.
func TestRebuildDropsOversizedABFeedItem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	now := time.Now().UTC().Truncate(time.Second)
	writeSnapshotFile(t, path, &snapshot{
		Owners:    owns(keyed("ab:123", true)),
		Published: map[string]bool{"ab:123": true},
		ABFeed: []journalItem{{
			item: item{
				Title:   strings.Repeat("a", maxPersistedFieldBytes+1),
				GUID:    "https://animebytes.tv/torrents.php?id=1&torrentid=123",
				PubDate: now,
			},
			Key: "ab:123", AniListID: 9, FirstSeen: now,
		}},
	})
	log, rec := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{
		NyaaTorznabURL: "http://prowlarr/1/api",
		ABTorznabURL:   "http://prowlarr/2/api",
		ABPasskey:      "PASSKEY",
	}}, log, nil)
	entries := []seadex.Entry{{
		AniListID: 9,
		Torrents: []seadex.Torrent{{
			Tracker: "AB", URL: "/torrents.php?id=1&torrentid=123", IsBest: true,
			Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
		}},
	}}
	if err := w.Rebuild(t.Context(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.ABFeed) != 0 {
		t.Errorf("ab_feed = %d items, want 0 (an over-limit ab_feed item must be dropped, not carried)", len(snap.ABFeed))
	}
	if !snap.Published["ab:123"] {
		t.Errorf("publication log missing the curated AB identity: %v", snap.Published)
	}
	if rec.Contains(msgSnapshotMalformed) {
		t.Errorf("an over-limit ab_feed item re-baselined the whole journal; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestRetainValidTitlesDropsOverLimitEntries pins BOTH arms of the
// harvested-title cache ingress: the cached title (applied over a carried
// item's title after the creation-time check) and its KEY, which is a journal
// key carried forward across rebuilds. Each over-limit entry is dropped and
// counted while the rest of the cache survives - the journal is never
// re-baselined for a re-earnable derived value (l-f60). The at-limit case pins
// the inclusive endpoint: the documented contract refuses only values PAST
// maxPersistedFieldBytes.
func TestRetainValidTitlesDropsOverLimitEntries(t *testing.T) {
	over := strings.Repeat("x", maxPersistedFieldBytes+1)
	atLimit := strings.Repeat("x", maxPersistedFieldBytes)
	tests := map[string]struct {
		in      map[string]string
		want    map[string]string
		dropped int
	}{
		"over-limit key dropped, sibling kept": {
			in:      map[string]string{over: "Show - S01 (1080p) [G]", "nyaa:7": "Show - S02 (1080p) [G]"},
			want:    map[string]string{"nyaa:7": "Show - S02 (1080p) [G]"},
			dropped: 1,
		},
		"over-limit title dropped, sibling kept": {
			in:      map[string]string{"nyaa:42": over, "nyaa:7": "Show - S02 (1080p) [G]"},
			want:    map[string]string{"nyaa:7": "Show - S02 (1080p) [G]"},
			dropped: 1,
		},
		"at-limit key and title kept": {
			in:   map[string]string{atLimit: atLimit},
			want: map[string]string{atLimit: atLimit},
		},
		"empty title dropped without counting": {
			in:   map[string]string{"nyaa:42": ""},
			want: map[string]string{},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, dropped := retainValidTitles(tc.in)
			if !maps.Equal(got, tc.want) {
				t.Errorf("retainValidTitles kept %d entries, want %d", len(got), len(tc.want))
			}
			if dropped != tc.dropped {
				t.Errorf("dropped = %d, want %d", dropped, tc.dropped)
			}
		})
	}
}

// TestSeenLedgerWithinLimitsBoundary pins the inclusive endpoint of the
// publication-log key cap: the documented contract rejects only keys PAST
// maxPersistedFieldBytes, so an exactly-at-limit key must stay accepted. The
// end-to-end re-baseline test covers only the over-limit side, leaving a
// boundary slip (a `>=` comparison) undetected - it would re-baseline the
// journal on an honest ledger and re-broadcast nothing while quietly
// discarding accumulated novelty state.
func TestSeenLedgerWithinLimitsBoundary(t *testing.T) {
	atLimit := strings.Repeat("k", maxPersistedFieldBytes)
	if !publicationLogWithinLimits(map[string]bool{atLimit: true}) {
		t.Errorf("publicationLogWithinLimits(key %d bytes) = false, want true", len(atLimit))
	}
	if publicationLogWithinLimits(map[string]bool{atLimit + "k": true}) {
		t.Errorf("publicationLogWithinLimits(key %d bytes) = true, want false", len(atLimit)+1)
	}
}

// TestSeenLedgerWithinLimitsChargesJSONEscaping pins that the aggregate cap is
// charged against the SERIALIZED ledger cost, not the decoded key bytes.
// encoding/json escapes the HTML-sensitive set, so every '<' costs six bytes
// (\u003c) in the file persist writes: a ledger of escape-heavy keys whose
// decoded length sits comfortably under maxPublicationLogBytes still pushes the
// rebuilt snapshot past maxFeedBytes, which is exactly the persist-wedges-
// forever case the aggregate cap exists to prevent. The test asserts the
// decoded approximation would have ACCEPTED this ledger, so it fails if the
// check ever reverts to len(k)+8.
func TestSeenLedgerWithinLimitsChargesJSONEscaping(t *testing.T) {
	const (
		keyRunes = 1000
		keys     = 1500
	)
	seen := make(map[string]bool, keys)
	decoded := 0
	for i := range keys {
		k := strings.Repeat("<", keyRunes) + strconv.Itoa(i)
		seen[k] = true
		decoded += len(k) + 8
	}
	if decoded > maxPublicationLogBytes {
		t.Fatalf("decoded ledger cost %d exceeds cap %d; the fixture no longer isolates the escaping charge", decoded, maxPublicationLogBytes)
	}
	if publicationLogWithinLimits(seen) {
		t.Errorf("publicationLogWithinLimits(%d escape-heavy keys) = true, want false (encoded cost ~%d bytes exceeds cap %d)",
			keys, keys*(6*keyRunes+10), maxPublicationLogBytes)
	}
}

// TestRebuildBlanksCarriedForeignInfoURL pins that the persisted-InfoURL
// allowlist lives in the shared decode gate (decodeSnapshot), not only in the
// reader: an item whose torrent has left the curation set keeps its STORED
// render (carryStoredItem) and persist scrubs only DownloadURL, so a
// foreign-host info link planted in feed.json would otherwise be re-persisted
// on every rebuild for up to feedJournalMaxAge while only the server blanked
// it at serve time.
func TestRebuildBlanksCarriedForeignInfoURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	now := time.Now().UTC().Truncate(time.Second)
	writeSnapshotFile(t, path, &snapshot{
		Owners:    owns(keyed("nyaa:42", true)),
		Published: map[string]bool{"nyaa:42": true},
		NyaaFeed: []journalItem{{
			item: item{
				Title:   "Show S01 1080p [Grp]",
				GUID:    "https://nyaa.si/view/42",
				InfoURL: "https://evil.example/phish",
				PubDate: now,
			},
			Key: "nyaa:42", AniListID: 7, FirstSeen: now,
		}},
	})
	w := newTestWriter(path, "", false)
	// No entries: the journaled torrent is no longer curated, so it is
	// carried with its stored render rather than re-synthesized.
	if err := w.Rebuild(t.Context(), nil, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	got := readSnapshotFile(t, path).NyaaFeed
	if len(got) != 1 {
		t.Fatalf("nyaa feed = %d items (%+v), want the carried item", len(got), got)
	}
	if got[0].InfoURL != "" {
		t.Errorf("carried item InfoURL = %q, want blanked: the decode gate must scrub a non-SeaDex info link for the writer too", got[0].InfoURL)
	}
}

// TestRebuildNeverLogsABPasskey pins the LOG side of the at-rest credential
// contract TestRebuildPersistsABItemsGUIDOnly pins on disk: the passkey is a
// per-cycle log hazard too (Rebuild's snapshot line names AB counts, the
// missing-passkey nudge fires per rebuild, and a failed persist wraps the
// path), so a full rebuild that journals AnimeBytes releases must emit ZERO
// log records - message or attribute - carrying the secret.
func TestRebuildNeverLogsABPasskey(t *testing.T) {
	const passkey = "SUPERSECRETPASSKEY123"
	log, rec := capture.New()
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	entries := []seadex.Entry{{
		AniListID: 154587,
		Torrents: []seadex.Torrent{{
			Tracker: "AB", URL: "/torrents.php?id=86576&torrentid=1167293", IsBest: true,
			Files: []seadex.File{{Length: 1, Name: "Frieren - S01E01 (BD Remux 1080p) [PMR].mkv"}},
		}},
	}}
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{
		ABTorznabURL: "http://prowlarr/2/api", ABPasskey: passkey,
	}}, log, nil)
	if err := w.Rebuild(t.Context(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	// The guard is vacuous unless the rebuild actually journaled the AB release
	// (renderJournalItem is what builds the passkey-bearing download link).
	if got := readSnapshotFile(t, path).ABFeed; len(got) != 1 {
		t.Fatalf("ab feed = %d items, want the journaled AB release", len(got))
	}
	if rec.Len() == 0 {
		t.Fatal("no log records captured; the rebuild must at least log the snapshot line")
	}
	for _, r := range rec.Records() {
		if strings.Contains(r.Message, passkey) {
			t.Errorf("log message leaks the AB passkey: %q", r.Message)
		}
		r.Attrs(func(a slog.Attr) bool {
			if strings.Contains(a.Value.String(), passkey) {
				t.Errorf("log attr %q leaks the AB passkey: %q", a.Key, a.Value.String())
			}
			return true
		})
	}
}

// TestLoadPreviousDropsOversizedHarvestCheckpoint pins the cursor's size-cap
// arm at the load boundary: a cursor past maxPersistedCursorBytes is
// external corruption and must be reset. The cursor is the one persisted string
// carried forward verbatim, so without the reset attacker-shaped text rides
// every future snapshot until persist exceeds maxFeedBytes and wedges every
// rebuild. Only the cursor is discarded - the valid journal stays usable.
func TestLoadPreviousDropsOversizedHarvestCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	cursor := strings.Repeat("x", maxPersistedCursorBytes+1)
	writeSnapshotFile(t, path, &snapshot{
		Owners:        owns(),
		Published:     map[string]bool{"nyaa:42": true},
		HarvestCursor: cursor,
	})
	log, rec := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{Path: path}, log, nil)
	prev, err := w.loadPrevious(t.Context())
	if err != nil {
		t.Fatalf("loadPrevious: %v", err)
	}
	if prev.baseline {
		t.Error("oversized harvest cursor re-baselined the valid journal, want only the cursor reset")
	}
	if prev.cursor != "" {
		t.Errorf("loadPrevious cursor = %d bytes, want empty after exceeding the %d-byte cap", len(prev.cursor), maxPersistedCursorBytes)
	}
	if !prev.published["nyaa:42"] {
		t.Errorf("loadPrevious discarded the valid publication log while resetting the cursor: %v", prev.published)
	}
	if !rec.Contains("previous feed snapshot harvest cursor exceeds size cap; restarting the harvest rotation") {
		t.Errorf("oversized harvest cursor warning not logged; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestRebuildBaselinesFalseSeenLedgerValue pins the publication log's producer
// invariant at ingress: the writer only ever records true membership, so a
// false value can only come from corruption or hand-editing. Because
// journalIfNew reads the VALUE, carrying one forward would re-broadcast an
// already-baselined release as newly curated (and the arr could auto-grab it).
// It must take the existing deterministic-corruption path instead: warn,
// re-baseline the current catalogue, and serve an empty journal.
func TestRebuildBaselinesFalseSeenLedgerValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	writeSnapshotFile(t, path, &snapshot{
		Owners:    owns(),
		Published: map[string]bool{"nyaa:42": false},
	})
	log, rec := capture.New()
	entries := []seadex.Entry{nyaaEntry(7, 42, true, "Show - S01E01 (1080p) [G].mkv")}
	if err := newLoggedTestWriter(path, log).Rebuild(t.Context(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 0 {
		t.Errorf("feed = %d items, want 0: a false publication-log value must re-baseline instead of re-broadcasting", len(snap.NyaaFeed))
	}
	if !snap.Published["nyaa:42"] {
		t.Errorf("publication log was not rebuilt from current curation: %v", snap.Published)
	}
	if !rec.Contains(msgSnapshotMalformed) {
		t.Errorf("false publication-log value not warned as malformed; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestRebuildOffTrackerJournalSurvivesAndReturns pins the reversibility of a
// tracker's off switch (l-f161). Blanking a Torznab URL used to skip the carry,
// so ONE rebuild dropped every journaled item for that scope - while the
// never-pruned publication log kept their identities, so journalIfNew reported
// isNew=false forever and those releases could never reach RSS again. An
// operator disabling AnimeBytes for a few days permanently lost the un-grabbed
// part of its journal window.
//
// The journal must therefore be CARRIED while the tracker is off (it costs
// nothing at rest - both feeds are stored GUID-only - and the serve side already
// returns nothing for an unconfigured scope), must not GROW while off, and must
// be servable again on re-enable.
func TestRebuildOffTrackerJournalSurvivesAndReturns(t *testing.T) {
	entries := []seadex.Entry{{
		AniListID: 1,
		Torrents: []seadex.Torrent{{
			Tracker: "AB", URL: "https://animebytes.tv/torrents.php?id=1&torrentid=123",
			IsBest: true, ReleaseGroup: "PMR",
			Files: []seadex.File{{Length: 100, Name: "Show - S01E01 [PMR].mkv"}},
		}},
	}}
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)

	// Tracker ON: the release journals.
	if err := newTestWriter(path, "passkey", true).Rebuild(t.Context(), entries, nil); err != nil {
		t.Fatalf("Rebuild (AB on): %v", err)
	}
	if got := len(readSnapshotFile(t, path).ABFeed); got != 1 {
		t.Fatalf("ab_feed = %d items after the first rebuild, want 1", got)
	}

	// Tracker OFF: one rebuild must not destroy the journal.
	if err := newTestWriter(path, "passkey", false).Rebuild(t.Context(), entries, nil); err != nil {
		t.Fatalf("Rebuild (AB off): %v", err)
	}
	off := readSnapshotFile(t, path)
	if len(off.ABFeed) != 1 {
		t.Errorf("ab_feed = %d items while the tracker is off, want the journal carried (1): %+v",
			len(off.ABFeed), off.ABFeed)
	}
	if !off.Published["ab:123"] {
		t.Error("publication log lost the AB identity while the tracker was off")
	}

	// Tracker BACK ON: the item is still there, so it can be served again.
	if err := newTestWriter(path, "passkey", true).Rebuild(t.Context(), entries, nil); err != nil {
		t.Fatalf("Rebuild (AB re-enabled): %v", err)
	}
	back := readSnapshotFile(t, path)
	if len(back.ABFeed) != 1 {
		t.Errorf("ab_feed = %d items after re-enabling, want the carried journal (1); the off switch must be reversible",
			len(back.ABFeed))
	}
}

// TestRebuildOffTrackerJournalDoesNotGrow pins the other half: while a tracker
// is off its journal shrinks (items keep aging out) but never GROWS, so a
// disabled tracker cannot accumulate releases the operator opted out of.
func TestRebuildOffTrackerJournalDoesNotGrow(t *testing.T) {
	first := []seadex.Entry{{
		AniListID: 1,
		Torrents: []seadex.Torrent{{
			Tracker: "AB", URL: "https://animebytes.tv/torrents.php?id=1&torrentid=123",
			IsBest: true, ReleaseGroup: "PMR",
			Files: []seadex.File{{Length: 100, Name: "Show - S01E01 [PMR].mkv"}},
		}},
	}}
	// A SECOND curated AB release, newly appearing while the tracker is off.
	second := append(append([]seadex.Entry{}, first...), seadex.Entry{
		AniListID: 2,
		Torrents: []seadex.Torrent{{
			Tracker: "AB", URL: "https://animebytes.tv/torrents.php?id=2&torrentid=456",
			IsBest: true, ReleaseGroup: "PMR",
			Files: []seadex.File{{Length: 100, Name: "Other - S01E01 [PMR].mkv"}},
		}},
	})

	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	if err := newTestWriter(path, "passkey", true).Rebuild(t.Context(), first, nil); err != nil {
		t.Fatalf("Rebuild (AB on): %v", err)
	}
	if err := newTestWriter(path, "passkey", false).Rebuild(t.Context(), second, nil); err != nil {
		t.Fatalf("Rebuild (AB off, new release curated): %v", err)
	}

	off := readSnapshotFile(t, path)
	if len(off.ABFeed) != 1 {
		t.Errorf("ab_feed = %d items, want only the carried one: an off tracker's journal must not grow", len(off.ABFeed))
	}
	// The new identity is still recorded, so it is not treated as new later.
	// This is the ONE write to the publication log that is not a publication,
	// and it is deliberate: a disabled tracker is opted out, not refused, so
	// re-enabling it must not journal that tracker's whole curated catalogue as
	// newly curated in one pass (see growJournal).
	if !off.Published["ab:456"] {
		t.Error("publication log missing the release curated while the tracker was off; re-enabling AB would re-broadcast its whole catalogue")
	}
}

// TestCurationProjectionBestWinsAcrossDuplicateOccurrences pins the OR fold in
// the search curation index: one SeaDex torrent can be attached to several
// entries, and search marks a matched Prowlarr result best-or-alt from these
// maps, so the fold must be best-wins in BOTH scan orders.
//
// The fold now runs at PROJECTION time over per-owner votes rather than being
// accumulated destructively into a persisted map, which is what makes it
// recomputable - and therefore what makes a best-to-alt demotion expressible at
// all (see TestPerOwnerVotesMakeADemotionRepresentable). The order-independence
// this test pins is unchanged.
func TestCurationProjectionBestWinsAcrossDuplicateOccurrences(t *testing.T) {
	const hash = "abcdef1234567890abcdef1234567890abcdef12"
	mkv := []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}}
	entries := []seadex.Entry{
		{AniListID: 1, Torrents: []seadex.Torrent{{
			Tracker: "Nyaa", URL: "https://nyaa.si/view/42", InfoHash: hash, Files: mkv,
		}}},
		{AniListID: 2, Torrents: []seadex.Torrent{{
			Tracker: "Nyaa", URL: "https://nyaa.si/view/42", InfoHash: hash, IsBest: true, Files: mkv,
		}}},
	}
	for _, order := range []string{"alt first", "best first"} {
		t.Run(order, func(t *testing.T) {
			set := projectCuration(ownershipOf(entries))
			if !set.byHash[hash] {
				t.Errorf("by_hash[%s] = false, want true (best-wins across occurrences)", hash)
			}
			if !set.byKey["nyaa:42"] {
				t.Error(`by_key["nyaa:42"] = false, want true (best-wins across occurrences)`)
			}
			if !set.byPair[pairKey(hash, "nyaa:42")] {
				t.Error("by_pair missing the same-torrent hash/key pair")
			}
		})
		slices.Reverse(entries)
	}
}

// TestSplitCurationWarnedLeavesInputUnmutated pins the aliasing contract the
// feed rebuild depends on: the cycle hands the SAME entries slice to the
// compare pass, so removing a curation-warned torrent must produce a fresh
// Torrents slice rather than filtering in place.
func TestSplitCurationWarnedLeavesInputUnmutated(t *testing.T) {
	mkv := []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [W].mkv"}}
	entries := []seadex.Entry{{AniListID: 7, Torrents: []seadex.Torrent{
		{Tracker: "Nyaa", URL: "https://nyaa.si/view/41", IsBest: true, Tags: []string{"Broken"}, Files: mkv},
		{Tracker: "Nyaa", URL: "https://nyaa.si/view/42", IsBest: true, Files: mkv},
	}}}
	kept, ws := splitCurationWarned(entries, feedExcludesWarnings())
	if len(entries[0].Torrents) != 2 || entries[0].Torrents[0].URL != "https://nyaa.si/view/41" {
		t.Errorf("splitCurationWarned mutated the shared input: %+v", entries[0].Torrents)
	}
	if len(kept[0].Torrents) != 1 || kept[0].Torrents[0].URL != "https://nyaa.si/view/42" {
		t.Errorf("kept torrents = %+v, want only the unwarned nyaa:42", kept[0].Torrents)
	}
	if _, ok := ws.keys["nyaa:41"]; !ok {
		t.Errorf("warned key set = %v, want the warned nyaa:41 key", ws.keys)
	}
}

// TestDecodeSnapshotDropsJournalItemWithoutIdentity pins the shared decode
// gate's journal-record invariant (h-f2): in a post-journal snapshot (seen
// ledger present) every feed item must carry a Key and a nonzero FirstSeen, or
// that ITEM is dropped and counted per tracker feed. Without the invariant the
// READER installs and serves a timestamp-less item indefinitely - the writer's
// carry gate drops that shape, but in resident-idle mode no rebuild ever runs it
// - so the item escapes the bounded journal window entirely. Dropping rather
// than refusing the whole snapshot is what keeps one corrupted item from taking
// the entire Torznab surface down on a cold start (l-f45): the curation ownership
// fact and the rest of the journal survive. There is no schema-scoped exemption
// any more: the version envelope means every snapshot this decode accepts was
// written by THIS schema, so there is no retired shape whose items must be
// excused from a promise it never made.
func TestDecodeSnapshotDropsJournalItemWithoutIdentity(t *testing.T) {
	tests := map[string]struct {
		doc      string
		wantKept int
	}{
		"item without FirstSeen dropped": {
			doc:      `{"version":2,"owners":{},"published":{},"nyaa_feed":[{"Key":"nyaa:1","Title":"x","GUID":"https://nyaa.si/view/1"}],"ab_feed":[]}`,
			wantKept: 0,
		},
		"item without Key dropped": {
			doc:      `{"version":2,"owners":{},"published":{},"nyaa_feed":[{"FirstSeen":"2026-07-01T00:00:00Z","Title":"x","GUID":"https://nyaa.si/view/1"}],"ab_feed":[]}`,
			wantKept: 0,
		},
		"bookkeeping-less item dropped, sibling kept": {
			doc: `{"version":2,"owners":{},"published":{},"nyaa_feed":[{"Title":"orphan","GUID":"https://nyaa.si/view/1"},` +
				`{"FirstSeen":"2026-07-01T00:00:00Z","Key":"nyaa:2","Title":"kept","GUID":"https://nyaa.si/view/2"}],"ab_feed":[]}`,
			wantKept: 1,
		},
		"item with both accepted": {
			doc:      `{"version":2,"owners":{},"published":{},"nyaa_feed":[{"FirstSeen":"2026-07-01T00:00:00Z","Key":"nyaa:1","Title":"x","GUID":"https://nyaa.si/view/1"}],"ab_feed":[]}`,
			wantKept: 1,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			snap, scrub, reason, err := decodeSnapshot([]byte(tc.doc))
			if err != nil {
				t.Fatalf("decodeSnapshot error = %v, want the snapshot accepted with the bad items pruned", err)
			}
			if reason != "" {
				t.Errorf("reason = %q, want \"\" (a per-item defect must not reject the snapshot)", reason)
			}
			if len(snap.NyaaFeed) != tc.wantKept {
				t.Errorf("nyaa_feed = %d items, want %d", len(snap.NyaaFeed), tc.wantKept)
			}
			if got, want := scrub.droppedItems[upstreamNyaa], strings.Count(tc.doc, "GUID")-tc.wantKept; got != want {
				t.Errorf("droppedItems[nyaa] = %d, want %d (the reader WARNs per tracker feed)", got, want)
			}
			if byHashOf(&snap) == nil || byKeyOf(&snap) == nil {
				t.Error("curation maps discarded; a per-item defect must not cost the search curation set")
			}
		})
	}
}

// TestDecodeSnapshotStructuralGateIsBounded pins the three structural
// properties of the snapshot decode's own ingress, which stopped riding a
// whole-document bounded.Preflight because that pass holds one key set per
// traversed object - unbounded in exactly the dimension this decode exists to
// bound, and paid on every load by both consumers (h-f6).
//
// The allocation arm is the finding itself: a key-dense document must cost a
// small multiple of its own bytes, not the ~24x churn (and tens of MB of LIVE
// heap) a per-object key set costs inside a 256 MiB container. An unknown field
// is consumed as one raw value, so the whole object costs about one copy of
// itself; the 8x bound leaves generous slack over the ~2x this does while still
// failing the key-set shape by a wide margin.
func TestDecodeSnapshotStructuralGateIsBounded(t *testing.T) {
	t.Run("repeated top-level schema field rejected", func(t *testing.T) {
		// The accumulated facts are the ambiguity that matters: Unmarshal would
		// silently resolve a second "published" to the last occurrence, and at a
		// tamperable boundary that is evidence, not a value to pick.
		doc := `{"version":2,"owners":{},"published":{"nyaa:1":true},"published":{},"nyaa_feed":[],"ab_feed":[]}`
		if _, _, _, err := decodeSnapshot([]byte(doc)); err == nil {
			t.Error("decodeSnapshot accepted a repeated top-level \"published\" field, want it refused as tampering")
		}
		// Case-insensitively too, since Unmarshal matches struct fields that way.
		mixed := `{"version":2,"owners":{},"published":{},"Published":{},"nyaa_feed":[],"ab_feed":[]}`
		if _, _, _, err := decodeSnapshot([]byte(mixed)); err == nil {
			t.Error("decodeSnapshot accepted \"published\" repeated under a different case, want it refused")
		}
	})

	t.Run("duplicate key inside a decoded value keeps Unmarshal semantics", func(t *testing.T) {
		// Deliberately NOT rejected: only the top-level schema fields fail
		// closed, so the persisted-file contract stays json.Unmarshal's
		// everywhere the ambiguity cannot swap one accumulated map for another.
		doc := `{"version":2,"owners":{},"published":{},"nyaa_feed":[{"Key":"nyaa:1","Title":"first","Title":"second",` +
			`"GUID":"https://nyaa.si/view/1","FirstSeen":"2026-07-01T00:00:00Z"}],"ab_feed":[]}`
		snap, _, reason, err := decodeSnapshot([]byte(doc))
		if err != nil || reason != "" {
			t.Fatalf("decodeSnapshot(nested duplicate key) reason=%q err=%v, want it accepted", reason, err)
		}
		if len(snap.NyaaFeed) != 1 || snap.NyaaFeed[0].Title != "second" {
			t.Errorf("nyaa_feed = %+v, want one item titled \"second\" (Unmarshal's last-occurrence resolution)", snap.NyaaFeed)
		}
	})

	t.Run("over-deep unknown field rejected", func(t *testing.T) {
		// Depth is still bounded without the preflight, by encoding/json's own
		// scanner ceiling: an unknown field is consumed through Decode, never a
		// token walk that would grow a container stack per open bracket.
		const depth = bounded.MaxDepth + 1
		doc := `{"version":2,"owners":{},"published":{},"unknown":` + strings.Repeat("[", depth) + strings.Repeat("]", depth) + `}`
		if _, _, _, err := decodeSnapshot([]byte(doc)); err == nil {
			t.Errorf("decodeSnapshot accepted an unknown field nested %d deep, want the scanner's depth ceiling to refuse it", depth)
		}
	})

	t.Run("key-dense unknown field costs a small multiple of its bytes", func(t *testing.T) {
		const keys = 300_000
		var doc strings.Builder
		doc.WriteString(`{"version":2,"owners":{"1":[{"key":"nyaa:1","best":true}]},"published":{},"nyaa_feed":[],"ab_feed":[],"unknown":{`)
		for i := range keys {
			if i > 0 {
				doc.WriteByte(',')
			}
			doc.WriteString(`"k`)
			doc.WriteString(strconv.Itoa(i))
			doc.WriteString(`":1`)
		}
		doc.WriteString("}}")
		data := []byte(doc.String())
		if len(data) > maxFeedBytes {
			t.Fatalf("key-dense document = %d bytes, want it under the %d byte cap the read admits", len(data), maxFeedBytes)
		}
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		snap, _, reason, err := decodeSnapshot(data)
		runtime.ReadMemStats(&after)
		if err != nil || reason != "" {
			t.Fatalf("decodeSnapshot(key-dense unknown field) reason=%q err=%v, want it accepted (unknown fields are skipped)", reason, err)
		}
		if !byKeyOf(&snap)["nyaa:1"] {
			t.Error("schema fields did not decode past the unknown field")
		}
		if allocated, budget := after.TotalAlloc-before.TotalAlloc, uint64(8*len(data)); allocated > budget {
			t.Errorf("decode of a %d-byte, %d-key document allocated %d bytes, want under %d (a per-object key set is what blows this budget)",
				len(data), keys, allocated, budget)
		}
	})
}

// TestDecodeSnapshotBoundsCardinality pins the decode-amplification bound
// (h-f4): maxFeedBytes caps the SERIALIZED file, which does not bound what a
// decode allocates - a document far below the byte cap can encode millions of
// compact array elements or map entries, each costing tens of bytes of live
// heap, so json.Unmarshal would materialize hundreds of MB past the 256 MiB
// container budget before any per-item validation could reject it. Both
// documents here stay well under maxFeedBytes and must still be refused.
func TestDecodeSnapshotBoundsCardinality(t *testing.T) {
	t.Run("feed item cardinality", func(t *testing.T) {
		feed := `{"version":2,"owners":{},"published":{},"nyaa_feed":[` +
			strings.TrimSuffix(strings.Repeat("{},", maxSnapshotFeedItems+1), ",") + `],"ab_feed":[]}`
		if len(feed) > maxFeedBytes {
			t.Fatalf("over-cardinality feed document = %d bytes, want it under the %d byte cap", len(feed), maxFeedBytes)
		}
		if _, _, _, err := decodeSnapshot([]byte(feed)); err == nil {
			t.Error("decodeSnapshot accepted a feed past its cardinality cap, want a bounded-decode error")
		}
	})

	t.Run("publication log entry cardinality", func(t *testing.T) {
		var ledger strings.Builder
		ledger.WriteString(`{"version":2,"owners":{},"nyaa_feed":[],"ab_feed":[],"published":{`)
		for i := range maxSnapshotMapEntries + 1 {
			if i > 0 {
				ledger.WriteByte(',')
			}
			ledger.WriteString(`"nyaa:`)
			ledger.WriteString(strconv.Itoa(i))
			ledger.WriteString(`":true`)
		}
		ledger.WriteString("}}")
		if ledger.Len() > maxFeedBytes {
			t.Fatalf("over-cardinality ledger document = %d bytes, want it under the %d byte cap", ledger.Len(), maxFeedBytes)
		}
		if _, _, _, err := decodeSnapshot([]byte(ledger.String())); err == nil {
			t.Error("decodeSnapshot accepted a publication log past its entry cap, want a bounded-decode error")
		}
	})
}

// TestCollectWarnedIdentitiesClosesReverseOrderedChain pins the transitive
// closure on the shape the retired fixpoint form was quadratic on (h-f1): an
// alternating key/hash chain listed in REVERSE order, where each sweep of a
// re-scanning implementation could only discover one new link. Every node in
// the chain must end up warned regardless of catalogue order.
func TestCollectWarnedIdentitiesClosesReverseOrderedChain(t *testing.T) {
	const links = 64
	hash := func(i int) string { return fmt.Sprintf("%040x", i+1) }
	// Entry i carries two occurrences of tracker key nyaa:i, one holding
	// hash(i) and one holding hash(i+1), so hash(i+1) is the only bridge from
	// nyaa:i to nyaa:i+1 - the chain is traversable hop by hop only. Entries
	// are appended deepest-first so a re-scanning implementation discovers at
	// most one new link per pass.
	entries := make([]seadex.Entry, 0, links)
	for i := links - 1; i >= 0; i-- {
		torrents := []seadex.Torrent{
			{Tracker: "Nyaa", URL: "https://nyaa.si/view/" + strconv.Itoa(i), InfoHash: hash(i)},
			{Tracker: "Nyaa", URL: "https://nyaa.si/view/" + strconv.Itoa(i), InfoHash: hash(i + 1)},
		}
		if i == links-1 {
			// Only the deepest link is directly curation-warned.
			torrents[1].Tags = []string{"Broken"}
		}
		entries = append(entries, seadex.Entry{AniListID: i + 1, Torrents: torrents})
	}

	keys, all := collectWarnedIdentities(entries, feedExcludesWarnings())
	for i := range links {
		key := "nyaa:" + strconv.Itoa(i)
		if _, warned := keys[key]; !warned {
			t.Fatalf("key %s not warned; the closure stopped short of the chain's far end (%d keys)", key, len(keys))
		}
		if _, warned := all[hash(i)]; !warned {
			t.Errorf("hash for link %d not warned", i)
		}
	}
}

// TestDecodeSnapshotSkipsUnknownFields pins the forward-compatibility arm of
// the bounded snapshot walk: an unknown object key is token-skipped (and its
// nested value never materialized) rather than failing the snapshot, so a
// feed.json written by a NEWER binary still loads after an image rollback -
// so a member this binary does not know is not by itself a reason to refuse the
// file (the schema VERSION is what decides that). A regression here (an error on
// an unrecognized key, or a Skip that mis-advances the token stream) makes the
// binary classify the snapshot as malformed and re-baseline: the whole RSS
// journal window is lost and every current release is recorded without being
// served. The known members must still decode, and an empty "published" object
// must ALLOCATE - nil is the structural sentinel decodeSnapshot refuses on, so an
// honestly empty log must round-trip as {} rather than reading as a missing fact.
func TestDecodeSnapshotSkipsUnknownFields(t *testing.T) {
	const doc = `{"version":2,"owners":{"1":[{"key":"nyaa:42","best":true}]},"published":{},"nyaa_feed":[],"ab_feed":[],` +
		`"future_field":{"nested":[1,2,{"deep":"value"}],"n":null},"another":"scalar"}`
	snap, _, reason, err := decodeSnapshot([]byte(doc))
	if err != nil || reason != "" {
		t.Fatalf("decodeSnapshot rejected a snapshot carrying unknown fields (reason=%q err=%v); a newer binary's snapshot must still load", reason, err)
	}
	if !byKeyOf(&snap)["nyaa:42"] {
		t.Errorf("by_key = %v, want the known field decoded alongside the unknown ones", byKeyOf(&snap))
	}
	if snap.Published == nil {
		t.Error("published decoded nil, want the empty log allocated (nil is the missing-fact sentinel the structural gate refuses on)")
	}
}

// TestDecodeSnapshotBoundsAggregateMapEntries pins the SNAPSHOT-WIDE half of
// the decode-cardinality bound (maxSnapshotMapEntriesTotal), which the
// per-map test cannot reach: three maps each exactly at maxSnapshotMapEntries
// are individually legal, so only the aggregate budget refuses the 750k
// entries they add up to. Without it json.Unmarshal materializes every entry -
// tens of bytes of live heap each - inside Run's warm-up reload, OOMing the
// 256 MiB container and crashlooping the compare loop with it, and the
// per-map test keeps passing while that hole is open (CWE-400). The document
// stays under maxFeedBytes, so the byte cap the read applies does not catch it
// either.
func TestDecodeSnapshotBoundsAggregateMapEntries(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"version":2,"nyaa_feed":[],"ab_feed":[]`)
	// Three DIFFERENT members, each exactly at maxSnapshotMapEntries and each
	// individually legal: the publication log (bool values), the harvested-title
	// cache (string values) and the ownership fact (array values, one empty array
	// per owner so each owner costs exactly one charged entry).
	for _, m := range []struct{ name, value string }{
		{"published", `:true`},
		{"titles", `:"t"`},
		{"owners", `:[]`},
	} {
		b.WriteString(`,"`)
		b.WriteString(m.name)
		b.WriteString(`":{`)
		for i := range maxSnapshotMapEntries {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`"`)
			b.WriteString(strconv.Itoa(i))
			b.WriteString(`"`)
			b.WriteString(m.value)
		}
		b.WriteString(`}`)
	}
	b.WriteString(`}`)
	doc := b.String()
	if len(doc) > maxFeedBytes {
		t.Fatalf("document = %d bytes, want it under the %d byte read cap (the premise: only the aggregate budget can reject it)", len(doc), maxFeedBytes)
	}
	_, _, _, err := decodeSnapshot([]byte(doc))
	if err == nil {
		t.Fatalf("decodeSnapshot accepted %d map entries across three at-cap maps, want the aggregate budget (max %d) to reject them", 3*maxSnapshotMapEntries, maxSnapshotMapEntriesTotal)
	}
	if !strings.Contains(err.Error(), "budget exceeded") {
		t.Errorf("error = %q, want the aggregate map-entry budget error", err)
	}
}

// TestLoadPreviousRefusesANonRegularSnapshot pins the defence the confined read
// exists for, and it is a HANG this test would otherwise reproduce rather than a
// wrong answer.
//
// An unconfined read blocks in open(2) on a FIFO with no writer, and a context
// deadline does not rescue it: the block is in the kernel before any Go-level
// context check runs. That read sits inside the compare pass, which holds the
// cross-process cycle lock, so a planted FIFO wedges the pass, starves the
// health marker (refreshed only on a COMPLETED pass), fails the container
// healthcheck, and hangs again after the restart. ReadBoundedInRoot opens
// O_NONBLOCK and stats the OPEN handle, so a FIFO is refused as ErrNotRegular
// and lands in classifyPreviousReadError's transient arm: the rebuild fails, the
// last-good snapshot stands, and the next cycle retries.
//
// The whole test body runs under a deadline BECAUSE a regression here does not
// fail an assertion - it hangs the suite.
func TestLoadPreviousRefusesANonRegularSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "feed.json")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- NewFeedWriter(&FeedWriterConfig{Path: path}, nil, nil).Rebuild(ctx, nil, nil)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Rebuild over a FIFO snapshot returned nil; a non-regular snapshot must fail the pass, never be accepted")
		}
		if !errors.Is(err, atomicfile.ErrNotRegular) {
			t.Errorf("error = %v, want it to wrap atomicfile.ErrNotRegular so the refusal is the inode-type gate rather than an incidental decode failure", err)
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("error = %q, want it to name the snapshot path %q", err, path)
		}
	case <-ctx.Done():
		t.Fatal("Rebuild BLOCKED on a FIFO snapshot: the confined read regressed to an ambient one, and a planted FIFO can wedge the compare pass while it holds the cycle lock")
	}
}

// TestLoadPreviousRefusesASnapshotSymlinkEscapingItsDirectory pins the other
// half of the confinement: the snapshot name is resolved INSIDE its own
// directory, so a symlink pointing out of it is refused rather than followed.
// Following it fed the target's bytes to the decoder, whose error message is
// logged (bounded), which is a small read-anything channel out of a directory
// the app is not meant to leave.
func TestLoadPreviousRefusesASnapshotSymlinkEscapingItsDirectory(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	const secret = "SUPER-SECRET-VALUE-THAT-MUST-NOT-BE-READ"
	if err := os.WriteFile(outside, []byte(secret), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	path := filepath.Join(dir, "feed.json")
	if err := os.Symlink(outside, path); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}

	err := NewFeedWriter(&FeedWriterConfig{Path: path}, nil, nil).Rebuild(t.Context(), nil, nil)
	if err == nil {
		t.Fatal("Rebuild over an escaping symlink returned nil, want a refusal")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the outside file's CONTENT reached the error path: %q", err)
	}
}

// TestPassPublishesToTheInProcessServer pins the daemon's primary handover: a
// completed pass hands its snapshot straight to the feed server running in the
// same process, so the feed is servable the moment the cycle finishes - no
// reload clock tick, no request-triggered read, and no dependency on the file
// being readable back. The published render must also be the same one a load
// produces: the persisted snapshot is GUID-only, so the download links have to be
// re-derived rather than carried from the pre-strip render.
func TestPassPublishesToTheInProcessServer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	ix := New(&Config{SnapshotPath: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api", ProwlarrAPIKey: "k"}}, nil, nil)
	w := NewFeedWriter(&FeedWriterConfig{
		Path:           path,
		Server:         ix,
		UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"},
	}, nil, nil)

	if err := w.Rebuild(t.Context(), nyaaTestEntries(1), nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	// No tick, no refresh: the server serves what the cycle handed it.
	items := ix.feedFor(upstreamNyaa)
	if len(items) != 1 {
		t.Fatalf("served feed after the pass = %d items, want 1 published in-process", len(items))
	}
	if items[0].DownloadURL == "" {
		t.Error("published item has no download URL; the load-path derivation must run on the published render too")
	}

	// The generation just written is recorded, so the reload clock recognizes the
	// published snapshot as current instead of re-reading the file this process
	// already holds.
	log, rec := capture.New()
	ix.cache.log = log
	tick(ix)
	if got := rec.Count("indexer feed snapshot loaded"); got != 0 {
		t.Errorf("reload clock re-read the file this process published (%d load lines); want the recorded identity to skip it:\n%s",
			got, strings.Join(rec.Messages(), "\n"))
	}
}

// TestPassWithoutAnInProcessServerOnlyWritesTheFile pins the out-of-process half
// of the same contract: the `poll` subcommand builds no server, so its pass has
// nothing to publish into and the file stays the whole channel - which is what
// the resident daemon's reload clock exists for.
func TestPassWithoutAnInProcessServerOnlyWritesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	ix := New(&Config{SnapshotPath: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api", ProwlarrAPIKey: "k"}}, nil, nil)

	if err := newTestWriter(path, "", false).Rebuild(t.Context(), nyaaTestEntries(1), nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if got := ix.feedFor(upstreamNyaa); len(got) != 0 {
		t.Fatalf("served feed = %d items with no in-process handover, want 0 until the reload clock runs", len(got))
	}
	tick(ix)
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 {
		t.Errorf("served feed after one tick = %d items, want the persisted snapshot loaded (1)", len(got))
	}
}

// TestDecodeSnapshotBoundsOneItemsInteriorArray pins the PER-ITEM byte bound,
// the one cardinality dimension neither the file's byte cap nor the per-array
// item cap can express: a single journal item's own Categories array is decoded
// by encoding/json, so one hand-edited item can amplify a few hundred KiB of
// document into a hundreds-of-MB int slice BEFORE validPersistedItem's
// maxPersistedCategories check can reject it - OOMing the 256 MiB container
// inside the warm-up load and crashlooping the compare loop with it (CWE-400).
// Without this assertion the bound can be widened or deleted with the whole
// suite still green: the over-long item is then pruned by the per-item gate, so
// decodeSnapshot reports no error and drops it silently, which is exactly what a
// caller-side check cannot distinguish from a clean load.
func TestDecodeSnapshotBoundsOneItemsInteriorArray(t *testing.T) {
	var it strings.Builder
	it.WriteString(`{"Key":"nyaa:1","FirstSeen":"2026-07-01T00:00:00Z","Categories":[`)
	for i := 0; it.Len() <= maxPersistedItemBytes; i++ {
		if i > 0 {
			it.WriteByte(',')
		}
		it.WriteString("5070")
	}
	it.WriteString(`]}`)
	doc := `{"version":2,"owners":{},"published":{},"ab_feed":[],"nyaa_feed":[` + it.String() + `]}`
	if len(doc) > maxFeedBytes {
		t.Fatalf("document = %d bytes, want it under the %d byte read cap (the premise: only the per-item bound can reject it)", len(doc), maxFeedBytes)
	}
	_, _, reason, err := decodeSnapshot([]byte(doc))
	if err == nil {
		t.Fatalf("decodeSnapshot accepted a %d-byte journal item (reason=%q), want the per-item byte bound to reject it before encoding/json allocates its Categories slice", it.Len(), reason)
	}
	if !strings.Contains(err.Error(), "item exceeds") {
		t.Errorf("error = %q, want the per-item byte-bound error", err)
	}
}

// TestDecodeSnapshotBoundsReleasesUnderOneOwnerKey pins the SECOND cardinality
// dimension of the ownership fact, which the per-owner-key charge cannot see:
// owners is a map of ARRAYS, so a million releases can hide inside ONE owner's
// list while the document carries a single key. A million owners with one
// release each and one owner with a million releases cost the same live heap, so
// a bound that only charges keys leaves the decode unbounded in the dimension a
// hand-edited or corrupted feed.json is cheapest to grow. Nothing else in the
// suite exercises a long release list: the aggregate-budget test gives every
// owner an EMPTY array precisely so only the outer keys are charged, so widening
// the decoder's element budget passes the whole suite today.
//
// The assertion is deliberately on the REFUSAL and not on which bound produced
// it: at the current sizes the decoder's own element budget answers first, and
// the point is that some bound must.
func TestDecodeSnapshotBoundsReleasesUnderOneOwnerKey(t *testing.T) {
	const releases = 2*maxSnapshotFeedItems + 1
	var b strings.Builder
	b.WriteString(`{"version":2,"published":{},"nyaa_feed":[],"ab_feed":[],"owners":{"1":[`)
	for i := range releases {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("{}")
	}
	b.WriteString(`]}}`)
	doc := b.String()
	if len(doc) > maxFeedBytes {
		t.Fatalf("document = %d bytes, want it under the %d byte read cap (the premise: only a cardinality bound can reject it)", len(doc), maxFeedBytes)
	}
	if _, _, reason, err := decodeSnapshot([]byte(doc)); err == nil {
		t.Fatalf("decodeSnapshot accepted %d releases under ONE owner key (reason=%q), want a bounded-decode error before the releases are allocated", releases, reason)
	}
}
