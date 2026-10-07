// Package align resolves which on-disk release groups a SeaDex entry should be
// compared against (scope) and owns the shared comparison decision over them
// (Decide): file presence before entry state, proven alignment over everything
// group-shaped (unless every held best group is provably an older revision than
// SeaDex lists, which is superseded rather than aligned), unverifiable evidence
// (release.OverlapUnknown: a NoGroup member that could hide the membership
// being tested) before the mixed and diverged claims, mixed only for a
// not-aligned multi-group unit, and the conservative whole-series aggregation
// in which a proven divergence outranks unverifiability and any unverifiable
// season blocks the best claim.
package align

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/release"
)

// specialSeason is the TVDB season number Sonarr files specials under.
const specialSeason = 0

// ScopeKind names the semantic comparison scope resolved for an item: which
// branch of the movie / season / offered / whole-series dispatch fired.
// It travels with the resolved groups in scopeResult so consumers (compare's
// findings and audit's rendered Scope column) branch and label from the one
// decision instead of re-deriving it.
type ScopeKind int

const (
	// ScopeWholeSeries is a whole-series comparison: a Sonarr item with no
	// positive mapped TVDB season and not a special (an absolute-numbered run
	// like One Piece, or a title-only match).
	ScopeWholeSeries ScopeKind = iota
	// ScopeMovie is a Radarr movie compared against its own groups.
	ScopeMovie
	// ScopeSeason is a series scoped to a positive mapped TVDB season (exact).
	ScopeSeason
	// ScopeOffered is a unit the app OFFERS in the feed and does not compare: a
	// film or special whose upstream season is a mapped zero, so it lands in
	// Sonarr's season-0 bucket, which can hold a different work entirely, and
	// the map does not place it on episodes the item lists.
	ScopeOffered
	// ScopeEpisodes is a film or special filed under season 0 that the map
	// places on season-0 episodes the item lists: it is judged on exactly the
	// files on those episodes, like a mapped season.
	ScopeEpisodes
)

// Entry is what one match says about the entry under comparison beyond its
// item: the record, the positive seasons sibling records map
// (mapping.Index.SiblingSeasons), the entry's own season ranges and its
// season-0 episodes (both from mapping.Index.MappingFor). A whole-series
// comparison reads the seasons, the episodes scope reads Specials.
type Entry struct {
	Record         *mapping.Record
	SiblingSeasons []int
	Seasons        []mapping.SeasonRange
	Specials       []int
}

// scopeResult is the single scoping decision returned by scope: the semantic
// Kind, the on-disk release groups to compare against, whether the scoped unit
// has any file on disk, and whether the comparison is approximate (an offered
// bucket holding any file, or a whole-series aggregate spanning more than one
// season or group).
type scopeResult struct {
	// Revisions is the held reading per normalized group for a single unit: the
	// item's for a movie, the season's for a mapped season, the held episodes'
	// for placed episodes, nil otherwise.
	Revisions map[string]release.Revision
	Groups    []string
	// Missing are the placed episodes with no file, on ScopeEpisodes only.
	Missing []int
	Kind    ScopeKind
	HasFile bool
	Approx  bool
}

// RecordSeason resolves, from a mapping record ALONE, which season the record pins
// and which scope kind pins it: a mapped zero is offered, an absent season is a
// whole-series comparison, a positive season keeps its season.
//
// The dispatch input is the season's PRESENCE, never the type label, because an
// absent season and a mapped zero mean opposite things upstream and both leave
// SeasonTvdb 0. A positive season is never overridden: FLCL has no season 0.
func RecordSeason(rec *mapping.Record) (kind ScopeKind, season int) {
	switch rec.SeasonPresence() {
	case mapping.SeasonPresent:
		if rec.HasMappedSeason() {
			return ScopeSeason, rec.SeasonTvdb
		}
		return ScopeOffered, specialSeason
	case mapping.SeasonAbsent:
		return ScopeWholeSeries, 0
	}
	// Unknown reaches here from one producer only, a Record persisted before the
	// season kind existed. Both halves of the union must stay: IsMovie alone
	// strands the mapped-zero specials, IsSpecial alone strands the films.
	switch {
	case rec.HasMappedSeason():
		return ScopeSeason, rec.SeasonTvdb
	case rec.IsSpecial() || rec.IsMovie():
		return ScopeOffered, specialSeason
	default:
		return ScopeWholeSeries, 0
	}
}

