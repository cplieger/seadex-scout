package indexer

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/cplieger/seadex-scout/internal/classify"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// journalItem is one persisted RSS-journal record: the served wire item plus
// the journal bookkeeping the wire never carries. FirstSeen is when the
// release entered the journal (PubDate mirrors it; the prune clock keys on
// it), Key is the torrent's stable tracker identity (nyaa:{id} / ab:{id} -
// the harvested-title cache key), and AniListID is the SeaDex entry's
// AniList id (the harvest query group). Proxied search results are plain
// items and are never persisted, so the type split makes the finding-class
// mistake unrepresentable: a change to the volatile Prowlarr parse shape
// cannot silently move the on-disk snapshot contract, and bookkeeping cannot
// leak into a search passthrough. encoding/json flattens the embedded item,
// so the persisted feed.json object keeps its exact historical flat shape.
type journalItem struct {
	FirstSeen time.Time `json:"FirstSeen,omitzero"`
	Key       string    `json:"Key,omitempty"`
	item
	AniListID int `json:"AniListID,omitempty"`
}

// feedJournalMaxAge bounds how long a newly curated release stays in the
// synthesized RSS journal. The arrs poll RSS on a minutes-scale sync interval,
// so 14 days is generous - it survives a week-long arr outage with margin -
// while keeping the feed a recent-additions journal rather than a catalogue
// re-broadcast. An aged-out item leaves the journal AND drops its cached
// harvested title; its identity stays in the never-pruned seen ledger, so it
// can never re-enter the journal as new.
const feedJournalMaxAge = 14 * 24 * time.Hour

// curatedRef points at one occurrence of a curated torrent. A torrent can be
// attached to several SeaDex entries, so a journal key can map to multiple
// refs; renderJournalItem folds them (best-wins marker, category union).
type curatedRef struct {
	entry   *seadex.Entry
	torrent *seadex.Torrent
}

// journalPass owns one rebuild's journal-scoped collaborators: the current
// catalogue index, the seen ledger, the curation-warned exclusion set, the
// entry-info lookup, the transition counters, and the rebuild clock. It exists
// so a new pass-scoped collaborator is one field rather than an edit to every
// signature on the carry/grow path, and so the journal pass is exercisable
// without a live writer. The writer itself stays a field: renderJournalItem
// and scopeConfigured read its passkey and per-tracker configured flags, which
// are writer state rather than pass state.
type journalPass struct {
	w       *FeedWriter
	cur     map[string][]curatedRef
	seen    map[string]bool
	ws      *warnedSet
	infoFor EntryInfoFunc
	js      *journalStats
	now     time.Time
}

// --- Journal identity ---

// journalKey returns a torrent's journal identity - its tracker key
// (nyaa:{id} / ab:{id}), the same stable id the search curation set and the
// harvest matching key on - or "" when the torrent has no parseable tracker
// id (such a torrent cannot be journaled: no download link is buildable
// either).
func journalKey(t *seadex.Torrent) string { return trackerKey(t.Tracker, t.URL) }

// identitySignals returns the CROSS-SCOPE identity forms of a curated
// torrent: its tracker key and its BARE info hash. This is the identity the
// curation-warned exclusion graph is built over (splitCurationWarned /
// sharesWarnedIdentity), where a warning against the bytes must retract every
// tracker listing of them - so the hash deliberately stays un-namespaced.
// RSS novelty is NOT decided here: the never-pruned seen ledger uses
// ledgerSignals, whose hash is scope-qualified so the same bytes listed on
// two trackers stay two separately journalable releases.
func identitySignals(t *seadex.Torrent) []string {
	var ids []string
	if k := journalKey(t); k != "" {
		ids = append(ids, k)
	}
	if h := validInfoHash(t.InfoHash); h != "" {
		ids = append(ids, h)
	}
	return ids
}

