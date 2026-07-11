// Package audit produces a full SeaDex-alignment report over the library: for
// every anime that has a matching SeaDex entry, what release you have and
// whether it is SeaDex's best, an alt, or unlisted. Unlike the daemon's
// report-by-exception findings, this enumerates everything.
//
// Matching is season-level: a SeaDex entry (one AniList ID = one cour/movie/
// special) is scoped to its TVDB season via the Fribb mapping and compared
// against that season's on-disk release groups. An item matched to a series but
// not resolvable to a season (no season mapping, or a title-only fallback) is
// reported as an unverified likely-match with no release validation.
package audit

import (
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// Verdict is the SeaDex-alignment classification of a library item's release.
type Verdict string

const (
	// VerdictBest means the on-disk release matches a SeaDex isBest release.
	VerdictBest Verdict = "have_best"
	// VerdictAlt means the on-disk release matches a listed non-best (alt) release.
	VerdictAlt Verdict = "have_alt"
	// VerdictUnlisted means the on-disk release matches nothing SeaDex lists.
	VerdictUnlisted Verdict = "have_unlisted"
	// VerdictNoFile means the item (or the mapped season) has no file on disk.
	VerdictNoFile Verdict = "no_file"
	// VerdictUnverified means the entry matched a series but was not resolvable
	// to a season/special, so no release validation was done (a likely match).
	VerdictUnverified Verdict = "unverified"
)

// verdictOrder is the report's most-actionable-first ordering.
var verdictOrder = []Verdict{VerdictUnlisted, VerdictAlt, VerdictUnverified, VerdictNoFile, VerdictBest}

// Release is one SeaDex torrent in a report row (best or alt), with a usable link.
type Release struct {
	Tracker string `json:"tracker"`
	Group   string `json:"group,omitempty"`
	URL     string `json:"url,omitempty"`
	Best    bool   `json:"best"`
}

// Row is one anime's alignment record.
type Row struct {
	Title         string    `json:"title"`
	Arr           string    `json:"arr"`
	ArrURL        string    `json:"arr_url,omitempty"`
	SeaDexURL     string    `json:"seadex_url"`
	Verdict       Verdict   `json:"verdict"`
	MatchSource   string    `json:"match_source"`
	CurrentGroups []string  `json:"current_groups,omitempty"`
	Releases      []Release `json:"releases,omitempty"`
	AniListID     int       `json:"al_id"`
	Season        int       `json:"season,omitempty"`
	Special       bool      `json:"special,omitempty"`
	Incomplete    bool      `json:"incomplete,omitempty"`
}

// Report is the full audit result.
type Report struct {
	GeneratedAt time.Time      `json:"generated_at"`
	Totals      map[string]int `json:"totals"`
	Rows        []Row          `json:"rows"`
}

// Config configures an Auditor.
type Config struct {
	Logger          *slog.Logger
	SeaDexBaseURL   string
	ExcludeSpecials bool
	AnimeBytes      bool
}

// Auditor builds alignment reports from matches.
type Auditor struct {
	log               *slog.Logger
	seadexBaseURL     string
	excludeSpecials   bool
	includeAnimeBytes bool
}

// NewAuditor builds an Auditor from cfg.
func NewAuditor(cfg Config) *Auditor {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Auditor{
		log:               log,
		seadexBaseURL:     cfg.SeaDexBaseURL,
		excludeSpecials:   cfg.ExcludeSpecials,
		includeAnimeBytes: cfg.AnimeBytes,
	}
}

// Audit produces the report from the matches (only in-library matches are
// included; specials are skipped when disabled).
func (a *Auditor) Audit(matches []match.Match) Report {
	rows := make([]Row, 0, len(matches))
	totals := make(map[string]int)
	for i := range matches {
		m := &matches[i]
		if !m.InLibrary() {
			continue
		}
		if a.excludeSpecials && m.Record.IsSpecial() {
			continue
		}
		row := a.assess(m)
		rows = append(rows, row)
		totals[string(row.Verdict)]++
	}
	sortRows(rows)
	return Report{GeneratedAt: time.Now(), Totals: totals, Rows: rows}
}

// assess builds one row: classify the entry's releases, scope the on-disk
// groups to the mapped season, and derive the verdict.
func (a *Auditor) assess(m *match.Match) Row {
	releases := a.classifyReleases(&m.Entry)
	best, alt := groupSets(releases)
	current, scoped, hasFile := scope(m)

	row := Row{
		CurrentGroups: current,
		Releases:      releases,
		Title:         m.Item.Title,
		Arr:           m.Arr,
		ArrURL:        m.Item.ArrURL,
		SeaDexURL:     a.seadexURL(m.Entry.AniListID),
		MatchSource:   string(m.Source),
		AniListID:     m.Entry.AniListID,
		Season:        m.Record.SeasonTvdb,
		Special:       m.Record.IsSpecial(),
		Incomplete:    m.Entry.Incomplete,
	}
	row.Verdict = verdict(scoped, hasFile, current, best, alt)
	return row
}

// verdict derives the alignment verdict from the scope outcome and group sets.
func verdict(scoped, hasFile bool, current, best, alt []string) Verdict {
	switch {
	case !scoped:
		return VerdictUnverified
	case !hasFile:
		return VerdictNoFile
	case intersects(current, best):
		return VerdictBest
	case intersects(current, alt):
		return VerdictAlt
	default:
		return VerdictUnlisted
	}
}

// scope returns the on-disk release groups to compare for this entry, whether
// scoping succeeded, and whether the scoped unit has any file. Movies scope to
// the movie's group; series scope to the mapped TVDB season's groups; a series
// with no season mapping (or a title-only match) cannot be scoped.
func scope(m *match.Match) (groups []string, scoped, hasFile bool) {
	item := m.Item
	if item.Arr == library.ArrRadarr {
		return item.Groups, true, item.HasFile
	}
	season := m.Record.SeasonTvdb
	if season <= 0 {
		return nil, false, false
	}
	g, ok := item.SeasonGroups[season]
	if !ok {
		return nil, true, false // season is mapped but not present on disk
	}
	return g, true, len(g) > 0
}

// classifyReleases turns every SeaDex torrent into a report Release (group
// normalized via the shared classifier, tracker, usable URL, best flag).
// AnimeBytes torrents are dropped when the operator has AnimeBytes off, so the
// report never surfaces AB releases or links they cannot use (and cannot leak
// them), mirroring the daemon's obtainability rule.
func (a *Auditor) classifyReleases(entry *seadex.Entry) []Release {
	out := make([]Release, 0, len(entry.Torrents))
	for i := range entry.Torrents {
		t := &entry.Torrents[i]
		if !a.includeAnimeBytes && release.IsAnimeBytes(t.Tracker) {
			continue
		}
		rel := release.Classify(&release.Input{
			Names:     torrentFileNames(t.Files),
			Notes:     entry.Notes,
			Group:     t.ReleaseGroup,
			Tracker:   t.Tracker,
			DualAudio: t.DualAudio,
		})
		out = append(out, Release{
			Tracker: t.Tracker,
			Group:   rel.Group,
			URL:     t.UsableURL(),
			Best:    t.IsBest,
		})
	}
	return out
}

// seadexURL builds the releases.moe entry link for an AniList ID.
func (a *Auditor) seadexURL(aniListID int) string {
	base := strings.TrimRight(a.seadexBaseURL, "/")
	return base + "/" + strconv.Itoa(aniListID)
}

// groupSets returns the distinct normalized groups among the best and the alt
// releases.
func groupSets(releases []Release) (best, alt []string) {
	bestSeen, altSeen := map[string]struct{}{}, map[string]struct{}{}
	for i := range releases {
		g := release.NormalizeGroup(releases[i].Group)
		if g == "" {
			continue
		}
		if releases[i].Best {
			addUnique(bestSeen, &best, g)
		} else {
			addUnique(altSeen, &alt, g)
		}
	}
	return best, alt
}

// addUnique appends g to out if not already seen.
func addUnique(seen map[string]struct{}, out *[]string, g string) {
	if _, ok := seen[g]; ok {
		return
	}
	seen[g] = struct{}{}
	*out = append(*out, g)
}

// intersects reports whether any of a is present in b (both normalized).
func intersects(a, b []string) bool {
	set := make(map[string]struct{}, len(b))
	for _, g := range b {
		set[release.NormalizeGroup(g)] = struct{}{}
	}
	for _, g := range a {
		if _, ok := set[release.NormalizeGroup(g)]; ok {
			return true
		}
	}
	return false
}

// sortRows orders rows by verdict actionability, then title.
func sortRows(rows []Row) {
	rank := make(map[Verdict]int, len(verdictOrder))
	for i, v := range verdictOrder {
		rank[v] = i
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Verdict != rows[j].Verdict {
			return rank[rows[i].Verdict] < rank[rows[j].Verdict]
		}
		return strings.ToLower(rows[i].Title) < strings.ToLower(rows[j].Title)
	})
}

// torrentFileNames returns the non-empty file names of a torrent.
func torrentFileNames(files []seadex.File) []string {
	names := make([]string, 0, len(files))
	for i := range files {
		if files[i].Name != "" {
			names = append(names, files[i].Name)
		}
	}
	return names
}
