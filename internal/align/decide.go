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
	// release-group evidence on exactly one side is unknown (release.NoGroup)
	// and could hide the very membership being tested, so neither alignment
	// nor divergence is proven. Also covers a proven-divergent best comparison
	// whose alt placement is indeterminate (read this as "the verdict cannot
	// be determined", not "nothing is known") and a placeholder whose file
	// state could not be read at all (library.Item.Comparable false).
	StandingUnverified
	// StandingBest means a known best group is proven present on the scoped
	// unit at a revision at least as new as the one SeaDex lists, or with
	// revision evidence missing on either side.
	StandingBest
	// StandingBestSuperseded means a known best group is proven present, but
	// for every best group the unit holds, the newest revision held is provably
	// older than the newest one SeaDex lists for it (release.RevisionBehind): an
	// upgrade within the same group.
	StandingBestSuperseded
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
	// OutcomeSuperseded means the unit holds a best group only at an older
	// revision than SeaDex lists: the actionable same-group upgrade.
	OutcomeSuperseded
	// OutcomeUnverifiable means the comparison is indeterminate: unknown group
	// evidence on one side could hide an alignment, so the daemon emits an
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

// Listing is the SeaDex side of one comparison.
type Listing struct {
	// BestRevisions is the NEWEST revision SeaDex lists for each normalized
	// best group (classify.BestRevisions). A group missing from it reads as an
	// unknown revision, which never claims an upgrade.
	BestRevisions map[string]release.Revision
	Best          []string
	Alt           []string
}

// Decision is the shared comparison decision for one matched (item, record):
// the resolved scope kind, the groups the unit was judged against (the scoped
// set, or the whole-series union), the file-first group-ladder Standing, the
// linearized Outcome, whether the comparison is approximate, and whether the
// prepared best set was empty.
type Decision struct {
	// Groups is the group set the unit was judged against - the scoped set or
	// the whole-series union - and is always owned by the caller: Decide never
	// returns a slice aliasing the library snapshot, whichever branch fired.
	Groups []string
	// SupersededGroups, HeldRevision and ListedRevision are set only for
	// StandingBestSuperseded: the sorted normalized best groups held at an older
	// revision, the NEWEST revision held across them, and the NEWEST revision
	// SeaDex lists across them.
	SupersededGroups []string
	HeldRevision     release.Revision
	ListedRevision   release.Revision
	Kind             ScopeKind
	Standing         Standing
	Outcome          Outcome
	// Season is the shared non-negative TVDB season label both consumers stamp on
	// their output: Record.SeasonTvdb for a ScopeSeason comparison, else 0.
	Season int
	Approx bool
	NoBest bool
}

// Decide resolves the one comparison decision both align consumers project
// their vocabulary from: the daemon's compare pass maps it to Finding/Status
// (internal/compare) and the audit report to Row/Verdict/Qualifier
// (internal/audit). siblingSeasons and seasons both bound a whole-series
// comparison to the entry's own seasons, from two sources: seasons (the entry's
// TVDB season ranges from the mapping list) wins when present, else
// the seasons sibling records map are dropped. Every other scope ignores both.
func Decide(item *library.Item, rec *mapping.Record, listing *Listing, siblingSeasons []int, seasons []mapping.SeasonRange) Decision {
	scoped := scope(item, rec)
	d := Decision{Kind: scoped.Kind, NoBest: len(listing.Best) == 0}
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
	case scoped.Kind == ScopeOffered:
		d.Groups, d.Approx = slices.Clone(scoped.Groups), scoped.Approx
		// HasFile keeps precedence over the kind: an EMPTY bucket proves absence
		// without attributing anything, so no_file stays the honest answer where
		// unverified would throw that proof away.
		if !scoped.HasFile {
			d.Standing = StandingNoFile
			break
		}
		d.Standing = StandingUnverified
	case scoped.Kind == ScopeWholeSeries:
		// An absolute-numbered run has no per-season mapping, so its single
		// whole-series recommendation is judged against every real season on disk,
		// conservatively: best only when every filed season provenly carries a best group.
		s := summarizeWholeSeries(item, listing, siblingSeasons, seasons)
		d.Groups, d.Approx = s.Groups, s.Approx
		d.Standing = wholeSeriesStanding(&s)
		if d.Standing == StandingBestSuperseded {
			d.setSuperseded(s.superseded, s.held, listing)
		}
	default:
		// Cloned at the edge: scope() takes the single-unit groups verbatim from
		// the library snapshot a concurrent daemon cycle owns and rebuilds, so
		// the exported Decision must not be a window into it.
		d.Groups, d.Approx = slices.Clone(scoped.Groups), scoped.Approx
		var behind []string
		d.Standing, behind = unitStanding(&scoped, listing)
		if d.Standing == StandingBestSuperseded {
			d.setSuperseded(behind, heldReadings(behind, scoped.Revisions), listing)
		}
	}
	d.Outcome = outcomeOf(d.Standing, len(d.Groups), d.NoBest)
	return d
}

