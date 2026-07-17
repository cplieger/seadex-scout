package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"syscall"
	"time"

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

const (
	feedDirMode = 0o755
	// feed.json is persisted GUID-only - AB items carry no passkey-bearing
	// download URL (see stripABDownloadURLs) - but it stays owner-only as
	// defense in depth for that invariant, and a legacy snapshot may still
	// embed a passkey until the first rebuild scrubs it. The daemon and the
	// `poll` subcommand both run as the same container user, so 0o600 stays
	// read/write-compatible.
	feedFileMode = 0o600
	// maxFeedBytes bounds the persisted feed snapshot, enforced on write and
	// read alike so a rebuild can never persist a snapshot the server's reload
	// would then reject.
	maxFeedBytes = 64 << 20
)

// snapshot is the materialized feed a cycle produces and the server serves:
// the search curation index (info hash / tracker key -> isBest, matched
// against Prowlarr results), the two synthesized per-tracker RSS journals
// (NyaaFeed/ABFeed: the newly-curated releases of the last feedJournalMaxAge,
// each item carrying its journal bookkeeping - see journal.go), the
// never-pruned seen ledger novelty is judged against, and the harvested-title
// cache. Persisting it is what lets one data engine (the compare cycle) feed
// both the findings and the Torznab feed from a single SeaDex fetch, and lets
// a cycle run by the `poll` subcommand refresh a resident daemon's feed
// across the process boundary. Field names are the on-disk JSON keys; a
// snapshot without a seen ledger is the retired pre-journal schema and
// re-baselines (see loadPrevious).
type snapshot struct {
	ByHash   map[string]bool   `json:"by_hash"`
	ByKey    map[string]bool   `json:"by_key"`
	Seen     map[string]bool   `json:"seen,omitempty"`
	Titles   map[string]string `json:"titles,omitempty"`
	NyaaFeed []item            `json:"nyaa_feed"`
	ABFeed   []item            `json:"ab_feed"`
}

// FeedWriterConfig configures NewFeedWriter. Path is where the snapshot is
// persisted (config.DefaultIndexerFeedPath in production). ABPasskey gates
// which AnimeBytes releases are journalable (a secret; empty leaves
// AnimeBytes without grabbable RSS links) - the writer never persists it: AB
// items are stored GUID-only and the server derives their served download
// links from its own configured passkey (see rebuildABDownloadURLs). The
// Torznab URLs and Prowlarr key mirror the server's Config: an empty URL is
// that tracker's off switch (its journal is neither built nor persisted), and
// the configured upstreams also power the title harvest (see harvest.go), so
// the writer queries exactly the trackers the server proxies.
type FeedWriterConfig struct {
	Path           string
	ABPasskey      string
	NyaaTorznabURL string
	ABTorznabURL   string
	ProwlarrAPIKey string
}

// FeedWriter builds the feed snapshot from a SeaDex fetch and persists it
// atomically for the server to read. It holds no SeaDex/Fribb clients of its
// own - the compare cycle owns the shared fetch and hands the results to
// Rebuild - but it does hold the Prowlarr upstreams (the same ones the server
// proxies searches through) for the title harvest.
type FeedWriter struct {
	log          *slog.Logger
	now          func() time.Time
	path         string
	abPasskey    string
	upstreams    []*upstream
	abConfigured bool
}

// NewFeedWriter returns a FeedWriter for cfg. deps carries the HTTP client
// the title harvest queries Prowlarr with (nil disables harvesting - items
// then keep their synthesized titles) and the logger (nil falls back to
// slog.Default). An AnimeBytes passkey configured for an unconfigured AB
// tracker (no ab_torznab_url - the README's off switch) is warned once here,
// field names only, so a half-configured AnimeBytes intent surfaces at boot;
// no passkey-embedded links are ever persisted for that off tracker.
func NewFeedWriter(cfg *FeedWriterConfig, deps Deps) *FeedWriter {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	abConfigured := cfg.ABTorznabURL != ""
	if cfg.ABPasskey != "" && !abConfigured {
		log.Warn("indexer.ab_passkey is set but indexer.ab_torznab_url is empty; the AnimeBytes feed stays off")
	}
	w := &FeedWriter{
		log:          log,
		now:          time.Now,
		path:         cfg.Path,
		abPasskey:    cfg.ABPasskey,
		abConfigured: abConfigured,
	}
	if deps.HTTP != nil {
		w.upstreams = wireUpstreams(deps.HTTP, log, cfg.NyaaTorznabURL, cfg.ABTorznabURL, cfg.ProwlarrAPIKey)
	}
	return w
}

