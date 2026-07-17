package align

import (
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/release"
)

// Standing is the file-first group-ladder state of the scoped on-disk unit
// against the SeaDex best and alt group sets: file presence is decided before
// anything else, then a group-less unit, then best, then alt, then unlisted.
// The audit report renders it 1:1 as the row verdict; the daemon's compare
// pass branches on the linearized Outcome instead.
type Standing int

const (
	// StandingNoFile means the scoped unit has no file on disk (for a
	// whole-series comparison: no real season carries files).
	StandingNoFile Standing = iota
	// StandingUnverified means files are on disk but carry no comparable
	// release group. Defensive: the release.NoGroup fallback makes a filed
	// unit always carry at least one group in practice.
	StandingUnverified
	// StandingBest means a best group is present on the scoped unit.
	StandingBest
	// StandingAlt means an alt group is present on the scoped unit and no
	// best group is.
	StandingAlt
	// StandingUnlisted means the unit's groups match neither prepared set.
	StandingUnlisted
)

// Outcome is the linearized comparison decision shared by the daemon's
// compare pass and the audit report, in the one branch order both flows
// follow: file presence before the entry state (no file beats the no-best
// nudge), the no-best fallback before any group comparison, alignment over
// the mixed-group nudge, and mixed over the single-group divergence.
type Outcome int

const (
	// OutcomeNoFile means there is nothing on disk to judge. The daemon stays
	// silent (report-by-exception); the audit records no_file.
	OutcomeNoFile Outcome = iota
	// OutcomeNoBest means the prepared best set is empty, so there is no
	// group comparison to act on; the entry state (classify.Fallback) decides
	// the nudge each consumer emits.
	OutcomeNoBest
	// OutcomeAligned means a best group is already present. Alignment wins no
	// matter how many groups the unit spans: the daemon stays silent and the
	// audit row is unqualified.
	OutcomeAligned
	// OutcomeMixed means the unit is not aligned and spans more than one
	// group, so no single current group can be attributed - a manual-review
	// nudge rather than a false divergence.
	OutcomeMixed
	// OutcomeDiverged means the unit is not aligned with a single
	// attributable group state: the actionable divergence (the daemon's
	// better_release, downgraded by both consumers when the entry is
	// incomplete).
	OutcomeDiverged
)

// Decision is the shared comparison decision for one matched (item, record):
// the resolved scope kind, the groups the unit was judged against (the scoped
// set, or the whole-series union), the file-first group-ladder Standing, the
// linearized Outcome, whether the comparison is approximate, and whether the
// prepared best set was empty. NoBest is exposed independently of Outcome
// because the audit annotates the entry state even on a no-file row the
// daemon silences (file presence wins the Outcome linearization).
type Decision struct {
	Groups   []string
	Kind     ScopeKind
	Standing Standing
	Outcome  Outcome
	Approx   bool
	NoBest   bool
}

// Decide resolves the one comparison decision both align consumers project
// their vocabulary from: the daemon's compare pass maps it to Finding/Status
// (internal/compare) and the audit report to Row/Verdict/Qualifier
// (internal/audit). The callers deliberately prepare DIFFERENT inputs - the
// daemon feeds its filtered obtainable recommendations as best with a nil alt
// (so a unit lacking a recommended group reads unlisted), while the report
// feeds the raw SeaDex best and alt sets - and Decide unifies the branch
// order and decision rules over those inputs, so the two flows cannot drift
// apart on the same title.
func Decide(item *library.Item, rec *mapping.Record, best, alt []string) Decision {
	scoped := Scope(item, rec)
	d := Decision{Kind: scoped.Kind, NoBest: len(best) == 0}
	if scoped.Kind == ScopeWholeSeries {
		// An absolute-numbered run / title-only match has no per-season Fribb
		// mapping: its single whole-series recommendation is judged against
		// every real season on disk, conservatively - best only when every
		// filed season carries a best group - so a later season that needs a
		// better release is not masked by an earlier season that has it.
		s := summarizeWholeSeries(item, best, alt)
		d.Groups, d.Approx = s.Groups, s.Approx
		d.Standing = wholeSeriesStanding(s)
	} else {
		d.Groups, d.Approx = scoped.Groups, scoped.Approx
		d.Standing = unitStanding(scoped.HasFile, scoped.Groups, best, alt)
	}
	d.Outcome = outcomeOf(d.Standing, len(d.Groups), d.NoBest)
	return d
}

// unitStanding derives the group-ladder standing of a single-unit scope (a
// movie, a mapped season, or the season-0 specials bucket): file presence
// first, then the current groups matched against the best then the alt sets.
func unitStanding(hasFile bool, current, best, alt []string) Standing {
	switch {
	case !hasFile:
		return StandingNoFile
	case len(current) == 0:
		return StandingUnverified
	case release.GroupsIntersect(current, best):
		return StandingBest
	case release.GroupsIntersect(current, alt):
		return StandingAlt
	default:
		return StandingUnlisted
	}
}

// wholeSeriesStanding collapses the per-real-season aggregate to the most
// conservative standing: no filed real season is no-file, any unlisted season
// downgrades the whole series to unlisted, any alt-only season to alt, and
// best requires every filed season to carry a best group. (With a nil alt -
// the daemon's inputs - a season lacking a best group reads unlisted, so
// "aligned" is exactly "no season is unlisted".)
func wholeSeriesStanding(s summary) Standing {
	switch {
	case s.Seasons == 0:
		return StandingNoFile
	case s.AnyUnlisted:
		return StandingUnlisted
	case s.AnyAlt:
		return StandingAlt
	default:
		return StandingBest
	}
}

// outcomeOf linearizes a standing into the shared branch order: file presence
// beats the no-best nudge, no-best beats any group comparison, alignment
// beats the mixed nudge, and mixed (a not-aligned unit spanning more than one
// group) beats the single-group divergence. A group-less StandingUnverified
// unit falls to OutcomeDiverged, never OutcomeMixed.
func outcomeOf(st Standing, groupCount int, noBest bool) Outcome {
	switch {
	case st == StandingNoFile:
		return OutcomeNoFile
	case noBest:
		return OutcomeNoBest
	case st == StandingBest:
		return OutcomeAligned
	case groupCount > 1 && (st == StandingAlt || st == StandingUnlisted):
		return OutcomeMixed
	default:
		return OutcomeDiverged
	}
}
