// Package audit produces a full SeaDex-alignment report over the library: for
// every anime that has a matching SeaDex entry, what release you have and
// whether it is SeaDex's best, an alt, or unlisted. Unlike the daemon's
// report-by-exception findings, this enumerates everything.
package audit

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/classify"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/tagfilter"
	"github.com/cplieger/seadex-scout/internal/tracker"
	"github.com/cplieger/seadex-scout/internal/trackerlink"
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
	// VerdictUnverified means the item has files on disk but the comparison is
	// unverifiable: the release-group evidence on at least one side is unknown,
	// so neither alignment nor a divergence can honestly be claimed.
	VerdictUnverified Verdict = "unverified"
	// VerdictNotOnSeaDex means the item is in the library and recognized as anime
	// (present in the Fribb map) but SeaDex lists no entry for it.
	VerdictNotOnSeaDex Verdict = "not_on_seadex"
)

// verdictOrder is the report's most-actionable-first ordering. not_on_seadex is
// last: it is informational (no SeaDex recommendation exists to act on).
var verdictOrder = []Verdict{VerdictUnlisted, VerdictAlt, VerdictUnverified, VerdictNoFile, VerdictBest, VerdictNotOnSeaDex}

// Qualifier annotates a row's verdict with the daemon's finding vocabulary for
// the same (item, entry). It annotates; it never forks the verdict enum.
type Qualifier string

const (
	// QualifierMixed marks a row where the daemon would emit mixed_group_manual:
	// the scoped on-disk groups span more than one group and none is a SeaDex best.
	QualifierMixed Qualifier = "mixed"
	// QualifierTheoretical marks a row whose SeaDex entry names only a theoretical
	// best (no isBest torrents), so nothing concrete is listed to compare against.
	QualifierTheoretical Qualifier = "theoretical"
	// QualifierIncomplete marks a row whose SeaDex entry is incomplete: it lists no
	// isBest torrents, or a listed best is not aligned with the on-disk group.
	QualifierIncomplete Qualifier = "incomplete"
)

// Release is one SeaDex torrent in a report row (best or alt). URL is
// empty when the upstream link fails usable-link validation.
type Release struct {
	Tracker string `json:"tracker"`
	Group   string `json:"group,omitempty"`
	URL     string `json:"url,omitempty"`
	// Warnings carries the canonical curation-warning tags (broken, incomplete)
	// SeaDex curators put on the release. Display vocabulary only: a warned
	// release is always listed and always annotated.
	Warnings []string `json:"warnings,omitempty"`
	Best     bool     `json:"best"`
	// Filtered marks a release the operator's filters.exclude_tags policy excludes
	// from the REPORT surface. Such a release stays listed and annotated but
	// forfeits the verdict's BEST group set (see groupSets).
	Filtered bool `json:"filtered,omitempty"`
	// Unobtainable marks a release the obtainability rule rejects as verdict
	// evidence: no usable link, or a tracker the operator cannot use. It stays
	// listed, drives neither the BEST group set nor the grab links, and does still
	// count on the descriptive ALT rung.
	Unobtainable bool `json:"unobtainable,omitempty"`
	// URLError marks a release whose SeaDex record carries a NON-EMPTY url that the
	// publisher refused. Reported separately from Unobtainable because this is an
	// upstream DATA defect to fix at the source, not the operator's configuration.
	URLError bool `json:"url_error,omitempty"`
	// UnknownTracker marks a release whose record names a tracker this app's
	// canonical table does not carry, so no link could be built. The remedy is the
	// opposite direction from URLError's: a seadex-scout change, not a SeaDex one.
	UnknownTracker bool `json:"unknown_tracker,omitempty"`
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
	// Scope is the comparison scope resolved for the row: the shared decision's
	// kind on a matched row (align.Decide), align.ItemKind on an uncovered one.
	// align.ScopeWholeSeries, the zero value, encodes and renders as "series".
	Scope      align.ScopeKind `json:"scope"`
	Special    bool            `json:"special,omitempty"`
	Incomplete bool            `json:"incomplete,omitempty"`
	// GroupsUnknown marks CurrentGroups as MISSING rather than empty: the library
	// walk could not establish this item's file data, so no group was ever read.
	// A not_on_seadex row never reaches align.Decide, so this is where it says so.
	GroupsUnknown bool `json:"groups_unknown,omitempty"`
	// Approx marks a coarse comparison: the season-0 specials bucket held more
	// than one group, or the whole-series fallback compared more than one real
	// season, so the verdict is not an exact per-season attribution.
	Approx bool `json:"approx,omitempty"`
	// HiddenAnimeBytes counts the entry's releases withheld by the operator's
	// AnimeBytes toggle. Without it a row whose only bests are AnimeBytes releases
	// is indistinguishable from an entry SeaDex lists no best for.
	HiddenAnimeBytes int `json:"hidden_animebytes,omitempty"`
	// HiddenAnimeBytesBest counts only the withheld releases SeaDex marks BEST. It
	// is a separate key rather than a re-reading of HiddenAnimeBytes, because a
	// hidden ALT says nothing about whether a best exists.
	HiddenAnimeBytesBest int `json:"hidden_animebytes_best,omitempty"`
}