// Rebuild refreshes the persisted feed snapshot from the SeaDex entries
// (categorized and titled via info, the per-show metadata closure the cycle
// builds over its persisted state; nil is valid and falls back to file-name
// synthesis). Curation-warned torrents (SeaDex tags them Broken/Incomplete)
// are excluded first - from the search curation set, the seen ledger, and the
// journal alike - and a previously journaled item whose torrent has since
// been warned is dropped, so the arrs can never grab a release the curators
// warn against (see splitCurationWarned). It rebuilds the search curation set
// from the whole catalogue, then advances the RSS journal: newly curated
// torrents (absent from the seen ledger) enter with a first-seen timestamp,
// carried items re-render from current data, items older than
// feedJournalMaxAge age out, and the title harvest upgrades synthesized
// titles to real tracker titles within its query budget (a harvest failure
// degrades to synthesized titles, never fails the rebuild). On the first run,
// after a schema upgrade, or over a malformed previous snapshot it baselines:
// the entire current curation set is recorded as seen and the journal starts
// empty, growing only from genuinely new curation (backfill is search's job).
// The caller skips a failed SeaDex fetch, so this errors only on a
// previous-snapshot read failure (transient; the last-good feed stays served)
// or on the persist side: an encode failure, a snapshot exceeding
// maxFeedBytes (kept out so the reader never rejects what a rebuild wrote),
// or the atomic write itself failing.
func (w *FeedWriter) Rebuild(ctx context.Context, entries []seadex.Entry, info func(alID int) EntryInfo) error {
	infoFor := entryInfoFunc(info)
	prev, err := w.loadPrevious(ctx)
	if err != nil {
		return err
	}
	entries, warned := splitCurationWarned(entries)
	set := buildCuration(entries)
	now := w.now()

	var js journalStats
	var nyaa, ab []item
	seen, titles := prev.seen, prev.titles
	if prev.baseline {
		seen, titles = allIdentities(entries), map[string]string{}
		w.log.Info("indexer feed journal baselined; RSS feed starts empty and grows from newly curated releases",
			"reason", prev.reason, "seen", len(seen))
	} else {
		cur := indexCurated(entries)
		nyaa = w.carryJournal(prev.nyaaFeed, cur, warned, infoFor, now, &js)
		ab = w.carryJournal(prev.abFeed, cur, warned, infoFor, now, &js)
		newNyaa, newAB := w.growJournal(entries, cur, seen, infoFor, now, &js)
		nyaa = append(nyaa, newNyaa...)
		ab = append(ab, newAB...)
	}
	if !w.abConfigured {
		ab = nil
	}
	feeds := map[string][]item{upstreamNyaa: nyaa, upstreamAB: ab}
	hs := w.harvestTitles(ctx, feeds, titles, infoFor)
	applyTitles(nyaa, titles)
	applyTitles(ab, titles)
	nyaa, ab = sortAndCap(nyaa), sortAndCap(ab)
	titles = retainTitles(titles, nyaa, ab)

	snap := snapshot{ByHash: set.byHash, ByKey: set.byKey, Seen: seen, Titles: titles, NyaaFeed: nyaa, ABFeed: ab}
	if err := w.persist(ctx, &snap); err != nil {
		return err
	}
	w.log.Info("indexer feed snapshot written",
		"entries", len(entries), "hashes", len(snap.ByHash), "keys", len(snap.ByKey),
		"nyaa_feed", len(snap.NyaaFeed), "ab_feed", len(snap.ABFeed),
		"warned_excluded", len(warned),
		"journal_new", js.added, "journal_pruned", js.pruned, "journal_dropped", js.dropped,
		"journal_warned_dropped", js.warned,
		"skipped_unresolvable", js.unresolvable,
		"harvest_queries", hs.queries, "harvest_matched", hs.matched, "harvest_pending", hs.pending)
	if js.abSkippedNoPasskey > 0 && w.abConfigured {
		w.log.Warn("ab RSS feed empty of grabbable links: set indexer.ab_passkey to serve AnimeBytes releases",
			"ab_releases_skipped", js.abSkippedNoPasskey)
	}
	return nil
}

// persist atomically writes the snapshot, mirroring the reader's size bound
// before committing: a snapshot the reload would reject must not replace the
// last-good file, or the next restart starts with an empty feed. It first
// strips the AB feed's download URLs so no passkey is ever serialized (see
// stripABDownloadURLs).
func (w *FeedWriter) persist(ctx context.Context, snap *snapshot) error {
	stripABDownloadURLs(snap.ABFeed)
	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("indexer: encode feed snapshot: %w", err)
	}
	if int64(len(data)) > maxFeedBytes {
		return fmt.Errorf("indexer: feed snapshot %d bytes exceeds max %d; keeping previous feed", len(data), maxFeedBytes)
	}
	if _, err := atomicfile.WriteFile(ctx, w.path, data,
		atomicfile.WithMkdirMode(feedDirMode), atomicfile.WithMode(feedFileMode)); err != nil {
		return fmt.Errorf("indexer: write feed snapshot %s: %w", w.path, err)
	}
	return nil
}

// stripABDownloadURLs blanks every AnimeBytes feed item's download URL before
// persistence: an AB download link embeds the operator's passkey, and the
// snapshot must stay GUID-only so /config/feed.json never holds that
// credential at rest. Nothing is lost - the server re-derives each served AB
// link from the item's non-secret tracker page URL (the GUID) and the
// currently configured passkey on every load (see rebuildABDownloadURLs) -
// and running at the persist choke point also scrubs a legacy snapshot whose
// carried items still embed a passkey on the first rebuild over it. Nyaa
// items live in their own feed and keep their public .torrent links.
func stripABDownloadURLs(feed []item) {
	for i := range feed {
		feed[i].DownloadURL = ""
	}
}

