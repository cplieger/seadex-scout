package indexer

import (
	"slices"
	"strings"
	"time"

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

// --- Journal identity ---

// journalKey returns a torrent's journal identity - its tracker key
// (nyaa:{id} / ab:{id}), the same stable id the search curation set and the
// harvest matching key on - or "" when the torrent has no parseable tracker
// id (such a torrent cannot be journaled: no download link is buildable
// either).
func journalKey(t *seadex.Torrent) string { return trackerKey(t.Tracker, t.URL) }

// identitySignals returns every identity form a curated torrent is known
// under: its tracker key and its info hash. The seen ledger stores all of
// them, so novelty detection survives one signal going missing (a URL-shape
// change upstream, a hash appearing later).
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

// allIdentities collects every identity signal in the current curation set:
// the seen ledger a baseline records, so the journal only grows from curation
// genuinely newer than the baseline.
func allIdentities(entries []seadex.Entry) map[string]bool {
	seen := make(map[string]bool)
	for i := range entries {
		for j := range entries[i].Torrents {
			t := &entries[i].Torrents[j]
			if trackerScope(t.Tracker) == "" {
				// Tail-tracker occurrences never reach the ledger (the same
				// guard journalIfNew applies on the growth path): AnimeTosho
				// mirrors Nyaa with the IDENTICAL info hash, so folding it at
				// baseline would pre-mark a later Nyaa listing of the same
				// bytes as already seen and deny it RSS exposure forever.
				continue
			}
			for _, id := range identitySignals(t) {
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

// --- Journal item rendering ---

// renderJournalItem materializes the journal item for key from its current
// curated occurrences: synthesis from the lowest-AniList-ID occurrence, then
// best-wins on the marker and category union across all of them (a torrent
// attached to several entries must not render conflicting duplicates). ok is
// false when the torrent cannot be served: no grabbable download link (an
// AnimeBytes release without a passkey - reported via noPasskey so the caller
// can nudge the operator - or an id-less URL, which journalKey already
// excludes) or no parseable title at all (no files and no release group).
func (w *FeedWriter) renderJournalItem(key string, refs []curatedRef, infoFor func(alID int) EntryInfo) (it journalItem, ok, noPasskey bool) {
	if len(refs) == 0 {
		return journalItem{}, false, false
	}
	first := refs[0]
	// Deterministic synthesis source: a torrent attached to several entries
	// must render the same item regardless of catalogue order (marker and
	// categories are already order-independent folds below).
	for _, r := range refs[1:] {
		if r.entry.AniListID < first.entry.AniListID {
			first = r
		}
	}
	dl, resolved := downloadURL(first.torrent.Tracker, first.torrent.URL, w.abPasskey)
	if !resolved {
		return journalItem{}, false, scopeOfKey(key) == upstreamAB && w.abPasskey == ""
	}
	it = journalItem{
		item: item{
			Title:                synthesizeTitle(first.torrent, infoFor(first.entry.AniListID)),
			GUID:                 first.torrent.UsableURL(),
			InfoURL:              w.entryURL(first.entry.AniListID),
			DownloadURL:          dl,
			InfoHash:             validInfoHash(first.torrent.InfoHash),
			DownloadVolumeFactor: dvfAlt,
			Size:                 totalSize(first.torrent.Files),
		},
		Key:       key,
		AniListID: first.entry.AniListID,
	}
	if it.Title == "" {
		// No episode files and no release group: an arr cannot parse or
		// match a title-less item, so drop it (counted as unresolvable).
		return journalItem{}, false, false
	}
	foldRefs(&it, refs, infoFor)
	if !validPersistedItem(&it) {
		// An oversized external value (a SeaDex filename synthesized into
		// the title, an over-long URL) is unservable: renderFeed's XML
		// escaping could amplify it well past the container memory budget
		// (see maxPersistedFieldBytes). Dropped as unresolvable - the caller
		// has already folded the identity into the seen ledger, so the item
		// never re-enters the journal as new.
		return journalItem{}, false, false
	}
	return it, true, false
}

// foldRefs applies the order-independent folds across all of a torrent's
// curated occurrences: best-wins on the download-volume-factor marker and
// category union (a torrent attached to several entries must not render
// conflicting duplicates).
func foldRefs(it *journalItem, refs []curatedRef, infoFor func(alID int) EntryInfo) {
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

// recordDrop accounts for a journal item that failed to render: an
// AB item skipped for a missing passkey is tallied separately from a
// genuine drop.
func (js *journalStats) recordDrop(noPasskey bool) {
	if noPasskey {
		js.abSkippedNoPasskey++
		return
	}
	js.dropped++
}

// --- Carrying the previous journal ---

// carryItem re-renders or prunes one carried journal item, updating js, and
// reports whether it survives into the rebuilt journal. ws is the
// curation-warned exclusion set splitCurationWarned built; a carried item it
// retracts (its key is excluded, or its stored info hash is warned under a
// DIFFERENT tracker key) is dropped (RSS must never keep serving bytes search
// suppresses).
func (w *FeedWriter) carryItem(it *journalItem, cur map[string][]curatedRef, ws *warnedSet, infoFor func(alID int) EntryInfo, now time.Time, js *journalStats) (journalItem, bool) {
	if it.Key == "" || it.FirstSeen.IsZero() {
		js.dropped++
		return journalItem{}, false
	}
	if it.FirstSeen.After(now) {
		// A FirstSeen ahead of the wall clock (a clock rollback, or a
		// snapshot restored from a future-skewed host) would make the
		// max-age check below see a negative age and keep the item past the
		// bounded journal window. Rebase it to now - preserving the item
		// across the clock correction while bounding its remaining lifetime
		// to feedJournalMaxAge - and count the rebase for the snapshot log
		// line.
		it.FirstSeen, it.PubDate = now, now
		js.rebased++
	}
	if now.Sub(it.FirstSeen) > feedJournalMaxAge {
		js.pruned++
		return journalItem{}, false
	}
	if ws.retracts(it) {
		js.warned++
		return journalItem{}, false
	}
	refs, curated := cur[it.Key]
	if !curated {
		if scopeOfKey(it.Key) == upstreamAB && w.abPasskey == "" {
			js.recordDrop(true)
			return journalItem{}, false
		}
		// Same GUID-identity gate as the curated arm below: a stored GUID
		// that no longer proves this item's journal identity (a cross-key,
		// foreign-host, or empty GUID from a hand-edited snapshot) must not
		// be carried - unlike a curated item there is no fresh render to
		// self-heal from, and reload derives the SERVED download link from
		// the GUID, so a cross-key GUID would plant a fetch target for a
		// different torrent id on the same tracker for the item's whole
		// journal window.
		if trackerKeyFromURL(it.GUID) != it.Key {
			js.dropped++
			return journalItem{}, false
		}
		return *it, true
	}
	fresh, ok, noPasskey := w.renderJournalItem(it.Key, refs, infoFor)
	if !ok {
		js.recordDrop(noPasskey)
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
	if it.GUID != "" && trackerKeyFromURL(it.GUID) == it.Key {
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
// is no longer grabbable, so serving it would be dead weight) are dropped.
func (w *FeedWriter) carryJournal(prevFeed []journalItem, cur map[string][]curatedRef, ws *warnedSet, infoFor func(alID int) EntryInfo, now time.Time, js *journalStats) []journalItem {
	kept := make([]journalItem, 0, len(prevFeed))
	for i := range prevFeed {
		if it, ok := w.carryItem(&prevFeed[i], cur, ws, infoFor, now, js); ok {
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
// unconfigured tracker, an unresolvable id), so the journal only ever grows
// from curation that is new AT THE TIME it is served; backfill is search's
// job.
func (w *FeedWriter) growJournal(entries []seadex.Entry, cur map[string][]curatedRef, seen map[string]bool, infoFor func(alID int) EntryInfo, now time.Time, js *journalStats) (nyaa, ab []journalItem) {
	for i := range entries {
		for j := range entries[i].Torrents {
			it, scope, ok := w.journalIfNew(&entries[i].Torrents[j], cur, seen, infoFor, js)
			if !ok {
				continue
			}
			it.FirstSeen, it.PubDate = now, now
			js.added++
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
// when it is genuinely new and servable. Tail-tracker occurrences never reach
// the ledger: see the guard below.
func (w *FeedWriter) journalIfNew(t *seadex.Torrent, cur map[string][]curatedRef, seen map[string]bool, infoFor func(alID int) EntryInfo, js *journalStats) (it journalItem, scope string, ok bool) {
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
	ids := identitySignals(t)
	if len(ids) == 0 {
		// No stable identity at all: the torrent can neither be journaled nor
		// remembered. For an enabled, supported tracker (Nyaa, or a configured
		// AnimeBytes - whose hashes SeaDex redacts, so an upstream AB
		// URL-shape change lands exactly here) this is the same
		// unresolvable-diagnostic case newJournalItem counts: surface it on
		// the snapshot log line instead of silently shrinking the feed.
		// Unknown tail trackers and an intentionally disabled AB stay silent.
		if w.scopeConfigured(scope) {
			js.unresolvable++
		}
		return journalItem{}, "", false
	}
	isNew := true
	for _, id := range ids {
		if seen[id] {
			isNew = false
		}
		seen[id] = true
	}
	if !isNew {
		return journalItem{}, "", false
	}
	return w.newJournalItem(t, scope, cur, infoFor, js)
}

// newJournalItem resolves one newly curated torrent into its journal item and
// scope, updating the skip counters when it cannot be served: a non-Nyaa/AB
// tracker (the negligible SeaDex tail) is silently ignored, an unconfigured
// tracker (Nyaa or AnimeBytes without its Torznab URL) is skipped without
// persisting anything for it (the README's off switch; its identity is
// already in seen, so enabling it later starts from current novelty instead
// of backfilling disabled-era curation), a missing AB passkey
// counts toward the operator nudge, and an in-scope torrent with no journal
// key or no parseable title counts as unresolvable so an upstream URL-shape
// change surfaces on the snapshot log line instead of silently shrinking the
// feed (unresolvable is counted only for configured scopes).
func (w *FeedWriter) newJournalItem(t *seadex.Torrent, scope string, cur map[string][]curatedRef, infoFor func(alID int) EntryInfo, js *journalStats) (journalItem, string, bool) {
	if !w.scopeConfigured(scope) {
		return journalItem{}, "", false
	}
	key := journalKey(t)
	if key == "" {
		js.unresolvable++
		return journalItem{}, "", false
	}
	it, ok, noPasskey := w.renderJournalItem(key, cur[key], infoFor)
	if noPasskey {
		js.abSkippedNoPasskey++
	}
	if !ok {
		if !noPasskey {
			js.unresolvable++
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
