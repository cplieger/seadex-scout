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
	// feedDirMode keeps an auto-created feed.json parent owner-only,
	// mirroring the report-dir 0o700 rationale: the snapshot's GUIDs are
	// private-tracker page URLs, so the directory stays unlistable by
	// other users as defense in depth.
	feedDirMode = 0o700
	// feedFileMode keeps the persisted snapshot owner-only: feed.json is
	// GUID-only - AB items carry no passkey-bearing download URL (see
	// stripDownloadURLs) - but it stays owner-only as defense in depth for
	// that invariant, and a legacy snapshot may still embed a passkey until
	// the first rebuild scrubs it. The daemon and the `poll` subcommand both
	// run as the same container user, so 0o600 stays read/write-compatible.
	feedFileMode = 0o600
	// maxFeedBytes bounds the persisted feed snapshot, enforced on write and
	// read alike so a rebuild can never persist a snapshot the server's reload
	// would then reject. It is sized against the DECODED cost, not the file:
	// json.Unmarshal turns the curation indexes into map[string]bool, where a
	// minimal entry ("nyaa:1":true, 14 JSON bytes) costs ~48+ bytes of live
	// heap, so the cap must stay several times below the 256 MiB container
	// limit (the same budget maxPersistedFieldBytes is reasoned against) or a
	// corrupted/hand-edited file OOMs the process inside New's warm-up reload -
	// before the listener serves, and again on every restart, crashlooping the
	// compare loop with it. The whole current SeaDex catalogue plus the
	// never-pruned seen ledger and a 14-day journal serialize to a few MB, so
	// 16 MiB leaves ample headroom for years of growth while bounding the
	// decoded blow-up.
	maxFeedBytes = 16 << 20
	// feedSizeWarnBytes is the pre-cliff warning threshold (80% of
	// maxFeedBytes): crossing the bound refuses every subsequent persist and
	// freezes the served RSS journal with no self-heal (the offending input
	// never shrinks on its own), so persist warns while there is still
	// headroom to act. Mirrors internal/state's stateSizeWarnBytes.
	feedSizeWarnBytes = maxFeedBytes / 10 * 8
	// maxPersistedFieldBytes caps each persisted feed item's string field
	// (title, GUID/info/download URL, journal key). It is aliased to
	// torznab.go's maxUpstreamFieldBytes so every harvested title and
	// Prowlarr URL fits by construction and a raise of the upstream cap can
	// never silently outgrow the persisted cap; only an external value with
	// no other bound (a SeaDex filename synthesized into a title can
	// approach the 48 MiB page limit) is rejected. Without a per-item cap,
	// one such value could pass the whole-snapshot maxFeedBytes check and
	// reach renderFeed, whose XML escaping expands an ampersand-heavy
	// title ~5x - enough to drive peak memory past the 256 MiB container
	// limit and OOM the indexer instead of degrading.
	maxPersistedFieldBytes = maxUpstreamFieldBytes
	// maxPersistedCategories caps one persisted item's category list. The
	// writer unions at most the three Torznab ids the feed uses (TV, Anime,
	// Movies); anything larger is a hand-edited snapshot.
	maxPersistedCategories = 8
	// maxPersistedCursorBytes bounds the persisted harvest checkpoint
	// (harvest_cursor). It is deliberately far above maxPersistedFieldBytes:
	// an honest checkpoint carries one Pages entry per still-pending deep
	// show, so a few hundred live entries legitimately exceed the per-field
	// cap (see TestLoadPreviousPreservesLargeHarvestCheckpoint), while 64 KiB
	// still bounds the one persisted string that is carried forward verbatim.
	maxPersistedCursorBytes = 64 << 10
	// maxPersistedSeenBytes bounds the seen ledger IN AGGREGATE - the
	// ledger's twin of maxPersistedCursorBytes, and for the same reason.
	// Seen is the other map carried forward VERBATIM and it is never pruned
	// (growJournal only ever adds; allIdentities replaces it only on a
	// baseline), so a hand-edited or corrupted ledger that is itself under
	// maxFeedBytes yet large enough that the REBUILT snapshot (the carried
	// ledger plus this cycle's new identities) crosses maxFeedBytes makes
	// persist fail ErrFileTooLarge on every cycle with no self-heal: the
	// file never shrinks, and loadPrevious keeps accepting it because every
	// individual key is within maxPersistedFieldBytes. Re-baselining
	// instead rebuilds the ledger from the current catalogue, which is
	// exactly what it should hold, and persist then atomically replaces the
	// offending file. The whole live SeaDex catalogue's identity signals
	// serialize to ~1 MB, so 8 MiB leaves years of headroom while staying
	// well inside maxFeedBytes.
	maxPersistedSeenBytes = 8 << 20
	// reasonMalformed is loadPrevious's baseline reason for a structurally invalid
	// previous snapshot (bad JSON, missing curation maps, or an over-limit item/title).
	reasonMalformed = "malformed"
	// msgSnapshotMalformed is the one operator-facing malformed-rebaseline
	// message loadPrevious's three Warn sites share (tests pin the exact text).
	msgSnapshotMalformed = "previous feed snapshot malformed; re-baselining the feed journal"
)