// previousJournal is the journal bookkeeping loaded from the previous
// snapshot: the seen ledger, the harvested-title cache, and the two persisted
// journal feeds. baseline marks that no usable previous journal exists
// (reason: fresh-install, the retired pre-journal schema, or a malformed
// file) and the rebuild must baseline instead of growing.
type previousJournal struct {
	reason   string
	seen     map[string]bool
	titles   map[string]string
	nyaaFeed []item
	abFeed   []item
	baseline bool
}

// loadPrevious reads the persisted snapshot's journal bookkeeping. A missing
// file (or a path whose parent is not a directory) is the fresh-install
// baseline; a decoded snapshot without a seen ledger is the retired
// whole-catalogue schema and re-baselines (the journal contract: treat it as
// absent); malformed JSON warns and re-baselines (self-healing - the seen
// ledger is rebuilt from the current catalogue, so nothing old can re-enter
// the journal). Any other read failure (EACCES, EIO, an over-cap file) is
// returned as an error so a TRANSIENT fault cannot blank a live journal: the
// caller keeps the last-good snapshot and the next cycle retries.
func (w *FeedWriter) loadPrevious(ctx context.Context) (previousJournal, error) {
	data, err := atomicfile.ReadBounded(ctx, w.path, maxFeedBytes)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return previousJournal{baseline: true, reason: "fresh-install"}, nil
		}
		return previousJournal{}, fmt.Errorf("indexer: read previous feed snapshot %s: %w", w.path, err)
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		w.log.Warn("previous feed snapshot malformed; re-baselining the feed journal", "path", w.path, "error", err)
		return previousJournal{baseline: true, reason: "malformed"}, nil
	}
	if snap.Seen == nil {
		return previousJournal{baseline: true, reason: "pre-journal-schema"}, nil
	}
	titles := snap.Titles
	if titles == nil {
		titles = map[string]string{}
	}
	return previousJournal{
		nyaaFeed: snap.NyaaFeed,
		abFeed:   snap.ABFeed,
		seen:     snap.Seen,
		titles:   titles,
	}, nil
}

// splitCurationWarned partitions the catalogue for the feed: it returns a
// copy of entries with every curation-warned torrent (release.CurationWarned
// over the SeaDex tags: Broken/Incomplete) removed, plus the warned torrents'
// journal keys, which carryJournal uses to drop a previously journaled item
// whose torrent has since been warned. Filtering at the source keeps every
// downstream consumer honest at once: the search curation set never marks a
// warned release (a Prowlarr result matching one is purged as uncurated), the
// journal never grows one, and the seen ledger never records one - so when a
// warning is lifted the torrent becomes grabbable curation for the first time
// and journals as new (a torrent journaled BEFORE it was warned stays in the
// persisted ledger, so un-warning it never re-broadcasts it). The input is
// never mutated: the cycle shares the entries slice with the compare pass, so
// an entry containing a warned torrent gets a fresh filtered Torrents slice.
func splitCurationWarned(entries []seadex.Entry) (kept []seadex.Entry, warned map[string]struct{}) {
	warned = make(map[string]struct{})
	kept = make([]seadex.Entry, len(entries))
	for i := range entries {
		kept[i] = entries[i]
		ts := entries[i].Torrents
		if !anyCurationWarned(ts) {
			continue
		}
		unwarned := make([]seadex.Torrent, 0, len(ts))
		for j := range ts {
			if release.CurationWarned(ts[j].Tags) {
				if k := journalKey(&ts[j]); k != "" {
					warned[k] = struct{}{}
				}
				continue
			}
			unwarned = append(unwarned, ts[j])
		}
		kept[i].Torrents = unwarned
	}
	return kept, warned
}

// anyCurationWarned reports whether any torrent carries a curation-warning
// tag, so splitCurationWarned only copies the torrent slices it must filter.
func anyCurationWarned(ts []seadex.Torrent) bool {
	for i := range ts {
		if release.CurationWarned(ts[i].Tags) {
			return true
		}
	}
	return false
}

// buildCuration builds the search curation index over the whole SeaDex
// catalogue: every torrent's info hash and tracker key mapped to whether any
// entry marks that release best (OR-accumulated for a torrent attached to
// several entries). Searches match Prowlarr results against it; unlike the
// RSS journal it always reflects the full current curation set.
func buildCuration(entries []seadex.Entry) curation {
	set := curation{byHash: make(map[string]bool), byKey: make(map[string]bool)}
	for i := range entries {
		for j := range entries[i].Torrents {
			t := &entries[i].Torrents[j]
			if h := validInfoHash(t.InfoHash); h != "" {
				set.byHash[h] = set.byHash[h] || t.IsBest
			}
			if k := trackerKey(t.Tracker, t.URL); k != "" {
				set.byKey[k] = set.byKey[k] || t.IsBest
			}
		}
	}
	return set
}
