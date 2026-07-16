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
// nil when there is nothing to report: the mapped scope has no file on disk
// (the audit's no_file; checked first, before anything about the entry), the
// item is aligned (already has a recommended group), or no recommended release
// survives the filters and the entry is neither incomplete nor
// theoretical-best.
func (c *Comparer) compareOne(m *match.Match) *Finding {
	entry := &m.Entry
	recommended := c.recommended(entry)

	// Scope the on-disk groups the same way the audit report does (movie / the
	// mapped TVDB season / the season-0 specials bucket), via the shared
	// internal/align, so a daemon finding never disagrees with the report.
	scoped := align.Scope(m.Item, &m.Record)
	if scoped.Kind == align.ScopeWholeSeries {
		// A Sonarr absolute-numbered run / title-only match has no per-season
		// Fribb mapping, so its single whole-series recommendation is compared
		// against every real season on disk, conservatively (compareWholeSeries)
		// - exactly as the audit report does.
		return c.compareWholeSeries(m, recommended)
	}
	if !scoped.HasFile {
		// File presence first, before the recommendation-emptiness check: the
		// mapped season/movie/special is not on disk, so there is nothing the
		// operator has for any recommendation (or incomplete/theoretical nudge)
		// to apply to. The audit records this scope as no_file; compare has no
		// no-file status, so report-by-exception means the daemon stays quiet.
		return nil
	}
	base := c.baseFinding(m, scoped.Groups)
	if len(recommended) == 0 {
		return emptyResult(entry, &base)
	}

	recGroups := groupSet(recommended)
	if release.GroupsIntersect(recGroups, scoped.Groups) {
		return nil // aligned: a recommended group is already present
	}
	// Alignment wins over the mixed-group nudge: a season that already carries a
	// recommended group is aligned no matter how many groups it spans (exactly
	// as the audit reports it). Only a NOT-aligned multi-group season needs the
	// manual review. The scoped group count, not the whole-item group set, so a
	// season that carries a single group is not misreported as
	// mixed_group_manual.
	if len(scoped.Groups) > 1 {
		fillBest(&base, recommended, recGroups)
		return finalize(&base, StatusMixedGroup, SevInfo)
	}

	// Not aligned: a better release the operator can obtain and lacks.
	return betterResult(entry, &base, recommended, recGroups)
}

// compareWholeSeries compares a Sonarr whole-series entry (an absolute-numbered
// run or title-only match, with no per-season Fribb mapping) against every real
// season on disk (season 0 excluded), conservatively: the item is aligned only
// when every on-disk season already carries a recommended group, matching the
// audit report's whole-series verdict via the shared align.SummarizeWholeSeries.
// It stays silent when no real season is on disk (checked first, before
// anything about the entry - the audit's no_file), and a not-aligned aggregate
// spanning more than one group is a mixed_group_manual nudge, exactly as in the
// season-scoped arm.
func (c *Comparer) compareWholeSeries(m *match.Match, recommended []candidate) *Finding {
	entry := &m.Entry
	recGroups := groupSet(recommended)
	// nil alt: the daemon only distinguishes best-vs-not, so an on-disk season
	// lacking a recommended group surfaces as AnyUnlisted.
	summary := align.SummarizeWholeSeries(m.Item, recGroups, nil)
	if summary.Seasons == 0 {
		// File presence first, before the recommendation-emptiness check
		// (mirroring compareOne): no real season on disk means nothing for any
		// recommendation (or incomplete/theoretical nudge) to apply to. The
		// audit records this as no_file; the daemon stays quiet.
		return nil
	}
	base := c.baseFinding(m, summary.Groups)
	if len(recommended) == 0 {
		return emptyResult(entry, &base)
	}
	if !summary.AnyUnlisted {
		return nil // aligned: every on-disk season already carries a recommended group
	}
	// Alignment wins over the mixed-group nudge, exactly as in compareOne: a
	// NOT-aligned aggregate spanning more than one group cannot attribute one
	// current group, so it is a manual-review nudge rather than a false
	// better_release.
	if len(summary.Groups) > 1 {
		fillBest(&base, recommended, recGroups)
		return finalize(&base, StatusMixedGroup, SevInfo)
	}

	// At least one on-disk season lacks a recommended group.
	return betterResult(entry, &base, recommended, recGroups)
}

// recommended classifies the entry's SeaDex "best" torrents and returns those
// the operator could act on: passing the content filters (remux policy,
// dual-audio) AND obtainable (a public tracker, or AnimeBytes when enabled).
func (c *Comparer) recommended(entry *seadex.Entry) []candidate {
	var out []candidate
	for i := range entry.Torrents {
		t := &entry.Torrents[i]
		if !t.IsBest {
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

// betterResult finalizes a not-aligned finding: a better release the operator
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

// baseFinding seeds a finding with the item identity fields, using the scope
// groups already resolved by the caller - the mapped season's groups, or the
// whole-series union for a whole-series comparison - so a season-scoped
// finding's CurrentGroup and dedupe key never leak whole-series groups.
func (c *Comparer) baseFinding(m *match.Match, groups []string) Finding {
	return Finding{
		Title:        m.Item.Title,
		Arr:          m.Arr,
		ArrURL:       m.Item.ArrURL,
		CurrentGroup: strings.Join(groups, ","),
		AniListID:    m.Entry.AniListID,
		Season:       max(0, m.Record.SeasonTvdb),
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
	for i := range groups {
		groups[i] = escapeDedupePart(groups[i])
	}
	key := strings.Join([]string{
		strconv.Itoa(f.AniListID),
		string(f.Status),
		strings.Join(groups, ","),
		escapeDedupePart(f.CurrentGroup),
		escapeDedupePart(releaseIdentity(f)),
	}, "|")
	if abLinks := animeBytesLinkKey(f.Links); abLinks != "" {
		key += "|ab=" + abLinks
	}
	return key
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

// animeBytesLinkKey returns the sorted AnimeBytes link URLs of a finding as a
// single comma-joined string, or "" when the finding carries no AB link, so
// the dedupe key changes when the AB source set changes. Each URL has its
// delimiters escaped before joining, matching dedupeKey's collision-proofing:
// a SeaDex-supplied URL containing ',' or '|' cannot collide two link sets.
func animeBytesLinkKey(links []ReleaseLink) string {
	var urls []string
	for i := range links {
		if release.IsAnimeBytes(links[i].Tracker) {
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
