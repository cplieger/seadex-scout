// Package compare turns matched SeaDex entries into findings. For each entry in
// the library it classifies the SeaDex "best" releases, keeps those that pass
// the content filters (remux/dual-audio) AND are obtainable (on a public
// tracker, or on AnimeBytes when the operator enables it), and compares the
// surviving recommended release groups against the groups present on the
// library item. The comparison is season-scoped and decided by the shared
// internal/align decision core (align.Decide) - the same decision the audit
// report renders, so a daemon finding never disagrees with the report: a
// mapped TVDB season against that season's groups, a special against Sonarr's
// season-0 bucket, a movie against its groups, and an absolute-numbered or
// title-only run against every real season conservatively -
// so a later season that needs a better release is not masked by an earlier
// season that already has it. An item that already has a recommended group is
// aligned and produces no finding; a recommended release the operator cannot
// obtain is simply absent, never a finding.
package compare

import (
	"slices"
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
	// currentGroups preserves the scoped on-disk group set with its element
	// boundaries for dedupe-key generation: CurrentGroup is the flattened
	// display join, where ["a,b","c"] and ["a","b,c"] are indistinguishable.
	// Unexported (never serialized); nil on manually constructed findings,
	// which fall back to the flattened CurrentGroup in dedupeKey.
	currentGroups []string
	AniListID     int  `json:"al_id"`
	Season        int  `json:"season,omitempty"`
	DualAudio     bool `json:"dual_audio,omitempty"`
}

// Comparer produces findings from matches under a fixed filter policy.
type Comparer struct {
	opts            filter.Options
	excludeSpecials bool
}

// Config configures a Comparer.
type Config struct {
	Filter          filter.Options
	ExcludeSpecials bool
}

