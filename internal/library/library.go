// Package library is the app's snapshot MODEL of the Sonarr/Radarr anime
// library: one Item per series or movie (its external IDs, current release
// groups, per-season group attribution and a representative release
// fingerprint), the Snapshot one walk produces, and the diff between two
// snapshots.
//
// It is a pure leaf over internal/release, keyenc, and the stdlib.
package library

import (
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/cplieger/keyenc"
	"github.com/cplieger/seadex-scout/internal/release"
)

// Arr names label an item's source instance.
const (
	ArrSonarr = "sonarr"
	ArrRadarr = "radarr"
)

// Item is one library entry (series or movie) in a snapshot. Fields are ordered
// for govet fieldalignment.
type Item struct {
	SeasonGroups map[int][]string `json:"season_groups,omitempty"`
	// SeasonRevisions is, per season and per normalized group, the NEWEST
	// revision that group's files in the season carry (release.NewestRevision,
	// so unknown when any of those files' revision is unknown). Keys mirror
	// SeasonGroups; a missing key reads as the unknown zero value.
	SeasonRevisions map[int]map[string]release.Revision `json:"season_revisions,omitempty"`
	// SeasonEpisodes maps a season to its episodes that have a file, by episode
	// number. It is nil when the walk could not read the series' episode list,
	// or the list holds a file it does not tie to an episode, which leaves
	// every size built on it unknown.
	SeasonEpisodes map[int]map[int]Episode `json:"season_episodes,omitempty"`
	// FileBytes maps each of the item's file ids to the size the arr reported.
	// A size of 0 or less is unknown: the arrs decode an absent size as 0.
	FileBytes map[int]int64 `json:"file_bytes,omitempty"`
	// Revisions is the same fold over all of the item's files per group; it is
	// the movie scope's source.
	Revisions map[string]release.Revision `json:"revisions,omitempty"`
	// Specials is, per Sonarr season-0 episode number, the file on it. It is
	// read only for a series holding a season-0 file, so nil means unknown,
	// never "no specials".
	Specials map[int]SpecialEpisode `json:"specials,omitempty"`
	Arr      string                 `json:"arr"`
	ImdbID   string                 `json:"imdb_id,omitempty"`
	Title    string                 `json:"title"`
	// ArrURL is the arr web-UI deep link, stored ALREADY REDACTED: the walker
	// builds it through SafeLogURL, so no configured-URL credential (reverse-proxy
	// Basic Auth, a query token) ever enters an Item, a Snapshot, a Finding, or an
	// audit Row. The sink-side SafeLogURL calls are belt-and-braces for an Item
	// built outside the walker (tests, future construction paths).
	ArrURL    string          `json:"arr_url,omitempty"`
	AltTitles []string        `json:"alt_titles,omitempty"`
	Groups    []string        `json:"groups,omitempty"`
	Current   release.Release `json:"current"`
	ArrID     int             `json:"arr_id"`
	TvdbID    int             `json:"tvdb_id,omitempty"`
	TmdbID    int             `json:"tmdb_id,omitempty"`
	Year      int             `json:"year,omitempty"`
	HasFile   bool            `json:"has_file"`
	// Failed marks an item whose file data this walk could not establish: a
	// series whose episode fetch failed, or a movie Radarr reports a file for
	// while sending no file payload.
	Failed bool `json:"failed,omitempty"`
}

// SpecialEpisode is one season-0 episode of a series: whether a file sits on
// it, and that file's normalized group and revision.
type SpecialEpisode struct {
	Group    string           `json:"group,omitempty"`
	Revision release.Revision `json:"revision,omitzero"`
	HasFile  bool             `json:"has_file,omitempty"`
}

// Episode is one episode of a series that has a file: the file's id and the
// episode's absolute number, 0 when the arr gives none.
type Episode struct {
	File     int `json:"file"`
	Absolute int `json:"absolute,omitempty"`
}

// FileKey identifies one of the item's files across the library: the arr and
// the file id, which each arr numbers on its own.
func (it *Item) FileKey(fileID int) string {
	return keyenc.Join(it.Arr, "file", strconv.Itoa(fileID))
}

// Key identifies the item by its arr source and arr ID ("arr:id") - the
// item's semantic identity across snapshots and packages. Snapshot diffing
// (indexByKey) and the audit's covered-item map both key on it, so the
// identity rule is written once here in the package that owns Item.
func (it *Item) Key() string {
	// Assembled through keyenc rather than concatenated: `:` is keyenc's own
	// separator, and Join makes the split unforgeable by construction instead of
	// by the accident that the decimal ArrID sits last.
	return keyenc.Join(it.Arr, strconv.Itoa(it.ArrID))
}

// Comparable reports whether the item's file state may be compared against a
// recommendation.
func (it *Item) Comparable() bool { return !it.Failed }

