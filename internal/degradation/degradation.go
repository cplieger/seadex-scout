// Package degradation is the neutral home of the application-wide
// degradation policy constants shared across domains: the escalation
// threshold the persisted degradation streaks (mapping refresh rejections,
// shrunk library walks, SeaDex fetch failures) escalate their log sites at,
// and the shrink guards' trigger fraction (mapping refresh + library walk).
// It is a leaf with no imports so both mapping and scout can reference the
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
// (state.State.ShrunkWalks), and the scout's SeaDex fetch-failure streak
// (state.State.SeadexFailures).
const EscalationThreshold = 8

// ShrinkGuardFactor is the shrink guards' trigger fraction: a refreshed data
// set that would replace the prior one with fewer than 1/ShrinkGuardFactor of
// its entries - below half, at the default 2 - is treated as a suspicious
// truncation rather than a real change, keeping the prior data and never
// auto-accepting. Shared by the mapping loader's refresh shrink guard
// (acceptRefresh) and the scout's library shrink guard.
const ShrinkGuardFactor = 2
