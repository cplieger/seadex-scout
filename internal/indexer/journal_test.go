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
	"github.com/cplieger/seadex-scout/internal/tagfilter"
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
	return NewFeedWriter(cfg, nil, nil)
}

// newLoggedTestWriter is newTestWriter (Nyaa configured, AnimeBytes off) with
// a capture logger injected, for tests asserting on the writer's log output.
func newLoggedTestWriter(path string, log *slog.Logger) *FeedWriter {
	return NewFeedWriter(&FeedWriterConfig{
		Path:           path,
		UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"},
	}, log, nil)
}

// feedExcludesWarnings is the operator tag policy the feed-exclusion tests
// configure: SeaDex's own curation-warning vocabulary excluded from the FEED
// surface. It exists because the shipped default excludes NOTHING (an absent or
// empty filters.exclude_tags), so every test pinning the identity-wide exclusion
// machinery must now ask for it explicitly - see
// TestRebuildKeepsCurationWarnedTorrentsByDefault for the default.
func feedExcludesWarnings() tagfilter.Filter {
	return tagfilter.New(map[string][]tagfilter.Surface{
		"broken":     {tagfilter.SurfaceFeed},
		"incomplete": {tagfilter.SurfaceFeed},
	})
}

// newExcludingTestWriter is newTestWriter with feedExcludesWarnings configured.
func newExcludingTestWriter(path string) *FeedWriter {
	return NewFeedWriter(&FeedWriterConfig{
		Path:           path,
		TagFilter:      feedExcludesWarnings(),
		UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"},
	}, nil, nil)
}

// newLoggedExcludingTestWriter is newExcludingTestWriter with a capture logger.
func newLoggedExcludingTestWriter(path string, log *slog.Logger) *FeedWriter {
	return NewFeedWriter(&FeedWriterConfig{
		Path:           path,
		TagFilter:      feedExcludesWarnings(),
		UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"},
	}, log, nil)
}

// emptyFeedJSON is the minimal journal-schema snapshot (an empty but PRESENT
// publication log), the seed that bypasses the first-run baseline in tests.
const emptyFeedJSON = `{"version":2,"owners":{},"published":{},"nyaa_feed":[],"ab_feed":[]}`

// seedEmptyFeed writes a journal-schema snapshot with an EMPTY publication log
// at path, so the next Rebuild treats every curated torrent as newly curated -
// bypassing the first-run baseline (which would record everything as seen and
// serve an empty journal).
func seedEmptyFeed(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(emptyFeedJSON), 0o600); err != nil {
		t.Fatalf("seed empty ledger: %v", err)
	}
}

