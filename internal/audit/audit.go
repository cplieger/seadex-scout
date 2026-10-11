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
	// verdictAlt means the on-disk release matches a listed non-best (alt) release.
	verdictAlt Verdict = "have_alt"
	// VerdictOlderRevision means the on-disk release is a SeaDex best group, but
	// only at an older revision than SeaDex lists for it (v1 against v2, an
	// original against a REPACK): the same group's newer release is listed.
	VerdictOlderRevision Verdict = "have_older_revision"
	// VerdictUnlisted means the on-disk release matches nothing SeaDex lists.
	VerdictUnlisted Verdict = "have_unlisted"
	// VerdictNoFile means the item (or the mapped season) has no file on disk.
	VerdictNoFile Verdict = "no_file"
	// VerdictUnverified means the item has files on disk but the comparison is
	// unverifiable: the release-group evidence on exactly one side is unknown
	// (an untagged release, NOGRP), or the library walk could not read this
	// item's file data at all, so neither alignment nor a divergence can
	// honestly be claimed.
	VerdictUnverified Verdict = "unverified"
	// verdictUnattributed means the item has files on disk but the app does not
	// compare this entry: it is an OFFERED unit, a film or special filed in
	// Sonarr's season-0 bucket that the map does not place on episodes the item
	// lists. The bucket's groups are reported for what it holds; the feed still
	// serves the entry.
	verdictUnattributed Verdict = "unattributed"
	// VerdictNotOnSeaDex means the item is in the library and recognized as anime
	// (present in the mapping) but SeaDex lists no entry for it.
	VerdictNotOnSeaDex Verdict = "not_on_seadex"
)

// verdictOrder is the report's most-actionable-first ordering. not_on_seadex is
// last: it is informational (no SeaDex recommendation exists to act on).
var verdictOrder = []Verdict{VerdictUnlisted, verdictAlt, VerdictOlderRevision, VerdictUnverified, verdictUnattributed, VerdictNoFile, VerdictBest, VerdictNotOnSeaDex}

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

