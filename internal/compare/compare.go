// Package compare turns matched SeaDex entries into findings. For each entry in
// the library it classifies the SeaDex "best" releases, keeps those that pass
// the content filters (remux/dual-audio) AND are obtainable (on a public
// tracker, or on AnimeBytes when the operator enables it), and compares the
// surviving recommended release groups against the groups present on the
// library item. The comparison is season-scoped and decided by the shared
// internal/align decision core (align.Decide) - the same decision rules the
// audit report renders, so the two flows cannot drift on shared inputs (they
// deliberately prepare different ones: the report judges the SeaDex best/alt
// sets minus only curation-warned and unobtainable releases, while this pass
// additionally applies the content filters and keeps best-only): a
// mapped TVDB season against that season's groups, a special against Sonarr's
// season-0 bucket, a movie against its groups, and an absolute-numbered or
// title-only run against every real season conservatively -
// so a later season that needs a better release is not masked by an earlier
// season that already has it. An item that provenly has a recommended group is
// aligned and produces no finding; an item whose group evidence is unknown on
// either side (the release.NoGroup sentinel) is unverifiable and produces an
// informational finding, never an aligned silence or a better-release warning;
// a recommended release the operator cannot obtain is simply absent, never a
// finding.
package compare

import (
	"slices"
	"strconv"
	"strings"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/classify"
	"github.com/cplieger/seadex-scout/internal/filter"
	"github.com/cplieger/seadex-scout/internal/keyenc"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/tracker"
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
	// StatusUnverifiable means the comparison is indeterminate: the release
	// group evidence on at least one side is unknown (a group-less on-disk
	// file or a group-less SeaDex release, both carried as the release.NoGroup
	// sentinel) and could hide an alignment - so neither a confident aligned
	// silence nor a better_release warning is honest. An informational
	// manual-review nudge.
	StatusUnverifiable Status = "unverifiable"
)

// ReleaseLink is one obtainable source for a recommended release: the tracker,
// a human-followable URL, and the AnimeBytes evidence the RAW upstream record
// carried. A recommended group present on both a public tracker and AnimeBytes
// yields two links, so a finding can surface both.
type ReleaseLink struct {
	Tracker string
	URL     string
	// AB is the AnimeBytes grade classify.ABEvidence read from the RAW
	// upstream (tracker, URL) pair this link was published from. It travels
	// with the link because the URL here is the PUBLISHED one, and grading
	// that instead is the mistake internal/classify's raw-URL invariant
	// exists to prevent: publishing trusts the tracker label and rewrites or
	// erases the very host evidence the grade reads. Carrying it keeps ONE
	// grading site for the whole app (compare and audit already read
	// classify.ABEvidence) and leaves notify owning only slot PRECEDENCE,
	// which is genuinely its policy (h-f43).
	//
	// The zero value is filter.ABNone, so a link assembled without a producer
	// (a test literal, a future caller) carries no AnimeBytes evidence rather
	// than a silently re-derived one.
	AB filter.ABEvidence
	// Headline reports whether this link belongs to the HEADLINE candidate's
	// group - the group Finding.RecommendedGroup names. It carries
	// obtainableLinks' already-computed affinity to the consumer as data,
	// because slice ORDER alone is not enough: notify.trackerURLs picks per
	// tracker CLASS, so a Nyaa link from another recommended group would
	// otherwise outrank a headline-group link on a different public tracker
	// and the alert's clickable URL would not belong to the group the same
	// line names. The zero value (false) is safe: with no producer affinity
	// supplied, every link is a non-headline source and the tracker-class
	// preference decides alone.
	Headline bool
}

