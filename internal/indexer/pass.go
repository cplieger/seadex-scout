package indexer

import (
	"context"

	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/tagfilter"
)

// This file is THE pass. The reconcile and the tick are ONE code path
// parameterized by SCOPE, which is the whole payoff of the rule table in
// members.go: they used to be two code paths, and that is WHY the tick got five
// persisted members wrong by omission - it was a different path rather than the
// same path with a smaller input.
//
// The collapse is honest but PARTIAL, and the remainder is named rather than
// hidden. Three computations are genuinely catalogue-scoped and stay
// reconcile-only, each because its criterion is absence from the input:
//
//   - the catalogue-wide warned-identity graph (collectWarnedIdentities closes
//     over shared info hashes, so a window computes a NARROWER graph),
//   - the Prowlarr title harvest (rate-paced, rotating over the whole journal),
//   - retainTitles (pruning the harvest cache to the keys still journaled).
//
// Everything else runs at both scopes under the same member rules.

// carryPolicy names which arm carryItem may take for one carried journal item.
// It is a property of the pass's EVIDENCE, not a choice at the call site, and it
// is where the difference between the reconcile's carry and the tick's carry
// lives now that there is one carry function.
type carryPolicy int

const (
	// carryVerbatim: this pass holds no evidence about the key. Keep the stored
	// item exactly as persisted and let the catalogue pass decide.
	carryVerbatim carryPolicy = iota + 1
	// carryRefreshed: the pass evaluated the key and holds EVERY occurrence of
	// it, so a fresh render is sound.
	carryRefreshed
	// carryDeCurated: the pass holds positive evidence the key has left SeaDex.
	carryDeCurated
)

// curationEvidence is what ONE pass HOLDS about curation. It is an interface
// with exactly two implementations because the difference between them is not a
// parameter to be read at a call site - it is which QUESTIONS the pass is
// entitled to answer. A window physically cannot hold a catalogue-wide warned
// set, so it cannot be handed one by mistake, and its carryPolicy cannot reach
// the de-curated arm at all.
type curationEvidence interface {
	// scope names the input this evidence was built from.
	scope() passScope
	// entries returns the evaluated entries, tag-filtered.
	entries() []seadex.Entry
	// carryPolicy names which carry arm this pass's evidence AUTHORIZES for a
	// carried journal key, plus the occurrences the refresh arm needs. A window
	// has exactly one arm and cannot reach the other two.
	carryPolicy(key string) (carryPolicy, []curatedRef)
	// index is the whole evaluated key set, for the passes that must walk it
	// (the file census). It is scope-appropriate by construction: a window's
	// index holds only the window's keys.
	index() map[string][]curatedRef
	// refs returns the curated occurrences of a journal key, and whether the
	// pass evaluated that key at all.
	refs(key string) ([]curatedRef, bool)
	// retracts reports whether a carried item shares a warned identity. This is
	// sound at BOTH scopes because retraction acts on positive evidence (a
	// torrent the pass evaluated carries an excluded tag), never on absence.
	retracts(it *journalItem) bool
	// ownership is the per-entry curation contribution this pass may write, or
	// nil when the pass cannot vouch for its own evidence (see
	// windowEvidence.ownership).
	ownership() map[string][]ownedRelease
	// warnedKeys is how many journal keys the tag policy excluded, for the log
	// line.
	warnedKeys() int
}

// catalogueEvidence is the reconcile's evidence: the whole SeaDex catalogue,
// with the catalogue-wide warned-identity closure.
type catalogueEvidence struct {
	cur    map[string][]curatedRef
	warned warnedSet
	kept   []seadex.Entry
}

// windowEvidence is the tick's evidence: the recently-changed entries only, and
// a warned closure computed over just those. It deliberately holds no
// catalogue-wide anything - there is no field it could be put in.
type windowEvidence struct {
	cur    map[string][]curatedRef
	warned warnedSet
	kept   []seadex.Entry
	// tagPolicySet records whether the operator has ANY tag exclusion
	// configured. It is what decides whether this window's warned closure is
	// complete enough to admit keys into the search index - see ownership.
	tagPolicySet bool
}

