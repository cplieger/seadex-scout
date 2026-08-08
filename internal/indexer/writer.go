package indexer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/jsonx/bounded"
	"github.com/cplieger/seadex-scout/internal/degradation"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/tagfilter"
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
	// corrupted/hand-edited file OOMs the process inside Run's warm-up reload -
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
	// headroom to act. The 80% fraction is internal/degradation's app-wide
	// persisted-file policy, shared with internal/state's stateSizeWarnBytes.
	feedSizeWarnBytes = maxFeedBytes / degradation.SizeWarnDenominator * degradation.SizeWarnNumerator
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
	// maxPersistedItemBytes bounds ONE persisted feed item's serialized JSON -
	// the bound the per-array cardinality caps cannot express, because
	// bounded.Array bounds how many items decode while each item's interior
	// arrays are still decoded by encoding/json. Sized well above any item the
	// writer can produce: validPersistedItem caps each of the seven string
	// fields at maxPersistedFieldBytes, JSON escaping can expand a field ~6x
	// (every '<' becomes the six-byte \u003c), and the numeric fields plus an
	// at-limit maxPersistedCategories list add a few hundred bytes, so
	// 8 * 6 * maxPersistedFieldBytes leaves ample slack while bounding one
	// item's decode to a few hundred KiB.
	maxPersistedItemBytes = 8 * 6 * maxPersistedFieldBytes
	// maxPersistedCursorBytes bounds the persisted harvest checkpoint
	// (harvest_cursor). An honest checkpoint is now one "scope:alID" group key
	// (per-group page state is gone - see the paging-removal note in
	// harvest.go), so the cap is nowhere near binding; it is kept as the
	// backstop on the one persisted string carried forward VERBATIM, where an
	// unbounded hand-edited value would ride every future snapshot until
	// persist exceeds maxFeedBytes (see
	// TestLoadPreviousDropsOversizedHarvestCheckpoint).
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
	// maxSnapshotFeedItems caps ONE persisted journal feed's item count at
	// decode time, and maxSnapshotMapEntries / maxSnapshotMapEntriesTotal cap
	// one curation/ledger map and all of them together. maxFeedBytes bounds
	// the SERIALIZED file, which is not the same bound: a 16 MiB document can
	// encode millions of compact elements ("{}" is three bytes, `"a":true`
	// ten) while each decoded journalItem or map entry costs tens of bytes of
	// live heap, so json.Unmarshal would materialize hundreds of MB - past the
	// 256 MiB container limit - BEFORE any per-item validation could reject it
	// (CWE-400). The bounded decoder enforces these before an element is
	// allocated. Sizing: the live catalogue is ~9k torrents contributing ~2
	// identity signals each (~20k ledger entries) and a 14-day journal holds
	// the newly curated slice of that, so both caps leave more than an order of
	// magnitude of growth headroom while keeping the worst accepted decode a
	// few tens of MB.
	maxSnapshotFeedItems       = 50_000
	maxSnapshotMapEntries      = 250_000
	maxSnapshotMapEntriesTotal = 600_000
	// reasonMalformed is loadPrevious's baseline reason for a structurally invalid
	// previous snapshot (bad JSON, missing curation maps, or an invalid seen ledger).
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

// pruneJournalFeed drops every item one persisted journal feed carries that
// the shared decode gate refuses, returning the kept items and how many were
// dropped (the count the reader WARNs per tracker feed, see snapshotScrub).
// Two per-item invariants are enforced:
//
//   - the shared persisted-item limits (validPersistedItem), and
//   - for a post-journal snapshot, the journal identity fields the schema
//     guarantees (validJournalRecord).
//
// A per-item invariant is enforced per ITEM rather than by refusing the whole
// snapshot, because on the READER the wholesale verdict discards the curation
// maps with the journal: readSnapshot reports ok=false and, when nothing was
// ever loaded (Run's warm-up after a restart, or a resident-idle daemon whose
// feed has not been rebuilt), the cache stays unavailable and EVERY request -
// search and RSS alike - answers a Torznab error until a rebuild writes a
// clean snapshot, which in resident-idle mode only an external `poll`
// delivers. One hand-edited or partially corrupted item out of thousands
// would take the whole indexer surface down, where the writer's own policy for
// the identical shape is to drop just that item (prepareCarriedItem). Dropping
// closes each hazard as completely as rejecting did - an over-limit field
// never reaches renderFeed and a timestamp-less item is never served past
// feedJournalMaxAge - and it is the pattern this file already established for
// at-rest corrections (sanitizeSnapshotInfoURLs), counted per feed so the
// operator still learns which journal was touched (l-f45). The writer ignores
// the counts: its rebuild re-persists the pruned feed.
func pruneJournalFeed(feed []journalItem, postJournal bool) (kept []journalItem, dropped int) {
	kept = feed[:0]
	for i := range feed {
		if !validPersistedItem(&feed[i]) || (postJournal && !validJournalRecord(&feed[i])) {
			dropped++
			continue
		}
		kept = append(kept, feed[i])
	}
	return kept, dropped
}

