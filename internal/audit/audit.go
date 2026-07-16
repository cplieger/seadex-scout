// Package audit produces a full SeaDex-alignment report over the library: for
// every anime that has a matching SeaDex entry, what release you have and
// whether it is SeaDex's best, an alt, or unlisted. Unlike the daemon's
// report-by-exception findings, this enumerates everything.
//
// Matching is season-level: a SeaDex entry (one AniList ID = one cour/movie/
// special) is scoped to its TVDB season via the Fribb mapping and compared
// against that season's on-disk release groups. Specials without a positive
// TVDB season compare against Sonarr's season-0 bucket, and seasonless
// non-special series are compared conservatively across the real seasons on
// disk. A row is unverified only when files are present but no comparable
// release group can be identified.
package audit

import (
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/classify"
	"github.com/cplieger/seadex-scout/internal/filter"
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

// Qualifier annotates a row's verdict with the daemon's finding vocabulary for
// the same (item, entry), so the report and the daemon's compare pass tell one
// story. A qualifier annotates; it never forks the verdict enum - the verdict
// stays what the group comparison said.
type Qualifier string

const (
	// QualifierMixed marks a row where the daemon would emit
	// mixed_group_manual: the scoped on-disk groups span more than one group
	// and none of them is a SeaDex best, so the row is a manual review rather
	// than a clean single-group divergence.
	QualifierMixed Qualifier = "mixed"
	// QualifierTheoretical marks a row whose SeaDex entry names only a
	// theoretical best (no isBest torrents), so its verdict means "SeaDex
	// lists nothing concrete to compare against", not "you have something
	// better than what SeaDex lists" - the daemon's theoretical_best.
	QualifierTheoretical Qualifier = "theoretical"
	// QualifierIncomplete marks a row whose SeaDex entry is incomplete and
	// lists no isBest torrents at all (nothing recommended) - the daemon's
	// incomplete status.
	QualifierIncomplete Qualifier = "incomplete"
)

// Release is one SeaDex torrent in a report row (best or alt), with a usable link.
type Release struct {
	Tracker string `json:"tracker"`
	Group   string `json:"group,omitempty"`
	URL     string `json:"url,omitempty"`
	Best    bool   `json:"best"`
}

// Row is one anime's alignment record.
type Row struct {
	Title     string  `json:"title"`
	Arr       string  `json:"arr"`
	ArrURL    string  `json:"arr_url,omitempty"`
	SeaDexURL string  `json:"seadex_url"`
	Verdict   Verdict `json:"verdict"`
	// Qualifier is the daemon-vocabulary annotation for the row
	// (mixed/theoretical/incomplete), empty when none applies.
	Qualifier     Qualifier `json:"qualifier,omitempty"`
	MatchSource   string    `json:"match_source"`
	CurrentGroups []string  `json:"current_groups,omitempty"`
	Releases      []Release `json:"releases,omitempty"`
	AniListID     int       `json:"al_id"`
	Season        int       `json:"season,omitempty"`
	// scope is the comparison scope align.Scope resolved, recorded at build
	// time and read by the renderer (align.ScopeWholeSeries, the zero value,
	// renders as "series"). Unexported: in-process only, absent from the JSON
	// wire shape.
	scope      align.ScopeKind
	Special    bool `json:"special,omitempty"`
	Incomplete bool `json:"incomplete,omitempty"`
	// Approx marks a coarse comparison: the season-0 specials bucket held more
	// than one group, or the whole-series fallback compared more than one real
	// season, so the verdict reflects "this group is present somewhere in the
	// series/specials" rather than an exact per-season/per-special attribution.
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
		if filter.ExcludeSpecial(m.Record.IsSpecial(), a.excludeSpecials) {
			continue
		}
		rows = append(rows, a.assess(m))
	}
	rows = append(rows, uncoveredRows(snap, idx, covered, a.excludeSpecials)...)

	totals := make(map[string]int, len(verdictOrder))
	for i := range rows {
		totals[string(rows[i].Verdict)]++
	}
	sortRows(rows)
	return Report{GeneratedAt: time.Now().UTC(), Totals: totals, Rows: rows}
}

// itemKey identifies a library item by arr and arr id.
func itemKey(it *library.Item) string {
	return it.Arr + ":" + strconv.Itoa(it.ArrID)
}

