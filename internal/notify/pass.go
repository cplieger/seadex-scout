package notify

import (
	"cmp"
	"maps"
	"slices"
	"time"

	"github.com/cplieger/seadex-scout/internal/compare"
)

// Each view is one value of the dashboard's Optional upgrades switch, named by
// the current_tier it hides: viewAlt hides upgrades from a SeaDex alt,
// viewNone hides nothing.
const (
	viewAlt = iota
	viewNone
	viewCount
)

var viewTiers = [viewCount]string{viewAlt: string(compare.TierAlt), viewNone: "none"}

func passID(start time.Time) int64 { return start.UnixMilli() }

type row struct {
	firstSeen time.Time
	key       string
	f         compare.Finding
	// 0 where the view hides the row or it is not an upgrade.
	ranks     [viewCount]int
	checkRank int
}

type view struct {
	tier string
	rows []*row
}

func isUpgrade(f *compare.Finding) bool {
	return f.Status == compare.StatusBetter || f.Status == compare.StatusNewerRevision
}

func isManualReview(f *compare.Finding) bool {
	return f.Status == compare.StatusMixedGroup || f.Status == compare.StatusUnverifiable
}

// rankChecks numbers the manual-review rows 1 upward in key order, records
// each row's rank, and returns how many there are.
func rankChecks(rows []row) int {
	n := 0
	for i := range rows {
		if isManualReview(&rows[i].f) {
			n++
			rows[i].checkRank = n
		}
	}
	return n
}

func sized(f *compare.Finding) bool { return f.ReleaseBytes > 0 && f.CurrentBytes > 0 }

// sizeKVs renders f's sizes, each only when known, so a reader shows an
// unknown size as absent rather than as 0.
func sizeKVs(f *compare.Finding) []any {
	var kvs []any
	if f.ReleaseBytes > 0 {
		kvs = append(kvs, "recommended_bytes", f.ReleaseBytes)
	}
	if sized(f) {
		kvs = append(kvs, "size_change_bytes", f.ReleaseBytes-f.CurrentBytes)
	}
	return kvs
}

// rankViews ranks each view's upgrade rows, 1 upward: newer revisions first,
// then better releases, each by size change descending with an unknown size
// last, then by key. It records each row's ranks and returns the views.
func rankViews(rows []row) [viewCount]view {
	upgrades := make([]*row, 0, len(rows))
	for i := range rows {
		if isUpgrade(&rows[i].f) {
			upgrades = append(upgrades, &rows[i])
		}
	}
	slices.SortStableFunc(upgrades, compareRank)
	var views [viewCount]view
	for v := range viewCount {
		views[v].tier = viewTiers[v]
		for _, r := range upgrades {
			if v == viewAlt && r.f.Tier == compare.TierAlt {
				continue
			}
			views[v].rows = append(views[v].rows, r)
			r.ranks[v] = len(views[v].rows)
		}
	}
	return views
}

func compareRank(a, b *row) int {
	return cmp.Or(
		cmp.Compare(statusOrder(a.f.Status), statusOrder(b.f.Status)),
		compareChange(&a.f, &b.f),
		cmp.Compare(a.key, b.key),
	)
}

func statusOrder(s compare.Status) int {
	if s == compare.StatusNewerRevision {
		return 0
	}
	return 1
}

// compareChange orders by size change descending, an unknown change last.
func compareChange(a, b *compare.Finding) int {
	switch sa, sb := sized(a), sized(b); {
	case sa && sb:
		return cmp.Compare(b.ReleaseBytes-b.CurrentBytes, a.ReleaseBytes-a.CurrentBytes)
	case sa:
		return -1
	case sb:
		return 1
	default:
		return 0
	}
}

// emitView logs one view's upgrade count.
func (n *Notifier) emitView(v *view, pass int64) {
	n.log.Info("upgrade sizes", "pass_id", pass, "hidden_tier", v.tier, "upgrades", len(v.rows))
}

// reportChanges logs one event per upgrade key entering or leaving the set and
// keeps firstSeen in step with next. The first report of a process marks its
// found events after_start, since they reflect the start rather than a change.
func (n *Notifier) reportChanges(next map[string]compare.Finding, pass int64, start time.Time) {
	for _, key := range slices.Sorted(maps.Keys(next)) {
		if _, present := n.current[key]; present {
			continue
		}
		n.firstSeen[key] = start
		f := next[key]
		n.upgradeEvent("upgrade found", &f, pass)
	}
	for _, key := range slices.Sorted(maps.Keys(n.current)) {
		if _, present := next[key]; present {
			continue
		}
		delete(n.firstSeen, key)
		f := n.current[key]
		n.upgradeEvent("upgrade resolved", &f, pass)
	}
}

func (n *Notifier) upgradeEvent(msg string, f *compare.Finding, pass int64) {
	if !isUpgrade(f) {
		return
	}
	if _, ignored := n.ignore[f.AniListID]; ignored {
		return
	}
	n.log.Info(msg, "pass_id", pass, "after_start", !n.reported,
		"title", capAttr(f.Title), "al_id", f.AniListID, "arr", f.Arr, "arr_url", capURLAttr(f.ArrURL),
		"season", f.Season, "status", string(f.Status), "current_tier", string(f.Tier),
		"current_group", capAttr(f.CurrentGroup), "recommended_group", capAttr(f.RecommendedGroup))
}
