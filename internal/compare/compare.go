// Package compare turns matched SeaDex entries into findings. For each entry in
// the library it classifies the SeaDex "best" releases, keeps those that pass
// the content filters (remux/dual-audio) AND are obtainable (on a public
// tracker, or on AnimeBytes when the operator enables it), and compares the
// surviving recommended release groups against the groups present on the
// library item. The comparison is season-scoped and decided by the shared
// internal/align decision core (align.Decide) - the same decision rules the
// audit report renders, so the two flows cannot drift on shared inputs (they
// deliberately prepare different ones: the report judges the raw SeaDex
// best/alt sets, this pass only its filtered obtainable recommendations): a
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
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
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
	// StatusUnverifiable means the comparison is indeterminate: the release
	// group evidence on at least one side is unknown (a group-less on-disk
	// file or a group-less SeaDex release, both carried as the release.NoGroup
	// sentinel) and could hide an alignment - so neither a confident aligned
	// silence nor a better_release warning is honest. An informational
	// manual-review nudge.
	StatusUnverifiable Status = "unverifiable"
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
		return finalize(&base, StatusUnverifiable, SevInfo)
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
	status, sev := StatusBetter, SevWarn
	if classify.DivergedIncomplete(entry) {
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
// and season the shared decision judged/attributed the unit against
// (align.Decision.Groups: the mapped season's groups, or the whole-series
// union; align.Decision.Season: the shared season label) - so a season-scoped
// finding's CurrentGroup and dedupe key never leak whole-series groups, and
// the season attribution cannot drift from the audit report's.
func baseFinding(m *match.Match, d *align.Decision) Finding {
	return Finding{
		Title:         m.Item.Title,
		Arr:           m.Arr,
		ArrURL:        m.Item.ArrURL,
		CurrentGroup:  strings.Join(d.Groups, ","),
		currentGroups: slices.Clone(d.Groups),
		AniListID:     m.Entry.AniListID,
		Season:        d.Season,
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

// --- Dedupe-key encoding ---

// dedupeKey keys a finding by AniList ID, status, recommended-group set, current
// group, release identity, and the full obtainable-source link set, so a
// same-group quality swap (new identity), a changed library state, or ANY
// change to the recommended sources re-surfaces while an unchanged finding is
// suppressed. The link-set component covers what the headline identity alone
// cannot: a NON-headline candidate's torrent replacement (a new tracker page
// URL) and an AnimeBytes toggle flip (AB links joining or leaving the set)
// both change the key, where previously only the headline candidate and the
// AB subset were keyed and a replaced secondary public source stayed
// suppressed forever.
// The untrusted components (group names, the current group, the release
// identity, and the link URLs - all parsed from SeaDex data or library file
// names) have their
// delimiter characters escaped (escapeDedupePart) before joining, so a value
// that itself contains the ',' or '|' delimiter cannot collide two distinct
// findings onto one key (which would suppress the second as already alerted),
// while a delimiter-free value keeps its legacy representation and existing
// persisted dedupe state stays valid. Every untrusted component is also
// size-bounded (boundedJoinParts/boundedPart): a component set larger than
// maxKeyComponentBytes is reduced to a fixed-size SHA-256 identity instead of
// being materialized into the key, so hostile bulk SeaDex data (hundreds of
// oversized URLs per entry) cannot amplify key construction into an
// out-of-memory failure.
func dedupeKey(f *Finding) string {
	groups := slices.Clone(f.RecommendedGroups)
	slices.Sort(groups)
	key := strings.Join([]string{
		strconv.Itoa(f.AniListID),
		string(f.Status),
		boundedJoinParts(groups),
		currentGroupKey(f),
		boundedPart(releaseIdentity(f)),
	}, "|")
	if linkSet := obtainableLinkKey(f.Links); linkSet != "" {
		key += "|links=" + linkSet
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
		return boundedJoinParts(f.currentGroups)
	}
	return boundedPart(f.CurrentGroup)
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

// maxKeyComponentBytes is the raw-size threshold above which a dedupe-key
// component (or component set) is reduced to a fixed-size SHA-256 identity
// instead of an escaped join. The untrusted components come from SeaDex data
// the client deliberately admits in bulk (up to 512 torrents per entry,
// arbitrarily long syntactically valid URLs), so materializing them into ever
// larger key strings - while the decoded catalogue is still resident - lets a
// compromised upstream drive peak memory past the deployment container limit
// (CWE-400). Honest components run well under this bound, so persisted dedupe
// keys keep their legacy escaped representation and remain valid.
const maxKeyComponentBytes = 8 << 10

// hashedKeyPrefix marks a hashed component identity; the raw encodings
// exclude it so the two output domains cannot collide (a small upstream
// component that literally spells "sha256:<hex>" would otherwise alias the
// hashed identity of a different, oversized component set).
const hashedKeyPrefix = "sha256:"

// boundedJoinParts returns escapeJoinParts(parts) when the components' raw
// size is within maxKeyComponentBytes, else the fixed-size hashed identity of
// the component set (see hashKeyParts). The threshold checks the raw sizes so
// an honest set's representation never depends on how many delimiters
// escaping added. An in-bound set whose escaped join itself begins with the
// hashed-identity prefix is routed through the hash too, keeping the raw and
// hashed output domains disjoint (injectivity across the size boundary).
func boundedJoinParts(parts []string) string {
	total := 0
	for _, p := range parts {
		total += len(p)
	}
	if total <= maxKeyComponentBytes {
		if joined := escapeJoinParts(parts); !strings.HasPrefix(joined, hashedKeyPrefix) {
			return joined
		}
	}
	return hashKeyParts(parts)
}

// boundedPart is boundedJoinParts for a single component: the escaped legacy
// form within the bound, the hashed identity above it (or when the escaped
// form would spell the hashed-identity prefix, keeping the domains disjoint).
func boundedPart(s string) string { return boundedJoinParts([]string{s}) }

// hashKeyParts streams each original component into SHA-256 under a
// length-prefixed encoding - element boundaries survive without ever joining
// the inputs into one allocation, so ["a,b"] and ["a","b"] hash differently -
// and returns the fixed-size "sha256:<hex>" identity.
func hashKeyParts(parts []string) string {
	h := sha256.New()
	var lenBuf [8]byte
	for _, p := range parts {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(p)))
		_, _ = h.Write(lenBuf[:])
		_, _ = h.Write([]byte(p))
	}
	return hashedKeyPrefix + hex.EncodeToString(h.Sum(nil))
}

// releaseIdentity returns the stable torrent identity used by finding dedupe,
// domain-tagged so the two identity sources can never alias each other: a
// VALIDATED 40-hex info hash ("hash:" + the lowercased hex), else the release
// page URL ("url:" + trimmed). The InfoHash is untrusted SeaDex data - the
// previous code trusted any non-redacted value verbatim as the identity, so a
// crafted or garbled hash field keyed the finding unvalidated; dedupe now
// applies the same seadex.ValidInfoHash gate the indexer feed already uses.
// SeaDex redacts AnimeBytes info hashes (ValidInfoHash rejects the redaction
// marker along with everything else non-hex), so every same-group AB
// replacement keys on its unique torrent page URL, as before.
func releaseIdentity(f *Finding) string {
	if h := seadex.ValidInfoHash(f.InfoHash); h != "" {
		return "hash:" + h
	}
	return "url:" + strings.TrimSpace(f.ReleaseURL)
}

// obtainableLinkKey returns a finding's full obtainable-source URL set
// (deduplicated by trimmed URL, sorted, bounded) as a single key component,
// or "" when the finding carries no links. Folding EVERY obtainable source
// into the key - not just the headline candidate's identity - re-surfaces a
// finding when any recommended source changes: a non-headline public-tracker
// torrent replacement (a new page URL) previously kept the key unchanged and
// was suppressed forever, and an AnimeBytes toggle flip changes the set
// exactly as the retired AB-only component did. Deduplicating by URL keeps
// the key label-insensitive: one source arriving twice (once mislabeled)
// keys once, so correcting the label later never re-alerts an unchanged
// source. The sorted raw set goes through boundedJoinParts, matching
// dedupeKey's collision-proofing and size-bounding: a SeaDex-supplied URL
// containing ',' or '|' cannot collide two link sets, and an oversized set
// (SeaDex admits up to 512 arbitrarily long URLs per entry) reduces to a
// fixed-size hash instead of one huge joined allocation.
func obtainableLinkKey(links []ReleaseLink) string {
	seen := make(map[string]struct{}, len(links))
	var urls []string
	for i := range links {
		u := strings.TrimSpace(links[i].URL)
		if u == "" {
			continue
		}
		if _, dup := seen[u]; dup {
			continue
		}
		seen[u] = struct{}{}
		urls = append(urls, u)
	}
	if len(urls) == 0 {
		return ""
	}
	slices.Sort(urls)
	return boundedJoinParts(urls)
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
	aPublic := a.rel.TrackerType == release.TrackerPublic
	bPublic := b.rel.TrackerType == release.TrackerPublic
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
// (boundedJoinParts, same as dedupeKey's components): representative memoizes
// each candidate's key, but the components are still attacker-controlled URLs
// across up to 512 torrents per entry, so an unbounded escaped join would
// recreate the memory amplification the dedupe-key bounding removed
// (CWE-400). Components within the bound keep the exact escaped
// representation, so ordinary headline selection is unchanged.
func candidateStableKey(c *candidate) string {
	return boundedJoinParts([]string{
		release.NormalizeGroup(c.rel.Group),
		strings.ToLower(strings.TrimSpace(c.rel.Tracker)),
		strings.ToLower(strings.TrimSpace(c.rel.Resolution)),
		strings.ToLower(strings.TrimSpace(c.rel.Codec)),
		string(c.rel.Kind),
		c.rel.Reason,
		strings.TrimSpace(c.torrent.InfoHash),
		strings.TrimSpace(c.torrent.UsableURL()),
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