// --- Persisted-item and snapshot validity ---

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
	if len(it.Categories) > maxPersistedCategories {
		return false
	}
	for _, category := range it.Categories {
		if category <= 0 {
			return false
		}
	}
	return true
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
//
// It also canonicalizes each accepted item's non-derived wire fields
// (normalizeSnapshotItems) HERE rather than in one consumer, because
// identity is compared by both: the writer's carry gates match a persisted
// item's InfoHash against the current catalogue's canonical hashes
// (warnedSet.retracts), so a non-canonical at-rest hash (uppercase or padded)
// would miss a warning retraction and keep re-persisting a curator-warned
// release, while the server saw the canonical form. The
// download-volume-factor marker is canonicalized in the same pass: the
// writer carries a non-curated item's stored render verbatim, so an
// out-of-vocabulary at-rest marker would otherwise be re-persisted on every
// rebuild while the arr acted on it.
//
// InfoURL is canonicalized here for the same reason
// (sanitizeSnapshotInfoURLs, whose blanked counts are returned PER TRACKER FEED
// so a caller can attribute them: the server WARNs once per affected feed on
// reload, while the writer ignores them - its rebuild re-persists the scrubbed
// form regardless): the field belongs to the persisted contract both ends must
// see canonical, not just the render path. The writer carries a non-curated item
// forward verbatim (carryStoredItem) and persist scrubs only DownloadURL, so a
// foreign-host or javascript: InfoURL planted
// in feed.json would otherwise be re-persisted on every rebuild for up to
// feedJournalMaxAge while only the reader blanked it at serve time.
//
// The derived PubDate is re-established here for the same reason
// (normalizeSnapshotPubDates): it is persisted as an independent field but
// documented to mirror FirstSeen, and neither consumer re-derives it for an
// item it carries or serves verbatim.
func decodeSnapshot(data []byte) (snap snapshot, scrub snapshotScrub, reason string, err error) {
	if err := json.Unmarshal(data, &snap); err != nil {
		return snapshot{}, snapshotScrub{}, "", err
	}
	if snap.ByHash == nil || snap.ByKey == nil {
		return snapshot{}, snapshotScrub{}, "missing required curation maps", nil
	}
	if !validFeedItems(snap.NyaaFeed, snap.ABFeed) {
		return snapshot{}, snapshotScrub{}, "item exceeds persisted-item limits", nil
	}
	normalizeSnapshotItems(snap.NyaaFeed)
	normalizeSnapshotItems(snap.ABFeed)
	normalizeSnapshotPubDates(snap.NyaaFeed)
	normalizeSnapshotPubDates(snap.ABFeed)
	scrub = snapshotScrub{
		blankedInfoURLs: map[string]int{
			upstreamNyaa: sanitizeSnapshotInfoURLs(snap.NyaaFeed),
			upstreamAB:   sanitizeSnapshotInfoURLs(snap.ABFeed),
		},
	}
	return snap, scrub, "", nil
}