// validJournalRecord reports whether one item carries the two journal identity
// fields the post-journal schema guarantees: a stable Key (the identity the
// carry gates and the download-URL rebuilds match on) and a nonzero FirstSeen
// (the age the bounded journal window prunes against).
//
// The writer already refuses such a record on its carry path
// (prepareCarriedItem drops every zero-FirstSeen item, because there is no
// journal age to prune it against), but the READER installs decoded records
// directly: a hand-edited or partially corrupted feed.json item whose GUID
// still proves its Key survives rebuildNyaaDownloadURLs/rebuildABDownloadURLs
// and is projected into the served RSS feed with no pubDate. In resident-idle
// mode no rebuild ever follows to apply the writer-only gate, so that item is
// served to the arrs indefinitely - outside feedJournalMaxAge entirely. Making
// the invariant part of the shared decode gate is what bounds it (h-f2).
func validJournalRecord(it *journalItem) bool {
	return it.Key != "" && !it.FirstSeen.IsZero()
}

// unmarshalSnapshot decodes persisted snapshot bytes with json.Unmarshal
// semantics but BOUNDED CARDINALITY. json.Unmarshal materializes the whole
// document before a caller-side check can run, so the byte cap the read applies
// (maxFeedBytes) does not bound what the decode ALLOCATES: a 16 MiB file can
// encode millions of compact array elements or map entries, each costing tens of
// bytes of live heap once decoded, and feed.json is a tamperable trust boundary
// (a corrupted or hand-written file could OOM the 256 MiB container inside Run's
// warm-up load and crashloop the compare daemon with it, instead of reaching the
// self-healing malformed-snapshot taxonomy - CWE-400, h-f4). The schema walk
// below rejects hostile cardinality BEFORE allocation scales with it, via the
// shared jsonx/bounded primitives (the same scaffold internal/seadex,
// internal/state, internal/anilist and internal/mapping decode through).
//
// Semantics stay json.Unmarshal's so both consumers keep their documented
// behavior: a JSON null leaves a field untouched, an empty object still
// ALLOCATES its map (nil-ness is the structural / pre-journal-schema sentinel
// decodeSnapshot and loadPrevious dispatch on, so `"seen": {}` must not decode
// nil), keys match case-insensitively, an unknown field is consumed without
// being materialized into a decoded value, a repeated key inside a decoded
// value keeps Unmarshal's last-occurrence/merge resolution, and trailing data
// is rejected. One deliberate tightening: a repeated TOP-LEVEL schema field
// (a second by_key/seen/nyaa_feed/...) fails closed, because at this boundary
// a document that names the same accumulated map twice is evidence of
// tampering and Unmarshal would silently resolve it to the last occurrence.
// The tightening is deliberately limited to the eight schema fields: it costs
// a fixed eight-entry set, while extending it to every nested object is what
// bounds the gate's own memory (see the note on the removed whole-document
// preflight below).
//
// No whole-document preflight runs (bounded.Preflight is deliberately NOT
// used): it holds one fold-canonicalized key set per traversed object, which
// is unbounded in exactly the dimension this walk exists to bound. A 16 MiB
// document - what maxFeedBytes and atomicfile.ReadBounded admit - carrying
// ~1.19M distinct short keys makes that pass alone hold ~91 MiB of live heap
// and churn ~379 MiB, inside the same 256 MiB container the compare loop
// shares, and it burned that on EVERY load (the reader's warm-up plus every
// mtime-change reload, the writer's loadPrevious every cycle) - the OOM
// crashloop the h-f4 comment above names as the hazard, arriving before a
// single entry could be charged against maxSnapshotMapEntries (h-f6).
// Reordering it after the walk would not help: an unknown top-level field's
// millions of keys are never charged by the schema walk at all.
//
// Nesting depth stays bounded without it, by encoding/json's own scanner
// ceiling (maxNestingDepth, the same 10000 bounded.MaxDepth mirrors): every
// value this walk consumes goes through Decode, whose scanner enforces it -
// which is why an unknown field is consumed as a json.RawMessage instead of
// through the decoder's token-walking Skip. Skip is depth-unbounded (a token
// walk grows json.Decoder's own container stack per open bracket, so a
// megabytes-of-'[' unknown field allocates once per byte), and a RawMessage
// is bounded by the field's own bytes, hence by maxFeedBytes.
func unmarshalSnapshot(data []byte) (snapshot, error) {
	var snap snapshot
	var mapEntries int
	claimed := make(map[string]struct{}, 8)
	// The aggregate array budget covers both journal feeds; each feed also
	// carries its own per-array cap, so neither one feed nor the pair can
	// multiply past the bound.
	d := bounded.NewDecoder(bytes.NewReader(data), 2*maxSnapshotFeedItems)
	err := d.Object(func(key string) error {
		field := snapshotField(key)
		if field == "" {
			// Unknown field: consumed, never materialized into a decoded
			// value, and depth-bounded by the scanner (see above).
			var raw json.RawMessage
			return d.Decode(&raw)
		}
		if err := claimSnapshotField(claimed, field); err != nil {
			return err
		}
		switch field {
		case "by_hash":
			set, err := decodeSnapshotMap(d, snap.ByHash, &mapEntries, "by_hash")
			snap.ByHash = set
			return err
		case "by_key":
			set, err := decodeSnapshotMap(d, snap.ByKey, &mapEntries, "by_key")
			snap.ByKey = set
			return err
		case "by_pair":
			set, err := decodeSnapshotMap(d, snap.ByPair, &mapEntries, "by_pair")
			snap.ByPair = set
			return err
		case "seen":
			set, err := decodeSnapshotMap(d, snap.Seen, &mapEntries, "seen")
			snap.Seen = set
			return err
		case "titles":
			titles, err := decodeSnapshotMap(d, snap.Titles, &mapEntries, "titles")
			snap.Titles = titles
			return err
		case "harvest_cursor":
			return d.Decode(&snap.HarvestCursor)
		case "nyaa_feed":
			return decodeSnapshotFeed(d, &snap.NyaaFeed, "nyaa_feed")
		default:
			return decodeSnapshotFeed(d, &snap.ABFeed, "ab_feed")
		}
	})
	if err != nil {
		return snapshot{}, err
	}
	if err := d.End(); err != nil {
		return snapshot{}, err
	}
	return snap, nil
}

