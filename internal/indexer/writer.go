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
	// maxPersistedFieldBytes caps each persisted feed item's string field
	// (title, GUID/info/download URL, journal key). It equals torznab.go's
	// maxUpstreamFieldBytes, so every harvested title and Prowlarr URL fits
	// by construction; only an external value with no other bound (a SeaDex
	// filename synthesized into a title can approach the 48 MiB page limit)
	// is rejected. Without a per-item cap, one such value could pass the
	// whole-snapshot maxFeedBytes check and reach renderFeed, whose XML
	// escaping expands an ampersand-heavy title ~5x - enough to drive peak
	// memory past the 256 MiB container limit and OOM the indexer instead
	// of degrading.
	maxPersistedFieldBytes = 4096
	// maxPersistedCategories caps one persisted item's category list. The
	// writer unions at most the three Torznab ids the feed uses (TV, Anime,
	// Movies); anything larger is a hand-edited snapshot.
	maxPersistedCategories = 8
	// reasonMalformed is loadPrevious's baseline reason for a structurally invalid
	// previous snapshot (bad JSON, missing curation maps, or an over-limit item/title).
	reasonMalformed = "malformed"
)

// validPersistedItem reports whether one feed item respects the shared
// persisted-item limits: every string field under maxPersistedFieldBytes, the
// category list under maxPersistedCategories, and the non-negative numeric
// domain both producers guarantee (toItem clamps size/seeders/leechers to
// >= 0; totalSize returns 0 on negative/overflowing sums), so a hand-edited
// or corrupted snapshot with a negative value is rejected at load instead of
// rendering an invalid enclosure length/size attr. Enforced when
// renderJournalItem creates an item (an oversized external value counts as
// unresolvable) and re-checked after every snapshot unmarshal (loadPrevious
// re-baselines; the server's readSnapshot treats it as malformed), so an
// over-limit item can neither be persisted nor served.
func validPersistedItem(it *journalItem) bool {
	if it.Size < 0 || it.Seeders < 0 || it.Leechers < 0 {
		return false
	}
	for _, f := range []string{it.Title, it.GUID, it.InfoURL, it.DownloadURL, it.InfoHash, it.DownloadVolumeFactor, it.Key} {
		if len(f) > maxPersistedFieldBytes {
			return false
		}
	}
	return len(it.Categories) <= maxPersistedCategories
}

// validFeedItems reports whether every item in the given feeds respects the
// shared persisted-item limits (see validPersistedItem).
func validFeedItems(feeds ...[]journalItem) bool {
	for _, feed := range feeds {
		for i := range feed {
			if !validPersistedItem(&feed[i]) {
				return false
			}
		}
	}
	return true
}

// decodeSnapshot unmarshals persisted snapshot bytes and applies the
// structural-validity gate BOTH consumers share (the server's readSnapshot
// and the writer's loadPrevious): valid JSON, the required curation maps
// present (the writer always persists both, even empty, so nil maps identify
// a structurally invalid snapshot without rejecting a valid empty feed), and
// every feed item within the shared persisted-item limits. err reports
// malformed JSON; a non-empty reason names a structural violation.
// Consumer-specific ingress checks (the writer's titles-cache cap) stay with
// their consumer.
func decodeSnapshot(data []byte) (snap snapshot, reason string, err error) {
	if err := json.Unmarshal(data, &snap); err != nil {
		return snapshot{}, "", err
	}
	if snap.ByHash == nil || snap.ByKey == nil {
		return snapshot{}, "missing required curation maps", nil
	}
	if !validFeedItems(snap.NyaaFeed, snap.ABFeed) {
		return snapshot{}, "item exceeds persisted-item limits", nil
	}
	return snap, "", nil
}

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
	ByHash map[string]bool `json:"by_hash"`
	ByKey  map[string]bool `json:"by_key"`
	// ByPair is the hash/key pair relation (pairKey of an info hash and a
	// tracker key observed on the same SeaDex torrent) lookup's cross-torrent
	// gate reads. Persisted without omitempty so a freshly written snapshot
	// always carries the map (even empty) and only a genuinely legacy
	// snapshot decodes it nil.
	ByPair map[string]bool `json:"by_pair"`
	// Seen is persisted without omitempty for the same reason as ByPair: its
	// nil-ness is loadPrevious's pre-journal-schema sentinel, so an honestly
	// empty ledger must round-trip as {} rather than aliasing the retired
	// schema and re-baselining every cycle (see loadPrevious).
	Seen   map[string]bool   `json:"seen"`
	Titles map[string]string `json:"titles,omitempty"`
	// HarvestCursor is the title harvest's rotation position: the
	// "scope:alID" of the last show group that consumed a harvest query, so
	// the next rebuild resumes AFTER it instead of restarting at the head
	// (see harvestTitles; a deep show can then never starve its successors
	// across rebuilds). Optional both ways: an older snapshot without it
	// starts at the head, and an older binary ignores it.
	HarvestCursor string        `json:"harvest_cursor,omitempty"`
	NyaaFeed      []journalItem `json:"nyaa_feed"`
	ABFeed        []journalItem `json:"ab_feed"`
}