// snapshotScrub carries decodeSnapshot's at-rest corrections keyed by TRACKER
// SCOPE rather than summed. An operator reading one combined count cannot tell
// whether the Nyaa or the AnimeBytes journal was tampered with, which is the
// whole diagnostic value of the line (l-f176) - and summing is how the
// attribution was lost when the shared-gate refactor moved the two calls into
// one expression (d-u8c3-1).
type snapshotScrub struct {
	blankedInfoURLs map[string]int
}

// normalizeSnapshotPubDates restores the journal's PubDate-mirrors-FirstSeen
// invariant (see journalItem) on every accepted item, for BOTH consumers: the
// reader renders PubDate straight into the served <pubDate>, and the writer's
// non-curated carry arm keeps a stored item verbatim, so a persisted PubDate
// that diverged from FirstSeen (a hand-edited or legacy snapshot) would
// otherwise be advertised to the arrs for the item's whole journal window - a
// far-future value can hold a release behind a delay profile indefinitely and
// mis-sorts a newest-first view. An item with no FirstSeen carries no journal
// timestamp to mirror, so it has nothing vouching for its stored PubDate
// either and the field is cleared rather than left alone.
func normalizeSnapshotPubDates(feed []journalItem) {
	for i := range feed {
		if feed[i].FirstSeen.IsZero() {
			// No journal timestamp to mirror, so there is nothing that
			// vouches for the stored PubDate either: the writer stamps
			// FirstSeen and PubDate together on every item it creates, and
			// rebaseFutureFeed can only correct a value it can compare
			// against FirstSeen. Clearing it makes writeItem omit
			// <pubDate> instead of advertising a hand-edited far-future
			// date the reader has no way to bound (the writer drops such
			// an item at carry, so this only changes what the READER
			// serves).
			feed[i].PubDate = time.Time{}
			continue
		}
		feed[i].PubDate = feed[i].FirstSeen
	}
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
	// HarvestCursor is the title harvest's persisted resumption state: an
	// encoded harvestCheckpoint (see decodeHarvestCheckpoint), NOT a single
	// key. It carries the rotation position - the "scope:alID" of the last
	// show group that consumed a harvest query, so the next rebuild resumes
	// AFTER it instead of restarting at the head (see harvestTitles; a deep
	// show can then never starve its successors across rebuilds) - plus each
	// still-paging group's next offset page, so a show cut off by
	// harvestShowPageCap resumes DEEPER on its next visit instead of
	// re-querying page zero forever. Backward compatible both ways: a
	// pages-less checkpoint encodes as the bare legacy "scope:alID" cursor an
	// older binary reads, an older snapshot without the field starts at the
	// head, and its size is deliberately NOT capped by
	// maxPersistedFieldBytes (an honest Pages map legitimately exceeds it).
	HarvestCursor string        `json:"harvest_cursor,omitempty"`
	NyaaFeed      []journalItem `json:"nyaa_feed"`
	ABFeed        []journalItem `json:"ab_feed"`
}

// --- Writer construction ---