// uncoveredRows lists library items that are recognized anime (present in the
// Fribb map) but were not covered by any SeaDex match. The Fribb catalogue
// filter is what keeps this to genuine anime gaps rather than every non-anime
// item in the arrs.
func uncoveredRows(snap *library.Snapshot, idx *mapping.Index, covered map[string]struct{}, excludeSpecials bool) []Row {
	if snap == nil {
		return nil
	}
	cat := newCatalogue(idx, excludeSpecials)
	var rows []Row
	for i := range snap.Items {
		it := &snap.Items[i]
		if _, ok := covered[itemKey(it)]; ok {
			continue
		}
		if !cat.has(it) {
			continue
		}
		// An uncovered item has no Fribb record, so its scope label resolves
		// through the shared align.Scope dispatch with the zero record (Radarr
		// -> movie; a seasonless non-special Sonarr series -> whole-series),
		// rather than re-deriving that arm choice locally where it could drift
		// from the protocol align owns.
		rows = append(rows, Row{
			Title:         it.Title,
			Arr:           it.Arr,
			ArrURL:        it.ArrURL,
			Verdict:       VerdictNotOnSeaDex,
			CurrentGroups: it.Groups,
			scope:         align.Scope(it, &mapping.Record{}).Kind,
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
// yields an empty catalogue (nothing is considered catalogued). When
// excludeSpecials is on, special (OVA/ONA/SPECIAL) records are skipped, so a
// specials-only item is not catalogued and cannot surface as not_on_seadex —
// mirroring the matched-rows arm's specials filter. A mixed series stays
// catalogued through its non-special records sharing the same TVDB id.
func newCatalogue(idx *mapping.Index, excludeSpecials bool) *catalogue {
	c := &catalogue{tvdb: map[int]struct{}{}, tmdb: map[int]struct{}{}, imdb: map[string]struct{}{}}
	idx.ForEachRecord(func(r mapping.Record) {
		if filter.ExcludeSpecial(r.IsSpecial(), excludeSpecials) {
			return
		}
		// Insert only the identifiers the record's routed arr consumes
		// (mapping.Record.RoutedIDs): a MOVIE record must not catalogue a
		// Sonarr item through a stray TVDB id, nor a series record a Radarr
		// item through its movie ids.
		tvdb, tmdbMovies, imdbIDs := r.RoutedIDs()
		if tvdb != 0 {
			c.tvdb[tvdb] = struct{}{}
		}
		for _, id := range tmdbMovies {
			c.tmdb[id] = struct{}{}
		}
		for _, im := range imdbIDs {
			c.imdb[im] = struct{}{}
		}
	})
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

	row := Row{
		Releases:    releases,
		Title:       m.Item.Title,
		Arr:         m.Arr,
		ArrURL:      m.Item.ArrURL,
		SeaDexURL:   a.seadexURL(m.Entry.AniListID),
		MatchSource: string(m.Source),
		AniListID:   m.Entry.AniListID,
		Season:      m.Record.SeasonTvdb,
		Special:     m.Record.IsSpecial(),
		Incomplete:  m.Entry.Incomplete,
	}
	scoped := align.Scope(m.Item, &m.Record)
	row.scope = scoped.Kind
	if scoped.Kind == align.ScopeWholeSeries {
		// Absolute-numbered run / title-only match: apply the single whole-series
		// recommendation to each real season (season 0 excluded), conservatively.
		row.Verdict, row.CurrentGroups, row.Approx = wholeSeriesVerdict(m.Item, best, alt)
	} else {
		row.CurrentGroups, row.Approx = scoped.Groups, scoped.Approx
		row.Verdict = verdict(scoped.HasFile, scoped.Groups, best, alt)
	}
	row.Qualifier = rowQualifier(&m.Entry, best, row.Verdict, row.CurrentGroups)
	return row
}

// rowQualifier derives the daemon-vocabulary qualifier for a row, so the report
// distinguishes the states the daemon's compare pass distinguishes. With no
// best release listed at all, a theoretical-best-only entry is "theoretical"
// and an incomplete one "incomplete" (mirroring the daemon's emptyResult
// precedence) - the row's verdict would otherwise imply an unlisted-better
// state that does not exist. With best releases listed, a not-aligned row
// (have_alt / have_unlisted) whose scoped groups span more than one group is
// "mixed", where the daemon emits mixed_group_manual. An aligned row is never
// mixed, matching the daemon's alignment-wins ordering.
func rowQualifier(entry *seadex.Entry, best []string, v Verdict, current []string) Qualifier {
	if len(best) == 0 {
		switch {
		case entry.HasTheoreticalBest():
			return QualifierTheoretical
		case entry.Incomplete:
			return QualifierIncomplete
		}
		return ""
	}
	if (v == VerdictAlt || v == VerdictUnlisted) && len(current) > 1 {
		return QualifierMixed
	}
	return ""
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
	case release.GroupsIntersect(current, best):
		return VerdictBest
	case release.GroupsIntersect(current, alt):
		return VerdictAlt
	default:
		return VerdictUnlisted
	}
}

// wholeSeriesVerdict applies the entry's single recommendation to each real
// season (season 0 specials excluded) and returns the most conservative
// verdict: have_best only when every on-disk season carries a best group,
// have_alt when all are best-or-alt with at least one alt, and have_unlisted
// when any season matches neither. It also returns the union of those seasons'
// groups for display and marks the comparison approximate when it spans more
// than one season or more than one release group (either way the single
// whole-series recommendation applies to a coarse aggregate). With no real
// season on disk it is no_file.
func wholeSeriesVerdict(item *library.Item, best, alt []string) (Verdict, []string, bool) {
	s := align.SummarizeWholeSeries(item, best, alt)
	if s.Seasons == 0 {
		return VerdictNoFile, nil, false
	}
	approx := s.Seasons > 1 || len(s.Groups) > 1
	switch {
	case s.AnyUnlisted:
		return VerdictUnlisted, s.Groups, approx
	case s.AnyAlt:
		return VerdictAlt, s.Groups, approx
	default:
		return VerdictBest, s.Groups, approx
	}
}

// classifyReleases turns every SeaDex torrent into a report Release (group
// normalized via the shared classifier, tracker, usable URL, best flag).
// AnimeBytes torrents are dropped when the operator has AnimeBytes off —
// whether identified by the tracker label OR by the URL host, since the label
// is untrusted upstream data — so the report never surfaces AB releases or
// links they cannot use (and cannot leak them), mirroring the daemon's
// obtainability rule.
func (a *Auditor) classifyReleases(entry *seadex.Entry) []Release {
	out := make([]Release, 0, len(entry.Torrents))
	for i := range entry.Torrents {
		t := &entry.Torrents[i]
		// AB guard on the raw upstream URL; the invariant lives in
		// classify.ABVisible (the rendered Release below still carries the
		// usable link).
		if !classify.ABVisible(t, a.includeAnimeBytes) {
			continue
		}
		rel := classify.Torrent(entry, t)
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