// snapshotField maps one decoded top-level key to its canonical schema field
// name, or "" for a field the schema does not know. Matching is
// case-insensitive because json.Unmarshal matches struct FIELDS
// case-insensitively too, so "Seen" and "seen" address the same field and are
// equally a repeat of it.
func snapshotField(key string) string {
	for _, field := range []string{"by_hash", "by_key", "by_pair", "seen", "titles", "harvest_cursor", "nyaa_feed", "ab_feed"} {
		if strings.EqualFold(key, field) {
			return field
		}
	}
	return ""
}

// claimSnapshotField records one top-level schema field as decoded, refusing a
// second occurrence (see unmarshalSnapshot for why the top level fails closed
// while nested duplicates keep Unmarshal's resolution). The set holds at most
// the eight schema fields, so it is bounded whatever the document does; an
// unknown key is never recorded, which is what keeps a key-dense document from
// growing it. The message names only our own field literal - never the decoded
// key, which is attacker-shaped text at this boundary.
func claimSnapshotField(claimed map[string]struct{}, field string) error {
	if _, dup := claimed[field]; dup {
		return fmt.Errorf("snapshot: repeated top-level %q field", field)
	}
	claimed[field] = struct{}{}
	return nil
}

// decodeSnapshotFeed decodes one persisted journal feed under its per-array
// cardinality cap, so an over-long feed errors before its elements are
// allocated. Each item still decodes through encoding/json for
// stdlib-identical field handling; per-item validity (validPersistedItem,
// validJournalRecord) is decodeSnapshot's separate prune.
func decodeSnapshotFeed(d *bounded.Decoder, dst *[]journalItem, what string) error {
	feed, err := bounded.Array(d, *dst, maxSnapshotFeedItems, what, func(it *journalItem) error {
		// The per-array cap bounds how many ITEMS decode, not what ONE item
		// allocates: an item's own Categories array is decoded by
		// encoding/json, so a single item can amplify the byte cap into a
		// hundreds-of-MB int slice before validPersistedItem's
		// maxPersistedCategories check can reject it. Capture the element as
		// raw bytes first (bounded by maxFeedBytes) and refuse an item larger
		// than any the writer can produce before unmarshalling it.
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			return err
		}
		if len(raw) > maxPersistedItemBytes {
			return fmt.Errorf("%s: item exceeds %d bytes (max %d)", what, len(raw), maxPersistedItemBytes)
		}
		return json.Unmarshal(raw, it)
	})
	if err != nil {
		return err
	}
	*dst = feed
	return nil
}

// decodeSnapshotMap decodes one persisted map field (a curation index, the seen
// ledger, or the harvested-title cache) entry by entry under the shared entry
// budget, and RETURNS the map for the caller to store back. A JSON null leaves
// the map as it was (Unmarshal's null-into-map); an empty object allocates,
// because a nil map is the structural sentinel both consumers read. Per-value
// LENGTH stays loadPrevious's own ingress prune (retainValidTitles): this
// pass bounds cardinality, which is what json.Unmarshal cannot.
func decodeSnapshotMap[V bool | string](d *bounded.Decoder, dst map[string]V, entries *int, what string) (map[string]V, error) {
	open, err := d.Open('{')
	if err != nil || !open {
		return dst, err
	}
	if dst == nil {
		dst = make(map[string]V)
	}
	perMap := 0
	for d.More() {
		key, err := d.Key()
		if err != nil {
			return dst, err
		}
		if err := chargeSnapshotEntry(what, &perMap, entries); err != nil {
			return dst, err
		}
		var value V
		if err := d.Decode(&value); err != nil {
			return dst, err
		}
		dst[key] = value
	}
	return dst, d.Close()
}

