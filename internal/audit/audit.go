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
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/tagfilter"
	"github.com/cplieger/seadex-scout/internal/tracker"
	"github.com/cplieger/seadex-scout/internal/trackerlink"
)

// --- Verdict + qualifier vocabulary ---

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
	//
	// It also covers an item whose file state the library walk could not
	// establish (a library placeholder), where alignment is undetermined
	// because the file data is missing rather than absent.
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
	// incomplete) SeaDex curators put on the release, when any. It is the
	// report's DISPLAY vocabulary and nothing else: a warned release is
	// always listed and always annotated with the warning marker, whether or
	// not the operator's filters.exclude_tags policy also excludes it (see
	// Filtered, curationWarnings, and the render layer). The
	// vocabulary is fixed here on purpose - it names what SeaDex's curators
	// said, not what the operator chose to filter.
	Warnings []string `json:"warnings,omitempty"`
	Best     bool     `json:"best"`
	// Filtered marks a release the operator's filters.exclude_tags policy
	// excludes from the REPORT surface (one of its SeaDex tags is listed for
	// `report`). Such a release stays listed and annotated - the report
	// enumerates raw SeaDex data - but forfeits the verdict's BEST group set
	// exactly like an unobtainable one (see groupSets). False by default,
	// because an absent or empty exclude_tags filters nothing anywhere;
	// serialized only when set, so an unfiltered row's JSON shape is
	// unchanged.
	Filtered bool `json:"filtered,omitempty"`
	// Unobtainable marks a release the daemon's obtainability rule
	// (filter.Obtainable) rejects as verdict evidence: no usable link, or a
	// tracker the operator cannot use. Like a curation-warned release it
	// stays listed - the report enumerates raw SeaDex data - but it drives
	// neither the verdict's BEST group set nor the grab links, rendering with
	// an "(unobtainable)" annotation instead (see groupSets and the render
	// layer); it does still count on the descriptive ALT rung, which asks
	// only whether SeaDex lists what is already on disk. Serialized so
	// machine consumers can see WHY a visible best did not drive the verdict;
	// omitted on the common obtainable release, so a fully obtainable row's
	// JSON shape is unchanged.
	Unobtainable bool `json:"unobtainable,omitempty"`
	// URLError marks a release whose SeaDex record carries a NON-EMPTY url that
	// the publisher refused (classify.PublishURL returned ""): a foreign
	// host under a trusted label, an unknown tracker, a smuggling form, or - the
	// one live case - a value with no torrent-page shape at all, such as a
	// release-group name typed into the url field. It is reported separately from
	// Unobtainable because the two point the operator at different places: an
	// unobtainable release is a consequence of THEIR configuration (a tracker
	// they cannot use), while this is an upstream DATA defect they can go fix at
	// the source. An omitted or empty url is not an error - SeaDex simply has no
	// link for that release - so only a present-but-unpublishable value sets it.
	// Omitted on the common healthy release, so an ordinary row's JSON shape is
	// unchanged.
	URLError bool `json:"url_error,omitempty"`
	// UnknownTracker marks a release whose record names a tracker this app's
	// canonical table does not carry (or carries without a base URL), so no
	// link could be built and the release drives no verdict. It is reported
	// separately from URLError because the remedy is the OPPOSITE direction:
	// nothing about the SeaDex record is wrong, and the fix is a seadex-scout
	// change - add the tracker to internal/tracker's table and ship a release.
	// Without it every release on a newly-adopted SeaDex tracker would silently
	// read as an upstream data defect (l-f127). Omitted on the common healthy
	// release, so an ordinary row's JSON shape is unchanged.
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
	//
	// Published, not in-process only. Every renderer projects this struct, so a
	// fact only two of the three can reach is a fact the JSON's consumer has to
	// re-derive - and the derivation IS align's dispatch (movie vs mapped season
	// vs special vs whole-series fallback), re-implemented outside the package
	// that owns it, which is how internal/indexer once drifted from the season
	// rule (l-f4). align.ScopeKind.MarshalJSON keeps the wire vocabulary
	// identical to the Markdown's and the log's; Season carries the number the
	// Markdown composes into its "S02" label.
	Scope      align.ScopeKind `json:"scope"`
	Special    bool            `json:"special,omitempty"`
	Incomplete bool            `json:"incomplete,omitempty"`
	// GroupsUnknown marks CurrentGroups as MISSING rather than empty: the library
	// walk could not establish this item's file data (library.Item.Failed), so no
	// group was ever read. Without it an empty groups cell is indistinguishable
	// from an item genuinely carrying no identifiable group - the
	// placeholder-read-as-fact that library.Item.Comparable exists to stop
	// (d-u2-2). A matched row also states it in its verdict column (align.Decide
	// answers a placeholder with StandingUnverified), but the not_on_seadex rows
	// uncoveredRows builds never reach Decide and their verdict stays true, so
	// this is the only place they can say it.
	GroupsUnknown bool `json:"groups_unknown,omitempty"`
	// Approx marks a coarse comparison: the season-0 specials bucket held more
	// than one group, or the whole-series fallback compared more than one real
	// season, so the verdict reflects "this group is present somewhere in the
	// series/specials" rather than an exact per-season/per-special attribution.
	Approx bool `json:"approx,omitempty"`
	// HiddenAnimeBytes counts the entry's releases withheld by the operator's
	// AnimeBytes toggle. Without it a row whose only bests are AnimeBytes
	// releases is indistinguishable from an entry SeaDex lists no best for:
	// both render an empty best column, no qualifier, and a have_unlisted
	// verdict.
	HiddenAnimeBytes int `json:"hidden_animebytes,omitempty"`
	// HiddenAnimeBytesBest counts only the withheld releases SeaDex marks BEST.
	// It is a SEPARATE key rather than a re-reading of HiddenAnimeBytes: a hidden
	// AnimeBytes ALT says nothing about whether a best exists, so annotating from
	// the total would tell a reader an empty best column means "hidden on a
	// tracker you do not use" for an entry SeaDex genuinely lists no best for.
	// HiddenAnimeBytes therefore keeps its established all-releases meaning and
	// this carries the best-only projection.
	//
	// Published on all three renderers - the Markdown best column, this JSON key,
	// and the slog attribute - because it is the ONLY fact that distinguishes
	// "SeaDex lists no best" from "every best is on a tracker you turned off", and
	// a machine consumer reading hidden_animebytes alone cannot make that
	// distinction at all.
	HiddenAnimeBytesBest int `json:"hidden_animebytes_best,omitempty"`
}

