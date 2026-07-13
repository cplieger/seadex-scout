// Package compare turns matched SeaDex entries into findings. For each entry in
// the library it classifies the SeaDex "best" releases, keeps those that pass
// the content filters (remux/dual-audio) AND are obtainable (on a public
// tracker, or on AnimeBytes when the operator enables it), and compares the
// surviving recommended release groups against the groups present on the
// library item. The comparison is season-scoped: a SeaDex entry (one AniList
// ID = one cour) is compared as the audit report scopes it (via internal/align):
// a mapped TVDB season against that season's groups, a special against Sonarr's
// season-0 bucket, a movie against its groups, and an absolute-numbered or
// title-only run against every real season conservatively -
// so a later season that needs a better release is not masked by an earlier
// season that already has it. An item that already has a recommended group is
// aligned and produces no finding; a recommended release the operator cannot
// obtain is simply absent, never a finding.
package compare

import (
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/classify"
	"github.com/cplieger/seadex-scout/internal/filter"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// Status is the comparison outcome for a finding.
type Status string

const (
	// StatusBetter means SeaDex recommends a release group the library lacks,
	// obtainable on a tracker the operator uses.
	StatusBetter Status = "better_release"
	// StatusMixedGroup means the series' episodes span multiple groups; a manual
	// review nudge rather than a false "better release".
	StatusMixedGroup Status = "mixed_group_manual"
	// StatusIncomplete means the SeaDex entry is incomplete; nothing complete to grab.
	StatusIncomplete Status = "incomplete"
	// StatusTheoretical means the entry only names a theoretical best (not muxed).
	StatusTheoretical Status = "theoretical_best"
)

// Severity is the log level a finding maps to.
type Severity string

const (
	// SevWarn is an actionable finding (a better release to go get).
	SevWarn Severity = "warn"
	// SevInfo is an informational finding (nothing directly actionable).
	SevInfo Severity = "info"
)

// ReleaseLink is one obtainable source for a recommended release: the tracker
// and a human-followable URL. A recommended group present on both a public
// tracker and AnimeBytes yields two links, so a finding can surface both.
type ReleaseLink struct {
	Tracker string `json:"tracker"`
	URL     string `json:"url"`
}

// Finding is one comparison result for a library item. It carries the fields
// the report layer emits and the dedupe key that suppresses re-alerts.
type Finding struct {
	Kind              string        `json:"kind,omitempty"`
	Reason            string        `json:"classification_reason,omitempty"`
	Arr               string        `json:"arr"`
	CurrentGroup      string        `json:"current_group,omitempty"`
	RecommendedGroup  string        `json:"recommended_group,omitempty"`
	Tracker           string        `json:"tracker,omitempty"`
	Title             string        `json:"title"`
	Resolution        string        `json:"resolution,omitempty"`
	Severity          Severity      `json:"severity"`
	Codec             string        `json:"codec,omitempty"`
	ReleaseURL        string        `json:"release_url,omitempty"`
	ArrURL            string        `json:"arr_url,omitempty"`
	InfoHash          string        `json:"info_hash,omitempty"`
	DedupeKey         string        `json:"dedupe_key"`
	Status            Status        `json:"status"`
	RecommendedGroups []string      `json:"recommended_groups,omitempty"`
	Links             []ReleaseLink `json:"links,omitempty"`
	AniListID         int           `json:"al_id"`
	Season            int           `json:"season,omitempty"`
	DualAudio         bool          `json:"dual_audio,omitempty"`
}

// Comparer produces findings from matches under a fixed filter policy.
type Comparer struct {
	log             *slog.Logger
	opts            filter.Options
	excludeSpecials bool
}

// Config configures a Comparer.
type Config struct {
	Logger          *slog.Logger
	Filter          filter.Options
	ExcludeSpecials bool
}

// NewComparer builds a Comparer from cfg.
func NewComparer(cfg Config) *Comparer {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Comparer{
		log:             log,
		opts:            cfg.Filter,
		excludeSpecials: cfg.ExcludeSpecials,
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
		if c.excludeSpecials && m.Record.IsSpecial() {
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

// compareOne compares one matched, in-library entry and returns a finding, or
// nil when the item is aligned (already has a recommended group).
func (c *Comparer) compareOne(m *match.Match) *Finding {
	entry := &m.Entry
	recommended := c.recommended(entry)

	// A Sonarr absolute-numbered run / title-only match has no per-season Fribb
	// mapping, so its single whole-series recommendation is compared against every
	// real season on disk, conservatively (compareWholeSeries) - exactly as the
	// audit report does.
	if align.WholeSeries(m.Item, &m.Record) {
		return c.compareWholeSeries(m, recommended)
	}

	// Scope the on-disk groups the same way the audit report does (movie / the
	// mapped TVDB season / the season-0 specials bucket), via the shared
	// internal/align, so a daemon finding never disagrees with the report.
	currentGroups, hasFile, _ := align.Scope(m.Item, &m.Record)
	base := c.baseFinding(m, currentGroups)
	if len(recommended) == 0 {
		return emptyResult(entry, &base)
	}
	if !hasFile {
		// The mapped season/movie/special is not on disk, so there is nothing the
		// operator has for a better release to replace. The audit records this as
		// no_file; the daemon stays quiet.
		return nil
	}

	recGroups := groupSet(recommended)
	// Use the scoped group count, not the whole-item group set, so a season that
	// carries a single group is not misreported as mixed_group_manual.
	if len(currentGroups) > 1 {
		fillBest(&base, recommended, recGroups)
		return finalize(&base, StatusMixedGroup, SevInfo)
	}
	if release.GroupsIntersect(recGroups, currentGroups) {
		return nil // aligned: a recommended group is already present
	}

	// Not aligned: a better release the operator can obtain and lacks. An
	// incomplete entry is a non-actionable info nudge (nothing complete to grab).
	status, sev := StatusBetter, SevWarn
	if entry.Incomplete {
		status, sev = StatusIncomplete, SevInfo
	}
	fillBest(&base, recommended, recGroups)
	return finalize(&base, status, sev)
}

// compareWholeSeries compares a Sonarr whole-series entry (an absolute-numbered
// run or title-only match, with no per-season Fribb mapping) against every real
// season on disk (season 0 excluded), conservatively: the item is aligned only
// when every on-disk season already carries a recommended group, matching the
// audit report's whole-series verdict via the shared align.SummarizeWholeSeries.
// It stays silent when no real season is on disk.
func (c *Comparer) compareWholeSeries(m *match.Match, recommended []candidate) *Finding {
	entry := &m.Entry
	recGroups := groupSet(recommended)
	// nil alt: the daemon only distinguishes best-vs-not, so an on-disk season
	// lacking a recommended group surfaces as AnyUnlisted.
	summary := align.SummarizeWholeSeries(m.Item, recGroups, nil)
	base := c.baseFinding(m, summary.Groups)

	if len(recommended) == 0 {
		return emptyResult(entry, &base)
	}
	if summary.Seasons == 0 {
		return nil // no real season on disk: nothing for a better release to replace
	}
	if !summary.AnyUnlisted {
		return nil // every on-disk season already carries a recommended group
	}

	// At least one on-disk season lacks a recommended group.
	status, sev := StatusBetter, SevWarn
	if entry.Incomplete {
		status, sev = StatusIncomplete, SevInfo
	}
	fillBest(&base, recommended, recGroups)
	return finalize(&base, status, sev)
}

// recommended classifies the entry's SeaDex "best" torrents and returns those
// the operator could act on: passing the content filters (remux/resolution/
// dual-audio) AND obtainable (a public tracker, or AnimeBytes when enabled).
func (c *Comparer) recommended(entry *seadex.Entry) []candidate {
	var out []candidate
	for i := range entry.Torrents {
		t := &entry.Torrents[i]
		if !t.IsBest {
			continue
		}
		rel := classify.Torrent(entry, t)
		if ok, _ := filter.KeepNonTracker(&rel, c.opts); !ok {
			continue
		}
		if !filter.Obtainable(&rel, c.opts) {
			continue
		}
		out = append(out, candidate{rel: rel, torrent: *t})
	}
	return out
}

// emptyResult decides the finding when no recommended release survives the
// content and obtainability filters: a theoretical-best-only or incomplete
// entry is an info nudge, everything else (nothing the operator can get) is
// silent.
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

// baseFinding seeds a finding with the item identity fields, using the
// season-scoped current groups already resolved by the caller so the finding's
// CurrentGroup and dedupe key never leak whole-series groups.
func (c *Comparer) baseFinding(m *match.Match, groups []string) Finding {
	return Finding{
		Title:        m.Item.Title,
		Arr:          m.Arr,
		ArrURL:       m.Item.ArrURL,
		CurrentGroup: strings.Join(groups, ","),
		AniListID:    m.Entry.AniListID,
		Season:       m.Record.SeasonTvdb,
	}
}

// fillBest sets the recommended-release fields from the headline candidate of
// pool (highest resolution, public tracker preferred) plus the full group set
// and every obtainable link, so a release on both Nyaa and AnimeBytes surfaces
// both.
func fillBest(f *Finding, pool []candidate, recGroups []string) {
	rep := representative(pool)
	fillFromCandidate(f, &rep)
	f.RecommendedGroups = recGroups
	f.Links = obtainableLinks(pool)
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
	f.DualAudio = cand.rel.DualAudio
}

// obtainableLinks returns the distinct (tracker, URL) links across the pool,
// deduped, preserving pool order. This is what lets a finding surface both a
// Nyaa and an AnimeBytes link for the same recommended release.
func obtainableLinks(pool []candidate) []ReleaseLink {
	seen := make(map[string]struct{}, len(pool))
	var links []ReleaseLink
	for i := range pool {
		u := pool[i].torrent.UsableURL()
		if u == "" {
			continue
		}
		key := pool[i].rel.Tracker + "|" + u
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		links = append(links, ReleaseLink{Tracker: pool[i].rel.Tracker, URL: u})
	}
	return links
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