// FeedWriterConfig configures NewFeedWriter. Path is where the snapshot is
// persisted (config.DefaultIndexerFeedPath in production). SeaDexBaseURL is
// the releases.moe site base the per-item info links are built under
// (config.DefaultSeaDexBaseURL in production; empty falls back to the same
// default). The embedded
// UpstreamConfig mirrors the server's Config - the shared upstream vocabulary
// has one home so the writer queries exactly the trackers the server proxies.
// ABPasskey gates which AnimeBytes releases are journalable (a secret; empty
// leaves AnimeBytes without grabbable RSS links) - the writer never persists
// it: AB items are stored GUID-only and the server derives their served
// download links from its own configured passkey (see rebuildABDownloadURLs).
// An empty Torznab URL is that tracker's off switch (its journal is neither
// built nor persisted), and the configured upstreams also power the title
// harvest (see harvest.go).
type FeedWriterConfig struct {
	Path          string
	SeaDexBaseURL string
	UpstreamConfig
}

// FeedWriter builds the feed snapshot from a SeaDex fetch and persists it
// atomically for the server to read. It holds no SeaDex/Fribb clients of its
// own - the compare cycle owns the shared fetch and hands the results to
// Rebuild - but it does hold the Prowlarr upstreams (the same ones the server
// proxies searches through) for the title harvest.
type FeedWriter struct {
	log            *slog.Logger
	now            func() time.Time
	path           string
	abPasskey      string
	seadexBaseURL  string
	upstreams      []*upstream
	nyaaConfigured bool
	abConfigured   bool
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
	base := cfg.SeaDexBaseURL
	if base == "" {
		base = defaultSeaDexBaseURL
	}
	w := &FeedWriter{
		log:            log,
		now:            time.Now,
		path:           cfg.Path,
		abPasskey:      cfg.ABPasskey,
		seadexBaseURL:  base,
		nyaaConfigured: cfg.NyaaTorznabURL != "",
		abConfigured:   abConfigured,
	}
	if deps.HTTP != nil {
		w.upstreams = wireUpstreams(deps.HTTP, log, cfg.UpstreamConfig)
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
	entries, ws := splitCurationWarned(entries)
	set := buildCuration(entries)
	now := w.now()

	var js journalStats
	var nyaa, ab []journalItem
	seen, titles := prev.seen, prev.titles
	if prev.baseline {
		seen, titles = allIdentities(entries), map[string]string{}
		w.log.Info("indexer feed journal baselined; RSS feed starts empty and grows from newly curated releases",
			"reason", prev.reason, "seen", len(seen))
	} else {
		cur := indexCurated(entries)
		if w.nyaaConfigured {
			nyaa = w.carryJournal(prev.nyaaFeed, cur, &ws, infoFor, now, &js)
		}
		if w.abConfigured {
			ab = w.carryJournal(prev.abFeed, cur, &ws, infoFor, now, &js)
		}
		newNyaa, newAB := w.growJournal(entries, cur, seen, infoFor, now, &js)
		nyaa = append(nyaa, newNyaa...)
		ab = append(ab, newAB...)
	}
	if !w.nyaaConfigured {
		nyaa = nil
	}
	if !w.abConfigured {
		ab = nil
	}
	feeds := map[string][]journalItem{upstreamNyaa: nyaa, upstreamAB: ab}
	hs, cursor := w.harvestTitles(ctx, feeds, titles, infoFor, prev.cursor)
	applyTitles(nyaa, titles)
	applyTitles(ab, titles)
	nyaa, ab = sortFeed(nyaa), sortFeed(ab)
	titles = retainTitles(titles, nyaa, ab)

	snap := snapshot{ByHash: set.byHash, ByKey: set.byKey, ByPair: set.byPair, Seen: seen, Titles: titles, NyaaFeed: nyaa, ABFeed: ab, HarvestCursor: cursor}
	if err := w.persist(ctx, &snap); err != nil {
		return err
	}
	w.log.Info("indexer feed snapshot written",
		"entries", len(entries), "hashes", len(snap.ByHash), "keys", len(snap.ByKey),
		"nyaa_feed", len(snap.NyaaFeed), "ab_feed", len(snap.ABFeed),
		"warned_excluded", len(ws.keys),
		"journal_new", js.added, "journal_pruned", js.pruned, "journal_dropped", js.dropped,
		"journal_warned_dropped", js.warned,
		"journal_clock_rebased", js.rebased,
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
// strips BOTH feeds' download URLs (stripABDownloadURLs /
// stripNyaaDownloadURLs) so no passkey is ever serialized and the snapshot
// stays GUID-only for fetch targets: the reader re-derives every served link
// from the item's tracker page URL on load. The wholesale Nyaa strip also
// scrubs an AB-scoped item misplaced in the Nyaa feed, so a scope mismatch
// cannot leak a passkey either.
func (w *FeedWriter) persist(ctx context.Context, snap *snapshot) error {
	stripABDownloadURLs(snap.ABFeed)
	stripNyaaDownloadURLs(snap.NyaaFeed)
	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("indexer: encode feed snapshot: %w", err)
	}
	if _, err := atomicfile.WriteFile(ctx, w.path, data,
		atomicfile.WithMkdirMode(feedDirMode), atomicfile.WithMode(feedFileMode),
		atomicfile.WithMaxBytes(maxFeedBytes)); err != nil {
		if errors.Is(err, atomicfile.ErrFileTooLarge) {
			return fmt.Errorf("indexer: feed snapshot %d bytes exceeds max %d; keeping previous feed", len(data), maxFeedBytes)
		}
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
// carried items still embed a passkey on the first rebuild over it.
func stripABDownloadURLs(feed []journalItem) {
	for i := range feed {
		feed[i].DownloadURL = ""
	}
}

// stripNyaaDownloadURLs blanks every Nyaa feed item's download URL before
// persistence, mirroring stripABDownloadURLs: the Nyaa link is public and
// carries no credential, but keeping the snapshot GUID-only for fetch targets
// on BOTH feeds means /config/feed.json is never authoritative for what the
// arrs download - the reader re-derives each served Nyaa link from the item's
// tracker page URL on load (see rebuildNyaaDownloadURLs), so a tampered
// snapshot cannot plant an arbitrary fetch target. Blanking the whole feed
// also subsumes the key-scoped scrub of an ab:-keyed item a legacy or
// corrupted snapshot misplaced in nyaa_feed (its passkey-bearing link is
// blanked with everything else).
func stripNyaaDownloadURLs(feed []journalItem) {
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
	cursor   string
	seen     map[string]bool
	titles   map[string]string
	nyaaFeed []journalItem
	abFeed   []journalItem
	baseline bool
}

// loadPrevious reads the persisted snapshot's journal bookkeeping. A missing
// file (or a path whose parent is not a directory) is the fresh-install
// baseline; a decoded snapshot without a seen ledger is the retired
// whole-catalogue schema and re-baselines (the journal contract: treat it as
// absent); malformed JSON and an over-cap file warn and re-baseline
// (self-healing - both are deterministic for unchanged bytes, and the seen
// ledger is rebuilt from the current catalogue, so nothing old can re-enter
// the journal). Any other read failure (EACCES, EIO) is returned as an error
// so a TRANSIENT fault cannot blank a live journal: the caller keeps the
// last-good snapshot and the next cycle retries.
func (w *FeedWriter) loadPrevious(ctx context.Context) (previousJournal, error) {
	data, err := atomicfile.ReadBounded(ctx, w.path, maxFeedBytes)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR):
			return previousJournal{baseline: true, reason: "fresh-install"}, nil
		case errors.Is(err, atomicfile.ErrFileTooLarge):
			// Deterministic, not transient: persist enforces the same
			// maxFeedBytes cap, so an over-cap snapshot can only come from
			// external corruption or hand-editing and never shrinks on its
			// own - returning an error here would wedge every future rebuild
			// on the same file. Treat it like malformed JSON: warn and
			// re-baseline; the rebuild's persist atomically replaces the
			// oversized file, so the state self-heals.
			w.log.Warn("previous feed snapshot exceeds size cap; re-baselining the feed journal",
				"path", w.path, "max_bytes", int64(maxFeedBytes))
			return previousJournal{baseline: true, reason: "oversized"}, nil
		}
		return previousJournal{}, fmt.Errorf("indexer: read previous feed snapshot %s: %w", w.path, err)
	}
	snap, structReason, decodeErr := decodeSnapshot(data)
	if decodeErr != nil {
		w.log.Warn("previous feed snapshot malformed; re-baselining the feed journal", "path", w.path, "error", decodeErr)
		return previousJournal{baseline: true, reason: reasonMalformed}, nil
	}
	if structReason != "" {
		// The offending value itself is never logged: it can be
		// attacker-shaped multi-megabyte text.
		w.log.Warn("previous feed snapshot malformed; re-baselining the feed journal",
			"path", w.path, "reason", structReason)
		return previousJournal{baseline: true, reason: reasonMalformed}, nil
	}
	for k, t := range snap.Titles {
		if len(k) > maxPersistedFieldBytes || len(t) > maxPersistedFieldBytes {
			// The titles cache is an ingress of its own: applyTitles overwrites
			// carried items' titles AFTER renderJournalItem's creation-time
			// check, so an over-limit cached title would let a rebuild persist
			// a snapshot the server's reload rejects. The value itself is never
			// logged: it can be attacker-shaped multi-megabyte text.
			w.log.Warn("previous feed snapshot malformed; re-baselining the feed journal",
				"path", w.path, "reason", "cached title exceeds persisted-item limits")
			return previousJournal{baseline: true, reason: reasonMalformed}, nil
		}
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
		cursor:   snap.HarvestCursor,
	}, nil
}

// warnedSet is the curation-warned exclusion set splitCurationWarned builds
// and the carry side consumes as one value: keys holds the excluded journal
// keys (every directly warned occurrence plus every duplicate removed through
// a shared identity - also the warned_excluded operator count), and ids holds
// the directly-warned identity-signal set (journal key AND info hash), which
// retracts uses to drop a previously journaled item whose stored info hash is
// warned under a DIFFERENT tracker key.
type warnedSet struct {
	keys map[string]struct{}
	ids  map[string]struct{}
}

// retracts reports whether a carried journal item shares a warned identity:
// its key is excluded, or its stored info hash is warned under any tracker
// key (RSS must never keep serving bytes search suppresses).
func (ws *warnedSet) retracts(it *journalItem) bool {
	if _, bad := ws.keys[it.Key]; bad {
		return true
	}
	if it.InfoHash != "" {
		if _, bad := ws.ids[it.InfoHash]; bad {
			return true
		}
	}
	return false
}

// splitCurationWarned partitions the catalogue for the feed: it returns a
// copy of entries with every curation-warned torrent (release.CurationWarned
// over the SeaDex tags: Broken/Incomplete) removed, plus the warnedSet the
// carry side consumes (see warnedSet for the two sets it holds and
// warnedSet.retracts for the retraction decision). The warning wins BY
// IDENTITY, not per
// occurrence: a torrent can be attached to several SeaDex entries, and when
// one occurrence is tagged Broken/Incomplete while a duplicate of the same
// tracker key is not, keeping the unwarned duplicate would let proxied
// searches serve and mark the release while carryJournal (which consumes the
// any-occurrence key set) removes it from RSS - the two indexer paths would
// disagree about whether the release is grabbable. So a first pass collects
// every warned identity signal - journal key AND info hash (identitySignals,
// the package's one identity definition) - across the whole catalogue, and a
// second pass
// removes every occurrence that is warned itself OR shares a warned identity.
// Filtering at the source keeps every downstream consumer honest at once: the
// search curation set never marks a warned release (a Prowlarr result
// matching one is purged as uncurated), the journal never grows one, and the
// seen ledger never records one - so when a warning is lifted the torrent
// becomes grabbable curation for the first time and journals as new (a
// torrent journaled BEFORE it was warned stays in the persisted ledger, so
// un-warning it never re-broadcasts it). The input is never mutated: the
// cycle shares the entries slice with the compare pass, so an entry
// containing a removed torrent gets a fresh filtered Torrents slice.
func splitCurationWarned(entries []seadex.Entry) (kept []seadex.Entry, ws warnedSet) {
	ws.keys, ws.ids = collectWarnedIdentities(entries)
	kept = make([]seadex.Entry, len(entries))
	for i := range entries {
		kept[i] = entries[i]
		if unwarned, changed := filterWarnedTorrents(entries[i].Torrents, ws.ids, ws.keys); changed {
			kept[i].Torrents = unwarned
		}
	}
	return kept, ws
}

// collectWarnedIdentities is splitCurationWarned's first pass: keys holds the
// warned journal keys (carryJournal's drop set and the warned_excluded count),
// all holds every warned identity signal (journal key AND info hash, the
// package's identitySignals definition), so a duplicate occurrence sharing a
// warned torrent's info hash under a different or unparseable URL is excluded
// too.
func collectWarnedIdentities(entries []seadex.Entry) (keys, all map[string]struct{}) {
	keys, all = make(map[string]struct{}), make(map[string]struct{})
	for i := range entries {
		for j := range entries[i].Torrents {
			t := &entries[i].Torrents[j]
			if !release.CurationWarned(t.Tags) {
				continue
			}
			if k := journalKey(t); k != "" {
				keys[k] = struct{}{}
			}
			for _, id := range identitySignals(t) {
				all[id] = struct{}{}
			}
		}
	}
	return keys, all
}

// filterWarnedTorrents is splitCurationWarned's second pass for one entry's
// torrents: it drops every occurrence that is warned itself OR shares a
// warned identity signal (journal key or info hash), reporting whether
// anything was removed (the caller only swaps in the fresh slice then,
// keeping the shared input unmutated). Every removed occurrence's journal key
// is folded into warnedKeys - the carry-drop set carryJournal consumes - so a
// duplicate excluded only through a shared identity (e.g. a warned sibling's
// info hash) still retracts its previously journaled item from RSS, keeping
// the identity filter the single policy point.
func filterWarnedTorrents(ts []seadex.Torrent, warnedIDs, warnedKeys map[string]struct{}) ([]seadex.Torrent, bool) {
	unwarned := make([]seadex.Torrent, 0, len(ts))
	changed := false
	for j := range ts {
		t := &ts[j]
		identityWarned := false
		for _, id := range identitySignals(t) {
			if _, ok := warnedIDs[id]; ok {
				identityWarned = true
				break
			}
		}
		if release.CurationWarned(t.Tags) || identityWarned {
			if k := journalKey(t); k != "" {
				warnedKeys[k] = struct{}{}
			}
			changed = true
			continue
		}
		unwarned = append(unwarned, *t)
	}
	return unwarned, changed
}

// buildCuration builds the search curation index over the whole SeaDex
// catalogue: every torrent's info hash and tracker key mapped to whether any
// entry marks that release best (OR-accumulated for a torrent attached to
// several entries), plus the pair relation - which hash/key combinations
// were observed on one and the same torrent - that lookup's cross-torrent
// gate reads. Searches match Prowlarr results against it; unlike the
// RSS journal it always reflects the full current curation set.
func buildCuration(entries []seadex.Entry) curation {
	set := curation{byHash: make(map[string]bool), byKey: make(map[string]bool), byPair: make(map[string]bool)}
	for i := range entries {
		for j := range entries[i].Torrents {
			t := &entries[i].Torrents[j]
			h := validInfoHash(t.InfoHash)
			k := trackerKey(t.Tracker, t.URL)
			if h != "" {
				set.byHash[h] = set.byHash[h] || t.IsBest
			}
			if k != "" {
				set.byKey[k] = set.byKey[k] || t.IsBest
			}
			if h != "" && k != "" {
				set.byPair[pairKey(h, k)] = true
			}
		}
	}
	return set
}
