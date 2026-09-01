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
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/cplieger/atomicfile/v3"
	"github.com/cplieger/jsoncap/v2"
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
	// feedFileMode keeps the persisted snapshot owner-only: feed.json is GUID-only - AB
	// items carry no passkey-bearing download URL (see stripDownloadURLs) - but it
	// stays owner-only as defense in depth for that invariant, and a legacy snapshot
	// may still embed a passkey until the first rebuild scrubs it.
	feedFileMode = 0o600
	// maxFeedBytes bounds the persisted feed snapshot, enforced on write and read alike
	// so a rebuild can never persist a snapshot the server's reload would then reject.
	maxFeedBytes = 16 << 20
	// maxPersistedFieldBytes caps each persisted feed item's string field (title,
	// GUID/info/download URL, journal key).
	maxPersistedFieldBytes = maxUpstreamFieldBytes
	// maxPersistedCategories caps one persisted item's category list. The
	// writer unions at most the three Torznab ids the feed uses (TV, Anime,
	// Movies); anything larger is a hand-edited snapshot.
	maxPersistedCategories = 8
	// maxPersistedItemBytes bounds ONE persisted feed item's serialized JSON - the
	// bound the per-array cardinality caps cannot express, because Decoder.Array bounds
	// how many items decode while each item's interior arrays are still decoded by
	// encoding/json.
	maxPersistedItemBytes = 8 * 6 * maxPersistedFieldBytes
	// maxPersistedCursorBytes bounds the persisted harvest checkpoint (harvest_cursor).
	maxPersistedCursorBytes = 64 << 10
	// maxPublicationLogBytes bounds the publication log IN AGGREGATE - the
	// log's twin of maxPersistedCursorBytes, and for the same reason.
	maxPublicationLogBytes = 8 << 20
	// maxSnapshotFeedItems caps ONE persisted journal feed's item count at decode time,
	// and maxSnapshotMapEntries / maxSnapshotMapEntriesTotal cap one persisted map and
	// all of them together.
	maxSnapshotFeedItems       = 50_000
	maxSnapshotMapEntries      = 250_000
	maxSnapshotMapEntriesTotal = 600_000
	// reasonMalformed is loadPrevious's baseline reason for a structurally invalid
	// previous snapshot (bad JSON, a missing fact, or an invalid publication log).
	reasonMalformed = "malformed"
	// reasonSchemaVersion is loadPrevious's baseline reason for a snapshot at a
	// version this binary does not read. It is deliberately distinct from
	// reasonMalformed: the file was not corrupt, and an operator reading the
	// line needs to see a version skew rather than a fault.
	reasonSchemaVersion = "schema-version"
	// msgSnapshotMalformed is the one operator-facing malformed-rebaseline
	// message loadPrevious's three Warn sites share (tests pin the exact text).
	msgSnapshotMalformed = "previous feed snapshot malformed; re-baselining the feed journal"
)

// validPersistedItem reports whether one feed item respects the shared persisted-item
// limits: every string field under maxPersistedFieldBytes, the category list under
// maxPersistedCategories, and the non-negative numeric domain both producers guarantee
// (toItem clamps size/seeders/leechers to >= 0; totalSize returns 0 on
// negative/overflowing sums), so a hand-edited or corrupted snapshot with a negative
// value is rejected at load instead of rendering an invalid enclosure length/size attr.
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
func pruneJournalFeed(feed []journalItem) (kept []journalItem, dropped int) {
	kept = feed[:0]
	for i := range feed {
		if !validPersistedItem(&feed[i]) || !validJournalRecord(&feed[i]) {
			dropped++
			continue
		}
		kept = append(kept, feed[i])
	}
	return kept, dropped
}

// validJournalRecord reports whether one item carries the two journal identity
// fields the schema guarantees: a stable Key (the identity the
// carry gates and the download-URL rebuilds match on) and a nonzero FirstSeen
// (the age the bounded journal window prunes against).
func validJournalRecord(it *journalItem) bool {
	return it.Key != "" && !it.FirstSeen.IsZero()
}

