// Package align resolves which on-disk release groups a SeaDex entry should be
// compared against (scope) and owns the shared comparison decision over them
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
	"encoding/json"
	"fmt"
	"slices"

	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
)

// specialSeason is the TVDB season number Sonarr files specials under.
const specialSeason = 0

// ScopeKind names the semantic comparison scope resolved for an item: which
// branch of the movie / season / special / whole-series dispatch fired.
// It travels with the resolved groups in scopeResult so consumers (compare's
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

// scopeResult is the single scoping decision returned by scope: the semantic
// Kind, the on-disk release groups to compare against, whether the scoped unit
// has any file on disk, and whether the comparison is approximate (the
// season-0 specials bucket held more than one group).
type scopeResult struct {
	Groups  []string
	Kind    ScopeKind
	HasFile bool
	Approx  bool
}

// RecordSeason resolves, from a Fribb record ALONE, which season the record
// pins and which scope kind pins it: ScopeSeason with its positive Fribb TVDB
// season, ScopeSpecial with the season-0 bucket a Fribb-typed special is filed
// under (a MAPPED season zero, not an absent one), or ScopeWholeSeries with no
// season (an absolute-numbered run, a title-only match, or a record with no
// Fribb typing at all).
//
// This is the ONE home of the season-resolution rule: scope's own season and
// special arms read it, and so does the feed's per-entry metadata
// (internal/scout), which must hand the indexer a RESOLVED season because that
// package deliberately imports neither align nor mapping and so has no
// legitimate way to re-interpret Fribb semantics (l-f4). Held as two copies -
// with a private specialSeason constant each - the dispatch drifted with no
// compile error and no test spanning both (l-f6/l-f132).
//
// The MOVIE arm is deliberately NOT here, because the two callers answer it
// from different evidence: scope keys it on the ARR the item lives on (a Radarr
// item is a movie whatever its record says) while the feed keys it on the
// record's own type, and folding either rule in here would silently impose it
// on the other caller. The runner-up home was a mapping.Record method
// (Record.MappedSeason), declined in the l-f4 wave because it would give
// internal/indexer a second way to ask the same question; align is the leaf
// that already owns scope resolution, and both callers sit above it.
func RecordSeason(rec *mapping.Record) (kind ScopeKind, season int) {
	switch {
	case rec.HasMappedSeason():
		return ScopeSeason, rec.SeasonTvdb
	case rec.IsSpecial():
		return ScopeSpecial, specialSeason
	default:
		return ScopeWholeSeries, 0
	}
}

// scope resolves the comparison scope of a matched entry once, for every
// consumer: the semantic Kind plus the on-disk release groups, file presence,
// and approximation flag that go with it. It handles the three single-unit
// scopes: a movie (the movie's groups), a series with a positive Fribb TVDB
// season (that season's groups, exact), and a special (the season-0 bucket
// Sonarr lumps specials into, approximate when it holds more than one group).
//
// A Sonarr series with no positive Fribb season and not a special has no
// single-unit scope: scope classifies it as ScopeWholeSeries (nil groups) and
// Decide resolves it with the conservative per-real-season aggregation, so a
// consumer cannot silently mis-scope such an item against the specials bucket.
func scope(item *library.Item, rec *mapping.Record) scopeResult {
	// The ARR decides the movie scope, ahead of anything the record says: a
	// Radarr item is a movie even when a broken upstream mapping carries a
	// season for it.
	if item.Arr == library.ArrRadarr {
		return scopeResult{Kind: ScopeMovie, Groups: item.Groups, HasFile: item.HasFile}
	}
	switch kind, season := RecordSeason(rec); kind {
	case ScopeSeason:
		// Group presence doubles as file presence here and in the specials
		// branch below: release.Classify falls back to the literal NOGRP
		// (release.NoGroup) for a group-less file, so a season with any file
		// on disk always carries at least one group member - possibly the
		// unknown-evidence sentinel, which the decision layer treats as
		// unverifiable, never as an identity.
		g := item.SeasonGroups[season]
		return scopeResult{Kind: ScopeSeason, Groups: g, HasFile: len(g) > 0}
	case ScopeSpecial:
		// a special: compare against the season-0 specials bucket
		g := item.SeasonGroups[season]
		return scopeResult{Kind: ScopeSpecial, Groups: g, HasFile: len(g) > 0, Approx: len(g) > 1}
	default:
		// Everything left is a whole-series comparison (a Sonarr
		// absolute-numbered run or a title-only match): it has no single-unit
		// scope, and Decide resolves it with the conservative per-real-season
		// aggregation. ScopeMovie is unreachable here - RecordSeason never
		// returns it - and a non-Radarr item could not use it anyway.
		return scopeResult{Kind: ScopeWholeSeries}
	}
}