// chargeSnapshotEntry charges one decoded map entry against its map's own cap
// and the snapshot-wide aggregate, so neither one oversized map nor many
// moderate ones can outgrow the decode budget. The message names the map but
// never a key or value: both are attacker-shaped text at this boundary.
func chargeSnapshotEntry(what string, perMap, entries *int) error {
	*perMap++
	if *perMap > maxSnapshotMapEntries {
		return fmt.Errorf("%s: too many entries (max %d)", what, maxSnapshotMapEntries)
	}
	*entries++
	if *entries > maxSnapshotMapEntriesTotal {
		return fmt.Errorf("%s: snapshot map entry budget exceeded (max %d)", what, maxSnapshotMapEntriesTotal)
	}
	return nil
}

// decodeSnapshot unmarshals persisted snapshot bytes and applies the
// structural-validity gate BOTH consumers share (the server's readSnapshot
// and the writer's loadPrevious): valid JSON and the required curation maps
// present (the writer always persists both, even empty, so nil maps identify
// a structurally invalid snapshot without rejecting a valid empty feed).
// err reports malformed JSON; a non-empty reason names a structural
// violation. Consumer-specific ingress checks (the writer's titles-cache and
// seen-ledger gates) stay with their consumer.
//
// A defect in ONE journal item is not a structural violation: the two
// per-item invariants (the shared persisted-item limits and, for a
// post-journal snapshot, the journal identity fields) drop just that item and
// report the count per tracker feed - see pruneJournalFeed for why the
// wholesale verdict was the wrong blast radius, and snapshotScrub for how the
// two consumers use the counts.
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
	snap, err = unmarshalSnapshot(data)
	if err != nil {
		return snapshot{}, snapshotScrub{}, "", err
	}
	if snap.ByHash == nil || snap.ByKey == nil {
		return snapshot{}, snapshotScrub{}, "missing required curation maps", nil
	}
	// The journal identity invariant is schema-scoped: only a post-journal
	// snapshot (Seen present) promises a Key and a FirstSeen on every item. A
	// Seen-less document is the retired pre-journal schema, whose feeds
	// reportTransitionalSchema/loadPrevious deliberately treat as absent - it
	// stays exempt so a legacy file still yields a usable search curation set
	// instead of having its items pruned for a promise its schema never made.
	// The requirement lives HERE rather than in validPersistedItem because
	// renderJournalItem validates a newly-built item before growJournal stamps
	// its FirstSeen.
	postJournal := snap.Seen != nil
	var nyaaDropped, abDropped int
	snap.NyaaFeed, nyaaDropped = pruneJournalFeed(snap.NyaaFeed, postJournal)
	snap.ABFeed, abDropped = pruneJournalFeed(snap.ABFeed, postJournal)
	normalizeSnapshotItems(snap.NyaaFeed)
	normalizeSnapshotItems(snap.ABFeed)
	normalizeSnapshotPubDates(snap.NyaaFeed)
	normalizeSnapshotPubDates(snap.ABFeed)
	scrub = snapshotScrub{
		blankedInfoURLs: map[string]int{
			upstreamNyaa: sanitizeSnapshotInfoURLs(snap.NyaaFeed),
			upstreamAB:   sanitizeSnapshotInfoURLs(snap.ABFeed),
		},
		droppedItems: map[string]int{
			upstreamNyaa: nyaaDropped,
			upstreamAB:   abDropped,
		},
	}
	return snap, scrub, "", nil
}

// snapshotScrub carries decodeSnapshot's at-rest corrections keyed by TRACKER
// SCOPE rather than summed. An operator reading one combined count cannot tell
// whether the Nyaa or the AnimeBytes journal was tampered with, which is the
// whole diagnostic value of the line (l-f176) - and summing is how the
// attribution was lost when the shared-gate refactor moved the two calls into
// one expression (d-u8c3-1). droppedItems carries the per-item gate's prune
// count the same way (pruneJournalFeed): the server WARNs once per affected
// feed, and the writer ignores it because its rebuild re-persists the pruned
// feed.
type snapshotScrub struct {
	blankedInfoURLs map[string]int
	droppedItems    map[string]int
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
	// HarvestCursor is the title harvest's persisted resumption state: the bare
	// "<scope>:<alID>" rotation cursor (see decodeHarvestCursor). It carries the
	// rotation position - the "scope:alID" of the last
	// show group that consumed a harvest query, so the next rebuild resumes
	// AFTER it instead of restarting at the head (see harvestTitles; a deep
	// show can then never starve its successors across rebuilds) - and
	// nothing else, since per-group offset paging was removed (see the
	// paging-removal note in harvest.go). An older snapshot without the field
	// starts at the head, and any value outside the rotation-cursor shape is
	// dropped to that same baseline.
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
	// TagFilter is the operator's filters.exclude_tags policy, asked about the
	// feed surface. Its zero value - the default - excludes nothing, so a
	// torrent SeaDex tags Broken is curated, journaled and served like any
	// other.
	TagFilter tagfilter.Filter
	UpstreamConfig
}

