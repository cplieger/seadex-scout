// Package degradation is the neutral home of the application-wide
// degradation policy: the two CADENCE-NAMED escalation thresholds a persisted
// degradation streak escalates its log site at, the transition rule those
// streaks advance and reset by (Advance), the library shrink guard's bounded
// acceptance threshold, the shrink guards' trigger fraction applied through
// Shrunk (mapping refresh + library walk + SeaDex catalogue walk), and the
// pre-cliff warning fraction the persisted-file byte caps warn at, applied
// through ApproachingLimit (state.json, the indexer feed snapshot, the Fribb
// download).
package degradation

// TickEscalationThreshold is the TICK-cadence consecutive-degraded-cycle streak
// at which a persisted degradation streak escalates its single log site from
// WARN to ERROR (firing the existing SeadexScoutCycleError Loki rule): tolerate
// 8 consecutive degraded passes - about 2h at the default 15m poll_interval -
// long enough to ride out a transient upstream or arr oddity, short enough that
// a persistent fault alerts instead of degrading silently forever.
const TickEscalationThreshold = 8

// ReconcileEscalationThreshold is the same fleet policy at the RECONCILE's daily
// cadence, for the streaks that advance only on a full pass
// (state.State.ShrunkWalksByArr, PartialWalks, SeadexFailures, AniListDegraded).
const ReconcileEscalationThreshold = 2

// ShrunkWalkAcceptThreshold is when the LIBRARY shrink guard gives up and
// accepts the smaller library as the new shape.
const ShrunkWalkAcceptThreshold = 3 * ReconcileEscalationThreshold

// Advance advances or resets a persisted degradation streak in place and reports
// whether it has reached its escalation threshold. The threshold is a PARAMETER
// so one rule serves both cadences; pass the cadence-named constant that matches
// how often the caller runs.
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
const shrinkGuardFactor = 2

// Shrunk reports whether a refreshed population of count entries is a
// suspicious truncation of a prior population of prevCount entries: it
// retains less than 1/shrinkGuardFactor of it (below half at the default 2).
// The candidate is multiplied rather than the previous count divided, so an
// odd prevCount never rounds in the guard's favour.
func Shrunk(count, prevCount int) bool {
	return count*shrinkGuardFactor < prevCount
}

// sizeWarnNumerator / sizeWarnDenominator are the pre-cliff warning fraction
// (80%) a persisted-file byte cap warns at, applied only through
// ApproachingLimit (the package's shared surface for it): crossing such a cap
// refuses every subsequent write with no self-heal (the offending input never
// shrinks on its own), so the writer warns while there is still headroom to act.
const (
	sizeWarnNumerator   = 8
	sizeWarnDenominator = 10
)

// ApproachingLimit reports whether a payload of size bytes has reached the
// pre-cliff warning fraction of limit (80% at the default), the point a
// persisted-file byte cap's writer starts warning that the next growth may
// cross it and freeze the file.
//
// The comparison is INCLUSIVE: the fraction is where the warning STARTS, not the
// last silent byte, so a payload landing exactly on it warns. The limit is
// divided before it is multiplied, so the threshold truncates DOWN rather than
// up - 13421768 for a 16 MiB limit, not the 13421772 the other operation order
// gives.
func ApproachingLimit(size, limit int64) bool {
	return size >= limit/sizeWarnDenominator*sizeWarnNumerator
}