// newEvidence builds the evidence for one pass over entries at scope. The tag
// filtering and the warned closure are the SAME computation at both scopes
// (splitCurationWarned over whatever the input is); what differs is what the
// result is allowed to conclude.
//
// The closure matters even at window scope, and this is why the window computes
// one at all rather than running unfiltered: warning identity is transitive
// across occurrences, and SeaDex routinely lists one release on two trackers
// with a shared info hash and the `broken` tag on one occurrence only.
// Filtering each torrent in isolation would admit the untagged twin, journal it,
// and record its identity in the never-pruned publication log - after which the
// next catalogue pass retracts it from the feed but can never re-admit it. What
// stays out of reach here is only the exclusion reachable through an entry the
// window did not carry; that one is the catalogue pass's.
func newEvidence(entries []seadex.Entry, tags tagfilter.Filter, scope passScope) curationEvidence {
	kept, warned := splitCurationWarned(entries, tags)
	cur := indexCurated(kept)
	if scope == scopeCatalogue {
		return &catalogueEvidence{kept: kept, cur: cur, warned: warned}
	}
	return &windowEvidence{kept: kept, cur: cur, warned: warned, tagPolicySet: tags.Len() > 0}
}

func (e *catalogueEvidence) scope() passScope        { return scopeCatalogue }
func (e *catalogueEvidence) entries() []seadex.Entry { return e.kept }
func (e *catalogueEvidence) warnedKeys() int         { return len(e.warned.keys) }

func (e *catalogueEvidence) refs(key string) ([]curatedRef, bool) {
	refs, ok := e.cur[key]
	return refs, ok
}

func (e *catalogueEvidence) index() map[string][]curatedRef { return e.cur }

// carryPolicy: a catalogue pass holds every occurrence of every key, so it may
// re-render what it evaluated - and, because its input IS the catalogue, a key
// absent from it is genuinely absent from SeaDex, so it may conclude de-curation.
func (e *catalogueEvidence) carryPolicy(key string) (carryPolicy, []curatedRef) {
	if refs, evaluated := e.cur[key]; evaluated {
		return carryRefreshed, refs
	}
	return carryDeCurated, nil
}

func (e *catalogueEvidence) retracts(it *journalItem) bool { return e.warned.retracts(it) }

// ownership: the catalogue pass vouches for everything, because its warned
// closure is complete.
func (e *catalogueEvidence) ownership() map[string][]ownedRelease {
	return ownershipOf(e.kept)
}

func (e *windowEvidence) scope() passScope        { return scopeWindow }
func (e *windowEvidence) entries() []seadex.Entry { return e.kept }
func (e *windowEvidence) warnedKeys() int         { return len(e.warned.keys) }

func (e *windowEvidence) refs(key string) ([]curatedRef, bool) {
	refs, ok := e.cur[key]
	return refs, ok
}

func (e *windowEvidence) index() map[string][]curatedRef { return e.cur }

// carryPolicy is ALWAYS carryVerbatim for a window, and this is the second most
// important method on this type.
//
// It cannot conclude de-curation: an empty window is legitimate, so absence from
// it proves nothing and the item may not be treated as having left SeaDex. But it
// also
// may not RE-RENDER an item it did evaluate, which is less obvious and just as
// binding: renderJournalItem folds the download-volume-factor marker and the
// category union across every occurrence of the key (foldRefs), so a fold over
// the window's occurrences alone would silently drop the vote of a parent entry
// that was not bumped into this window - downgrading a best marker to alt, or
// dropping the category that routes the release to the other arr. 4.4% of
// torrents are attached to several entries, so that is a real population. A
// re-render is therefore acting on absence from the pass's own input, exactly
// what the law forbids, and it stays the catalogue pass's job.
//
// The ownership fact is windowable for precisely the reason the render is not:
// it stores each owner's vote SEPARATELY, so replacing one owner's contribution
// leaves the others' intact and the projection's OR recomputes exactly.
func (e *windowEvidence) carryPolicy(string) (carryPolicy, []curatedRef) {
	return carryVerbatim, nil
}

