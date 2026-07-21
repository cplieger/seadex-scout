// Package release classifies a media release from its names, notes, and
// metadata into a normalized fingerprint: release group, tracker kind,
// resolution, video codec, dual-audio, and remux-vs-encode. It is pure (it
// operates on strings, not on SeaDex or arr types) so both the SeaDex side and
// the library side can classify into one shared vocabulary and be compared.
// It also owns the group vocabulary that comparison rests on: NormalizeGroup
// (the canonical spelling, with every no-group variant folded onto the NoGroup
// sentinel) and GroupsOverlap, the three-valued group-set comparison in which
// a NoGroup member is unknown evidence — it can neither prove an overlap nor
// permit a divergence proof — rather than an identity token. The same
// single-home rule extends to the canonical tracker table (trackers.go:
// names, aliases, obtainability class, site base URLs, and the fail-closed
// host-classification gates) and to the curation-warning tag vocabulary
// (curation.go), both of which this package owns so they cannot fork
// across consumers.
//
// The remux-vs-encode decision is deliberately name-and-notes based, never a
// size or bitrate inference: on SeaDex a remux is stated in the release name or
// the entry notes, which is what makes name parsing reliable here. An
// unclassifiable release is Unknown, never silently dropped.
//
// Dual-audio, by contrast, is never derived from name or notes text. SeaDex
// entry notes are entry-wide — they describe every release in the entry and
// can even negate a marker ("lacks dual audio"), so a text mention is
// unreliable evidence for any single release. Input.DualAudio, the caller's
// structured per-release metadata (SeaDex's per-torrent dualAudio flag, the
// arr's MediaInfo audio languages), is the only source.
package release

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// --- Classification vocabulary: kinds, tracker types, Release/Input ---

// Kind is the remux-vs-encode classification of a release.
type Kind string

const (
	// KindRemux is an untouched stream copy (a BD/BDMV remux).
	KindRemux Kind = "remux"
	// KindEncode is a transcode (x265/x264/HEVC/AVC, CRF/bitrate targeted).
	KindEncode Kind = "encode"
	// KindUnknown is a release that carries neither a remux nor an encode
	// marker; it is surfaced, never auto-dropped.
	KindUnknown Kind = "unknown"
)

// TrackerType is the obtainability class of a release's tracker.
type TrackerType string

const (
	// TrackerPublic is an openly accessible tracker (Nyaa).
	TrackerPublic TrackerType = "public"
	// TrackerPrivate is a private tracker requiring membership (AnimeBytes).
	TrackerPrivate TrackerType = "private"
	// TrackerUnknown is an unrecognized tracker.
	TrackerUnknown TrackerType = "unknown"
)

// Release is a normalized release fingerprint. Both a SeaDex torrent and a
// library file classify into this shape so they compare in the same vocabulary.
type Release struct {
	Group       string      `json:"group,omitempty"`
	Tracker     string      `json:"tracker,omitempty"`
	Resolution  string      `json:"resolution,omitempty"`
	Codec       string      `json:"codec,omitempty"`
	Kind        Kind        `json:"kind,omitempty"`
	TrackerType TrackerType `json:"tracker_type,omitempty"`
	Reason      string      `json:"reason,omitempty"`
	DualAudio   bool        `json:"dual_audio,omitempty"`
}

// Input is the raw material for Classify. Names are the release/file names to
// parse; Notes is the SeaDex entry notes (empty for a library file); Group and
// Tracker come from the source; VideoCodec is the arr MediaInfo codec (empty
// for SeaDex); DualAudio is the source's structured per-release dual-audio
// metadata (SeaDex's per-torrent dualAudio flag, or the arr's MediaInfo audio
// languages) and is passed through as-is — Classify never derives dual-audio
// from Names or Notes text (entry-wide notes are unreliable per-release
// evidence; see the package doc).
type Input struct {
	Notes      string
	Group      string
	Tracker    string
	VideoCodec string
	Names      []string
	DualAudio  bool
}

// --- Evidence parsing: marker regexes, codec tokens, Classify ---

// resolutionHeights is the single home of the recognized resolution
// vocabulary, highest first. reResolution's alternation and ResolutionRank
// both derive from it, so a height added to one consumer cannot silently
// miss the other (the same single-home rule trackerTable applies to the
// tracker vocabulary).
var resolutionHeights = []string{"2160p", "1440p", "1080p", "720p", "480p"}