// ledgerSignals returns the identity forms the never-pruned seen ledger
// records for one curated torrent: its tracker key (already
// scope-namespaced) and its info hash NAMESPACED BY SCOPE. The hash is
// scope-qualified here and only here, because the ledger decides RSS novelty
// PER TRACKER FEED: two tracker records carrying the identical bytes (a
// release cross-posted to Nyaa and AnimeBytes) are two separately journalable
// releases, and folding a bare shared hash lets catalogue iteration order
// decide which of the two ever reaches RSS - the same failure the
// tail-tracker guard prevents one namespace up. The warned-identity graph
// deliberately keeps the bare, cross-scope hash (identitySignals): a curator
// warning against the bytes must retract every listing of them.
func ledgerSignals(scope string, t *seadex.Torrent) []string {
	var ids []string
	if k := journalKey(t); k != "" {
		ids = append(ids, k)
	}
	if h := validInfoHash(t.InfoHash); h != "" {
		ids = append(ids, scope+":h:"+h)
	}
	return ids
}

// allIdentities collects every identity signal in the current curation set:
// the seen ledger a baseline records, so the journal only grows from curation
// genuinely newer than the baseline.
func allIdentities(entries []seadex.Entry) map[string]bool {
	seen := make(map[string]bool)
	for i := range entries {
		for j := range entries[i].Torrents {
			t := &entries[i].Torrents[j]
			scope := trackerScope(t.Tracker)
			if scope == "" {
				// Tail-tracker occurrences never reach the ledger (the same
				// guard journalIfNew applies on the growth path): AnimeTosho
				// mirrors Nyaa with the IDENTICAL info hash, so folding it at
				// baseline would pre-mark a later Nyaa listing of the same
				// bytes as already seen and deny it RSS exposure forever.
				continue
			}
			if journalKey(t) == "" {
				// Same rule as journalIfNew's growth path: a torrent with no
				// journal key is unservable for an upstream DATA reason, so
				// folding its info hash would deny a later corrected record RSS
				// exposure forever (the ledger is never pruned).
				continue
			}
			for _, id := range ledgerSignals(scope, t) {
				seen[id] = true
			}
		}
	}
	return seen
}

// indexCurated groups the current catalogue's torrents by journal key, so the
// journal can re-render a carried item from its current source data and fold a
// torrent attached to several entries into one item.
func indexCurated(entries []seadex.Entry) map[string][]curatedRef {
	cur := make(map[string][]curatedRef)
	for i := range entries {
		for j := range entries[i].Torrents {
			t := &entries[i].Torrents[j]
			if k := journalKey(t); k != "" {
				cur[k] = append(cur[k], curatedRef{entry: &entries[i], torrent: t})
			}
		}
	}
	return cur
}

// scopeOfKey returns the tracker scope a journal key belongs to (the prefix of
// its "scope:id" form).
func scopeOfKey(key string) string {
	scope, _, _ := strings.Cut(key, ":")
	return scope
}

// journalIdentityMatches reports whether a journal item's stored GUID still
// proves its journal identity: a non-empty Key that the GUID resolves back
// to via trackerKeyFromURL. It is the ONE home of the journal's GUID-to-Key
// invariant, shared by the writer's carry gates and the reader's snapshot
// rebuild (rebuildDownloadURLs): a cross-key, foreign-host, empty, or
// undecodable GUID must never authorize serving - or deriving a download
// link for - a different torrent than the persisted curation binding names.
func journalIdentityMatches(it *journalItem) bool {
	return it.Key != "" && trackerKeyFromURL(it.GUID) == it.Key
}

// --- Journal item rendering ---