// FeedWriterConfig configures NewFeedWriter. Path is where the snapshot is
// persisted (config.DefaultIndexerFeedPath in production). Per-item info
// links are built under the canonical SeaDex site base (feed.go's
// defaultSeaDexBaseURL - the same constant the reader's InfoURL allowlist is
// derived from, so the two ends of the persisted contract cannot drift). The
// embedded UpstreamConfig mirrors the server's Config - the shared upstream
// vocabulary has one home so the writer queries exactly the trackers the
// server proxies. ABPasskey gates which AnimeBytes releases are journalable
// (a secret; empty leaves AnimeBytes without grabbable RSS links) - the writer
// never persists it: AB items are stored GUID-only and the server derives
// their served download links from its own configured passkey (see
// rebuildABDownloadURLs). An empty Torznab URL is that tracker's off switch
// (its journal is neither built nor persisted), and the configured upstreams
// also power the title harvest (see harvest.go).
type FeedWriterConfig struct {
	Path string
	UpstreamConfig
}

// FeedWriter builds the feed snapshot from a SeaDex fetch and persists it
// atomically for the server to read. It holds no SeaDex/Fribb clients of its
// own - the compare cycle owns the shared fetch and hands the results to
// Rebuild - and no Prowlarr clients either: the title harvest is its own
// component (harvester, see harvest.go), held here as a single collaborator
// because Rebuild is where it runs.
type FeedWriter struct {
	log            *slog.Logger
	now            func() time.Time
	harvest        *harvester
	path           string
	abPasskey      string
	nyaaConfigured bool
	abConfigured   bool
}

// NewFeedWriter returns a FeedWriter for cfg. ups carries the wired Prowlarr
// upstreams the title harvest queries (a zero Upstreams disables harvesting -
// items then keep their synthesized titles) and log may be nil (falls back to
// slog.Default).
//
// The half-configured AnimeBytes intent (a passkey set for a tracker with no
// ab_torznab_url - the README's off switch) is NOT reported here. It is one
// diagnostic rule and internal/config owns it: config validation runs in every
// mode, including modes that construct no FeedWriter, and it deliberately logs
// the condition at INFO because "a deliberately parked passkey must not raise
// Loki alert noise". This constructor used to re-evaluate the identical
// condition at WARN, so a configured feed emitted BOTH lines at boot and the
// WARN re-fired on every `poll` subcommand run - producing exactly the
// warn-level noise the owning site's policy exists to avoid (l-f13). No
// passkey-embedded links are ever persisted for an off tracker regardless.
func NewFeedWriter(cfg *FeedWriterConfig, log *slog.Logger, ups Upstreams) *FeedWriter {
	if log == nil {
		log = slog.Default()
	}
	w := &FeedWriter{
		log:            log,
		now:            time.Now,
		path:           cfg.Path,
		abPasskey:      cfg.ABPasskey,
		nyaaConfigured: cfg.enabled(upstreamNyaa),
		abConfigured:   cfg.enabled(upstreamAB),
	}
	// The harvest reads the writer's clock through a closure over w rather than
	// a copy of the func value: w.now is a live field the test suite replaces
	// AFTER construction (the pacing/time-slice tests drive a fake clock), and
	// the harvest's pacer read it directly before it became its own component.
	// Copying time.Now here instead would fork the two clocks silently.
	w.harvest = newHarvester(log, func() time.Time { return w.now() }, ownUpstreams(ups.ups))
	return w
}

