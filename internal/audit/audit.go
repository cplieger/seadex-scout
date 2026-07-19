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
// disk. A row is unverified when the comparison is unverifiable: files are
// present but the release-group evidence on either side is unknown (the
// release.NoGroup sentinel), so alignment can be neither proven nor refuted.
//
// A run degraded by a transient AniList failure is not withheld: the report
// renders with an explicit incomplete-mapping section listing the affected
// entries by AniList id (Report.Incomplete) and a completeness caveat in the
// Markdown header, so the unaffected majority still audits.
package audit

import (
	"cmp"
	"slices"
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
	// VerdictUnverified means the item has files on disk but the comparison
	// is unverifiable: the release-group evidence on at least one side is
	// unknown (an on-disk file with no identifiable group, or a SeaDex
	// release with no group tag - both carried as the release.NoGroup
	// sentinel) and could hide the very match being tested, so neither
	// have_best/have_alt nor a divergence can honestly be claimed.
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
	// QualifierIncomplete marks a row whose SeaDex entry is incomplete: either
	// it lists no isBest torrents at all (nothing recommended), or a listed
	// best is not aligned with the single on-disk group - both the daemon's
	// incomplete status.
	QualifierIncomplete Qualifier = "incomplete"
)

// Release is one SeaDex torrent in a report row (best or alt). URL is
// empty when the upstream link fails usable-link validation.
type Release struct {
	Tracker string `json:"tracker"`
	Group   string `json:"group,omitempty"`
	URL     string `json:"url,omitempty"`
	// Warnings carries the canonical curation-warning tags (broken,
	// incomplete) SeaDex curators put on the release, when any. A warned
	// release stays listed - the report enumerates raw SeaDex data - but it
	// is excluded from the verdict's best/alt group sets and from the grab
	// links, rendering with the warning marker instead (see groupSets and
	// the render layer).
	Warnings []string `json:"warnings,omitempty"`
	Best     bool     `json:"best"`
	// Unobtainable marks a release the daemon's obtainability rule
	// (filter.Obtainable) rejects as verdict evidence: no usable link, or a
	// tracker the operator cannot use. Like a curation-warned release it
	// stays listed - the report enumerates raw SeaDex data - but it drives
	// neither the verdict's group sets nor the grab links, rendering with an
	// "(unobtainable)" annotation instead (see groupSets and the render
	// layer). Serialized so machine consumers can see WHY a visible best did
	// not drive the verdict; omitted on the common obtainable release, so a
	// fully obtainable row's JSON shape is unchanged.
	Unobtainable bool `json:"unobtainable,omitempty"`
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
	// scope is the comparison scope the shared decision resolved (align.Scope
	// via align.Decide), recorded at build time and read by the renderer
	// (align.ScopeWholeSeries, the zero value, renders as "series").
	// Unexported: in-process only, absent from the JSON wire shape.
	scope      align.ScopeKind
	Special    bool `json:"special,omitempty"`
	Incomplete bool `json:"incomplete,omitempty"`
	// Approx marks a coarse comparison: the season-0 specials bucket held more
	// than one group, or the whole-series fallback compared more than one real
	// season, so the verdict reflects "this group is present somewhere in the
	// series/specials" rather than an exact per-season/per-special attribution.
	Approx bool `json:"approx,omitempty"`
}

// IncompleteEntry is one SeaDex entry whose library mapping could not be
// resolved this run: the AniList lookup that would link it to a library item
// failed transiently, so whether (and where) it maps into the library is
// unknown. It renders in the report's incomplete-mapping section; a row for it
// may be missing from (or misfiled in) the verdict sections.
type IncompleteEntry struct {
	SeaDexURL string `json:"seadex_url"`
	AniListID int    `json:"al_id"`
}

// Report is the full audit result.
type Report struct {
	GeneratedAt time.Time      `json:"generated_at"`
	Totals      map[string]int `json:"totals"`
	Rows        []Row          `json:"rows"`
	// Incomplete lists the SeaDex entries whose library mapping could not be
	// resolved this run (a transient AniList failure), sorted by AniList id.
	// Non-empty, it is the machine-readable completeness caveat: the verdict
	// rows cover everything else, but these entries' alignment is unknown.
	// Empty on a fully resolved run, and omitted from the JSON so a healthy
	// report's shape is unchanged.
	Incomplete []IncompleteEntry `json:"incomplete_mappings,omitempty"`
}

// Config configures an Auditor.
type Config struct {
	SeaDexBaseURL   string
	ExcludeSpecials bool
	AnimeBytes      bool
}

