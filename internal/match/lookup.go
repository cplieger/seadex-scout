package match

import (
	"context"
	"errors"
	"time"

	"github.com/cplieger/seadex-scout/internal/anilist"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// AniListClient is the AniList fallback surface the matcher needs: a single
// lookup for the per-entry path and a batched lookup the matcher uses to
// pre-warm the memo for a whole cycle in a handful of requests.
type AniListClient interface {
	Fetch(ctx context.Context, aniListID int) (anilist.Media, error)
	FetchMany(ctx context.Context, ids []int) (map[int]anilist.Media, error)
}

// --- Memo: the persisted AniList lookup cache and its expiry policy ---

// memoMinTTL and memoMaxTTL bound the uniform random TTL stamped on every
// memo write: mean 14 days with ±25% jitter, so entries written together (a
// cold cycle's whole batch) expire spread across a week instead of in
// lockstep. The policy is time-based, not run-based, so it is independent of
// poll_interval.
const (
	memoMinTTL = 252 * time.Hour // 10.5 days (14d − 25%)
	memoMaxTTL = 420 * time.Hour // 17.5 days (14d + 25%)
	// memoMinMigration is the migration window's lower bound: a legacy entry
	// (persisted before the expiry policy, so it loads with a zero Expiry) is
	// stamped on first load with a TTL in [memoMinMigration, memoMaxTTL),
	// spreading the whole backlog's first renewal across ~17 days with no
	// day-one re-fetch stampede.
	memoMinMigration = 24 * time.Hour
)

// MemoEntry is a cached AniList lookup (titles/format/year), or a negative
// result, keyed by AniList ID in a Memo. Expiry is the instant the entry
// stops being served (stamped at write time with a jittered TTL; see the
// package comment): an expired entry is a lookup miss, re-fetched and
// re-stamped on its next use, and pruned when a Match pass ends without
// renewing it. A zero Expiry marks a legacy entry persisted before the
// policy; Match stamps it on first load.
type MemoEntry struct {
	Expiry   time.Time `json:"expiry,omitzero"`
	Format   string    `json:"format,omitempty"`
	Titles   []string  `json:"titles,omitempty"`
	Year     int       `json:"year,omitempty"`
	NotFound bool      `json:"not_found,omitempty"`
}

// expired reports whether the entry's expiry has passed at now. A zero Expiry
// (a legacy entry) also reads as expired, defensively; Match migrates legacy
// entries to a real expiry before any lookup consults them.
func (e *MemoEntry) expired(now time.Time) bool { return !e.Expiry.After(now) }

// Memo persists AniList fallback lookups across cycles. Entries carry
// per-entry jittered expiries so stale answers age out (AniList data changes:
// entries and titles are added after licensing) with renewals staggered
// across cycles rather than expiring in lockstep.
type Memo struct {
	Entries map[int]MemoEntry `json:"entries,omitempty"`
}

// liveEntry returns the memo entry for id when it exists and is unexpired at
// now: the ONE liveness rule both pendingAniListIDs (skip a non-pending id)
// and lookupAniList (serve a memo hit) consult, so the batch worklist and the
// per-entry hit test cannot drift.
func (m *Memo) liveEntry(id int, now time.Time) (MemoEntry, bool) {
	ent, ok := m.Entries[id]
	if !ok || ent.expired(now) {
		return MemoEntry{}, false
	}
	return ent, true
}

// StaleTitle returns the memoized AniList title/year for id, deliberately
// ignoring expiry: the memo's expiry governs re-fetch cadence, and a stale
// show title still beats a file-name derivation (the feed's title tier).
// ok is false for an absent entry, a not-found negative, or an entry with
// no titles.
func (m *Memo) StaleTitle(id int) (title string, year int, ok bool) {
	ent, cached := m.Entries[id]
	if !cached || ent.NotFound || len(ent.Titles) == 0 {
		return "", 0, false
	}
	return ent.Titles[0], ent.Year, true
}

// jitteredTTL draws one uniform random TTL from [minTTL, memoMaxTTL): the
// per-entry stagger that keeps memo renewals spread across cycles. Every memo
// write draws its own TTL, so even entries written by the same batch renew
// apart.
func (m *Matcher) jitteredTTL(minTTL time.Duration) time.Duration {
	return minTTL + time.Duration(m.rand()*float64(memoMaxTTL-minTTL))
}

// freshExpiry stamps one memo write's expiry: now plus a fresh jittered TTL.
// Each write calls it separately, so entries written in the same pass (batch
// or per-id) still expire staggered.
func (m *Matcher) freshExpiry(now time.Time) time.Time {
	return now.Add(m.jitteredTTL(memoMinTTL))
}

// migrateMemo stamps every legacy entry (a zero Expiry, persisted before the
// expiry policy) with an expiry drawn from the wider [memoMinMigration,
// memoMaxTTL) window, so the accumulated backlog's first renewal spreads
// across the whole window instead of stampeding on one day (or, without any
// stamp, living forever). A migrated entry is live until its drawn expiry, so
// migration itself never triggers a fetch.
func (m *Matcher) migrateMemo(memo *Memo, now time.Time) {
	for id, ent := range memo.Entries {
		if ent.Expiry.IsZero() {
			ent.Expiry = now.Add(m.jitteredTTL(memoMinMigration))
			memo.Entries[id] = ent
		}
	}
}

// pruneExpired drops every entry still expired at the run's clock: renewals
// were re-stamped with a future expiry during the pass, and it only runs on a
// clean (non-degraded) pass, so what remains expired was simply not consulted
// this cycle and reads as a miss anyway — next cycle's batched prefetch
// re-fetches whichever ids are still needed. Pruning keeps state.json from
// accumulating dead entries for ids the match no longer consults.
func pruneExpired(memo *Memo, now time.Time) {
	for id, ent := range memo.Entries {
		if ent.expired(now) {
			delete(memo.Entries, id)
		}
	}
}

// markIncomplete flags the pass degraded and records the AniList id whose
// needed lookup failed transiently, so Result.IncompleteIDs carries exactly
// the entries whose library mapping is unknown this pass (never the memoized
// or definitively answered ones).
func (r *matchRun) markIncomplete(aniListID int) {
	r.degraded = true
	if r.incomplete == nil {
		r.incomplete = make(map[int]struct{})
	}
	r.incomplete[aniListID] = struct{}{}
}

// entryExpiry draws one fresh jittered expiry from the run's clock. Each memo
// write calls it separately, so entries renewed in the same pass still expire
// staggered.
func (r *matchRun) entryExpiry() time.Time { return r.m.freshExpiry(r.now) }

// mediaEntry builds a positive memo entry for media stamped with expiry.
func mediaEntry(media anilist.Media, expiry time.Time) MemoEntry {
	return MemoEntry{Titles: media.Titles, Format: media.Format, Year: media.Year, Expiry: expiry}
}

// notFoundEntry builds a negative (not-found) memo entry stamped with expiry,
// the negative twin of mediaEntry.
func notFoundEntry(expiry time.Time) MemoEntry {
	return MemoEntry{NotFound: true, Expiry: expiry}
}

// --- Prefetch: the batched cold-cycle memo warm-up ---

// prefetch batch-fetches into the memo every AniList id the per-entry pass will
// consult but has no live (unexpired) entry for, so a cold cycle costs a
// handful of batched AniList requests instead of one request per id-less entry
// — and an expired entry renews through the same batch, since pendingAniListIDs
// counts it as pending. Every write (positive or negative) is stamped with a
// fresh jittered expiry. A PARTIAL batch failure is best-effort: an id a
// partial batch does not return is left uncached and falls through to
// matchEntry's single Fetch, so batching never changes the match result,
// only the request count. A TOTAL batch failure (no chunk succeeded -
// an AniList outage) instead returns the pending ids so the per-entry pass
// fails them fast: every per-id lookup would be doomed against the same outage,
// and the unbounded futile tail of requests would only stall the cycle.
func (m *Matcher) prefetch(ctx context.Context, entries []seadex.Entry, idx *mapping.Index, lib *LibIndex, memo *Memo, now time.Time) map[int]struct{} {
	if ctx.Err() != nil {
		// Mirror the per-entry loop's cancellation guard: a batch issued on an
		// already-cancelled cycle can only fail with context.Canceled, and the
		// loop below breaks (and flags the cycle degraded) before using it.
		return nil
	}
	ids := pendingAniListIDs(entries, idx, lib, memo, now)
	if len(ids) == 0 {
		return nil
	}
	fetched, err := m.anilist.FetchMany(ctx, ids)
	switch {
	case err == nil:
	case errors.Is(err, context.Canceled):
		// A cancellation is not a fault (same contract as Scout.save).
		m.log.Debug("anilist batch prefetch cancelled",
			"requested", len(ids), "fetched", len(fetched))
	case fetched == nil:
		// TOTAL failure: FetchMany's completion contract returns a nil map
		// only when NO chunk completed (a request/envelope failure before
		// any chunk finished). A non-nil-but-EMPTY result is NOT an outage —
		// at least one chunk completed and simply produced no media (every
		// id definitively not found, or every record malformed, which is
		// record-local) — so it falls to the default branch and each absent
		// id stays uncached for the documented per-id Fetch fallback instead
		// of being failed fast.
		// Degrade fast: fail the pending ids immediately instead of
		// regressing to one doomed per-id request each.
		m.log.Warn("anilist batch prefetch failed; skipping per-id fallback for pending ids",
			"requested", len(ids), "error", err)
		outage := make(map[int]struct{}, len(ids))
		for _, id := range ids {
			outage[id] = struct{}{}
		}
		return outage
	default:
		m.log.Warn("anilist batch prefetch incomplete; remaining ids fall back to per-id fetch",
			"requested", len(ids), "fetched", len(fetched), "error", err)
	}
	for _, id := range ids {
		if media, ok := fetched[id]; ok {
			memo.Entries[id] = mediaEntry(media, m.freshExpiry(now))
			continue
		}
		if err == nil {
			// The batch completed without returning this id: AniList has no such
			// media. Memoize the negative so it is not re-fetched this run; the
			// expiry gives the negative the same lifetime policy as a positive,
			// so a show created on AniList later is eventually seen.
			memo.Entries[id] = notFoundEntry(m.freshExpiry(now))
		}
		// err != nil and id not returned: leave uncached so matchEntry retries it
		// via the single Fetch.
	}
	return nil
}

// pendingAniListIDs returns the distinct AniList ids the match will look up but
// has no live memo entry for: exactly the entries aniListNeed - the shared
// trigger matchEntry also consults - classifies as needing a lookup, so the
// batch fetches no more (which would re-introduce the not-in-library lookups
// the HasArrIdentifier gate removed) and no less than the per-entry pass needs.
// An EXPIRED entry counts as pending — the same rule that makes it a miss in
// lookupAniList — so renewals ride the batch instead of one per-id request
// each.
func pendingAniListIDs(entries []seadex.Entry, idx *mapping.Index, lib *LibIndex, memo *Memo, now time.Time) []int {
	seen := make(map[int]struct{})
	var ids []int
	add := func(alID int) {
		if _, live := memo.liveEntry(alID, now); live {
			return
		}
		if _, dup := seen[alID]; dup {
			return
		}
		seen[alID] = struct{}{}
		ids = append(ids, alID)
	}
	for i := range entries {
		if _, _, _, needsLookup := aniListNeed(entries[i].AniListID, idx, lib); needsLookup {
			add(entries[i].AniListID)
		}
	}
	return ids
}

// --- lookupGate + per-id lookup: fast-fail and degradation accounting ---

// transientFailureCap is the consecutive transient per-id AniList failure
// streak at which the matcher stops issuing further lookups for the cycle: an
// outage that begins after the first prefetch chunk succeeds looks like a
// PARTIAL batch failure to prefetch, so without this breaker every remaining
// uncached id regresses to one doomed (internally retried) request each - the
// same futile tail the total-outage fast-fail exists to avoid.
const transientFailureCap = 3

// lookupGate gates per-id AniList lookups for one Match pass: ids covered by a
// totally failed batch prefetch fail fast, and a streak of consecutive
// transient per-id failures trips the same fast-fail for every remaining
// uncached id.
type lookupGate struct {
	outage  map[int]struct{}
	streak  int
	tripped bool
}

// down reports whether the id must fail fast (outage-covered or breaker tripped).
func (g *lookupGate) down(id int) bool {
	if g.tripped {
		return true
	}
	_, down := g.outage[id]
	return down
}

// recordFailure counts a consecutive transient failure; it returns true on the
// call that trips the breaker.
func (g *lookupGate) recordFailure() bool {
	g.streak++
	if g.streak == transientFailureCap {
		g.tripped = true
		return true
	}
	return false
}

// recordSuccess resets the streak (a definitive answer - media or not-found -
// proves the upstream is answering).
func (g *lookupGate) recordSuccess() { g.streak = 0 }

// lookupAniList consults the memo, then AniList. An expired memo entry is a
// miss: it falls through to a fresh fetch and is re-stamped on renewal, so a
// stale answer (a show created on AniList after the negative was cached, a
// title added after the positive was) ages out. A not-found result is memoized
// (negatively) so it is not re-fetched before its expiry; a transient error is
// not memoized so it is retried next cycle. An id the gate reports down
// (covered by a totally-failed batch prefetch, or the
// consecutive-transient-failure breaker tripped) fails fast without a per-id
// request: the same outage would doom it, and the id stays un-memoized so it
// is retried next cycle.
func (r *matchRun) lookupAniList(ctx context.Context, aniListID int) (anilist.Media, bool) {
	if ent, live := r.memo.liveEntry(aniListID, r.now); live {
		if ent.NotFound {
			return anilist.Media{}, false
		}
		return anilist.Media{Titles: ent.Titles, Format: ent.Format, Year: ent.Year}, true
	}
	if r.gate.down(aniListID) {
		// Degrade fast through the existing accounting (the prefetch already
		// logged the single outage WARN): the affected entry's prior findings
		// are preserved rather than the missing match read as resolved.
		r.markIncomplete(aniListID)
		return anilist.Media{}, false
	}
	media, err := r.m.anilist.Fetch(ctx, aniListID)
	if err != nil {
		r.handleLookupFailure(aniListID, err)
		return anilist.Media{}, false
	}
	r.gate.recordSuccess()
	r.memo.Entries[aniListID] = mediaEntry(media, r.entryExpiry())
	return media, true
}

// handleLookupFailure classifies a failed AniList fetch: a definitive
// not-found is memoized negatively (a definitive answer, so the breaker
// streak resets); anything else marks the cycle incomplete and leaves the
// id un-memoized so it is retried next cycle.
func (r *matchRun) handleLookupFailure(aniListID int, err error) {
	if errors.Is(err, anilist.ErrNotFound) {
		r.gate.recordSuccess()
		r.memo.Entries[aniListID] = notFoundEntry(r.entryExpiry())
		return
	}
	// A transient/upstream error (network, context cancellation, rate-limit
	// exhaustion) means this needed fallback lookup could not be completed.
	// Record the id as incomplete (flagging the cycle degraded) so the
	// caller preserves the affected entry's prior findings rather than
	// treating the missing match as a resolved finding, and leave the
	// id un-memoized so it is retried next cycle.
	r.markIncomplete(aniListID)
	if errors.Is(err, context.Canceled) {
		// A cancellation is not a fault (same contract as Scout.save):
		// log at Debug so a redeploy is not attributed to an AniList outage.
		r.m.log.Debug("anilist fallback cancelled", "al_id", aniListID)
		return
	}
	r.m.log.Warn("anilist fallback failed", "al_id", aniListID, "error", err)
	if r.gate.recordFailure() {
		r.m.log.Warn("anilist fallback failing repeatedly; failing remaining lookups fast this cycle",
			"consecutive_failures", transientFailureCap)
	}
}