// --- Rebuild and persistence ---

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
func (w *FeedWriter) Rebuild(ctx context.Context, entries []seadex.Entry, info EntryInfoFunc) error {
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
		pass := &journalPass{w: w, cur: indexCurated(entries), seen: seen, ws: &ws, infoFor: infoFor, js: &js, now: now}
		// Carry BOTH journals regardless of configuration: a tracker's off
		// switch must be reversible. Blanking a Torznab URL used to skip the
		// carry, so a single rebuild dropped every journaled item for that
		// scope - while the never-pruned seen ledger kept their identities, so
		// journalIfNew reported isNew=false forever and those releases could
		// never reach RSS again. An operator disabling AnimeBytes for a few days
		// permanently lost the un-grabbed part of its journal window (l-f161).
		// Carrying costs nothing at rest (both feeds are stored GUID-only, see
		// stripDownloadURLs) and nothing on the wire (feedFor serves an
		// unconfigured scope nothing, and its comment already anticipates
		// exactly this stale-snapshot case).
		//
		// Carried items keep AGING OUT on the normal feedJournalMaxAge window
		// rather than freezing (prepareCarriedItem prunes them), so an off
		// tracker's journal stays bounded and a disable longer than the window
		// converges on empty - the operator has effectively opted out of that
		// window - instead of thawing weeks-old releases as "recent" on
		// re-enable.
		//
		// The AB passkey is the tracker's SECOND off switch and is now reversible
		// the same way: carryStoredItem and refreshCarriedItem keep a carried AB
		// item through a passkey-less window instead of dropping it (see those
		// two), since a passkey only supplies the grabbable link.
		nyaa = pass.carryJournal(prev.nyaaFeed, upstreamNyaa)
		ab = pass.carryJournal(prev.abFeed, upstreamAB)
		// Growth stays gated per scope: journalIfNew -> newJournalItem returns
		// early for an unconfigured tracker, so an off tracker's journal shrinks
		// but never grows.
		newNyaa, newAB := pass.growJournal(entries)
		nyaa = append(nyaa, newNyaa...)
		ab = append(ab, newAB...)
	}
	feeds := map[string][]journalItem{upstreamNyaa: nyaa, upstreamAB: ab}
	hs, cursor := w.harvest.harvestTitles(ctx, feeds, titles, infoFor, prev.cursor)
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
		"harvest_queries", hs.queries, "harvest_matched", hs.matched,
		"harvest_rejected", hs.rejected, "harvest_pending", hs.pending)
	if js.abSkippedNoPasskey > 0 && w.abConfigured {
		w.log.Warn("ab RSS feed empty of grabbable links: set indexer.ab_passkey to serve AnimeBytes releases",
			"ab_releases_skipped", js.abSkippedNoPasskey)
	}
	return nil
}

// persist atomically writes the snapshot, mirroring the reader's size bound
// before committing: a snapshot the reload would reject must not replace the
// last-good file, or the next restart starts with an empty feed. It first
// strips BOTH feeds' download URLs (stripDownloadURLs) so no passkey is ever
// serialized and the snapshot stays GUID-only for fetch targets: the reader
// re-derives every served link from the item's tracker page URL on load. The
// wholesale Nyaa strip also scrubs an AB-scoped item misplaced in the Nyaa
// feed, so a scope mismatch cannot leak a passkey either.
func (w *FeedWriter) persist(ctx context.Context, snap *snapshot) error {
	stripDownloadURLs(snap.ABFeed)
	stripDownloadURLs(snap.NyaaFeed)
	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("indexer: encode feed snapshot: %w", err)
	}
	if len(data) > feedSizeWarnBytes {
		w.log.Warn("indexer feed snapshot approaching the size limit; a rebuild that exceeds it is refused and the served feed freezes",
			"path", w.path, "bytes", len(data), "limit", maxFeedBytes)
	}
	if _, err := atomicfile.WriteFile(ctx, w.path, data,
		atomicfile.WithLogger(w.log),
		atomicfile.WithMkdirMode(feedDirMode), atomicfile.WithMode(feedFileMode),
		atomicfile.WithMaxBytes(maxFeedBytes)); err != nil {
		if errors.Is(err, atomicfile.ErrFileTooLarge) {
			return fmt.Errorf("indexer: feed snapshot %d bytes exceeds max %d; keeping previous feed: %w", len(data), maxFeedBytes, err)
		}
		return fmt.Errorf("indexer: write feed snapshot %s: %w", w.path, err)
	}
	return nil
}