// unmarshalSnapshot decodes persisted snapshot bytes with json.Unmarshal semantics but
// BOUNDED CARDINALITY.
func unmarshalSnapshot(data []byte) (snapshot, error) {
	var snap snapshot
	var mapEntries int
	claimed := make(map[snapshotMember]struct{}, len(allSnapshotMembers))
	// The aggregate array budget covers both journal feeds AND every owned
	// release, which decodeSnapshotOwners decodes through d.Array as well; each
	// array also carries its own per-array cap, so neither one array nor the
	// set can multiply past the bound. The owners' release dimension is
	// therefore bounded at 2*maxSnapshotFeedItems by a budget named for the
	// feeds rather than at maxSnapshotMapEntries, while the owner KEYS charge
	// nothing against it and chargeSnapshotEntry is their only bound.
	d := jsoncap.NewDecoder(bytes.NewReader(data), 2*maxSnapshotFeedItems)
	err := d.Object(func(key string) error {
		member := snapshotField(key)
		if member == "" {
			// Unknown field: consumed, never materialized into a decoded
			// value, and depth-bounded by the scanner.
			var raw json.RawMessage
			return d.Decode(&raw)
		}
		if err := claimSnapshotField(claimed, member); err != nil {
			return err
		}
		switch member {
		case memberVersion:
			return d.Decode(&snap.Version)
		case memberOwners:
			owners, err := decodeSnapshotOwners(d, snap.Owners, &mapEntries)
			snap.Owners = owners
			return err
		case memberPublished:
			set, err := decodeSnapshotMap(d, snap.Published, &mapEntries, string(memberPublished))
			snap.Published = set
			return err
		case memberTitles:
			titles, err := decodeSnapshotMap(d, snap.Titles, &mapEntries, string(memberTitles))
			snap.Titles = titles
			return err
		case memberHarvestCursor:
			return d.Decode(&snap.HarvestCursor)
		case memberNyaaFeed:
			return decodeSnapshotFeed(d, &snap.NyaaFeed, string(memberNyaaFeed))
		case memberABFeed:
			return decodeSnapshotFeed(d, &snap.ABFeed, string(memberABFeed))
		}
		return nil
	})
	if err != nil {
		return snapshot{}, err
	}
	if err := d.End(); err != nil {
		return snapshot{}, err
	}
	return snap, nil
}

// snapshotField maps one decoded top-level key to its canonical persisted
// MEMBER, or "" for a key the schema does not know. Matching is
// case-insensitive because json.Unmarshal matches struct FIELDS
// case-insensitively too, so "Owners" and "owners" address the same member and
// are equally a repeat of it. The vocabulary is allSnapshotMembers, so the
// decoder and the persisted members cannot drift.
func snapshotField(key string) snapshotMember {
	for _, member := range allSnapshotMembers {
		if strings.EqualFold(key, string(member)) {
			return member
		}
	}
	return ""
}

// claimSnapshotField records one top-level member as decoded, refusing a
// second occurrence (see unmarshalSnapshot for why the top level fails closed
// while nested duplicates keep Unmarshal's resolution). The set holds at most
// the members of allSnapshotMembers, so it is bounded whatever the document
// does; an unknown key is never recorded, which is what keeps a key-dense
// document from growing it. The message names only our own member literal -
// never the decoded key, which is attacker-shaped text at this boundary.
func claimSnapshotField(claimed map[snapshotMember]struct{}, member snapshotMember) error {
	if _, dup := claimed[member]; dup {
		return fmt.Errorf("snapshot: repeated top-level %q field", member)
	}
	claimed[member] = struct{}{}
	return nil
}

