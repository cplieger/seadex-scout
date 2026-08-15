// Package align resolves which on-disk release groups a SeaDex entry should be
// compared against (scope) and owns the shared comparison decision over them
// (Decide): file presence before entry state, proven alignment over everything
// group-shaped, unverifiable evidence (release.OverlapUnknown: a NoGroup
// member that could hide the membership being tested) before the mixed and
// diverged claims, mixed only for a not-aligned multi-group unit, and the
// conservative whole-series aggregation in which a proven divergence outranks
// unverifiability and any unverifiable season blocks the best claim.
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
// decision instead of re-deriving it.
type ScopeKind int

const (
	// ScopeWholeSeries is a whole-series comparison: a Sonarr item with no
	// positive Fribb TVDB season and not a special (an absolute-numbered run
	// like One Piece, or a title-only match).
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
// and approximation flag that go with it.
func scope(item *library.Item, rec *mapping.Record) scopeResult {
	// The ARR decides the movie scope, ahead of anything the record says: a
	// Radarr item is a movie even when a broken upstream mapping carries a
	// season for it.
	if item.Arr == library.ArrRadarr {
		return scopeResult{Kind: ScopeMovie, Groups: item.Groups, HasFile: item.HasFile}
	}
	switch kind, season := RecordSeason(rec); kind {
	case ScopeSeason:
		// Group presence doubles as file presence here and in the specials branch:
		// release.Classify falls back to the literal NOGRP for a group-less file.
		g := item.SeasonGroups[season]
		return scopeResult{Kind: ScopeSeason, Groups: g, HasFile: len(g) > 0}
	case ScopeSpecial:
		// a special: compare against the season-0 specials bucket
		g := item.SeasonGroups[season]
		return scopeResult{Kind: ScopeSpecial, Groups: g, HasFile: len(g) > 0, Approx: len(g) > 1}
	default:
		// Everything left is a whole-series comparison with no single-unit scope;
		// Decide resolves it by conservative per-real-season aggregation.
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
// The type owns its own encoding deliberately.
func (k ScopeKind) MarshalJSON() ([]byte, error) {
	return json.Marshal(k.String())
}

// UnmarshalJSON reads the String() vocabulary back.
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
// whole-series comparison.
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
	// indeterminate, which blocks the whole-series best claim without proving a downgrade.
	AnyUnverified bool
	// Approx marks the comparison approximate when the aggregate spans more than one
	// season or group, so the single recommendation applies to a coarse aggregate.
	Approx bool
}

// summarizeWholeSeries walks the item's real seasons (season 0 excluded), unions
// their on-disk groups (sorted, deduped), and classifies each filed season
// under the three-valued release.GroupsOverlap - proven best, unverifiable,
// proven alt, or unlisted - so wholeSeriesStanding can pick the most
// conservative whole-series standing (proven downgrades outrank
// unverifiability; any unverifiable season blocks the best claim).
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
