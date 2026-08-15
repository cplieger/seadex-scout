package indexer

import (
	"context"
	"fmt"

	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/tagfilter"
)

// This file is THE pass. The reconcile and the tick are ONE code path
// parameterized by SCOPE, which is the payoff of the rule table in members.go.

// carryPolicy names which arm carryItem may take for one carried journal item.
// It is a property of the pass's EVIDENCE, not a choice at the call site.
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

// renderPolicy names which render arm the pass's evidence AUTHORIZES for a key it
// is journaling for the FIRST time - carryPolicy's twin on the GROWTH side:
// renderJournalItem folds the download-volume-factor marker and the category union
// across the occurrences it is handed, so whether that fold is the whole answer is
// a property of the pass's evidence.
type renderPolicy int

const (
	// renderUnevaluated: the pass holds no occurrence of the key, so there is
	// nothing to render from - the render fails and nothing is published.
	renderUnevaluated renderPolicy = iota + 1
	// renderComplete: the pass holds EVERY occurrence of the key, so the fold
	// over its own evidence is the value the feed may serve.
	renderComplete
	// renderPartial: the pass holds SOME occurrences of the key and cannot know whether
	// there are others - a window's ordinary case, since ~4.4% of curated torrents are
	// attached to several entries.
	renderPartial
)

// curationEvidence is what ONE pass HOLDS about curation. It is an interface with
// exactly two implementations because the difference is not a parameter to read at
// a call site - it is which QUESTIONS the pass is entitled to answer. A window
// physically cannot hold a catalogue-wide warned set, so it cannot be handed one
// by mistake.
type curationEvidence interface {
	// scope names the input this evidence was built from.
	scope() passScope
	// entries returns the evaluated entries, tag-filtered.
	entries() []seadex.Entry
	// carryPolicy names which carry arm this pass's evidence AUTHORIZES for a
	// carried journal key, plus the occurrences the refresh arm needs.
	carryPolicy(key string) (carryPolicy, []curatedRef)
	// renderPolicy names which render arm this pass's evidence AUTHORIZES for a key
	// it is journaling for the FIRST time, plus the occurrences that arm folds.
	renderPolicy(key string) (renderPolicy, []curatedRef)
	// census is the file-census verdict for the keys this pass EVALUATED. A key it
	// did not evaluate is ABSENT rather than present-and-empty, and that absence IS
	// the authorization: this pass may not judge that key's title (applyTitles).
	census() map[string]packCensus
	// retracts reports whether a carried item shares a warned identity. Sound at
	// BOTH scopes, because retraction acts on positive evidence, never on absence.
	retracts(it *journalItem) bool
	// ownership is the per-entry curation contribution this pass may write, or nil
	// when the pass cannot vouch for its own evidence.
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

// windowEvidence is the tick's evidence: the recently-changed entries only, and a
// warned closure over just those. It holds no catalogue-wide anything.
type windowEvidence struct {
	cur    map[string][]curatedRef
	warned warnedSet
	kept   []seadex.Entry
	// tagPolicySet records whether the operator has ANY tag exclusion configured,
	// which decides whether this window's warned closure is complete enough to
	// admit keys into the search index - see ownership.
	tagPolicySet bool
}

// newEvidence builds the evidence for one pass over entries at scope. The tag
// filtering and the warned closure are the SAME computation at both scopes; what
// differs is what the result may conclude. The closure matters even at window
// scope because warning identity is transitive across occurrences, and SeaDex
// routinely lists one release on two trackers with a shared info hash and the
// `broken` tag on one occurrence only: filtering each torrent in isolation would
// journal the untagged twin and record its identity in the never-pruned
// publication log, after which it can never be re-admitted.
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

// renderPolicy: a catalogue pass holds every occurrence of every key it
// evaluated, so a fold over its own occurrences is the whole answer. A key absent
// from the catalogue has no occurrence to render from.
func (e *catalogueEvidence) renderPolicy(key string) (renderPolicy, []curatedRef) {
	if refs, evaluated := e.cur[key]; evaluated {
		return renderComplete, refs
	}
	return renderUnevaluated, nil
}

// census: the catalogue pass's file evidence covers every curated key, so every
// key it can journal is judged and only a de-curated key is carried.
func (e *catalogueEvidence) census() map[string]packCensus { return censusPacks(e.cur) }

// carryPolicy: a catalogue pass holds every occurrence of every key, so it may
// re-render what it evaluated - and because its input IS the catalogue, a key
// absent from it is absent from SeaDex, so it may conclude de-curation.
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

func (e *windowEvidence) renderPolicy(key string) (renderPolicy, []curatedRef) {
	refs, evaluated := e.cur[key]
	if !evaluated {
		return renderUnevaluated, nil
	}
	// A window holds the occurrences carried by the entries a curator happened to
	// touch, never the key's whole owner set, and cannot tell the two apart. So the
	// render is authorized only as PARTIAL, completed by carrying the unevaluated
	// owners' stored votes.
	return renderPartial, refs
}

// census covers only the WINDOW's keys, which is what makes the CARRIED title
// load-bearing: every journal key outside the window is unjudged here (the journal
// holds feedJournalMaxAge of items against a 48h window), so applyTitles must have
// a served value to carry rather than a raw harvested claim to re-judge blind.
func (e *windowEvidence) census() map[string]packCensus { return censusPacks(e.cur) }

// carryPolicy is ALWAYS carryVerbatim for a window. It cannot conclude de-curation,
// because an empty window is legitimate.
func (e *windowEvidence) carryPolicy(string) (carryPolicy, []curatedRef) {
	return carryVerbatim, nil
}

func (e *windowEvidence) retracts(it *journalItem) bool { return e.warned.retracts(it) }

// ownership admits this window's entries into the search index only when the
// window's own warned closure is COMPLETE, which is exactly when the operator has
// configured no tag exclusions at all: with an empty policy nothing is warned
// anywhere, so there is no reachable-only-from-outside exclusion to miss.
func (e *windowEvidence) ownership() map[string][]ownedRelease {
	if e.tagPolicySet {
		return nil
	}
	return ownershipOf(e.kept)
}

// ownershipOf reads the PRESENT fact off the entries a pass evaluated: for each
// entry, the set of releases it contributes, with that entry's OWN isBest vote.
// Two entries listing the same torrent produce two owner records, which is what
// makes the cross-owner fold recomputable and a demotion representable.
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
			// An entry evaluated down to nothing still has to APPEAR in the evaluated
			// set, so upsertOwners can clear a contribution that is no longer curated.
			out[id] = nil
		}
	}
	return out
}

