// Package degradation is the neutral home of the application-wide
// degradation policy constants shared across domains: the tick-cadence
// escalation threshold the mapping loader's refresh-rejection streak
// escalates its log site at, the shrink guards' trigger
// fraction, applied through Shrunk (mapping refresh + library walk + SeaDex
// catalogue walk), and the pre-cliff warning fraction the persisted-file byte
// caps warn at (state.json, the indexer feed snapshot).
// It is a leaf with no imports so mapping, scout and seadex can reference the
// one policy without either domain owning cross-domain operational policy.
package degradation

// EscalationThreshold is the TICK-cadence consecutive-degraded-cycle streak at
// which a persisted degradation streak escalates its single log site from WARN
// to ERROR (firing the existing SeadexScoutCycleError Loki rule): tolerate 8
// consecutive degraded passes - about 2h at the default 15m poll_interval -
// long enough to ride out a transient upstream or arr oddity, short enough
// that a persistent fault alerts instead of degrading silently forever.
//
// Its one consumer is the mapping loader's refresh-rejection streak
// (mapping.Cache.RejectedRefreshes, read as scout's
// mappingRejectionEscalationThreshold), because loadMapping runs on every
// changed tick as well as every reconcile. The streaks that advance only on a
// RECONCILE - state.State.ShrunkWalksByArr, PartialWalks, SeadexFailures and
// AniListDegraded - escalate at scout's reconcileEscalationThreshold (2, i.e.
// 48h) instead: the count is cadence-relative, so 8 daily reconciles would be
// 8 days. Keep this constant's cadence claim and consumer list in step with
// that block in internal/scout/scout.go.
const EscalationThreshold = 8

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
// after a bounded, loudly-alerted streak (scout's shrunkWalkAcceptThreshold),
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
