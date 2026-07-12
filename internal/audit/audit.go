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
	"github.com/cplieger/seadex-scout/internal/mapping"
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
	// VerdictUnverified means the item has files on disk but none carried an
	// identifiable release group, so there was nothing to compare. Most former
	// unverified rows (specials, absolute-numbered runs) now resolve via the
	// season-0 and whole-series fallbacks in scope.
	VerdictUnverified Verdict = "unverified"
	// VerdictNotOnSeaDex means the item is in the library and recognized as anime
	// (present in the Fribb map) but SeaDex lists no entry for it, so there is no
	// recommendation to compare against. These rows carry no SeaDex entry.
	VerdictNotOnSeaDex Verdict = "not_on_seadex"
)

// verdictOrder is the report's most-actionable-first ordering. not_on_seadex is
// last: it is informational (no SeaDex recommendation exists to act on).
var verdictOrder = []Verdict{VerdictUnlisted, VerdictAlt, VerdictUnverified, VerdictNoFile, VerdictBest, VerdictNotOnSeaDex}

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
	// Approx marks a coarse comparison: the on-disk groups came from the season-0
	// specials bucket or the whole-series fallback and hold more than one group,
	// so the verdict reflects "this group is present somewhere in the series/
	// specials" rather than an exact per-season/per-special attribution.
	Approx bool `json:"approx,omitempty"`
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

// Audit produces the report: one row per in-library SeaDex match (specials
// skipped when disabled), plus one not_on_seadex row per library item that is
// recognized anime (in the Fribb map) but has no SeaDex entry. snap and idx may
// be nil, in which case the not_on_seadex section is empty.
func (a *Auditor) Audit(matches []match.Match, snap *library.Snapshot, idx *mapping.Index) Report {
	rows := make([]Row, 0, len(matches))
	covered := make(map[string]struct{})
	for i := range matches {
		m := &matches[i]
		if !m.InLibrary() {
			continue
		}
		covered[itemKey(m.Item)] = struct{}{}
		if a.excludeSpecials && m.Record.IsSpecial() {
			continue
		}
		rows = append(rows, a.assess(m))
	}
	rows = append(rows, uncoveredRows(snap, idx, covered)...)

	totals := make(map[string]int, len(verdictOrder))
	for i := range rows {
		totals[string(rows[i].Verdict)]++
	}
	sortRows(rows)
	return Report{GeneratedAt: time.Now(), Totals: totals, Rows: rows}
}

// itemKey identifies a library item by arr and arr id.
func itemKey(it *library.Item) string {
	return it.Arr + ":" + strconv.Itoa(it.ArrID)
}

// uncoveredRows lists library items that are recognized anime (present in the
// Fribb map) but were not covered by any SeaDex match. The Fribb catalogue
// filter is what keeps this to genuine anime gaps rather than every non-anime
// item in the arrs.
func uncoveredRows(snap *library.Snapshot, idx *mapping.Index, covered map[string]struct{}) []Row {
	if snap == nil {
		return nil
	}
	cat := newCatalogue(idx)
	var rows []Row
	for i := range snap.Items {
		it := &snap.Items[i]
		if _, ok := covered[itemKey(it)]; ok {
			continue
		}
		if !cat.has(it) {
			continue
		}
		rows = append(rows, Row{
			Title:         it.Title,
			Arr:           it.Arr,
			ArrURL:        it.ArrURL,
			Verdict:       VerdictNotOnSeaDex,
			CurrentGroups: it.Groups,
		})
	}
	return rows
}

// catalogue is a reverse (arr-ID) lookup over the Fribb map: the set of TVDB,
// TMDB-movie, and IMDb IDs any record references, used to tell a recognized
// anime from an arbitrary library entry.
type catalogue struct {
	tvdb map[int]struct{}
	tmdb map[int]struct{}
	imdb map[string]struct{}
}

// newCatalogue builds the reverse ID sets from the mapping records. A nil index
// yields an empty catalogue (nothing is considered catalogued).
func newCatalogue(idx *mapping.Index) *catalogue {
	c := &catalogue{tvdb: map[int]struct{}{}, tmdb: map[int]struct{}{}, imdb: map[string]struct{}{}}
	for _, r := range idx.Records() {
		if r.TvdbID != 0 {
			c.tvdb[r.TvdbID] = struct{}{}
		}
		for _, id := range r.TmdbMovies {
			c.tmdb[id] = struct{}{}
		}
		for _, im := range r.IMDbIDs {
			c.imdb[im] = struct{}{}
		}
	}
	return c
}

// has reports whether a library item corresponds to any Fribb record: a Radarr
// movie by its TMDB or IMDb id, a Sonarr series by its TVDB id.
func (c *catalogue) has(it *library.Item) bool {
	if it.Arr == library.ArrRadarr {
		if it.TmdbID != 0 {
			if _, ok := c.tmdb[it.TmdbID]; ok {
				return true
			}
		}
		if it.ImdbID != "" {
			if _, ok := c.imdb[it.ImdbID]; ok {
				return true
			}
		}
		return false
	}
	if it.TvdbID == 0 {
		return false
	}
	_, ok := c.tvdb[it.TvdbID]
	return ok
}

// assess builds one row: classify the entry's releases, scope the on-disk
// groups to the mapped season, and derive the verdict.
func (a *Auditor) assess(m *match.Match) Row {
	releases := a.classifyReleases(&m.Entry)
	best, alt := groupSets(releases)
	current, hasFile, approx := scope(m)

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
		Approx:        approx,
	}
	row.Verdict = verdict(hasFile, current, best, alt)
	return row
}

// verdict derives the alignment verdict from the scoped group set. No file is
// no_file; a file with no identifiable group is unverified; otherwise the
// current group is matched against the best then the alt release groups.
func verdict(hasFile bool, current, best, alt []string) Verdict {
	switch {
	case !hasFile:
		return VerdictNoFile
	case len(current) == 0:
		return VerdictUnverified
	case intersects(current, best):
		return VerdictBest
	case intersects(current, alt):
		return VerdictAlt
	default:
		return VerdictUnlisted
	}
}

// specialSeason is the TVDB season number Sonarr files specials under.
const specialSeason = 0

// scope returns the on-disk release groups to compare for this entry, whether
// the scoped unit has any file, and whether the comparison is approximate (a
// coarse multi-group bucket).
//
// Movies scope to the movie's group. A series with a positive Fribb TVDB season
// scopes to that season's groups (exact). A special with no positive season
// scopes to the season-0 specials bucket Sonarr lumps them into. Any other
// series with no season mapping (an absolute-numbered long run, or a title-only
// match with no record) falls back to the whole-series group set. The season-0
// and whole-series buckets are approximate when they hold more than one group,
// since a single entry cannot be attributed to one of them.
func scope(m *match.Match) (groups []string, hasFile, approx bool) {
	item := m.Item
	if item.Arr == library.ArrRadarr {
		return item.Groups, item.HasFile, false
	}
	if season := m.Record.SeasonTvdb; season > 0 {
		g, ok := item.SeasonGroups[season]
		if !ok {
			return nil, false, false // season is mapped but not present on disk
		}
		return g, len(g) > 0, false
	}
	if m.Record.IsSpecial() {
		g, ok := item.SeasonGroups[specialSeason]
		if !ok {
			return nil, false, false // no specials present on disk
		}
		return g, len(g) > 0, len(g) > 1
	}
	// No TVDB season (absolute-numbered run or title-only match): fall back to the
	// whole-series group set.
	return item.Groups, item.HasFile, len(item.Groups) > 1
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