type rowRelease struct {
	Tracker string `json:"tracker"`
	Group   string `json:"group,omitempty"`
	// URL is empty when the upstream link fails usable-link validation.
	URL string `json:"url,omitempty"`
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

type reportRow struct {
	Title     string  `json:"title"`
	Arr       string  `json:"arr"`
	ArrURL    string  `json:"arr_url,omitempty"`
	SeaDexURL string  `json:"seadex_url"`
	Verdict   Verdict `json:"verdict"`
	// Qualifier is the daemon-vocabulary annotation for the row
	// (mixed/theoretical/incomplete), empty when none applies.
	Qualifier     Qualifier    `json:"qualifier,omitempty"`
	MatchSource   string       `json:"match_source"`
	CurrentGroups []string     `json:"current_groups,omitempty"`
	Releases      []rowRelease `json:"releases,omitempty"`
	// Episodes and MissingEpisodes are set only on an "episodes" row: the
	// season-0 episodes the entry is, and those of them with no file.
	Episodes        []int `json:"episodes,omitempty"`
	MissingEpisodes []int `json:"missing_episodes,omitempty"`
	// CurrentRevision and BestRevision are set only on a have_older_revision
	// row: the newest revision held of the superseded best groups and the
	// newest revision SeaDex lists for them.
	CurrentRevision release.Revision `json:"current_revision,omitzero"`
	BestRevision    release.Revision `json:"best_revision,omitzero"`
	AniListID       int              `json:"al_id"`
	Season          int              `json:"season,omitempty"`
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
	// Approx marks a coarse comparison: an offered bucket held any file, or the
	// whole-series fallback compared more than one real season, so the verdict is
	// not an exact per-season attribution.
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

// incompleteEntry is one SeaDex entry whose AniList lookup failed transiently
// this run, so its library mapping is unconfirmed: left unmapped, or resolved
// from an expired memo entry. It renders in the incomplete-mapping section.
type incompleteEntry struct {
	SeaDexURL string `json:"seadex_url"`
	AniListID int    `json:"al_id"`
}

// itemCounts counts library ITEMS (one series or one film), where Report.Totals
// counts rows: a series that three SeaDex entries match is one item and three rows.
type itemCounts struct {
	// Anime is every item the report has a row for: a SeaDex match or not_on_seadex.
	Anime int `json:"anime"`
	// WithEntry is the items at least one SeaDex entry matched.
	WithEntry int `json:"with_entry"`
	// AllBest is the items with an entry whose every COMPARED row is have_best. A
	// row nothing was compared on (no_file, unattributed) neither earns nor blocks
	// it, so an item made only of such rows is not counted.
	AllBest int `json:"all_best"`
	// AllBestOrAlt is the items with an entry whose every compared row is
	// have_best or have_alt, by the same rules as AllBest, so it includes AllBest.
	AllBestOrAlt int `json:"all_best_or_alt"`
}

// Report is the full audit result.
type Report struct {
	GeneratedAt time.Time      `json:"generated_at"`
	Totals      map[string]int `json:"totals"`
	Rows        []reportRow    `json:"rows"`
	// Incomplete lists the SeaDex entries whose library mapping could not be
	// resolved this run (a transient AniList failure), sorted by AniList id.
	// Empty on a fully resolved run, and omitted from the JSON.
	Incomplete []incompleteEntry `json:"incomplete_mappings,omitempty"`
	Items      itemCounts        `json:"items"`
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
	rows := make([]reportRow, 0, len(matches))
	covered := make(map[string]struct{})
	matched := make(map[string]itemStanding)
	for i := range matches {
		m := &matches[i]
		if !m.InLibrary() {
			continue
		}
		// Coverage is a property of the COMPARISON, not of the link. The mark stays
		// ABOVE the excludeSpecials continue, so an excluded special's coverage
		// moves only by the comparability rule.
		if align.ClaimsCoverage(m.Item, &m.Record) {
			covered[m.Item.Key()] = struct{}{}
		}
		if u := m.Uncompared; u != nil && align.ClaimsCoverage(u, &m.Record) {
			covered[u.Key()] = struct{}{}
		}
		if a.excludeSpecials && m.Record.IsSpecial() {
			continue
		}
		row := a.assess(m)
		matched[m.Item.Key()] = matched[m.Item.Key()].with(row.Verdict)
		rows = append(rows, row)
	}
	uncovered, uncoveredKeys := uncoveredRows(snap, idx, covered, a.excludeSpecials)
	rows = append(rows, uncovered...)

	totals := make(map[string]int, len(verdictOrder))
	for i := range rows {
		totals[string(rows[i].Verdict)]++
	}
	sortRows(rows)
	return Report{
		GeneratedAt: time.Now().UTC(),
		Totals:      totals,
		Items:       itemTotals(matched, uncoveredKeys),
		Rows:        rows,
		Incomplete:  incompleteEntries(incompleteIDs),
	}
}

type itemStanding struct {
	best, alt, belowAlt bool
}

func (s itemStanding) with(v Verdict) itemStanding {
	switch v {
	case VerdictBest:
		s.best = true
	case verdictAlt:
		s.alt = true
	case VerdictOlderRevision, VerdictUnlisted, VerdictUnverified:
		s.belowAlt = true
	case VerdictNoFile, verdictUnattributed, VerdictNotOnSeaDex:
	}
	return s
}

// An item can be both matched and uncovered (its only entries are offered
// ones), so Anime is the union.
func itemTotals(matched map[string]itemStanding, uncoveredKeys []string) itemCounts {
	t := itemCounts{Anime: len(matched), WithEntry: len(matched)}
	for _, key := range uncoveredKeys {
		if _, ok := matched[key]; !ok {
			t.Anime++
		}
	}
	for _, s := range matched {
		if s.belowAlt {
			continue
		}
		if s.best || s.alt {
			t.AllBestOrAlt++
		}
		if s.best && !s.alt {
			t.AllBest++
		}
	}
	return t
}

// incompleteEntries renders the transiently-unresolved AniList ids as the
// report's incomplete-mapping section, sorted by id. Nil on a resolved run.
func incompleteEntries(ids map[int]struct{}) []incompleteEntry {
	if len(ids) == 0 {
		return nil
	}
	out := make([]incompleteEntry, 0, len(ids))
	for id := range ids {
		out = append(out, incompleteEntry{AniListID: id, SeaDexURL: seadex.EntryURL(id)})
	}
	slices.SortFunc(out, func(x, y incompleteEntry) int { return cmp.Compare(x.AniListID, y.AniListID) })
	return out
}

// uncoveredRows lists library items that are recognized anime (present in the
// mapping) but were not covered by any SeaDex match, plus each row's item key.
func uncoveredRows(snap *library.Snapshot, idx *mapping.Index, covered map[string]struct{}, excludeSpecials bool) (rows []reportRow, keys []string) {
	if snap == nil {
		return nil, nil
	}
	// audit contributes only its specials policy: with the filter on, a special
	// record catalogues nothing, so a specials-only item cannot surface as
	// not_on_seadex, while a mixed series stays catalogued through its siblings.
	cat := match.NewCatalogue(idx, func(r mapping.Record) bool {
		return !excludeSpecials || !r.IsSpecial()
	})
	for i := range snap.Items {
		it := &snap.Items[i]
		if _, ok := covered[it.Key()]; ok {
			continue
		}
		if !cat.Has(it) {
			continue
		}
		// An uncovered item has no SeaDex-associated record to supply a scope.
		rows = append(rows, reportRow{
			Title:         it.Title,
			Arr:           it.Arr,
			ArrURL:        it.ArrURL,
			Verdict:       VerdictNotOnSeaDex,
			CurrentGroups: slices.Clone(it.Groups),
			GroupsUnknown: !it.Comparable(),
			Scope:         align.ItemKind(it),
		})
		keys = append(keys, it.Key())
	}
	return rows, keys
}

// assess builds one row: classify the entry's releases, resolve the shared
// comparison decision (align.Decide), and render it as verdict and qualifier.
func (a *Auditor) assess(m *match.Match) reportRow {
	releases := a.classifyReleases(&m.Entry)
	best, alt := groupSets(releases)

	row := reportRow{
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
	listing := align.Listing{Best: best, Alt: alt, BestRevisions: classify.BestRevisions(&m.Entry)}
	d := align.Decide(m.Item, m.AlignEntry(), &listing)
	row.Scope = d.Kind
	row.Season = d.Season
	row.Episodes, row.MissingEpisodes = d.Episodes, d.MissingEpisodes
	row.GroupsUnknown = !m.Item.Comparable()
	// align.Decision.Groups is caller-owned, so the row can take it without cloning.
	row.CurrentGroups, row.Approx = d.Groups, d.Approx
	row.Verdict = verdictFor(&d, row.GroupsUnknown)
	row.Qualifier = rowQualifier(&m.Entry, &d)
	if d.Standing == align.StandingBestSuperseded {
		row.CurrentRevision, row.BestRevision = d.HeldRevision, d.ListedRevision
	}
	return row
}

// verdictFor renders the shared decision core's group-ladder standing in the
// report's verdict vocabulary. Every standing maps 1:1 except unverified, which
// the report splits by origin: an OFFERED unit whose file data was read is
// unattributed (the app does not compare it), while a NOGRP side or a placeholder
// whose files could not be read (groupsUnknown, which is also what separates an
// unreadable film from an offered one, since the two decisions are identical)
// stays unverified.
func verdictFor(d *align.Decision, groupsUnknown bool) Verdict {
	switch d.Standing {
	case align.StandingNoFile:
		return VerdictNoFile
	case align.StandingUnverified:
		if d.Kind == align.ScopeOffered && !groupsUnknown {
			return verdictUnattributed
		}
		return VerdictUnverified
	case align.StandingBest:
		return VerdictBest
	case align.StandingBestSuperseded:
		return VerdictOlderRevision
	case align.StandingAlt:
		return verdictAlt
	case align.StandingUnlisted:
		return VerdictUnlisted
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
	case (d.Outcome == align.OutcomeDiverged || d.Outcome == align.OutcomeSuperseded) && entry.Incomplete:
		return QualifierIncomplete
	default:
		return ""
	}
}

// classifyReleases turns every SeaDex torrent into a rowRelease (group,
// tracker, usable URL, best flag, curation warnings). DEFINITIVELY AnimeBytes
// torrents are dropped when the operator has AnimeBytes off. A public-labeled
// release whose URL evidence is malformed or ambiguous is NOT dropped: it stays
// listed with Unobtainable set, so a release that drove no verdict is explained.
func (a *Auditor) classifyReleases(entry *seadex.Entry) []rowRelease {
	out := make([]rowRelease, 0, len(entry.Torrents))
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
		out = append(out, rowRelease{
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
func groupSets(releases []rowRelease) (best, alt []string) {
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
func forfeitsBest(rel *rowRelease) bool {
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
func sortRows(rows []reportRow) {
	rank := make(map[Verdict]int, len(verdictOrder))
	for i, v := range verdictOrder {
		rank[v] = i
	}
	slices.SortStableFunc(rows, func(a, b reportRow) int {
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