// stripDownloadURLs blanks every feed item's download URL before persistence:
// BOTH feeds are GUID-only at rest. For AnimeBytes the download link embeds
// the operator's passkey, and the snapshot must stay GUID-only so
// /config/feed.json never holds that credential at rest; running at the
// persist choke point also scrubs a legacy snapshot whose carried items still
// embed a passkey on the first rebuild over it. The Nyaa link is public and
// carries no credential, but keeping fetch targets GUID-only on BOTH feeds
// means /config/feed.json is never authoritative for what the arrs download -
// a tampered snapshot cannot plant an arbitrary fetch target - and blanking
// the whole Nyaa feed subsumes the key-scoped scrub of an ab:-keyed item a
// legacy or corrupted snapshot misplaced in nyaa_feed. Nothing is lost: the
// server re-derives each served link from the item's non-secret tracker page
// URL (the GUID) on every load (see rebuildABDownloadURLs /
// rebuildNyaaDownloadURLs).
func stripDownloadURLs(feed []journalItem) {
	for i := range feed {
		feed[i].DownloadURL = ""
	}
}

// --- Previous-snapshot loading ---

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
		return w.classifyPreviousReadError(err)
	}
	snap, _, structReason, decodeErr := decodeSnapshot(data)
	if decodeErr != nil {
		// Bounded like the reader's sibling gate (reload.go): a decoder error
		// can embed the offending document text, and feed.json is a
		// tamperable boundary.
		w.log.Warn(msgSnapshotMalformed, "path", w.path, "error", capLogText(decodeErr.Error(), 256))
		return previousJournal{baseline: true, reason: reasonMalformed}, nil
	}
	if structReason != "" {
		// The offending value itself is never logged: it can be
		// attacker-shaped multi-megabyte text.
		w.log.Warn(msgSnapshotMalformed, "path", w.path, "reason", structReason)
		return previousJournal{baseline: true, reason: reasonMalformed}, nil
	}
	if !titleCacheWithinLimits(snap.Titles) {
		// The titles cache is an ingress of its own: applyTitles overwrites
		// carried items' titles AFTER renderJournalItem's creation-time
		// check, so an over-limit cached title would let a rebuild persist
		// a snapshot the server's reload rejects. The value itself is never
		// logged: it can be attacker-shaped multi-megabyte text.
		w.log.Warn(msgSnapshotMalformed,
			"path", w.path, "reason", "cached title exceeds persisted-item limits")
		return previousJournal{baseline: true, reason: reasonMalformed}, nil
	}
	if !seenLedgerWithinLimits(snap.Seen) {
		// The seen ledger is carried forward verbatim and never pruned, so an
		// over-limit identity key from a hand-edited snapshot would otherwise
		// persist in every future snapshot, and a false membership value (which
		// the writer never emits) would make journalIfNew re-broadcast an
		// already-baselined release. An over-aggregate ledger wedges persist
		// on every cycle instead (see maxPersistedSeenBytes). The value itself
		// is never logged.
		w.log.Warn(msgSnapshotMalformed,
			"path", w.path, "reason", "seen ledger is invalid or exceeds its size cap")
		return previousJournal{baseline: true, reason: reasonMalformed}, nil
	}
	if snap.Seen == nil {
		return previousJournal{baseline: true, reason: "pre-journal-schema"}, nil
	}
	titles := make(map[string]string, len(snap.Titles))
	for k, t := range snap.Titles {
		if t != "" {
			titles[k] = t
		}
	}
	cursor := snap.HarvestCursor
	if len(cursor) > maxPersistedCursorBytes {
		// The harvest cursor is the one persisted string carried forward
		// VERBATIM: decodeHarvestCheckpoint keeps an unparseable value as
		// Last and encodeHarvestCheckpoint re-emits it unchanged whenever no
		// group consumed a query this rebuild, so a hand-edited multi-MiB
		// value rides in every future snapshot and can push the rebuilt
		// snapshot past maxFeedBytes - wedging persist on every cycle with no
		// self-heal. Dropping it is the same safe degradation
		// decodeHarvestCheckpoint already applies to malformed checkpoint
		// JSON: rotation restarts at the head and paging at zero. The value
		// itself is never logged (it can be attacker-shaped text).
		w.log.Warn("previous feed snapshot harvest cursor exceeds size cap; restarting the harvest rotation",
			"path", w.path, "max_bytes", maxPersistedCursorBytes, "cursor_bytes", len(cursor))
		cursor = ""
	}
	return previousJournal{
		nyaaFeed: snap.NyaaFeed,
		abFeed:   snap.ABFeed,
		seen:     snap.Seen,
		titles:   titles,
		cursor:   cursor,
	}, nil
}