func (e *windowEvidence) retracts(it *journalItem) bool { return e.warned.retracts(it) }

// ownership admits this window's entries into the search index only when the
// window's own warned closure is COMPLETE, and it is complete exactly when the
// operator has configured no tag exclusions at all: with an empty policy
// nothing is warned anywhere in the catalogue, so there is no
// reachable-only-from-outside exclusion for the window to miss.
//
// This is the one genuine regression risk in windowing the search index, and it
// is closed here rather than mitigated. The catalogue pass filters
// catalogue-wide (splitCurationWarned over every entry, closing over shared
// info hashes) BEFORE building the index; a window closes only over itself, so
// admitting its keys on window evidence alone would mark a warned identity
// curated for up to one reconcile interval - a release the operator explicitly
// excluded, offered to the arrs. With a tag policy configured the tick
// therefore writes no ownership at all and the reconcile remains the only
// writer of it, which is exactly today's behaviour; with the default empty
// policy (the overwhelmingly common case, and the one h-f8 is about) the tick
// admits freely.
func (e *windowEvidence) ownership() map[string][]ownedRelease {
	if e.tagPolicySet {
		return nil
	}
	return ownershipOf(e.kept)
}

// ownershipOf reads the PRESENT fact off the entries a pass evaluated: for each
// entry, the set of releases it contributes, with that entry's OWN isBest vote.
// Two entries listing the same torrent produce two owner records, which is what
// makes the cross-owner isBest fold recomputable (projectCuration ORs them) and
// a demotion representable.
//
// Records sharing an AniList id are UNIONED rather than overwriting each other:
// the contribution of entry X is everything the pass evaluated for X, and
// SeaDex can return two catalogue records carrying the same alID.
func ownershipOf(entries []seadex.Entry) map[string][]ownedRelease {
	out := make(map[string][]ownedRelease, len(entries))
	for i := range entries {
		id := ownerKey(entries[i].AniListID)
		torrents := entries[i].Torrents
		for j := range torrents {
			t := &torrents[j]
			r := ownedRelease{
				Key:    trackerKey(t.Tracker, t.URL),
				Hash:   validInfoHash(t.InfoHash),
				IsBest: t.IsBest,
			}
			if r.Key == "" && r.Hash == "" {
				// Nothing a search can match on, so nothing to own.
				continue
			}
			out[id] = append(out[id], r)
		}
		if _, present := out[id]; !present {
			// An entry evaluated down to nothing still has to APPEAR in the
			// evaluated set, so upsertOwners can clear a stored contribution
			// that is no longer curated. A nil slice is that statement.
			out[id] = nil
		}
	}
	return out
}

// --- The pass ---