// String names the scope kind for an operator-facing label: "movie",
// "season", "special", or "series" for a whole-series comparison. It is the
// one home of that vocabulary, shared by the daemon's finding line and the
// audit report's scope cell (which adds the season NUMBER for ScopeSeason).
func (k ScopeKind) String() string {
	switch k {
	case ScopeMovie:
		return "movie"
	case ScopeSeason:
		return "season"
	case ScopeSpecial:
		return "special"
	default:
		return "series"
	}
}

// MarshalJSON encodes the kind as its String() name, so a machine-readable
// consumer reads the same vocabulary a human does ("season", "movie", "special",
// "series") instead of an integer whose meaning is this file's iota order.
//
// The type owns its own encoding deliberately. The alternative was for a consumer
// to carry a second, stringly-typed copy of the same fact beside the typed one,
// which is exactly the split that kept this value off the audit report's wire
// shape in the first place (l-f18): two of three renderers could read the typed
// field and the JSON could not, so the JSON's consumer had to re-derive the scope
// from other keys - and that derivation IS this package's dispatch, re-implemented
// elsewhere. This app has already paid for that class of drift once, when
// internal/indexer re-derived the season rule from raw Fribb fields (l-f4).
func (k ScopeKind) MarshalJSON() ([]byte, error) {
	return json.Marshal(k.String())
}

// UnmarshalJSON reads the String() vocabulary back. It exists because publishing a
// custom encoding without its inverse breaks round-tripping SILENTLY for the
// encoding/json caller who has no reason to expect asymmetry - the audit package's
// own render tests decode a rendered report back into its struct as a
// completeness check, and would have failed on a type error rather than on
// anything they were written to detect.
//
// An unrecognized token is an error rather than the ScopeWholeSeries zero value:
// String() maps every unknown kind TO "series", so silently accepting one would
// turn a future vocabulary this build does not know into a confident wrong scope.
func (k *ScopeKind) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return err
	}
	for _, candidate := range []ScopeKind{ScopeWholeSeries, ScopeMovie, ScopeSeason, ScopeSpecial} {
		if candidate.String() == name {
			*k = candidate
			return nil
		}
	}
	return fmt.Errorf("unknown scope kind %q", name)
}

// ItemKind resolves the comparison scope kind of a library item that has no
// SeaDex-associated Fribb record (an item enumerated by the audit's reverse
// catalogue, not matched to a SeaDex entry): a Radarr movie scopes to the
// movie, a Sonarr series has no per-season mapping and reads as the
// whole-series comparison. It delegates to scope so the scope dispatch stays
// single-homed - a caller must never synthesize an empty mapping.Record to
// reach this classification.
func ItemKind(item *library.Item) ScopeKind {
	return scope(item, &mapping.Record{}).Kind
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
	// the same coarseness rule as scopeResult.Approx (the single whole-series
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
		switch groupStanding(groups, best, alt) {
		case StandingAlt:
			s.AnyAlt = true
		case StandingUnlisted:
			s.AnyUnlisted = true
		case StandingUnverified:
			s.AnyUnverified = true
		case StandingNoFile, StandingBest:
			// a season provenly carrying a best group sets no flag (and
			// NoFile is unreachable: the loop skips empty seasons)
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
