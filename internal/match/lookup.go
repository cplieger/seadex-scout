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
	FetchMany(ctx context.Context, ids []int) (anilist.BatchResult, error)
}

// The assertion sits at the DECLARATION, not at *anilist.Client's own package as
// go.md's default prescribes: internal/match imports internal/anilist, so an
// assertion there would close an import cycle. internal/scout pins its three
// consumer-side interfaces (SeaDexSource, StateStore, MappingSource) the same way
// for the same reason.
var _ AniListClient = (*anilist.Client)(nil)

// --- Memo: the persisted AniList lookup cache and its expiry policy ---

// memoMinTTL and memoMaxTTL bound the uniform random TTL stamped on every
// memo write: mean 14 days with ±25% jitter, so entries written together (a
// cold cycle's whole batch) expire spread across a week instead of in
// lockstep. The policy is time-based, not run-based, so it is independent of
// poll_interval.
const (
	memoMinTTL = 252 * time.Hour // 10.5 days (14d − 25%)
	memoMaxTTL = 420 * time.Hour // 17.5 days (14d + 25%)
)

// MemoEntry is a cached AniList lookup (titles/format/year), or a negative
// result, keyed by AniList ID in a Memo.
type MemoEntry struct {
	Expiry   time.Time `json:"expiry,omitzero"`
	Format   string    `json:"format,omitempty"`
	Titles   []string  `json:"titles,omitempty"`
	Year     int       `json:"year,omitempty"`
	NotFound bool      `json:"not_found,omitempty"`
}

// expired reports whether the entry's expiry has passed at now. A zero Expiry
// reads as expired too, which is what makes the absence of a migration safe:
// an entry with no stamp is re-fetched and re-stamped rather than served
// forever.
func (e *MemoEntry) expired(now time.Time) bool { return !e.Expiry.After(now) }

// Memo persists AniList fallback lookups across cycles. Entries carry
// per-entry jittered expiries so stale answers age out (AniList data changes:
// entries and titles are added after licensing) with renewals staggered
// across cycles rather than expiring in lockstep.
type Memo struct {
	Entries map[int]MemoEntry `json:"entries,omitempty"`
	// dirty records whether this pass wrote to the memo at all, so a caller can
	// decide whether persisting is worth the cost. It is UNEXPORTED, so it never
	// reaches the wire and a loaded memo always starts clean.
	dirty bool
}

// put records an entry and marks the memo dirty. Every write goes through here.
//
// ent is a pointer purely to satisfy gocritic's hugeParam (MemoEntry is 80
// bytes); it is copied into the map, never retained.
func (m *Memo) put(id int, ent *MemoEntry) {
	if m.Entries == nil {
		m.Entries = make(map[int]MemoEntry)
	}
	m.Entries[id] = *ent
	m.dirty = true
}

// remove drops an entry and marks the memo dirty; pruning's only write path.
func (m *Memo) remove(id int) {
	delete(m.Entries, id)
	m.dirty = true
}

// Changed reports whether this pass wrote to the memo. A caller that persists
// the memo as part of a larger document uses it to skip a write that would
// change nothing.
func (m *Memo) Changed() bool { return m.dirty }

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

// StaleFormat returns the memoized AniList media format for id under
// StaleTitle's expiry-ignoring rule, so a consumer taking a stale title can
// take the type that came with it. ok is false for an absent entry, a
// not-found negative, or an entry AniList gave no format for.
func (m *Memo) StaleFormat(id int) (format string, ok bool) {
	ent, cached := m.Entries[id]
	if !cached || ent.NotFound || ent.Format == "" {
		return "", false
	}
	return ent.Format, true
}