// FeedWriter builds the feed snapshot from a SeaDex fetch and persists it
// atomically for the server to read. It holds no SeaDex/Fribb clients of its
// own - the compare cycle owns the shared fetch and hands the results to
// Rebuild - and no Prowlarr clients either: the title harvest is its own
// component (harvester, see harvest.go), held here as a single collaborator
// because Rebuild is where it runs.
type FeedWriter struct {
	log     *slog.Logger
	now     func() time.Time
	harvest *harvester
	tags    tagfilter.Filter
	path    string
	// enablement is the operator's per-tracker input, held whole rather than
	// flattened into derived booleans, mirroring Indexer.enablement:
	// enabled(scope) is the package's one home for the scope-to-config
	// dispatch (see UpstreamConfig.torznabURL), so the writer evaluates the
	// same expression the server's gates do and a third tracker stays one
	// case. ProwlarrAPIKey is deliberately left unset - the same narrowing the
	// server applies - because it is reachability, consumed only inside the
	// wired upstreams.
	enablement UpstreamConfig
}

// NewFeedWriter returns a FeedWriter for cfg. client is the HTTP client the
// title harvest queries Prowlarr with (nil disables harvesting - items then
// keep their synthesized titles) and log may be nil (falls back to
// slog.Default). The upstreams are wired here from cfg's own UpstreamConfig
// (see wireUpstreams), so the writer's enablement and its reachability come
// from one operator input; the client stays a parameter because this package
// must never construct one.
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
func NewFeedWriter(cfg *FeedWriterConfig, log *slog.Logger, client *http.Client) *FeedWriter {
	if log == nil {
		log = slog.Default()
	}
	w := &FeedWriter{
		log:  log,
		now:  time.Now,
		tags: cfg.TagFilter,
		path: cfg.Path,
		enablement: UpstreamConfig{
			NyaaTorznabURL: cfg.NyaaTorznabURL,
			ABTorznabURL:   cfg.ABTorznabURL,
			ABPasskey:      cfg.ABPasskey,
		},
	}
	// The harvest reads the writer's clock through a closure over w rather than
	// a copy of the func value: w.now is a live field the test suite replaces
	// AFTER construction (the pacing/time-slice tests drive a fake clock), and
	// the harvest's pacer read it directly before it became its own component.
	// Copying time.Now here instead would fork the two clocks silently.
	w.harvest = newHarvester(log, func() time.Time { return w.now() },
		wireUpstreams(client, log, cfg.UpstreamConfig))
	return w
}

// --- Rebuild and persistence ---