// evidenceWordClass is the raw-text word alphabet the marker edges are
// defined against: the ASCII alphanumerics plus U+0130 (LATIN CAPITAL LETTER
// I WITH DOT ABOVE) and U+212A (KELVIN SIGN) — exactly the runes
// strings.ToLower folds onto an ASCII alphanumeric. The pre-optimization
// classifier lowercased the evidence and used [[:alnum:]] edges; defining
// word-ness on the raw text via this class preserves those exact token
// boundaries without allocating the lowercased copy. Underscore stays a
// delimiter (the old normalization replaced it with a space before matching).
const evidenceWordClass = `A-Za-z0-9\x{0130}\x{212A}`

// nonWordEdge matches one raw-text rune the old normalized comparison
// treated as a token delimiter: any rune outside evidenceWordClass.
const nonWordEdge = `[^` + evidenceWordClass + `]`

// lowerLiteralPattern renders a lowercase marker token as a regexp fragment
// matching exactly the raw spellings whose strings.ToLower image equals the
// token: each ASCII letter becomes an explicit case class — with U+0130
// added to the i class and U+212A to the k class, the only non-ASCII runes
// unicode.ToLower maps onto ASCII — digits match themselves, and anything
// else is quoted literally. A global (?i) is deliberately NOT used: regexp
// case folding follows unicode.SimpleFold, which diverges from
// strings.ToLower — (?i)s also matches U+017F (ſ), which ToLower never folds
// onto s, while (?i)i misses U+0130, which ToLower does fold onto i — so
// (?i) silently changes classification decisions on such runes (pinned by
// the Unicode rows in TestClassifyKind and TestClassifyResolution).
func lowerLiteralPattern(token string) string {
	var b strings.Builder
	for _, r := range token {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteByte('[')
			b.WriteRune(r)
			b.WriteRune(r - 'a' + 'A')
			switch r {
			case 'i':
				b.WriteString(`\x{0130}`)
			case 'k':
				b.WriteString(`\x{212A}`)
			}
			b.WriteByte(']')
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	return b.String()
}

// lowerTokensPattern joins the lowerLiteralPattern renderings of tokens into
// one regexp alternation.
func lowerTokensPattern(tokens []string) string {
	parts := make([]string, len(tokens))
	for i, t := range tokens {
		parts[i] = lowerLiteralPattern(t)
	}
	return strings.Join(parts, "|")
}

var (
	// reResolution matches a known resolution height with hand-built edges
	// instead of \b: Go regexp word boundaries require a non-word character
	// before the first digit, which misses compact spellings such as
	// "BD1080p" and "1920x1080p" that the live SeaDex catalogue uses. The
	// left edge rejects only a preceding digit (so "21080p" is not read as
	// 1080p) and the right edge rejects a word-rune continuation via
	// nonWordEdge (so "x1080py" stays unmatched); the height itself is
	// captured in group 1 for detectResolution.
	reResolution = regexp.MustCompile(`(?:^|[^0-9])(` + lowerTokensPattern(resolutionHeights) + `)(?:$|` + nonWordEdge + `)`)
	// reBitrate / reCRF / reRemux / reEncode match the raw evidence text in
	// place, built via lowerLiteralPattern (strings.ToLower-faithful case
	// classes; see its doc) with explicit nonWordEdge boundaries instead of
	// \b. Go regexp treats "_" as a word character, so \b would miss
	// underscore-delimited scene names such as Show_CRF18_BDRemux;
	// nonWordEdge treats "_" as a delimiter, and the optional separator
	// classes accept the full scene-delimiter set [\s._-] between token
	// halves — dot- and hyphen-joined spellings (CRF.18, 4500-kbps,
	// BD.Remux) are as real as the space/underscore forms, and accepting
	// them on one marker but not another made classification depend on
	// which delimiter a group happens to use. Matching in place means no
	// evidence-sized lowercased/underscore-replaced copy is ever allocated
	// for an upstream-controlled name or notes value.
	reBitrate = regexp.MustCompile(`(?:^|` + nonWordEdge + `)\d+[\s._-]?(?:` + lowerTokensPattern([]string{"kbps", "mbps"}) + `)(?:$|` + nonWordEdge + `)`)
	// reCRF matches an x264/x265 CRF tag such as "crf18", "crf 20", or "crf.18".
	reCRF = regexp.MustCompile(`(?:^|` + nonWordEdge + `)` + lowerLiteralPattern("crf") + `[\s._-]?\d+(?:$|` + nonWordEdge + `)`)
	// reRemux matches a remux marker as a delimiter-bounded token ("remux",
	// "BDRemux", "BD-Remux"), never a bare substring inside a longer word.
	// "PREMUX" is included deliberately: SeaDex uses it for pre-muxed
	// releases, and token-bounding alone would lose it (no word boundary
	// between the "p" and "remux"). The inflected "-ed" forms ("remuxed
	// from the JPBD", "BD-Remuxed") count too — reEncode already accepts
	// "encoded" alongside "encode", and rejecting the same inflection on
	// the remux side silently declassified stated remuxes to unknown.
	reRemux = regexp.MustCompile(`(?:^|` + nonWordEdge + `)(?:` + lowerLiteralPattern("bd") + `[\s._-]?)?(?:` + lowerTokensPattern([]string{"premux", "remux"}) + `)(?:` + lowerLiteralPattern("ed") + `)?(?:$|` + nonWordEdge + `)`)
	// reEncode matches a generic encode marker ("encode", "encoded", "BDRip")
	// with reRemux's delimiter-bounded token style, so a bare substring inside
	// a longer word ("reencoded", "encoder") is never a marker. It is the
	// weakest encoder-marker rung in kindFromEvidence — checked after the remux
	// token and the codec/CRF/bitrate markers, so it only ever moves a release
	// from unknown to encode, never off remux. Live SeaDex data motivates it:
	// many isBest encodes state "encode"/"BDRip" in their name or notes
	// without any codec, CRF, or bitrate marker and previously classified
	// unknown.
	reEncode = regexp.MustCompile(`(?:^|` + nonWordEdge + `)(?:` + lowerTokensPattern([]string{"bdrip", "encoded", "encode"}) + `)(?:$|` + nonWordEdge + `)`)
)

// Canonical codec families the classifier normalizes video codecs to.
const (
	codecX265 = "x265"
	codecX264 = "x264"
)

// x265Tokens / x264Tokens are the codec markers accepted in the authoritative
// MediaInfo codec value (canonicalCodec).
// The x265 family takes precedence when input contains markers from both families.
var (
	x265Tokens = []string{codecX265, "h265", "h.265", "hevc"}
	x264Tokens = []string{codecX264, "h264", "h.264", "avc"}
)

// x265TextTokens / x264TextTokens are the codec markers detected in release
// text by substring (compact spellings such as "BDx265" are real in the live
// catalogue, so no boundary is applied). The dotted spellings are excluded
// here and matched by reDottedX265/reDottedX264 instead, which require a
// non-alphanumeric left boundary: without it a title ending in "h" followed
// by a dot-delimited episode number ("Bleach.264.1080p") contains the
// substring "h.264" and misclassifies the release as an x264 encode.
var (
	x265TextTokens = []string{codecX265, "h265", "hevc"}
	x264TextTokens = []string{codecX264, "h264", "avc"}
	// reTextX265 / reTextX264 apply the text-token lists to raw evidence in
	// place (ToLower-faithful case classes via lowerTokensPattern, no
	// boundary — see above), so codec detection needs no lowercased copy of
	// the evidence. The alternations derive from the token lists to keep the
	// vocabulary single-homed.
	reTextX265 = regexp.MustCompile(lowerTokensPattern(x265TextTokens))
	reTextX264 = regexp.MustCompile(lowerTokensPattern(x264TextTokens))
	// reDottedX265 / reDottedX264 require a non-word left boundary
	// (nonWordEdge, the same raw-text word set the marker edges use).
	reDottedX265 = regexp.MustCompile(`(?:^|` + nonWordEdge + `)` + lowerLiteralPattern("h.265"))
	reDottedX264 = regexp.MustCompile(`(?:^|` + nonWordEdge + `)` + lowerLiteralPattern("h.264"))
)

// evidence accumulates the classification signals of one text source (the
// release names, or the entry notes) one observed piece at a time, so a large
// evidence set — up to thousands of upstream-controlled file names per SeaDex
// torrent — is never materialized as a single joined and normalized string
// (which cost several simultaneous evidence-sized allocations and could OOM a
// memory-limited container on a malformed page). Each piece is matched IN
// PLACE by the ToLower-faithful, underscore-aware marker regexes (built via
// lowerLiteralPattern) — no
// per-piece lowercased or underscore-replaced copy is allocated either, so
// even a single decode-cap-sized name or notes value adds no evidence-sized
// allocations on top of the decoded source string. Only the marker flags, the
// codec-family flags, and the first observed resolution are retained. The
// original whole-text precedence is preserved by resolving over the
// accumulated flags: first resolution in observation order, the x265 family
// over x264 (textCodec), and remux over the encoder-marker rungs
// (kindFromEvidence).
type evidence struct {
	resolution string
	x265       bool
	x264       bool
	remux      bool
	crf        bool
	bitrate    bool
	encode     bool
}

// observe folds one piece of evidence text (a single release/file name, or the
// entry notes) into the accumulator. Already-set flags short-circuit their
// matchers; the matchers run against the text in place, allocating nothing
// evidence-sized.
func (e *evidence) observe(text string) {
	if e.resolution == "" {
		e.resolution = detectResolution(text)
	}
	e.x265 = e.x265 || reTextX265.MatchString(text) || reDottedX265.MatchString(text)
	e.x264 = e.x264 || reTextX264.MatchString(text) || reDottedX264.MatchString(text)
	e.remux = e.remux || reRemux.MatchString(text)
	e.crf = e.crf || reCRF.MatchString(text)
	e.bitrate = e.bitrate || reBitrate.MatchString(text)
	e.encode = e.encode || reEncode.MatchString(text)
}

// textCodec resolves the accumulated codec-family markers to the canonical
// codec, x265 family first (the family precedence when evidence carries
// markers from both), or "" when neither family was observed.
func (e *evidence) textCodec() string {
	switch {
	case e.x265:
		return codecX265
	case e.x264:
		return codecX264
	default:
		return ""
	}
}

// Classify converts raw release material into a normalized Release. It never
// errors: an unclassifiable release is KindUnknown with a recorded reason.
// DualAudio passes through from the structured input flag untouched — text is
// never evidence for it (see the Input and package docs).
func Classify(in *Input) Release {
	var nameEv, notesEv evidence
	for _, name := range in.Names {
		nameEv.observe(name)
	}
	notesEv.observe(in.Notes)
	// The Codec FIELD folds the authoritative MediaInfo value in first, then
	// name tokens, then notes (per-file evidence wins, the entry-wide notes
	// only fill the gap). The KIND decision deliberately excludes MediaInfo:
	// every video stream HAS a codec, so MediaInfo reporting AVC/HEVC is a
	// property of the file, never a statement that it is an encode — a BD
	// remux's stream is AVC/HEVC too, and feeding MediaInfo into the
	// encoder-marker rung misclassified every marker-less library remux as
	// an encode. Only a codec token someone WROTE (release name or notes) is
	// encode evidence, so the Kind reason names the written token, which can
	// differ from the authoritative Codec field when they disagree.
	mediaCodec := canonicalCodec(strings.ToLower(strings.TrimSpace(in.VideoCodec)))
	nameCodec := nameEv.textCodec()
	notesCodec := notesEv.textCodec()
	codec := mediaCodec
	if codec == "" {
		codec = nameCodec
	}
	if codec == "" {
		codec = notesCodec
	}
	kind, reason := classifyKind(&nameEv, &notesEv, nameCodec, notesCodec)
	resolution := nameEv.resolution
	if resolution == "" {
		resolution = notesEv.resolution
	}

	return Release{
		Group:       groupOrNoGroup(in.Group),
		Tracker:     strings.TrimSpace(in.Tracker),
		Resolution:  resolution,
		Codec:       codec,
		Kind:        kind,
		TrackerType: classifyTracker(in.Tracker),
		Reason:      reason,
		DualAudio:   in.DualAudio,
	}
}

// detectResolution extracts the normalized resolution height from evidence
// text via reResolution's capture group (the edge characters the pattern
// consumes around it are not part of the value), or "" when no marker is
// present.
func detectResolution(text string) string {
	match := reResolution.FindStringSubmatch(text)
	if len(match) < 2 {
		return ""
	}
	return strings.ToLower(match[1])
}

// classifyKind applies per-file-evidence-first scoping to the remux -> encode
// -> unknown rules. The release names are classified first and win for this
// release; the entry-wide SeaDex notes only fill the gap when the names carry
// no marker, so a notes-level remux note cannot override a contradicting
// per-file encode marker. The codec arguments are the TEXT-observed families
// only — the MediaInfo codec is deliberately not kind evidence (see Classify).
// The remux decision stays name-and-notes based (never size/bitrate
// inference), so no operator-supplied group list is needed.
func classifyKind(nameEv, notesEv *evidence, nameCodec, notesCodec string) (kind Kind, reason string) {
	if kind, reason := kindFromEvidence(nameEv, nameCodec); kind != KindUnknown {
		return kind, reason
	}
	return kindFromEvidence(notesEv, notesCodec)
}

// kindFromEvidence classifies one accumulated evidence source (names or notes)
// in isolation: a delimiter-bounded remux token (reRemux) wins, then an
// encoder marker (codec, CRF tag, bitrate, or a generic encode token —
// reEncode, the weakest rung), else unknown. It returns the kind and a short
// reason for observability.
func kindFromEvidence(e *evidence, codec string) (kind Kind, reason string) {
	if e.remux {
		return KindRemux, "name/notes marker: remux"
	}
	switch {
	case codec != "":
		return KindEncode, "encoder marker: " + codec
	case e.crf:
		return KindEncode, "encoder marker: crf"
	case e.bitrate:
		return KindEncode, "encoder marker: bitrate"
	case e.encode:
		return KindEncode, "encoder marker: encode"
	}
	return KindUnknown, "no remux or encode marker"
}

// canonicalCodec maps a MediaInfo codec token to the canonical codec family.
func canonicalCodec(s string) string {
	switch {
	case s == "":
		return ""
	case containsAny(s, x265Tokens):
		return codecX265
	case containsAny(s, x264Tokens):
		return codecX264
	default:
		return ""
	}
}

// --- Tracker classification ---

// classifyTracker maps a tracker name to its obtainability class via the
// canonical tracker table (LookupTracker).
func classifyTracker(tracker string) TrackerType {
	t, ok := LookupTracker(tracker)
	if !ok {
		return TrackerUnknown
	}
	return t.Type
}

// IsAnimeBytes reports whether the tracker name is AnimeBytes (SeaDex uses
// "AB"; "animebytes" is accepted defensively via the table aliases). It is the
// one private tracker seadex-scout gates behind an opt-in toggle (the operator
// has an account), so obtainability can single it out from other private
// trackers.
func IsAnimeBytes(tracker string) bool {
	t, ok := LookupTracker(tracker)
	return ok && t.Name == TrackerNameAnimeBytes
}

// IsAnimeBytesHost reports whether a URL host (case-insensitively; one
// DNS-root trailing dot tolerated) is the AnimeBytes site host or one of its
// dot-delimited subdomains, resolved through the canonical tracker table
// (LookupTrackerByHost). It is the URL-host twin of IsAnimeBytes (the
// tracker-name check), so the AB classification rule has one home.
func IsAnimeBytesHost(host string) bool {
	t, ok := LookupTrackerByHost(host)
	return ok && t.Name == TrackerNameAnimeBytes
}

// IsNyaaHost reports whether a URL host (case-insensitively; one DNS-root
// trailing dot tolerated) is the Nyaa site host or one of its dot-delimited
// subdomains, resolved through the canonical tracker table
// (LookupTrackerByHost), mirroring IsAnimeBytesHost so the
// host-classification rule has one home.
func IsNyaaHost(host string) bool {
	t, ok := LookupTrackerByHost(host)
	return ok && t.Name == TrackerNameNyaa
}

// --- Group vocabulary and three-valued overlap ---

// NoGroup is the placeholder release group for a release that specifies none.
// SeaDex already tags some group-less releases with the literal "NOGRP", so
// falling back to it keeps a group-less release a first-class, serializable
// value — in stored findings, dedupe keys, and report cells — instead of an
// empty string that gets skipped. It is a serialization and display token
// carrying UNKNOWN EVIDENCE, never an identity: the decision layer
// (GroupsOverlap) treats a NoGroup member as "the group could not be
// determined", so two NoGroup members are never read as the same group.
const NoGroup = "NOGRP"

// noGroupNormalized is NoGroup in NormalizeGroup's canonical lowercase form:
// the one token GroupsOverlap classifies as unknown evidence.
var noGroupNormalized = strings.ToLower(NoGroup)

// groupOrNoGroup trims a release group, falling back to NoGroup when none is
// given, so a group-less release is a first-class comparable value rather than
// an empty string that gets skipped.
func groupOrNoGroup(group string) string {
	if g := strings.TrimSpace(group); g != "" {
		return g
	}
	return NoGroup
}

// noGroupVariants are the spellings of "no release group" (lowercased) that
// NormalizeGroup folds onto the canonical NoGroup, so a SeaDex side or library
// side using any variant compares equal to a group-less release.
var noGroupVariants = map[string]bool{
	"nogrp": true, "nogroup": true, "no-group": true, "no_group": true, "no group": true,
}

// NormalizeGroup lowercases and trims a release-group name for override and
// comparison lookups (SeaDex and arr casing differ), so the compare layer keys
// group-membership sets the same way Classify keys overrides. An empty group
// and every no-group spelling variant (NOGRP, NoGroup, no-group, ...)
// normalizes to NoGroup, the canonical unknown-evidence token, so a missing
// group serializes identically however it was spelled.
func NormalizeGroup(group string) string {
	g := strings.ToLower(strings.TrimSpace(group))
	if g == "" || noGroupVariants[g] {
		return noGroupNormalized
	}
	return g
}

// Overlap is the three-valued outcome of comparing two release-group sets.
// Group evidence parsed from untrusted release names is inherently
// three-valued — a known group, a known different group, or unknown (the
// NoGroup sentinel and its spelling variants) — so a set comparison cannot
// collapse to a boolean without reading absence of evidence as evidence:
// a known shared group proves overlap, all-known disjoint sets prove
// divergence, and an unknown member that could hide a shared group proves
// nothing either way.
type Overlap int

const (
	// OverlapNone means every member on both sides is known evidence and no
	// group is shared: a proven divergence. An empty side is also None —
	// nothing can overlap with an empty set, and an unknown member cannot
	// hide a match against it.
	OverlapNone Overlap = iota
	// OverlapKnown means a known group on one side is present, known, on the
	// other: proven common membership. Known evidence wins outright, whatever
	// unknown members ride along in either set.
	OverlapKnown
	// OverlapUnknown means the comparison is indeterminate: no known group is
	// shared, and at least one side carries an unknown member (NoGroup) while
	// the other side is non-empty — the unknown member could be any group,
	// including one that would make the sets overlap, so neither overlap nor
	// divergence is proven.
	OverlapUnknown
)

// groupEvidence partitions one group set into its known members (normalized
// via NormalizeGroup, as a set) and whether it carries unknown evidence (a
// member that normalizes to the NoGroup sentinel). Both sides of
// GroupsOverlap share this partition so the normalization rule lives in
// exactly one place.
func groupEvidence(groups []string) (known map[string]struct{}, unknown bool) {
	known = make(map[string]struct{}, len(groups))
	for _, group := range groups {
		normalized := NormalizeGroup(group)
		if normalized == noGroupNormalized {
			unknown = true
			continue
		}
		known[normalized] = struct{}{}
	}
	return known, unknown
}

// GroupsOverlap is the shared three-valued group-set comparison the compare
// and audit layers key alignment on, comparing both sides normalized
// (NormalizeGroup) so the overlap decision lives in exactly one place. A
// member that normalizes to the NoGroup sentinel is unknown evidence, never
// an identity token: it can neither prove an overlap (sentinel∩sentinel is
// OverlapUnknown, not a match) nor allow a divergence proof while it could
// hide a match. It operates only on []string, keeping release a pure,
// seadex-free leaf.
func GroupsOverlap(a, b []string) Overlap {
	knownA, unknownA := groupEvidence(a)
	knownB, unknownB := groupEvidence(b)
	for group := range knownA {
		if _, ok := knownB[group]; ok {
			return OverlapKnown
		}
	}
	if (unknownA && len(b) > 0) || (unknownB && len(a) > 0) {
		return OverlapUnknown
	}
	return OverlapNone
}

// --- Ranking and generic helpers ---

// ResolutionRank returns a comparable rank for a resolution string (its height
// in pixels; higher is better). An empty or unrecognized resolution ranks 0, so
// a resolution floor never drops a release whose resolution could not be
// parsed.
func ResolutionRank(res string) int {
	r := strings.ToLower(strings.TrimSpace(res))
	if !slices.Contains(resolutionHeights, r) {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSuffix(r, "p"))
	if err != nil {
		return 0
	}
	return n
}

// containsAny reports whether s contains any of the substrings.
func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
