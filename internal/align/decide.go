package align

import (
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/release"
)

// Standing is the file-first group-ladder state of the scoped on-disk unit
// against the SeaDex best and alt group sets: file presence is decided before
// anything else, then proven-best, then unverifiable evidence, then alt, then
// unlisted. Best and alt require PROVEN membership (a known on-disk group
// matching a known recommended group, release.OverlapKnown); when the group
// evidence on either side is unknown (the release.NoGroup sentinel) and could
// hide such a match, the unit is unverified rather than confidently placed.
// The audit report renders the standing 1:1 as the row verdict; the daemon's
// compare pass branches on the linearized Outcome instead.
type Standing int

const (
	// StandingNoFile means the scoped unit has no file on disk (for a
	// whole-series comparison: no real season carries files).
	StandingNoFile Standing = iota
	// StandingUnverified means files are on disk but the comparison is
	// unverifiable: the release-group evidence on at least one side is
	// unknown (a group-less on-disk file or a group-less SeaDex release,
	// both carried as the release.NoGroup sentinel) and could hide the very
	// membership being tested, so neither alignment nor divergence is
	// proven. It also covers the defensive case of a filed unit carrying no
	// group list at all.
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
	// OutcomeAligned means a known best group is proven present. Alignment
	// wins no matter how many groups the unit spans or what unknown members
	// ride along: the daemon stays silent and the audit row is unqualified.
	OutcomeAligned
	// OutcomeUnverifiable means the comparison is indeterminate
	// (StandingUnverified): unknown group evidence on either side could hide
	// an alignment, so the daemon emits an informational unverifiable finding
	// instead of a confident aligned silence or a better-release warning, and
	// the audit records unverified.
	OutcomeUnverifiable
	// OutcomeMixed means the unit is not aligned and its known evidence spans
	// more than one group, so no single current group can be attributed - a
	// manual-review nudge rather than a false divergence.
	OutcomeMixed
	// OutcomeDiverged means the unit is provenly not aligned with a single
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
		// filed season provenly carries a best group - so a later season that
		// needs a better release is not masked by an earlier season that has
		// it, and a season with unknown evidence cannot make the whole series
		// read best.
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
// first, then the current groups matched against the best then the alt sets
// under the three-valued release.GroupsOverlap. A proven best match wins; an
// unverifiable best comparison short-circuits to unverified BEFORE the alt
// rung (when "do you have the best?" is unanswerable, no alt placement may
// imply you lack it); a proven-divergent best comparison falls to the alt
// rung under the same rules; and only an all-known, matchless unit is
// unlisted. A filed unit with no group list at all is unverified defensively.
func unitStanding(hasFile bool, current, best, alt []string) Standing {
	switch {
	case !hasFile:
		return StandingNoFile
	case len(current) == 0:
		return StandingUnverified
	}
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
// conservative standing. The aggregation rule, in order: no filed real season
// is no-file; any provenly-unlisted season downgrades the whole series to
// unlisted and any proven alt-only season to alt (a proven divergence
// somewhere stands regardless of unknown evidence elsewhere - the
// recommendation is actionable either way); otherwise any season with
// unverifiable evidence makes the series unverified (an unknown season could
// hide a divergence, so it blocks the best claim without being able to prove
// a downgrade); and best requires every filed season to provenly carry a
// best group. (With a nil alt - the daemon's inputs - a season lacking a
// best group reads unlisted, so "aligned" is exactly "no season is unlisted
// or unverifiable".)
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
// unknown evidence), and mixed (a not-aligned unit whose known evidence spans
// more than one group) beats the single-group divergence.
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
	default:
		return OutcomeDiverged
	}
}
