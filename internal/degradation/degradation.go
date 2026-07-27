// Package degradation is the neutral home of the application-wide
// degradation policy constants shared across domains: the escalation
// threshold the persisted degradation streaks (mapping refresh rejections,
// shrunk library walks, partial library walks, SeaDex fetch failures, AniList
// degradation) escalate their log sites at, and the shrink guards' trigger
// fraction (mapping refresh + library walk + SeaDex catalogue walk).
// It is a leaf with no imports so mapping, scout and seadex can reference the
// one policy without either domain owning cross-domain operational policy.
package degradation

// EscalationThreshold is the consecutive-degraded-cycle streak at which a
// persisted degradation streak escalates its single log site from WARN to
// ERROR (firing the existing SeadexScoutCycleError Loki rule): tolerate 8
// consecutive degraded cycles, about a day at the default 3h cadence, before
// escalating - long enough to ride out a transient upstream or arr oddity,
// short enough that a persistent fault alerts instead of degrading silently
// forever. Shared by the mapping loader's refresh-rejection streak
// (mapping.Cache.RejectedRefreshes), the scout's shrunk-walk streak
// (state.State.ShrunkWalks), the scout's partial-walk streak
// (state.State.PartialWalks), the scout's SeaDex fetch-failure streak
// (state.State.SeadexFailures), and the scout's AniList-degradation streak
// (state.State.AniListDegraded).
const EscalationThreshold = 8

// ShrinkGuardFactor is the shrink guards' trigger fraction: a refreshed data
// set that would replace the prior one with fewer than 1/ShrinkGuardFactor of
// its entries - below half, at the default 2 - is treated as a suspicious
// truncation rather than a real change, keeping the prior data and never
// auto-accepting. Shared by the mapping loader's refresh shrink guard
// (acceptRefresh), the scout's library shrink guard, and the SeaDex client's
// catalogue-walk shortfall guard (which errors the fetch below half).
const ShrinkGuardFactor = 2

// Shrunk reports whether a refreshed population of count entries is a
// suspicious truncation of a prior population of prevCount entries: it
// retains less than 1/ShrinkGuardFactor of it (below half at the default 2).
// The candidate is multiplied rather than the previous count divided, so an
// odd prevCount never rounds in the guard's favour. It is the single home of
// the shrink comparison the mapping loader's refresh guards and the scout's
// library walk guard share.
func Shrunk(count, prevCount int) bool {
	return count*ShrinkGuardFactor < prevCount
}