// decodeSnapshotOwners decodes the per-entry curation ownership fact under the
// shared entry budget. BOTH dimensions are charged - the outer owner keys and
// every release inside them - because the fact is a map of arrays and either
// dimension alone can carry hostile cardinality: a million owners with one
// release each and one owner with a million releases cost the same heap. ONE
// counter across both dimensions is also why this walks the object with
// jsoncap's token primitives rather than d.Map, whose per-container cap plus
// d.Array's would bound each dimension independently and their product not at
// all. A JSON null KEEPS the caller's map, the opposite of Unmarshal's
// null-into-map; decodeSnapshotMap carries why that is safe at both sites.
func decodeSnapshotOwners(d *jsoncap.Decoder, dst map[string][]ownedRelease, entries *int) (map[string][]ownedRelease, error) {
	const what = string(memberOwners)
	open, err := d.Open('{')
	if err != nil || !open {
		return dst, err
	}
	if dst == nil {
		dst = make(map[string][]ownedRelease)
	}
	perMap := 0
	for d.More() {
		// The charge lands before the KEY is read, the ordering
		// jsoncap.(*Decoder).Map states for the same walk: the key is itself an
		// unbounded allocation from the wire.
		if chargeErr := chargeSnapshotEntry(what, &perMap, entries); chargeErr != nil {
			return dst, chargeErr
		}
		key, keyErr := d.Key()
		if keyErr != nil {
			return dst, keyErr
		}
		releases, arrErr := d.Array([]ownedRelease(nil), maxSnapshotMapEntries, what, func(r *ownedRelease) error {
			if chargeErr := chargeSnapshotEntry(what, &perMap, entries); chargeErr != nil {
				return chargeErr
			}
			return d.Decode(r)
		})
		if arrErr != nil {
			return dst, arrErr
		}
		dst[key] = releases
	}
	return dst, d.Close()
}