// NewComparer builds a Comparer from cfg.
func NewComparer(cfg Config) *Comparer {
	return &Comparer{
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
// for an aligned unit, the classify.Fallback nudge when no recommended
// release survives the filters, a mixed_group_manual nudge for a not-aligned
// multi-group unit, and a better release otherwise.
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
	base := c.baseFinding(m, d.Groups)
	switch d.Outcome {
	case align.OutcomeNoBest:
		return emptyResult(entry, &base)
	case align.OutcomeAligned:
		return nil
	case align.OutcomeMixed:
		fillBest(&base, recommended, recGroups)
		return finalize(&base, StatusMixedGroup, SevInfo)
	default: // align.OutcomeDiverged
		return betterResult(entry, &base, recommended, recGroups)
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
		if !classify.ABVisible(t, c.opts.AnimeBytes) {
			continue
		}
		rel := classify.Torrent(entry, t)
		if ok, _ := filter.KeepNonTracker(&rel, c.opts); !ok {
			continue
		}
		if !filter.Obtainable(&rel, t.URL, c.opts) {
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
	status, sev := StatusBetter, SevWarn
	if entry.Incomplete {
		status, sev = StatusIncomplete, SevInfo
	}
	fillBest(base, recommended, recGroups)
	return finalize(base, status, sev)
}

// emptyResult decides the finding when no recommended release survives the
// content and obtainability filters: a theoretical-best-only or incomplete
// entry is an info nudge, everything else (nothing the operator can get) is
// silent. The precedence lives in classify.Fallback, shared with the audit
// report's rowQualifier so the two flows cannot drift.
func emptyResult(entry *seadex.Entry, base *Finding) *Finding {
	switch classify.Fallback(entry) {
	case classify.FallbackTheoretical:
		return finalize(base, StatusTheoretical, SevInfo)
	case classify.FallbackIncomplete:
		return finalize(base, StatusIncomplete, SevInfo)
	default:
		return nil
	}
}

// baseFinding seeds a finding with the item identity fields, using the groups
// the shared decision judged the unit against (align.Decision.Groups: the
// mapped season's groups, or the whole-series union) - so a season-scoped
// finding's CurrentGroup and dedupe key never leak whole-series groups.
func (c *Comparer) baseFinding(m *match.Match, groups []string) Finding {
	return Finding{
		Title:         m.Item.Title,
		Arr:           m.Arr,
		ArrURL:        m.Item.ArrURL,
		CurrentGroup:  strings.Join(groups, ","),
		currentGroups: slices.Clone(groups),
		AniListID:     m.Entry.AniListID,
		Season:        max(0, m.Record.SeasonTvdb),
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
// Nyaa and an AnimeBytes link for the same recommended release. The dedupe
// keys on the ReleaseLink value itself (a comparable struct), so a crafted
// tracker or URL containing a would-be delimiter cannot collide two distinct
// pairs.
func obtainableLinks(pool []candidate) []ReleaseLink {
	seen := make(map[ReleaseLink]struct{}, len(pool))
	var links []ReleaseLink
	for i := range pool {
		u := pool[i].torrent.UsableURL()
		if u == "" {
			continue
		}
		link := ReleaseLink{Tracker: pool[i].rel.Tracker, URL: u}
		if _, dup := seen[link]; dup {
			continue
		}
		seen[link] = struct{}{}
		links = append(links, link)
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
// group, and release identity, so a same-group quality swap (new identity) or a
// changed library state re-surfaces while an unchanged finding is suppressed.
// It is AnimeBytes-aware two ways: SeaDex redacts AB info hashes, so the
// identity falls back to the release URL (releaseIdentity), and the AB link set
// is appended when present (animeBytesLinkKey), so enabling AnimeBytes on an
// existing public-tracker finding re-surfaces the newly obtainable AB source.
// The untrusted components (group names, the current group, and the release
// identity - all parsed from SeaDex data or library file names) have their
// delimiter characters escaped (escapeDedupePart) before joining, so a value
// that itself contains the ',' or '|' delimiter cannot collide two distinct
// findings onto one key (which would suppress the second as already alerted),
// while a delimiter-free value keeps its legacy representation and existing
// persisted dedupe state stays valid.
func dedupeKey(f *Finding) string {
	groups := slices.Clone(f.RecommendedGroups)
	slices.Sort(groups)
	key := strings.Join([]string{
		strconv.Itoa(f.AniListID),
		string(f.Status),
		escapeJoinParts(groups),
		currentGroupKey(f),
		escapeDedupePart(releaseIdentity(f)),
	}, "|")
	if abLinks := animeBytesLinkKey(f.Links); abLinks != "" {
		key += "|ab=" + abLinks
	}
	return key
}

// escapeJoinParts escapes each part with escapeDedupePart BEFORE comma-joining,
// so element boundaries survive in the encoding: a part that itself contains a
// comma is escaped while the joining commas stay raw, making ["a,b"] and
// ["a","b"] encode differently. Delimiter-free parts stay byte-identical to
// their naive join.
func escapeJoinParts(parts []string) string {
	escaped := make([]string, len(parts))
	for i, p := range parts {
		escaped[i] = escapeDedupePart(p)
	}
	return strings.Join(escaped, ",")
}

// currentGroupKey encodes the finding's current-group component for the dedupe
// key. When the structured group slice is present (production findings built
// by baseFinding), each element is escaped before joining so distinct group
// sets whose display joins collide (["a,b","c"] vs ["a","b,c"], or ["A","B"]
// vs the literal ["A,B"]) keep distinct keys. A manually constructed finding
// (nil currentGroups) falls back to escaping the flattened CurrentGroup;
// delimiter-free production keys are byte-identical either way.
func currentGroupKey(f *Finding) string {
	if f.currentGroups != nil {
		return escapeJoinParts(f.currentGroups)
	}
	return escapeDedupePart(f.CurrentGroup)
}

// dedupePartEscaper escapes the characters that participate in the dedupe-key
// grammar (the '|' field and ',' list delimiters, plus the '\' escape itself,
// escaped first so the mapping stays injective). Escaping only the reserved
// characters keeps every delimiter-free component byte-identical to its legacy
// unescaped form, so persisted dedupe keys from earlier versions remain valid.
var dedupePartEscaper = strings.NewReplacer(
	`\`, `\\`,
	",", `\,`,
	"|", `\|`,
)

// escapeDedupePart makes an untrusted dedupe-key component safe to join with
// the ',' and '|' delimiters (see dedupePartEscaper).
func escapeDedupePart(s string) string { return dedupePartEscaper.Replace(s) }

// releaseIdentity returns the stable torrent identity used by finding dedupe.
// SeaDex redacts AnimeBytes info hashes (the literal "<redacted>"), so use the
// unique torrent page URL there; otherwise every same-group AB replacement
// would keep the same key and the later replacement would be suppressed.
func releaseIdentity(f *Finding) string {
	hash := strings.TrimSpace(f.InfoHash)
	if hash == "" || strings.EqualFold(hash, "<redacted>") {
		return strings.TrimSpace(f.ReleaseURL)
	}
	return hash
}

// animeBytesLinkKey returns the sorted toggle-gated (AnimeBytes) link URLs of
// a finding as a single comma-joined string, or "" when the finding carries no
// such link, so the dedupe key changes when the AB source set changes. A link
// is toggle-gated when the URL-aware filter.ABVisible invariant - the same
// boundary candidate filtering uses - would hide it with the toggle off, so a
// mislabeled AB URL still keys the same as a correctly labeled one. Each URL
// has its delimiters escaped before joining, matching dedupeKey's
// collision-proofing: a SeaDex-supplied URL containing ',' or '|' cannot
// collide two link sets.
func animeBytesLinkKey(links []ReleaseLink) string {
	var urls []string
	for i := range links {
		if !filter.ABVisible(links[i].Tracker, links[i].URL, false) {
			urls = append(urls, escapeDedupePart(strings.TrimSpace(links[i].URL)))
		}
	}
	slices.Sort(urls)
	return strings.Join(urls, ",")
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
		if _, dup := seen[g]; dup {
			continue
		}
		seen[g] = struct{}{}
		groups = append(groups, g)
	}
	slices.Sort(groups)
	return groups
}
