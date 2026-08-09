// Package degradation is the neutral home of the application-wide
// degradation policy: the two CADENCE-NAMED escalation thresholds a persisted
// degradation streak escalates its log site at, the transition rule those
// streaks advance and reset by (Advance), the library shrink guard's bounded
// acceptance threshold, the shrink guards' trigger fraction applied through
// Shrunk (mapping refresh + library walk + SeaDex catalogue walk), and the
// pre-cliff warning fraction the persisted-file byte caps warn at (state.json,
// the indexer feed snapshot).
// It is a leaf with no imports so mapping, scout, state and seadex can reference
// the one policy without either domain owning cross-domain operational policy.
//
// The THRESHOLDS and the TRANSITION live here; the COUNTERS stay with the file
// that persists them (state.State's streak fields, mapping.Cache's own). That
// split is deliberate: a counter is persisted data whose shape belongs to its
// envelope, while "how many consecutive failures is too many, and how does a
// streak advance" is cross-domain policy no single domain should own. Both
// thresholds sit here TOGETHER and are named for their cadence, because the
// mistake this package exists to prevent is an author reaching for whichever
// number the file they happened to open contained - the count is
// cadence-relative, so the same integer means 2h on the tick and 8 days on the
// reconcile (h-f23).
package degradation

// TickEscalationThreshold is the TICK-cadence consecutive-degraded-cycle streak
// at which a persisted degradation streak escalates its single log site from
// WARN to ERROR (firing the existing SeadexScoutCycleError Loki rule): tolerate
// 8 consecutive degraded passes - about 2h at the default 15m poll_interval -
// long enough to ride out a transient upstream or arr oddity, short enough that
// a persistent fault alerts instead of degrading silently forever.
//
// Its one consumer is the mapping loader's refresh-rejection streak
// (mapping.Cache.RejectedRefreshes), because loadMapping runs on every changed
// tick as well as every reconcile.
const TickEscalationThreshold = 8

// ReconcileEscalationThreshold is the same fleet policy at the RECONCILE's daily
// cadence, for the streaks that advance only on a full pass
// (state.State.ShrunkWalksByArr, PartialWalks, SeadexFailures, AniListDegraded).
// Two consecutive failed full passes is 48h, the closest whole-run threshold to
// the ~24h the fleet's 8 used to buy when every cycle was a full pass on a 3h
// interval - and one full run of tolerance is the minimum that can still
// distinguish a transient failure from a condition that will not self-heal.
//
// It is a SEPARATE constant rather than a reuse of the tick number because the
// count is cadence-relative: 8 daily reconciles would be 8 days, which is how
// the fleet's number silently became a week once the passes it counted stopped
// running hourly.
const ReconcileEscalationThreshold = 2

// ShrunkWalkAcceptThreshold is when the LIBRARY shrink guard gives up and
// accepts the smaller library as the new shape. Derived from the reconcile
// threshold rather than invented (the same derivation shape as the harvest's
// fruitless latch): three times it is 6 consecutive reconciles, and since the
// arr walk runs ONLY on a reconcile and the reconcile interval is a 24h
// constant, a streak here counts DAYS with none of the cadence traps above. So
// the operator gets a WARN on day 1, an ERROR from day 2, and four further days
// of loud alerting before the app stops withholding.
//
// Only the LIBRARY guard has one; see shrinkGuardFactor for why its siblings
// deliberately never auto-accept.
const ShrunkWalkAcceptThreshold = 3 * ReconcileEscalationThreshold

// Advance advances or resets a persisted degradation streak in place and reports
// whether it has reached its escalation threshold. The threshold is a PARAMETER
// so one rule serves both cadences; pass the cadence-named constant that matches
// how often the caller runs.
//
// The reset arm is the half that matters and the half that could drift: a streak
// counts CONSECUTIVE failures, so evidence of success has to zero it.
//
// A caller that observed NEITHER outcome must not call this at all. An
// interrupted or gated pass saw no outage and no recovery, so advancing would
// invent evidence and resetting would discard it; those callers return before
// reaching here. That is why this takes a definite `degraded` bool rather than a
// three-valued state - the do-nothing case is the caller's to recognise, and
// making it representable here would let a caller pass indecision off as data.
func Advance(counter *int, degraded bool, escalateAt int) bool {
	if !degraded {
		*counter = 0
		return false
	}
	*counter++
	return *counter >= escalateAt
}

// shrinkGuardFactor is the shrink guards' trigger fraction, applied only
// through Shrunk (the package's shared surface for it): a refreshed data set
// that would replace the prior one with fewer than 1/shrinkGuardFactor of its
// entries - below half, at the default 2 - is a suspicious truncation rather
// than a real change, so the prior data is kept.
//
// How long it is kept is each consumer's own policy, and the two answers
// deliberately differ. The mapping refresh and the SeaDex catalogue walk NEVER
// auto-accept: an unusable map or a truncated catalogue is not a legitimate end
// state, so accepting one would produce wrong findings indefinitely and the
// remedy stays an operator's (removing state.json). The library walk DOES accept,
// after a bounded, loudly-alerted streak (ShrunkWalkAcceptThreshold),
// because a smaller library IS a legitimate end state the app can serve
// correctly. Do not unify the two.
const shrinkGuardFactor = 2

// Shrunk reports whether a refreshed population of count entries is a
// suspicious truncation of a prior population of prevCount entries: it
// retains less than 1/shrinkGuardFactor of it (below half at the default 2).
// The candidate is multiplied rather than the previous count divided, so an
// odd prevCount never rounds in the guard's favour. It is the single home of
// the shrink comparison every shrink guard shares.
func Shrunk(count, prevCount int) bool {
	return count*shrinkGuardFactor < prevCount
}

// SizeWarnNumerator / SizeWarnDenominator are the pre-cliff warning fraction
// (80%) a persisted-file byte cap warns at: crossing such a cap refuses every
// subsequent write with no self-heal (the offending input never shrinks on its
// own), so the writer warns while there is still headroom to act. Shared by
// internal/state's save guard (stateSizeWarnBytes) and the indexer feed
// snapshot's persist guard (feedSizeWarnBytes); the CAPS themselves stay with
// their own file, only the fraction is app-wide policy. They are constants
// rather than a helper function because both call sites are themselves
// constants.
const (
	SizeWarnNumerator   = 8
	SizeWarnDenominator = 10
)
