package indexer

import (
	"cmp"
	"slices"
	"strconv"
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
// harvested title; its identity stays in the never-pruned publication log, so
// it can never re-enter the journal as new.
const feedJournalMaxAge = 14 * 24 * time.Hour

// curatedRef points at one occurrence of a curated torrent. A torrent can be
// attached to several SeaDex entries, so a journal key can map to multiple
// refs; renderJournalItem folds them (best-wins marker, category union).
type curatedRef struct {
	entry   *seadex.Entry
	torrent *seadex.Torrent
}

// journalPass owns one pass's journal-scoped collaborators: the curation
// EVIDENCE this pass holds (see curationEvidence - a catalogue pass and a
// windowed pass hold different types, so the windowed one cannot be handed a
// catalogue-wide warned set), the loaded publication log, this pass's own
// publications, the entry-info lookup, the transition counters, and the pass
// clock. It exists so a new pass-scoped collaborator is one field rather than an
// edit to every signature on the carry/grow path, and so the journal pass is
// exercisable without a live writer. The writer itself stays a field:
// renderJournalItem and the grow path read its held UpstreamConfig (the passkey
// and the per-tracker on switches), which is writer state rather than pass
// state.
//
// published and publish are deliberately two maps, not one mutated in place: the
// publication log's rule is APPEND, and separating what was already recorded
// from what THIS pass published is what lets the rule be applied at the persist
// boundary (appendPublished) instead of by whoever happened to hold the map.
type journalPass struct {
	w         *FeedWriter
	ev        curationEvidence
	published map[string]bool
	publish   map[string]bool
	// prior is the ownership fact the PREVIOUS snapshot holds (feedState.owners):
	// the only record of the votes of the owners this pass did NOT evaluate. The
	// renderPartial arm carries them onto an item journaled for the first time,
	// so its marker and its categories cover the same owner set the search index
	// ORs over (projectCuration). A catalogue pass never reads it: there,
	// absence from the input IS de-curation and a stored vote must not count.
	prior map[string][]ownedRelease
	// evaluated memoizes the ownerKey set this pass evaluated, so a vote the pass
	// has already replaced is never carried on top of its own fresh evidence.
	// Built on first use by evaluatedOwners; one pass runs on one goroutine.
	evaluated map[string]bool
	infoFor   EntryInfoFunc
	js        *journalStats
	now       time.Time
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
// RSS novelty is NOT decided here: the never-pruned publication log uses
// publicationSignals, whose hash is scope-qualified so the same bytes listed on
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

// publicationSignals returns the identity forms the never-pruned publication log
// records for one curated torrent: its tracker key (already
// scope-namespaced) and its info hash NAMESPACED BY SCOPE. The hash is
// scope-qualified here and only here, because the log decides RSS novelty
// PER TRACKER FEED: two tracker records carrying the identical bytes (a
// release cross-posted to Nyaa and AnimeBytes) are two separately journalable
// releases, and folding a bare shared hash lets catalogue iteration order
// decide which of the two ever reaches RSS - the same failure the
// tail-tracker guard prevents one namespace up. The warned-identity graph
// deliberately keeps the bare, cross-scope hash (identitySignals): a curator
// warning against the bytes must retract every listing of them.
func publicationSignals(scope string, t *seadex.Torrent) []string {
	var ids []string
	if k := journalKey(t); k != "" {
		ids = append(ids, k)
	}
	if h := validInfoHash(t.InfoHash); h != "" {
		ids = append(ids, scope+":h:"+h)
	}
	return ids
}

// baselinePublications collects every identity signal in the current curation
// set: what a BASELINE forfeits into the publication log so the journal only
// grows from curation genuinely newer than the baseline.
//
// A baseline is the one write to the log that is not a publication, and it is
// honest about that: it FORFEITS the current catalogue rather than claiming it
// was served. The alternative - starting with an empty log - would broadcast the
// whole catalogue as newly curated, which is the catalogue re-broadcast the log
// exists to prevent. Backfill is search's job.
func baselinePublications(entries []seadex.Entry) map[string]bool {
	published := make(map[string]bool)
	for i := range entries {
		for j := range entries[i].Torrents {
			t := &entries[i].Torrents[j]
			scope := trackerScope(t.Tracker)
			if journalKey(t) == "" {
				// No journal key - an unsupported tail tracker, or a supported
				// tracker whose URL fails the ownership gate - so nothing is
				// forfeited. Two reasons: an AnimeTosho mirror carries Nyaa's
				// IDENTICAL info hash, so forfeiting it would pre-mark a later
				// Nyaa listing of the same bytes as already published; and a
				// torrent unservable for an upstream DATA reason must not deny a
				// later corrected record its RSS exposure.
				continue
			}
			for _, id := range publicationSignals(scope, t) {
				published[id] = true
			}
		}
	}
	return published
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
// download target (journalLink - a grabbable link; an AnimeBytes release with
// no usable passkey is NOT renderable and nothing is published for it), a
// non-empty
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
	// one journal key can share an AniList id (a duplicated trs relation row on
	// one record - NOT two catalogue records sharing an alID, which
	// seadexapi.validatePageIdentities refuses outright at either scope), and a
	// stable sort then leaves catalogue order deciding which one is synthesized -
	// the exact dependency this sort exists to remove. URL, info hash and tracker
	// break the tie on the torrent's own identity; the synthesized title and
	// summed size close it on the remaining first-occurrence output, so two refs
	// that still compare equal render byte-identical items (a duplicated relation
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
			// Not journaled for want of a grabbable link (AnimeBytes, no
			// passkey): report it so the caller can nudge the operator. Nothing
			// is published, so the release journals as new once the passkey
			// arrives (see journalLink).
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
			// (see maxPersistedFieldBytes). Try the next occurrence - and if
			// EVERY occurrence fails nothing is published, so nothing is
			// recorded and a corrected upstream record still journals as new.
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
//   - not ok with linkless=true: an AnimeBytes release that is structurally
//     sound (resolvableForScope) while no USABLE indexer.ab_passkey is
//     configured - absent, or an unexpanded ${VAR}/$VAR reference that is not a
//     credential at all (unusableABPasskey). NOTHING is journaled and nothing is
//     published; the flag exists only so the caller can nudge the operator.
//   - not ok with linkless=false: unresolvable for an upstream DATA reason (a
//     foreign host, an id-less URL, an unknown tracker), which must stay
//     refused.
//
// Refusing the render is what makes the passkey a REVERSIBLE off switch now that
// the publication log records what was SERVED rather than what was examined: a
// release the app could not hand an arr was never published, so nothing is
// recorded for it and it journals as new the first pass after the passkey
// arrives. This deliberately REPLACES the older exemption that journaled the
// item GUID-only (l-f161): that shape only dodged the permanence of a ledger
// written on examination, and with the log written on publication the general
// rule - a failed render is retryable, never terminal - covers the case at the
// root. The operator nudge still fires (newJournalItem counts
// abSkippedNoPasskey), and it now promises a recovery the log actually allows.
func (w *FeedWriter) journalLink(t *seadex.Torrent) (dl string, ok, linkless bool) {
	// Both arms read unusableABPasskey - the app's ONE home for "can this
	// passkey build a grabbable AnimeBytes link" (server.go, over
	// internal/secretref), applied inside downloadURLForScope for the first arm
	// - so neither carries its own emptiness test. An unexpanded ${VAR} or $VAR
	// reference is not a passkey, and it takes the documented no-passkey path
	// here, exactly as the reader already does (rebuildABDownloadURLs clears the
	// whole AB feed). On the daemon path config's validateABPasskey has already
	// refused a configured-but-malformed passkey on an ENABLED AB feed, so in a
	// running deployment this predicate distinguishes the off switch; it stays
	// fail-closed for any other construction of the writer.
	scope := trackerScope(t.Tracker)
	if dl, resolved := downloadURLForScope(scope, t.URL, w.enablement.ABPasskey); resolved {
		return dl, true, false
	}
	if scope == upstreamAB && unusableABPasskey(w.enablement.ABPasskey) && resolvableForScope(scope, t.URL) {
		return "", false, true
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

// journalStats counts one pass's journal transitions for the pass log
// line. abSkippedNoPasskey counts the AnimeBytes releases this pass could not
// turn into a grabbable link because no indexer.ab_passkey is configured (they
// are NOT journaled and nothing is published for them - see journalLink - so
// they journal as new once the passkey arrives); it keeps its name because the
// pass WARN publishes it as
// ab_releases_skipped, the attribute an operator's log queries already read.
// harvest carries the title harvest's own counters, zero at window scope where
// the harvest deliberately does not run (see pass.go's catalogue-only steps).
type journalStats struct {
	harvest            harvestStats
	added              int
	pruned             int
	dropped            int
	warned             int
	rebased            int
	unresolvable       int
	abSkippedNoPasskey int
	// storedDecurated counts the items kept on their stored render because the
	// torrent has left SeaDex's curation set - the state this app's runbook
	// calls an expected steady state for up to feedJournalMaxAge, and the one
	// number an operator diagnosing "the feed lists it, the site does not" needs.
	storedDecurated int
	// storedUnrenderable counts the items kept on their stored render while
	// STILL curated, because every current occurrence failed to render. It is the
	// carry-path twin of unresolvable: without it a systematic upstream data
	// change freezes every carried render with no signal above Debug.
	storedUnrenderable int
}

// --- Carrying the previous journal ---

// carryItem re-renders or prunes one carried journal item, updating js, and
// reports whether it survives into the rebuilt journal: the top-down
// dispatcher over the cohesive carry phases - validation/clock/expiry
// (prepareCarriedItem), the warned retraction, and the carry policy
// (refreshCarriedItem / carryStoredItem / carryUnevaluatedItem).
//
// EVERY arm acts on evidence this pass HOLDS. The age-out reads the item's own
// FirstSeen; the retraction reads a positive exclusion the pass evaluated; the
// de-curated arm reads absence from the input and is therefore reachable ONLY at
// catalogue scope (a window's carryPolicy is always carryVerbatim - see
// windowEvidence.carryPolicy). Routing the arms through the pass's evidence is
// what makes that structural rather than documented; the two carry functions it
// replaced disagreed on an item with an unproven stored GUID, and
// carryUnevaluatedItem's doc records why the window's verdict was the
// irreversible one.
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
		// here instead - the criterion is the item's own Key, so this is sound
		// at either pass scope.
		p.w.log.Debug("indexer journal item refused: key names another tracker scope",
			"key", it.Key, "feed_scope", scope, "cause", "scope-mismatch")
		p.js.dropped++
		return journalItem{}, false
	}
	if p.ev.retracts(it) {
		p.js.warned++
		return journalItem{}, false
	}
	switch policy, refs := p.ev.carryPolicy(it.Key); policy {
	case carryRefreshed:
		return p.refreshCarriedItem(it, refs)
	case carryDeCurated:
		kept, keptOK := p.carryStoredItem(it)
		if keptOK {
			p.js.storedDecurated++
		}
		return kept, keptOK
	default:
		return p.carryUnevaluatedItem(it)
	}
}

// carryUnevaluatedItem is the WINDOW's arm, and its ONLY arm: this pass holds no
// evidence it may act on for this item, so the stored item is carried VERBATIM -
// no de-curation verdict, no re-render, and deliberately no GUID gate.
//
// Skipping the GUID gate here is the fix for a real disagreement, not a
// weakening. The tick used to apply the gate and DROP on failure while the
// reconcile routed the same item to a fresh render that self-heals its GUID, so
// a tick permanently discarded (the publication log is never pruned) an item the
// reconcile would have repaired. Since only the reconcile holds the evidence to
// repair it, the sound window verdict is to leave it alone and let the reconcile
// decide within one reconcile interval. Nothing unservable escapes meanwhile:
// the reader applies the same GUID-to-Key invariant at serve time
// (rebuild*DownloadURLs), so an item with an unproven GUID is not served
// regardless of what the file holds.
func (p *journalPass) carryUnevaluatedItem(it *journalItem) (journalItem, bool) {
	return *it, true
}

// prepareCarriedItem applies carryItem's clock-correction and expiry phase,
// reporting whether the item is still carryable: a future FirstSeen is rebased,
// and an item older than feedJournalMaxAge is pruned.
//
// It does NOT re-test that the item carries a Key and a FirstSeen. That
// invariant has one home, the shared decode gate both consumers cross
// (validJournalRecord, applied by pruneJournalFeed inside decodeSnapshot), and
// the writer reaches this path only through loadPrevious - the same
// one-rule-one-home reasoning the InfoHash note below states. Both shapes stay
// refused regardless: a zero FirstSeen saturates now.Sub to the maximum
// Duration and is pruned by the age check below, and an empty Key fails
// carryItem's scope gate.
func (p *journalPass) prepareCarriedItem(it *journalItem) bool {
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

// carryStoredItem applies carryItem's DE-CURATED carry policy: an item whose
// torrent has left the curation set - which only a CATALOGUE pass can establish
// - keeps its stored render, subject to the GUID-identity gate.
//
// A missing AB passkey is deliberately NOT a drop here. It used to be, which
// made removing `ab_passkey` a second irreversible off switch: one rebuild
// dropped every carried AB item and the never-pruned publication log stopped them
// ever returning (l-f161). A passkey only supplies the grabbable LINK - items
// persist GUID-only (stripDownloadURLs) and the reader clears the entire AB feed
// while no passkey is configured (rebuildABDownloadURLs) - so carrying them
// serves nothing prematurely and makes the switch reversible.
func (p *journalPass) carryStoredItem(it *journalItem) (journalItem, bool) {
	// The journal's GUID-to-Key invariant, applied by the one arm that has both
	// the standing to refuse and no fresh render to self-heal from: a stored GUID
	// that no longer proves this item's journal identity (a cross-key,
	// foreign-host, or empty GUID from a hand-edited snapshot) must not be
	// carried, because reload derives the SERVED download link from that GUID, so
	// a cross-key GUID would plant a fetch target for a different torrent id on
	// the same tracker for the item's whole journal window. The curated arm
	// (refreshCarriedItem) reaches this by delegating here; a WINDOW pass
	// deliberately does not apply it at all (see carryUnevaluatedItem), since it
	// cannot re-render and refusing there would make a repair the catalogue pass
	// would have performed permanently impossible.
	//
	// The GUID itself is never logged: it is an attacker-shapeable value from a
	// tamperable file, and the key is the diagnostic that identifies the record.
	if !journalIdentityMatches(it) {
		p.w.log.Debug("indexer journal item refused: stored GUID no longer proves its journal identity",
			"key", it.Key, "cause", "guid-identity")
		p.js.dropped++
		return journalItem{}, false
	}
	return *it, true
}

// refreshCarriedItem applies carryItem's still-curated carry policy: a fresh
// render from current data with the item's FirstSeen (and, when identity
// still holds, its GUID) preserved. When the fresh render fails - for either
// reason - the item keeps its STORED render (carryStoredItem) rather than being
// dropped, since the never-pruned publication log makes a drop permanent.
func (p *journalPass) refreshCarriedItem(it *journalItem, refs []curatedRef) (journalItem, bool) {
	fresh, ok, noPasskey := p.w.renderJournalItem(it.Key, refs, p.infoFor)
	if !ok {
		if !noPasskey {
			// The fresh render failed for an upstream DATA reason on every
			// occurrence (a title that no longer synthesizes, an unpublishable
			// page URL, an over-limit field). It keeps the STORED render for the
			// same reason the passkey case does: dropping is PERMANENT, since
			// the never-pruned publication log stops the growth path ever
			// re-admitting the release, so a transient upstream data defect used
			// to cost a curated release its RSS exposure forever - the omission
			// settled feed-rss-filtering forbids ("a release is never omitted
			// because the app cannot parse or route it"). Nothing unservable
			// escapes: the stored item was rendered from valid data, is
			// canonicalized at decode, has its download link re-derived by the
			// reader, and ages out on the normal window - and the DE-CURATED arm
			// already keeps a stored render for a torrent with strictly less
			// standing.
			p.w.log.Debug("indexer journal item kept on its stored render: still curated but no longer renderable",
				"key", it.Key, "cause", "render-unresolvable")
		}
		// Both failure reasons take the l-f161 stance: keep the stored render,
		// subject to carryStoredItem's GUID-identity gate. The passkey arm is
		// the residual AnimeBytes case (a fresh render cannot produce a
		// grabbable link while no usable passkey is configured, so journalLink
		// refuses it) and it must be as reversible as blanking the tracker's
		// Torznab URL; links are stripped at rest (stripDownloadURLs) and the
		// reader re-derives them, clearing the whole AB feed while no passkey is
		// configured (rebuildABDownloadURLs).
		kept, keptOK := p.carryStoredItem(it)
		if keptOK {
			p.js.storedUnrenderable++
		}
		return kept, keptOK
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

// carryJournal re-renders or prunes one scope's previous journal items against
// this pass's evidence. It is the same function at both pass scopes, and every
// verdict it can reach is one the pass has evidence for:
//
//   - an item past feedJournalMaxAge leaves, and a future FirstSeen is rebased
//     (prepareCarriedItem) - the criterion is the item's OWN timestamp, so this
//     is sound at any scope;
//   - an item with no Key or FirstSeen, or one whose Key names the
//     other tracker scope, is dropped - again on the item's own fields;
//   - an item whose torrent has become curation-warned is dropped
//     (curationEvidence.retracts, which a window answers over its own evidence:
//     its key is excluded, or its stored info hash is warned under a different
//     tracker key). Unlike a curated-then-replaced torrent, SeaDex's curators now
//     warn against it, so serving it would hand the arrs a Broken/Incomplete
//     release. This acts on a POSITIVE exclusion, so a window may do it too;
//   - a still-EVALUATED item is re-synthesized from current data (title, size,
//     marker and categories refresh; the harvested-title cache is applied by the
//     caller) with its FirstSeen preserved. Only a CATALOGUE pass reaches this
//     arm: the render folds the marker and categories across every occurrence of
//     the key, so a window's fold would drop an out-of-window parent's vote (see
//     windowEvidence.carryPolicy);
//   - an item the CATALOGUE pass finds absent has genuinely left the curation set
//     and keeps its stored render (a curated-then-replaced torrent is still a
//     valid release), subject to the GUID gate - there is no fresh render to
//     self-heal from, and reload derives the served download link from that GUID;
//   - a WINDOW pass carries every item verbatim, because it holds no evidence it
//     may act on (carryUnevaluatedItem).
//
// A missing AB passkey is not a drop for an ALREADY-JOURNALED item: its fresh
// render cannot produce a grabbable link (journalLink refuses it), so it falls
// back to its stored render while the reader suppresses the ungrabbable feed,
// and the switch remains reversible. Neither is a fresh render that fails
// outright: a
// still-curated item whose current data no longer renders falls back to its
// STORED render, because dropping it would be permanent - the never-pruned
// publication log stops the growth path re-admitting the release once the
// upstream record is corrected.
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

// growJournal adds the newly published torrents to the per-scope journals.
//
// THE LEDGER WRITE IS ON PUBLICATION, NOT EXAMINATION, and that is the whole of
// this half of the rewrite. A torrent is NEW only when none of its identity
// signals is already in the publication log - the tracker post date is
// deliberately not the novelty key, since SeaDex routinely adds old torrents -
// and its identity is recorded when it ENTERS a feed, never on refusal. The log
// therefore answers "what was SERVED", which is the only question a permanent,
// never-pruned record may answer: an unconfigured scope, a failed render, or a
// tag-excluded release admitted by a narrow window can no longer burn an
// identity with nothing published, so a corrected upstream record journals as
// new instead of being permanently invisible to RSS.
//
// The one write that is not a publication is the OFF-TRACKER case, and it is
// deliberate rather than an exemption to the rule above. A tracker with no
// Torznab URL is opted out, not refused: nothing was examined and nothing was
// judged unservable, the operator simply turned the surface off. Recording its
// identities is what keeps "re-broadcast stays impossible" true - without it,
// enabling a tracker after a period of disuse would journal that tracker's
// entire curated catalogue as newly curated in one pass, which is exactly the
// catalogue re-broadcast the log exists to prevent. It is baselining that scope,
// not logging a publication, and growJournal says so at the site.
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

// alreadyPublished reports whether any of a torrent's identity signals is
// already in the publication log - either from a previous pass or from earlier
// in THIS one, so two ENTRIES naming the same release cannot both journal it.
// That is the reachable duplicate: ~4.4% of torrents are attached to several
// entries, each a DIFFERENT AniList id. Two records under ONE alID is a
// different thing and cannot arrive (seadexapi.validatePageIdentities).
func (p *journalPass) alreadyPublished(ids []string) bool {
	for _, id := range ids {
		if p.published[id] || p.publish[id] {
			return true
		}
	}
	return false
}

// record appends a torrent's identity signals to this pass's publication set.
// APPEND is the log's only rule: nothing here ever removes an entry, because you
// cannot un-serve something.
func (p *journalPass) record(ids []string) {
	for _, id := range ids {
		p.publish[id] = true
	}
}

// journalIfNew applies growJournal's novelty test to one torrent and
// materializes its journal item when it is genuinely new and servable. It is
// the ONE site that decides what enters the publication log, and every arm below
// says which of the three things it is doing: publishing, baselining a
// disabled scope, or recording nothing at all.
func (p *journalPass) journalIfNew(t *seadex.Torrent) (it journalItem, scope string, ok bool) {
	scope = trackerScope(t.Tracker)
	key := journalKey(t)
	if key == "" {
		// No journal key: this torrent can never be journaled. Either its
		// tracker is unsupported - a tail tracker (AnimeTosho, RuTracker),
		// which trackerKey refuses outright - or the reason is an upstream
		// DATA defect on a supported tracker (a URL that fails the
		// tracker-ownership gate, or no stable identity at all - a configured
		// AnimeBytes record whose hash SeaDex redacts lands exactly here).
		// NOTHING is recorded, which is now simply the general rule (nothing was
		// published) rather than a special guard bolted on ahead of the fold. A
		// tail tracker shows why it must be so: AnimeTosho is a Nyaa MIRROR
		// carrying the IDENTICAL info hash, so recording its identity would,
		// depending on nothing but catalogue iteration order, mark the Nyaa
		// listing of the same bytes as already published and silently deny it
		// RSS exposure forever. For an enabled, supported tracker surface it on
		// the pass log line instead of silently shrinking the feed; unknown tail
		// trackers (enabled("") is false) and an intentionally disabled AB stay
		// silent.
		if p.w.enablement.enabled(scope) {
			p.js.unresolvable++
		}
		return journalItem{}, "", false
	}
	ids := publicationSignals(scope, t)
	if p.alreadyPublished(ids) {
		// Already served, so not new. Re-record every signal: a record that has
		// since gained a second identity form (an info hash SeaDex used to
		// redact) must be matched by either one next pass, and adding a signal
		// to an existing record is an APPEND like any other.
		p.record(ids)
		return journalItem{}, "", false
	}
	if !p.w.enablement.enabled(scope) {
		// BASELINING a disabled scope, not recording a refusal - see
		// growJournal's doc for why this write is the one that keeps
		// re-enabling a tracker from re-broadcasting its whole catalogue. The
		// README's off switch: nothing is persisted for the release itself.
		p.record(ids)
		return journalItem{}, "", false
	}
	it, ok = p.newJournalItem(key)
	if !ok {
		// The render failed for an upstream DATA reason on a tracker the
		// operator has ENABLED (no parseable title, an unpublishable page URL,
		// a field over the persisted limits). Record NOTHING: the log is never
		// pruned, so recording an identity that was never served would deny the
		// corrected record its RSS exposure forever.
		return journalItem{}, "", false
	}
	p.record(ids)
	return it, scope, true
}

// newJournalItem renders one newly curated torrent into its journal item,
// updating the skip counters when it cannot be served: an AnimeBytes release the
// app cannot hand an arr for want of a passkey counts toward the operator nudge
// (nothing is journaled and nothing published - see journalLink - so the nudge's
// implied recovery actually happens when the passkey arrives), and an in-scope
// torrent with no parseable title counts as unresolvable so an upstream data
// change surfaces on the pass log line instead of silently shrinking the feed.
// The tracker's enablement and the keyless case are decided and counted one
// level up, in journalIfNew, which is also where the publication decision lives
// - so this function only renders.
//
// It ASKS PERMISSION for the fold rather than folding whatever occurrences it
// happens to hold: renderPolicy names what this pass's evidence authorizes, and
// the partial arm has the unevaluated owners' stored votes carried onto the
// rendered item (carryUnevaluatedVotes).
func (p *journalPass) newJournalItem(key string) (journalItem, bool) {
	policy, refs := p.ev.renderPolicy(key)
	it, ok, noPasskey := p.w.renderJournalItem(key, refs, p.infoFor)
	if noPasskey {
		p.js.abSkippedNoPasskey++
	}
	if !ok {
		// Includes renderUnevaluated, which hands the render no occurrence at
		// all: there is nothing to serve, so the general failure path applies
		// and nothing is published.
		if !noPasskey {
			p.js.unresolvable++
		}
		return journalItem{}, false
	}
	if policy == renderPartial {
		p.carryUnevaluatedVotes(&it)
	}
	return it, true
}

// carryUnevaluatedVotes completes a partially-authorized render by carrying the
// votes of the owners this pass did NOT evaluate, read from the previous
// snapshot's ownership fact.
//
// renderJournalItem folds the marker and the category union across the
// occurrences the pass HOLDS (foldRefs). At window scope that is one subset of a
// shared torrent's owners: ~4.4% of curated torrents are attached to several
// AniList entries, each carrying its OWN isBest vote and its own movie/series
// typing, and a curator edit bumps only the entry it touched into the window.
// Folding the window alone therefore journals a release another entry votes best
// as ALT (dvfAlt, ~100 custom-format points below a best-marked sibling), and
// drops the Movies category of a film whose movie-typed owner stayed outside the
// window - so Radarr's cat=2000 RSS check never sees the item at all - for up to
// one reconcile interval, and the item's identity is recorded in the never-pruned
// publication log meanwhile.
//
// The persisted ownership fact is exactly the missing evidence, and it is the
// same set projectCuration ORs to answer a SEARCH for the same torrent: without
// this carry the two indexer surfaces built from ONE snapshot disagree about the
// same release's marker.
//
// It only ever ADDS a best marker or a category. Nothing here can drop an item
// or a label, so it cannot filter the feed (settled feed-rss-filtering).
func (p *journalPass) carryUnevaluatedVotes(it *journalItem) {
	if len(p.prior) == 0 {
		return
	}
	evaluated := p.evaluatedOwners()
	carried := false
	for owner, releases := range p.prior {
		if evaluated[owner] {
			// This pass replaced that owner's contribution wholesale, so its
			// fresh evidence is already in the render.
			continue
		}
		if p.carryOwnerVote(it, owner, releases) {
			carried = true
		}
	}
	if carried {
		// The same canonical order foldRefs leaves: the union appends in map
		// iteration order, which must not reach the persisted item.
		slices.Sort(it.Categories)
	}
}

// carryOwnerVote applies ONE unevaluated owner's stored votes to the item and
// reports whether that owner owns this release at all. Both votes are additive:
// best-wins on the marker and a category union, the same two folds foldRefs
// applies across the occurrences the pass does hold.
func (p *journalPass) carryOwnerVote(it *journalItem, owner string, releases []ownedRelease) bool {
	best, owns := priorOwnerVote(releases, it.Key, it.InfoHash)
	if !owns {
		return false
	}
	if best {
		it.DownloadVolumeFactor = dvfBest
	}
	alID, err := strconv.Atoi(owner)
	if err != nil {
		// An owner key that is not an AniList id cannot be typed, so its
		// category vote is unknowable; its best vote still counts.
		return true
	}
	for _, c := range categoriesFor(p.infoFor(alID).IsMovie) {
		if !slices.Contains(it.Categories, c) {
			it.Categories = append(it.Categories, c)
		}
	}
	return true
}

// evaluatedOwners returns the ownerKey set this pass evaluated, memoized for the
// pass. It is what separates a stored vote the pass has already replaced from one
// it never saw.
func (p *journalPass) evaluatedOwners() map[string]bool {
	if p.evaluated != nil {
		return p.evaluated
	}
	entries := p.ev.entries()
	p.evaluated = make(map[string]bool, len(entries))
	for i := range entries {
		p.evaluated[ownerKey(entries[i].AniListID)] = true
	}
	return p.evaluated
}

// priorOwnerVote reports whether one owner's stored contribution names this
// journal identity, and whether that owner votes it best. The identity test is
// the snapshot's own (ownedRelease): the journal key, or the item's canonical
// info hash.
func priorOwnerVote(releases []ownedRelease, key, hash string) (best, owns bool) {
	for _, r := range releases {
		if r.Key != key && (hash == "" || r.Hash != hash) {
			continue
		}
		owns = true
		if r.IsBest {
			best = true
		}
	}
	return best, owns
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
//
// The cache holds the title the app SERVES for a key, not the raw harvested
// claim, and that is what makes the audit's verbatim arm honest at window scope.
// The season correction is derived from a file census a pass may or may not hold:
// a tick's census covers only its 48h window while the journal holds fourteen
// days of items, and a de-curated key has no census entry at either scope. With
// the raw claim cached, every such pass re-applied the uncorrected whole-season
// title over the correction the previous pass persisted - the episode-suppression
// failure titleAudit.served exists to prevent - and re-persisted it. Writing the
// served title back means a pass with no evidence CARRIES what the app already
// serves instead of recomputing it from evidence it does not have. The harvest is
// what replaces the entry with a fresh tracker title.
func applyTitles(items []journalItem, titles map[string]string, audit titleAudit) {
	for i := range items {
		t, ok := titles[items[i].Key]
		if !ok || t == "" {
			continue
		}
		served := audit.served(items[i].Key, t)
		items[i].Title = served
		// Re-applying an already-corrected title is a no-op: packFromTitle reads
		// its SxxExx token as episode evidence, so served returns it verbatim and
		// reports nothing.
		titles[items[i].Key] = served
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
		// This pass holds no file evidence for the key, so it is not entitled to
		// judge the title: serve what the cache holds, which is the title the app
		// last SERVED for it (applyTitles writes the served value back). Carrying
		// it is why a window pass no longer undoes a correction a catalogue pass
		// made.
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
// every journal key the PASS EVALUATED, so applyTitles can tell what a harvested
// title claims from what the release actually ships. It is reached through
// curationEvidence.census, whose scope decides which keys are judged at all: a
// key the pass did not evaluate is absent here and its title is carried instead.
// A key's occurrences are the SAME tracker torrent attached to several entries,
// and the fold across them takes the STRONGEST evidence (pack over single over
// unknown) so the result cannot depend on catalogue order - the same
// order-independence renderJournalItem's sort exists to guarantee for the item
// itself.
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
// it (its publication record guarantees it can never return to need it).
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
