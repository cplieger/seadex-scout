package indexer

import (
	"context"

	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/tagfilter"
)

// Advance folds a bounded WINDOW of recently-changed SeaDex entries into the
// persisted feed, without re-deriving anything the window cannot speak for.
//
// It exists because Rebuild cannot take a window. Rebuild is written for a
// COMPLETE entry set and three of the things it does are only correct against
// one:
//
//   - it REPLACES the search curation index from its argument (buildCuration),
//     so a window would shrink the index from the whole catalogue to the
//     window's handful and take search down until the next full pass;
//   - it can take the BASELINE path over a missing or malformed snapshot,
//     which records "everything currently curated" in the never-pruned seen
//     ledger - from a window that would burn ~8700 identities the app has not
//     actually served, and they can never be re-admitted;
//   - collectWarnedIdentities builds its identity graph in one catalogue pass,
//     propagating an exclusion through shared info hashes, so a window computes
//     a NARROWER graph and would fail to retract bytes warned elsewhere.
//
// Every one of those lives in a branch a window has no business entering, so
// Advance enters none of them. What it does instead is the minimum that is
// sound from partial evidence: admit what is genuinely new, place it correctly,
// expire what is old, and carry everything else through untouched.
//
// It is deliberately NOT the whole of Rebuild's behaviour. A carried item is not
// re-rendered (so a title, size or marker corrected upstream keeps its stored
// form until the next full pass), an item whose curation or tags changed in
// place is not reconsidered, and a de-curated item is not dropped. A NEW item's
// marker and categories are folded over the window's refs only, so a torrent
// shared with an entry that was NOT bumped into this window renders without that
// entry's vote - the weaker freeleech marker, or a category set missing the arr
// the other parent routes to (4.4% of torrents are shared, and a mis-routed
// category is invisible to that arr's RSS until the reconcile re-folds it). All
// of those are the full pass's job, and the full pass runs daily.
func (w *FeedWriter) Advance(ctx context.Context, window []seadex.Entry, info EntryInfoFunc) error {
	infoFor := entryInfoFunc(info)
	snap, prev, err := w.loadPrevious(ctx)
	if err != nil {
		return err
	}
	if prev.baseline {
		// Recovery is the full pass's job. Baselining from a window would
		// record the window's identities as "seen" - permanently, since the
		// ledger is never pruned - and silently discard the journal.
		w.log.Warn("indexer feed snapshot unusable; deferring to the next full rebuild",
			"reason", prev.reason)
		return nil
	}

	// The operator's tag exclusions apply to GROWTH here, exactly as they gate
	// growth in Rebuild. Skipping them would journal a release the operator
	// excluded - servable for up to a full reconcile interval - and burn its
	// identity into the ledger, so a later un-exclusion could never restore it.
	// The identity closure is computed over the WINDOW, which is narrower than
	// Rebuild's catalogue-wide graph but not empty: two occurrences of the same
	// release sharing an info hash, one of them warned, routinely sit inside a
	// single window entry, and admitting the unwarned one would burn exactly the
	// identity Rebuild refuses to record. The cross-catalogue half - an
	// exclusion reachable only through an entry outside the window - still
	// belongs to the full pass.
	kept := filterWindowByTags(window, w.tags)

	var js journalStats
	pass := &journalPass{
		w: w, cur: indexCurated(kept), seen: prev.seen,
		ws: &warnedSet{}, infoFor: infoFor, js: &js, now: w.now(),
	}
	nyaa, ab := pass.growJournal(kept)

	// Carry, expire and re-place. prepareCarriedItem is what applies the
	// journal window and the future-clock rebase; it is reached here through
	// expireCarried rather than through carryItem, because carryItem's other
	// arms (the curated/uncurated split, the warned retraction) all need the
	// full curation set to be meaningful.
	nyaaFeed := append(expireCarried(pass, snap.NyaaFeed, upstreamNyaa), nyaa...)
	abFeed := append(expireCarried(pass, snap.ABFeed, upstreamAB), ab...)

	// sortFeed is NOT optional. The reader serves the persisted order with no
	// sort of its own, and an arr walking an RSS feed stops at the first item
	// older than its last sync - so a new item appended at the tail is
	// unreachable, which would break the very freshness this path exists to
	// deliver.
	snap.NyaaFeed, snap.ABFeed = sortFeed(nyaaFeed), sortFeed(abFeed)
	snap.Seen = pass.seen

	if !seenLedgerWithinLimits(snap.Seen) || len(snap.Seen) > maxSnapshotMapEntries {
		// Growth is the only way a loaded ledger can cross either cap (the
		// decode refuses an over-cap one before loadPrevious returns), and the
		// remedy is a rebuild from the whole catalogue - which is the full pass,
		// not this one. Persisting an over-cap ledger would have the reader
		// refuse the snapshot and serve nothing. Both caps are checked because
		// neither implies the other: a short tracker-key entry serializes in
		// ~20 bytes, so the byte budget admits ~419k entries against the
		// decode's 250k cardinality cap.
		w.log.Warn("indexer seen ledger crossed its cap while advancing; deferring to the next full rebuild",
			"entries", len(snap.Seen), "max_entries", maxSnapshotMapEntries)
		return nil
	}

	if err := w.persist(ctx, &snap); err != nil {
		return err
	}
	w.log.Info("indexer feed advanced",
		"window_entries", len(window), "curated_entries", len(kept),
		"nyaa_feed", len(snap.NyaaFeed), "ab_feed", len(snap.ABFeed),
		"journal_new", js.added, "journal_pruned", js.pruned,
		"journal_dropped", js.dropped, "skipped_unresolvable", js.unresolvable,
		"seen", len(snap.Seen))
	return nil
}

// filterWindowByTags drops the torrents the operator's tag policy excludes from
// the feed surface, entry by entry. It is the window-scoped half of
// splitCurationWarned: the same per-torrent decision AND the same identity
// closure, computed over the window rather than the catalogue.
//
// The closure matters even at window scope. Warning identity is transitive
// across occurrences, and SeaDex routinely lists one release on two trackers
// with a shared info hash and the `broken` tag on one occurrence only. Filtering
// each torrent in isolation would admit the untagged twin, journal it, and record
// its identity in the never-pruned seen ledger - after which the next full pass
// retracts it from the feed but can never re-admit it, which is the permanent
// omission the feed's non-filtering stance exists to avoid. What stays out of
// reach here is only the exclusion reachable through an entry the window did not
// carry; that one is the full pass's.
func filterWindowByTags(window []seadex.Entry, tags tagfilter.Filter) []seadex.Entry {
	_, warnedIDs := collectWarnedIdentities(window, tags)
	out := make([]seadex.Entry, 0, len(window))
	for i := range window {
		e := window[i]
		ts, _ := filterWarnedTorrents(e.Torrents, warnedIDs, tags)
		if len(ts) == 0 {
			continue
		}
		e.Torrents = ts
		out = append(out, e)
	}
	return out
}

// expireCarried applies the journal window to already-journaled items without
// re-rendering them: a scope mismatch or a missing identity is dropped, a
// future FirstSeen is rebased, an item past feedJournalMaxAge leaves. Nothing
// here consults the curation set, which is what makes it sound from a window.
func expireCarried(p *journalPass, feed []journalItem, scope string) []journalItem {
	kept := make([]journalItem, 0, len(feed))
	for i := range feed {
		it := feed[i]
		if scopeOfKey(it.Key) != scope {
			p.js.dropped++
			continue
		}
		if !p.prepareCarriedItem(&it) {
			continue
		}
		kept = append(kept, it)
	}
	return kept
}
