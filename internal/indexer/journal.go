package indexer

import (
	"slices"
	"strings"
	"time"

	"github.com/cplieger/seadex-scout/internal/seadex"
)

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
			for _, id := range identitySignals(&entries[i].Torrents[j]) {
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

// renderJournalItem materializes the journal item for key from its current
// curated occurrences: synthesis from the first occurrence, then best-wins on
// the marker and category union across all of them (a torrent attached to
// several entries must not render conflicting duplicates). ok is false when
// the torrent cannot be served: no grabbable download link (an AnimeBytes
// release without a passkey - reported via noPasskey so the caller can nudge
// the operator - or an id-less URL, which journalKey already excludes) or no
// parseable title at all (no files and no release group).
func renderJournalItem(key string, refs []curatedRef, infoFor func(alID int) EntryInfo, abPasskey string) (it item, ok, noPasskey bool) {
	if len(refs) == 0 {
		return item{}, false, false
	}
	first := refs[0]
	dl, resolved := downloadURL(first.torrent.Tracker, first.torrent.URL, abPasskey)
	if !resolved {
		return item{}, false, scopeOfKey(key) == upstreamAB && abPasskey == ""
	}
	it = item{
		Title:                synthesizeTitle(first.torrent, infoFor(first.entry.AniListID)),
		GUID:                 first.torrent.UsableURL(),
		InfoURL:              entryURL(first.entry.AniListID),
		DownloadURL:          dl,
		InfoHash:             validInfoHash(first.torrent.InfoHash),
		DownloadVolumeFactor: dvfAlt,
		Size:                 totalSize(first.torrent.Files),
		Key:                  key,
		AniListID:            first.entry.AniListID,
	}
	if it.Title == "" {
		// No episode files and no release group: an arr cannot parse or
		// match a title-less item, so drop it (counted as unresolvable).
		return item{}, false, false
	}
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
	return it, true, false
}

// journalStats counts one rebuild's journal transitions for the snapshot log
// line.
type journalStats struct {
	added              int
	pruned             int
	dropped            int
	unresolvable       int
	abSkippedNoPasskey int
}

// carryJournal re-renders one scope's previous journal items against the
// current catalogue and prunes aged-out ones. A carried item whose torrent is
// still curated is re-synthesized from current data (its title, size, marker,
// and categories refresh; the harvested-title cache is applied by the caller
// after the harvest) with its FirstSeen preserved; one whose torrent left the
// curation set keeps its stored render (a curated-then-replaced torrent is
// still a valid release). An item older than feedJournalMaxAge leaves the
// journal (its cached title is dropped by the caller's retainTitles); a
// pre-journal item with no Key or FirstSeen (unreachable after a baseline,
// defensive against hand-edited snapshots) and a carried AnimeBytes item whose
// download link can no longer be built (the passkey was removed - a stale
// credential must never be re-persisted) are dropped.
func (w *FeedWriter) carryJournal(prevFeed []item, cur map[string][]curatedRef, infoFor func(alID int) EntryInfo, now time.Time, js *journalStats) []item {
	kept := make([]item, 0, len(prevFeed))
	for i := range prevFeed {
		it := prevFeed[i]
		if it.Key == "" || it.FirstSeen.IsZero() {
			js.dropped++
			continue
		}
		if now.Sub(it.FirstSeen) > feedJournalMaxAge {
			js.pruned++
			continue
		}
		refs, curated := cur[it.Key]
		if !curated {
			kept = append(kept, it)
			continue
		}
		fresh, ok, _ := renderJournalItem(it.Key, refs, infoFor, w.abPasskey)
		if !ok {
			js.dropped++
			continue
		}
		fresh.FirstSeen = it.FirstSeen
		fresh.PubDate = it.FirstSeen
		kept = append(kept, fresh)
	}
	return kept
}

// growJournal adds the newly curated torrents to the per-scope journals and
// folds every current identity into the seen ledger. A torrent is NEW only
// when none of its identity signals is in seen - the tracker post date is
// deliberately not the novelty key, since SeaDex routinely adds old torrents.
// Every identity signal is recorded in seen whether or not the torrent could
// be journaled (an AnimeBytes release skipped for a missing passkey, an
// unconfigured tracker, an unresolvable id), so the journal only ever grows
// from curation that is new AT THE TIME it is served; backfill is search's
// job.
func (w *FeedWriter) growJournal(entries []seadex.Entry, cur map[string][]curatedRef, seen map[string]bool, infoFor func(alID int) EntryInfo, now time.Time, js *journalStats) (nyaa, ab []item) {
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

// journalIfNew applies growJournal's novelty test to one torrent - folding its
// identity signals into seen either way - and materializes its journal item
// when it is genuinely new and servable.
func (w *FeedWriter) journalIfNew(t *seadex.Torrent, cur map[string][]curatedRef, seen map[string]bool, infoFor func(alID int) EntryInfo, js *journalStats) (it item, scope string, ok bool) {
	ids := identitySignals(t)
	if len(ids) == 0 {
		return item{}, "", false // no stable identity: cannot journal or remember it
	}
	isNew := true
	for _, id := range ids {
		if seen[id] {
			isNew = false
		}
	}
	for _, id := range ids {
		seen[id] = true
	}
	if !isNew {
		return item{}, "", false
	}
	return w.newJournalItem(t, cur, infoFor, js)
}

// newJournalItem resolves one newly curated torrent into its journal item and
// scope, updating the skip counters when it cannot be served: a non-Nyaa/AB
// tracker (the negligible SeaDex tail) is silently ignored, an unconfigured
// AnimeBytes tracker is skipped without persisting anything for it (the
// README's off switch; its identity is already in seen), a missing AB passkey
// counts toward the operator nudge, and an in-scope torrent with no journal
// key or no parseable title counts as unresolvable so an upstream URL-shape
// change surfaces on the snapshot log line instead of silently shrinking the
// feed.
func (w *FeedWriter) newJournalItem(t *seadex.Torrent, cur map[string][]curatedRef, infoFor func(alID int) EntryInfo, js *journalStats) (it item, scope string, ok bool) {
	scope = trackerScope(t.Tracker)
	if scope == "" {
		return item{}, "", false
	}
	key := journalKey(t)
	if key == "" {
		js.unresolvable++
		return item{}, "", false
	}
	if scope == upstreamAB && !w.abConfigured {
		return item{}, "", false
	}
	it, ok, noPasskey := renderJournalItem(key, cur[key], infoFor, w.abPasskey)
	if noPasskey {
		js.abSkippedNoPasskey++
	}
	if !ok {
		if !noPasskey {
			js.unresolvable++
		}
		return item{}, "", false
	}
	return it, scope, true
}

// applyTitles upgrades each journal item's served title to its harvested real
// title when the cache holds one; items without a cached title keep their
// synthesized title (the permanent fallback). GUIDs never change with the
// title, so an upgrade cannot re-trigger a grab.
func applyTitles(items []item, titles map[string]string) {
	for i := range items {
		if t, ok := titles[items[i].Key]; ok && t != "" {
			items[i].Title = t
		}
	}
}

// retainTitles prunes the harvested-title cache to the keys still present in
// the journal feeds, so a pruned or capped-out item's cached title leaves with
// it (its seen-ledger identity guarantees it can never return to need it).
func retainTitles(titles map[string]string, feeds ...[]item) map[string]string {
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
