// Package align resolves which on-disk release groups a SeaDex entry should be
// compared against (Scope) and owns the shared comparison decision over them
// (Decide): file presence before entry state, proven alignment over everything
// group-shaped, unverifiable evidence (release.OverlapUnknown: a NoGroup
// member that could hide the membership being tested) before the mixed and
// diverged claims, mixed only for a not-aligned multi-group unit, and the
// conservative whole-series aggregation in which a proven divergence outranks
// unverifiability and any unverifiable season blocks the best claim. It is the
// single source of truth for both, consumed by BOTH the daemon's compare pass
// (internal/compare) and the audit report (internal/audit) so the two never
// disagree about the same title - each consumer only projects the one decision
// into its own vocabulary.
//
// It stays a thin, library-aware leaf: it depends only on library, mapping, and
// the pure release classifier - never on seadex, match, or the consumers - so it
// can be shared without a dependency cycle. (It is a separate package rather than
// living in internal/release because release is deliberately pure and imports no
// library/mapping types.)
package align

import (
	"slices"

	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/release"
)

// specialSeason is the TVDB season number Sonarr files specials under.
const specialSeason = 0

// ScopeKind names the semantic comparison scope Scope resolved for an item:
// which branch of the movie / season / special / whole-series dispatch fired.
// It travels with the resolved groups in ScopeResult so consumers (compare's
// findings and audit's rendered Scope column) branch and label from the one
// decision instead of re-deriving it. ScopeWholeSeries is the zero value, so
// an unset kind reads as the conservative whole-series label.
type ScopeKind int

const (
	// ScopeWholeSeries is a whole-series comparison: a Sonarr item with no
	// positive Fribb TVDB season and not a special (an absolute-numbered run
	// like One Piece, or a title-only match). It has no single-unit scope;
	// Decide resolves it with the conservative per-real-season aggregation.
	ScopeWholeSeries ScopeKind = iota
	// ScopeMovie is a Radarr movie compared against its own groups.
	ScopeMovie
	// ScopeSeason is a series scoped to a positive Fribb TVDB season (exact).
	ScopeSeason
	// ScopeSpecial is a special compared against the season-0 bucket Sonarr
	// lumps specials into.
	ScopeSpecial
)

// ScopeResult is the single scoping decision returned by Scope: the semantic
// Kind, the on-disk release groups to compare against, whether the scoped unit
// has any file on disk, and whether the comparison is approximate (the
// season-0 specials bucket held more than one group).
type ScopeResult struct {
	Groups  []string
	Kind    ScopeKind
	HasFile bool
	Approx  bool
}

// Scope resolves the comparison scope of a matched entry once, for every
// consumer: the semantic Kind plus the on-disk release groups, file presence,
// and approximation flag that go with it. It handles the three single-unit
// scopes: a movie (the movie's groups), a series with a positive Fribb TVDB
// season (that season's groups, exact), and a special (the season-0 bucket
// Sonarr lumps specials into, approximate when it holds more than one group).
//
// A Sonarr series with no positive Fribb season and not a special has no
// single-unit scope: Scope classifies it as ScopeWholeSeries (nil groups) and
// Decide resolves it with the conservative per-real-season aggregation, so a
// consumer cannot silently mis-scope such an item against the specials bucket.
func Scope(item *library.Item, rec *mapping.Record) ScopeResult {
	switch {
	case item.Arr == library.ArrRadarr:
		return ScopeResult{Kind: ScopeMovie, Groups: item.Groups, HasFile: item.HasFile}
	case rec.SeasonTvdb > 0:
		// Group presence doubles as file presence here and in the specials
		// branch below: release.Classify falls back to the literal NOGRP
		// (release.NoGroup) for a group-less file, so a season with any file
		// on disk always carries at least one group member - possibly the
		// unknown-evidence sentinel, which the decision layer treats as
		// unverifiable, never as an identity.
		g := item.SeasonGroups[rec.SeasonTvdb]
		return ScopeResult{Kind: ScopeSeason, Groups: g, HasFile: len(g) > 0}
	case wholeSeries(item, rec):
		// A whole-series comparison has no single-unit scope; Decide resolves
		// it with the conservative per-real-season aggregation.
		return ScopeResult{Kind: ScopeWholeSeries}
	default: // a special: compare against the season-0 specials bucket
		g := item.SeasonGroups[specialSeason]
		return ScopeResult{Kind: ScopeSpecial, Groups: g, HasFile: len(g) > 0, Approx: len(g) > 1}
	}
}