// run is the one pass, at one scope. Every difference between the reconcile and
// the tick is either the scope value or one of the three catalogue-only steps.
func (w *FeedWriter) run(ctx context.Context, entries []seadex.Entry, info EntryInfoFunc, scope passScope) error {
	infoFor := entryInfoFunc(info)
	prev, err := w.loadPrevious(ctx)
	if err != nil {
		return err
	}
	if prev.baseline && scope == scopeWindow {
		// Recovery is the catalogue pass's job. Baselining from a window would record
		// the window's identities as published - permanently, since the log is never
		// pruned - and silently discard the journal.
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
		// Only reachable at catalogue scope. The whole current curation set is
		// FORFEITED into the publication log and the journal starts empty, growing
		// only from genuinely new curation: backfill is search's job.
		writes.published = baselinePublications(ev.entries())
		writes.titles = map[string]string{}
		w.log.Info("indexer feed journal baselined; RSS feed starts empty and grows from newly curated releases",
			"reason", prev.reason, "published", len(writes.published))
	} else {
		pass := &journalPass{
			w: w, ev: ev, published: prev.published, publish: writes.published,
			// prior is the ownership fact the PREVIOUS snapshot holds: the only record
			// of the votes of owners this pass did not evaluate.
			prior:   prev.owners,
			infoFor: infoFor, js: &js, now: now,
		}
		// The file-census pack verdict per journal key, judged by the evidence this
		// pass holds, so applyTitles can tell when a harvested title contradicts the
		// payload it names. A key this pass did not evaluate has no census entry and
		// keeps the title the app already SERVES for it.
		census := ev.census()
		// Carry BOTH journals regardless of configuration: a tracker's off switch must
		// be reversible.
		carriedNyaa := pass.carryJournal(prev.nyaaFeed, upstreamNyaa)
		carriedAB := pass.carryJournal(prev.abFeed, upstreamAB)
		// Growth stays gated per scope: newJournalItem returns early for an
		// unconfigured tracker, so an off tracker's journal shrinks but never grows.
		newNyaa, newAB := pass.growJournal(ev.entries())
		carriedNyaa = append(carriedNyaa, newNyaa...)
		carriedAB = append(carriedAB, newAB...)
		writes.nyaa, writes.ab = carriedNyaa, carriedAB

		feeds := map[string][]journalItem{upstreamNyaa: writes.nyaa, upstreamAB: writes.ab}
		if scope == scopeCatalogue {
			// CATALOGUE-ONLY STEP: the Prowlarr title harvest. It is rate-paced and
			// rotates over the whole journal, so a window cannot own its cursor.
			hs, cursor := w.harvest.harvestTitles(ctx, feeds, writes.titles, infoFor, prev.cursor)
			writes.cursor = cursor
			js.harvest = hs
		} else {
			// The other three harvest counters are ACTIONS, so a tick's zeros are true.
			js.harvest.pending = syntheticCount(feeds, writes.titles)
		}
		// ONE audit value across both feeds, so its onset latch is per PASS.
		audit := titleAudit{census: census, report: w.packDisagreementReporter()}
		applyTitles(writes.nyaa, writes.titles, audit)
		applyTitles(writes.ab, writes.titles, audit)
	}

	// sortFeed is NOT optional at either scope. The reader serves the persisted
	// order with no sort of its own, and an arr walking an RSS feed stops at the
	// first item older than its last sync - so a new item appended at the tail is
	// unreachable.
	writes.nyaa, writes.ab = sortFeed(writes.nyaa), sortFeed(writes.ab)
	if scope == scopeCatalogue {
		// CATALOGUE-ONLY STEP: retainTitles. It prunes the harvest cache to the keys
		// still journaled, reading the whole journal against the whole catalogue, so a
		// window would drop the titles of every key it did not evaluate.
		writes.titles = retainTitles(writes.titles, writes.nyaa, writes.ab)
	}

	snap, err := buildSnapshot(&prev, &writes)
	if err != nil {
		return err
	}
	if err := w.publicationLogPersistable(&snap, scope); err != nil {
		return err
	}
	if err := w.persist(ctx, &snap); err != nil {
		return err
	}
	w.logPass(&snap, ev, &js, len(entries), scope)
	return nil
}