// seenLedgerWithinLimits reports whether every seen-ledger entry respects the
// producer contract: a bounded identity key mapped to true membership, and the
// ledger as a WHOLE within maxPersistedSeenBytes. Honest
// keys are tracker keys ("scope:digits") and 40-hex info hashes, orders of
// magnitude under the bound; see loadPrevious's ingress checks for why the
// ledger is validated separately (it is the one map the writer carries forward
// verbatim). A false value is only reachable by external corruption or
// hand-editing - the writer only ever records true - and journalIfNew reads
// the VALUE, so carrying one forward would re-broadcast an already-baselined
// release as newly curated. The aggregate cap exists because the per-entry
// bound cannot catch a ledger of per-entry-valid keys that is large enough to
// push the REBUILT snapshot past maxFeedBytes (see maxPersistedSeenBytes):
// that wedges persist on every cycle with no self-heal, so an over-aggregate
// ledger re-baselines instead.
func seenLedgerWithinLimits(seen map[string]bool) bool {
	total := 0
	for k, wasSeen := range seen {
		if len(k) > maxPersistedFieldBytes || !wasSeen {
			return false
		}
		// Each entry serializes as `"<key>":true,`, so charging len(k)
		// alone would under-count a ledger of millions of short keys by
		// more than half.
		total += len(k) + 8
		if total > maxPersistedSeenBytes {
			return false
		}
	}
	return true
}

// classifyPreviousReadError is loadPrevious's transient-versus-baseline
// decision for a snapshot read failure. A missing file (or a path whose
// parent is not a directory) is the fresh-install baseline. An over-cap file
// is deterministic, not transient: persist enforces the same maxFeedBytes
// cap, so an over-cap snapshot can only come from external corruption or
// hand-editing and never shrinks on its own - returning an error there would
// wedge every future rebuild on the same file. Treat it like malformed JSON:
// warn and re-baseline; the rebuild's persist atomically replaces the
// oversized file, so the state self-heals. Any other read failure (EACCES,
// EIO) is returned as an error so a TRANSIENT fault cannot blank a live
// journal.
func (w *FeedWriter) classifyPreviousReadError(err error) (previousJournal, error) {
	switch {
	case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR):
		return previousJournal{baseline: true, reason: "fresh-install"}, nil
	case errors.Is(err, atomicfile.ErrFileTooLarge):
		w.log.Warn("previous feed snapshot exceeds size cap; re-baselining the feed journal",
			"path", w.path, "max_bytes", int64(maxFeedBytes), "error", err)
		return previousJournal{baseline: true, reason: "oversized"}, nil
	default:
		return previousJournal{}, fmt.Errorf("indexer: read previous feed snapshot %s: %w", w.path, err)
	}
}

// titleCacheWithinLimits reports whether every harvested-title cache entry
// (key and title alike) respects maxPersistedFieldBytes (see loadPrevious's
// ingress check for why the cache is validated separately).
func titleCacheWithinLimits(titles map[string]string) bool {
	for k, title := range titles {
		if len(k) > maxPersistedFieldBytes || len(title) > maxPersistedFieldBytes {
			return false
		}
	}
	return true
}

// --- Curation-warned exclusion ---