// Auditor builds alignment reports from matches.
type Auditor struct {
	seadexBaseURL     string
	excludeSpecials   bool
	includeAnimeBytes bool
}

// NewAuditor builds an Auditor from cfg.
func NewAuditor(cfg Config) *Auditor {
	return &Auditor{
		seadexBaseURL:     cfg.SeaDexBaseURL,
		excludeSpecials:   cfg.ExcludeSpecials,
		includeAnimeBytes: cfg.AnimeBytes,
	}
}

// Audit produces the report: one row per in-library SeaDex match (specials
// skipped when disabled), plus one not_on_seadex row per library item that is
// recognized anime (in the Fribb map) but has no SeaDex entry. snap and idx may
// be nil, in which case the not_on_seadex section is empty. incompleteIDs
// carries the AniList ids whose needed lookup failed transiently this run
// (match.Result.IncompleteIDs); they render as the report's incomplete-mapping
// section. Nil or empty on a fully resolved run, leaving the section absent.
func (a *Auditor) Audit(matches []match.Match, snap *library.Snapshot, idx *mapping.Index, incompleteIDs map[int]struct{}) Report {
	rows := make([]Row, 0, len(matches))
	covered := make(map[string]struct{})
	for i := range matches {
		m := &matches[i]
		if !m.InLibrary() {
			continue
		}
		covered[m.Item.Key()] = struct{}{}
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
	return Report{GeneratedAt: time.Now().UTC(), Totals: totals, Rows: rows, Incomplete: a.incompleteEntries(incompleteIDs)}
}

// incompleteEntries renders the transiently-unresolved AniList ids as the
// report's incomplete-mapping section, sorted by id for a stable render, each
// carrying its releases.moe link. Nil on a fully resolved run so the section
// (and the JSON key) is omitted entirely.
func (a *Auditor) incompleteEntries(ids map[int]struct{}) []IncompleteEntry {
	if len(ids) == 0 {
		return nil
	}
	out := make([]IncompleteEntry, 0, len(ids))
	for id := range ids {
		out = append(out, IncompleteEntry{AniListID: id, SeaDexURL: a.seadexURL(id)})
	}
	slices.SortFunc(out, func(x, y IncompleteEntry) int { return cmp.Compare(x.AniListID, y.AniListID) })
	return out
}

// uncoveredRows lists library items that are recognized anime (present in the
// Fribb map) but were not covered by any SeaDex match. The Fribb catalogue
// filter is what keeps this to genuine anime gaps rather than every non-anime
// item in the arrs.
func uncoveredRows(snap *library.Snapshot, idx *mapping.Index, covered map[string]struct{}, excludeSpecials bool) []Row {
	if snap == nil {
		return nil
	}
	// The reverse item->record catalogue lives in match beside the forward ID
	// bridge (one home for the arr-consistent pairing rule); audit contributes
	// only its specials policy, as a record predicate mirroring the
	// matched-rows arm's filter: with the filter on, a special record
	// catalogues nothing, so a specials-only item is not catalogued and cannot
	// surface as not_on_seadex, while a mixed series stays catalogued through
	// its non-special records sharing the same TVDB id.
	cat := match.NewCatalogue(idx, func(r mapping.Record) bool {
		return !filter.ExcludeSpecial(r.IsSpecial(), excludeSpecials)
	})
	var rows []Row
	for i := range snap.Items {
		it := &snap.Items[i]
		if _, ok := covered[it.Key()]; ok {
			continue
		}
		if !cat.Has(it) {
			continue
		}
		// An uncovered item has no SeaDex-associated Fribb record to supply a
		// specific scope, so resolve its label through align.Scope with a zero
		// record (Radarr -> movie; Sonarr -> whole-series).
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

// assess builds one row: classify the entry's releases, resolve the shared
// comparison decision (align.Decide - the same decision the daemon's compare
// pass projects, fed here with the raw SeaDex best and alt group sets), and
// render it as the row's verdict and qualifier.
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
		Special:     m.Record.IsSpecial(),
		Incomplete:  m.Entry.Incomplete,
	}
	d := align.Decide(m.Item, &m.Record, best, alt)
	row.scope = d.Kind
	row.Season = d.Season
	row.CurrentGroups, row.Approx = d.Groups, d.Approx
	row.Verdict = verdictFor(d.Standing)
	row.Qualifier = rowQualifier(&m.Entry, &d)
	return row
}

// verdictFor renders the shared decision core's group-ladder standing in the
// report's verdict vocabulary, 1:1.
func verdictFor(s align.Standing) Verdict {
	switch s {
	case align.StandingNoFile:
		return VerdictNoFile
	case align.StandingUnverified:
		return VerdictUnverified
	case align.StandingBest:
		return VerdictBest
	case align.StandingAlt:
		return VerdictAlt
	default:
		return VerdictUnlisted
	}
}

// rowQualifier derives the daemon-vocabulary qualifier for a row from the
// shared decision, so the report distinguishes the states the daemon's
// compare pass distinguishes. With no best release listed at all (d.NoBest,
// read independently of the outcome because the report annotates the entry
// state even on a no-file row the daemon silences), the classify.Fallback
// precedence shared with the daemon's emptyResult picks "theoretical" or
// "incomplete" - the row's verdict would otherwise imply an unlisted-better
// state that does not exist. With best releases listed, a mixed outcome is
// "mixed" (where the daemon emits mixed_group_manual), and a diverged
// alt/unlisted row of an incomplete entry is "incomplete", mirroring the
// daemon's betterResult downgrade. An aligned row is never qualified -
// alignment wins - and an unverifiable row of an entry that still lists a
// best is never qualified either: its verdict (unverified) already carries
// the daemon's story (the info-level unverifiable finding). When the entry
// lists NO best, the NoBest annotation above applies even on an unverified
// row, matching the daemon (emptyResult nudges the same entry regardless
// of the group ladder).
func rowQualifier(entry *seadex.Entry, d *align.Decision) Qualifier {
	if d.NoBest {
		switch classify.Fallback(entry) {
		case classify.FallbackTheoretical:
			return QualifierTheoretical
		case classify.FallbackIncomplete:
			return QualifierIncomplete
		}
		return ""
	}
	switch {
	case d.Outcome == align.OutcomeMixed:
		return QualifierMixed
	case d.Outcome == align.OutcomeDiverged && entry.Incomplete &&
		(d.Standing == align.StandingAlt || d.Standing == align.StandingUnlisted):
		return QualifierIncomplete
	default:
		return ""
	}
}

// classifyReleases turns every SeaDex torrent into a report Release (group
// normalized via the shared classifier, tracker, usable URL, best flag,
// curation warnings). AnimeBytes torrents are dropped when the operator has
// AnimeBytes off — whether identified by the tracker label OR by the URL
// host, since the label is untrusted upstream data — so the report never
// surfaces AB releases or links they cannot use (and cannot leak them),
// mirroring the daemon's obtainability rule. A curation-warned release
// (SeaDex tags it Broken/Incomplete) stays listed but annotated: the report
// enumerates raw SeaDex data by design, so hiding it would misrepresent the
// entry, while groupSets and the render layer keep it out of the verdict and
// the grab links. A release the daemon's filter.Obtainable rule rejects (no
// usable link, or a tracker the operator cannot use) gets the same treatment,
// carried on Release.Unobtainable: listed and annotated, never verdict
// evidence - so a visible best the verdict ignored is always explained.
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
			Tracker:      t.Tracker,
			Group:        rel.Group,
			URL:          t.UsableURL(),
			Best:         t.IsBest,
			Warnings:     release.CurationWarnings(t.Tags),
			Unobtainable: !classify.Obtainable(&rel, t, a.includeAnimeBytes),
		})
	}
	return out
}

