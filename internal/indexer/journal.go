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
// without a live writer. The writer itself stays a field: renderJournalItem and
// the grow path read its held UpstreamConfig (the passkey and the per-tracker
// on switches), which is writer state rather than pass state.
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
			if journalKey(t) == "" {
				// No journal key - an unsupported tail tracker, or a supported
				// tracker whose URL fails the ownership gate - so nothing is
				// folded into the never-pruned ledger (the same rule
				// journalIfNew applies on the growth path). Two reasons: an
				// AnimeTosho mirror carries Nyaa's IDENTICAL info hash, so
				// folding it at baseline would pre-mark a later Nyaa listing of
				// the same bytes as already seen; and a torrent unservable for
				// an upstream DATA reason must not deny a later corrected
				// record its RSS exposure.
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

// refusesUnprovenGUID reports whether a carried item fails the journal's
// GUID-to-Key invariant, logging and counting the refusal. ONE home for the
// gate both carry paths apply - the reconcile's non-curated arm
// (carryStoredItem, which has no fresh render to self-heal from) and the
// tick's expireCarried (which has no curation set at all) - so the
// operator-facing cause attribute behind the snapshot line's aggregate
// journal_dropped count cannot drift between them. It reports the refusal and
// nothing more: each caller keeps its own exit shape, because the two passes
// are deliberately different (Advance reaches prepareCarriedItem through
// expireCarried precisely because carryItem's other arms need the full
// curation set). The GUID itself is never logged: it is an attacker-shapeable
// value from a tamperable file, and the key is the diagnostic that identifies
// the refused record.
func (p *journalPass) refusesUnprovenGUID(it *journalItem) bool {
	if journalIdentityMatches(it) {
		return false
	}
	p.w.log.Debug("indexer journal item refused: stored GUID no longer proves its journal identity",
		"key", it.Key, "cause", "guid-identity")
	p.js.dropped++
	return true
}

// --- Journal item rendering ---

// renderJournalItem materializes the journal item for key from its current
// curated occurrences: synthesis from the first RENDERABLE occurrence in
// ascending AniList-ID order, then best-wins on the marker and category union
// across all of them (a torrent attached to several entries must not render
// conflicting duplicates). An occurrence is renderable when it yields a
// download target (journalLink - a grabbable link, or an AnimeBytes release
// awaiting the operator's passkey, journaled GUID-only), a non-empty
// synthesized title, a GUID that proves the journal key
// (journalIdentityMatches), and fields within
// the persisted limits; trying siblings in a deterministic order keeps the
// render catalogue-order independent while one partial occurrence (no files
// and no release group on the lowest AniList ID) cannot deny the whole key
// RSS when a renderable sibling exists. ok is false only when EVERY
// occurrence is unrenderable: no download target at all (an id-less URL, which
// journalKey already excludes, or a foreign host), no
// parseable title at all (no files and no release group), a page URL whose
// GUID cannot prove the journal key, or a field over the persisted size
// limits (validPersistedItem). noPasskey reports that at least one AnimeBytes
// occurrence had no derivable link because no passkey is configured - whether
// or not the item was journaled - so the caller can nudge the operator.
func (w *FeedWriter) renderJournalItem(key string, refs []curatedRef, infoFor EntryInfoFunc) (it journalItem, ok, noPasskey bool) {
	// Deterministic synthesis order: a torrent attached to several entries
	// must render the same item regardless of catalogue order (marker and
	// categories are already order-independent folds below).
	ordered := slices.Clone(refs)
	// The order must be TOTAL, not just AniList-ascending: two occurrences of
	// one journal key can share an AniList id (a duplicated trs relation row,
	// or two catalogue records carrying the same alID), and a stable sort then
	// leaves catalogue order deciding which one is synthesized - the exact
	// dependency this sort exists to remove. URL, info hash and tracker break
	// the tie on the torrent's own identity; the synthesized title and summed
	// size close it on the remaining first-occurrence output, so two refs that
	// still compare equal render byte-identical items (a duplicated relation
	// row can repeat the id, URL and hash while carrying different Files or
	// ReleaseGroup values).
	slices.SortStableFunc(ordered, func(a, b curatedRef) int {
		return cmp.Or(
			cmp.Compare(a.entry.AniListID, b.entry.AniListID),
			cmp.Compare(a.torrent.URL, b.torrent.URL),
			cmp.Compare(a.torrent.InfoHash, b.torrent.InfoHash),
			cmp.Compare(a.torrent.Tracker, b.torrent.Tracker),
			cmp.Compare(
				synthesizeTitle(a.torrent, infoFor(a.entry.AniListID)),
				synthesizeTitle(b.torrent, infoFor(b.entry.AniListID)),
			),
			cmp.Compare(totalSize(a.torrent.Files), totalSize(b.torrent.Files)),
		)
	})
	for _, occ := range ordered {
		dl, resolved, linkless := w.journalLink(occ.torrent)
		if linkless {
			// Journaled without a grabbable link (AnimeBytes, no passkey):
			// still report it so the caller can nudge the operator.
			noPasskey = true
		}
		if !resolved {
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
		return it, true, noPasskey
	}
	return journalItem{}, false, noPasskey
}

// journalLink resolves the download link for one journal render, splitting the
// AnimeBytes-passkey case out of plain unresolvability. It reports:
//
//   - ok with a link: the normal case.
//   - ok with an EMPTY link plus linkless=true: an AnimeBytes release that is
//     structurally sound (resolvableForScope) while no USABLE indexer.ab_passkey
//     is configured - absent, or an unexpanded ${VAR}/$VAR reference that is not
//     a credential at all (unusableABPasskey). The item is journaled anyway,
//     GUID-only.
//   - not ok: unresolvable for an upstream DATA reason (a foreign host, an
//     id-less URL, an unknown tracker), which must stay refused.
//
// Journaling the link-less AnimeBytes item is what makes the passkey a
// REVERSIBLE off switch on the GROWTH path too, matching the carry path's
// existing stance (carryStoredItem / refreshCarriedItem, l-f161). Skipping it
// lost the release permanently: journalIfNew folds the identity into the
// never-pruned seen ledger before the render, so a release curated during a
// passkey-less window could never journal as new afterwards - which made the
// operator nudge ("set indexer.ab_passkey") promise a recovery that never
// happened. Nothing unservable escapes: every feed persists GUID-only
// (stripDownloadURLs) and the reader re-derives each served link from the GUID,
// clearing the whole AnimeBytes feed while no passkey is configured
// (rebuildABDownloadURLs). When the passkey arrives the journaled item becomes
// grabbable on the next load, and it keeps aging out on the normal
// feedJournalMaxAge window meanwhile.
func (w *FeedWriter) journalLink(t *seadex.Torrent) (dl string, ok, linkless bool) {
	// unusableABPasskey is the app's ONE home for "can this passkey build a
	// grabbable AnimeBytes link" (server.go, over internal/secretref): an
	// unexpanded ${VAR} or $VAR reference is not a passkey, and passing it
	// through mints the literal placeholder into the link while reporting the
	// release as fully resolved. Normalize it to the empty passkey so both arms
	// below take the documented no-passkey path - the same path the reader
	// already takes (rebuildABDownloadURLs clears the whole AB feed).
	passkey := w.enablement.ABPasskey
	if unusableABPasskey(passkey) {
		passkey = ""
	}
	scope := trackerScope(t.Tracker)
	if dl, resolved := downloadURLForScope(scope, t.URL, passkey); resolved {
		return dl, true, false
	}
	if scope == upstreamAB && passkey == "" && resolvableForScope(scope, t.URL) {
		return "", true, true
	}
	return "", false, false
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
// line. abSkippedNoPasskey counts the AnimeBytes releases journaled without a
// grabbable link because no indexer.ab_passkey is configured (they ARE in the
// journal - see journalLink - and become grabbable when the passkey arrives);
// it keeps its name because the snapshot WARN publishes it as
// ab_releases_skipped, the attribute an operator's log queries already read.
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
	if !p.prepareCarriedItem(it) {
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
func (p *journalPass) prepareCarriedItem(it *journalItem) bool {
	if it.Key == "" || it.FirstSeen.IsZero() {
		p.js.dropped++
		return false
	}
	// The persisted InfoHash this phase hands to warnedSet.retracts is already
	// canonical: decodeSnapshot runs normalizeSnapshotItems (validInfoHash) on
	// both feeds for BOTH consumers before loadPrevious returns them, which is
	// where that invariant is documented and pinned
	// (TestRebuildCanonicalizesStoredHashBeforeWarningRetraction). Re-doing it
	// here would give one rule two homes and only the writer's copy.

	// A FirstSeen ahead of the wall clock (a clock rollback, or a snapshot
	// restored from a future-skewed host) would make the max-age check below
	// see a negative age and keep the item past the bounded journal window.
	// rebaseFutureFirstSeen owns that predicate and reports whether it
	// corrected the item - preserving the item across the clock correction
	// while bounding its remaining lifetime to feedJournalMaxAge - so the
	// count for the snapshot log line comes off its verdict rather than off a
	// second copy of the same test (the shape rebaseFutureFeed already uses).
	if rebaseFutureFirstSeen(it, p.now) {
		p.js.rebased++
	}
	if p.now.Sub(it.FirstSeen) > feedJournalMaxAge {
		p.js.pruned++
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
	if p.refusesUnprovenGUID(it) {
		return journalItem{}, false
	}
	return *it, true
}

// refreshCarriedItem applies carryItem's still-curated carry policy: a fresh
// render from current data with the item's FirstSeen (and, when identity
// still holds, its GUID) preserved. When the fresh render fails - for either
// reason - the item keeps its STORED render (carryStoredItem) rather than being
// dropped, since the never-pruned seen ledger makes a drop permanent.
func (p *journalPass) refreshCarriedItem(it *journalItem, refs []curatedRef) (journalItem, bool) {
	fresh, ok, noPasskey := p.w.renderJournalItem(it.Key, refs, p.infoFor)
	if !ok {
		if !noPasskey {
			// The fresh render failed for an upstream DATA reason on every
			// occurrence (a title that no longer synthesizes, an unpublishable
			// page URL, an over-limit field). It keeps the STORED render for the
			// same reason the passkey case does: dropping is PERMANENT, since
			// the never-pruned seen ledger stops growJournal ever re-admitting
			// the release, so a transient upstream data defect used to cost a
			// curated release its RSS exposure forever - the omission settled
			// feed-rss-filtering forbids ("a release is never omitted because
			// the app cannot parse or route it"). Nothing unservable escapes:
			// the stored item was rendered from valid data, is canonicalized at
			// decode, has its download link re-derived by the reader, and ages
			// out on the normal window - and the DE-CURATED arm already keeps a
			// stored render for a torrent with strictly less standing.
			p.w.log.Debug("indexer journal item kept on its stored render: still curated but no longer renderable",
				"key", it.Key, "cause", "render-unresolvable")
		}
		// Both failure reasons take the l-f161 stance: keep the stored render,
		// subject to carryStoredItem's GUID-identity gate. The passkey arm is
		// the residual AnimeBytes case (journalLink already journals an AB
		// release GUID-only, so the passkey itself no longer blocks a render)
		// and it must be as reversible as blanking the tracker's Torznab URL;
		// links are stripped at rest (stripDownloadURLs) and the reader
		// re-derives them, clearing the whole AB feed while no passkey is
		// configured (rebuildABDownloadURLs).
		return p.carryStoredItem(it)
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
// against it, so serving it would hand the arrs a Broken/Incomplete release.
// A pre-journal item with no Key or FirstSeen (unreachable after a baseline,
// defensive against hand-edited snapshots), or an item whose Key names the other
// tracker scope, is dropped; so is a NON-curated item whose stored GUID no longer
// proves its Key (there is no fresh render to self-heal from, and reload derives the
// served download link from that GUID). A still-curated item with such a GUID is
// normally kept: refreshCarriedItem re-renders it and simply does not carry the
// unproven GUID forward.
// A missing AB passkey is not a drop either: an AnimeBytes item re-renders
// GUID-only (journalLink) while the reader suppresses the ungrabbable feed, so the
// switch remains reversible. Neither is a fresh render that fails outright: a
// still-curated item whose current data no longer renders falls back to its
// STORED render (refreshCarriedItem hands it to carryStoredItem, whose GUID gate
// may drop it there), because dropping it would be permanent - the never-pruned
// seen ledger stops growJournal re-admitting the release once the upstream record
// is corrected.
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
// be journaled (an unconfigured tracker), so the journal only ever grows from
// curation that is new AT THE TIME it is served; backfill is search's job. A
// torrent with no journal key is the exception: it is unservable for an
// upstream DATA reason, so nothing is recorded for it and a corrected record
// still journals as new (see journalIfNew). An AnimeBytes release curated
// while no passkey is configured is NOT such an exception either way: it is
// journaled GUID-only (journalLink) so the passkey stays a reversible switch,
// and its identity is recorded like any other journaled release.
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

// journalIfNew applies growJournal's novelty test to one torrent - folding its
// identity signals into seen either way - and materializes its journal item
// when it is genuinely new and servable. An occurrence with no journal key
// never reaches the ledger at all (see the guard below).
func (p *journalPass) journalIfNew(t *seadex.Torrent) (it journalItem, scope string, ok bool) {
	scope = trackerScope(t.Tracker)
	if journalKey(t) == "" {
		// No journal key: this torrent can never be journaled. Either its
		// tracker is unsupported - a tail tracker (AnimeTosho, RuTracker),
		// which trackerKey refuses outright - or the reason is an upstream
		// DATA defect on a supported tracker (a URL that fails the
		// tracker-ownership gate, or no stable identity at all - a configured
		// AnimeBytes record whose hash SeaDex redacts lands exactly here).
		// Neither is an operator switch, so NOTHING is folded into the
		// never-pruned ledger: the deliberate fold-though-unservable cases
		// below are the config ones (an off tracker, a missing AB passkey),
		// whose identities must not backfill when the operator flips them,
		// whereas a corrected upstream record is a legitimate later republish
		// that MUST journal as new - the contract
		// TestRebuildRejectsForeignHostTrackerURLs states. A tail tracker has
		// the sharper version of the same rule: AnimeTosho is a Nyaa MIRROR
		// carrying the IDENTICAL info hash, so folding its identity would,
		// depending on nothing but catalogue iteration order, mark the Nyaa
		// listing of the same bytes as already seen and silently deny it RSS
		// exposure forever - and unlike a disabled tracker, a tail tracker has
		// no later. Folding the info hash here also silenced the diagnostic
		// after one cycle: the next rebuild saw the hash as seen and returned
		// before the count. For an enabled, supported tracker surface it on
		// the snapshot log line instead of silently shrinking the feed;
		// unknown tail trackers (enabled("") is false) and an intentionally
		// disabled AB stay silent.
		if p.w.enablement.enabled(scope) {
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
// scope, updating the skip counters when it cannot be served: an unconfigured
// tracker (Nyaa or AnimeBytes without its Torznab URL) is skipped without
// persisting anything for it (the README's off switch; its identity is
// already in seen, so enabling it later starts from current novelty instead
// of backfilling disabled-era curation), an AnimeBytes release journaled
// GUID-only for want of a passkey counts toward the operator nudge (it IS
// journaled - see journalLink - so the nudge's implied recovery actually
// happens when the passkey arrives), and an in-scope torrent with no parseable
// title counts as unresolvable so an upstream data change surfaces on the
// snapshot log line instead of silently shrinking the feed (unresolvable is
// counted only for configured scopes; the keyless case is refused and counted
// one level up, in journalIfNew).
func (p *journalPass) newJournalItem(t *seadex.Torrent, scope string) (journalItem, string, bool) {
	if !p.w.enablement.enabled(scope) {
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
//
// The harvested title still WINS: the audit never falls back to the synthesized
// title and never drops an item (both forbidden). Its one intervention is a
// surgical rewrite of a season claim the file list proves wrong - see
// titleAudit.served.
func applyTitles(items []journalItem, titles map[string]string, audit titleAudit) {
	for i := range items {
		t, ok := titles[items[i].Key]
		if !ok || t == "" {
			continue
		}
		items[i].Title = audit.served(items[i].Key, t)
	}
}

// packCensus is what ONE journal key's file list proves about its episode count
// (packEvidenceOf), plus the census's own single-episode marker - the token a
// correction splices into a title that wrongly claims a whole season. The marker
// is meaningful only for packEvidenceSingle.
type packCensus struct {
	marker   string
	evidence packEvidence
}

// titleAudit is applyTitles' title-vs-file-list cross-check, and the one place a
// provably-wrong season-pack claim is corrected: census holds the three-valued
// file evidence per journal key (censusPacks) and report is the per-rebuild
// diagnostic sink (FeedWriter.packDisagreementReporter). The zero value disables
// both - what a caller with no census to compare against passes (a baselined
// rebuild, whose journal is empty anyway).
type titleAudit struct {
	census map[string]packCensus
	report func(key string, titlePack, filesPack, corrected bool)
}

// served returns the title to serve for one upgraded item, reporting a
// disagreement between the harvested title and the release's own file list.
//
// A title claiming a whole SEASON over a file list that positively proves ONE
// episode (packEvidenceSingle) is corrected in place: Sonarr parses FullSeason
// from such a title, ranks it above loose episodes, and once it grabs it treats
// that season as covered - so the season's real episodes are suppressed and the
// operator silently ends up missing them. Rewriting only the season token
// (correctSeasonOnlyTitle) removes the false claim while keeping every other byte
// of the tracker's real release name, which is what Sonarr reads for quality and
// custom-format decisions.
//
// Everything else is served verbatim, deliberately:
//
//   - packEvidenceUnknown is NOT a disagreement: zero recognized tokens means
//     absent files or naming outside the recognized forms, which proves nothing
//     about the payload. Nothing to report, nothing to correct - reading it as
//     single-episode evidence is exactly the false correction the three-valued
//     census exists to prevent.
//   - a title naming an EPISODE over a pack census keeps warning and is NOT
//     corrected: its consequence is weaker (the release merely loses the pack
//     ranking) and correcting it would cost the real title.
func (a titleAudit) served(key, title string) string {
	c, ok := a.census[key]
	if !ok {
		return title
	}
	titlePack, known := packFromTitle(title)
	if !known {
		return title
	}
	switch {
	case titlePack && c.evidence == packEvidenceSingle:
		corrected, done := correctedTitle(title, c.marker)
		a.warn(key, true, false, done)
		return corrected
	case !titlePack && c.evidence == packEvidencePack:
		a.warn(key, false, true, false)
		return title
	default:
		return title
	}
}

// warn reports one disagreement through the audit's sink, if it has one.
func (a titleAudit) warn(key string, titlePack, filesPack, corrected bool) {
	if a.report == nil {
		return
	}
	a.report(key, titlePack, filesPack, corrected)
}

// correctedTitle applies the season-token rewrite, refusing it when the marker
// is unreadable - defensively, since packEvidenceSingle guarantees a recognized
// token - or when the rewritten title would exceed the persisted-field cap: an
// over-limit title is dropped by the shared decode gate on reload
// (validPersistedItem), and losing the item entirely is far worse than serving
// its wrong season claim with a diagnostic that explains it.
func correctedTitle(title, marker string) (string, bool) {
	corrected, ok := correctSeasonOnlyTitle(title, marker)
	if !ok || len(corrected) > maxPersistedFieldBytes {
		return title, false
	}
	return corrected, true
}

// censusPacks reads the three-valued file-list evidence (packEvidenceOf) for
// every journal key in the current catalogue, so applyTitles can tell what a
// harvested title claims from what the release actually ships. A key's
// occurrences are the SAME tracker torrent attached to several entries, and the
// fold across them takes the STRONGEST evidence (pack over single over unknown)
// so the result cannot depend on catalogue order - the same order-independence
// renderJournalItem's sort exists to guarantee for the item itself.
func censusPacks(cur map[string][]curatedRef) map[string]packCensus {
	packs := make(map[string]packCensus, len(cur))
	for key, refs := range cur {
		var c packCensus
		for _, ref := range refs {
			if e := packEvidenceOf(ref.torrent); e > c.evidence {
				c.evidence = e
			}
		}
		if c.evidence == packEvidenceSingle {
			c.marker = censusMarker(refs)
		}
		packs[key] = c
	}
	return packs
}

// censusMarker returns the single-episode marker a corrected title is built
// from: the smallest non-empty marker across the key's occurrences, so the
// choice cannot depend on catalogue order either (the occurrences are the same
// tracker torrent, so in practice they all name the same episode).
func censusMarker(refs []curatedRef) string {
	best := ""
	for _, ref := range refs {
		m := singleEpisodeMarker(ref.torrent.Files)
		if m == "" {
			continue
		}
		if best == "" || m < best {
			best = m
		}
	}
	return best
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