// setSuperseded records which groups are behind and the two revisions the
// claim compares.
func (d *Decision) setSuperseded(groups []string, held []release.Revision, listing *Listing) {
	d.SupersededGroups = slices.Sorted(slices.Values(groups))
	d.HeldRevision = release.NewestRevision(held...)
	listed := make([]release.Revision, len(d.SupersededGroups))
	for i, group := range d.SupersededGroups {
		listed[i] = listing.BestRevisions[group]
	}
	d.ListedRevision = release.NewestRevision(listed...)
}

// heldReadings returns the held reading of each normalized group.
func heldReadings(groups []string, held map[string]release.Revision) []release.Revision {
	out := make([]release.Revision, len(groups))
	for i, group := range groups {
		out[i] = held[group]
	}
	return out
}

// unitStanding derives the group-ladder standing of a single-unit scope (a movie
// or a mapped season; the offered kind takes its own arm in Decide ahead of
// this): file presence first, then the current groups matched against the best
// then the alt sets under the three-valued release.GroupsOverlap. It also
// returns the superseded groups when the standing is StandingBestSuperseded.
func unitStanding(scoped *scopeResult, listing *Listing) (standing Standing, behind []string) {
	switch {
	case !scoped.HasFile:
		return StandingNoFile, nil
	case len(scoped.Groups) == 0:
		return StandingUnverified, nil
	}
	return groupStanding(scoped.Groups, scoped.Revisions, listing)
}

// groupStanding is groupLadder plus the revision check: a proven best match
// is superseded only when every held best group is behind (supersededGroups),
// and the behind groups come back with it.
func groupStanding(current []string, held map[string]release.Revision, listing *Listing) (standing Standing, behind []string) {
	standing = groupLadder(current, listing)
	if standing != StandingBest {
		return standing, nil
	}
	if behind = supersededGroups(current, held, listing); len(behind) > 0 {
		return StandingBestSuperseded, behind
	}
	return StandingBest, nil
}

// groupLadder is the shared tri-state group ladder over a non-empty filed
// unit's current groups, revisions aside: the best rung first (a proven match
// wins, an unverifiable comparison short-circuits before the alt rung), then
// the alt rung under the same rules, and only an all-known matchless unit is
// unlisted.
func groupLadder(current []string, listing *Listing) Standing {
	switch release.GroupsOverlap(current, listing.Best) {
	case release.OverlapKnown:
		return StandingBest
	case release.OverlapUnknown:
		return StandingUnverified
	}
	switch release.GroupsOverlap(current, listing.Alt) {
	case release.OverlapKnown:
		return StandingAlt
	case release.OverlapUnknown:
		return StandingUnverified
	}
	return StandingUnlisted
}

// supersededGroups returns the normalized best groups the unit holds when EVERY
// one of them is provably an older revision than SeaDex lists, else nil. One
// current best group keeps the unit aligned, and an unlisted group riding along
// does not block the claim: the attributable group is known. NoGroup is never
// a best group here.
func supersededGroups(current []string, held map[string]release.Revision, listing *Listing) []string {
	best := make(map[string]struct{}, len(listing.Best))
	for _, group := range listing.Best {
		best[release.NormalizeGroup(group)] = struct{}{}
	}
	delete(best, release.NormalizeGroup(release.NoGroup))
	var behind []string
	for _, group := range current {
		normalized := release.NormalizeGroup(group)
		if _, ok := best[normalized]; !ok || slices.Contains(behind, normalized) {
			continue
		}
		if !release.RevisionBehind(held[normalized], listing.BestRevisions[normalized]) {
			return nil
		}
		behind = append(behind, normalized)
	}
	return behind
}

// wholeSeriesStanding collapses the per-real-season aggregate to the most
// conservative standing.
func wholeSeriesStanding(s *summary) Standing {
	switch {
	case s.Seasons == 0:
		return StandingNoFile
	case s.AnyUnlisted:
		return StandingUnlisted
	case s.AnyAlt:
		return StandingAlt
	case len(s.superseded) > 0:
		return StandingBestSuperseded
	case s.AnyUnverified:
		return StandingUnverified
	default:
		return StandingBest
	}
}

// outcomeOf linearizes a standing into the shared branch order: file presence
// beats the no-best nudge, no-best beats any group comparison, proven
// alignment beats everything group-shaped, a proven same-group revision gap
// comes next (its group is attributable), an unverifiable comparison beats
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
	case st == StandingBestSuperseded:
		return OutcomeSuperseded
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