// publicationLogPersistable applies the publication log's caps to the snapshot
// this pass built, and reports an error when it may NOT be written. Growth is the
// only way to cross either cap, and the answer is the same at BOTH scopes: keep
// the last-good feed.json and fail the pass so the cycle reports degradation.
func (w *FeedWriter) publicationLogPersistable(snap *snapshot, scope passScope) error {
	if publicationLogWithinLimits(snap.Published) && len(snap.Published) <= maxSnapshotMapEntries {
		return nil
	}
	w.log.Warn("indexer publication log crossed its decode caps; the last-good feed snapshot is kept unchanged - remove feed.json to re-baseline",
		"scope", scope.String(), "entries", len(snap.Published), "max_entries", maxSnapshotMapEntries)
	return fmt.Errorf("indexer: publication log exceeds its decode caps (%d entries, max %d): snapshot not written",
		len(snap.Published), maxSnapshotMapEntries)
}

// logPass emits the one completion line both scopes share. The scope attribute is
// what an operator reads to tell a reconcile from a tick, and the counters are the
// same set for both - the tick used to drop several of them.
func (w *FeedWriter) logPass(snap *snapshot, ev curationEvidence, js *journalStats, windowEntries int, scope passScope) {
	w.log.Info("indexer feed snapshot written",
		"scope", scope.String(),
		"input_entries", windowEntries, "curated_entries", len(ev.entries()),
		"owners", len(snap.Owners),
		"nyaa_feed", len(snap.NyaaFeed), "ab_feed", len(snap.ABFeed),
		"warned_excluded", ev.warnedKeys(),
		"journal_new", js.added, "journal_pruned", js.pruned, "journal_dropped", js.dropped,
		"journal_warned_dropped", js.warned,
		"journal_stored_decurated", js.storedDecurated,
		"journal_stored_unrenderable", js.storedUnrenderable,
		"journal_clock_rebased", js.rebased,
		"skipped_unresolvable", js.unresolvable,
		"ab_releases_skipped", js.abSkippedNoPasskey,
		"published", len(snap.Published),
		"harvest_queries", js.harvest.queries, "harvest_matched", js.harvest.matched,
		"harvest_rejected", js.harvest.rejected, "harvest_pending", js.harvest.pending)
	if js.abSkippedNoPasskey > 0 && w.enablement.enabled(upstreamAB) {
		// The nudge fires on BOTH paths so the operator learns why the AB feed is
		// empty from the pass that met the releases. Nothing is published for a
		// release the app could not hand an arr, so the count is recoverable: the
		// releases journal as new once the passkey arrives.
		w.log.Warn("ab RSS feed empty of grabbable links: set indexer.ab_passkey to serve AnimeBytes releases",
			"ab_releases_skipped", js.abSkippedNoPasskey)
	}
}