// run is the one pass, at one scope. Every difference between the reconcile and
// the tick is either the scope value or one of the three named catalogue-only
// steps; there is no third behaviour.
func (w *FeedWriter) run(ctx context.Context, entries []seadex.Entry, info EntryInfoFunc, scope passScope) error {
	infoFor := entryInfoFunc(info)
	prev, err := w.loadPrevious(ctx)
	if err != nil {
		return err
	}
	if prev.baseline && scope == scopeWindow {
		// Recovery is the catalogue pass's job. Baselining from a window would
		// record the window's identities as published - permanently, since the
		// log is never pruned - and silently discard the journal.
		w.log.Warn("indexer feed snapshot unusable; deferring to the next full rebuild",
			"reason", prev.reason)
		return nil
	}

	ev := newEvidence(entries, w.tags, scope)
	now := w.now()
	var js journalStats
	writes := passWrites{
		scope:     scope,
		evaluated: ev.ownership(),
		titles:    prev.titles,
		cursor:    prev.cursor,
		published: map[string]bool{},
	}

	if prev.baseline {
		// Only reachable at catalogue scope (the window returned above). The
		// whole current curation set is FORFEITED into the publication log and
		// the journal starts empty, growing only from genuinely new curation:
		// backfill is search's job.
		writes.published = baselinePublications(ev.entries())
		writes.titles = map[string]string{}
		w.log.Info("indexer feed journal baselined; RSS feed starts empty and grows from newly curated releases",
			"reason", prev.reason, "published", len(writes.published))
	} else {
		pass := &journalPass{
			w: w, ev: ev, published: prev.published, publish: writes.published,
			infoFor: infoFor, js: &js, now: now,
		}
		// The file-census pack verdict per journal key, read from the same
		// evidence the items render from, so applyTitles can tell when a
		// harvested title contradicts the payload it names (titleAudit). A key
		// this pass did not evaluate has no census entry and is served
		// verbatim, which is what makes the audit sound at window scope.
		census := censusPacks(ev.index())
		// Carry BOTH journals regardless of configuration: a tracker's off
		// switch must be reversible. Blanking a Torznab URL used to skip the
		// carry, so a single rebuild dropped every journaled item for that
		// scope - while the never-pruned publication log kept their identities,
		// so the novelty test reported isNew=false forever and those releases
		// could never reach RSS again (l-f161). Carrying costs nothing at rest
		// (both feeds are stored GUID-only, see stripDownloadURLs) and nothing
		// on the wire (feedFor serves an unconfigured scope nothing).
		//
		// Carried items keep AGING OUT on the normal feedJournalMaxAge window
		// rather than freezing (prepareCarriedItem prunes them), and the
		// age-out is sound at ANY scope because its criterion is the item's own
		// FirstSeen rather than membership of this pass's input.
		carriedNyaa := pass.carryJournal(prev.nyaaFeed, upstreamNyaa)
		carriedAB := pass.carryJournal(prev.abFeed, upstreamAB)
		// Growth stays gated per scope: newJournalItem returns early for an
		// unconfigured tracker, so an off tracker's journal shrinks but never
		// grows.
		newNyaa, newAB := pass.growJournal(ev.entries())
		carriedNyaa = append(carriedNyaa, newNyaa...)
		carriedAB = append(carriedAB, newAB...)
		writes.nyaa, writes.ab = carriedNyaa, carriedAB

		if scope == scopeCatalogue {
			// CATALOGUE-ONLY STEP: the Prowlarr title harvest. It is rate-paced
			// and rotates over the whole journal, so a window cannot own its
			// cursor without starving the rotation.
			feeds := map[string][]journalItem{upstreamNyaa: writes.nyaa, upstreamAB: writes.ab}
			hs, cursor := w.harvest.harvestTitles(ctx, feeds, writes.titles, infoFor, prev.cursor)
			writes.cursor = cursor
			js.harvest = hs
		}
		// ONE audit value across both feeds, so its onset latch is per PASS.
		audit := titleAudit{census: census, report: w.packDisagreementReporter()}
		applyTitles(writes.nyaa, writes.titles, audit)
		applyTitles(writes.ab, writes.titles, audit)
	}

	// sortFeed is NOT optional at either scope. The reader serves the persisted
	// order with no sort of its own, and an arr walking an RSS feed stops at the
	// first item older than its last sync - so a new item appended at the tail
	// is unreachable.
	writes.nyaa, writes.ab = sortFeed(writes.nyaa), sortFeed(writes.ab)
	if scope == scopeCatalogue {
		// CATALOGUE-ONLY STEP: retainTitles. It prunes the harvest cache to the
		// keys still journaled, which reads the whole journal against the whole
		// catalogue - so a window would drop the titles of every key it did not
		// evaluate.
		writes.titles = retainTitles(writes.titles, writes.nyaa, writes.ab)
	}

	snap, err := buildSnapshot(&prev, &writes)
	if err != nil {
		return err
	}
	if !w.publicationLogPersistable(&snap, ev, scope) {
		return nil
	}
	if err := w.persist(ctx, &snap); err != nil {
		return err
	}
	w.logPass(&snap, ev, &js, len(entries), scope)
	return nil
}