// IncompleteEntry is one SeaDex entry whose AniList lookup failed transiently
// this run, so its library mapping is unconfirmed: it was either left unmapped
// or resolved from an expired memo entry (match.Memo.staleMedia), which still
// counts as degraded. It renders in the report's incomplete-mapping section; a
// row for it may be missing from the verdict sections, misfiled in them, or
// present with a verdict that rests on stale titles.
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

// --- Report building ---

// Config configures an Auditor.
type Config struct {
	// TagFilter is the operator's filters.exclude_tags policy, asked about the
	// report surface. Its zero value - the default - excludes nothing, so a
	// release SeaDex tags Broken is listed, annotated, AND counted as best
	// evidence.
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

// NewAuditor builds an Auditor from cfg.
func NewAuditor(cfg Config) *Auditor {
	return &Auditor{
		tags:              cfg.TagFilter,
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
// report's incomplete-mapping section, sorted by id for a stable render, each
// carrying its releases.moe link. Nil on a fully resolved run so the section
// (and the JSON key) is omitted entirely.
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
		// An uncovered item has no SeaDex-associated Fribb record to supply a
		// specific scope, so take the record-less label align owns
		// (Radarr -> movie; Sonarr -> whole-series).
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
// comparison decision (align.Decide - the same decision the daemon's compare
// pass projects, fed here with the SeaDex best set minus curation-warned and
// unobtainable releases and the full alt set - see groupSets), and
// render it as the row's verdict and qualifier.
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
	// Both row producers ask library.Item.Comparable - the one shared predicate for
	// "was this item's file data ever established" - rather than each reading
	// Item.Failed and deciding what it means.
	row.GroupsUnknown = !m.Item.Comparable()
	// align.Decision.Groups is caller-owned on every branch (align.Decide clones
	// the single-unit set at the edge and builds the whole-series union fresh), so
	// the row can take it directly - the report still never holds a window into the
	// snapshot a concurrent daemon cycle owns.
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
	case d.Outcome == align.OutcomeDiverged && entry.Incomplete:
		return QualifierIncomplete
	default:
		return ""
	}
}

// classifyReleases turns every SeaDex torrent into a report Release (group
// normalized via the shared classifier, tracker, usable URL, best flag,
// curation warnings). DEFINITIVELY AnimeBytes torrents — identified by the
// tracker label OR by successfully extracted URL host evidence, since the
// label is untrusted upstream data — are dropped when the operator has
// AnimeBytes off, so the report never surfaces AB releases or links they
// cannot use (and cannot leak them). A public-labeled release whose URL
// evidence is malformed or ambiguous is NOT dropped: the fail-closed
// filter.ABVisible gate inside classify.Obtainable (kept for verdict
// eligibility) cannot prove it is AB, and the report's contract is that a
// release with no usable link stays listed with Unobtainable=true so the
// operator can see why it did not affect the verdict. A curation-warned
// release (SeaDex tags it Broken/Incomplete) stays listed AND annotated, and by
// default also counts: the report enumerates raw SeaDex data by design, and
// whether a warning removes the release from the verdict is now the operator's
// filters.exclude_tags call (carried on Release.Filtered, empty by default),
// not this app's. The grab-links cell stays annotation-driven either way - the
// report does not offer a one-click grab for a release SeaDex's own curators
// warn against. A release the daemon's filter.Obtainable rule rejects (no usable
// link, or a tracker the operator cannot use) gets the same treatment,
// carried on Release.Unobtainable: listed and annotated, never verdict
// evidence - so a visible best the verdict ignored is always explained.
func (a *Auditor) classifyReleases(entry *seadex.Entry) []Release {
	out := make([]Release, 0, len(entry.Torrents))
	for i := range entry.Torrents {
		t := &entry.Torrents[i]
		// Hide only a DEFINITIVELY AB torrent (label or extracted raw-URL
		// host evidence; the grading lives in classify.ABEvidence) when the
		// toggle is off. Malformed/ambiguous public-labeled evidence
		// stays listed: the publisher drops the link and the fail-closed
		// Obtainable below annotates the row unobtainable instead.
		if a.hiddenByABToggle(t) {
			continue
		}
		rel := classify.Torrent(entry, t)
		// One evaluation of the publisher: URL, URLError and UnknownTracker are
		// three readings of the same decision, so the "a refusal means no link"
		// invariant is structural rather than a coincidence of calls agreeing.
		// The refusal REASON comes from the publisher itself rather than being
		// re-derived here: an empty result alone cannot tell an upstream data
		// defect from a tracker this app's table does not carry, and the two
		// point the operator at different remedies (l-f127).
		published, refusal := classify.PublishRefusal(t)
		out = append(out, Release{
			Tracker: rel.Tracker,
			Group:   rel.Group,
			URL:     published,
			Best:    t.IsBest,
			// A record that carries a URL the publisher refused is an UPSTREAM
			// DATA fault, not an obtainability policy decision, and the report is
			// where an operator can see it and go fix the SeaDex record.
			// The live catalogue has one: tracker AB, url "Chihiro" - a
			// release-group name typed into the url field. Reported distinctly
			// because "(unobtainable)" would read as "a tracker you cannot use",
			// pointing the operator at their own config instead of at the record.
			URLError: refusal == trackerlink.RefusalUnvouchableURL,
			// An unknown tracker is the OTHER refusal, and its remedy is this
			// app's, not the record's: nothing about the SeaDex data is wrong.
			UnknownTracker: refusal == trackerlink.RefusalUnknownTracker,
			Warnings:       curationWarnings(t.Tags),
			// The FILTER question, distinct from the annotation above: the
			// operator's configured tag exclusions for this surface. Empty by
			// default, so a warned release is annotated AND counted.
			Filtered:     a.tags.Excludes(t.Tags, tagfilter.SurfaceReport),
			Unobtainable: !classify.Obtainable(&rel, t, a.includeAnimeBytes),
		})
	}
	return out
}

// hiddenByABToggle reports whether the operator's AnimeBytes toggle withholds
// t from the report. It is the ONE expression of that gate, so the per-row
// hidden count cannot drift from the drop it accounts for.
func (a *Auditor) hiddenByABToggle(t *seadex.Torrent) bool {
	return !a.includeAnimeBytes && classify.ABEvidence(t) == tracker.ABDefinite
}

// --- Group sets + row ordering ---

// groupSets returns the distinct normalized groups among the best and the alt
// releases. The two rungs answer DIFFERENT questions, so the exclusion
// classes gate only one of them.
//
// BEST is prescriptive - "is this the release to have?" - so a release that
// forfeits best evidence (see forfeitsBest: the operator's tag policy excludes
// it from the report, the publisher refused its url, or the daemon's
// filter.Obtainable rule rejects it as unreachable) contributes nothing:
// counting an ungettable release would let it read as a best to have or to
// want, where the daemon's compare pass excludes it - the two flows must tell
// one story. The eligibility here IS the daemon's filter.Obtainable, computed
// in classifyReleases, not a mirror of it, so the two flows cannot drift when
// the tracker table grows, and the tag half is the SAME tagfilter policy the
// daemon and the feed read, so all three surfaces answer one configured
// question.
//
// A curation warning alone no longer forfeits best evidence: with the default
// (empty) filters.exclude_tags, a release SeaDex tags Broken IS counted here,
// and the operator sees it in the SeaDex-best column with its "(broken)" note
// in the Notes column. That is the deliberate default - the annotation informs,
// the config decides - so this set and the render layer's annotated() predicate
// are no longer one list: annotated() still governs DISPLAY (the note, the
// clean-before-annotated ordering, and the grab-links affordance), while this
// rung governs the VERDICT.
//
// ALT is DESCRIPTIVE - "is what I already have something SeaDex lists?" - and
// a curation warning or a broken link does not change that answer. Gating it
// too made a row whose on-disk group SeaDex lists as a warned or ungettable
// alt render have_unlisted, whose rendered explanation ("You have a release
// SeaDex does not list as best or alt") was then false for that row, and the
// Markdown table has no alt column to correct it from. The daemon-parity
// argument does not reach this rung either: compare passes alt=nil, so it has
// no alt concept for the two flows to agree on (l-f144). An annotated alt
// still never becomes a grab link or a best-column recommendation - that
// gating lives on the best rung and in the links cell, where the operator
// acts. Both classes stay visible in the row's release list, annotated (the
// warning tags / "(url error)" / "(unobtainable)").
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
// the verdict: the operator's filters.exclude_tags policy excludes it from the
// report surface, or it is unreachable (no usable link). It is deliberately
// NARROWER than the render layer's annotated(): a curation warning is display,
// this is policy, and by default no tag is filtered at all.
//
// Release.URLError and Release.UnknownTracker are display diagnostics only, and
// are deliberately NOT operands here: both name a publisher refusal that returns
// an empty URL, which classify.Obtainable then rejects, so Unobtainable is
// already true whenever either is set.
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
