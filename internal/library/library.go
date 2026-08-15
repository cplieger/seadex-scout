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
	Arr          string           `json:"arr"`
	ImdbID       string           `json:"imdb_id,omitempty"`
	Title        string           `json:"title"`
	// ArrURL is the arr web-UI deep link, stored ALREADY REDACTED: the walker
	// builds it through SafeLogURL, so no configured-URL credential (reverse-proxy
	// Basic Auth, a query token) ever enters an Item, a Snapshot, a Finding, or an
	// audit Row. The sink-side SafeLogURL calls are belt-and-braces for an Item
	// built outside the walker (tests, future construction paths).
	ArrURL    string          `json:"arr_url,omitempty"`
	Current   release.Release `json:"current"`
	AltTitles []string        `json:"alt_titles,omitempty"`
	Groups    []string        `json:"groups,omitempty"`
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

// Snapshot is one library walk.
type Snapshot struct {
	TakenAt time.Time `json:"taken_at"`
	Items   []Item    `json:"items,omitempty"`
	// Partial reports that the walk could not READ part of the library: at
	// least one series' episode fetch failed.
	Partial bool `json:"partial,omitempty"`
	// FilteredEmpty reports that arr_tags filtering kept nothing out of a
	// non-empty arr list on at least one enabled side, so that side
	// contributed zero items for a configuration reason (a dead include set,
	// or labels no item carries) rather than because the library is empty.
	FilteredEmpty bool `json:"filtered_empty,omitempty"`
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
// attribution, or current fingerprint differs.
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
// (file presence, group set, per-season group attribution, and fingerprint),
// for diff change detection.
func sameItem(a, b *Item) bool {
	return a.HasFile == b.HasFile && a.Current == b.Current &&
		slices.Equal(a.Groups, b.Groups) &&
		maps.EqualFunc(a.SeasonGroups, b.SeasonGroups, slices.Equal)
}