// scope resolves the comparison scope of a matched entry once, for every
// consumer: the semantic Kind plus the on-disk release groups, file presence,
// and approximation flag that go with it. specials are the entry's placed
// season-0 episodes.
func scope(item *library.Item, rec *mapping.Record, specials []int) scopeResult {
	// The ARR decides the movie scope, ahead of anything the record says: a
	// Radarr item is a movie even when a broken upstream mapping carries a
	// season for it.
	if item.Arr == library.ArrRadarr {
		return scopeResult{Kind: ScopeMovie, Groups: item.Groups, HasFile: item.HasFile, Revisions: item.Revisions}
	}
	switch kind, season := RecordSeason(rec); kind {
	case ScopeSeason:
		// Group presence doubles as file presence here and in the specials branch:
		// release.Classify falls back to the literal NOGRP for a group-less file.
		g := item.SeasonGroups[season]
		return scopeResult{Kind: ScopeSeason, Groups: g, HasFile: len(g) > 0, Revisions: item.SeasonRevisions[season]}
	case ScopeOffered:
		if placed, ok := episodesScope(item, specials); ok {
			return placed
		}
		// The bucket is read for what it HOLDS, never to attribute a file to this
		// entry, so Approx means "never attributed" and must stay true for a
		// SINGLE-group bucket too: that one escapes every multi-group guard.
		g := item.SeasonGroups[season]
		return scopeResult{Kind: ScopeOffered, Groups: g, HasFile: len(g) > 0, Approx: len(g) > 0}
	default:
		// Everything left is a whole-series comparison with no single-unit scope;
		// Decide resolves it by conservative per-real-season aggregation.
		return scopeResult{Kind: ScopeWholeSeries}
	}
}

// episodesScope judges the files on the placed season-0 episodes, false when
// the item's episodes are unknown or it lists one of them not at all: a file
// Sonarr cannot import there must not be asked for.
func episodesScope(item *library.Item, specials []int) (scopeResult, bool) {
	if len(specials) == 0 {
		return scopeResult{}, false
	}
	res := scopeResult{Kind: ScopeEpisodes}
	readings := make(map[string][]release.Revision)
	for _, ep := range specials {
		held, listed := item.Specials[ep]
		switch {
		case !listed:
			return scopeResult{}, false
		case !held.HasFile:
			res.Missing = append(res.Missing, ep)
			continue
		}
		res.HasFile = true
		if _, seen := readings[held.Group]; !seen {
			res.Groups = append(res.Groups, held.Group)
		}
		readings[held.Group] = append(readings[held.Group], held.Revision)
	}
	slices.Sort(res.Groups)
	if res.HasFile {
		res.Revisions = make(map[string]release.Revision, len(readings))
		for group, revs := range readings {
			res.Revisions[group] = release.NewestRevision(revs...)
		}
	}
	return res, true
}

// String names the scope kind for an operator-facing label: "movie",
// "season", "offered", "episodes", or "series" for a whole-series comparison.
// It is the one home of that vocabulary, shared by the daemon's finding line
// and the audit report's scope cell (which adds the season NUMBER for
// ScopeSeason and the episodes for ScopeEpisodes).
func (k ScopeKind) String() string {
	switch k {
	case ScopeMovie:
		return "movie"
	case ScopeSeason:
		return "season"
	case ScopeOffered:
		return "offered"
	case ScopeEpisodes:
		return "episodes"
	default:
		return "series"
	}
}

// EpisodeLabel renders season-0 episode numbers for an operator, runs
// collapsed: [9 10 12] is "S00E09-E10, S00E12". It is "" for none.
func EpisodeLabel(episodes []int) string {
	var parts []string
	for i := 0; i < len(episodes); {
		j := i
		for j+1 < len(episodes) && episodes[j+1] == episodes[j]+1 {
			j++
		}
		part := fmt.Sprintf("S%02dE%02d", specialSeason, episodes[i])
		if j > i {
			part += fmt.Sprintf("-E%02d", episodes[j])
		}
		parts = append(parts, part)
		i = j + 1
	}
	return strings.Join(parts, ", ")
}