// warnedSet is the curation-warned exclusion set splitCurationWarned builds
// and the carry side consumes as one value: keys holds the excluded journal
// keys (every directly warned occurrence plus every duplicate removed through
// a shared identity - also the warned_excluded operator count), and ids holds
// the warned identity-signal set (journal key AND info hash), transitively
// closed over shared identities by collectWarnedIdentities' fixpoint, which
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
// IDENTITY, not per occurrence: a torrent can be attached to several SeaDex
// entries, and when one occurrence is tagged Broken/Incomplete while a
// duplicate of the same tracker key is not, keeping the unwarned duplicate
// would let proxied searches serve and mark the release while carryJournal
// (which consumes the any-occurrence key set) removes it from RSS - the two
// indexer paths would disagree about whether the release is grabbable. So a
// first pass collects every warned identity signal - journal key AND info
// hash (identitySignals, the package's one identity definition) - across the
// whole catalogue, and a second pass removes every occurrence that is warned
// itself OR shares a warned identity.
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
		if unwarned, changed := filterWarnedTorrents(entries[i].Torrents, ws.ids); changed {
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
			if release.CurationWarned(t.Tags) {
				markWarnedIdentity(t, keys, all)
			}
		}
	}

	// Warning identity is transitive across occurrences: if A shares a hash
	// with B and B shares its tracker key with C, all three name the same
	// warned release graph and must be excluded together.
	for propagateWarnedIdentities(entries, keys, all) {
	}
	return keys, all
}

// propagateWarnedIdentities runs one sweep of the transitive closure over the
// warned sets: every torrent sharing a warned identity signal has its own
// journal key and signals folded in. It reports whether the sweep added
// anything, so the caller loops it to a fixpoint.
func propagateWarnedIdentities(entries []seadex.Entry, keys, all map[string]struct{}) bool {
	changed := false
	for i := range entries {
		for j := range entries[i].Torrents {
			t := &entries[i].Torrents[j]
			if sharesWarnedIdentity(t, all) && markWarnedIdentity(t, keys, all) {
				changed = true
			}
		}
	}
	return changed
}

// markWarnedIdentity folds torrent t's identity signals (journal key + info
// hash) into the warned sets, reporting whether any signal was new.
func markWarnedIdentity(t *seadex.Torrent, keys, all map[string]struct{}) bool {
	added := false
	if k := journalKey(t); k != "" {
		if _, warned := keys[k]; !warned {
			keys[k] = struct{}{}
			added = true
		}
	}
	for _, id := range identitySignals(t) {
		if _, warned := all[id]; !warned {
			all[id] = struct{}{}
			added = true
		}
	}
	return added
}

// sharesWarnedIdentity reports whether any of t's identity signals is already
// in the warned set.
func sharesWarnedIdentity(t *seadex.Torrent, all map[string]struct{}) bool {
	for _, id := range identitySignals(t) {
		if _, warned := all[id]; warned {
			return true
		}
	}
	return false
}

// filterWarnedTorrents is splitCurationWarned's second pass for one entry's
// torrents: it drops every occurrence that is warned itself OR shares a
// warned identity signal (journal key or info hash), reporting whether
// anything was removed (the caller only swaps in the fresh slice then,
// keeping the shared input unmutated). It is a pure query over the sets
// collectWarnedIdentities already closed: that fixpoint marks the journal key of
// every occurrence sharing a warned identity, so a duplicate excluded only through
// a warned sibling's info hash is already in the carry-drop set carryJournal
// consumes and needs no second fold here.
func filterWarnedTorrents(ts []seadex.Torrent, warnedIDs map[string]struct{}) ([]seadex.Torrent, bool) {
	unwarned := make([]seadex.Torrent, 0, len(ts))
	changed := false
	for j := range ts {
		t := &ts[j]
		if release.CurationWarned(t.Tags) || sharesWarnedIdentity(t, warnedIDs) {
			changed = true
			continue
		}
		unwarned = append(unwarned, *t)
	}
	return unwarned, changed
}

// --- Search curation index ---

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
