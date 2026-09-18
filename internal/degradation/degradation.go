// Package degradation is the neutral home of the application-wide degradation
// policy: the cadence-named escalation thresholds, the streak transition rule,
// and the fractions the shrink and size guards trigger at.
package degradation

// TickEscalationThreshold is the TICK-cadence consecutive-degraded-cycle streak
// at which a persisted streak escalates its log site from WARN to ERROR (firing
// the SeadexScoutCycleError Loki rule). 8 passes is about 2h at the default 15m
// poll_interval: long enough to ride out a transient upstream or arr oddity,
// short enough that a persistent fault alerts instead of degrading forever.
const TickEscalationThreshold = 8

// ReconcileEscalationThreshold is the same policy at the RECONCILE's daily
// cadence, for the streaks that advance only on a full pass.
const ReconcileEscalationThreshold = 2

// ShrunkWalkAcceptThreshold is when the LIBRARY shrink guard gives up and
// accepts the smaller library as the new shape.
const ShrunkWalkAcceptThreshold = 3 * ReconcileEscalationThreshold

// Advance advances or resets a persisted degradation streak in place and reports
// whether it has reached its escalation threshold. The threshold is a PARAMETER so
// one rule serves both cadences: pass the constant matching the caller's cadence.
func Advance(counter *int, degraded bool, escalateAt int) bool {
	if !degraded {
		*counter = 0
		return false
	}
	*counter++
	return *counter >= escalateAt
}

// shrinkGuardFactor is the shrink guards' trigger fraction: a refresh retaining
// fewer than 1/shrinkGuardFactor of the prior entries - below half at 2 - is
// read as a suspicious truncation rather than a real change, so the prior data
// is kept.
const shrinkGuardFactor = 2

// Shrunk reports whether a refreshed population of count entries is a suspicious
// truncation of a prior population of prevCount. The candidate is multiplied
// rather than prevCount divided, so an odd prevCount never rounds in the guard's
// favour.
func Shrunk(count, prevCount int) bool {
	return count*shrinkGuardFactor < prevCount
}

// sizeWarnNumerator / sizeWarnDenominator are the pre-cliff warning fraction
// (80%) a persisted-file byte cap warns at: crossing such a cap refuses every
// subsequent write with no self-heal (the offending input never shrinks on its
// own), so the writer warns while there is still headroom to act.
const (
	sizeWarnNumerator   = 8
	sizeWarnDenominator = 10
)

// ApproachingLimit reports whether a payload of size bytes has reached the
// pre-cliff warning fraction of limit.
//
// The comparison is INCLUSIVE: the fraction is where the warning STARTS, so a
// payload landing exactly on it warns. The limit is divided before it is
// multiplied, so the threshold truncates DOWN - 13421768 for a 16 MiB limit,
// not the 13421772 the other operation order gives.
func ApproachingLimit(size, limit int64) bool {
	return size >= limit/sizeWarnDenominator*sizeWarnNumerator
}