// staleMedia returns the memoized media for id ignoring expiry: the same
// expiry-ignoring read StaleTitle and StaleFormat give the indexer feed's
// title/type tier, widened to the whole title list a match needs.
func (m *Memo) staleMedia(id int) (anilist.Media, bool) {
	ent, cached := m.Entries[id]
	if !cached || ent.NotFound || (len(ent.Titles) == 0 && ent.Format == "") {
		return anilist.Media{}, false
	}
	return anilist.Media{Titles: ent.Titles, Format: ent.Format, Year: ent.Year}, true
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

// restampSkewedExpiries re-stamps every loaded entry whose expiry sits BEYOND
// anything this policy could have written. It is a clock correction, not a
// migration: the app carries no old-to-new conversion for a persisted memo, so
// an entry with no expiry at all simply reads as expired and is re-fetched on
// its next use.
func (m *Matcher) restampSkewedExpiries(memo *Memo, now time.Time) {
	horizon := now.Add(memoMaxTTL)
	restamped := 0
	for id, ent := range memo.Entries {
		if !ent.Expiry.After(horizon) {
			continue
		}
		// An expiry further out than any this policy can stamp did not
		// come from this policy: the clock was wrong when the entry was
		// written (a boot before NTP sync), or state.json was edited.
		ent.Expiry = m.freshExpiry(now)
		restamped++
		memo.put(id, &ent)
	}
	if restamped > 0 {
		// One counted line, not one per entry: the cause is a whole-file
		// property (a skewed clock during one cycle, or an edited state.json),
		// so the count is the signal.
		m.log.Warn("anilist memo: expiries beyond the policy horizon re-stamped",
			"restamped", restamped)
	}
}

// pruneExpired drops the expired entries that are dead cache data, and keeps
// the ones that are still an ACTIVE consumer's fallback.
func pruneExpired(memo *Memo, now time.Time, entries []seadex.Entry) {
	active := make(map[int]struct{}, len(entries))
	for i := range entries {
		active[entries[i].AniListID] = struct{}{}
	}
	for id, ent := range memo.Entries {
		if !ent.expired(now) {
			continue
		}
		_, stillCurated := active[id]
		// The serviceability half is staleMedia's own admissibility rule, read rather
		// than restated: retention exists exactly so the entries staleMedia and the
		// feed's stale-title tier can still serve survive, so the two must not be two
		// lists.
		_, usefulStale := memo.staleMedia(id)
		if stillCurated && usefulStale {
			continue
		}
		memo.remove(id)
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
// counts it as pending.
func (m *Matcher) prefetch(ctx context.Context, entries []seadex.Entry, idx *mapping.Index, lib *LibIndex, memo *Memo, now time.Time) map[int]struct{} {
	if ctx.Err() != nil {
		// Mirror the per-entry loop's cancellation guard: a batch issued on an
		// already-cancelled cycle can only fail with context.Canceled, and the
		// loop below breaks (and flags the cycle degraded) before using it.
		return nil
	}
	pending := pendingAniListIDs(entries, idx, lib, memo, now)
	if len(pending) == 0 {
		return nil
	}
	for {
		res := m.prefetchPass(ctx, pending, memo, now)
		if res.outage != nil {
			return res.outage
		}
		// A CANCELLATION is not an upstream abort, and the ids no request
		// covered would only be refused by the same done context - so
		// re-batching buys one doomed request and its WARN would blame
		// AniList for a routine redeploy.
		if errors.Is(res.err, context.Canceled) {
			return nil
		}
		// A shrinking worklist means the pass abandoned chunks it never asked:
		// re-batch exactly those.
		if len(res.unrequested) > 0 && len(res.unrequested) < len(pending) {
			m.log.Warn("anilist batch prefetch aborted; re-batching the ids no request covered",
				"requested", len(pending), "fetched", res.fetched,
				"retrying", len(res.unrequested), "error", res.err)
			pending = res.unrequested
			continue
		}
		if res.err != nil && !errors.Is(res.err, context.Canceled) {
			m.log.Warn("anilist batch prefetch incomplete; remaining ids fall back to per-id fetch",
				"requested", len(pending), "fetched", res.fetched, "error", res.err)
		}
		return nil
	}
}

// prefetchResult is what one batched prefetch pass leaves for the caller to do.
type prefetchResult struct {
	err error
	// outage is non-nil only for a TOTAL failure: no chunk completed, so every
	// id in the pass fails fast instead of regressing to a doomed per-id Fetch.
	outage map[int]struct{}
	// unrequested are the ids the pass abandoned without asking (an aborting
	// chunk and the ones after it), which the caller re-batches.
	unrequested []int
	fetched     int
}

// prefetchPass issues ONE batched FetchMany over pending and applies prefetch's
// memoization rules to the answer: every id a completed chunk definitively
// resolved is memoized (positively, or negatively when absent), and every id
// whose chunk was not trustworthy is left uncached. The rules read one verdict
// per id, so this pass knows nothing about the batch's chunking.
func (m *Matcher) prefetchPass(ctx context.Context, pending []int, memo *Memo, now time.Time) prefetchResult {
	res, err := m.anilist.FetchMany(ctx, pending)
	fetched := res.Media
	out := prefetchResult{err: err, fetched: len(fetched)}
	switch {
	case err == nil:
	case errors.Is(err, context.Canceled):
		// A cancellation is not a fault (same contract as Scout.save).
		m.log.Debug("anilist batch prefetch cancelled",
			"requested", len(pending), "fetched", len(fetched))
	case len(res.Verdicts) == 0:
		// TOTAL failure: not one chunk completed (a request/envelope failure
		// before any chunk finished), so no id carries an answer at all.
		m.log.Warn("anilist batch prefetch failed; skipping per-id fallback for pending ids",
			"requested", len(pending), "error", err)
		outage := make(map[int]struct{}, len(pending))
		for _, id := range pending {
			outage[id] = struct{}{}
		}
		out.outage = outage
		return out
	}
	for _, id := range pending {
		switch res.Verdicts[id] {
		case anilist.VerdictFound:
			entry := mediaEntry(fetched[id], m.freshExpiry(now))
			memo.put(id, &entry)
		case anilist.VerdictAbsent:
			// The batch definitively answered this id and AniList has no such
			// media.
			entry := notFoundEntry(m.freshExpiry(now))
			memo.put(id, &entry)
		case anilist.VerdictUnverified:
			// This id's chunk was not trustworthy: leave it uncached so
			// matchEntry retries it via the single Fetch. Re-batching would
			// only re-fetch the same poisoned record.
		case anilist.VerdictUnrequested:
			// Never asked at all, so no answer exists yet - the caller
			// re-batches these 50 at a time.
			out.unrequested = append(out.unrequested, id)
		}
	}
	return out
}

// pendingAniListIDs returns the distinct AniList ids the match will look up but
// has no live memo entry for: exactly the entries aniListNeed - the shared
// trigger matchEntry also consults - classifies as needing a lookup, so the
// batch fetches no more (which would re-introduce the not-in-library lookups
// the HasArrIdentifier gate removed) and no less than the per-entry pass needs.
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
// title added after the positive was) ages out.
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
		return r.memo.staleMedia(aniListID)
	}
	media, err := r.m.anilist.Fetch(ctx, aniListID)
	if err != nil {
		if !r.handleLookupFailure(aniListID, err) {
			// A definitive answer (not-found, or an unusable record): it
			// supersedes whatever the memo still holds, so no stale fallback.
			return anilist.Media{}, false
		}
		return r.memo.staleMedia(aniListID)
	}
	r.gate.recordSuccess()
	entry := mediaEntry(media, r.entryExpiry())
	r.memo.put(aniListID, &entry)
	return media, true
}

// handleLookupFailure classifies a failed AniList fetch: a definitive answer -
// a not-found, or a record whose own content makes it unmatchable
// (ErrRecordUnusable) - is memoized negatively and resets the breaker streak;
// anything else marks the cycle incomplete and leaves the id un-memoized so it
// is retried next cycle.
func (r *matchRun) handleLookupFailure(aniListID int, err error) (transient bool) {
	// A DEFINITIVE answer, in one arm because the handling is one decision: AniList
	// has no such media, or the record exists but its own content cannot yield a
	// match key.
	unusable := errors.Is(err, anilist.ErrRecordUnusable)
	if unusable || errors.Is(err, anilist.ErrNotFound) {
		r.gate.recordSuccess()
		entry := notFoundEntry(r.entryExpiry())
		r.memo.put(aniListID, &entry)
		if unusable {
			// Say it once, naming the remedy that actually applies; a routine
			// not-found stays silent.
			r.m.log.Warn("anilist record unusable for matching; add an overrides.json entry to map it directly",
				"al_id", aniListID, "error", err)
		}
		return false
	}
	// A transient/upstream error (network, context cancellation, rate-limit
	// exhaustion) means this needed fallback lookup could not be completed.
	r.markIncomplete(aniListID)
	if errors.Is(err, context.Canceled) {
		// A cancellation is not a fault (same contract as Scout.save):
		// log at Debug so a redeploy is not attributed to an AniList outage.
		r.m.log.Debug("anilist fallback cancelled", "al_id", aniListID)
		return true
	}
	r.m.log.Warn("anilist fallback failed", "al_id", aniListID, "error", err)
	if r.gate.recordFailure() {
		r.m.log.Warn("anilist fallback failing repeatedly; failing remaining lookups fast this cycle",
			"consecutive_failures", transientFailureCap)
	}
	return true
}