// renderJournalItem materializes the journal item for key from its current
// curated occurrences: synthesis from the first RENDERABLE occurrence in
// ascending AniList-ID order, then best-wins on the marker and category union
// across all of them (a torrent attached to several entries must not render
// conflicting duplicates). An occurrence is renderable when it yields a
// grabbable download link, a non-empty synthesized title, a GUID that proves
// the journal key (journalIdentityMatches), and fields within
// the persisted limits; trying siblings in a deterministic order keeps the
// render catalogue-order independent while one partial occurrence (no files
// and no release group on the lowest AniList ID) cannot deny the whole key
// RSS when a renderable sibling exists. ok is false only when EVERY
// occurrence is unrenderable: no grabbable download link (an AnimeBytes
// release without a passkey - reported via noPasskey so the caller can nudge
// the operator - or an id-less URL, which journalKey already excludes), no
// parseable title at all (no files and no release group), a page URL whose
// GUID cannot prove the journal key, or a field over the persisted size
// limits (validPersistedItem).
func (w *FeedWriter) renderJournalItem(key string, refs []curatedRef, infoFor EntryInfoFunc) (it journalItem, ok, noPasskey bool) {
	// Deterministic synthesis order: a torrent attached to several entries
	// must render the same item regardless of catalogue order (marker and
	// categories are already order-independent folds below).
	ordered := slices.Clone(refs)
	slices.SortStableFunc(ordered, func(a, b curatedRef) int {
		return cmp.Compare(a.entry.AniListID, b.entry.AniListID)
	})
	for _, occ := range ordered {
		dl, resolved := downloadURL(occ.torrent.Tracker, occ.torrent.URL, w.abPasskey)
		if !resolved {
			noPasskey = noPasskey || (scopeOfKey(key) == upstreamAB && w.abPasskey == "")
			continue
		}
		it = journalItem{
			item: item{
				Title:                synthesizeTitle(occ.torrent, infoFor(occ.entry.AniListID)),
				GUID:                 classify.PublishURL(occ.torrent),
				InfoURL:              entryURL(occ.entry.AniListID),
				DownloadURL:          dl,
				InfoHash:             validInfoHash(occ.torrent.InfoHash),
				DownloadVolumeFactor: dvfAlt,
				Size:                 totalSize(occ.torrent.Files),
			},
			Key:       key,
			AniListID: occ.entry.AniListID,
		}
		if it.Title == "" {
			// No episode files and no release group on this occurrence: an
			// arr cannot parse or match a title-less item, so try the next
			// occurrence (counted as unresolvable only when all fail).
			continue
		}
		if !journalIdentityMatches(&it) {
			// Creation-time enforcement of the journal's GUID-to-Key
			// invariant (the carry gates and the reader's rebuild already
			// enforce it): an occurrence whose page URL is unpublishable
			// (the publisher dropped it - e.g. a bad-port-bearing URL that
			// still passes trackerKey) would journal an item every
			// reader load then drops as undecodable. Try the next
			// occurrence.
			continue
		}
		foldRefs(&it, refs, infoFor)
		if !validPersistedItem(&it) {
			// An oversized external value (a SeaDex filename synthesized into
			// the title, an over-long URL) is unservable: renderFeed's XML
			// escaping could amplify it well past the container memory budget
			// (see maxPersistedFieldBytes). Try the next occurrence - the
			// caller has already folded the identity into the seen ledger, so
			// a fully unrenderable key never re-enters the journal as new.
			continue
		}
		return it, true, false
	}
	return journalItem{}, false, noPasskey
}

// foldRefs applies the order-independent folds across all of a torrent's
// curated occurrences: best-wins on the download-volume-factor marker and
// category union (a torrent attached to several entries must not render
// conflicting duplicates).
func foldRefs(it *journalItem, refs []curatedRef, infoFor EntryInfoFunc) {
	for _, ref := range refs {
		if ref.torrent.IsBest {
			it.DownloadVolumeFactor = dvfBest
		}
		for _, c := range categoriesFor(infoFor(ref.entry.AniListID).IsMovie) {
			if !slices.Contains(it.Categories, c) {
				it.Categories = append(it.Categories, c)
			}
		}
	}
	// The union above appends in catalogue order, which is the one input
	// renderJournalItem's sort exists to neutralize: without a canonical order
	// a torrent attached to both a movie and a series entry persists and
	// serves its categories in whichever order PocketBase returned the
	// relation in. Sorting makes the fold order-independent in its OUTPUT too,
	// not just as a set.
	slices.Sort(it.Categories)
}

// --- Rebuild accounting ---