// Finding is one comparison result for a library item. It carries the
// semantic fields the notification layer emits; alert dedupe-key construction
// (the persisted suppression identity) is the notify package's own policy,
// derived from these fields at the notification boundary. It is a plain
// domain value: nothing serializes a Finding, and the persisted attribute
// schema is declared solely by notify.StoredFinding.
type Finding struct {
	Kind             string
	Reason           string
	Arr              string
	CurrentGroup     string
	RecommendedGroup string
	Tracker          string
	Title            string
	Resolution       string
	Codec            string
	ReleaseURL       string
	ArrURL           string
	InfoHash         string
	Status           Status
	// Scope is the comparison scope the shared decision resolved
	// (align.Decision.Kind, rendered via its String): "season", "movie",
	// "special" or "series". Season alone cannot carry it - a movie, a
	// season-0 special and a whole-series aggregate all report season 0 - so
	// without this the alert cannot say what unit was compared, where the
	// audit report's scope cell does.
	Scope             string
	RecommendedGroups []string
	Links             []ReleaseLink
	// CurrentGroups preserves the scoped on-disk group set with its element
	// boundaries as semantic structured data: CurrentGroup is the flattened
	// display join, where ["a,b","c"] and ["a","b,c"] are indistinguishable.
	// Nil on manually constructed findings, which the notify key builder
	// falls back to the flattened CurrentGroup for.
	CurrentGroups []string
	AniListID     int
	Season        int
	DualAudio     bool
	// Approx marks a coarse comparison (align.Decision.Approx): the season-0
	// specials bucket held more than one group, or the whole-series fallback
	// spanned more than one real season or group, so CurrentGroup is an
	// aggregate rather than an exact per-unit attribution. The audit report
	// renders the same fact as "(approx)".
	Approx bool
}

// --- Comparison flow ---

// Comparer produces findings from matches under a fixed filter policy.
type Comparer struct {
	opts            filter.Options
	excludeSpecials bool
	animeBytes      bool
}

// Config configures a Comparer.
type Config struct {
	Filter          filter.Options
	ExcludeSpecials bool
	// AnimeBytes includes AnimeBytes (private tracker) releases in the
	// obtainability check; public trackers are always considered. Off means
	// AnimeBytes releases are invisible. It is the comparer's own carrier for
	// the tracker toggle (mirroring audit.Config.AnimeBytes) because
	// filter.Options holds only the content filters.
	AnimeBytes bool
}

