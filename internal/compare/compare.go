// Package compare turns matched SeaDex entries into findings. For each entry in
// the library it classifies the SeaDex "best" releases, applies the content
// filters (remux/resolution/dual-audio), and compares the surviving recommended
// release groups against the groups present on the library item (series-level
// by default, per-season behind a flag). An item that already has a recommended
// group is aligned and produces no finding.
//
// The tracker allowlist is a preference, not a hard filter: when SeaDex's best
// release is only on a tracker the operator does not use, the finding is still
// emitted (tagged unavailable_on_selected_trackers) if NotifyUnavailableTracker
// is set, so the operator learns a better release exists even if they cannot
// grab it from their indexers.
package compare

import (
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/cplieger/seadex-scout/internal/filter"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// Status is the comparison outcome for a finding.
type Status string

const (
	// StatusBetter means SeaDex recommends a release group the library lacks,
	// available on a tracker the operator uses.
	StatusBetter Status = "better_release"
	// StatusUnavailableTracker means a better release exists but only on a
	// tracker outside the operator's allowlist.
	StatusUnavailableTracker Status = "unavailable_on_selected_trackers"
	// StatusMixedGroup means the series' episodes span multiple groups; a manual
	// review nudge rather than a false "better release".
	StatusMixedGroup Status = "mixed_group_manual"
	// StatusIncomplete means the SeaDex entry is incomplete; nothing complete to grab.
	StatusIncomplete Status = "incomplete"
	// StatusTheoretical means the entry only names a theoretical best (not muxed).
	StatusTheoretical Status = "theoretical_best"
	// StatusPrivateOnly means the recommended release is only on an excluded
	// tracker and unavailable-tracker notifications are off (suppressed to info).
	StatusPrivateOnly Status = "private_only"
)

// Severity is the log level a finding maps to.
type Severity string

const (
	// SevWarn is an actionable finding (a better release to go get).
	SevWarn Severity = "warn"
	// SevInfo is an informational finding (nothing directly actionable).
	SevInfo Severity = "info"
)

// Finding is one comparison result for a library item. It carries the fields
// the report layer emits and the dedupe key that suppresses re-alerts.
type Finding struct {
	Kind              string   `json:"kind,omitempty"`
	Reason            string   `json:"classification_reason,omitempty"`
	Arr               string   `json:"arr"`
	CurrentGroup      string   `json:"current_group,omitempty"`
	RecommendedGroup  string   `json:"recommended_group,omitempty"`
	Tracker           string   `json:"tracker,omitempty"`
	Title             string   `json:"title"`
	Resolution        string   `json:"resolution,omitempty"`
	Severity          Severity `json:"severity"`
	Codec             string   `json:"codec,omitempty"`
	ReleaseURL        string   `json:"release_url,omitempty"`
	InfoHash          string   `json:"info_hash,omitempty"`
	DedupeKey         string   `json:"dedupe_key"`
	Status            Status   `json:"status"`
	RecommendedGroups []string `json:"recommended_groups,omitempty"`
	AniListID         int      `json:"al_id"`
}

// Comparer produces findings from matches under a fixed filter/scoping policy.
type Comparer struct {
	log             *slog.Logger
	remuxGroups     map[string]bool
	opts            filter.Options
	seasonScoping   bool
	notifyUnavail   bool
	includeSpecials bool
}

// Config configures a Comparer.
type Config struct {
	Logger                   *slog.Logger
	RemuxGroups              map[string]bool
	Filter                   filter.Options
	SeasonScoping            bool
	NotifyUnavailableTracker bool
	IncludeSpecials          bool
}

// NewComparer builds a Comparer from cfg.
func NewComparer(cfg Config) *Comparer {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Comparer{
		log:             log,
		remuxGroups:     cfg.RemuxGroups,
		opts:            cfg.Filter,
		seasonScoping:   cfg.SeasonScoping,
		notifyUnavail:   cfg.NotifyUnavailableTracker,
		includeSpecials: cfg.IncludeSpecials,
	}
}

// Compare produces a finding for every in-library match that has something to
// report, skipping matches not in the library, items already aligned, and
// specials when they are excluded.
func (c *Comparer) Compare(matches []match.Match) []Finding {
	var findings []Finding
	for i := range matches {
		m := &matches[i]
		if !m.InLibrary() {
			continue
		}
		if !c.includeSpecials && m.Record.IsSpecial() {
			continue
		}
		if f := c.compareOne(m); f != nil {
			findings = append(findings, *f)
		}
	}
	return findings
}

// candidate pairs a SeaDex torrent with its classified release so the finding
// can carry the torrent's URL and info hash after filtering on the release.
type candidate struct {
	rel     release.Release
	torrent seadex.Torrent
}

// partition splits the classified best releases into the recommended set (those
// passing the content filters) and, within it, those on an allowed tracker vs
// only on an excluded one.
type partition struct {
	recommended []candidate
	onAllowed   []candidate
	onExcluded  []candidate
}

// compareOne compares one matched, in-library entry and returns a finding, or
// nil when the item is aligned (already has a recommended group).
func (c *Comparer) compareOne(m *match.Match) *Finding {
	item := m.Item
	entry := &m.Entry
	p := c.partition(c.classifyBest(entry))

	base := c.baseFinding(m)
	if len(p.recommended) == 0 {
		return emptyResult(entry, &base)
	}

	recGroups := groupSet(p.recommended)
	if item.MixedGroups {
		fillBest(&base, p.recommended, recGroups)
		return finalize(&base, StatusMixedGroup, SevInfo)
	}
	if intersects(recGroups, c.currentGroups(item, &m.Record)) {
		return nil // aligned: a recommended group is already present
	}

	status, sev, pool := classifyBetter(entry, &p, c.notifyUnavail)
	fillBest(&base, pool, recGroups)
	return finalize(&base, status, sev)
}

// partition classifies each best release through the content filters and, for
// survivors, the tracker allowlist.
func (c *Comparer) partition(candidates []candidate) partition {
	var p partition
	for i := range candidates {
		cand := &candidates[i]
		if ok, _ := filter.KeepNonTracker(&cand.rel, c.opts); !ok {
			continue
		}
		p.recommended = append(p.recommended, *cand)
		if filter.TrackerAllowed(&cand.rel, c.opts) {
			p.onAllowed = append(p.onAllowed, *cand)
		} else {
			p.onExcluded = append(p.onExcluded, *cand)
		}
	}
	return p
}

// classifyBetter decides the status, severity, and headline pool for an item
// that lacks a recommended group. An incomplete entry is info; a release on an
// allowed tracker is an actionable better_release; a release only on an excluded
// tracker is either an unavailable-tracker notification (when enabled) or a
// suppressed private_only info.
func classifyBetter(entry *seadex.Entry, p *partition, notifyUnavail bool) (Status, Severity, []candidate) {
	switch {
	case entry.Incomplete:
		return StatusIncomplete, SevInfo, p.recommended
	case len(p.onAllowed) > 0:
		return StatusBetter, SevWarn, p.onAllowed
	case notifyUnavail:
		return StatusUnavailableTracker, SevWarn, p.onExcluded
	default:
		return StatusPrivateOnly, SevInfo, p.onExcluded
	}
}

// classifyBest classifies every SeaDex "best" torrent into a candidate.
func (c *Comparer) classifyBest(entry *seadex.Entry) []candidate {
	var candidates []candidate
	for i := range entry.Torrents {
		t := &entry.Torrents[i]
		if !t.IsBest {
			continue
		}
		rel := release.Classify(&release.Input{
			Names:       fileNames(t.Files),
			RemuxGroups: c.remuxGroups,
			Notes:       entry.Notes,
			Group:       t.ReleaseGroup,
			Tracker:     t.Tracker,
			DualAudio:   t.DualAudio,
		})
		candidates = append(candidates, candidate{rel: rel, torrent: *t})
	}
	return candidates
}

// emptyResult decides the finding when no recommended release survives the
// content filters: a theoretical-best-only or incomplete entry is an info
// nudge, everything else (operator opted out via filters) is silent.
func emptyResult(entry *seadex.Entry, base *Finding) *Finding {
	switch {
	case entry.HasTheoreticalBest():
		return finalize(base, StatusTheoretical, SevInfo)
	case entry.Incomplete:
		return finalize(base, StatusIncomplete, SevInfo)
	default:
		return nil
	}
}

// currentGroups returns the library groups to compare against: the specific
// season's groups when season-scoping is enabled and the entry maps to a
// season, else the whole-item group set.
func (c *Comparer) currentGroups(item *library.Item, rec *mapping.Record) []string {
	if c.seasonScoping && rec.SeasonTvdb > 0 {
		if g, ok := item.SeasonGroups[rec.SeasonTvdb]; ok {
			return g
		}
	}
	return item.Groups
}

// baseFinding seeds a finding with the item identity fields.
func (c *Comparer) baseFinding(m *match.Match) Finding {
	return Finding{
		Title:        m.Item.Title,
		Arr:          m.Arr,
		CurrentGroup: currentGroup(m.Item),
		AniListID:    m.Entry.AniListID,
	}
}

// fillBest sets the recommended-release fields from the headline candidate of
// pool (highest resolution, public tracker preferred) plus the full group set.
func fillBest(f *Finding, pool []candidate, recGroups []string) {
	rep := representative(pool)
	fillFromCandidate(f, &rep)
	f.RecommendedGroups = recGroups
}

// fillFromCandidate copies a candidate's release + torrent fields onto a finding.
func fillFromCandidate(f *Finding, cand *candidate) {
	f.RecommendedGroup = cand.rel.Group
	f.Tracker = cand.rel.Tracker
	f.Resolution = cand.rel.Resolution
	f.Codec = cand.rel.Codec
	f.Kind = string(cand.rel.Kind)
	f.Reason = cand.rel.Reason
	f.InfoHash = cand.torrent.InfoHash
	f.ReleaseURL = cand.torrent.UsableURL()
}

// finalize sets a finding's status/severity and computes its dedupe key.
func finalize(f *Finding, status Status, sev Severity) *Finding {
	f.Status = status
	f.Severity = sev
	f.DedupeKey = dedupeKey(f)
	return f
}

// dedupeKey keys a finding by AniList ID, status, recommended-group set, current
// group, and SeaDex info hash, so a same-group quality swap (new info hash) or a
// changed library state re-surfaces while an unchanged finding is suppressed.
func dedupeKey(f *Finding) string {
	groups := append([]string(nil), f.RecommendedGroups...)
	sort.Strings(groups)
	return strings.Join([]string{
		strconv.Itoa(f.AniListID),
		string(f.Status),
		strings.Join(groups, ","),
		f.CurrentGroup,
		f.InfoHash,
	}, "|")
}

// currentGroup renders the item's current group(s) for display: the single
// group, or a comma-joined list when the item spans several.
func currentGroup(item *library.Item) string {
	return strings.Join(item.Groups, ",")
}

// representative picks the headline recommended release: highest resolution,
// then a public tracker, then the first. It assumes len(pool) > 0.
func representative(pool []candidate) candidate {
	bestIdx := 0
	for i := 1; i < len(pool); i++ {
		if betterCandidate(&pool[i], &pool[bestIdx]) {
			bestIdx = i
		}
	}
	return pool[bestIdx]
}

// betterCandidate reports whether a should outrank b as the headline
// recommendation (higher resolution, then public-over-private tracker).
func betterCandidate(a, b *candidate) bool {
	ra, rb := release.ResolutionRank(a.rel.Resolution), release.ResolutionRank(b.rel.Resolution)
	if ra != rb {
		return ra > rb
	}
	return a.rel.TrackerType == release.TrackerPublic && b.rel.TrackerType != release.TrackerPublic
}

// groupSet returns the sorted distinct normalized groups of the given releases.
func groupSet(cands []candidate) []string {
	seen := make(map[string]struct{}, len(cands))
	var groups []string
	for i := range cands {
		g := release.NormalizeGroup(cands[i].rel.Group)
		if g == "" {
			continue
		}
		if _, dup := seen[g]; dup {
			continue
		}
		seen[g] = struct{}{}
		groups = append(groups, g)
	}
	sort.Strings(groups)
	return groups
}

// intersects reports whether any recommended group is present in the current
// group set (both normalized).
func intersects(recommended, current []string) bool {
	have := make(map[string]struct{}, len(current))
	for _, g := range current {
		have[release.NormalizeGroup(g)] = struct{}{}
	}
	for _, g := range recommended {
		if _, ok := have[release.NormalizeGroup(g)]; ok {
			return true
		}
	}
	return false
}

// fileNames returns the names of a torrent's files, for classification.
func fileNames(files []seadex.File) []string {
	names := make([]string, 0, len(files))
	for i := range files {
		if files[i].Name != "" {
			names = append(names, files[i].Name)
		}
	}
	return names
}