// journalStats counts one rebuild's journal transitions for the snapshot log
// line.
type journalStats struct {
	added              int
	pruned             int
	dropped            int
	warned             int
	rebased            int
	unresolvable       int
	abSkippedNoPasskey int
}

// --- Carrying the previous journal ---

// carryItem re-renders or prunes one carried journal item, updating js, and
// reports whether it survives into the rebuilt journal: the top-down
// dispatcher over the three cohesive carry phases - validation/clock/expiry
// (prepareCarriedItem), the warned-set retraction, and the
// curated-vs-uncurated carry policy (refreshCarriedItem / carryStoredItem).
// ws is the curation-warned exclusion set splitCurationWarned built; a
// carried item it retracts (its key is excluded, or its stored info hash is
// warned under a DIFFERENT tracker key) is dropped (RSS must never keep
// serving bytes search suppresses).
func (p *journalPass) carryItem(it *journalItem, scope string) (journalItem, bool) {
	if !prepareCarriedItem(it, p.now, p.js) {
		return journalItem{}, false
	}
	if scopeOfKey(it.Key) != scope {
		// The feed an item was loaded from IS its scope binding. A key naming
		// the OTHER tracker (only reachable from a corrupted or hand-edited
		// snapshot) would otherwise re-render with the wrong tracker's
		// download link, consume a rate-paced harvest query against the wrong
		// upstream, and be dropped again by every reader load
		// (rebuild*DownloadURLs) for the item's whole journal window. Drop it
		// here instead.
		p.w.log.Debug("indexer journal item refused: key names another tracker scope",
			"key", it.Key, "feed_scope", scope, "cause", "scope-mismatch")
		p.js.dropped++
		return journalItem{}, false
	}
	if p.ws.retracts(it) {
		p.js.warned++
		return journalItem{}, false
	}
	refs, curated := p.cur[it.Key]
	if !curated {
		return p.carryStoredItem(it)
	}
	return p.refreshCarriedItem(it, refs)
}

// prepareCarriedItem applies carryItem's validation, clock-correction, and
// expiry phase, reporting whether the item is still carryable: a pre-journal
// item with no Key or FirstSeen is dropped, a future FirstSeen is rebased,
// and an item older than feedJournalMaxAge is pruned.
func prepareCarriedItem(it *journalItem, now time.Time, js *journalStats) bool {
	if it.Key == "" || it.FirstSeen.IsZero() {
		js.dropped++
		return false
	}
	if it.FirstSeen.After(now) {
		// A FirstSeen ahead of the wall clock (a clock rollback, or a
		// snapshot restored from a future-skewed host) would make the
		// max-age check below see a negative age and keep the item past the
		// bounded journal window. Rebase it to now - preserving the item
		// across the clock correction while bounding its remaining lifetime
		// to feedJournalMaxAge - and count the rebase for the snapshot log
		// line.
		rebaseFutureFirstSeen(it, now)
		js.rebased++
	}
	if now.Sub(it.FirstSeen) > feedJournalMaxAge {
		js.pruned++
		return false
	}
	return true
}

// rebaseFutureFirstSeen applies the clock-skew correction both consumers of a
// persisted snapshot need: an item whose FirstSeen sits ahead of now (a
// snapshot restored from a future-skewed host, a hand-edited year-9999 value)
// has BOTH its journal timestamp and its derived PubDate reset to now, so its
// remaining lifetime is bounded by feedJournalMaxAge and the served <pubDate>
// can never advertise a negative release age (an arr delay profile would
// otherwise hold the release indefinitely). It reports whether it corrected
// the item, so the writer can count the rebase (journalStats.rebased) and the
// reader can warn once per reload.
func rebaseFutureFirstSeen(it *journalItem, now time.Time) bool {
	if !it.FirstSeen.After(now) {
		return false
	}
	it.FirstSeen, it.PubDate = now, now
	return true
}