// SpecialsUnknown reports whether the series holds a season-0 file whose
// episode is not known: the episode list read failed or tied no season-0
// episode to it, or the snapshot predates the field. A series with no
// season-0 file is known to hold none.
func (it *Item) SpecialsUnknown() bool {
	return it.Specials == nil && len(it.SeasonGroups[0]) > 0
}

// Snapshot is one library walk.
type Snapshot struct {
	TakenAt time.Time `json:"taken_at"`
	Items   []Item    `json:"items,omitempty"`
	// FilteredEmptyArrs names every arr whose arr_tags filtering kept nothing
	// out of a non-empty arr list, so that arr contributed zero items for a
	// configuration reason (a dead include set, or labels no item carries)
	// rather than because the library is empty. Naming the arrs, rather than
	// reporting one bool, is what lets a consumer tell partial blindness from
	// every enabled arr emptied at once.
	FilteredEmptyArrs []string `json:"filtered_empty_arrs,omitempty"`
	// Partial reports that the walk could not READ part of the library: at
	// least one series' episode fetch failed.
	Partial bool `json:"partial,omitempty"`
}

// Diff summarizes what changed between two snapshots (by arr + arr id).
type Diff struct {
	Added   int
	Removed int
	Changed int
}

// --- Snapshot diffing ---

// DiffSnapshots reports what changed between prev and cur, keyed by arr + id.
// An item is Changed when its file presence, group set, per-season group
// attribution, revision readings, season-0 episode files, file sizes,
// episode-to-file map, or current fingerprint differs.
func DiffSnapshots(prev, cur *Snapshot) Diff {
	prevByKey, prevFailed := indexByKey(prev)
	curByKey, curFailed := indexByKey(cur)
	var d Diff
	for k, c := range curByKey {
		if p, ok := prevByKey[k]; ok && !sameItem(p, c) {
			d.Changed++
		}
	}
	d.Added = countAbsent(curByKey, curFailed, prevByKey, prevFailed)
	d.Removed = countAbsent(prevByKey, prevFailed, curByKey, curFailed)
	return d
}

// countAbsent counts keys present on the "from" side (its comparable index or
// its placeholders) but absent from the "other" side entirely
// (neither comparable nor a placeholder). A key that debuts as - or disappears
// while - a placeholder still asserts arr presence, so it counts as a
// genuine arrival/departure exactly once regardless of which side holds it.
func countAbsent(fromByKey map[string]*Item, fromFailed map[string]struct{},
	otherByKey map[string]*Item, otherFailed map[string]struct{},
) int {
	n := 0
	for k := range fromByKey {
		if !presentIn(k, otherByKey, otherFailed) {
			n++
		}
	}
	for k := range fromFailed {
		if !presentIn(k, otherByKey, otherFailed) {
			n++
		}
	}
	return n
}

// presentIn reports whether key k appears on a side, counting both its
// comparable index and its placeholders.
func presentIn(k string, byKey map[string]*Item, failed map[string]struct{}) bool {
	if _, ok := byKey[k]; ok {
		return true
	}
	_, ok := failed[k]
	return ok
}

// indexByKey keys a snapshot's comparable items by "arr:id" (values point
// into the snapshot's backing array, avoiding per-item copies) and returns
// its placeholders' keys separately: a placeholder carries no
// comparable file state (it exists so the compare pass and the diff can
// scope their handling to the keys the walk could not establish), so it
// joins the failed set instead of the comparable index. failed is nil when
// the snapshot has no placeholders (the common case).
func indexByKey(s *Snapshot) (byKey map[string]*Item, failed map[string]struct{}) {
	byKey = make(map[string]*Item, len(s.Items))
	for i := range s.Items {
		it := &s.Items[i]
		if !it.Comparable() {
			if failed == nil {
				failed = make(map[string]struct{})
			}
			failed[it.Key()] = struct{}{}
			continue
		}
		byKey[it.Key()] = it
	}
	return byKey, failed
}

// sameItem reports whether two items have the same current release state
// (every field DiffSnapshots names), for diff change detection.
func sameItem(a, b *Item) bool {
	return a.HasFile == b.HasFile && a.Current == b.Current &&
		slices.Equal(a.Groups, b.Groups) &&
		maps.EqualFunc(a.SeasonGroups, b.SeasonGroups, slices.Equal) &&
		maps.EqualFunc(a.SeasonRevisions, b.SeasonRevisions, maps.Equal) &&
		maps.Equal(a.Revisions, b.Revisions) &&
		maps.Equal(a.Specials, b.Specials) &&
		maps.Equal(a.FileBytes, b.FileBytes) &&
		maps.EqualFunc(a.SeasonEpisodes, b.SeasonEpisodes, maps.Equal)
}