// Rebuild refreshes the persisted feed snapshot from the SeaDex entries
// (categorized and titled via info, the per-show metadata closure the cycle
// builds over its persisted state; nil is valid and falls back to file-name
// synthesis). Torrents the operator's filters.exclude_tags policy excludes
// from the feed surface are removed first - from the search curation set, the
// seen ledger, and the journal alike - and a previously journaled item whose
// torrent has since become excluded is dropped, so the arrs can never grab a
// release the operator filtered (see splitCurationWarned; the default policy
// is empty, so by default nothing is excluded and a Broken-tagged release does
// enter the feed). It rebuilds the search curation set
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
	_, prev, err := w.loadPrevious(ctx)
	if err != nil {
		return err
	}
	entries, ws := splitCurationWarned(entries, w.tags)
	set := buildCuration(entries)
	now := w.now()

	var js journalStats
	var nyaa, ab []journalItem
	var census map[string]packCensus
	seen, titles := prev.seen, prev.titles
	if prev.baseline {
		seen, titles = allIdentities(entries), map[string]string{}
		w.log.Info("indexer feed journal baselined; RSS feed starts empty and grows from newly curated releases",
			"reason", prev.reason, "seen", len(seen))
	} else {
		pass := &journalPass{w: w, cur: indexCurated(entries), seen: seen, ws: &ws, infoFor: infoFor, js: &js, now: now}
		// The file-census pack verdict per journal key, read from the same
		// curation index the items render from, so applyTitles can tell when a
		// harvested title contradicts the payload it names (titleAudit).
		census = censusPacks(pass.cur)
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
	// ONE audit value across both feeds, so its onset latch is per REBUILD.
	audit := titleAudit{census: census, report: w.packDisagreementReporter()}
	applyTitles(nyaa, titles, audit)
	applyTitles(ab, titles, audit)
	nyaa, ab = sortFeed(nyaa), sortFeed(ab)
	titles = retainTitles(titles, nyaa, ab)

	if len(seen) > maxSnapshotMapEntries {
		// Mirror of the read-side cardinality cap: persist enforces only
		// maxFeedBytes, so an over-cardinality ledger would be written and then
		// refused by the shared decode - the reader serves nothing until the next
		// rebuild and this journal window is lost. The ledger's byte guard
		// (maxPersistedSeenBytes) cannot imply the cardinality one: a short
		// tracker-key entry serializes in ~20 bytes, so 8 MiB admits ~419k
		// entries. Rebuilding it from the current catalogue is exactly the remedy
		// seenLedgerWithinLimits already prescribes for the byte cap, and it keeps
		// the journal intact (every journaled item is in the catalogue).
		w.log.Warn("indexer seen ledger exceeded its decode cardinality cap; rebuilt from the current catalogue",
			"entries", len(seen), "max_entries", maxSnapshotMapEntries)
		seen = allIdentities(entries)
	}
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
	if js.abSkippedNoPasskey > 0 && w.enablement.enabled(upstreamAB) {
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

// loadPrevious reads the snapshot ONCE and returns both the raw decoded form
// and the journal projection of it. Advance needs the raw form for the three
// members the projection drops - ByHash, ByKey and ByPair, the search curation
// index - which it must carry through to persist verbatim; re-reading the file
// to get them would race a concurrent writer against its own first read.
//
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
func (w *FeedWriter) loadPrevious(ctx context.Context) (snapshot, previousJournal, error) {
	data, err := atomicfile.ReadBounded(ctx, w.path, maxFeedBytes)
	if err != nil {
		prev, cErr := w.classifyPreviousReadError(err)
		return snapshot{}, prev, cErr
	}
	snap, _, structReason, decodeErr := decodeSnapshot(data)
	if decodeErr != nil {
		// Bounded like the reader's sibling gate (reload.go): a decoder error
		// can embed the offending document text, and feed.json is a
		// tamperable boundary.
		w.log.Warn(msgSnapshotMalformed, "path", w.path, "error", capLogText(decodeErr.Error(), 256))
		return snapshot{}, previousJournal{baseline: true, reason: reasonMalformed}, nil
	}
	if structReason != "" {
		// The offending value itself is never logged: it can be
		// attacker-shaped multi-megabyte text.
		w.log.Warn(msgSnapshotMalformed, "path", w.path, "reason", structReason)
		return snapshot{}, previousJournal{baseline: true, reason: reasonMalformed}, nil
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
		return snapshot{}, previousJournal{baseline: true, reason: reasonMalformed}, nil
	}
	if snap.Seen == nil {
		return snapshot{}, previousJournal{baseline: true, reason: "pre-journal-schema"}, nil
	}
	titles, droppedTitles := retainValidTitles(snap.Titles)
	if droppedTitles > 0 {
		// A count, never the value: a cached title can be attacker-shaped
		// multi-megabyte text.
		w.log.Warn("previous feed snapshot dropped over-limit cached titles; the harvest re-earns them",
			"path", w.path, "dropped", droppedTitles, "max_bytes", maxPersistedFieldBytes)
	}
	cursor := snap.HarvestCursor
	if len(cursor) > maxPersistedCursorBytes {
		// The harvest cursor is the one persisted string carried forward
		// VERBATIM: harvestTitles re-emits the loaded value unchanged whenever
		// no group consumed a query this rebuild, so a hand-edited multi-MiB
		// value rides in every future snapshot and can push the rebuilt
		// snapshot past maxFeedBytes - wedging persist on every cycle with no
		// self-heal. Dropping it is the same safe degradation
		// decodeHarvestCursor applies to any value outside the rotation-cursor
		// shape: rotation restarts at the head. The value
		// itself is never logged (it can be attacker-shaped text).
		w.log.Warn("previous feed snapshot harvest cursor exceeds size cap; restarting the harvest rotation",
			"path", w.path, "max_bytes", maxPersistedCursorBytes, "cursor_bytes", len(cursor))
		cursor = ""
	}
	return snap, previousJournal{
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
// keys are tracker keys ("scope:digits") and scope-namespaced info hashes
// ("scope:h:<40hex>", plus bare 40-hex hashes still carried in a ledger
// written before ledgerSignals namespaced them), orders of
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
		// Charge the EXACT serialized cost, not the decoded key length:
		// each entry serializes as `"<key>":true,`, and encoding/json
		// escapes quotes, backslashes, control bytes and the
		// HTML-sensitive set (every '<' becomes the six-byte \u003c). A
		// decoded-byte approximation therefore lets an escape-heavy
		// ledger pass this cap and still push the REBUILT snapshot past
		// maxFeedBytes - the very wedge the aggregate cap exists to
		// prevent. json.Marshal on the key applies the same escaping
		// policy persist's json.Marshal(snap) will.
		//
		// json.Marshal cannot fail for a string: invalid UTF-8, control
		// bytes and the HTML-sensitive set are all escaped rather than
		// rejected, so there is no failure mode to branch on (only an
		// argument-type change could add one).
		encodedKey, _ := json.Marshal(k)
		total += len(encodedKey) + len(`:true,`)
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

// retainValidTitles copies the harvested-title cache forward, dropping the
// entries a rebuild must not apply and reporting how many were dropped: an
// empty title (nothing to upgrade a synthesized one with) and an entry whose
// key or title exceeds maxPersistedFieldBytes.
//
// The over-limit entry is DROPPED rather than re-baselining the whole journal,
// which is what this ingress used to do. The cache is a derived, re-earnable
// value: the baseline path replaces titles with an empty map anyway and the
// harvest re-earns a title within its query budget, so dropping costs one
// re-harvest. Re-baselining cost the WINDOW - seen is rebuilt from the current
// catalogue and the journal starts empty, so every release then inside
// feedJournalMaxAge is marked seen without ever having been served and
// journalIfNew reports isNew=false for it forever (the ledger is never pruned).
// That made one corrupted title permanently lose un-grabbed releases, where the
// sibling verbatim-carried field (harvest_cursor) already took the
// proportionate route of dropping just itself (l-f60). The cap still matters:
// applyTitles overwrites a carried item's title AFTER renderJournalItem's
// creation-time check, so an over-limit title would otherwise let this rebuild
// persist a snapshot the server's reload prunes.
func retainValidTitles(titles map[string]string) (kept map[string]string, dropped int) {
	kept = make(map[string]string, len(titles))
	for k, title := range titles {
		if title == "" {
			continue
		}
		if len(k) > maxPersistedFieldBytes || len(title) > maxPersistedFieldBytes {
			dropped++
			continue
		}
		kept[k] = title
	}
	return kept, dropped
}

// packDisagreementReporter returns applyTitles' contradiction sink for ONE
// rebuild: the first harvested title whose season-pack verdict disagrees with
// its release's file list is warned with its journal key, both verdicts, and
// whether the title was corrected, and the rest of the rebuild stays silent.
// That onset latch is the shape this package's other per-rebuild diagnostics
// already take (the harvest's per-scope latches, the snapshot log line's
// counters): a systematically disagreeing upstream - one Prowlarr indexer
// definition whose titles all drift, say - must surface once, not once per item.
//
// The raw title is deliberately NOT logged. It is untrusted tracker text (the
// Torznab decode tags it runesafe.Untrusted for exactly that reason), and the
// key plus the verdicts already name which release to look at.
//
// corrected=true means the item's own season token was rewritten from the file
// census (a title claiming a whole season over positively-proven single-episode
// content, the case that makes Sonarr suppress the season's real episodes);
// corrected=false means the title was served exactly as harvested and the line
// is evidence only.
func (w *FeedWriter) packDisagreementReporter() func(key string, titlePack, filesPack, corrected bool) {
	warned := false
	return func(key string, titlePack, filesPack, corrected bool) {
		if warned {
			return
		}
		warned = true
		w.log.Warn("indexer feed title and file list disagree about a season pack",
			"key", key, "title_pack", titlePack, "files_pack", filesPack, "corrected", corrected)
	}
}

// --- Curation-warned exclusion ---

// warnedSet is the exclusion set splitCurationWarned builds
// and the carry side consumes as one value: keys holds the excluded journal
// keys (every directly excluded occurrence plus every duplicate removed through
// a shared identity - also the warned_excluded operator count), and ids holds
// the excluded identity-signal set (journal key AND info hash), transitively
// closed over shared identities by collectWarnedIdentities' graph traversal,
// which retracts uses to drop a previously journaled item whose stored info
// hash is excluded under a DIFFERENT tracker key. Both sets are empty under the
// default (empty) filters.exclude_tags policy.
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
// copy of entries with every EXCLUDED torrent removed, plus the warnedSet the
// carry side consumes (see warnedSet for the two sets it holds and
// warnedSet.retracts for the retraction decision). Which torrents are excluded
// is the operator's filters.exclude_tags policy asked about the feed surface
// (tags), NOT a fixed vocabulary: the default policy is empty, so by default
// nothing is excluded and even a torrent SeaDex tags Broken is curated,
// journaled and served. The names here keep the curation-warning vocabulary
// because that is the motivating case the exclusion exists for.
//
// The exclusion wins BY
// IDENTITY, not per occurrence: a torrent can be attached to several SeaDex
// entries, and when one occurrence carries an excluded tag while a
// duplicate of the same tracker key does not, keeping the unexcluded duplicate
// would let proxied searches serve and mark the release while carryJournal
// (which consumes the any-occurrence key set) removes it from RSS - the two
// indexer paths would disagree about whether the release is grabbable. So a
// first pass collects every excluded identity signal - journal key AND info
// hash (identitySignals, the deliberately CROSS-SCOPE identity form: an
// exclusion of the bytes must retract every listing of them, unlike the seen
// ledger's per-scope ledgerSignals) - across the
// whole catalogue, and a second pass removes every occurrence that is excluded
// itself OR shares an excluded identity.
//
// This identity-wide scope deliberately DIFFERS from the daemon's
// per-occurrence check in internal/compare (which drops only the occurrence
// whose own tags are excluded), and the asymmetry was measured and kept, not
// missed: across the live catalogue (2806 entries, 9175 torrent records, 254
// curation-warned, measured 2026-07-29) 380 of the identities are shared by
// more than one entry and ZERO of them carry differing tag sets, so the two
// scopes cannot currently disagree. The feed hands the arrs bytes they will
// GRAB, so it closes over every listing of them; an alert names one occurrence
// a human then looks at.
//
// Filtering at the source keeps every downstream consumer honest at once: the
// search curation set never marks an excluded release (a Prowlarr result
// matching one is purged as uncurated), the journal never grows one, and the
// seen ledger never records one - so when the exclusion is lifted (the warning
// dropped upstream, or the operator's config changed) the torrent becomes
// grabbable curation for the first time and journals as new (a
// torrent journaled BEFORE it was excluded stays in the persisted ledger, so
// un-excluding it never re-broadcasts it). The input is never mutated: the
// cycle shares the entries slice with the compare pass, so an entry
// containing a removed torrent gets a fresh filtered Torrents slice.
func splitCurationWarned(entries []seadex.Entry, tags tagfilter.Filter) (kept []seadex.Entry, ws warnedSet) {
	ws.keys, ws.ids = collectWarnedIdentities(entries, tags)
	kept = make([]seadex.Entry, len(entries))
	for i := range entries {
		kept[i] = entries[i]
		if unwarned, changed := filterWarnedTorrents(entries[i].Torrents, ws.ids, tags); changed {
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
//
// Warning identity is TRANSITIVE across occurrences: if A shares a hash with B
// and B shares its tracker key with C, all three name the same warned release
// graph and must be excluded together. That closure is computed by building the
// signal graph once and traversing it, NOT by re-sweeping the catalogue until a
// sweep adds nothing: a reverse-ordered alternating key/hash chain reveals only
// one new node per sweep, so the fixpoint form was quadratic in torrent count
// (at the ~9k-torrent catalogue's worst shape, tens of millions of visits, each
// re-parsing every journal key through trackerKey/urlform) and structurally
// valid upstream input could make one rebuild overrun the poll interval (h-f1).
// Building the index once and visiting each node and each signal once is linear
// in torrents plus signals.
func collectWarnedIdentities(entries []seadex.Entry, tags tagfilter.Filter) (keys, all map[string]struct{}) {
	keys, all = make(map[string]struct{}), make(map[string]struct{})
	nodes, bySignal, pending := indexWarnedIdentities(entries, tags)
	visited := make([]bool, len(nodes))
	expanded := make(map[string]struct{}, len(bySignal))
	for len(pending) > 0 {
		idx := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if visited[idx] {
			continue
		}
		visited[idx] = true
		node := &nodes[idx]
		if node.key != "" {
			keys[node.key] = struct{}{}
		}
		for _, signal := range node.signals {
			all[signal] = struct{}{}
			// Each signal expands once: every torrent carrying it is
			// enqueued, and a later node sharing it needs no second sweep.
			if _, done := expanded[signal]; done {
				continue
			}
			expanded[signal] = struct{}{}
			pending = append(pending, bySignal[signal]...)
		}
	}
	return keys, all
}

// warnedNode is one torrent's contribution to the warned-identity graph: its
// journal key (the carry-drop identity, empty when the URL carries no parseable
// tracker id) and its identity signals (key + bare info hash), read once so the
// traversal never re-parses a URL.
type warnedNode struct {
	key     string
	signals []string
}

// indexWarnedIdentities builds the warned-identity graph in one catalogue pass:
// one node per torrent, an index from each identity signal to the nodes carrying
// it (the graph's edges, since two torrents are adjacent exactly when they share
// a signal), and the traversal seeds - the nodes the operator's tag policy
// excludes from the feed surface. With the default (empty) policy there are no
// seeds, so the traversal is a no-op and the whole catalogue is kept.
func indexWarnedIdentities(entries []seadex.Entry, tags tagfilter.Filter) (nodes []warnedNode, bySignal map[string][]int, seeds []int) {
	total := 0
	for i := range entries {
		total += len(entries[i].Torrents)
	}
	nodes = make([]warnedNode, 0, total)
	bySignal = make(map[string][]int, total)
	for i := range entries {
		for j := range entries[i].Torrents {
			t := &entries[i].Torrents[j]
			idx := len(nodes)
			nodes = append(nodes, warnedNode{key: journalKey(t), signals: identitySignals(t)})
			for _, signal := range nodes[idx].signals {
				bySignal[signal] = append(bySignal[signal], idx)
			}
			if tags.Excludes(t.Tags, tagfilter.SurfaceFeed) {
				seeds = append(seeds, idx)
			}
		}
	}
	return nodes, bySignal, seeds
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
// torrents: it drops every occurrence the operator's tag policy excludes from
// the feed surface OR that shares an excluded identity signal (journal key or
// info hash), reporting whether
// anything was removed (the caller only swaps in the fresh slice then,
// keeping the shared input unmutated). It is a pure query over the sets
// collectWarnedIdentities already closed: that traversal marks the journal key of
// every occurrence sharing an excluded identity, so a duplicate excluded only through
// an excluded sibling's info hash is already in the carry-drop set carryJournal
// consumes and needs no second fold here.
func filterWarnedTorrents(ts []seadex.Torrent, warnedIDs map[string]struct{}, tags tagfilter.Filter) ([]seadex.Torrent, bool) {
	unwarned := make([]seadex.Torrent, 0, len(ts))
	changed := false
	for j := range ts {
		t := &ts[j]
		if tags.Excludes(t.Tags, tagfilter.SurfaceFeed) || sharesWarnedIdentity(t, warnedIDs) {
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
