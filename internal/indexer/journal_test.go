package indexer

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/slogx/capture"
)

// newTestWriter builds a FeedWriter for path with no harvest upstreams (the
// common shape of the journal tests). Nyaa is always configured (a fake Nyaa
// Torznab URL, the tracker's on switch - without it the Nyaa journal is
// neither carried nor grown). abConfigured wires a fake AB Torznab URL
// (the tracker's on switch); abPasskey makes AB releases journalable (persisted
// GUID-only; the server derives the served links).
func newTestWriter(path, abPasskey string, abConfigured bool) *FeedWriter {
	cfg := &FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api", ABPasskey: abPasskey}}
	if abConfigured {
		cfg.ABTorznabURL = "http://prowlarr/2/api"
	}
	return NewFeedWriter(cfg, Deps{})
}

// emptyLedgerJSON is the minimal journal-schema snapshot (an empty but PRESENT
// seen ledger), the seed that bypasses the first-run baseline in tests.
const emptyLedgerJSON = `{"by_hash":{},"by_key":{},"seen":{},"nyaa_feed":[],"ab_feed":[]}`

// seedEmptyLedger writes a journal-schema snapshot with an EMPTY seen ledger
// at path, so the next Rebuild treats every curated torrent as newly curated -
// bypassing the first-run baseline (which would record everything as seen and
// serve an empty journal).
func seedEmptyLedger(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(emptyLedgerJSON), 0o600); err != nil {
		t.Fatalf("seed empty ledger: %v", err)
	}
}

// readSnapshotFile decodes the persisted snapshot for assertions.
func readSnapshotFile(t *testing.T, path string) snapshot {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	return snap
}

// writeSnapshotFile persists a hand-built snapshot for tests that seed journal
// state directly (titles, first-seen times).
func writeSnapshotFile(t *testing.T, path string, snap *snapshot) {
	t.Helper()
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
}

// nyaaEntry builds one single-torrent Nyaa SeaDex entry with the given AniList
// id, view id, and file names.
func nyaaEntry(alID, viewID int, best bool, names ...string) seadex.Entry {
	files := make([]seadex.File, 0, len(names))
	for _, n := range names {
		files = append(files, seadex.File{Name: n, Length: 1})
	}
	return seadex.Entry{
		AniListID: alID,
		Torrents: []seadex.Torrent{{
			Tracker: "Nyaa",
			URL:     "https://nyaa.si/view/" + strconv.Itoa(viewID),
			IsBest:  best,
			Files:   files,
		}},
	}
}

// TestRebuildBaselinesFreshInstall pins the first-run contract: with no
// previous snapshot the entire current curation set is recorded as seen and
// the journal is served EMPTY - the feed only grows from curation newer than
// the baseline (backfill is search's job) - while the search curation set is
// fully populated from the same catalogue.
func TestRebuildBaselinesFreshInstall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	entries := []seadex.Entry{nyaaEntry(7, 42, true, "Show - S01E01 (1080p) [G].mkv")}
	if err := newTestWriter(path, "", false).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 0 || len(snap.ABFeed) != 0 {
		t.Errorf("baseline feeds = nyaa %d / ab %d items, want both empty", len(snap.NyaaFeed), len(snap.ABFeed))
	}
	if !snap.Seen["nyaa:42"] {
		t.Errorf("seen ledger missing nyaa:42 after baseline: %v", snap.Seen)
	}
	if len(snap.ByKey) != 1 {
		t.Errorf("search curation keys = %d, want 1 (search must still cover the whole catalogue)", len(snap.ByKey))
	}

	// A second rebuild over the same catalogue stays empty: nothing is new.
	if err := newTestWriter(path, "", false).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("second Rebuild: %v", err)
	}
	if snap := readSnapshotFile(t, path); len(snap.NyaaFeed) != 0 {
		t.Errorf("feed after unchanged catalogue = %d items, want 0", len(snap.NyaaFeed))
	}
}