// rebaseFutureFeed applies rebaseFutureFirstSeen across a whole persisted feed,
// returning how many items it corrected.
func rebaseFutureFeed(feed []journalItem, now time.Time) int {
	rebased := 0
	for i := range feed {
		if rebaseFutureFirstSeen(&feed[i], now) {
			rebased++
		}
	}
	return rebased
}

// carryStoredItem applies carryItem's non-curated carry policy: an item whose
// torrent has left the curation set keeps its stored render, subject to the
// GUID-identity gate.
//
// A missing AB passkey is deliberately NOT a drop here. It used to be, which
// made removing `ab_passkey` a second irreversible off switch: one rebuild
// dropped every carried AB item and the never-pruned seen ledger stopped them
// ever returning (l-f161). A passkey only supplies the grabbable LINK - items
// persist GUID-only (stripDownloadURLs) and the reader clears the entire AB feed
// while no passkey is configured (rebuildABDownloadURLs) - so carrying them
// serves nothing prematurely and makes the switch reversible.
func (p *journalPass) carryStoredItem(it *journalItem) (journalItem, bool) {
	// Same GUID-identity gate as the curated arm (refreshCarriedItem): a
	// stored GUID that no longer proves this item's journal identity (a
	// cross-key, foreign-host, or empty GUID from a hand-edited snapshot)
	// must not be carried - unlike a curated item there is no fresh render
	// to self-heal from, and reload derives the SERVED download link from
	// the GUID, so a cross-key GUID would plant a fetch target for a
	// different torrent id on the same tracker for the item's whole journal
	// window.
	if !journalIdentityMatches(it) {
		// The GUID itself is not logged: it is an attacker-shapeable value from
		// a tamperable file, and the key is the diagnostic that identifies the
		// refused record.
		p.w.log.Debug("indexer journal item refused: stored GUID no longer proves its journal identity",
			"key", it.Key, "cause", "guid-identity")
		p.js.dropped++
		return journalItem{}, false
	}
	return *it, true
}

// refreshCarriedItem applies carryItem's still-curated carry policy: a fresh
// render from current data with the item's FirstSeen (and, when identity
// still holds, its GUID) preserved.
func (p *journalPass) refreshCarriedItem(it *journalItem, refs []curatedRef) (journalItem, bool) {
	fresh, ok, noPasskey := p.w.renderJournalItem(it.Key, refs, p.infoFor)
	if !ok {
		if noPasskey {
			// The AB passkey is the SECOND off switch for a tracker (beside
			// blanking its Torznab URL), and it must be as reversible as the
			// first: dropping the item here destroyed the AB journal on the
			// first rebuild after the operator removed the passkey, and because
			// the seen ledger is never pruned those releases could never return
			// (l-f161). The only thing a missing passkey costs is the grabbable
			// LINK, so keep the stored render instead - links are stripped at
			// rest anyway (stripDownloadURLs) and the reader re-derives them,
			// clearing the whole AB feed while no passkey is configured
			// (rebuildABDownloadURLs), so nothing unservable is served. When the
			// passkey comes back the carried item is renderable again.
			return p.carryStoredItem(it)
		}
		p.js.dropped++
		return journalItem{}, false
	}
	fresh.FirstSeen = it.FirstSeen
	fresh.PubDate = it.FirstSeen
	// GUID is journal identity, not refreshable presentation: the arrs
	// dedupe RSS releases by GUID, so a SeaDex URL-text change on the same
	// tracker identity (a query param appended, scheme/casing normalized)
	// must never mint a new GUID and re-trigger a grab for an
	// already-journaled torrent. Only a stored GUID that still proves the
	// same journal identity is kept (trackerKeyFromURL resolves it back to
	// this item's key): a malformed, foreign-host, or cross-key GUID from a
	// hand-edited snapshot would otherwise permanently displace the valid
	// fresh GUID and make reload drop the item every rebuild. Such a record
	// - like one with an empty stored GUID - self-heals from the fresh
	// render.
	if journalIdentityMatches(it) {
		fresh.GUID = it.GUID
	}
	return fresh, true
}

