// Package align resolves which on-disk release groups a SeaDex entry should be
// compared against, and summarizes a whole-series comparison. It is the single
// source of truth for that scoping, consumed by BOTH the daemon's compare pass
// (internal/compare) and the audit report (internal/audit) so the two never
// disagree about the same title.
//
// It stays a thin, library-aware leaf: it depends only on library, mapping, and
// the pure release classifier - never on seadex, match, or the consumers - so it
// can be shared without a dependency cycle. (It is a separate package rather than
// living in internal/release because release is deliberately pure and imports no
// library/mapping types.)
package align

import (
	"sort"

	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/release"
)

// SpecialSeason is the TVDB season number Sonarr files specials under.
const SpecialSeason = 0

// Scope returns the on-disk release groups to compare a matched entry against,
// whether the scoped unit has any file on disk, and whether the comparison is
// approximate. It handles the three single-unit scopes: a movie (the movie's
// groups), a series with a positive Fribb TVDB season (that season's groups,
// exact), and a special (the season-0 bucket Sonarr lumps specials into,
// approximate when it holds more than one group).
//
// A Sonarr series with no positive Fribb season and not a special is a
// whole-series comparison (WholeSeries reports true for it); resolve that with
// SummarizeWholeSeries instead of Scope.
func Scope(item *library.Item, rec *mapping.Record) (groups []string, hasFile, approx bool) {
	switch {
	case item.Arr == library.ArrRadarr:
		return item.Groups, item.HasFile, false
	case rec.SeasonTvdb > 0:
		g, ok := item.SeasonGroups[rec.SeasonTvdb]
		return g, ok && len(g) > 0, false
	default: // a special: compare against the season-0 specials bucket
		g, ok := item.SeasonGroups[SpecialSeason]
		return g, ok && len(g) > 0, ok && len(g) > 1
	}
}

// WholeSeries reports whether the item must be compared against the whole series
// rather than a single unit: a Sonarr item with no positive Fribb TVDB season
// and not a special (an absolute-numbered run like One Piece, or a title-only
// match). SeaDex carries one whole-series recommendation for these, with no
// per-season mapping.
func WholeSeries(item *library.Item, rec *mapping.Record) bool {
	return item.Arr == library.ArrSonarr && rec.SeasonTvdb <= 0 && !rec.IsSpecial()
}

// Summary is the per-real-season aggregate SummarizeWholeSeries collects: the
// sorted, deduped union of on-disk groups; how many real seasons (season 0
// excluded) carried files; and whether any of those seasons matched an alt-only
// or an unlisted group.
type Summary struct {
	Groups      []string
	Seasons     int
	AnyAlt      bool
	AnyUnlisted bool
}

// SummarizeWholeSeries walks the item's real seasons (season 0 excluded), unions
// their on-disk groups (sorted, deduped), and records whether any real season
// carried an alt-only or an unlisted group, so a caller can pick the most
// conservative whole-series verdict.
//
// A caller that only distinguishes best-vs-not (the daemon's compare pass) passes
// a nil alt: a season lacking a best group then surfaces as AnyUnlisted, so
// "every on-disk season has a best group" is exactly "!AnyUnlisted".
func SummarizeWholeSeries(item *library.Item, best, alt []string) Summary {
	seen := make(map[string]struct{})
	var s Summary
	for season, groups := range item.SeasonGroups {
		if season == SpecialSeason || len(groups) == 0 {
			continue
		}
		s.Seasons++
		s.Groups = appendMissingGroups(s.Groups, seen, groups)
		switch {
		case release.GroupsIntersect(groups, best):
			// this season carries a best group
		case release.GroupsIntersect(groups, alt):
			s.AnyAlt = true
		default:
			s.AnyUnlisted = true
		}
	}
	sort.Strings(s.Groups)
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