// publicationLogPersistable applies the publication log's caps to the snapshot
// this pass built, and reports whether it may be written. Growth is the only
// way the log can cross either cap (the decode refuses an over-cap one before
// loadPrevious returns), and the REMEDY is the one thing that differs by scope:
//
//   - a catalogue pass re-derives the log from the whole catalogue in place,
//     which is exactly the remedy the byte cap already prescribes and keeps the
//     journal intact (every journaled item is in the catalogue);
//   - a window pass has no whole-catalogue input to re-derive from, so it
//     defers to the next reconcile rather than persisting an over-cap log the
//     reader would refuse - which would have it serve nothing.
//
// Both caps are checked because neither implies the other: a short tracker-key
// entry serializes in ~20 bytes, so the byte budget admits ~419k entries
// against the decode's 250k cardinality cap.
func (w *FeedWriter) publicationLogPersistable(snap *snapshot, ev curationEvidence, scope passScope) bool {
	if publicationLogWithinLimits(snap.Published) && len(snap.Published) <= maxSnapshotMapEntries {
		return true
	}
	if scope == scopeCatalogue {
		w.log.Warn("indexer publication log exceeded its decode caps; rebuilt from the current catalogue",
			"entries", len(snap.Published), "max_entries", maxSnapshotMapEntries)
		snap.Published = baselinePublications(ev.entries())
		return true
	}
	w.log.Warn("indexer publication log crossed its cap while advancing; deferring to the next full rebuild",
		"entries", len(snap.Published), "max_entries", maxSnapshotMapEntries)
	return false
}

// logPass emits the one completion line both scopes share. The scope attribute
// is what an operator reads to tell a reconcile from a tick, and the counters
// are the same set for both - the tick used to drop several of them, which is
// how a counter it computed became unrecoverable.
func (w *FeedWriter) logPass(snap *snapshot, ev curationEvidence, js *journalStats, windowEntries int, scope passScope) {
	w.log.Info("indexer feed snapshot written",
		"scope", scope.String(),
		"input_entries", windowEntries, "curated_entries", len(ev.entries()),
		"owners", len(snap.Owners),
		"nyaa_feed", len(snap.NyaaFeed), "ab_feed", len(snap.ABFeed),
		"warned_excluded", ev.warnedKeys(),
		"journal_new", js.added, "journal_pruned", js.pruned, "journal_dropped", js.dropped,
		"journal_warned_dropped", js.warned,
		"journal_clock_rebased", js.rebased,
		"skipped_unresolvable", js.unresolvable,
		"ab_releases_skipped", js.abSkippedNoPasskey,
		"published", len(snap.Published),
		"harvest_queries", js.harvest.queries, "harvest_matched", js.harvest.matched,
		"harvest_rejected", js.harvest.rejected, "harvest_pending", js.harvest.pending)
	if js.abSkippedNoPasskey > 0 && w.enablement.enabled(upstreamAB) {
		// The nudge fires on BOTH paths. It cannot be deferred to the
		// reconcile: a tick that journals a link-less AnimeBytes release
		// records its identity in the never-pruned publication log, so the next
		// full pass reports isNew=false for it and counts zero - which made the
		// counter unrecoverable rather than merely unlogged.
		w.log.Warn("ab RSS feed empty of grabbable links: set indexer.ab_passkey to serve AnimeBytes releases",
			"ab_releases_skipped", js.abSkippedNoPasskey)
	}
}