// carryJournal re-renders one scope's previous journal items against the
// current catalogue and prunes aged-out ones. A carried item whose torrent is
// still curated is re-synthesized from current data (its title, size, marker,
// and categories refresh; the harvested-title cache is applied by the caller
// after the harvest) with its FirstSeen preserved; one whose torrent left the
// curation set keeps its stored render (a curated-then-replaced torrent is
// still a valid release). An item older than feedJournalMaxAge leaves the
// journal (its cached title is dropped by the caller's retainTitles); an item
// whose torrent has become curation-warned (ws.retracts: its key is excluded,
// or its stored info hash is warned under a different tracker key - a warning
// under another key still retracts the shared bytes) is dropped
// - unlike a curated-then-replaced torrent, SeaDex's curators now warn
// against it, so serving it would hand the arrs a Broken/Incomplete release;
// a pre-journal item with no Key or FirstSeen (unreachable after a baseline,
// defensive against hand-edited snapshots) and a carried AnimeBytes item whose
// download link can no longer be built (the passkey was removed - the release
// is no longer grabbable, so serving it would be dead weight) are dropped; so
// is a carried item whose Key names the other tracker's scope (only reachable
// from a corrupted or hand-edited snapshot).
func (p *journalPass) carryJournal(prevFeed []journalItem, scope string) []journalItem {
	kept := make([]journalItem, 0, len(prevFeed))
	for i := range prevFeed {
		if it, ok := p.carryItem(&prevFeed[i], scope); ok {
			kept = append(kept, it)
		}
	}
	return kept
}

// --- Growing the journal ---

// growJournal adds the newly curated torrents to the per-scope journals and
// folds every current identity into the seen ledger. A torrent is NEW only
// when none of its identity signals is in seen - the tracker post date is
// deliberately not the novelty key, since SeaDex routinely adds old torrents.
// Every identity signal is recorded in seen whether or not the torrent could
// be journaled (an AnimeBytes release skipped for a missing passkey, an
// unconfigured tracker), so the journal only ever grows from curation that is
// new AT THE TIME it is served; backfill is search's job. A torrent with no
// journal key is the exception: it is unservable for an upstream DATA reason,
// so nothing is recorded for it and a corrected record still journals as new
// (see journalIfNew).
func (p *journalPass) growJournal(entries []seadex.Entry) (nyaa, ab []journalItem) {
	for i := range entries {
		for j := range entries[i].Torrents {
			it, scope, ok := p.journalIfNew(&entries[i].Torrents[j])
			if !ok {
				continue
			}
			it.FirstSeen, it.PubDate = p.now, p.now
			p.js.added++
			if scope == upstreamAB {
				ab = append(ab, it)
			} else {
				nyaa = append(nyaa, it)
			}
		}
	}
	return nyaa, ab
}

// scopeConfigured reports whether a tracker scope's Prowlarr Torznab URL is
// configured (the README's per-tracker on switch); "" (a tail tracker) is
// never configured.
func (w *FeedWriter) scopeConfigured(scope string) bool {
	return (scope == upstreamNyaa && w.nyaaConfigured) || (scope == upstreamAB && w.abConfigured)
}