// MarshalJSON encodes the kind as its String() name, so a machine-readable
// consumer reads the same vocabulary a human does instead of an integer whose
// meaning is this file's iota order.
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
	for _, candidate := range []ScopeKind{ScopeWholeSeries, ScopeMovie, ScopeSeason, ScopeOffered, ScopeEpisodes} {
		if candidate.String() == name {
			*k = candidate
			return nil
		}
	}
	return fmt.Errorf("unknown scope kind %q", name)
}

// ClaimsCoverage reports whether this (item, record) pair may stand in for
// SeaDex covering the item's files: all but a season-0 film or special, which
// answers at most for its own episodes, never for the series.
//
// It must resolve the SCOPE rather than read the record: scope's arr-first early
// return keeps a Radarr-owned film with a mapped zero comparable as a movie, so a
// record-keyed predicate would strip its coverage. No file state is read, so a
// partial walk changes no answer.
func ClaimsCoverage(item *library.Item, rec *mapping.Record) bool {
	return scope(item, rec, nil).Kind != ScopeOffered
}

// ItemKind resolves the comparison scope kind of a library item that has no
// SeaDex-associated mapping record (an item enumerated by the audit's reverse
// catalogue, not matched to a SeaDex entry): a Radarr movie scopes to the
// movie, a Sonarr series has no per-season mapping and reads as the
// whole-series comparison.
func ItemKind(item *library.Item) ScopeKind {
	return scope(item, &mapping.Record{}, nil).Kind
}

// summary is the per-real-season aggregate summarizeWholeSeries collects: the
// sorted, deduped union of on-disk groups; how many real seasons (season 0
// excluded) carried files; and whether any of those seasons matched an
// alt-only group, proved unlisted, or was unverifiable (unknown group evidence
// on one side of its comparison).
type summary struct {
	Groups []string
	// superseded is the set of best groups held behind the listing once each
	// group's held revision is folded across the counted seasons, and held
	// their folded readings.
	superseded  []string
	held        []release.Revision
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
// their on-disk groups (sorted, deduped), and classifies each filed season under
// release.GroupsOverlap for wholeSeriesStanding to collapse. Which of those
// seasons are THIS entry's is ownSeason's single-source call. Reachable edge: an
// item whose every season belongs to siblings sums to ZERO seasons.
func summarizeWholeSeries(item *library.Item, listing *Listing, siblingSeasons []int, seasons []mapping.SeasonRange) summary {
	seen := make(map[string]struct{})
	var s summary
	var bestGroups []string
	readings := make(map[string][]release.Revision)
	for season, groups := range item.SeasonGroups {
		if season == specialSeason || len(groups) == 0 || !ownSeason(season, siblingSeasons, seasons) {
			continue
		}
		s.Seasons++
		s.Groups = appendMissingGroups(s.Groups, seen, groups)
		switch groupLadder(groups, listing) {
		case StandingAlt:
			s.AnyAlt = true
		case StandingUnlisted:
			s.AnyUnlisted = true
		case StandingUnverified:
			s.AnyUnverified = true
		case StandingBest:
			bestGroups = append(bestGroups, groups...)
			for _, group := range groups {
				normalized := release.NormalizeGroup(group)
				readings[normalized] = append(readings[normalized], item.SeasonRevisions[season][normalized])
			}
		case StandingNoFile, StandingBestSuperseded:
			// unreachable: the loop skips empty seasons and groupLadder
			// never reads revisions
		}
	}
	// The listing's newest revision spans the whole entry, so a season with no
	// reissued episode must not read behind it on its own: the held side is
	// folded across the counted seasons first.
	held := make(map[string]release.Revision, len(readings))
	for group, revs := range readings {
		held[group] = release.NewestRevision(revs...)
	}
	s.superseded = supersededGroups(bestGroups, held, listing)
	s.held = heldReadings(s.superseded, held)
	slices.Sort(s.Groups)
	s.Approx = s.Seasons > 1 || len(s.Groups) > 1
	return s
}

// ownSeason reports whether a filed real season belongs to the entry under
// comparison: named by one of the entry's own ranges when it has any, else not
// claimed by a sibling record.
func ownSeason(season int, siblingSeasons []int, seasons []mapping.SeasonRange) bool {
	if len(seasons) > 0 {
		return slices.ContainsFunc(seasons, func(r mapping.SeasonRange) bool { return r.Season == season })
	}
	return !slices.Contains(siblingSeasons, season)
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
