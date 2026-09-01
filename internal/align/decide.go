package align

import (
	"slices"

	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/release"
)

// Standing is the file-first group-ladder state of the scoped on-disk unit
// against the SeaDex best and alt group sets: file presence is decided before
// anything else, then proven-best, then unverifiable evidence, then alt, then
// unlisted.
type Standing int

const (
	// StandingNoFile means the scoped unit has no file on disk (for a
	// whole-series comparison: no real season carries files).
	StandingNoFile Standing = iota
	// StandingUnverified means the comparison is unverifiable: the
	// release-group evidence on at least one side is unknown (release.NoGroup)
	// and could hide the very membership being tested, so neither alignment
	// nor divergence is proven. Also covers a proven-divergent best comparison
	// whose alt placement is indeterminate (read this as "the verdict cannot
	// be determined", not "nothing is known") and a placeholder whose file
	// state could not be read at all (library.Item.Comparable false).
	StandingUnverified
	// StandingBest means a known best group is proven present on the scoped
	// unit.
	StandingBest
	// StandingAlt means a known alt group is proven present on the scoped
	// unit, the best comparison having proven divergent (all evidence known,
	// no best group on disk).
	StandingAlt
	// StandingUnlisted means every group on both sides is known evidence and
	// the unit's groups match neither prepared set: a proven divergence.
	StandingUnlisted
)

// Outcome is the linearized comparison decision shared by the daemon's
// compare pass and the audit report, in the one branch order both flows
// follow: file presence before the entry state (no file beats the no-best
// nudge), the no-best fallback before any group comparison, proven alignment
// over everything group-shaped, unverifiable evidence before the mixed and
// diverged claims (an unproven comparison must not surface as either), and
// mixed over the single-group divergence.
type Outcome int

const (
	// OutcomeNoFile means there is nothing on disk to judge. The daemon stays
	// silent (report-by-exception); the audit records no_file.
	OutcomeNoFile Outcome = iota
	// OutcomeNoBest means the prepared best set is empty, so there is no
	// group comparison to act on; the entry state (classify.Fallback) decides
	// the nudge each consumer emits.
	OutcomeNoBest
	// OutcomeAligned means a known best group is proven present.
	OutcomeAligned
	// OutcomeUnverifiable means the comparison is indeterminate: unknown group
	// evidence on either side could hide an alignment, so the daemon emits an
	// informational finding and the audit records unverified.
	OutcomeUnverifiable
	// OutcomeMixed means the unit is not aligned and its group evidence spans more
	// than one member, so no single current group can be attributed - a
	// manual-review nudge rather than a false divergence.
	OutcomeMixed
	// OutcomeDiverged means the unit is provenly not aligned with a single
	// attributable group state: the actionable divergence.
	OutcomeDiverged
)

// Decision is the shared comparison decision for one matched (item, record):
// the resolved scope kind, the groups the unit was judged against (the scoped
// set, or the whole-series union), the file-first group-ladder Standing, the
// linearized Outcome, whether the comparison is approximate, and whether the
// prepared best set was empty.
type Decision struct {
	// Groups is the group set the unit was judged against - the scoped set or
	// the whole-series union - and is always owned by the caller: Decide never
	// returns a slice aliasing the library snapshot, whichever branch fired.
	Groups   []string
	Kind     ScopeKind
	Standing Standing
	Outcome  Outcome
	// Season is the shared non-negative TVDB season label both consumers stamp on
	// their output: Record.SeasonTvdb for a ScopeSeason comparison, else 0.
	Season int
	Approx bool
	NoBest bool
}