// NewComparer builds a Comparer from cfg.
func NewComparer(cfg Config) *Comparer {
	return &Comparer{
		opts:            cfg.Filter,
		excludeSpecials: cfg.ExcludeSpecials,
		animeBytes:      cfg.AnimeBytes,
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
		if filter.ExcludeSpecial(m.Record.IsSpecial(), c.excludeSpecials) {
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
// nil when there is nothing to report. The branch order and decision rules
// live in the shared align.Decide (the same decision the audit report
// renders); this function only projects the outcome into finding vocabulary:
// silence for a unit with no file on disk (the audit's no_file - compare has
// no no-file status, so report-by-exception means the daemon stays quiet) and
// for a provenly aligned unit, the classify.Fallback nudge when no
// recommended release survives the filters, an unverifiable info nudge when
// unknown group evidence makes the comparison indeterminate (never a
// confident aligned silence, never a better_release warning), a
// mixed_group_manual nudge for a not-aligned multi-group unit, and a better
// release otherwise.
func (c *Comparer) compareOne(m *match.Match) *Finding {
	entry := &m.Entry
	recommended := c.recommended(entry)
	recGroups := groupSet(recommended)
	// The daemon only distinguishes best-vs-not, so alt is nil: an on-disk
	// unit lacking a recommended group reads as unlisted (not aligned).
	d := align.Decide(m.Item, &m.Record, recGroups, nil)
	if d.Outcome == align.OutcomeNoFile {
		return nil
	}
	base := baseFinding(m, &d)
	switch d.Outcome {
	case align.OutcomeNoBest:
		return emptyResult(entry, &base)
	case align.OutcomeAligned:
		return nil
	case align.OutcomeUnverifiable:
		fillBest(&base, recommended, recGroups)
		return finalize(&base, StatusUnverifiable)
	case align.OutcomeMixed:
		fillBest(&base, recommended, recGroups)
		return finalize(&base, StatusMixedGroup)
	case align.OutcomeDiverged:
		return betterResult(entry, &base, recommended, recGroups)
	default:
		// Every Outcome the shared linearization produces is handled above. An
		// Outcome added later must not fall into the STRONGEST claim in this
		// projection (the warn-level better_release): mirror
		// align.outcomeOf's own conservative default and project an
		// unrecognized outcome as the informational unverifiable nudge.
		fillBest(&base, recommended, recGroups)
		return finalize(&base, StatusUnverifiable)
	}
}

// recommended classifies the entry's SeaDex "best" torrents and returns those
// the operator could act on: not curation-warned (a torrent SeaDex tags
// Broken/Incomplete is warned against, never recommended), passing the
// content filters (remux policy, dual-audio) AND obtainable (a public
// tracker, or AnimeBytes when enabled).
func (c *Comparer) recommended(entry *seadex.Entry) []candidate {
	var out []candidate
	for i := range entry.Torrents {
		t := &entry.Torrents[i]
		if !t.IsBest {
			continue
		}
		// A curation-warned release (SeaDex tags it Broken/Incomplete) is
		// never recommended: the curators themselves warn against grabbing
		// it, so like an unobtainable release it is absent, never a finding.
		// An entry whose every best is warned flows through emptyResult (the
		// theoretical/incomplete nudge or silence) unchanged.
		if release.CurationWarned(t.Tags) {
			continue
		}
		// AB guard before classification; the raw-URL invariant lives in
		// classify.ABVisible. Obtainable below re-checks the label as defense
		// in depth.
		if !classify.ABVisible(t, c.animeBytes) {
			continue
		}
		rel := classify.Torrent(entry, t)
		if ok, _ := filter.KeepNonTracker(&rel, c.opts); !ok {
			continue
		}
		if !classify.Obtainable(&rel, t, c.animeBytes) {
			continue
		}
		out = append(out, candidate{rel: rel, torrent: *t})
	}
	return out
}

// betterResult finalizes a diverged finding: a better release the operator
// can obtain and lacks, downgraded to an incomplete info nudge when the entry
// is incomplete (nothing complete to grab).
func betterResult(entry *seadex.Entry, base *Finding, recommended []candidate, recGroups []string) *Finding {
	status := StatusBetter
	if classify.DivergedIncomplete(entry) {
		status = StatusIncomplete
	}
	fillBest(base, recommended, recGroups)
	return finalize(base, status)
}

// emptyResult decides the finding when no recommended release survives the
// content and obtainability filters: a theoretical-best-only or incomplete
// entry is an info nudge, everything else (nothing the operator can get) is
// silent. The precedence lives in classify.Fallback, shared with the audit
// report's rowQualifier so the two flows cannot drift.
func emptyResult(entry *seadex.Entry, base *Finding) *Finding {
	switch classify.Fallback(entry) {
	case classify.FallbackTheoretical:
		return finalize(base, StatusTheoretical)
	case classify.FallbackIncomplete:
		return finalize(base, StatusIncomplete)
	default:
		return nil
	}
}

// baseFinding seeds a finding with the item identity fields, using the groups
// and season the shared decision judged/attributed the unit against
// (align.Decision.Groups: the mapped season's groups, or the whole-series
// union; align.Decision.Season: the shared season label) - so a season-scoped
// finding's CurrentGroup (and the dedupe key notify derives from it) never
// leaks whole-series groups, and the season attribution cannot drift from
// the audit report's.
func baseFinding(m *match.Match, d *align.Decision) Finding {
	return Finding{
		Title:         m.Item.Title,
		Arr:           m.Arr,
		ArrURL:        m.Item.ArrURL,
		CurrentGroup:  strings.Join(d.Groups, ","),
		CurrentGroups: slices.Clone(d.Groups),
		AniListID:     m.Entry.AniListID,
		Season:        d.Season,
		Scope:         d.Kind.String(),
		Approx:        d.Approx,
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
	f.Links = obtainableLinks(pool, release.NormalizeGroup(rep.rel.Group))
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
	f.ReleaseURL = classify.PublishURL(&cand.torrent)
	f.DualAudio = cand.rel.DualAudio
}

// obtainableLinks returns the distinct (tracker, URL) links across the pool,
// deduped, ordered headlineGroup-first and then by (URL, tracker). This is
// what lets a finding surface both a Nyaa and an AnimeBytes link for the same
// recommended release. The dedupe keys on the (tracker, URL) identity as a
// comparable struct, so a crafted tracker or URL containing a would-be
// delimiter cannot collide two distinct pairs.
//
// The ORDER is part of the contract, not incidental: notify.trackerURLs fills
// the alert's nyaa_url / public_url / ab_url slots first-link-wins, so leaving
// the slice in upstream (PocketBase relation) order would let the operator's
// clickable link be chosen by upstream ordering and belong to a group the same
// line's recommended_group does not name. Sorting the headline candidate's own
// sources first makes the rendered link agree with the headline fields, and the
// total (URL, tracker) order makes the rest reproducible across cycles - the
// same order-independence representative/candidateStableKey already guarantee
// for the headline pick.
func obtainableLinks(pool []candidate, headlineGroup string) []ReleaseLink {
	sources := sourcedLinks(pool, headlineGroup)
	slices.SortFunc(sources, compareSourcedLinks)
	links := make([]ReleaseLink, 0, len(sources))
	for i := range sources {
		link := sources[i].link
		// Carry the rank to the consumer as data, not just as slice order:
		// notify.trackerURLs selects per tracker class, so order alone loses
		// the affinity (see ReleaseLink.Headline).
		link.Headline = sources[i].rank == 0
		links = append(links, link)
	}
	return links
}

// sourcedLink is one obtainable link plus its headline rank (0 = a source of
// the headline candidate's group, 1 = any other source), the key
// obtainableLinks' total order sorts on.
type sourcedLink struct {
	link ReleaseLink
	rank int
}

// sourcedLinks collects the pool's distinct URL-carrying links in first-seen
// order, each ranked headline-first. The rank is computed BEFORE the dedupe so
// a duplicate link keeps the best rank any of its occurrences earns: the same
// (tracker, URL) pair can arrive on several candidates whose upstream group
// metadata differs, and first-occurrence-wins would leave a shared link ranked
// as a non-headline source purely because the non-headline candidate came
// first in relation order.
func sourcedLinks(pool []candidate, headlineGroup string) []sourcedLink {
	// Keyed on the link IDENTITY (tracker + URL) only: ReleaseLink.Headline
	// is producer affinity, not identity, so it must never take part in the
	// dedupe - obtainableLinks assigns it after this collection runs. The AB
	// grade is evidence about the record, not identity either, and is merged
	// (strongest wins) rather than keyed on.
	type linkKey struct{ tracker, url string }
	seen := make(map[linkKey]int, len(pool))
	sources := make([]sourcedLink, 0, len(pool))
	for i := range pool {
		u := classify.PublishURL(&pool[i].torrent)
		if u == "" {
			continue
		}
		link := ReleaseLink{
			Tracker: pool[i].rel.Tracker,
			URL:     u,
			// Graded from the RAW record, never from u: see ReleaseLink.AB.
			AB: classify.ABEvidence(&pool[i].torrent),
		}
		rank := 1
		if release.NormalizeGroup(pool[i].rel.Group) == headlineGroup {
			rank = 0
		}
		key := linkKey{tracker: link.Tracker, url: link.URL}
		if idx, dup := seen[key]; dup {
			sources[idx].rank = min(sources[idx].rank, rank)
			// Two records can publish the same (tracker, URL) from different
			// raw values, so the surviving link keeps the STRONGEST AnimeBytes
			// evidence any of them carried - the same fail-closed direction
			// the AB gates take, rather than letting record order decide
			// whether the link is announced as AnimeBytes.
			sources[idx].link.AB = max(sources[idx].link.AB, link.AB)
			continue
		}
		seen[key] = len(sources)
		sources = append(sources, sourcedLink{link: link, rank: rank})
	}
	return sources
}

// compareSourcedLinks orders collected links headline-group-first, then by URL,
// then by tracker - the deterministic total order obtainableLinks documents.
func compareSourcedLinks(a, b sourcedLink) int {
	if a.rank != b.rank {
		if a.rank < b.rank {
			return -1
		}
		return 1
	}
	if c := strings.Compare(a.link.URL, b.link.URL); c != 0 {
		return c
	}
	return strings.Compare(a.link.Tracker, b.link.Tracker)
}

// finalize sets a finding's status.
func finalize(f *Finding, status Status) *Finding {
	f.Status = status
	return f
}

// --- Headline candidate selection ---

// representative picks the headline recommended release: highest resolution,
// then a public tracker, then the stable content key (never upstream order).
// It assumes len(pool) > 0. Each candidate's stable key is memoized so it is
// hashed at most once per pool rather than once per equal-rank comparison:
// candidateStableKey streams the candidate's raw components (including
// attacker-controlled URLs) through SHA-256 when oversized, so recomputing
// the incumbent's key per comparison would make the hashing WORK (not the
// bounded output) quadratic on hostile data - up to 512 tied candidates with
// multi-MB URLs per entry.
func representative(pool []candidate) candidate {
	keys := make([]string, len(pool)) // candidateStableKey memo; "" = not yet computed (a real key is never empty)
	keyOf := func(i int) string {
		if keys[i] == "" {
			keys[i] = candidateStableKey(&pool[i])
		}
		return keys[i]
	}
	bestIdx := 0
	for i := 1; i < len(pool); i++ {
		if betterCandidate(&pool[i], &pool[bestIdx], keyOf(i), keyOf(bestIdx)) {
			bestIdx = i
		}
	}
	return pool[bestIdx]
}

// betterCandidate reports whether a should outrank b as the headline
// recommendation (higher resolution, then public-over-private tracker, then
// the candidates' precomputed stable content keys keyA/keyB). The final
// tie-break must not fall through to upstream slice order: the chosen
// candidate's identity enters the dedupe key, so two equal-ranked candidates
// arriving in the opposite relation order from PocketBase would otherwise
// flip the headline and emit a different key for an unchanged finding (a
// duplicate alert plus a false resolution).
func betterCandidate(a, b *candidate, keyA, keyB string) bool {
	ra, rb := release.ResolutionRank(a.rel.Resolution), release.ResolutionRank(b.rel.Resolution)
	if ra != rb {
		return ra > rb
	}
	aPublic := a.rel.TrackerType == tracker.Public
	bPublic := b.rel.TrackerType == tracker.Public
	if aPublic != bPublic {
		return aPublic
	}
	return keyA < keyB
}

// candidateStableKey is the deterministic content identity that breaks
// equal-rank headline ties independently of upstream order: the same
// candidate set always selects the same representative, whatever order
// PocketBase returned the torrents relation in. Delimiters are escaped
// element-wise so a field containing the join delimiter cannot make two
// distinct candidates compare equal, and the component set is size-bounded
// (keyenc.BoundedJoinParts, the same encoding notify's dedupe keys use):
// representative memoizes each candidate's key, but the components are still
// attacker-controlled URLs across up to 512 torrents per entry, so an
// unbounded escaped join would recreate the memory amplification the
// bounding removed (CWE-400). Components within the bound keep the exact
// escaped representation, so ordinary headline selection is unchanged.
func candidateStableKey(c *candidate) string {
	return keyenc.BoundedJoinParts([]string{
		release.NormalizeGroup(c.rel.Group),
		strings.ToLower(strings.TrimSpace(c.rel.Tracker)),
		strings.ToLower(strings.TrimSpace(c.rel.Resolution)),
		strings.ToLower(strings.TrimSpace(c.rel.Codec)),
		string(c.rel.Kind),
		c.rel.Reason,
		strings.TrimSpace(c.torrent.InfoHash),
		strings.TrimSpace(classify.PublishURL(&c.torrent)),
		strconv.FormatBool(c.rel.DualAudio),
	})
}

// groupSet returns the sorted distinct normalized groups of the given releases.
func groupSet(cands []candidate) []string {
	seen := make(map[string]struct{}, len(cands))
	var groups []string
	for i := range cands {
		g := release.NormalizeGroup(cands[i].rel.Group)
		if _, dup := seen[g]; dup {
			continue
		}
		seen[g] = struct{}{}
		groups = append(groups, g)
	}
	slices.Sort(groups)
	return groups
}