// decodeSnapshotFeed decodes one persisted journal feed under its per-array
// cardinality cap, so an over-long feed errors before its elements are
// allocated. Each item still decodes through encoding/json for
// stdlib-identical field handling; per-item validity (validPersistedItem,
// validJournalRecord) is decodeSnapshot's separate prune.
func decodeSnapshotFeed(d *jsoncap.Decoder, dst *[]journalItem, what string) error {
	feed, err := d.Array(*dst, maxSnapshotFeedItems, what, func(it *journalItem) error {
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
// publication log, or the harvested-title cache) entry by entry under the shared entry
// budget, and RETURNS the map for the caller to store back. A JSON null KEEPS the
// caller's map, the OPPOSITE of Unmarshal's null-into-map (measured: json.Unmarshal
// nils a pre-populated map, and jsoncap.Map matches Unmarshal). That is
// unobservable here only because claimSnapshotField refuses a repeated top-level
// member, so the prior is always nil and either policy yields the nil the
// structural gate then refuses - re-check that precondition before lifting this
// shape anywhere the prior can be non-nil. An empty object allocates, because a
// nil map is the structural sentinel both consumers read. Per-value LENGTH stays
// loadPrevious's own ingress prune (retainValidTitles): this pass bounds
// cardinality, which is what json.Unmarshal cannot.
func decodeSnapshotMap[V bool | string](d *jsoncap.Decoder, dst map[string]V, entries *int, what string) (map[string]V, error) {
	open, err := d.Open('{')
	if err != nil || !open {
		return dst, err
	}
	if dst == nil {
		dst = make(map[string]V)
	}
	perMap := 0
	for d.More() {
		// Charge before the key is read, as decodeSnapshotOwners does and for
		// the same reason.
		if err := chargeSnapshotEntry(what, &perMap, entries); err != nil {
			return dst, err
		}
		key, err := d.Key()
		if err != nil {
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

// publicationLogEntriesWithinDecodeCap reports whether a publication log of
// entries entries clears the per-map cardinality bound the DECODE side applies
// through chargeSnapshotEntry, and is the half of persist's pre-flight that
// mirrors it. It lives beside chargeSnapshotEntry because the two must move
// together: the decode accepts a map of exactly maxSnapshotMapEntries and
// refuses the entry after it, so a writer refusing AT the cap would freeze the
// feed one entry before its own reader would - and tell the operator to
// re-baseline a snapshot that would have loaded.
func publicationLogEntriesWithinDecodeCap(entries int) bool {
	return entries <= maxSnapshotMapEntries
}

// decodeSnapshot unmarshals persisted snapshot bytes and applies the gate BOTH
// consumers share (the server's readSnapshot and the writer's loadPrevious): valid
// JSON, a PRESENT and SUPPORTED schema version, and the two required facts - the
// curation ownership map and the publication log (the writer always persists both, even
// empty, so nil identifies a structurally invalid snapshot without rejecting a valid
// empty feed).
func decodeSnapshot(data []byte) (snap snapshot, scrub snapshotScrub, reason string, err error) {
	snap, err = unmarshalSnapshot(data)
	if err != nil {
		return snapshot{}, snapshotScrub{}, "", err
	}
	if snap.Version == 0 {
		// A document that does not IDENTIFY itself is corruption, not a version skew,
		// and the two must not be conflated: `null`, `{}`, a truncated write and a
		// retired pre-version file all land here.
		return snapshot{}, snapshotScrub{}, "missing schema version", nil
	}
	if snap.Version != currentFeedVersion {
		// A document that DOES identify itself is trusted about that much, so a foreign
		// version RE-BASELINES rather than being refused.
		return snapshot{Version: snap.Version}, snapshotScrub{}, "", nil
	}
	if snap.Owners == nil || snap.Published == nil {
		return snapshot{}, snapshotScrub{}, "missing required curation ownership or publication log", nil
	}
	var nyaaDropped, abDropped int
	snap.NyaaFeed, nyaaDropped = pruneJournalFeed(snap.NyaaFeed)
	snap.ABFeed, abDropped = pruneJournalFeed(snap.ABFeed)
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

// snapshotScrub carries decodeSnapshot's at-rest corrections keyed by TRACKER SCOPE
// rather than summed.
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
// mis-sorts a newest-first view.
func normalizeSnapshotPubDates(feed []journalItem) {
	for i := range feed {
		feed[i].PubDate = feed[i].FirstSeen
	}
}

// snapshot is the materialized feed a cycle produces and the server serves:
// ONE store holding TWO FACTS (per-entry curation ownership and the
// append-only publication log) plus the two materialized RSS journals and the
// harvested-title cache. The search index is a PROJECTION of the ownership
// fact, derived in memory on load rather than persisted (see members.go for
// the whole contract and the per-member rule table).
type snapshot struct {
	// Owners is FACT 1, the PRESENT: per-entry curation ownership, an AniList id
	// (ownerKey) mapped to the set of releases that entry contributes.
	Owners map[string][]ownedRelease `json:"owners"`
	// Published is FACT 2, the PAST: the append-only publication log, recorded at the
	// moment an item ENTERS a feed and never on refusal.
	Published map[string]bool   `json:"published"`
	Titles    map[string]string `json:"titles,omitempty"`
	// HarvestCursor is the title harvest's persisted resumption state: the bare
	// "<scope>:<alID>" rotation cursor (see decodeHarvestCursor).
	HarvestCursor string        `json:"harvest_cursor,omitempty"`
	NyaaFeed      []journalItem `json:"nyaa_feed"`
	ABFeed        []journalItem `json:"ab_feed"`
	// Version is the schema envelope (currentFeedVersion). Any other value re-baselines
	// rather than being refused - see currentFeedVersion for why the key exists at all
	// and why re-baseline is the right answer.
	Version int `json:"version"`
}

// supportedVersion reports whether a decoded snapshot is one this binary may
// read. It is the ONE home of that test, so the writer's baseline decision and
// the reader's serve-empty decision cannot disagree about which files are
// readable.
func (s *snapshot) supportedVersion() bool { return s.Version == currentFeedVersion }

// FeedWriterConfig configures NewFeedWriter. Path is where the snapshot is persisted
// (config.DefaultIndexerFeedPath in production).
type FeedWriterConfig struct {
	Path string
	// Server is the Torznab feed server running in THIS process, when there is one (the
	// daemon runs the compare loop and the feed together).
	Server *Indexer
	// TagFilter is the operator's filters.exclude_tags policy, asked about the
	// feed surface. Its zero value - the default - excludes nothing, so a
	// torrent SeaDex tags Broken is curated, journaled and served like any
	// other.
	TagFilter tagfilter.Filter
	UpstreamConfig
}

// FeedWriter builds the feed snapshot from a SeaDex fetch, persists it
// atomically, and hands it to the in-process feed server when one runs here. It
// holds no SeaDex/Fribb clients of its own - the compare cycle owns the shared
// fetch and hands the results to Rebuild - and no Prowlarr clients either: the
// title harvest is its own component (harvester, see harvest.go), held here as a
// single collaborator because Rebuild is where it runs.
type FeedWriter struct {
	log     *slog.Logger
	now     func() time.Time
	harvest *harvester
	// cache is the in-process feed server's snapshot cache, or nil when no server runs
	// in this process (the `poll` subcommand).
	cache *snapshotCache
	tags  tagfilter.Filter
	path  string
	// enablement is the operator's per-tracker input, held whole rather than flattened
	// into derived booleans, mirroring Indexer.enablement: enabled(scope) is the
	// package's one home for the scope-to-config dispatch (see
	// UpstreamConfig.torznabURL), so the writer evaluates the same expression the
	// server's gates do and a third tracker stays one case.
	enablement UpstreamConfig
}

// NewFeedWriter returns a FeedWriter for cfg. client is the HTTP client the title
// harvest queries Prowlarr with (nil disables harvesting - items then keep their
// synthesized titles) and log may be nil (falls back to slog.Default).
func NewFeedWriter(cfg *FeedWriterConfig, log *slog.Logger, client *http.Client) *FeedWriter {
	if log == nil {
		log = slog.Default()
	}
	w := &FeedWriter{
		log:        log,
		now:        time.Now,
		tags:       cfg.TagFilter,
		path:       cfg.Path,
		enablement: cfg.enablementOnly(),
	}
	if cfg.Server != nil {
		w.cache = cfg.Server.cache
	}
	// The harvest reads the writer's clock through a closure over w rather than
	// a copy of the func value: w.now is a live field the test suite replaces
	// AFTER construction (the pacing/time-slice tests drive a fake clock), and
	// the harvest's pacer read it directly before it became its own component.
	w.harvest = newHarvester(log, func() time.Time { return w.now() },
		wireUpstreams(client, log, cfg.UpstreamConfig))
	return w
}

// Rebuild refreshes the persisted feed from the WHOLE SeaDex catalogue
// (categorized and titled via info, the per-show metadata closure the cycle
// builds over its persisted state; nil is valid and falls back to file-name
// synthesis). It is the pass at catalogue scope, and that is its ONLY
// difference from Advance - see run for the shared body and pass.go for the
// three steps that are genuinely catalogue-only.
func (w *FeedWriter) Rebuild(ctx context.Context, entries []seadex.Entry, info EntryInfoFunc) error {
	return w.run(ctx, entries, info, scopeCatalogue)
}

// persist atomically writes the snapshot and hands it to the in-process feed server,
// mirroring the reader's size bound before committing: a snapshot the reload would
// reject must not replace the last-good file, or the next restart starts with an empty
// feed.
func (w *FeedWriter) persist(ctx context.Context, snap *snapshot) error {
	stripDownloadURLs(snap.ABFeed)
	stripDownloadURLs(snap.NyaaFeed)
	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("indexer: encode feed snapshot: %w", err)
	}
	// Crossing maxFeedBytes refuses every subsequent persist and freezes the served
	// RSS journal with no self-heal, so warn while there is still headroom to act.
	if degradation.ApproachingLimit(int64(len(data)), maxFeedBytes) {
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
	w.publishSnapshot(snap)
	return nil
}

// publishSnapshot hands the completed pass's snapshot to the feed server running
// in this process, so serving it never waits on - or depends on - a file read.
func (w *FeedWriter) publishSnapshot(snap *snapshot) {
	if w.cache == nil {
		return
	}
	info, err := os.Stat(w.path)
	if err != nil {
		// Field-name-only at Debug: nothing is broken (the write succeeded and
		// the snapshot is still published), and the only consequence is one
		// redundant read on the server's next tick.
		w.log.Debug("indexer feed snapshot stat after write failed; the server will re-read the file once",
			"path", w.path, "error", err)
		info = nil
	}
	w.cache.publish(snap, info)
}

// stripDownloadURLs blanks every feed item's download URL before persistence: BOTH
// feeds are GUID-only at rest.
func stripDownloadURLs(feed []journalItem) {
	for i := range feed {
		feed[i].DownloadURL = ""
	}
}

// feedState is the loaded feed, VALIDATED - and it is the ONLY view of it.
type feedState struct {
	reason    string
	cursor    string
	owners    map[string][]ownedRelease
	published map[string]bool
	titles    map[string]string
	nyaaFeed  []journalItem
	abFeed    []journalItem
	baseline  bool
}

// loadPrevious reads the persisted feed ONCE and returns the one validated view of it.
func (w *FeedWriter) loadPrevious(ctx context.Context) (feedState, error) {
	data, err := w.readPrevious(ctx)
	if err != nil {
		return w.classifyPreviousReadError(err)
	}
	snap, _, structReason, decodeErr := decodeSnapshot(data)
	if decodeErr != nil {
		// Bounded like the reader's sibling gate (reload.go): a decoder error
		// can embed the offending document text, and feed.json is a
		// tamperable boundary.
		w.log.Warn(msgSnapshotMalformed, "path", w.path, "error", capLogText(decodeErr.Error(), 256))
		return feedState{baseline: true, reason: reasonMalformed}, nil
	}
	if structReason != "" {
		// The offending value itself is never logged: it can be
		// attacker-shaped multi-megabyte text.
		w.log.Warn(msgSnapshotMalformed, "path", w.path, "reason", structReason)
		return feedState{baseline: true, reason: reasonMalformed}, nil
	}
	if !snap.supportedVersion() {
		// Not a fault: this app supports no migration, so a foreign version
		// starts over rather than being read or converted. The version number
		// IS safe to log - it is a small integer from our own schema field,
		// not attacker-shaped text.
		w.log.Info("previous feed snapshot has an unsupported schema version; re-baselining the feed",
			"path", w.path, "version", snap.Version, "supported", currentFeedVersion)
		return feedState{baseline: true, reason: reasonSchemaVersion}, nil
	}
	if !publicationLogWithinLimits(snap.Published) {
		// The publication log is carried forward verbatim and never pruned, so an
		// over-limit identity key from a hand-edited snapshot would otherwise persist
		// in every future snapshot, and a false membership value (which the writer
		// never emits) would make the novelty test re-broadcast an already-recorded
		// release.
		w.log.Warn(msgSnapshotMalformed,
			"path", w.path, "reason", "publication log is invalid or exceeds its size cap")
		return feedState{baseline: true, reason: reasonMalformed}, nil
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
		// The harvest cursor is the one persisted string carried forward VERBATIM:
		// harvestTitles re-emits the loaded value unchanged whenever no group consumed
		// a query this rebuild, so a hand-edited multi-MiB value rides in every future
		// snapshot and can push the rebuilt snapshot past maxFeedBytes - wedging
		// persist on every cycle with no self-heal.
		w.log.Warn("previous feed snapshot harvest cursor exceeds size cap; restarting the harvest rotation",
			"path", w.path, "max_bytes", maxPersistedCursorBytes, "cursor_bytes", len(cursor))
		cursor = ""
	}
	return feedState{
		owners:    snap.Owners,
		published: snap.Published,
		nyaaFeed:  snap.NyaaFeed,
		abFeed:    snap.ABFeed,
		titles:    titles,
		cursor:    cursor,
	}, nil
}

// publicationLogWithinLimits reports whether every publication-log entry respects the
// producer contract: a bounded identity key mapped to true membership, and the log as a
// WHOLE within maxPublicationLogBytes.
func publicationLogWithinLimits(published map[string]bool) bool {
	total := 0
	for k, wasPublished := range published {
		if len(k) > maxPersistedFieldBytes || !wasPublished {
			return false
		}
		// Charge the EXACT serialized cost, not the decoded key length: each entry
		// serializes as `"<key>":true,`, and encoding/json escapes quotes, backslashes,
		// control bytes and the HTML-sensitive set (every '<' becomes the six-byte
		// \u003c).
		encodedKey, _ := json.Marshal(k)
		total += len(encodedKey) + len(`:true,`)
		if total > maxPublicationLogBytes {
			return false
		}
	}
	return true
}

// readPrevious reads the persisted snapshot CONFINED to its own directory, the
// same shape h-f24 applied to the mapping loader's overrides read, which named
// this call site as the sibling that should be swept with it.
func (w *FeedWriter) readPrevious(ctx context.Context) ([]byte, error) {
	dir := filepath.Dir(w.path)
	root, err := os.OpenRoot(dir)
	if err != nil {
		// A parent that cannot hold a snapshot at all reads as ABSENT, which is what
		// the unconfined read did: it returned ENOTDIR and classifyPreviousReadError
		// mapped that to fresh-install, letting the real failure surface at the WRITE -
		// the right place for a deployment error of that shape.
		if fi, statErr := os.Stat(dir); statErr == nil && !fi.IsDir() {
			return nil, fmt.Errorf("%w: feed snapshot parent %s", syscall.ENOTDIR, dir)
		}
		return nil, err
	}
	defer func() {
		if clErr := root.Close(); clErr != nil {
			w.log.Warn("indexer: could not close feed snapshot directory handle",
				"dir", dir, "error", clErr)
		}
	}()
	return atomicfile.ReadBoundedInRoot(ctx, root, filepath.Base(w.path), maxFeedBytes)
}

// classifyPreviousReadError is loadPrevious's transient-versus-baseline decision for a
// snapshot read failure.
func (w *FeedWriter) classifyPreviousReadError(err error) (feedState, error) {
	switch {
	case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR):
		return feedState{baseline: true, reason: "fresh-install"}, nil
	case errors.Is(err, atomicfile.ErrFileTooLarge):
		w.log.Warn("previous feed snapshot exceeds size cap; re-baselining the feed journal",
			"path", w.path, "max_bytes", int64(maxFeedBytes), "error", err)
		return feedState{baseline: true, reason: "oversized"}, nil
	default:
		return feedState{}, fmt.Errorf("indexer: read previous feed snapshot %s: %w", w.path, err)
	}
}

// retainValidTitles copies the harvested-title cache forward, dropping the
// entries a rebuild must not apply and reporting how many were dropped: an
// empty title (nothing to upgrade a synthesized one with) and an entry whose
// key or title exceeds maxPersistedFieldBytes.
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

// warnedSet is the exclusion set splitCurationWarned builds and the carry side consumes
// as one value: keys holds the excluded journal keys (every directly excluded
// occurrence plus every duplicate removed through a shared identity - also the
// warned_excluded operator count), and ids holds the excluded identity-signal set
// (journal key AND info hash), transitively closed over shared identities by
// collectWarnedIdentities' graph traversal, which retracts uses to drop a previously
// journaled item whose stored info hash is excluded under a DIFFERENT tracker key.
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

// splitCurationWarned partitions the catalogue for the feed: it returns a copy of
// entries with every EXCLUDED torrent removed, plus the warnedSet the carry side
// consumes (see warnedSet for the two sets it holds and warnedSet.retracts for the
// retraction decision).
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

// filterWarnedTorrents is splitCurationWarned's second pass for one entry's torrents:
// it drops every occurrence the operator's tag policy excludes from the feed surface OR
// that shares an excluded identity signal (journal key or info hash), reporting whether
// anything was removed (the caller only swaps in the fresh slice then, keeping the
// shared input unmutated).
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