// IncompleteEntry is one SeaDex entry whose AniList lookup failed transiently
// this run, so its library mapping is unconfirmed: left unmapped, or resolved
// from an expired memo entry. It renders in the incomplete-mapping section.
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
	// Empty on a fully resolved run, and omitted from the JSON.
	Incomplete []IncompleteEntry `json:"incomplete_mappings,omitempty"`
}

// Config configures an Auditor.
type Config struct {
	// TagFilter is the operator's filters.exclude_tags policy, asked about the
	// report surface. Its zero value excludes nothing.
	TagFilter       tagfilter.Filter
	ExcludeSpecials bool
	AnimeBytes      bool
}

// Auditor builds alignment reports from matches.
type Auditor struct {
	tags              tagfilter.Filter
	excludeSpecials   bool
	includeAnimeBytes bool
}

// New builds an Auditor from cfg.
func New(cfg Config) *Auditor {
	return &Auditor{
		tags:              cfg.TagFilter,
		excludeSpecials:   cfg.ExcludeSpecials,
		includeAnimeBytes: cfg.AnimeBytes,
	}
}

// Audit produces the report: one row per in-library SeaDex match (specials
// skipped when disabled), plus one not_on_seadex row per library item that is
// recognized anime but has no SeaDex entry. snap and idx may be nil, in which
// case the not_on_seadex section is empty. incompleteIDs carries the AniList ids
// whose needed lookup failed transiently this run.
func (a *Auditor) Audit(matches []match.Match, snap *library.Snapshot, idx *mapping.Index, incompleteIDs map[int]struct{}) Report {
	rows := make([]Row, 0, len(matches))
	covered := make(map[string]struct{})
	for i := range matches {
		m := &matches[i]
		if !m.InLibrary() {
			continue
		}
		covered[m.Item.Key()] = struct{}{}
		if a.excludeSpecials && m.Record.IsSpecial() {
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
// report's incomplete-mapping section, sorted by id. Nil on a resolved run.
func (a *Auditor) incompleteEntries(ids map[int]struct{}) []IncompleteEntry {
	if len(ids) == 0 {
		return nil
	}
	out := make([]IncompleteEntry, 0, len(ids))
	for id := range ids {
		out = append(out, IncompleteEntry{AniListID: id, SeaDexURL: seadex.EntryURL(id)})
	}
	slices.SortFunc(out, func(x, y IncompleteEntry) int { return cmp.Compare(x.AniListID, y.AniListID) })
	return out
}

// uncoveredRows lists library items that are recognized anime (present in the
// Fribb map) but were not covered by any SeaDex match.
func uncoveredRows(snap *library.Snapshot, idx *mapping.Index, covered map[string]struct{}, excludeSpecials bool) []Row {
	if snap == nil {
		return nil
	}
	// audit contributes only its specials policy: with the filter on, a special
	// record catalogues nothing, so a specials-only item cannot surface as
	// not_on_seadex, while a mixed series stays catalogued through its siblings.
	cat := match.NewCatalogue(idx, func(r mapping.Record) bool {
		return !excludeSpecials || !r.IsSpecial()
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
		// An uncovered item has no SeaDex-associated record to supply a scope.
		rows = append(rows, Row{
			Title:         it.Title,
			Arr:           it.Arr,
			ArrURL:        it.ArrURL,
			Verdict:       VerdictNotOnSeaDex,
			CurrentGroups: slices.Clone(it.Groups),
			GroupsUnknown: !it.Comparable(),
			Scope:         align.ItemKind(it),
		})
	}
	return rows
}

// assess builds one row: classify the entry's releases, resolve the shared
// comparison decision (align.Decide), and render it as verdict and qualifier.
func (a *Auditor) assess(m *match.Match) Row {
	releases := a.classifyReleases(&m.Entry)
	best, alt := groupSets(releases)

	row := Row{
		Releases:    releases,
		Title:       m.Item.Title,
		Arr:         m.Arr,
		ArrURL:      m.Item.ArrURL,
		SeaDexURL:   seadex.EntryURL(m.Entry.AniListID),
		MatchSource: string(m.Source),
		AniListID:   m.Entry.AniListID,
		Special:     m.Record.IsSpecial(),
		Incomplete:  m.Entry.Incomplete,
	}
	for i := range m.Entry.Torrents {
		if a.hiddenByABToggle(&m.Entry.Torrents[i]) {
			row.HiddenAnimeBytes++
			if m.Entry.Torrents[i].IsBest {
				row.HiddenAnimeBytesBest++
			}
		}
	}
	d := align.Decide(m.Item, &m.Record, best, alt)
	row.Scope = d.Kind
	row.Season = d.Season
	row.GroupsUnknown = !m.Item.Comparable()
	// align.Decision.Groups is caller-owned, so the row can take it without cloning.
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

// rowQualifier derives the daemon-vocabulary qualifier for a row from the shared
// decision. With no best release listed at all (d.NoBest, read independently of
// the outcome), the classify.Fallback precedence picks theoretical or incomplete;
// an aligned row is never qualified, and neither is an unverifiable row of an
// entry that still lists a best.
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
	case d.Outcome == align.OutcomeDiverged && entry.Incomplete:
		return QualifierIncomplete
	default:
		return ""
	}
}

// classifyReleases turns every SeaDex torrent into a report Release (group,
// tracker, usable URL, best flag, curation warnings). DEFINITIVELY AnimeBytes
// torrents are dropped when the operator has AnimeBytes off. A public-labeled
// release whose URL evidence is malformed or ambiguous is NOT dropped: it stays
// listed with Unobtainable set, so a release that drove no verdict is explained.
func (a *Auditor) classifyReleases(entry *seadex.Entry) []Release {
	out := make([]Release, 0, len(entry.Torrents))
	for i := range entry.Torrents {
		t := &entry.Torrents[i]
		// Hide only a DEFINITIVELY AB torrent when the toggle is off; ambiguous
		// evidence stays listed and is annotated unobtainable instead.
		if a.hiddenByABToggle(t) {
			continue
		}
		rel := classify.Torrent(entry, t)
		// One evaluation of the publisher: URL, URLError and UnknownTracker are three
		// readings of the same decision, so "a refusal means no link" is structural.
		// The refusal REASON comes from the publisher rather than being re-derived.
		published, refusal := classify.PublishRefusal(t)
		out = append(out, Release{
			Tracker:        rel.Tracker,
			Group:          rel.Group,
			URL:            published,
			Best:           t.IsBest,
			URLError:       refusal == trackerlink.RefusalUnvouchableURL,
			UnknownTracker: refusal == trackerlink.RefusalUnknownTracker,
			Warnings:       curationWarnings(t.Tags),
			Filtered:       a.tags.Excludes(t.Tags, tagfilter.SurfaceReport),
			Unobtainable:   !classify.Obtainable(&rel, t, a.includeAnimeBytes),
		})
	}
	return out
}

// hiddenByABToggle reports whether the operator's AnimeBytes toggle withholds t
// from the report. It is the ONE expression of that gate, so the per-row hidden
// count cannot drift from the drop it accounts for.
func (a *Auditor) hiddenByABToggle(t *seadex.Torrent) bool {
	return !a.includeAnimeBytes && classify.ABEvidence(t) == tracker.ABDefinite
}

// groupSets returns the distinct normalized groups among the best and the alt
// releases. The two rungs answer DIFFERENT questions: BEST is prescriptive, so a
// release that forfeits best evidence (forfeitsBest) contributes nothing, while
// ALT is descriptive - "is what I already have something SeaDex lists?" - and
// gates nothing. Both classes stay visible in the row's release list, annotated.
func groupSets(releases []Release) (best, alt []string) {
	bestSeen, altSeen := map[string]struct{}{}, map[string]struct{}{}
	for i := range releases {
		rel := &releases[i]
		g := release.NormalizeGroup(rel.Group)
		if rel.Best {
			if forfeitsBest(rel) {
				continue
			}
			addUnique(bestSeen, &best, g)
		} else {
			addUnique(altSeen, &alt, g)
		}
	}
	return best, alt
}

// forfeitsBest reports whether a best release contributes no BEST evidence to
// the verdict: the operator's tag policy excludes it from the report surface, or
// it is unreachable. Deliberately NARROWER than the render layer's annotated():
// a curation warning is display, this is policy.
func forfeitsBest(rel *Release) bool {
	return rel.Filtered || rel.Unobtainable
}

// addUnique appends g to out if not already seen.
func addUnique(seen map[string]struct{}, out *[]string, g string) {
	if _, ok := seen[g]; ok {
		return
	}
	seen[g] = struct{}{}
	*out = append(*out, g)
}

// sortRows orders rows by verdict actionability, then title, then season and
// AniList id for same-title rows.
func sortRows(rows []Row) {
	rank := make(map[Verdict]int, len(verdictOrder))
	for i, v := range verdictOrder {
		rank[v] = i
	}
	slices.SortStableFunc(rows, func(a, b Row) int {
		if c := cmp.Compare(rank[a.Verdict], rank[b.Verdict]); c != 0 {
			return c
		}
		if c := cmp.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title)); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Season, b.Season); c != 0 {
			return c
		}
		return cmp.Compare(a.AniListID, b.AniListID)
	})
}