// wholeSeries reports whether the item must be compared against the whole series
// rather than a single unit: a Sonarr item with no positive Fribb TVDB season
// and not a special (an absolute-numbered run like One Piece, or a title-only
// match). SeaDex carries one whole-series recommendation for these, with no
// per-season mapping. Consumers read the classification via Scope's Kind.
func wholeSeries(item *library.Item, rec *mapping.Record) bool {
	return item.Arr == library.ArrSonarr && rec.SeasonTvdb <= 0 && !rec.IsSpecial()
}

// summary is the per-real-season aggregate summarizeWholeSeries collects: the
// sorted, deduped union of on-disk groups; how many real seasons (season 0
// excluded) carried files; and whether any of those seasons matched an
// alt-only group, proved unlisted, or was unverifiable (unknown group
// evidence on either side of its comparison).
type summary struct {
	Groups      []string
	Seasons     int
	AnyAlt      bool
	AnyUnlisted bool
	// AnyUnverified marks at least one filed real season whose comparison was
	// indeterminate (release.OverlapUnknown on the best or the alt rung): the
	// season's evidence could hide an alignment or a divergence, so it blocks
	// the whole-series best claim without proving a downgrade.
	AnyUnverified bool
	// Approx marks the comparison approximate when the aggregate spans more
	// than one season or more than one release group: the whole-series arm of
	// the same coarseness rule as ScopeResult.Approx (the single whole-series
	// recommendation then applies to a coarse aggregate).
	Approx bool
}

// summarizeWholeSeries walks the item's real seasons (season 0 excluded), unions
// their on-disk groups (sorted, deduped), and classifies each filed season
// under the three-valued release.GroupsOverlap - proven best, unverifiable,
// proven alt, or unlisted - so wholeSeriesStanding can pick the most
// conservative whole-series standing (proven downgrades outrank
// unverifiability; any unverifiable season blocks the best claim).
//
// A caller that only distinguishes best-vs-not (the daemon's compare pass) passes
// a nil alt: a season provenly lacking a best group then surfaces as
// AnyUnlisted, so "every on-disk season provenly has a best group" is exactly
// "!AnyUnlisted && !AnyUnverified".
func summarizeWholeSeries(item *library.Item, best, alt []string) summary {
	seen := make(map[string]struct{})
	var s summary
	for season, groups := range item.SeasonGroups {
		if season == specialSeason || len(groups) == 0 {
			continue
		}
		s.Seasons++
		s.Groups = appendMissingGroups(s.Groups, seen, groups)
		switch release.GroupsOverlap(groups, best) {
		case release.OverlapKnown:
			continue // this season provenly carries a best group
		case release.OverlapUnknown:
			s.AnyUnverified = true
			continue
		}
		switch release.GroupsOverlap(groups, alt) {
		case release.OverlapKnown:
			s.AnyAlt = true
		case release.OverlapUnknown:
			s.AnyUnverified = true
		default:
			s.AnyUnlisted = true
		}
	}
	slices.Sort(s.Groups)
	s.Approx = s.Seasons > 1 || len(s.Groups) > 1
	return s
}

// appendMissingGroups appends each group not already in seen to out, recording
// it in seen, and returns the grown slice.
func appendMissingGroups(out []string, seen map[string]struct{}, groups []string) []string {
	for _, group := range groups {
		if _, dup := seen[group]; dup {
			continue
		}
		seen[group] = struct{}{}
		out = append(out, group)
	}
	return out
}
