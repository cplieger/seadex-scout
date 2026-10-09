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

// Per-view list bound, so the lines one pass adds stay fixed.
const maxBiggestUpgrades = 25

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
	if f.CurrentBytes > 0 {
		kvs = append(kvs, "current_bytes", f.CurrentBytes)
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

// totals is a view's size summary: the downloads and replaced files of every
// counted finding, each identity once.
type totals struct {
	downloads map[string]int64
	replaced  map[string]int64
	download  int64
	current   int64
	counted   int
}

// add counts f into the totals, or leaves them unchanged when f is unsized,
// when one of its identities already carries another size (the returned
// conflict names it), or when a sum would pass math.MaxInt64.
func (t *totals) add(f *compare.Finding) (conflict string) {
	if !sized(f) {
		return ""
	}
	replaced := make([]part, 0, len(f.Replaced))
	for _, r := range f.Replaced {
		replaced = append(replaced, part{r.Key, r.Bytes})
	}
	download, newDownloads, conflict, ok := union(t.downloads, t.download, downloadParts(f))
	if !ok {
		return conflict
	}
	current, newReplaced, conflict, ok := union(t.replaced, t.current, replaced)
	if !ok {
		return conflict
	}
	maps.Copy(t.downloads, newDownloads)
	maps.Copy(t.replaced, newReplaced)
	t.download, t.current = download, current
	t.counted++
	return ""
}

// downloadTotal is a view's download floor: the downloads of every finding
// whose download size is known, each torrent once, whether or not the size of
// the files it replaces is known.
type downloadTotal struct {
	seen    map[string]int64
	bytes   int64
	counted int
}

// add counts f's downloads, or leaves the total unchanged when f's download
// size is unknown, when one of its torrents already carries another size (the
// returned conflict names it), or when the sum would pass math.MaxInt64.
func (t *downloadTotal) add(f *compare.Finding) (conflict string) {
	if f.ReleaseBytes <= 0 {
		return ""
	}
	bytes, fresh, conflict, ok := union(t.seen, t.bytes, downloadParts(f))
	if !ok {
		return conflict
	}
	maps.Copy(t.seen, fresh)
	t.bytes = bytes
	t.counted++
	return ""
}

type part struct {
	key   string
	bytes int64
}

func downloadParts(f *compare.Finding) []part {
	parts := make([]part, 0, len(f.Downloads))
	for _, d := range f.Downloads {
		parts = append(parts, part{d.ID, d.Bytes})
	}
	return parts
}

// union adds the parts seen does not hold yet to total. It returns the new
// total and parts, or false with the key that carries a second size, or with
// no key when the sum would pass math.MaxInt64. seen is not modified.
func union(seen map[string]int64, total int64, parts []part) (sum int64, fresh map[string]int64, conflict string, ok bool) {
	fresh = map[string]int64{}
	for _, p := range parts {
		prev, known := seen[p.key]
		if !known {
			prev, known = fresh[p.key]
		}
		if known {
			if prev != p.bytes {
				return 0, nil, p.key, false
			}
			continue
		}
		if total, ok = compare.AddBytes(total, p.bytes); !ok {
			return 0, nil, "", false
		}
		fresh[p.key] = p.bytes
	}
	return total, fresh, "", true
}

// emitView logs one view's size summary and its biggest size changes.
// conflicts holds the identities already warned about this pass, so a conflict
// both views meet is reported once.
func (n *Notifier) emitView(v *view, pass int64, conflicts map[string]bool) {
	t := totals{downloads: map[string]int64{}, replaced: map[string]int64{}}
	floor := downloadTotal{seen: map[string]int64{}}
	better, newer := 0, 0
	for _, r := range v.rows {
		if r.f.Status == compare.StatusNewerRevision {
			newer++
		} else {
			better++
		}
		for _, conflict := range []string{t.add(&r.f), floor.add(&r.f)} {
			if conflict != "" && !conflicts[conflict] {
				conflicts[conflict] = true
				n.log.Warn("one download or file carries two sizes across findings; the finding that brought the second is left out of the size totals",
					"pass_id", pass, "id", conflict, "title", capAttr(r.f.Title), "al_id", r.f.AniListID)
			}
		}
	}
	n.log.Info("upgrade sizes",
		"pass_id", pass, "hidden_tier", v.tier,
		"upgrades", len(v.rows), "upgrades_better_release", better, "upgrades_newer_revision", newer,
		"upgrades_sized", t.counted, "upgrades_unsized", len(v.rows)-t.counted,
		"recommended_bytes_total", t.download, "current_bytes_replaced", t.current,
		"size_change_bytes", t.download-t.current,
		"download_bytes_total", floor.bytes, "upgrades_download_unsized", len(v.rows)-floor.counted)
	n.emitBiggest(v, pass)
}

// emitBiggest logs the view's maxBiggestUpgrades sized upgrades with the
// largest size change either way, biggest first.
func (n *Notifier) emitBiggest(v *view, pass int64) {
	var biggest []*row
	for _, r := range v.rows {
		if sized(&r.f) {
			biggest = append(biggest, r)
		}
	}
	slices.SortStableFunc(biggest, func(a, b *row) int {
		return cmp.Compare(absChange(&b.f), absChange(&a.f))
	})
	for i, r := range biggest[:min(len(biggest), maxBiggestUpgrades)] {
		n.log.Info("biggest upgrade",
			"pass_id", pass, "hidden_tier", v.tier, "rank", i+1,
			"current_tier", string(r.f.Tier), "status", string(r.f.Status),
			"title", capAttr(r.f.Title), "al_id", r.f.AniListID, "arr", r.f.Arr,
			"arr_url", capURLAttr(r.f.ArrURL), "season", r.f.Season,
			"current_group", capAttr(r.f.CurrentGroup), "recommended_group", capAttr(r.f.RecommendedGroup),
			"recommended_bytes", r.f.ReleaseBytes, "current_bytes", r.f.CurrentBytes,
			"size_change_bytes", r.f.ReleaseBytes-r.f.CurrentBytes)
	}
}

// absChange is |download - current|; both are positive, so neither the
// difference nor its negation can overflow.
func absChange(f *compare.Finding) int64 {
	d := f.ReleaseBytes - f.CurrentBytes
	if d < 0 {
		return -d
	}
	return d
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