// seadexURL builds the releases.moe entry link for an AniList ID. The URL
// rule is the shared releases.moe contract in internal/seadex; this is a
// thin delegate over the injected base.
func (a *Auditor) seadexURL(aniListID int) string {
	return seadex.EntryURL(a.seadexBaseURL, aniListID)
}

// groupSets returns the distinct normalized groups among the best and the alt
// releases. A curation-warned release contributes to neither set: counting it
// would let a release SeaDex tags Broken/Incomplete drive the verdict (read
// as a best to have or to want), where the daemon's compare pass excludes it
// - the two flows must tell one story. An Unobtainable release (one the
// daemon's filter.Obtainable rule rejects: no usable link, or a tracker the
// operator cannot use) contributes to neither set for the same reason - the
// eligibility here IS the daemon's filter.Obtainable, computed in
// classifyReleases, not a mirror of it, so the two flows cannot drift when
// the tracker table grows. Both stay visible in the row's release list,
// annotated (the warning tags / "(unobtainable)").
func groupSets(releases []Release) (best, alt []string) {
	bestSeen, altSeen := map[string]struct{}{}, map[string]struct{}{}
	for i := range releases {
		if releases[i].Unobtainable || len(releases[i].Warnings) > 0 {
			continue
		}
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
	slices.SortStableFunc(rows, func(a, b Row) int {
		if c := cmp.Compare(rank[a.Verdict], rank[b.Verdict]); c != 0 {
			return c
		}
		return cmp.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title))
	})
}