// journalIfNew applies growJournal's novelty test to one torrent - folding its
// identity signals into seen either way - and materializes its journal item
// when it is genuinely new and servable. Two kinds of occurrence never reach
// the ledger at all: a tail tracker's, and one with no journal key (see the
// two guards below).
func (p *journalPass) journalIfNew(t *seadex.Torrent) (it journalItem, scope string, ok bool) {
	scope = trackerScope(t.Tracker)
	if scope == "" {
		// A tail tracker (AnimeTosho, RuTracker) can never be journaled - and
		// AnimeTosho is a Nyaa MIRROR carrying the IDENTICAL info hash, so
		// folding its identity into the seen ledger would, depending on
		// nothing but catalogue iteration order, mark the Nyaa listing of the
		// same bytes as already seen and silently deny it RSS exposure
		// forever. The deliberate fold-though-unservable cases below (an
		// unconfigured tracker's off switch, a missing AB passkey) are
		// different: those trackers CAN be enabled later, and their
		// identities must not backfill then - a tail tracker has no later.
		return journalItem{}, "", false
	}
	if journalKey(t) == "" {
		// No journal key: this torrent can never be journaled, and the reason
		// is an upstream DATA defect (a URL that fails the tracker-ownership
		// gate, or no stable identity at all - a configured AnimeBytes record
		// whose hash SeaDex redacts lands exactly here), NOT an operator
		// switch. So NOTHING is folded into the never-pruned ledger: the
		// deliberate fold-though-unservable cases below are the config ones
		// (an off tracker, a missing AB passkey), whose identities must not
		// backfill when the operator flips them, whereas a corrected upstream
		// record is a legitimate later republish that MUST journal as new -
		// the same reason the tail-tracker guard above folds nothing, and the
		// contract TestRebuildRejectsForeignHostTrackerURLs states. Folding
		// the info hash here also silenced the diagnostic after one cycle: the
		// next rebuild saw the hash as seen and returned before the count.
		// For an enabled, supported tracker surface it on the snapshot log
		// line instead of silently shrinking the feed; unknown tail trackers
		// and an intentionally disabled AB stay silent.
		if p.w.scopeConfigured(scope) {
			p.js.unresolvable++
		}
		return journalItem{}, "", false
	}
	ids := ledgerSignals(scope, t)
	isNew := true
	for _, id := range ids {
		if p.seen[id] {
			isNew = false
		}
		p.seen[id] = true
	}
	if !isNew {
		return journalItem{}, "", false
	}
	return p.newJournalItem(t, scope)
}

// newJournalItem resolves one newly curated torrent into its journal item and
// scope, updating the skip counters when it cannot be served: a non-Nyaa/AB
// tracker (the negligible SeaDex tail) is silently ignored, an unconfigured
// tracker (Nyaa or AnimeBytes without its Torznab URL) is skipped without
// persisting anything for it (the README's off switch; its identity is
// already in seen, so enabling it later starts from current novelty instead
// of backfilling disabled-era curation), a missing AB passkey
// counts toward the operator nudge, and an in-scope torrent with no parseable
// title counts as unresolvable so an upstream data change surfaces on the
// snapshot log line instead of silently shrinking the feed (unresolvable is
// counted only for configured scopes; the keyless case is refused and counted
// one level up, in journalIfNew).
func (p *journalPass) newJournalItem(t *seadex.Torrent, scope string) (journalItem, string, bool) {
	if !p.w.scopeConfigured(scope) {
		return journalItem{}, "", false
	}
	// journalIfNew's keyless guard already refused and counted a torrent with
	// no journal key, so the key is non-empty by construction here.
	key := journalKey(t)
	it, ok, noPasskey := p.w.renderJournalItem(key, p.cur[key], p.infoFor)
	if noPasskey {
		p.js.abSkippedNoPasskey++
	}
	if !ok {
		if !noPasskey {
			p.js.unresolvable++
		}
		return journalItem{}, "", false
	}
	return it, scope, true
}

// --- Harvested-title cache ---

// applyTitles upgrades each journal item's served title to its harvested real
// title when the cache holds one; items without a cached title keep their
// synthesized title (the permanent fallback). GUIDs never change with the
// title, so an upgrade cannot re-trigger a grab.
func applyTitles(items []journalItem, titles map[string]string) {
	for i := range items {
		if t, ok := titles[items[i].Key]; ok && t != "" {
			items[i].Title = t
		}
	}
}

// retainTitles prunes the harvested-title cache to the keys still present in
// the journal feeds, so an aged-out or dropped item's cached title leaves with
// it (its seen-ledger identity guarantees it can never return to need it).
func retainTitles(titles map[string]string, feeds ...[]journalItem) map[string]string {
	kept := make(map[string]string, len(titles))
	for _, feed := range feeds {
		for i := range feed {
			if t, ok := titles[feed[i].Key]; ok {
				kept[feed[i].Key] = t
			}
		}
	}
	return kept
}