// seedLedgerWithCursor is seedEmptyFeed plus a persisted harvest rotation
// cursor, for tests pinning where the next rebuild's title harvest resumes.
func seedLedgerWithCursor(t *testing.T, path, cursor string) {
	t.Helper()
	snap := `{"version":2,"owners":{},"published":{},"nyaa_feed":[],"ab_feed":[],"harvest_cursor":` + strconv.Quote(cursor) + `}`
	if err := os.WriteFile(path, []byte(snap), 0o600); err != nil {
		t.Fatalf("seed ledger with cursor: %v", err)
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

// writeSnapshotFile persists a hand-built snapshot for tests that seed feed
// state directly (titles, first-seen times).
//
// It stamps the CURRENT schema version when a fixture leaves it zero, so each
// test keeps expressing only the property it is about; a fixture testing the
// version envelope itself sets Version explicitly and is left alone. It also
// allocates the two required facts a fixture omitted, since a document naming
// neither is structurally invalid by design.
//
// Every feed item must satisfy the shared decode gate's journal-record
// invariant - a Key and a nonzero FirstSeen (validJournalRecord, h-f2) - or that
// item is dropped at decode. Fixtures that do not care about the timestamp get
// one stamped here; a fixture that sets FirstSeen itself (a skewed or aged
// clock) is left alone, and a deliberately keyless item stays keyless.
func writeSnapshotFile(t *testing.T, path string, snap *snapshot) {
	t.Helper()
	if snap.Version == 0 {
		snap.Version = currentFeedVersion
	}
	if snap.Owners == nil {
		snap.Owners = map[string][]ownedRelease{}
	}
	if snap.Published == nil {
		snap.Published = map[string]bool{}
	}
	stampFixtureFirstSeen(snap.NyaaFeed)
	stampFixtureFirstSeen(snap.ABFeed)
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
}

// stampFixtureFirstSeen gives every timestamp-less fixture item a stable
// nonzero FirstSeen (recent, so it is inside feedJournalMaxAge and not
// future-skewed), leaving an explicitly-set one alone.
func stampFixtureFirstSeen(feed []journalItem) {
	for i := range feed {
		if feed[i].FirstSeen.IsZero() {
			feed[i].FirstSeen = time.Now().UTC().Add(-time.Hour)
		}
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
	if !snap.Published["nyaa:42"] {
		t.Errorf("publication log missing nyaa:42 after baseline: %v", snap.Published)
	}
	if len(byKeyOf(&snap)) != 1 {
		t.Errorf("search curation keys = %d, want 1 (search must still cover the whole catalogue)", len(byKeyOf(&snap)))
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
// snapshot without a publication log (the retired whole-catalogue window model) is
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
	if err := NewFeedWriter(&FeedWriterConfig{Path: path}, log, nil).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 0 {
		t.Errorf("feed after old-schema migration = %d items, want 0 (baseline-empty)", len(snap.NyaaFeed))
	}
	if !snap.Published["nyaa:42"] {
		t.Errorf("publication log missing the migrated catalogue: %v", snap.Published)
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
	if err := NewFeedWriter(&FeedWriterConfig{Path: path}, log, nil).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if snap := readSnapshotFile(t, path); len(snap.NyaaFeed) != 0 || !snap.Published["nyaa:42"] {
		t.Errorf("malformed snapshot did not re-baseline: feed=%d seen=%v", len(snap.NyaaFeed), snap.Published)
	}
	if !rec.Contains("previous feed snapshot malformed; re-baselining the feed journal") {
		t.Errorf("malformed snapshot not warned; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestRebuildJournalsNewlyCurated pins the journal growth contract: a torrent
// newly present in the curation set (absent from the publication log) enters the
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
// title, while the publication log keeps its identity - so the pruned item can
// never re-enter the journal as new even though SeaDex still curates it.
func TestRebuildPrunesAgedItemsAndTitles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	w := newTestWriter(path, "", false)
	t0 := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return t0 }
	seedEmptyFeed(t, path)
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

	// Within the window the cached title is served. The harvested title claims a
	// whole season ("S01") over a file list that proves ONE episode, so the
	// pack-claim correction rewrites its season token in place before serving -
	// the corrected form is still the HARVESTED text (group + BluRay), which is
	// what distinguishes it from the synthesized "Show - S01E01 (1080p) [G]".
	t1 := t0.Add(24 * time.Hour)
	w.now = func() time.Time { return t1 }
	if err := w.Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("within-window Rebuild: %v", err)
	}
	snap = readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 1 || snap.NyaaFeed[0].Title != "Show S01E01 1080p BluRay [G]" {
		t.Fatalf("within-window feed = %+v, want the cached harvested title served", snap.NyaaFeed)
	}

	// Past the window the item ages out, its title cache entry goes with it,
	// and the publication log keeps the identity.
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
	if !snap.Published["nyaa:42"] {
		t.Errorf("publication log lost the pruned identity: %v", snap.Published)
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
	seedEmptyFeed(t, path)
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

// TestRenderJournalItemUnionsCategoriesWithoutDuplicates pins the dedupe half
// of the category fold: a torrent attached to several SeaDex entries of the
// SAME media type must render exactly one category, not one per occurrence.
// The mixed-type case (TestRebuildSharedTorrentMergesBestWins) cannot see this
// - two different categories are two entries either way - so without this test
// a fold that appends unconditionally serves an RSS item repeating the same
// <category> element once per sharing entry.
func TestRenderJournalItemUnionsCategoriesWithoutDuplicates(t *testing.T) {
	w := newTestWriter(filepath.Join(t.TempDir(), "feed.json"), "", false)
	torrent := seadex.Torrent{
		Tracker: "Nyaa", URL: "https://nyaa.si/view/1234567",
		Files: []seadex.File{{Length: 7, Name: "Show - S01E01 (1080p) [G].mkv"}},
	}
	e1 := &seadex.Entry{AniListID: 1, Torrents: []seadex.Torrent{torrent}}
	e2 := &seadex.Entry{AniListID: 2, Torrents: []seadex.Torrent{torrent}}
	refs := []curatedRef{
		{entry: e1, torrent: &e1.Torrents[0]},
		{entry: e2, torrent: &e2.Torrents[0]},
	}
	it, ok, _ := w.renderJournalItem("nyaa:1234567", refs, func(int) EntryInfo { return EntryInfo{} })
	if !ok {
		t.Fatal("renderJournalItem: item not rendered")
	}
	if len(it.Categories) != 1 || it.Categories[0] != catAnime {
		t.Errorf("Categories = %v, want exactly [%d] (the union must not repeat a category per sharing entry)", it.Categories, catAnime)
	}
}

// TestRenderJournalItemSortsCategoryUnion pins the OUTPUT order of foldRefs'
// category union, which its comment is explicit about ("sorting makes the fold
// order-independent in its OUTPUT too, not just as a set"): a torrent attached
// to a series entry and a movie entry must render its categories in ascending
// order whatever order PocketBase returned the relation in. The order-invariance
// property compares categories as a SET (it sorts both sides before comparing),
// so dropping the production sort keeps every existing test green while the
// persisted snapshot and the served <category> order start flapping with
// catalogue order - rewriting feed.json on rebuilds that changed nothing.
func TestRenderJournalItemSortsCategoryUnion(t *testing.T) {
	w := newTestWriter(filepath.Join(t.TempDir(), "feed.json"), "", false)
	torrent := seadex.Torrent{
		Tracker: "Nyaa", URL: "https://nyaa.si/view/1234567",
		Files: []seadex.File{{Length: 7, Name: "Show - S01E01 (1080p) [G].mkv"}},
	}
	// The series entry is listed FIRST, so the append order (Anime, Movies) is
	// the reverse of the sorted order (Movies 2000 < Anime 5070).
	series := &seadex.Entry{AniListID: 1, Torrents: []seadex.Torrent{torrent}}
	movie := &seadex.Entry{AniListID: 2, Torrents: []seadex.Torrent{torrent}}
	refs := []curatedRef{
		{entry: series, torrent: &series.Torrents[0]},
		{entry: movie, torrent: &movie.Torrents[0]},
	}
	it, ok, _ := w.renderJournalItem("nyaa:1234567", refs, func(alID int) EntryInfo {
		return EntryInfo{IsMovie: alID == 2}
	})
	if !ok {
		t.Fatal("renderJournalItem: item not rendered")
	}
	if len(it.Categories) != 2 || it.Categories[0] != catMovies || it.Categories[1] != catAnime {
		t.Errorf("Categories = %v, want [%d %d] ascending regardless of the occurrence order the catalogue supplied",
			it.Categories, catMovies, catAnime)
	}
}

// TestRebuildRejectsForeignHostTrackerURLs pins the curation trust boundary
// (trackerKey's host gate): a SeaDex record whose tracker label says Nyaa but
// whose URL sits on a foreign host must mint NO curation key and NO journal
// item - the label alone must never authorize an id extracted from a foreign
// URL (a compromised record with evil.example/view/111 would otherwise both
// admit the REAL Nyaa torrent 111 into the search curation set and serve a
// canonical nyaa.si download link for it on RSS). The gated torrents surface
// on the unresolvable counter instead of vanishing silently, and their
// identities stay OUT of the publication log, so the same torrent republished
// with its real tracker URL still journals as new.
func TestRebuildRejectsForeignHostTrackerURLs(t *testing.T) {
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
	seedEmptyFeed(t, path)
	log, rec := capture.New()
	w := newTestWriter(path, "", false)
	w.log = log
	if err := w.Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 0 {
		t.Errorf("feed = %d items, want 0 (foreign-host URLs must not journal)", len(snap.NyaaFeed))
	}
	if len(byKeyOf(&snap)) != 0 || len(byHashOf(&snap)) != 0 {
		t.Errorf("curation set = %d keys / %d hashes, want empty (no authorization from a foreign URL)", len(byKeyOf(&snap)), len(byHashOf(&snap)))
	}
	if len(snap.Published) != 0 {
		t.Errorf("publication log = %v, want empty (a later legitimate republish must journal as new)", snap.Published)
	}
	if !rec.Contains("indexer feed snapshot written") {
		t.Fatalf("no snapshot log line; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// foreignHostInfoHash is a valid 40-hex info hash for the ownership-gate
// regression tests below: the ledger rule they pin is only observable when the
// gated torrent HAS a second identity signal to fold.
const foreignHostInfoHash = "abcdef0123456789abcdef0123456789abcdef01"

// TestRebuildKeepsHashedForeignHostTorrentOutOfLedger closes the hole the
// sibling TestRebuildRejectsForeignHostTrackerURLs only appeared to cover: its
// fixture torrents carry no InfoHash, so an empty ledger was guaranteed by the
// fixture rather than by the rule. A gated torrent WITH a valid info hash used
// to fold that hash into the never-pruned ledger (journalKey is "" for it, so
// it can never be journaled), which permanently denied the release RSS
// exposure once upstream corrected the URL - and silenced the unresolvable
// diagnostic after the first rebuild, since the folded hash made the next
// rebuild return before the count. Nothing is folded for a keyless torrent
// now; only the two OPERATOR switches (an off tracker, a missing AB passkey)
// consume novelty.
func TestRebuildKeepsHashedForeignHostTorrentOutOfLedger(t *testing.T) {
	entries := []seadex.Entry{{
		AniListID: 9,
		Torrents: []seadex.Torrent{{
			Tracker: "Nyaa", URL: "https://evil.example/view/111", IsBest: true,
			InfoHash: foreignHostInfoHash,
			Files:    []seadex.File{{Length: 1, Name: "Show A - S01E01 (1080p) [G].mkv"}},
		}},
	}}
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	if err := newTestWriter(path, "", false).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 0 {
		t.Errorf("feed = %d items, want 0 (a foreign-host URL must not journal)", len(snap.NyaaFeed))
	}
	if len(snap.Published) != 0 {
		t.Errorf("publication log = %v, want empty (an unjournalable torrent's hash must not consume novelty)", snap.Published)
	}
}

// TestRebuildJournalsReleaseAfterTrackerURLCorrected is the end-to-end half of
// the same rule: the release the ownership gate refused on the first rebuild
// must journal as new once SeaDex publishes its real tracker URL, with the info
// hash unchanged across both rebuilds. Before the fix the first rebuild's
// folded hash made the corrected record read as already seen forever.
func TestRebuildJournalsReleaseAfterTrackerURLCorrected(t *testing.T) {
	files := []seadex.File{{Length: 1, Name: "Show A - S01E01 (1080p) [G].mkv"}}
	gated := []seadex.Entry{{
		AniListID: 9,
		Torrents: []seadex.Torrent{{
			Tracker: "Nyaa", URL: "https://evil.example/view/111", IsBest: true,
			InfoHash: foreignHostInfoHash, Files: files,
		}},
	}}
	corrected := []seadex.Entry{{
		AniListID: 9,
		Torrents: []seadex.Torrent{{
			Tracker: "Nyaa", URL: "https://nyaa.si/view/111", IsBest: true,
			InfoHash: foreignHostInfoHash, Files: files,
		}},
	}}
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	w := newTestWriter(path, "", false)
	if err := w.Rebuild(context.Background(), gated, nil); err != nil {
		t.Fatalf("Rebuild (gated URL): %v", err)
	}
	if err := w.Rebuild(context.Background(), corrected, nil); err != nil {
		t.Fatalf("Rebuild (corrected URL): %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 1 {
		t.Fatalf("feed = %d items, want 1 (a corrected upstream record is a legitimate later republish)", len(snap.NyaaFeed))
	}
	if got := snap.NyaaFeed[0].Key; got != "nyaa:111" {
		t.Errorf("journaled key = %q, want %q", got, "nyaa:111")
	}
}

// TestRebuildBaselineKeylessTorrentDoesNotConsumeNovelty pins the same keyless
// guard on the OTHER entry point: the fresh-install baseline (baselinePublications),
// which runs before any ledger exists and so bypasses journalIfNew entirely. A
// supported-tracker record whose URL is foreign or unparseable has no key, so its
// info hash must NOT enter the never-pruned publication log - otherwise the record
// can never journal after SeaDex corrects the URL and the arr never sees the
// release on RSS.
func TestRebuildBaselineKeylessTorrentDoesNotConsumeNovelty(t *testing.T) {
	files := []seadex.File{{Length: 1, Name: "Show A - S01E01 (1080p) [G].mkv"}}
	gated := []seadex.Entry{{
		AniListID: 9,
		Torrents: []seadex.Torrent{{
			Tracker: "Nyaa", URL: "https://evil.example/view/111", IsBest: true,
			InfoHash: foreignHostInfoHash, Files: files,
		}},
	}}
	corrected := []seadex.Entry{{
		AniListID: 9,
		Torrents: []seadex.Torrent{{
			Tracker: "Nyaa", URL: "https://nyaa.si/view/111", IsBest: true,
			InfoHash: foreignHostInfoHash, Files: files,
		}},
	}}
	path := filepath.Join(t.TempDir(), "feed.json")
	w := newTestWriter(path, "", false)
	if err := w.Rebuild(t.Context(), gated, nil); err != nil {
		t.Fatalf("baseline Rebuild: %v", err)
	}
	if err := w.Rebuild(t.Context(), corrected, nil); err != nil {
		t.Fatalf("corrected Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 1 || snap.NyaaFeed[0].Key != "nyaa:111" {
		t.Errorf("corrected release feed = %+v, want one nyaa:111 item", snap.NyaaFeed)
	}
}

// TestRebuildDropsUnknownTracker pins the tail-drop: a SeaDex torrent on a
// tracker other than Nyaa/AB (the negligible AnimeTosho/RuTracker tail) never
// enters a journal feed and is not classified into a configured scope (a tail
// tracker resolves to no scope, so it is not counted as unresolvable either).
func TestRebuildDropsUnknownTracker(t *testing.T) {
	entries := []seadex.Entry{{
		AniListID: 5,
		Torrents: []seadex.Torrent{{
			Tracker: "AnimeTosho", URL: "https://animetosho.org/view/1", IsBest: true,
			Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
		}},
	}}
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	log, rec := capture.New()
	if err := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{ABPasskey: "PK", ABTorznabURL: "http://prowlarr/2/api"}}, log, nil).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 0 || len(snap.ABFeed) != 0 {
		t.Errorf("unknown tracker leaked into a feed: nyaa=%d ab=%d, want 0 and 0", len(snap.NyaaFeed), len(snap.ABFeed))
	}
	if got, ok := rec.AttrValue("indexer feed snapshot written", "skipped_unresolvable"); !ok || got != "0" {
		t.Errorf("skipped_unresolvable = %q (found=%v), want 0: an unknown tracker must not be classified into a configured scope; log output:\n%s",
			got, ok, strings.Join(rec.Messages(), "\n"))
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
	seedEmptyFeed(t, path)
	log, rec := capture.New()
	if err := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{ABTorznabURL: "http://prowlarr/2/api"}}, log, nil).Rebuild(context.Background(), entries, nil); err != nil {
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
	seedEmptyFeed(t, path)
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
// tracker split, GUID-only persistence on both feeds (no download URL at
// rest; the reader derives the public Nyaa .torrent and the passkey-bearing
// AB link from each item's GUID on load), best/alt markers, the dropped
// redacted AB info hash, the SeaDex entry info URL, the summed pack size,
// the synthesized title from the show metadata, and PubDate mirroring
// FirstSeen (not the SeaDex entry update).
func TestRebuildJournalItemShape(t *testing.T) {
	pmrFiles := []seadex.File{
		{Length: 400_000_000, Name: "NCED 01 (BD Remux 1080p AVC FLAC) [PMR].mkv"},
		{Length: 7_500_699_108, Name: "Frieren Beyond Journey's End - S01E01 (BD Remux 1080p AVC FLAC AAC) [Dual Audio] [PMR].mkv"},
		{Length: 7_497_267_058, Name: "Frieren Beyond Journey's End - S01E02 (BD Remux 1080p AVC FLAC AAC) [Dual Audio] [PMR].mkv"},
	}
	entries := []seadex.Entry{{
		AniListID: 154587,
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
		return EntryInfo{Title: "Frieren: Beyond Journey's End", Season: 1, SeasonKnown: true}
	}
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api", ABPasskey: "PASSKEY123", ABTorznabURL: "http://prowlarr/2/api"}}, nil, nil)
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
	if pmrNyaa.DownloadURL != "" {
		t.Errorf("PMR nyaa persisted download = %q, want empty (GUID-only; the reader derives the public link)", pmrNyaa.DownloadURL)
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

	byKey := map[string]journalItem{}
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
// current curation set keeps its stored render (title, FirstSeen) - it is
// still a valid release the arrs may grab - instead of being re-rendered or
// dropped. The download URL is GUID-only at rest (stripDownloadURLs); the
// reader re-derives it on load.
func TestRebuildCarriesUncuratedItemStoredRender(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	first := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	writeSnapshotFile(t, path, &snapshot{
		Owners:    owns(),
		Published: map[string]bool{"nyaa:42": true},
		NyaaFeed: []journalItem{
			{item: item{Title: "Stored Show - S01 (1080p) [G]", GUID: "https://nyaa.si/view/42", DownloadURL: "https://nyaa.si/download/42.torrent", PubDate: first}, Key: "nyaa:42", AniListID: 7, FirstSeen: first},
		},
	})
	if err := newTestWriter(path, "", false).Rebuild(context.Background(), nil, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 1 {
		t.Fatalf("feed = %d items, want 1 (a curated-then-replaced torrent keeps its stored render)", len(snap.NyaaFeed))
	}
	got := snap.NyaaFeed[0]
	if got.Title != "Stored Show - S01 (1080p) [G]" {
		t.Errorf("carried item = %+v, want the stored render unchanged", got)
	}
	if got.DownloadURL != "" {
		t.Errorf("carried item persisted download = %q, want empty (GUID-only; the reader derives the public link)", got.DownloadURL)
	}
	if !got.FirstSeen.Equal(first) {
		t.Errorf("FirstSeen = %v, want the original %v", got.FirstSeen, first)
	}
}

// TestRebuildCarriedGUIDKeptOnlyForSameIdentity pins the GUID half of the
// carry contract both ways: a stored GUID that still resolves to the carried
// item's tracker key is preserved verbatim (URL-text churn on the same
// identity must never mint a new GUID and re-trigger a grab), while a
// foreign-host stored GUID (a hand-edited or corrupted snapshot) fails the
// identity check and self-heals to the fresh canonical URL instead of
// permanently displacing it.
func TestRebuildCarriedGUIDKeptOnlyForSameIdentity(t *testing.T) {
	first := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	tests := []struct {
		name   string
		stored string
		want   string
	}{
		{"same-key stored GUID with changed URL text is preserved", "https://nyaa.si/view/42?utm=x", "https://nyaa.si/view/42?utm=x"},
		{"foreign stored GUID self-heals to the fresh canonical URL", "https://evil.example/view/42", "https://nyaa.si/view/42"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "feed.json")
			writeSnapshotFile(t, path, &snapshot{
				Owners:    owns(),
				Published: map[string]bool{"nyaa:42": true},
				NyaaFeed: []journalItem{
					{item: item{Title: "Show - S01 (1080p) [G]", GUID: tc.stored, PubDate: first}, Key: "nyaa:42", AniListID: 7, FirstSeen: first},
				},
			})
			entries := []seadex.Entry{nyaaEntry(7, 42, true, "Show - S01E01 (1080p) [G].mkv")}
			if err := newTestWriter(path, "", false).Rebuild(context.Background(), entries, nil); err != nil {
				t.Fatalf("Rebuild: %v", err)
			}
			snap := readSnapshotFile(t, path)
			if len(snap.NyaaFeed) != 1 {
				t.Fatalf("feed = %d items, want 1 (the carried item survives the rebuild)", len(snap.NyaaFeed))
			}
			if got := snap.NyaaFeed[0].GUID; got != tc.want {
				t.Errorf("carried GUID = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRebuildCarriesABItemWhenPasskeyRemoved pins the reversibility of the AB
// passkey - the tracker's SECOND off switch, beside blanking its Torznab URL.
// The carry used to drop a journaled AnimeBytes item as soon as ab_passkey went
// missing, so one rebuild destroyed the AB journal and the never-pruned seen
// ledger stopped those releases ever returning: removing the key to debug
// something cost the un-grabbed part of the journal window permanently
// (l-f161). A passkey only supplies the grabbable LINK, and nothing unservable
// escapes - items persist GUID-only (stripDownloadURLs) and the reader clears
// the entire AB feed while no passkey is configured (rebuildABDownloadURLs) - so
// the item is CARRIED and becomes servable again when the key returns.
func TestRebuildCarriesABItemWhenPasskeyRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	first := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	writeSnapshotFile(t, path, &snapshot{
		Owners:    owns(),
		Published: map[string]bool{"ab:1167293": true},
		ABFeed: []journalItem{
			{item: item{Title: "Frieren - S01 (BD Remux 1080p) [PMR]", GUID: "https://animebytes.tv/torrents.php?id=86576&torrentid=1167293", PubDate: first}, Key: "ab:1167293", AniListID: 154587, FirstSeen: first},
		},
	})
	entries := []seadex.Entry{{
		AniListID: 154587,
		Torrents: []seadex.Torrent{{
			Tracker: "AB", URL: "/torrents.php?id=86576&torrentid=1167293", InfoHash: "<redacted>",
			IsBest: true, ReleaseGroup: "PMR",
			Files: []seadex.File{{Length: 1, Name: "Frieren - S01E01 (BD Remux 1080p) [PMR].mkv"}},
		}},
	}}
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{ABTorznabURL: "http://prowlarr/2/api"}}, nil, nil)
	if err := w.Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if snap := readSnapshotFile(t, path); len(snap.ABFeed) != 1 {
		t.Errorf("ab feed = %+v, want the carried item kept: the passkey is a reversible off switch", snap.ABFeed)
	}
}

// TestRebuildCarriesNonCuratedABItemWhenPasskeyRemoved pins the sibling arm:
// a previously journaled AnimeBytes item whose torrent has LEFT the curation set
// is carried through a passkey-less window for the same reason as a still-curated
// one, so the restored AB feed is not an arbitrary subset of what it held.
func TestRebuildCarriesNonCuratedABItemWhenPasskeyRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	first := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	writeSnapshotFile(t, path, &snapshot{
		Owners:    owns(),
		Published: map[string]bool{"ab:1167293": true},
		ABFeed: []journalItem{
			{item: item{Title: "Frieren - S01 (BD Remux 1080p) [PMR]", GUID: "https://animebytes.tv/torrents.php?id=86576&torrentid=1167293", PubDate: first}, Key: "ab:1167293", AniListID: 154587, FirstSeen: first},
		},
	})
	// No entries: the carried item's torrent is absent from the curation set,
	// exercising the non-curated carry arm.
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{ABTorznabURL: "http://prowlarr/2/api"}}, nil, nil)
	if err := w.Rebuild(context.Background(), nil, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if snap := readSnapshotFile(t, path); len(snap.ABFeed) != 1 {
		t.Errorf("ab feed = %+v, want the carried non-curated item kept through a passkey-less window", snap.ABFeed)
	}
}

// TestRebuildCarriesCuratedABItemWhenRenderFailsWithoutPasskey pins
// refreshCarriedItem's residual AnimeBytes arm: an item still IN the curation
// set whose fresh render fails on every occurrence (here no files and no
// release group, so no title synthesizes) while no indexer.ab_passkey is
// configured keeps its STORED render instead of being dropped.
//
// The drop would be permanent. The never-pruned publication log still holds the
// identity, so growJournal can never re-admit the release afterwards - which is
// exactly the irreversible second off switch l-f161 closed for the passkey, and
// it is invisible in every count assertion that only watches a renderable item.
func TestRebuildCarriesCuratedABItemWhenRenderFailsWithoutPasskey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	first := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	const guid = "https://animebytes.tv/torrents.php?id=86576&torrentid=1000"
	const stored = "Stored AB Show - S01 (1080p) [G]"
	writeSnapshotFile(t, path, &snapshot{
		Owners:    owns(keyed("ab:1000", true)),
		Published: map[string]bool{"ab:1000": true},
		ABFeed: []journalItem{{
			item: item{Title: stored, GUID: guid, PubDate: first},
			Key:  "ab:1000", AniListID: 11, FirstSeen: first,
		}},
	})
	// AnimeBytes configured (its Torznab URL is set) with the passkey REMOVED.
	w := newTestWriter(path, "", true)
	w.now = func() time.Time { return first.Add(time.Hour) }
	// The same torrent, still curated, but unrenderable: no files and no release
	// group, so synthesizeTitle yields nothing for every occurrence.
	entries := []seadex.Entry{{
		AniListID: 11,
		Torrents: []seadex.Torrent{{
			Tracker: "AB", URL: "/torrents.php?id=86576&torrentid=1000", IsBest: true,
		}},
	}}
	if err := w.Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.ABFeed) != 1 {
		t.Fatalf("ab_feed = %d items, want the stored item carried (a drop is permanent: the publication log keeps the identity): %+v",
			len(snap.ABFeed), snap.ABFeed)
	}
	got := snap.ABFeed[0]
	if got.Title != stored {
		t.Errorf("carried title = %q, want the stored render %q", got.Title, stored)
	}
	if !got.FirstSeen.Equal(first) {
		t.Errorf("carried FirstSeen = %v, want the stored %v", got.FirstSeen, first)
	}
	if got.GUID != guid {
		t.Errorf("carried GUID = %q, want the stored %q", got.GUID, guid)
	}
}

// TestRebuildDefersNewABItemUntilPasskeyArrives pins the GROWTH half of the
// AB-passkey reversibility the two carry tests above pin, in the shape the
// publication log makes possible: a release SeaDex curates while ab_passkey is
// unset is NOT journaled and NOT published, so it journals as new - with a
// grabbable link - on the first rebuild after the passkey arrives. The older
// shape journaled it GUID-only because journalIfNew folded its identity into the
// never-pruned log BEFORE the render; with the log written on PUBLICATION the
// general rule (a failed render is retryable, never terminal) covers the case at
// the root, so that exemption is gone. The nudge still fires and still counts it.
func TestRebuildDefersNewABItemUntilPasskeyArrives(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	log, rec := capture.New()
	entries := []seadex.Entry{{
		AniListID: 154587,
		Torrents: []seadex.Torrent{{
			Tracker: "AB", URL: "/torrents.php?id=86576&torrentid=1167293", InfoHash: "<redacted>",
			IsBest: true, ReleaseGroup: "PMR",
			Files: []seadex.File{{Length: 1, Name: "Frieren - S01E01 (BD Remux 1080p) [PMR].mkv"}},
		}},
	}}
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{ABTorznabURL: "http://prowlarr/2/api"}}, log, nil)
	if err := w.Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.ABFeed) != 0 {
		t.Fatalf("ab feed = %+v, want nothing journaled while no passkey can build a grabbable link", snap.ABFeed)
	}
	if snap.Published["ab:1167293"] {
		t.Error("publication log recorded ab:1167293 although nothing was served; the log is never pruned, so the release could never journal as new")
	}
	if v, ok := rec.AttrValue("ab RSS feed empty of grabbable links", "ab_releases_skipped"); !ok || v != "1" {
		t.Errorf("operator nudge missing or miscounted (got %q, found=%v); log output:\n%s", v, ok, strings.Join(rec.Messages(), "\n"))
	}
	// The nudge's promise, which only an unwritten log can keep: setting the
	// passkey journals the release, with a grabbable link.
	withKey := newTestWriter(path, strings.Repeat("a", 32), true)
	if err := withKey.Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild with a passkey configured: %v", err)
	}
	snap = readSnapshotFile(t, path)
	if len(snap.ABFeed) != 1 {
		t.Fatalf("ab feed = %+v, want the release journaled once the passkey arrives", snap.ABFeed)
	}
	if got := snap.ABFeed[0]; got.Key != "ab:1167293" || got.GUID == "" {
		t.Errorf("journaled item = %+v, want the AB key and its tracker page URL as GUID", got)
	}
	if !snap.Published["ab:1167293"] {
		t.Error("publication log missing ab:1167293 after the release entered the served feed")
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
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, log, nil)
	t0 := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return t0 }
	future := t0.Add(72 * time.Hour)
	writeSnapshotFile(t, path, &snapshot{
		Owners:    owns(),
		Published: map[string]bool{"nyaa:42": true},
		NyaaFeed: []journalItem{
			{item: item{Title: "Show - S01 (1080p) [G]", GUID: "https://nyaa.si/view/42", DownloadURL: "https://nyaa.si/download/42.torrent", PubDate: future}, Key: "nyaa:42", AniListID: 7, FirstSeen: future},
		},
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
	if got, ok := rec.AttrValue("indexer feed snapshot written", "journal_clock_rebased"); !ok || got != "1" {
		t.Errorf("journal_clock_rebased = %q (found=%v), want 1; log:\n%s", got, ok, strings.Join(rec.Messages(), "\n"))
	}
}

// TestRebuildDropsKeylessSeededItem pins where a journal-bookkeeping-less item
// is now refused: a post-journal snapshot (publication log present) whose feed
// carries an item with no Key or no FirstSeen violates the shared decode gate's
// journal-record invariant (validJournalRecord, h-f2), so THAT item is dropped
// at decode and never carried - the reason the reader can no longer serve such
// an item forever in resident-idle mode. The rest of the snapshot survives: a
// per-item defect must not re-baseline the journal or cost the curation set
// (l-f45), so no malformed warning fires. carryJournal's per-item guards stay
// as defense in depth for any snapshot that reaches them.
func TestRebuildDropsKeylessSeededItem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	const seeded = `{"version":2,"owners":{},"published":{"nyaa:11":true},"nyaa_feed":[{"Title":"orphan","GUID":"https://nyaa.si/view/9"},` +
		`{"Title":"no first seen","GUID":"https://nyaa.si/view/10","Key":"nyaa:10"},` +
		`{"Title":"kept","GUID":"https://nyaa.si/view/11","Key":"nyaa:11","FirstSeen":"2026-07-01T00:00:00Z","PubDate":"2026-07-01T00:00:00Z"}],"ab_feed":[]}`
	if err := os.WriteFile(path, []byte(seeded), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	log, rec := capture.New()
	w := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, log, nil)
	w.now = func() time.Time { return time.Date(2026, time.July, 2, 0, 0, 0, 0, time.UTC) }
	if err := w.Rebuild(context.Background(), nil, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 1 || snap.NyaaFeed[0].Key != "nyaa:11" {
		t.Errorf("feed = %+v, want only the bookkeeping-complete nyaa:11 (a bookkeeping-less item cannot be carried: it could never be pruned or re-rendered)", snap.NyaaFeed)
	}
	if rec.Contains(msgSnapshotMalformed) {
		t.Errorf("a journal item without identity or first-seen re-baselined the whole journal; log:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestPrepareCarriedItemDropsBookkeepinglessItem pins prepareCarriedItem's
// bookkeeping guard directly, the only arm of this unit no test reaches since
// the shared decode gate (validJournalRecord) started pruning such an item
// before carryJournal ever sees it: a carried item with no Key or a
// zero FirstSeen is a DROP, not a prune. The distinction is the operator's
// signal on the snapshot log line - a zero FirstSeen otherwise reads as an
// item aged out of a 14-day window it never entered - and the guard is the
// last defense for any snapshot that reaches carryJournal without the gate.
func TestPrepareCarriedItemDropsBookkeepinglessItem(t *testing.T) {
	now := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	tests := map[string]journalItem{
		"no key":        {item: item{Title: "Show - S01 (1080p) [G]"}, FirstSeen: now.Add(-time.Hour)},
		"no first seen": {item: item{Title: "Show - S01 (1080p) [G]"}, Key: "nyaa:42"},
	}
	for name, it := range tests {
		t.Run(name, func(t *testing.T) {
			var js journalStats
			p := &journalPass{js: &js, now: now}
			if p.prepareCarriedItem(&it) {
				t.Errorf("prepareCarriedItem(%+v) = true, want false", it)
			}
			if js.dropped != 1 || js.pruned != 0 || js.rebased != 0 {
				t.Errorf("stats = dropped %d / pruned %d / rebased %d, want 1 / 0 / 0", js.dropped, js.pruned, js.rebased)
			}
		})
	}
	carryable := journalItem{item: item{Title: "Show - S01 (1080p) [G]"}, Key: "nyaa:42", FirstSeen: now.Add(-time.Hour)}
	var js journalStats
	if p := (&journalPass{js: &js, now: now}); !p.prepareCarriedItem(&carryable) {
		t.Error("prepareCarriedItem(a keyed, in-window item) = false, want true")
	}
	if js.dropped != 0 {
		t.Errorf("dropped = %d, want 0 for a carryable item", js.dropped)
	}
}

// TestRebuildSkipsTitlelessTorrentAsUnresolvable pins the unresolvable
// accounting AND the publication rule it turns on, and the second half INVERTS
// what this test used to assert.
//
// A newly curated torrent with a parseable tracker key but no files and no
// release group synthesizes no title at all, so it is excluded from the journal
// (an arr cannot parse a title-less item) and counted on the pass log line as
// skipped_unresolvable - the signal that an upstream data-shape change is
// shrinking the feed.
//
// Its identity must NOT be recorded. Nothing was published, and the log is never
// pruned, so recording here is what used to make the loss PERMANENT: the reason
// the render failed is an upstream DATA property (SeaDex published the record
// with an empty file list), and a curator adding the files an hour later is a
// legitimate later republish that must journal as new. The two operator switches
// are the deliberate exceptions and they are elsewhere (an off tracker baselines
// its scope; a missing AB passkey journals GUID-only, so it publishes).
func TestRebuildSkipsTitlelessTorrentAsUnresolvable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	entries := []seadex.Entry{{
		AniListID: 7,
		Torrents:  []seadex.Torrent{{Tracker: "Nyaa", URL: "https://nyaa.si/view/7", IsBest: true}},
	}}
	log, rec := capture.New()
	if err := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, log, nil).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 0 {
		t.Errorf("feed = %+v, want empty (a title-less item cannot be parsed by an arr)", snap.NyaaFeed)
	}
	if snap.Published["nyaa:7"] {
		t.Errorf("publication log recorded nyaa:7 though nothing was published: %v; "+
			"the log is never pruned, so the corrected upstream record could never journal as new", snap.Published)
	}
	if got, ok := rec.AttrValue("indexer feed snapshot written", "skipped_unresolvable"); !ok || got != "1" {
		t.Errorf("skipped_unresolvable = %q (found=%v), want 1; log:\n%s", got, ok, strings.Join(rec.Messages(), "\n"))
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
			seedEmptyFeed(t, path)
			log, rec := capture.New()
			if err := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: tc.cfg}, log, nil).Rebuild(context.Background(), entries, nil); err != nil {
				t.Fatalf("Rebuild: %v", err)
			}
			if snap := readSnapshotFile(t, path); len(snap.ABFeed) != 0 {
				t.Errorf("identity-less AB release leaked into the feed: %d items, want 0", len(snap.ABFeed))
			}
			want := strconv.FormatInt(tc.want, 10)
			if got, ok := rec.AttrValue("indexer feed snapshot written", "skipped_unresolvable"); !ok || got != want {
				t.Errorf("skipped_unresolvable = %q (found=%v), want %s; log:\n%s", got, ok, want, strings.Join(rec.Messages(), "\n"))
			}
		})
	}
}

// TestRebuildUnknownTrackerWithHashSilentlyIgnored pins the tail-tracker
// guard for a torrent that DOES carry a stable identity: an
// AnimeTosho/RuTracker release with a valid info hash is silently ignored -
// never counted unresolvable, since the tail is expected - and contributes
// NOTHING to the publication log: AnimeTosho is a Nyaa mirror carrying the
// identical info hash, so folding it would let catalogue order decide
// whether the Nyaa listing of the same bytes ever journals (see
// TestRebuildMirrorTrackerCannotSuppressNyaaJournal).
func TestRebuildUnknownTrackerWithHashSilentlyIgnored(t *testing.T) {
	const hash = "143ed15e5e3df072ae91adaeb149973a887590dd"
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	entries := []seadex.Entry{{
		AniListID: 5,
		Torrents: []seadex.Torrent{{
			Tracker: "AnimeTosho", URL: "https://animetosho.org/view/1", InfoHash: hash, IsBest: true,
			Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
		}},
	}}
	log, rec := capture.New()
	if err := NewFeedWriter(&FeedWriterConfig{Path: path}, log, nil).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 0 || len(snap.ABFeed) != 0 {
		t.Errorf("unknown tracker leaked into a feed: nyaa=%d ab=%d", len(snap.NyaaFeed), len(snap.ABFeed))
	}
	if snap.Published[hash] {
		t.Errorf("tail-tracker hash folded into the publication log, want absent (a mirror's hash must not pre-mark the Nyaa listing): %v", snap.Published)
	}
	if got, ok := rec.AttrValue("indexer feed snapshot written", "skipped_unresolvable"); !ok || got != "0" {
		t.Errorf("skipped_unresolvable = %q (found=%v), want 0 (the tail is silently ignored, not an upstream fault signal)", got, ok)
	}
}

// TestRebuildKeepsCarriedItemBecomingUnresolvable pins carryJournal's
// stored-render fallback for a still-curated item that can no longer render: a
// journaled torrent whose current SeaDex record has lost its files and release
// group synthesizes no title, so the carried item keeps its STORED render
// rather than being dropped - a drop would be permanent, since the never-pruned
// publication log stops growJournal re-admitting the release once the upstream
// record is corrected, which is the omission settled feed-rss-filtering
// forbids. Nothing is counted as journal_dropped, and nothing is counted as an
// AB passkey skip either.
func TestRebuildKeepsCarriedItemBecomingUnresolvable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	first := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	writeSnapshotFile(t, path, &snapshot{
		Owners:    owns(keyed("nyaa:42", true)),
		Published: map[string]bool{"nyaa:42": true},
		NyaaFeed: []journalItem{
			{item: item{Title: "Show - S01 (1080p) [G]", GUID: "https://nyaa.si/view/42", DownloadURL: "https://nyaa.si/download/42.torrent", PubDate: first}, Key: "nyaa:42", AniListID: 7, FirstSeen: first},
		},
	})
	entries := []seadex.Entry{{
		AniListID: 7,
		Torrents:  []seadex.Torrent{{Tracker: "Nyaa", URL: "https://nyaa.si/view/42", IsBest: true}},
	}}
	log, rec := capture.New()
	if err := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, log, nil).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 1 {
		t.Fatalf("nyaa feed = %+v, want the carried item kept on its stored render", snap.NyaaFeed)
	}
	if got := snap.NyaaFeed[0].Title; got != "Show - S01 (1080p) [G]" {
		t.Errorf("carried title = %q, want the STORED render preserved", got)
	}
	if !snap.Published["nyaa:42"] {
		t.Errorf("publication log lost the carried identity: %v", snap.Published)
	}
	if got, ok := rec.AttrValue("indexer feed snapshot written", "journal_dropped"); !ok || got != "0" {
		t.Errorf("journal_dropped = %q (found=%v), want 0; log:\n%s", got, ok, strings.Join(rec.Messages(), "\n"))
	}
	if rec.Contains("ab RSS feed empty of grabbable links") {
		t.Errorf("the unresolvable render was counted as an AB passkey skip; log:\n%s", strings.Join(rec.Messages(), "\n"))
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

// TestRenderJournalItemFallsBackToRenderableOccurrence pins the multi-entry
// fallback contract: when the lowest-AniList-ID occurrence of a journal key
// cannot synthesize a title (no files, no release group) but a higher sibling
// with the same key can, the item renders from the renderable sibling instead
// of being rejected - one partial upstream occurrence must not permanently
// deny the release RSS while buildCuration still exposes it to search - and
// the marker fold still spans ALL occurrences.
func TestRenderJournalItemFallsBackToRenderableOccurrence(t *testing.T) {
	w := newTestWriter(filepath.Join(t.TempDir(), "feed.json"), "", false)
	// Lowest AniList ID: no files and no release group -> no parseable title.
	partial := &seadex.Entry{AniListID: 1, Torrents: []seadex.Torrent{{
		Tracker: "Nyaa", URL: "https://nyaa.si/view/77", IsBest: true,
	}}}
	full := &seadex.Entry{AniListID: 2, Torrents: []seadex.Torrent{{
		Tracker: "Nyaa", URL: "https://nyaa.si/view/77",
		Files: []seadex.File{{Length: 9, Name: "Show - S01E01 (1080p) [G].mkv"}},
	}}}
	refs := []curatedRef{
		{entry: partial, torrent: &partial.Torrents[0]},
		{entry: full, torrent: &full.Torrents[0]},
	}
	it, ok, noPasskey := w.renderJournalItem("nyaa:77", refs, func(int) EntryInfo { return EntryInfo{} })
	if !ok || noPasskey {
		t.Fatalf("renderJournalItem = (ok=%v, noPasskey=%v), want (true, false): a renderable sibling must render", ok, noPasskey)
	}
	if it.AniListID != 2 {
		t.Errorf("AniListID = %d, want 2 (the first RENDERABLE occurrence in AniList-ID order)", it.AniListID)
	}
	if it.Title == "" {
		t.Error("Title is empty, want the sibling's synthesized title")
	}
	if it.DownloadVolumeFactor != dvfBest {
		t.Errorf("marker = %q, want %q (best-wins fold must still span the unrenderable occurrence)", it.DownloadVolumeFactor, dvfBest)
	}
}

// TestRenderJournalItemOrderIndependentForDuplicateRelationRows pins the
// TOTALITY of renderJournalItem's synthesis order for the shape the ordering
// exists to close: two occurrences of one journal key that share their AniList
// ID, URL, tracker AND info hash (a duplicated trs relation row) while carrying
// different Files. Ordering only on the torrent's identity bytes leaves those
// rows comparing equal, so SortStableFunc keeps catalogue order and the served
// GUID alternates between two titles and sizes as the catalogue is re-fetched.
// Both permutations must render the same observable item.
func TestRenderJournalItemOrderIndependentForDuplicateRelationRows(t *testing.T) {
	w := newTestWriter(filepath.Join(t.TempDir(), "feed.json"), "", false)
	hash := strings.Repeat("b", 40)
	// One entry, two identical-identity relation rows with different payloads.
	entry := &seadex.Entry{AniListID: 7, Torrents: []seadex.Torrent{
		{
			Tracker: "Nyaa", URL: "https://nyaa.si/view/77", InfoHash: hash,
			Files: []seadex.File{{Length: 9, Name: "Show - S01E01 (1080p) [A].mkv"}},
		},
		{
			Tracker: "Nyaa", URL: "https://nyaa.si/view/77", InfoHash: hash,
			Files: []seadex.File{{Length: 12, Name: "Show - S01E02 (1080p) [B].mkv"}},
		},
	}}
	forward := []curatedRef{
		{entry: entry, torrent: &entry.Torrents[0]},
		{entry: entry, torrent: &entry.Torrents[1]},
	}
	reversed := []curatedRef{forward[1], forward[0]}
	infoFor := func(int) EntryInfo { return EntryInfo{} }

	first, ok, noPasskey := w.renderJournalItem("nyaa:77", forward, infoFor)
	if !ok || noPasskey {
		t.Fatalf("renderJournalItem(forward) = (ok=%v, noPasskey=%v), want (true, false)", ok, noPasskey)
	}
	if first.Title == "" {
		t.Fatal("renderJournalItem(forward) Title is empty, want a synthesized title")
	}
	second, ok, noPasskey := w.renderJournalItem("nyaa:77", reversed, infoFor)
	if !ok || noPasskey {
		t.Fatalf("renderJournalItem(reversed) = (ok=%v, noPasskey=%v), want (true, false)", ok, noPasskey)
	}
	if first.Title != second.Title {
		t.Errorf("Title = %q under reversed catalogue order, want %q: duplicated relation rows must not depend on catalogue order",
			second.Title, first.Title)
	}
	if first.Size != second.Size {
		t.Errorf("Size = %d under reversed catalogue order, want %d", second.Size, first.Size)
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
		Owners:    owns(keyed("nyaa:99", true)),
		Published: map[string]bool{"nyaa:99": true},
		NyaaFeed: []journalItem{
			{item: item{Title: "Show - S01 (1080p) [W]", GUID: "https://nyaa.si/view/99", DownloadURL: "https://nyaa.si/download/99.torrent", InfoHash: hash, PubDate: time.Now().UTC()}, Key: "nyaa:99", AniListID: 8, FirstSeen: time.Now().UTC()},
		},
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
	if err := newLoggedExcludingTestWriter(path, log).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 0 {
		t.Errorf("nyaa feed = %+v, want empty (the carried item's stored hash is warned under a different key)", snap.NyaaFeed)
	}
	if got, ok := rec.AttrValue("indexer feed snapshot written", "journal_warned_dropped"); !ok || got != "1" {
		t.Errorf("snapshot log line journal_warned_dropped = %q (found=%v), want 1 (the hash-retracted carried item); log output:\n%s", got, ok, strings.Join(rec.Messages(), "\n"))
	}
}

// TestRebuildDropsCarriedItemWarnedAcrossTrackers pins the CROSS-SCOPE half of
// the warned-identity rule identitySignals documents ("a curator warning
// against the bytes must retract every tracker listing of them", which is why
// its info hash stays un-namespaced while the publication log's is scope-qualified
// in publicationSignals): the same release cross-posted to Nyaa and AnimeBytes is
// one set of bytes, so an AnimeBytes occurrence tagged Broken must retract the
// carried Nyaa item storing that hash AND keep the un-warned Nyaa occurrence
// out of the search curation set. The three sibling warned tests all warn and
// retract within ONE tracker, so they stay green if the warned graph ever
// becomes scope-aware; this one is what makes the cross-tracker retraction
// fail loudly instead of leaving RSS serving bytes search suppresses.
func TestRebuildDropsCarriedItemWarnedAcrossTrackers(t *testing.T) {
	const sharedHash = "143ed15e5e3df072ae91adaeb149973a887590dd"
	path := filepath.Join(t.TempDir(), "feed.json")
	first := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	writeSnapshotFile(t, path, &snapshot{
		Owners:    owns(),
		Published: map[string]bool{"nyaa:42": true, "nyaa:h:" + sharedHash: true},
		NyaaFeed: []journalItem{
			{item: item{Title: "Show - S01 (1080p) [G]", GUID: "https://nyaa.si/view/42", InfoHash: sharedHash, PubDate: first}, Key: "nyaa:42", AniListID: 7, FirstSeen: first},
		},
	})
	files := []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}}
	entries := []seadex.Entry{{
		AniListID: 7,
		Torrents: []seadex.Torrent{
			{
				Tracker: "AB", URL: "/torrents.php?id=1&torrentid=555", InfoHash: sharedHash,
				IsBest: true, Tags: []string{"Broken"}, Files: files,
			},
			{
				Tracker: "Nyaa", URL: "https://nyaa.si/view/42", InfoHash: sharedHash,
				IsBest: true, Files: files,
			},
		},
	}}
	log, rec := capture.New()
	w := newExcludingTestWriter(path)
	w.log = log
	if err := w.Rebuild(t.Context(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 0 {
		t.Errorf("nyaa feed = %+v, want empty (a warning against the bytes must retract every tracker's listing of them)", snap.NyaaFeed)
	}
	if byKeyOf(&snap)["nyaa:42"] || byHashOf(&snap)[sharedHash] {
		t.Errorf("curation set still marks the cross-posted listing: keys = %v, hashes = %v", byKeyOf(&snap), byHashOf(&snap))
	}
	if got, ok := rec.AttrValue("indexer feed snapshot written", "journal_warned_dropped"); !ok || got != "1" {
		t.Errorf("journal_warned_dropped = %q (found=%v), want 1; log:\n%s", got, ok, strings.Join(rec.Messages(), "\n"))
	}
}

// TestRebuildHashVetoesNoveltyAcrossKeyChange pins the multi-signal novelty
// contract publicationSignals documents ("novelty detection survives one signal
// going missing - a URL-shape change upstream"): a torrent whose info hash is
// already in the publication log (under its tracker scope) must NOT re-enter the journal as new when its
// tracker URL changes shape (a new /view id, i.e. a new journal key). Novelty
// is judged across ALL identity signals, so a re-upload or upstream URL change
// keeping the same bytes never re-broadcasts old curation, while both the new
// key and the hash fold into the publication log.
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
	if !snap.Published["nyaa:9042"] || !snap.Published["nyaa:h:"+hash] {
		t.Errorf("publication log missing the new key or the carried hash: %v", snap.Published)
	}
}

// TestRebuildKeyVetoesNoveltyAcrossHashChange pins the mirror image of
// TestRebuildHashVetoesNoveltyAcrossKeyChange: a torrent whose journal KEY is
// already in the publication log must not re-enter the journal as new when its
// info hash changes (SeaDex correcting a hash, or a same-view-id in-place
// replacement). Novelty is vetoed by ANY seen identity signal - not just the
// hash - and the NEW hash still folds into the publication log even though the
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
	if !snap.Published["nyaa:h:"+hashB] || !snap.Published["nyaa:42"] {
		t.Errorf("publication log missing the new hash or the key (every signal must fold even when not new): %v", snap.Published)
	}
}

// TestRebuildJournalsSameHashIndependentlyPerTracker pins the cross-scope half
// of the ledger rule the two novelty tests above pin per scope: the same bytes
// cross-posted to Nyaa and AnimeBytes are two separately journalable releases,
// so a Nyaa occurrence must not consume the AB occurrence's novelty (or the
// reverse - catalogue iteration order would decide which tracker feed ever
// carries it). The ledger's hash entries are scope-namespaced for exactly this
// reason; asserting the two spellings separately means a change that re-folded
// a bare shared hash could not keep this test green.
func TestRebuildJournalsSameHashIndependentlyPerTracker(t *testing.T) {
	const hash = "143ed15e5e3df072ae91adaeb149973a887590dd"
	files := []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}}
	entries := []seadex.Entry{{
		AniListID: 7,
		Torrents: []seadex.Torrent{
			{
				Tracker: "Nyaa", URL: "https://nyaa.si/view/42", InfoHash: hash,
				IsBest: true, Files: files,
			},
			{
				Tracker: "AB", URL: "/torrents.php?id=1&torrentid=123", InfoHash: hash,
				IsBest: true, Files: files,
			},
		},
	}}
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	if err := newTestWriter(path, "PK", true).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 1 || len(snap.ABFeed) != 1 {
		t.Errorf("feeds = nyaa %d / ab %d items, want 1 each (one tracker's listing must not suppress the other's)",
			len(snap.NyaaFeed), len(snap.ABFeed))
	}
	if !snap.Published["nyaa:h:"+hash] || !snap.Published["ab:h:"+hash] {
		t.Errorf("publication log = %v, want both scope-namespaced hash entries", snap.Published)
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
		Owners:    owns(),
		Published: map[string]bool{"nyaa:42": true},
		NyaaFeed: []journalItem{
			{item: item{Title: "Show - S01 (1080p) [G]", GUID: "https://nyaa.si/view/42", DownloadURL: "https://nyaa.si/download/42.torrent", PubDate: first}, Key: "nyaa:42", AniListID: 7, FirstSeen: first},
		},
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
	items := []journalItem{
		{item: item{Title: "Synth A"}, Key: "nyaa:1"},
		{item: item{Title: "Synth B"}, Key: "nyaa:2"},
		{item: item{Title: "Synth C"}, Key: "nyaa:3"},
	}
	applyTitles(items, map[string]string{"nyaa:1": "", "nyaa:2": "Harvested B"}, titleAudit{})
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

// TestRebuildMirrorTrackerCannotSuppressNyaaJournal pins the tail-tracker
// publication-log guard: AnimeTosho is a Nyaa mirror carrying the IDENTICAL info
// hash, and folding its (never-journalable) occurrence into the publication log
// first - purely a catalogue-order accident - used to mark the Nyaa listing
// of the same bytes as already seen, silently denying it RSS exposure
// forever. A tail-tracker occurrence must contribute nothing to the ledger.
func TestRebuildMirrorTrackerCannotSuppressNyaaJournal(t *testing.T) {
	hash := strings.Repeat("a", 40)
	entries := []seadex.Entry{{
		AniListID: 9,
		Torrents: []seadex.Torrent{
			// The mirror occurrence deliberately sorts FIRST in the catalogue.
			{
				Tracker: "AnimeTosho", URL: "https://animetosho.org/view/900", InfoHash: hash, IsBest: true,
				Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
			},
			{
				Tracker: "Nyaa", URL: "https://nyaa.si/view/900", InfoHash: hash, IsBest: true,
				Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
			},
		},
	}}
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	if err := newTestWriter(path, "", false).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 1 {
		t.Fatalf("nyaa feed = %d items, want 1 (the mirror occurrence must not pre-mark the shared hash seen)", len(snap.NyaaFeed))
	}
	if snap.NyaaFeed[0].Key != "nyaa:900" {
		t.Errorf("journaled key = %q, want nyaa:900", snap.NyaaFeed[0].Key)
	}
}

// TestRebuildBaselineTailTrackerCannotSuppressLaterNyaa pins the fresh-install
// baseline arm of the tail-tracker guard (baselinePublications): an AnimeTosho/
// RuTracker occurrence in the FIRST catalogue must not seed its shared info
// hash into the baseline publication log, or the later supported Nyaa listing of
// the same bytes would be silently denied RSS exposure forever. The growth
// arm is pinned by TestRebuildMirrorTrackerCannotSuppressNyaaJournal; this
// covers baseline construction.
func TestRebuildBaselineTailTrackerCannotSuppressLaterNyaa(t *testing.T) {
	const hash = "143ed15e5e3df072ae91adaeb149973a887590dd"
	path := filepath.Join(t.TempDir(), "feed.json")
	w := newTestWriter(path, "", false)

	tail := seadex.Entry{
		AniListID: 7,
		Torrents: []seadex.Torrent{{
			Tracker:  "AnimeTosho",
			URL:      "https://animetosho.org/view/42",
			InfoHash: hash,
			IsBest:   true,
			Files:    []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
		}},
	}
	if err := w.Rebuild(context.Background(), []seadex.Entry{tail}, nil); err != nil {
		t.Fatalf("baseline Rebuild: %v", err)
	}
	if snap := readSnapshotFile(t, path); snap.Published[hash] {
		t.Fatalf("fresh baseline recorded tail-tracker hash %q; a mirror must not suppress a later Nyaa listing", hash)
	}

	nyaa := tail
	nyaa.Torrents = []seadex.Torrent{tail.Torrents[0]}
	nyaa.Torrents[0].Tracker = "Nyaa"
	nyaa.Torrents[0].URL = "https://nyaa.si/view/42"
	if err := w.Rebuild(context.Background(), []seadex.Entry{nyaa}, nil); err != nil {
		t.Fatalf("Nyaa Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 1 {
		t.Fatalf("Nyaa feed = %d items, want 1 (the tail-tracker baseline must not veto later Nyaa novelty)", len(snap.NyaaFeed))
	}
	if snap.NyaaFeed[0].Key != "nyaa:42" {
		t.Errorf("journaled key = %q, want nyaa:42", snap.NyaaFeed[0].Key)
	}
}

// TestRebuildDropsNonCuratedCarriedItemWithBadGUID pins carryStoredItem's
// GUID-identity gate on the NON-curated carry arm: unlike a still-curated
// item there is no fresh render to self-heal from, and reload derives the
// SERVED download link from the GUID, so a stored GUID that no longer proves
// the item's journal identity (foreign-host, cross-key, or empty - a
// hand-edited or corrupted snapshot) must drop the item instead of planting
// a fetch target for a different torrent for the item's whole journal
// window, and the drop is counted on the snapshot log line.
func TestRebuildDropsNonCuratedCarriedItemWithBadGUID(t *testing.T) {
	tests := []struct {
		name string
		guid string
	}{
		{"foreign-host stored GUID", "https://evil.example/view/42"},
		{"cross-key stored GUID", "https://nyaa.si/view/43"},
		{"empty stored GUID", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "feed.json")
			first := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
			writeSnapshotFile(t, path, &snapshot{
				Owners:    owns(),
				Published: map[string]bool{"nyaa:42": true},
				NyaaFeed: []journalItem{
					{item: item{Title: "Show - S01 (1080p) [G]", GUID: tc.guid, PubDate: first}, Key: "nyaa:42", AniListID: 7, FirstSeen: first},
				},
			})
			log, rec := capture.New()
			if err := NewFeedWriter(&FeedWriterConfig{Path: path, UpstreamConfig: UpstreamConfig{NyaaTorznabURL: "http://prowlarr/1/api"}}, log, nil).Rebuild(context.Background(), nil, nil); err != nil {
				t.Fatalf("Rebuild: %v", err)
			}
			if snap := readSnapshotFile(t, path); len(snap.NyaaFeed) != 0 {
				t.Errorf("nyaa feed = %+v, want empty (a non-curated carry has no fresh render to self-heal from, so a GUID that no longer proves the item's identity must drop it)", snap.NyaaFeed)
			}
			if got, ok := rec.AttrValue("indexer feed snapshot written", "journal_dropped"); !ok || got != "1" {
				t.Errorf("journal_dropped = %q (found=%v), want 1; log:\n%s", got, ok, strings.Join(rec.Messages(), "\n"))
			}
		})
	}
}

// TestRebuildFallsBackFromUnpublishableOccurrence pins the sibling fallback of
// the render's creation-time GUID-to-Key gate: two entries share one journal
// key (nyaa:77), and the LOWER AniList id - the one the deterministic
// synthesis order tries first - carries an out-of-range-port page URL
// (":65536" parses, so trackerKey/journalKey still mint nyaa:77, but
// the publisher's 16-bit port check drops it). Without the
// journalIdentityMatches fallback that occurrence would win and journal an
// unpublishable empty GUID (dropped again on every reader load), instead of
// the publishable sibling.
func TestRebuildFallsBackFromUnpublishableOccurrence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	entries := []seadex.Entry{
		{AniListID: 1, Torrents: []seadex.Torrent{{
			Tracker: "Nyaa", URL: "https://nyaa.si:65536/view/77",
			Files: []seadex.File{{Length: 9, Name: "Show - S01E01 (1080p) [G].mkv"}},
		}}},
		{AniListID: 2, Torrents: []seadex.Torrent{{
			Tracker: "Nyaa", URL: "https://nyaa.si/view/77",
			Files: []seadex.File{{Length: 9, Name: "Show - S01E01 (1080p) [G].mkv"}},
		}}},
	}

	if err := newTestWriter(path, "", false).Rebuild(t.Context(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 1 {
		t.Fatalf("feed = %d items, want 1 from the publishable sibling", len(snap.NyaaFeed))
	}
	if got := snap.NyaaFeed[0]; got.AniListID != 2 || got.GUID != "https://nyaa.si/view/77" {
		t.Errorf("journal item = (AniListID=%d, GUID=%q), want the publishable sibling (2, %q)", got.AniListID, got.GUID, "https://nyaa.si/view/77")
	}
}

// TestApplyTitlesReportsPackDisagreementOnce pins the title-vs-file-list
// diagnostic: a harvested title whose season-pack verdict contradicts the
// release's own file census is warned ONCE per rebuild (the onset latch, so a
// systematically drifting upstream cannot flood one rebuild), the warning
// carries the journal key and both verdicts, and it never carries the raw
// title - untrusted tracker text the decode tags runesafe.Untrusted.
//
// The disagreement pinned here is the class the audit does NOT correct (the
// title names an episode, the file census proves a pack): it keeps warning with
// corrected=false and the harvested title still wins the served title verbatim.
// The corrected class has its own tests below.
func TestApplyTitlesReportsPackDisagreementOnce(t *testing.T) {
	log, rec := capture.New()
	w := newLoggedTestWriter(filepath.Join(t.TempDir(), "feed.json"), log)
	items := []journalItem{
		{item: item{Title: "Show S01E01 [720p]"}, Key: "nyaa:1"},
		{item: item{Title: "Other S01E01 [720p]"}, Key: "nyaa:2"},
		{item: item{Title: "Third S01E01 [720p]"}, Key: "nyaa:3"},
		{item: item{Title: "Fourth S01E01 [720p]"}, Key: "nyaa:4"},
	}
	titles := map[string]string{
		"nyaa:1": "Show - S01E01 [1080p]",   // says episode, census says pack: reported
		"nyaa:2": "Other - S01E01 [1080p]",  // disagrees too, but the latch holds
		"nyaa:3": "Third - S01 [1080p]",     // agrees with the census
		"nyaa:4": "Fourth - S01E01 [1080p]", // agrees the other way
	}
	census := map[string]packCensus{
		"nyaa:1": {evidence: packEvidencePack},
		"nyaa:2": {evidence: packEvidencePack},
		"nyaa:3": {evidence: packEvidencePack},
		"nyaa:4": {evidence: packEvidenceSingle, marker: "S01E01"},
	}
	applyTitles(items, titles, titleAudit{census: census, report: w.packDisagreementReporter()})

	const msg = "indexer feed title and file list disagree about a season pack"
	if got := rec.CountExact(msg); got != 1 {
		t.Errorf("disagreement warnings = %d, want 1 (onset-latched per rebuild); log output:\n%s", got, strings.Join(rec.Messages(), "\n"))
	}
	if got := rec.CountLevel(slog.LevelWarn, msg); got != 1 {
		t.Errorf("disagreement warnings at WARN = %d, want 1", got)
	}
	for key, want := range map[string]string{"key": "nyaa:1", "title_pack": "false", "files_pack": "true", "corrected": "false"} {
		if got, ok := rec.AttrValue(msg, key); !ok || got != want {
			t.Errorf("warning attr %s = %q (found=%v), want %q", key, got, ok, want)
		}
	}
	// The raw harvested title must not reach the log, in the message or in any
	// attribute value.
	for _, r := range rec.Records() {
		if strings.Contains(r.Message, "1080p") {
			t.Errorf("log message carries raw title text: %q", r.Message)
		}
		r.Attrs(func(a slog.Attr) bool {
			if strings.Contains(a.Value.String(), "1080p") {
				t.Errorf("log attr %s carries raw title text: %q", a.Key, a.Value.String())
			}
			return true
		})
	}
	// The served title is untouched by the audit: every harvested title wins.
	for i := range items {
		if want := titles[items[i].Key]; items[i].Title != want {
			t.Errorf("item %s title = %q, want the harvested title %q", items[i].Key, items[i].Title, want)
		}
	}
}

// TestApplyTitlesAuditSilentWithoutCensus pins the audit's two no-op guards: a
// key with no census entry (a carried item whose torrent left the catalogue) and
// a title that says nothing about packs are both silent, so the diagnostic
// reports only genuine contradictions.
func TestApplyTitlesAuditSilentWithoutCensus(t *testing.T) {
	log, rec := capture.New()
	w := newLoggedTestWriter(filepath.Join(t.TempDir(), "feed.json"), log)
	items := []journalItem{
		{item: item{Title: "Show S01E01"}, Key: "nyaa:1"},
		{item: item{Title: "Other S01E01"}, Key: "nyaa:2"},
	}
	titles := map[string]string{"nyaa:1": "Show - S01", "nyaa:2": "some unparseable name"}
	applyTitles(items, titles, titleAudit{
		// nyaa:1 absent, nyaa:2's title says nothing about packs.
		census: map[string]packCensus{"nyaa:2": {evidence: packEvidencePack}},
		report: w.packDisagreementReporter(),
	})
	if rec.Contains("disagree about a season pack") {
		t.Errorf("diagnostic fired without a comparable verdict pair; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
	for i := range items {
		if want := titles[items[i].Key]; items[i].Title != want {
			t.Errorf("item %s title = %q, want the harvested title %q", items[i].Key, items[i].Title, want)
		}
	}
}

// TestCensusPacksFoldsOccurrences pins the per-key census the audit compares
// against: it reads the three-valued file evidence per journal key, folds a
// key's several curated occurrences to the STRONGEST evidence so the result
// cannot depend on catalogue order, and carries the single-episode marker a
// correction is built from (only for the single-episode grade).
func TestCensusPacksFoldsOccurrences(t *testing.T) {
	packFiles := []seadex.File{
		{Name: "Show S01E01 [1080p].mkv", Length: 1 << 30},
		{Name: "Show S01E02 [1080p].mkv", Length: 1 << 30},
	}
	singleFiles := []seadex.File{{Name: "Show S01E07 [1080p].mkv", Length: 1 << 30}}
	entry := &seadex.Entry{AniListID: 7}
	cur := map[string][]curatedRef{
		"nyaa:1": {{entry: entry, torrent: &seadex.Torrent{Files: packFiles}}},
		"nyaa:2": {{entry: entry, torrent: &seadex.Torrent{Files: singleFiles}}},
		"nyaa:3": {
			{entry: entry, torrent: &seadex.Torrent{Files: singleFiles}},
			{entry: entry, torrent: &seadex.Torrent{Files: packFiles}},
		},
		"nyaa:4": {{entry: entry, torrent: &seadex.Torrent{}}},
	}
	got := censusPacks(cur)
	want := map[string]packCensus{
		"nyaa:1": {evidence: packEvidencePack},
		"nyaa:2": {evidence: packEvidenceSingle, marker: "S01E07"},
		"nyaa:3": {evidence: packEvidencePack},
		"nyaa:4": {evidence: packEvidenceUnknown},
	}
	for key, want := range want {
		if got[key] != want {
			t.Errorf("censusPacks[%s] = %+v, want %+v", key, got[key], want)
		}
	}
	if len(got) != len(cur) {
		t.Errorf("censusPacks returned %d keys, want %d (one per journal key)", len(got), len(cur))
	}
}

// TestApplyTitlesCorrectsProvablyWrongSeasonClaim pins the one intervention the
// audit makes on a served title: a harvested title claiming a whole SEASON over
// a file list that positively proves ONE episode has its season token rewritten
// into the season+episode form the census names. Sonarr parses FullSeason from
// such a title, ranks it above loose episodes and then treats the season as
// covered, so the operator silently ends up missing that season's real episodes.
//
// The rewrite is surgical on purpose, and this test pins that: the tracker's own
// group, resolution and codec bytes SURVIVE. Falling back to the synthesized
// title would fix the pack claim and throw exactly those bytes away - strictly
// worse for the arr's matching - and dropping the item is forbidden outright.
func TestApplyTitlesCorrectsProvablyWrongSeasonClaim(t *testing.T) {
	log, rec := capture.New()
	w := newLoggedTestWriter(filepath.Join(t.TempDir(), "feed.json"), log)
	entry := &seadex.Entry{AniListID: 7}
	cur := map[string][]curatedRef{
		"nyaa:1": {{entry: entry, torrent: &seadex.Torrent{
			Files: []seadex.File{{Name: "Show S01E07.mkv", Length: 1 << 30}},
		}}},
		"nyaa:2": {{entry: entry, torrent: &seadex.Torrent{
			Files: []seadex.File{{Name: "Show - 07.mkv", Length: 1 << 30}},
		}}},
	}
	items := []journalItem{
		{item: item{Title: "Synth 1"}, Key: "nyaa:1"},
		{item: item{Title: "Synth 2"}, Key: "nyaa:2"},
	}
	titles := map[string]string{
		"nyaa:1": "Show - S01 [1080p][x265]-GRP",
		"nyaa:2": "Show Season 2",
	}
	applyTitles(items, titles, titleAudit{census: censusPacks(cur), report: w.packDisagreementReporter()})

	if got, want := items[0].Title, "Show - S01E07 [1080p][x265]-GRP"; got != want {
		t.Errorf("served title = %q, want the season token corrected in place %q", got, want)
	}
	// Named explicitly: these are the bytes a whole-title replacement would lose.
	for _, frag := range []string{"[1080p]", "[x265]", "-GRP"} {
		if !strings.Contains(items[0].Title, frag) {
			t.Errorf("corrected title %q lost the tracker's own %q text", items[0].Title, frag)
		}
	}
	// The absolute "- NN" census marker renders against the title's own season.
	if got, want := items[1].Title, "Show S02E07"; got != want {
		t.Errorf("served title = %q, want the absolute census marker as %q", got, want)
	}

	const msg = "indexer feed title and file list disagree about a season pack"
	if got := rec.CountExact(msg); got != 1 {
		t.Errorf("disagreement warnings = %d, want 1 (onset-latched per rebuild); log output:\n%s", got, strings.Join(rec.Messages(), "\n"))
	}
	for key, want := range map[string]string{"key": "nyaa:1", "title_pack": "true", "files_pack": "false", "corrected": "true"} {
		if got, ok := rec.AttrValue(msg, key); !ok || got != want {
			t.Errorf("warning attr %s = %q (found=%v), want %q", key, got, ok, want)
		}
	}
}

// TestTitleAuditRefusesACorrectionThatOverflowsThePersistedCap pins
// correctedTitle's size gate. Splicing the census's episode half over a title's
// season token GROWS the title, and an over-cap title is pruned by the shared
// decode gate on the next load (validPersistedItem) - so the release would
// vanish from RSS entirely rather than merely carrying a wrong season claim.
// Serving the wrong claim with a diagnostic is the lesser loss, so a correction
// that would not fit is refused and the harvested title is served verbatim.
func TestTitleAuditRefusesACorrectionThatOverflowsThePersistedCap(t *testing.T) {
	// A season-only title (no SxxExx token, so packFromTitle reads a whole
	// season) sized to exactly the persisted cap: splicing "E05" over its "S01"
	// token would push it three bytes past.
	const tail = " - S01"
	title := strings.Repeat("A", maxPersistedFieldBytes-len(tail)) + tail
	if len(title) != maxPersistedFieldBytes {
		t.Fatalf("fixture title = %d bytes, want exactly the %d-byte cap", len(title), maxPersistedFieldBytes)
	}
	var calls int
	var gotKey string
	var gotTitlePack, gotFilesPack, gotCorrected bool
	audit := titleAudit{
		census: map[string]packCensus{"nyaa:42": {marker: "S01E05", evidence: packEvidenceSingle}},
		report: func(key string, titlePack, filesPack, corrected bool) {
			calls++
			gotKey, gotTitlePack, gotFilesPack, gotCorrected = key, titlePack, filesPack, corrected
		},
	}

	if served := audit.served("nyaa:42", title); served != title {
		t.Errorf("served title = %d bytes, want the %d-byte harvested title unchanged; an over-cap correction loses the whole item at reload",
			len(served), len(title))
	}
	if calls != 1 || gotKey != "nyaa:42" || !gotTitlePack || gotFilesPack || gotCorrected {
		t.Errorf("report = (calls %d, key %q, titlePack %v, filesPack %v, corrected %v), want (1, nyaa:42, true, false, false)",
			calls, gotKey, gotTitlePack, gotFilesPack, gotCorrected)
	}

	// The same title three bytes shorter IS corrected, so the refusal above pins
	// the cap rather than a rewrite that never happens.
	shorter := strings.Repeat("A", maxPersistedFieldBytes-len(tail)-len("E05")) + tail
	if served := audit.served("nyaa:42", shorter); served == shorter {
		t.Errorf("served title = %q unchanged, want the season token corrected when the rewrite fits the cap", served)
	}
}

// TestApplyTitlesLeavesUnknownCensusEvidenceAlone pins why the census is
// three-valued: zero recognized episode tokens - an absent file list, or naming
// outside the recognized forms - proves NOTHING, so a season-claiming title over
// it is neither a disagreement nor a correction. It is served verbatim and
// silently, exactly as before the correction existed. Reading absence as
// single-episode evidence is the false correction a two-valued census would have
// introduced.
func TestApplyTitlesLeavesUnknownCensusEvidenceAlone(t *testing.T) {
	log, rec := capture.New()
	w := newLoggedTestWriter(filepath.Join(t.TempDir(), "feed.json"), log)
	entry := &seadex.Entry{AniListID: 7}
	cur := map[string][]curatedRef{
		"nyaa:1": {{entry: entry, torrent: &seadex.Torrent{}}},
		"nyaa:2": {{entry: entry, torrent: &seadex.Torrent{
			Files: []seadex.File{{Name: "01.mkv", Length: 1 << 30}},
		}}},
	}
	items := []journalItem{
		{item: item{Title: "Synth 1"}, Key: "nyaa:1"},
		{item: item{Title: "Synth 2"}, Key: "nyaa:2"},
	}
	titles := map[string]string{"nyaa:1": "Show - S01", "nyaa:2": "Show - S01"}
	applyTitles(items, titles, titleAudit{census: censusPacks(cur), report: w.packDisagreementReporter()})

	for i := range items {
		if want := titles[items[i].Key]; items[i].Title != want {
			t.Errorf("item %s title = %q, want the harvested title untouched %q", items[i].Key, items[i].Title, want)
		}
	}
	if rec.Contains("disagree about a season pack") {
		t.Errorf("absent episode evidence reported as a disagreement; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestCorrectedUpstreamRecordJournalsAsNew is the central behavioural claim of
// the publication-log rewrite, and the class of loss it recovers.
//
// The ledger write used to sit on the novelty TEST rather than on the journal
// ADMISSION, so every keyed torrent the pass merely LOOKED AT was recorded -
// before anything decided whether it was servable. SeaDex publishing a record
// with an empty file list therefore burned that release's novelty permanently
// (the log is never pruned) with nothing published, and a curator adding the file
// list an hour later could never get it onto RSS. Search still found it, which is
// exactly why the loss was silent: the app looked healthy.
//
// Recording on publication instead makes the second pass admit it.
func TestCorrectedUpstreamRecordJournalsAsNew(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)

	// Pass 1: the record has a parseable tracker key but no files and no release
	// group, so no title synthesizes and nothing is journaled.
	defective := []seadex.Entry{{
		AniListID: 7,
		Torrents:  []seadex.Torrent{{Tracker: "Nyaa", URL: "https://nyaa.si/view/7", IsBest: true}},
	}}
	w := newTestWriter(path, "", false)
	if err := w.Rebuild(context.Background(), defective, nil); err != nil {
		t.Fatalf("Rebuild (defective record): %v", err)
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 0 {
		t.Fatalf("nyaa feed = %+v, want empty: a title-less item cannot be journaled", snap.NyaaFeed)
	}
	if snap.Published["nyaa:7"] {
		t.Fatalf("publication log recorded nyaa:7 though nothing was served: %v", snap.Published)
	}

	// Pass 2: the curator adds the file list. This is a legitimate later
	// republish and must journal as new.
	corrected := []seadex.Entry{nyaaEntry(7, 7, true, "Show - S01E01 (1080p) [G].mkv")}
	if err := newTestWriter(path, "", false).Rebuild(context.Background(), corrected, nil); err != nil {
		t.Fatalf("Rebuild (corrected record): %v", err)
	}
	snap = readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 1 {
		t.Fatalf("nyaa feed = %+v, want the corrected record journaled as new", snap.NyaaFeed)
	}
	if !snap.Published["nyaa:7"] {
		t.Errorf("publication log missing nyaa:7 after it was actually served: %v", snap.Published)
	}
}

// TestPublishedReleaseIsNeverReadmitted is the other half of the same rule, and
// the property that must survive the change: recording on publication must not
// weaken the guard against a catalogue RE-BROADCAST. A release that was served
// once stays recorded forever - you cannot un-serve something - so a later pass
// carries it rather than journaling it again.
func TestPublishedReleaseIsNeverReadmitted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	entries := []seadex.Entry{nyaaEntry(7, 7, true, "Show - S01E01 (1080p) [G].mkv")}

	for pass := 1; pass <= 3; pass++ {
		if err := newTestWriter(path, "", false).Rebuild(context.Background(), entries, nil); err != nil {
			t.Fatalf("Rebuild (pass %d): %v", pass, err)
		}
		snap := readSnapshotFile(t, path)
		if len(snap.NyaaFeed) != 1 {
			t.Fatalf("pass %d: nyaa feed = %d items, want exactly 1 (journaled once, carried thereafter): %v",
				pass, len(snap.NyaaFeed), feedKeys(snap.NyaaFeed))
		}
	}
}