// TestRebuildBaselinesPreJournalSchema pins the schema migration: a previous
// snapshot without a seen ledger (the retired whole-catalogue window model) is
// treated as absent - the journal baselines empty even though the old snapshot
// carried feed items, and the old items never re-enter.
func TestRebuildBaselinesPreJournalSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	old := `{"by_hash":{},"by_key":{"nyaa:42":true},"nyaa_feed":[{"Title":"Show - S01 (1080p) [G]","GUID":"https://nyaa.si/view/42","DownloadURL":"https://nyaa.si/download/42.torrent"}],"ab_feed":[]}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatalf("write old-schema snapshot: %v", err)
	}
	entries := []seadex.Entry{nyaaEntry(7, 42, true, "Show - S01E01 (1080p) [G].mkv")}
	log, rec := capture.New()
	if err := NewFeedWriter(&FeedWriterConfig{Path: path}, Deps{Logger: log}).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 0 {
		t.Errorf("feed after old-schema migration = %d items, want 0 (baseline-empty)", len(snap.NyaaFeed))
	}
	if !snap.Seen["nyaa:42"] {
		t.Errorf("seen ledger missing the migrated catalogue: %v", snap.Seen)
	}
	if !rec.Contains("indexer feed journal baselined") {
		t.Errorf("baseline not logged; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestRebuildBaselinesMalformedSnapshot pins the corruption posture: a
// malformed previous snapshot warns and re-baselines (self-healing - the seen
// ledger rebuilds from the current catalogue) instead of failing the rebuild
// forever or silently seeding a bogus journal.
func TestRebuildBaselinesMalformedSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write malformed snapshot: %v", err)
	}
	log, rec := capture.New()
	entries := []seadex.Entry{nyaaEntry(7, 42, true, "Show - S01E01 (1080p) [G].mkv")}
	if err := NewFeedWriter(&FeedWriterConfig{Path: path}, Deps{Logger: log}).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if snap := readSnapshotFile(t, path); len(snap.NyaaFeed) != 0 || !snap.Seen["nyaa:42"] {
		t.Errorf("malformed snapshot did not re-baseline: feed=%d seen=%v", len(snap.NyaaFeed), snap.Seen)
	}
	if !rec.Contains("previous feed snapshot malformed; re-baselining the feed journal") {
		t.Errorf("malformed snapshot not warned; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestRebuildJournalsNewlyCurated pins the journal growth contract: a torrent
// newly present in the curation set (absent from the seen ledger) enters the
// feed ONCE with its first-seen timestamp (PubDate mirrors it), stays in the
// journal on following rebuilds with FirstSeen unchanged, and an item already
// baselined never enters.
func TestRebuildJournalsNewlyCurated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	w := newTestWriter(path, "", false)
	t0 := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return t0 }

	// Baseline over catalogue A.
	a := nyaaEntry(7, 42, true, "Show A - S01E01 (1080p) [G].mkv")
	if err := w.Rebuild(context.Background(), []seadex.Entry{a}, nil); err != nil {
		t.Fatalf("baseline Rebuild: %v", err)
	}

	// SeaDex curates B: only B enters the journal, stamped t1.
	t1 := t0.Add(3 * time.Hour)
	w.now = func() time.Time { return t1 }
	b := nyaaEntry(8, 43, true, "Show B - S01E01 (1080p) [G].mkv")
	if err := w.Rebuild(context.Background(), []seadex.Entry{a, b}, nil); err != nil {
		t.Fatalf("growth Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 1 {
		t.Fatalf("feed = %d items, want 1 (only the newly curated torrent)", len(snap.NyaaFeed))
	}
	got := snap.NyaaFeed[0]
	if got.Key != "nyaa:43" {
		t.Errorf("journaled key = %q, want nyaa:43", got.Key)
	}
	if !got.FirstSeen.Equal(t1) {
		t.Errorf("FirstSeen = %v, want %v", got.FirstSeen, t1)
	}
	if !got.PubDate.Equal(t1) {
		t.Errorf("PubDate = %v, want FirstSeen %v", got.PubDate, t1)
	}

	// A third rebuild over the same catalogue keeps B (it stays until pruned)
	// with its original FirstSeen, and adds nothing.
	t2 := t1.Add(3 * time.Hour)
	w.now = func() time.Time { return t2 }
	if err := w.Rebuild(context.Background(), []seadex.Entry{a, b}, nil); err != nil {
		t.Fatalf("steady-state Rebuild: %v", err)
	}
	snap = readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 1 {
		t.Fatalf("steady-state feed = %d items, want 1 (journal keeps the item until pruned)", len(snap.NyaaFeed))
	}
	if !snap.NyaaFeed[0].FirstSeen.Equal(t1) {
		t.Errorf("steady-state FirstSeen = %v, want the original %v", snap.NyaaFeed[0].FirstSeen, t1)
	}
}

// TestRebuildPrunesAgedItemsAndTitles pins the prune contract: an item older
// than feedJournalMaxAge leaves the journal AND drops its cached harvested
// title, while the seen ledger keeps its identity - so the pruned item can
// never re-enter the journal as new even though SeaDex still curates it.
func TestRebuildPrunesAgedItemsAndTitles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	w := newTestWriter(path, "", false)
	t0 := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return t0 }
	seedEmptyLedger(t, path)
	entries := []seadex.Entry{nyaaEntry(7, 42, true, "Show - S01E01 (1080p) [G].mkv")}
	if err := w.Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	// Hand-cache a harvested title for the journaled item, as a harvest would.
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 1 {
		t.Fatalf("feed = %d items, want 1", len(snap.NyaaFeed))
	}
	snap.Titles = map[string]string{"nyaa:42": "Show S01 1080p BluRay [G]"}
	writeSnapshotFile(t, path, &snap)

	// Within the window the cached title is served.
	t1 := t0.Add(24 * time.Hour)
	w.now = func() time.Time { return t1 }
	if err := w.Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("within-window Rebuild: %v", err)
	}
	snap = readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 1 || snap.NyaaFeed[0].Title != "Show S01 1080p BluRay [G]" {
		t.Fatalf("within-window feed = %+v, want the cached harvested title served", snap.NyaaFeed)
	}

	// Past the window the item ages out, its title cache entry goes with it,
	// and the seen ledger keeps the identity.
	t2 := t0.Add(feedJournalMaxAge + time.Hour)
	w.now = func() time.Time { return t2 }
	if err := w.Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("prune Rebuild: %v", err)
	}
	snap = readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 0 {
		t.Errorf("feed after prune = %d items, want 0", len(snap.NyaaFeed))
	}
	if len(snap.Titles) != 0 {
		t.Errorf("titles after prune = %v, want empty (the aged-out item drops its cached title)", snap.Titles)
	}
	if !snap.Seen["nyaa:42"] {
		t.Errorf("seen ledger lost the pruned identity: %v", snap.Seen)
	}

	// The torrent is still curated: it must never resurrect as new.
	t3 := t2.Add(3 * time.Hour)
	w.now = func() time.Time { return t3 }
	if err := w.Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("post-prune Rebuild: %v", err)
	}
	if snap := readSnapshotFile(t, path); len(snap.NyaaFeed) != 0 {
		t.Errorf("feed after post-prune rebuild = %d items, want 0 (pruned items never re-enter)", len(snap.NyaaFeed))
	}
}

// TestRebuildSharedTorrentMergesBestWins pins the shared-torrent fold: a
// torrent attached to two SeaDex entries (same tracker key) journals as ONE
// item with best-wins on the marker and the categories of both entries
// unioned - the alt entry is listed first, so a first-wins fold would fail the
// marker assertion.
func TestRebuildSharedTorrentMergesBestWins(t *testing.T) {
	shared := seadex.Torrent{
		Tracker: "Nyaa", URL: "https://nyaa.si/view/1234567",
		Files: []seadex.File{{Length: 7, Name: "Show - S01E01 (1080p) [G].mkv"}},
	}
	alt := shared
	best := shared
	best.IsBest = true
	entries := []seadex.Entry{
		{AniListID: 1, Torrents: []seadex.Torrent{alt}},
		{AniListID: 2, Torrents: []seadex.Torrent{best}},
	}
	info := func(alID int) EntryInfo {
		return EntryInfo{IsMovie: alID == 2}
	}
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	if err := newTestWriter(path, "", false).Rebuild(context.Background(), entries, info); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 1 {
		t.Fatalf("feed = %d items, want 1 (shared torrent merged)", len(snap.NyaaFeed))
	}
	got := snap.NyaaFeed[0]
	if got.DownloadVolumeFactor != dvfBest {
		t.Errorf("marker = %q, want %q (best-wins even when the alt entry is listed first)", got.DownloadVolumeFactor, dvfBest)
	}
	if len(got.Categories) != 2 {
		t.Errorf("categories = %v, want the union of both entries' categories", got.Categories)
	}
}

// TestRenderJournalItemDeterministicSynthesisSource pins the synthesis-source
// selection for a torrent shared by several SeaDex entries: the rendered
// item's identity fields (AniListID, InfoURL) must come from the LOWEST
// AniList id, regardless of the untrusted upstream catalogue order - a
// first-wins fold would flip the served InfoURL and AniListID between
// rebuilds whenever the catalogue order changes, and AniListID also drives
// harvest grouping.
func TestRenderJournalItemDeterministicSynthesisSource(t *testing.T) {
	w := newTestWriter(filepath.Join(t.TempDir(), "feed.json"), "", false)
	torrent := seadex.Torrent{
		Tracker: "Nyaa", URL: "https://nyaa.si/view/1234567",
		Files: []seadex.File{{Length: 7, Name: "Show - S01E01 (1080p) [G].mkv"}},
	}
	e1 := &seadex.Entry{AniListID: 1, Torrents: []seadex.Torrent{torrent}}
	e2 := &seadex.Entry{AniListID: 2, Torrents: []seadex.Torrent{torrent}}
	info := func(int) EntryInfo { return EntryInfo{} }
	orders := map[string][]curatedRef{
		"lowest id first": {
			{entry: e1, torrent: &e1.Torrents[0]},
			{entry: e2, torrent: &e2.Torrents[0]},
		},
		"lowest id second": {
			{entry: e2, torrent: &e2.Torrents[0]},
			{entry: e1, torrent: &e1.Torrents[0]},
		},
	}
	for name, refs := range orders {
		t.Run(name, func(t *testing.T) {
			it, ok, _ := w.renderJournalItem("nyaa:1234567", refs, info)
			if !ok {
				t.Fatal("renderJournalItem: item not rendered")
			}
			if it.AniListID != 1 {
				t.Errorf("AniListID = %d, want 1 (synthesis source must be the lowest AniList id, not the first occurrence)", it.AniListID)
			}
			if !strings.HasSuffix(it.InfoURL, "/1") {
				t.Errorf("InfoURL = %q, want the lowest AniList id's releases.moe/1 link", it.InfoURL)
			}
		})
	}
}

// TestRebuildDistinctEmptyGUIDItemsStayDistinct pins the journal identity key:
// two DISTINCT Nyaa torrents whose SeaDex URLs sit on a foreign host resolve
// canonical download links (nyaaID reads /view/{id} without host validation)
// while UsableURL rejects the host and leaves their stored GUIDs empty - they
// must journal as two items keyed nyaa:111 / nyaa:222, never merge on the
// empty GUID.
func TestRebuildDistinctEmptyGUIDItemsStayDistinct(t *testing.T) {
	entries := []seadex.Entry{{
		AniListID: 9,
		Torrents: []seadex.Torrent{
			{
				Tracker: "Nyaa", URL: "https://evil.example/view/111", IsBest: true,
				Files: []seadex.File{{Length: 1, Name: "Show A - S01E01 (1080p) [G].mkv"}},
			},
			{
				Tracker: "Nyaa", URL: "https://evil.example/view/222",
				Files: []seadex.File{{Length: 1, Name: "Show B - S01E01 (1080p) [G].mkv"}},
			},
		},
	}}
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	if err := newTestWriter(path, "", false).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 2 {
		t.Fatalf("feed = %d items, want 2 (distinct empty-GUID torrents must not merge)", len(snap.NyaaFeed))
	}
	for i := range snap.NyaaFeed {
		if snap.NyaaFeed[i].GUID != "" {
			t.Errorf("item %d stored GUID = %q, want empty (UsableURL must reject the foreign host)", i, snap.NyaaFeed[i].GUID)
		}
	}
	if snap.NyaaFeed[0].DownloadURL == snap.NyaaFeed[1].DownloadURL {
		t.Errorf("both items share download URL %q, want distinct canonical links", snap.NyaaFeed[0].DownloadURL)
	}
}

// TestRebuildDropsUnknownTracker pins the tail-drop: a SeaDex torrent on a
// tracker other than Nyaa/AB (the negligible AnimeTosho/RuTracker tail) never
// enters a journal feed and does not trigger the AB passkey nudge.
func TestRebuildDropsUnknownTracker(t *testing.T) {
	entries := []seadex.Entry{{
		AniListID: 5,
		Torrents: []seadex.Torrent{{
			Tracker: "AnimeTosho", URL: "https://animetosho.org/view/1", IsBest: true,
			Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
		}},
	}}
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	log, rec := capture.New()
	if err := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{ABPasskey: "PK", ABTorznabURL: "http://prowlarr/2/api"}}, Deps{Logger: log}).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 0 || len(snap.ABFeed) != 0 {
		t.Errorf("unknown tracker leaked into a feed: nyaa=%d ab=%d, want 0 and 0", len(snap.NyaaFeed), len(snap.ABFeed))
	}
	if rec.Contains("ab RSS feed empty of grabbable links") {
		t.Errorf("unknown tracker triggered the AB passkey nudge; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestRebuildIdlessABNotCountedAsPasskeySkip pins the precision of the
// missing-passkey nudge: an AnimeBytes release whose URL carries no parseable
// torrent id is un-grabbable regardless of the passkey, so it is excluded from
// the journal WITHOUT triggering the nudge - the operator warning must only
// count releases a passkey would actually make grabbable.
func TestRebuildIdlessABNotCountedAsPasskeySkip(t *testing.T) {
	entries := []seadex.Entry{{
		AniListID: 5,
		Torrents: []seadex.Torrent{{
			Tracker: "AB", URL: "/torrents.php?id=1", IsBest: true,
			InfoHash: "aa" + strings.Repeat("b", 38),
			Files:    []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
		}},
	}}
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	log, rec := capture.New()
	if err := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{ABTorznabURL: "http://prowlarr/2/api"}}, Deps{Logger: log}).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.ABFeed) != 0 {
		t.Errorf("id-less AB release leaked into the feed: %d items, want 0", len(snap.ABFeed))
	}
	if rec.Contains("ab RSS feed empty of grabbable links") {
		t.Errorf("id-less AB release counted as a passkey skip; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestRebuildUnpackedSeasonListsPerEpisode pins the per-episode listing at the
// journal level: a season SeaDex tracks as one torrent PER episode (each a
// single-file release) journals one item per episode, each keeping its SxxExx
// title - never collapsed to the season (which would let the arr grab a single
// episode believing it was the whole season) and never merged.
func TestRebuildUnpackedSeasonListsPerEpisode(t *testing.T) {
	entries := []seadex.Entry{{
		AniListID: 187989,
		Torrents: []seadex.Torrent{
			{Tracker: "Nyaa", URL: "https://nyaa.si/view/1", IsBest: true, Files: []seadex.File{{Length: 1, Name: "Scum of the Brave - S01E01 (WEB 1080p) [G].mkv"}}},
			{Tracker: "Nyaa", URL: "https://nyaa.si/view/2", IsBest: true, Files: []seadex.File{{Length: 1, Name: "Scum of the Brave - S01E02 (WEB 1080p) [G].mkv"}}},
			{Tracker: "Nyaa", URL: "https://nyaa.si/view/3", IsBest: true, Files: []seadex.File{{Length: 1, Name: "Scum of the Brave - S01E03 (WEB 1080p) [G].mkv"}}},
		},
	}}
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	if err := newTestWriter(path, "", false).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 3 {
		t.Fatalf("feed = %d items, want 3 (one per episode torrent, not collapsed/deduped)", len(snap.NyaaFeed))
	}
	titles := map[string]bool{}
	for i := range snap.NyaaFeed {
		titles[snap.NyaaFeed[i].Title] = true
	}
	for _, want := range []string{
		"Scum of the Brave - S01E01 (WEB 1080p) [G]",
		"Scum of the Brave - S01E02 (WEB 1080p) [G]",
		"Scum of the Brave - S01E03 (WEB 1080p) [G]",
	} {
		if !titles[want] {
			t.Errorf("missing per-episode title %q; got %v", want, titles)
		}
	}
}

// TestRebuildJournalItemShape pins the journaled item fields on the real
// Frieren catalogue shape (PMR best + LostYears alt, each on Nyaa and AB):
// tracker split, per-tracker download links (public Nyaa .torrent persisted;
// AB persisted GUID-only, its passkey link derived by the reader), best/alt
// markers, the dropped redacted AB info hash, the SeaDex entry info URL, the
// summed pack size, the synthesized title from the show metadata, and PubDate
// mirroring FirstSeen (not the SeaDex entry update).
func TestRebuildJournalItemShape(t *testing.T) {
	updated := time.Date(2025, 7, 26, 15, 5, 59, 0, time.UTC)
	pmrFiles := []seadex.File{
		{Length: 400_000_000, Name: "NCED 01 (BD Remux 1080p AVC FLAC) [PMR].mkv"},
		{Length: 7_500_699_108, Name: "Frieren Beyond Journey's End - S01E01 (BD Remux 1080p AVC FLAC AAC) [Dual Audio] [PMR].mkv"},
		{Length: 7_497_267_058, Name: "Frieren Beyond Journey's End - S01E02 (BD Remux 1080p AVC FLAC AAC) [Dual Audio] [PMR].mkv"},
	}
	entries := []seadex.Entry{{
		AniListID: 154587,
		Updated:   updated,
		Torrents: []seadex.Torrent{
			{Tracker: "Nyaa", URL: "https://nyaa.si/view/1961373", InfoHash: "143ed15e5e3df072ae91adaeb149973a887590dd", IsBest: true, ReleaseGroup: "PMR", DualAudio: true, Files: pmrFiles},
			{Tracker: "AB", URL: "/torrents.php?id=86576&torrentid=1167293", InfoHash: "<redacted>", IsBest: true, ReleaseGroup: "PMR", DualAudio: true, Files: pmrFiles},
			{Tracker: "AB", URL: "/torrents.php?id=86576&torrentid=1162986", InfoHash: "<redacted>", IsBest: false, ReleaseGroup: "LostYears", Files: pmrFiles},
		},
	}}
	info := func(alID int) EntryInfo {
		if alID != 154587 {
			t.Errorf("info called with alID %d, want 154587", alID)
		}
		return EntryInfo{Title: "Frieren: Beyond Journey's End", SeasonTvdb: 1}
	}
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api", ABPasskey: "PASSKEY123", ABTorznabURL: "http://prowlarr/2/api"}}, Deps{})
	now := time.Date(2026, time.July, 2, 9, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return now }
	if err := w.Rebuild(context.Background(), entries, info); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 1 || len(snap.ABFeed) != 2 {
		t.Fatalf("feeds: nyaa=%d ab=%d, want 1 and 2", len(snap.NyaaFeed), len(snap.ABFeed))
	}

	pmrNyaa := snap.NyaaFeed[0]
	if want := "Frieren: Beyond Journey's End S01 1080p Dual Audio [PMR]"; pmrNyaa.Title != want {
		t.Errorf("PMR nyaa title = %q, want %q", pmrNyaa.Title, want)
	}
	if pmrNyaa.DownloadURL != "https://nyaa.si/download/1961373.torrent" {
		t.Errorf("PMR nyaa download = %q", pmrNyaa.DownloadURL)
	}
	if pmrNyaa.DownloadVolumeFactor != dvfBest {
		t.Errorf("PMR nyaa dvf = %q, want %q", pmrNyaa.DownloadVolumeFactor, dvfBest)
	}
	if pmrNyaa.InfoHash != "143ed15e5e3df072ae91adaeb149973a887590dd" {
		t.Errorf("PMR nyaa infohash = %q", pmrNyaa.InfoHash)
	}
	if len(pmrNyaa.Categories) != 1 || pmrNyaa.Categories[0] != catAnime {
		t.Errorf("PMR nyaa categories = %v, want [%d]", pmrNyaa.Categories, catAnime)
	}
	if pmrNyaa.InfoURL != "https://releases.moe/154587" {
		t.Errorf("PMR nyaa infoURL = %q", pmrNyaa.InfoURL)
	}
	if pmrNyaa.Size != 400_000_000+7_500_699_108+7_497_267_058 {
		t.Errorf("PMR nyaa size = %d, want summed pack size", pmrNyaa.Size)
	}
	if !pmrNyaa.PubDate.Equal(now) {
		t.Errorf("PMR nyaa pubDate = %v, want the journal first-seen %v (not the SeaDex entry update)", pmrNyaa.PubDate, now)
	}
	if pmrNyaa.Key != "nyaa:1961373" || pmrNyaa.AniListID != 154587 {
		t.Errorf("journal bookkeeping = key %q / alID %d, want nyaa:1961373 / 154587", pmrNyaa.Key, pmrNyaa.AniListID)
	}

	byKey := map[string]item{}
	for i := range snap.ABFeed {
		byKey[snap.ABFeed[i].Key] = snap.ABFeed[i]
	}
	pmrAB, ok := byKey["ab:1167293"]
	if !ok {
		t.Fatal("PMR ab item missing")
	}
	if pmrAB.DownloadURL != "" {
		t.Errorf("PMR ab persisted download = %q, want empty (GUID-only; the reader derives the passkey link)", pmrAB.DownloadURL)
	}
	if pmrAB.InfoHash != "" {
		t.Errorf("PMR ab infohash = %q, want empty (redacted dropped)", pmrAB.InfoHash)
	}
	if pmrAB.GUID != "https://animebytes.tv/torrents.php?id=86576&torrentid=1167293" {
		t.Errorf("PMR ab guid = %q, want the usable AB page URL", pmrAB.GUID)
	}
	lyAB, ok := byKey["ab:1162986"]
	if !ok {
		t.Fatal("LostYears ab item missing")
	}
	if lyAB.DownloadVolumeFactor != dvfAlt {
		t.Errorf("LostYears ab dvf = %q, want %q (alt)", lyAB.DownloadVolumeFactor, dvfAlt)
	}
}

// TestCategoriesFor verifies the RSS category comes from the entry's real
// media typing, not a guess from the file name: a movie routes to Radarr
// (Movies) and everything else to Sonarr (Anime) - a single-file OVA/special
// is indistinguishable from a film by name, so the safe default matters.
func TestCategoriesFor(t *testing.T) {
	if got := categoriesFor(true); len(got) != 1 || got[0] != catMovies {
		t.Errorf("categoriesFor(movie) = %v, want [%d]", got, catMovies)
	}
	if got := categoriesFor(false); len(got) != 1 || got[0] != catAnime {
		t.Errorf("categoriesFor(series) = %v, want [%d]", got, catAnime)
	}
}

// TestRebuildCarriesUncuratedItemStoredRender pins the carry contract for a
// curated-then-replaced torrent: a journaled item whose torrent has LEFT the
// current curation set keeps its stored render verbatim (title, download URL,
// FirstSeen) - it is still a valid release the arrs may grab - instead of
// being re-rendered or dropped.
func TestRebuildCarriesUncuratedItemStoredRender(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	first := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	writeSnapshotFile(t, path, &snapshot{
		ByHash: map[string]bool{},
		ByKey:  map[string]bool{},
		Seen:   map[string]bool{"nyaa:42": true},
		NyaaFeed: []item{{
			Title: "Stored Show - S01 (1080p) [G]", GUID: "https://nyaa.si/view/42",
			DownloadURL: "https://nyaa.si/download/42.torrent",
			Key:         "nyaa:42", AniListID: 7,
			FirstSeen: first, PubDate: first,
		}},
	})
	if err := newTestWriter(path, "", false).Rebuild(context.Background(), nil, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 1 {
		t.Fatalf("feed = %d items, want 1 (a curated-then-replaced torrent keeps its stored render)", len(snap.NyaaFeed))
	}
	got := snap.NyaaFeed[0]
	if got.Title != "Stored Show - S01 (1080p) [G]" || got.DownloadURL != "https://nyaa.si/download/42.torrent" {
		t.Errorf("carried item = %+v, want the stored render unchanged", got)
	}
	if !got.FirstSeen.Equal(first) {
		t.Errorf("FirstSeen = %v, want the original %v", got.FirstSeen, first)
	}
}

// TestRebuildDropsCarriedABItemWhenPasskeyRemoved pins the carry-side passkey
// gate: a previously journaled AnimeBytes item whose download link can no
// longer be built (the operator removed ab_passkey while the item was in the
// journal window) is dropped from the persisted AB feed - it is no longer
// grabbable, so carrying it would be dead weight.
func TestRebuildDropsCarriedABItemWhenPasskeyRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	first := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	writeSnapshotFile(t, path, &snapshot{
		ByHash: map[string]bool{},
		ByKey:  map[string]bool{},
		Seen:   map[string]bool{"ab:1167293": true},
		ABFeed: []item{{
			Title: "Frieren - S01 (BD Remux 1080p) [PMR]",
			GUID:  "https://animebytes.tv/torrents.php?id=86576&torrentid=1167293",
			Key:   "ab:1167293", AniListID: 154587,
			FirstSeen: first, PubDate: first,
		}},
	})
	entries := []seadex.Entry{{
		AniListID: 154587,
		Torrents: []seadex.Torrent{{
			Tracker: "AB", URL: "/torrents.php?id=86576&torrentid=1167293", InfoHash: "<redacted>",
			IsBest: true, ReleaseGroup: "PMR",
			Files: []seadex.File{{Length: 1, Name: "Frieren - S01E01 (BD Remux 1080p) [PMR].mkv"}},
		}},
	}}
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{ABTorznabURL: "http://prowlarr/2/api"}}, Deps{})
	if err := w.Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if snap := readSnapshotFile(t, path); len(snap.ABFeed) != 0 {
		t.Errorf("ab feed = %+v, want empty (no passkey, the carried item is no longer grabbable)", snap.ABFeed)
	}
}

// TestRebuildDropsCarriedNonCuratedABItemWhenPasskeyRemoved pins the sibling
// arm of the carry-side passkey gate: a previously journaled AnimeBytes item
// whose torrent has LEFT the curation set is subject to the same documented
// passkey drop as a still-curated one - with ab_passkey removed its download
// link can no longer be built either, so keeping its stored render would make
// the post-restore AB feed an arbitrary subset (curated carries dropped,
// non-curated carries kept).
func TestRebuildDropsCarriedNonCuratedABItemWhenPasskeyRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	first := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	writeSnapshotFile(t, path, &snapshot{
		ByHash: map[string]bool{},
		ByKey:  map[string]bool{},
		Seen:   map[string]bool{"ab:1167293": true},
		ABFeed: []item{{
			Title: "Frieren - S01 (BD Remux 1080p) [PMR]",
			GUID:  "https://animebytes.tv/torrents.php?id=86576&torrentid=1167293",
			Key:   "ab:1167293", AniListID: 154587,
			FirstSeen: first, PubDate: first,
		}},
	})
	// No entries: the carried item's torrent is absent from the curation set,
	// exercising the non-curated carry arm.
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{ABTorznabURL: "http://prowlarr/2/api"}}, Deps{})
	if err := w.Rebuild(context.Background(), nil, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if snap := readSnapshotFile(t, path); len(snap.ABFeed) != 0 {
		t.Errorf("ab feed = %+v, want empty (no passkey, the carried non-curated item is no longer grabbable)", snap.ABFeed)
	}
}

// TestRebuildRebasesFutureFirstSeenCarriedItem pins the clock-rollback guard:
// a carried item whose FirstSeen is AHEAD of the wall clock (a clock rollback,
// or a snapshot restored from a future-skewed host) is kept but rebased to
// now - FirstSeen and PubDate move to the current rebuild time, bounding its
// remaining journal lifetime to feedJournalMaxAge instead of letting the
// negative age hold it in RSS until the clock catches up plus 14 days - and
// the rebase is counted on the snapshot log line.
func TestRebuildRebasesFutureFirstSeenCarriedItem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	log, rec := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{Logger: log})
	t0 := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return t0 }
	future := t0.Add(72 * time.Hour)
	writeSnapshotFile(t, path, &snapshot{
		ByHash: map[string]bool{},
		ByKey:  map[string]bool{},
		Seen:   map[string]bool{"nyaa:42": true},
		NyaaFeed: []item{{
			Title: "Show - S01 (1080p) [G]", GUID: "https://nyaa.si/view/42",
			DownloadURL: "https://nyaa.si/download/42.torrent",
			Key:         "nyaa:42", AniListID: 7,
			FirstSeen: future, PubDate: future,
		}},
	})
	if err := w.Rebuild(context.Background(), nil, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 1 {
		t.Fatalf("feed = %d items, want 1 (the future-stamped item survives the clock correction)", len(snap.NyaaFeed))
	}
	if !snap.NyaaFeed[0].FirstSeen.Equal(t0) || !snap.NyaaFeed[0].PubDate.Equal(t0) {
		t.Errorf("rebased FirstSeen/PubDate = %v/%v, want both %v", snap.NyaaFeed[0].FirstSeen, snap.NyaaFeed[0].PubDate, t0)
	}
	rebased := int64(-1)
	for _, r := range rec.Records() {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "journal_clock_rebased" {
				rebased = a.Value.Int64()
			}
			return true
		})
	}
	if rebased != 1 {
		t.Errorf("journal_clock_rebased = %d, want 1; log:\n%s", rebased, strings.Join(rec.Messages(), "\n"))
	}
}

// TestRebuildDropsKeylessCarriedItem pins carryJournal's defensive drop of a
// pre-journal item (no Key / no FirstSeen, e.g. a hand-edited snapshot that
// kept the seen ledger but stripped an item's bookkeeping): it leaves the
// journal instead of being carried forever un-prunable, and each guard arm is
// counted as a genuine drop on the snapshot log line.
func TestRebuildDropsKeylessCarriedItem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	const seeded = `{"by_hash":{},"by_key":{},"seen":{},"nyaa_feed":[{"Title":"orphan","GUID":"https://nyaa.si/view/9"},{"Title":"no first seen","GUID":"https://nyaa.si/view/10","Key":"nyaa:10"}],"ab_feed":[]}`
	if err := os.WriteFile(path, []byte(seeded), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	log, rec := capture.New()
	if err := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{Logger: log}).Rebuild(context.Background(), nil, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if snap := readSnapshotFile(t, path); len(snap.NyaaFeed) != 0 {
		t.Errorf("feed = %+v, want empty (a keyless pre-journal item cannot be carried: it could never be pruned or re-rendered)", snap.NyaaFeed)
	}
	dropped := int64(-1)
	for _, r := range rec.Records() {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "journal_dropped" {
				dropped = a.Value.Int64()
			}
			return true
		})
	}
	if dropped != 2 {
		t.Errorf("journal_dropped = %d, want 2 (one per defensive-guard arm: no Key, no FirstSeen); log:\n%s", dropped, strings.Join(rec.Messages(), "\n"))
	}
}

// TestRebuildSkipsTitlelessTorrentAsUnresolvable pins the unresolvable
// accounting: a newly curated torrent with a parseable tracker key but no
// files and no release group synthesizes no title at all, so it is excluded
// from the journal (an arr cannot parse a title-less item), counted on the
// snapshot log line as skipped_unresolvable (the signal that an upstream
// data-shape change is shrinking the feed), and its identity still enters the
// seen ledger so it can never later re-enter as new.
func TestRebuildSkipsTitlelessTorrentAsUnresolvable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	entries := []seadex.Entry{{
		AniListID: 7,
		Torrents:  []seadex.Torrent{{Tracker: "Nyaa", URL: "https://nyaa.si/view/7", IsBest: true}},
	}}
	log, rec := capture.New()
	if err := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{Logger: log}).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 0 {
		t.Errorf("feed = %+v, want empty (a title-less item cannot be parsed by an arr)", snap.NyaaFeed)
	}
	if !snap.Seen["nyaa:7"] {
		t.Errorf("seen ledger missing nyaa:7: %v", snap.Seen)
	}
	unresolvable := int64(-1)
	for _, r := range rec.Records() {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "skipped_unresolvable" {
				unresolvable = a.Value.Int64()
			}
			return true
		})
	}
	if unresolvable != 1 {
		t.Errorf("skipped_unresolvable = %d, want 1; log:\n%s", unresolvable, strings.Join(rec.Messages(), "\n"))
	}
}

// TestRebuildCountsIdentitylessABTorrentAsUnresolvable pins journalIfNew's
// no-identity accounting: an enabled AnimeBytes torrent whose info hash is
// redacted (AB always redacts) and whose URL shape is unrecognized carries no
// identity signal at all - the exact shape an upstream AB URL change produces
// - so the rebuild must report it as skipped_unresolvable on the snapshot log
// line instead of silently losing the release from both the RSS journal and
// search curation. An intentionally disabled AB scope (no ab_torznab_url)
// stays silent: the operator opted out, so the loss is not a fault signal.
func TestRebuildCountsIdentitylessABTorrentAsUnresolvable(t *testing.T) {
	entries := []seadex.Entry{{
		AniListID: 5,
		Torrents: []seadex.Torrent{{
			Tracker: "AB", URL: "/details.php?torrent=1167293", InfoHash: "<redacted>", IsBest: true,
			Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
		}},
	}}
	tests := map[string]struct {
		cfg  UpstreamConfig
		want int64
	}{
		"enabled AB counts the loss":  {cfg: UpstreamConfig{ABPasskey: "PK", ABTorznabURL: "http://prowlarr/2/api"}, want: 1},
		"disabled AB scope is silent": {cfg: UpstreamConfig{}, want: 0},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "feed.json")
			seedEmptyLedger(t, path)
			log, rec := capture.New()
			if err := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: tc.cfg}, Deps{Logger: log}).Rebuild(context.Background(), entries, nil); err != nil {
				t.Fatalf("Rebuild: %v", err)
			}
			if snap := readSnapshotFile(t, path); len(snap.ABFeed) != 0 {
				t.Errorf("identity-less AB release leaked into the feed: %d items, want 0", len(snap.ABFeed))
			}
			unresolvable := int64(-1)
			for _, r := range rec.Records() {
				r.Attrs(func(a slog.Attr) bool {
					if a.Key == "skipped_unresolvable" {
						unresolvable = a.Value.Int64()
					}
					return true
				})
			}
			if unresolvable != tc.want {
				t.Errorf("skipped_unresolvable = %d, want %d; log:\n%s", unresolvable, tc.want, strings.Join(rec.Messages(), "\n"))
			}
		})
	}
}

// TestRebuildUnknownTrackerWithHashSilentlyIgnored pins newJournalItem's
// tail-tracker branch for a torrent that DOES carry a stable identity: an
// AnimeTosho/RuTracker release with a valid info hash reaches the journal
// logic (unlike an id-less tail torrent, which has no identity at all), is
// silently ignored - never counted unresolvable, since the tail is expected -
// and its hash is still folded into the seen ledger.
func TestRebuildUnknownTrackerWithHashSilentlyIgnored(t *testing.T) {
	const hash = "143ed15e5e3df072ae91adaeb149973a887590dd"
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	entries := []seadex.Entry{{
		AniListID: 5,
		Torrents: []seadex.Torrent{{
			Tracker: "AnimeTosho", URL: "https://animetosho.org/view/1", InfoHash: hash, IsBest: true,
			Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
		}},
	}}
	log, rec := capture.New()
	if err := NewFeedWriter(&FeedWriterConfig{Path: path}, Deps{Logger: log}).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 0 || len(snap.ABFeed) != 0 {
		t.Errorf("unknown tracker leaked into a feed: nyaa=%d ab=%d", len(snap.NyaaFeed), len(snap.ABFeed))
	}
	if !snap.Seen[hash] {
		t.Errorf("seen ledger missing the hash identity: %v", snap.Seen)
	}
	unresolvable := int64(-1)
	for _, r := range rec.Records() {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "skipped_unresolvable" {
				unresolvable = a.Value.Int64()
			}
			return true
		})
	}
	if unresolvable != 0 {
		t.Errorf("skipped_unresolvable = %d, want 0 (the tail is silently ignored, not an upstream fault signal)", unresolvable)
	}
}

// TestRebuildDropsCarriedItemBecomingUnresolvable pins carryJournal's drop
// accounting for a still-curated item that can no longer render: a journaled
// torrent whose current SeaDex record has lost its files and release group
// synthesizes no title, so the carried item is dropped as a genuine drop -
// counted as journal_dropped on the snapshot log line, never as an AB passkey
// skip - while the seen ledger keeps its identity so it can never re-enter.
func TestRebuildDropsCarriedItemBecomingUnresolvable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	first := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	writeSnapshotFile(t, path, &snapshot{
		ByHash: map[string]bool{},
		ByKey:  map[string]bool{"nyaa:42": true},
		Seen:   map[string]bool{"nyaa:42": true},
		NyaaFeed: []item{{
			Title: "Show - S01 (1080p) [G]", GUID: "https://nyaa.si/view/42",
			DownloadURL: "https://nyaa.si/download/42.torrent",
			Key:         "nyaa:42", AniListID: 7,
			FirstSeen: first, PubDate: first,
		}},
	})
	entries := []seadex.Entry{{
		AniListID: 7,
		Torrents:  []seadex.Torrent{{Tracker: "Nyaa", URL: "https://nyaa.si/view/42", IsBest: true}},
	}}
	log, rec := capture.New()
	if err := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{Logger: log}).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 0 {
		t.Errorf("nyaa feed = %+v, want empty (the carried item can no longer render a title)", snap.NyaaFeed)
	}
	if !snap.Seen["nyaa:42"] {
		t.Errorf("seen ledger lost the dropped identity: %v", snap.Seen)
	}
	dropped := int64(-1)
	for _, r := range rec.Records() {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "journal_dropped" {
				dropped = a.Value.Int64()
			}
			return true
		})
	}
	if dropped != 1 {
		t.Errorf("journal_dropped = %d, want 1; log:\n%s", dropped, strings.Join(rec.Messages(), "\n"))
	}
	if rec.Contains("ab RSS feed empty of grabbable links") {
		t.Errorf("the genuine drop was counted as an AB passkey skip; log:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestRenderJournalItemNoOccurrencesRejected pins renderJournalItem's
// defensive empty-refs guard: a journal key with no curated occurrences
// renders no item (ok=false) and never counts as an AB passkey skip, so an
// inconsistent or hand-edited snapshot can never materialize a bogus feed
// item. Unreachable through Rebuild today (carryJournal and growJournal only
// pass curated occurrences), so it is pinned by direct call.
func TestRenderJournalItemNoOccurrencesRejected(t *testing.T) {
	w := newTestWriter(filepath.Join(t.TempDir(), "feed.json"), "", false)
	it, ok, noPasskey := w.renderJournalItem("nyaa:1", nil, func(int) EntryInfo { return EntryInfo{} })
	if ok || noPasskey {
		t.Errorf("renderJournalItem(no refs) = (ok=%v, noPasskey=%v), want (false, false)", ok, noPasskey)
	}
	if it.Key != "" || it.Title != "" || it.DownloadURL != "" {
		t.Errorf("renderJournalItem(no refs) item = %+v, want the zero item", it)
	}
}

// TestRebuildDropsCarriedItemWarnedByStoredHashOnly pins carryItem's
// stored-hash branch (warnedSet.retracts via ws.ids[it.InfoHash]) in isolation: the carried
// nyaa:99 item has NO current occurrence in the catalogue (so its key never
// enters the widened carry-drop key set), but its stored info hash matches a
// Broken torrent journaled under a DIFFERENT key (nyaa:41). The carried item
// must still be retracted through the stored hash - deleting the
// warnedSet.retracts' stored-hash branch would leave it serving warned bytes on RSS -
// and the drop is counted on the snapshot log line.
func TestRebuildDropsCarriedItemWarnedByStoredHashOnly(t *testing.T) {
	log, rec := capture.New()
	path := filepath.Join(t.TempDir(), "feed.json")
	hash := strings.Repeat("a", 40)
	writeSnapshotFile(t, path, &snapshot{
		ByHash: map[string]bool{},
		ByKey:  map[string]bool{"nyaa:99": true},
		Seen:   map[string]bool{"nyaa:99": true},
		NyaaFeed: []item{{
			Title: "Show - S01 (1080p) [W]", GUID: "https://nyaa.si/view/99",
			DownloadURL: "https://nyaa.si/download/99.torrent",
			Key:         "nyaa:99", InfoHash: hash, AniListID: 8,
			FirstSeen: time.Now().UTC(), PubDate: time.Now().UTC(),
		}},
	})
	entries := []seadex.Entry{{
		AniListID: 7,
		Torrents: []seadex.Torrent{{
			Tracker: "Nyaa", URL: "https://nyaa.si/view/41", IsBest: true,
			InfoHash: hash,
			Tags:     []string{"Broken"},
			Files:    []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [W].mkv"}},
		}},
	}}
	if err := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, Deps{Logger: log}).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 0 {
		t.Errorf("nyaa feed = %+v, want empty (the carried item's stored hash is warned under a different key)", snap.NyaaFeed)
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
		t.Errorf("snapshot log line journal_warned_dropped = %d, want 1 (the hash-retracted carried item); log output:\n%s", warnedDropped, strings.Join(rec.Messages(), "\n"))
	}
}

// TestRebuildHashVetoesNoveltyAcrossKeyChange pins the multi-signal novelty
// contract identitySignals documents ("novelty detection survives one signal
// going missing - a URL-shape change upstream"): a torrent whose info hash is
// already in the seen ledger must NOT re-enter the journal as new when its
// tracker URL changes shape (a new /view id, i.e. a new journal key). Novelty
// is judged across ALL identity signals, so a re-upload or upstream URL change
// keeping the same bytes never re-broadcasts old curation, while both the new
// key and the hash fold into the seen ledger.
func TestRebuildHashVetoesNoveltyAcrossKeyChange(t *testing.T) {
	const hash = "143ed15e5e3df072ae91adaeb149973a887590dd"
	path := filepath.Join(t.TempDir(), "feed.json")
	w := newTestWriter(path, "", false)
	t0 := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return t0 }

	// Baseline over the torrent at its original URL: key AND hash enter seen.
	orig := seadex.Entry{
		AniListID: 7,
		Torrents: []seadex.Torrent{{
			Tracker: "Nyaa", URL: "https://nyaa.si/view/42", InfoHash: hash, IsBest: true,
			Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
		}},
	}
	if err := w.Rebuild(context.Background(), []seadex.Entry{orig}, nil); err != nil {
		t.Fatalf("baseline Rebuild: %v", err)
	}

	// The same torrent re-appears under a NEW view id: its journal key is new
	// but its hash is seen, so it must not journal as new.
	moved := orig
	moved.Torrents = []seadex.Torrent{orig.Torrents[0]}
	moved.Torrents[0].URL = "https://nyaa.si/view/9042"
	t1 := t0.Add(3 * time.Hour)
	w.now = func() time.Time { return t1 }
	if err := w.Rebuild(context.Background(), []seadex.Entry{moved}, nil); err != nil {
		t.Fatalf("moved Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 0 {
		t.Errorf("feed = %d items, want 0 (a seen hash under a new key must veto novelty)", len(snap.NyaaFeed))
	}
	if !snap.Seen["nyaa:9042"] || !snap.Seen[hash] {
		t.Errorf("seen ledger missing the new key or the carried hash: %v", snap.Seen)
	}
}

// TestRebuildKeyVetoesNoveltyAcrossHashChange pins the mirror image of
// TestRebuildHashVetoesNoveltyAcrossKeyChange: a torrent whose journal KEY is
// already in the seen ledger must not re-enter the journal as new when its
// info hash changes (SeaDex correcting a hash, or a same-view-id in-place
// replacement). Novelty is vetoed by ANY seen identity signal - not just the
// hash - and the NEW hash still folds into the seen ledger even though the
// torrent is not new.
func TestRebuildKeyVetoesNoveltyAcrossHashChange(t *testing.T) {
	const hashA = "143ed15e5e3df072ae91adaeb149973a887590dd"
	const hashB = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	path := filepath.Join(t.TempDir(), "feed.json")
	w := newTestWriter(path, "", false)
	t0 := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return t0 }
	orig := seadex.Entry{
		AniListID: 7,
		Torrents: []seadex.Torrent{{
			Tracker: "Nyaa", URL: "https://nyaa.si/view/42", InfoHash: hashA, IsBest: true,
			Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
		}},
	}
	if err := w.Rebuild(context.Background(), []seadex.Entry{orig}, nil); err != nil {
		t.Fatalf("baseline Rebuild: %v", err)
	}
	swapped := orig
	swapped.Torrents = []seadex.Torrent{orig.Torrents[0]}
	swapped.Torrents[0].InfoHash = hashB
	w.now = func() time.Time { return t0.Add(3 * time.Hour) }
	if err := w.Rebuild(context.Background(), []seadex.Entry{swapped}, nil); err != nil {
		t.Fatalf("swapped Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 0 {
		t.Errorf("feed = %d items, want 0 (a seen key under a new hash must veto novelty)", len(snap.NyaaFeed))
	}
	if !snap.Seen[hashB] || !snap.Seen["nyaa:42"] {
		t.Errorf("seen ledger missing the new hash or the key (every signal must fold even when not new): %v", snap.Seen)
	}
}

// TestRebuildKeepsItemAtExactMaxAgeBoundary pins the strict-inequality prune
// boundary carryItem's contract documents ("an item OLDER than
// feedJournalMaxAge leaves the journal"): a carried item whose age equals
// feedJournalMaxAge exactly stays in the feed, and one second past it prunes.
func TestRebuildKeepsItemAtExactMaxAgeBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	w := newTestWriter(path, "", false)
	t0 := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	first := t0.Add(-feedJournalMaxAge) // age == feedJournalMaxAge exactly
	w.now = func() time.Time { return t0 }
	writeSnapshotFile(t, path, &snapshot{
		ByHash: map[string]bool{},
		ByKey:  map[string]bool{},
		Seen:   map[string]bool{"nyaa:42": true},
		NyaaFeed: []item{{
			Title: "Show - S01 (1080p) [G]", GUID: "https://nyaa.si/view/42",
			DownloadURL: "https://nyaa.si/download/42.torrent",
			Key:         "nyaa:42", AniListID: 7,
			FirstSeen: first, PubDate: first,
		}},
	})
	if err := w.Rebuild(context.Background(), nil, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 1 {
		t.Fatalf("feed at the exact max-age boundary = %d items, want 1 (only STRICTLY older items prune)", len(snap.NyaaFeed))
	}
	if !snap.NyaaFeed[0].FirstSeen.Equal(first) {
		t.Errorf("FirstSeen = %v, want the original %v", snap.NyaaFeed[0].FirstSeen, first)
	}
	// One second past the boundary the item prunes.
	w.now = func() time.Time { return t0.Add(time.Second) }
	if err := w.Rebuild(context.Background(), nil, nil); err != nil {
		t.Fatalf("past-boundary Rebuild: %v", err)
	}
	if snap := readSnapshotFile(t, path); len(snap.NyaaFeed) != 0 {
		t.Errorf("feed one second past the boundary = %d items, want 0", len(snap.NyaaFeed))
	}
}

// TestApplyTitlesSkipsEmptyCachedTitle pins applyTitles' empty-value guard
// (the documented "items without a cached title keep their synthesized title"
// fallback): a cache entry holding an EMPTY string must not blank the served
// title, a non-empty cached title upgrades it, and an unknown key leaves the
// item untouched.
func TestApplyTitlesSkipsEmptyCachedTitle(t *testing.T) {
	items := []item{
		{Key: "nyaa:1", Title: "Synth A"},
		{Key: "nyaa:2", Title: "Synth B"},
		{Key: "nyaa:3", Title: "Synth C"},
	}
	applyTitles(items, map[string]string{"nyaa:1": "", "nyaa:2": "Harvested B"})
	if items[0].Title != "Synth A" {
		t.Errorf("empty cached title overwrote the synthesized title: %q, want %q", items[0].Title, "Synth A")
	}
	if items[1].Title != "Harvested B" {
		t.Errorf("cached title not applied: %q, want %q", items[1].Title, "Harvested B")
	}
	if items[2].Title != "Synth C" {
		t.Errorf("unknown key changed the title: %q, want %q", items[2].Title, "Synth C")
	}
}