// Decide resolves the one comparison decision both align consumers project
// their vocabulary from: the daemon's compare pass maps it to Finding/Status
// (internal/compare) and the audit report to Row/Verdict/Qualifier
// (internal/audit).
func Decide(item *library.Item, rec *mapping.Record, best, alt []string) Decision {
	scoped := scope(item, rec)
	d := Decision{Kind: scoped.Kind, NoBest: len(best) == 0}
	if scoped.Kind == ScopeSeason {
		// scope only returns ScopeSeason for rec.HasMappedSeason(), which IS
		// SeasonTvdb > 0, so the label is positive by construction; every
		// other scope leaves it 0.
		d.Season = rec.SeasonTvdb
	}
	switch {
	case !item.Comparable():
		// A placeholder's file state is MISSING, not empty (library.Item.Failed: a
		// series whose episode fetch failed, or a movie Radarr reports a file for
		// while sending no MovieFile payload).
		d.Standing = StandingUnverified
	case scoped.Kind == ScopeWholeSeries:
		// An absolute-numbered run has no per-season Fribb mapping, so its single
		// whole-series recommendation is judged against every real season on disk,
		// conservatively: best only when every filed season provenly carries a best group.
		s := summarizeWholeSeries(item, best, alt)
		d.Groups, d.Approx = s.Groups, s.Approx
		d.Standing = wholeSeriesStanding(s)
	default:
		// Cloned at the edge: scope() takes the single-unit groups verbatim from
		// the library snapshot a concurrent daemon cycle owns and rebuilds, so
		// the exported Decision must not be a window into it.
		d.Groups, d.Approx = slices.Clone(scoped.Groups), scoped.Approx
		d.Standing = unitStanding(scoped.HasFile, scoped.Groups, best, alt)
	}
	d.Outcome = outcomeOf(d.Standing, len(d.Groups), d.NoBest)
	return d
}

// unitStanding derives the group-ladder standing of a single-unit scope (a
// movie, a mapped season, or the season-0 specials bucket): file presence
// first, then the current groups matched against the best then the alt sets
// under the three-valued release.GroupsOverlap.
func unitStanding(hasFile bool, current, best, alt []string) Standing {
	switch {
	case !hasFile:
		return StandingNoFile
	case len(current) == 0:
		return StandingUnverified
	}
	return groupStanding(current, best, alt)
}

// groupStanding is the shared tri-state group ladder over a non-empty filed
// unit's current groups: the best rung first (a proven match wins, an
// unverifiable comparison short-circuits before the alt rung), then the alt
// rung under the same rules, and only an all-known matchless unit is
// unlisted.
func groupStanding(current, best, alt []string) Standing {
	switch release.GroupsOverlap(current, best) {
	case release.OverlapKnown:
		return StandingBest
	case release.OverlapUnknown:
		return StandingUnverified
	}
	switch release.GroupsOverlap(current, alt) {
	case release.OverlapKnown:
		return StandingAlt
	case release.OverlapUnknown:
		return StandingUnverified
	}
	return StandingUnlisted
}

// wholeSeriesStanding collapses the per-real-season aggregate to the most
// conservative standing.
func wholeSeriesStanding(s summary) Standing {
	switch {
	case s.Seasons == 0:
		return StandingNoFile
	case s.AnyUnlisted:
		return StandingUnlisted
	case s.AnyAlt:
		return StandingAlt
	case s.AnyUnverified:
		return StandingUnverified
	default:
		return StandingBest
	}
}

// outcomeOf linearizes a standing into the shared branch order: file presence
// beats the no-best nudge, no-best beats any group comparison, proven
// alignment beats everything group-shaped, an unverifiable comparison beats
// both the mixed nudge and the divergence claim (neither may be asserted on
// unknown evidence), and mixed (a not-aligned unit whose group evidence -
// including the unknown sentinel in a whole-series union - spans more than
// one member) beats the single-group divergence.
func outcomeOf(st Standing, groupCount int, noBest bool) Outcome {
	switch {
	case st == StandingNoFile:
		return OutcomeNoFile
	case noBest:
		return OutcomeNoBest
	case st == StandingBest:
		return OutcomeAligned
	case st == StandingUnverified:
		return OutcomeUnverifiable
	case groupCount > 1 && (st == StandingAlt || st == StandingUnlisted):
		return OutcomeMixed
	case st == StandingAlt || st == StandingUnlisted:
		return OutcomeDiverged
	default:
		// Every Standing the ladder produces is handled above.
		return OutcomeUnverifiable
	}
}
