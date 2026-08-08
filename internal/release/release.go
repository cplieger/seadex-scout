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
// single-home rule extends to the curation-warning tag vocabulary
// (curation.go), which this package owns so it cannot fork across consumers.
// Tracker identity is NOT this package's: the canonical table, the
// obtainability class, and the fail-closed host/relative-URL gates live in
// internal/tracker, which this package reads only for a Release's
// TrackerType — so the pure classifier carries no URL parsing and every
// tracker consumer reaches the vocabulary without importing the whole
// classification engine.
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

	"github.com/cplieger/seadex-scout/internal/nametoken"
	"github.com/cplieger/seadex-scout/internal/tracker"
)

// --- Classification vocabulary: kinds, Release/Input ---

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

// Release is a normalized release fingerprint. Both a SeaDex torrent and a
// library file classify into this shape so they compare in the same vocabulary.
// TrackerType is the obtainability class resolved from the canonical tracker
// vocabulary (internal/tracker), the one home of tracker identity.
type Release struct {
	Group       string       `json:"group,omitempty"`
	Tracker     string       `json:"tracker,omitempty"`
	Resolution  string       `json:"resolution,omitempty"`
	Codec       string       `json:"codec,omitempty"`
	Kind        Kind         `json:"kind,omitempty"`
	TrackerType tracker.Type `json:"tracker_type,omitempty"`
	Reason      string       `json:"reason,omitempty"`
	DualAudio   bool         `json:"dual_audio,omitempty"`
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
// vocabulary. reResolution's alternation and ResolutionRank both derive
// from it, so a height added to one consumer cannot silently miss the
// other (the same single-home rule internal/tracker's table applies to the tracker
// vocabulary). The descending order is presentational only: no two
// heights can match at the same offset, so detectResolution takes the
// FIRST height in the evidence text, never the highest present (the
// first-in-observation-order rule the evidence accumulator documents) -
// adding or reordering a height changes nothing about precedence.
var resolutionHeights = []string{"2160p", "1440p", "1080p", "720p", "480p"}

// The marker edges below are defined against the shared release-name word
// alphabet (nametoken.NonWordEdge: the ASCII alphanumerics plus the two runes
// strings.ToLower folds onto an ASCII letter, with underscore a delimiter), and
// the marker tokens against the shared strings.ToLower-faithful case classes
// (nametoken.Literal, nametoken.Alternation). The pre-optimization classifier
// lowercased the evidence and used [[:alnum:]] edges; reading word-ness off the
// RAW text through the shared class preserves those exact token boundaries
// without allocating the lowercased copy. Both rules are single-homed in
// internal/nametoken because the indexer's season/episode tokenizer and
// payload's extra markers parse the same names and had drifted from them; that
// package's doc carries the divergence and why the strings.ToLower reading won.
var (
	// reResolution matches a known resolution height with hand-built edges
	// instead of \b: Go regexp word boundaries require a non-word character
	// before the first digit, which misses compact spellings such as
	// "BD1080p" and "1920x1080p" that the live SeaDex catalogue uses. The
	// left edge rejects only a preceding digit (so "21080p" is not read as
	// 1080p) and the right edge rejects a word-rune continuation (so
	// "x1080py" stays unmatched); the height itself is captured in group 1
	// for detectResolution.
	reResolution = regexp.MustCompile(`(?:^|[^0-9])(` + nametoken.Alternation(resolutionHeights) + `)(?:$|` + nametoken.NonWordEdge + `)`)
	// reBitrate / reCRF / reRemux / reEncode match the raw evidence text in
	// place, with explicit edges instead of \b. Go regexp treats "_" as a
	// word character, so \b would miss underscore-delimited scene names such
	// as Show_CRF18_BDRemux; the shared edge treats "_" as a delimiter, and
	// the optional separator classes accept the full scene-delimiter set
	// [\s._-] between token halves — dot- and hyphen-joined spellings
	// (CRF.18, 4500-kbps, BD.Remux) are as real as the space/underscore
	// forms, and accepting them on one marker but not another made
	// classification depend on which delimiter a group happens to use. That
	// separator set is this package's own policy, NOT shared vocabulary: it
	// says which delimiters may sit INSIDE one of these markers, a different
	// question from where a token ends.
	reBitrate = regexp.MustCompile(`(?:^|` + nametoken.NonWordEdge + `)\d+[\s._-]?(?:` + nametoken.Alternation([]string{"kbps", "mbps"}) + `)(?:$|` + nametoken.NonWordEdge + `)`)
	// reCRF matches an x264/x265 CRF tag such as "crf18", "crf 20", or "crf.18".
	reCRF = regexp.MustCompile(`(?:^|` + nametoken.NonWordEdge + `)` + nametoken.Literal("crf") + `[\s._-]?\d+(?:$|` + nametoken.NonWordEdge + `)`)
	// reRemux matches a remux marker as a delimiter-bounded token ("remux",
	// "BDRemux", "BD-Remux"), never a bare substring inside a longer word.
	// "PREMUX" is included deliberately: SeaDex uses it for pre-muxed
	// releases, and token-bounding alone would lose it (no word boundary
	// between the "p" and "remux"). The inflected "-ed" forms ("remuxed
	// from the JPBD", "BD-Remuxed") count too — reEncode already accepts
	// "encoded" alongside "encode", and rejecting the same inflection on
	// the remux side silently declassified stated remuxes to unknown. The
	// PLURAL forms ("remuxes", "premuxes") count for the same reason (l-f62):
	// the notes field is prose, where the plural is the natural spelling
	// ("both remuxes are from the JPBD"), and a release whose only kind
	// evidence was a plural statement classified unknown - so its `kind`
	// attribute and report row understated it, and filters.exclude_remux did
	// not drop it. The tail is the whole-token alternation (?:ed|es), never
	// a bare optional "s": "remuxes" matches while "remuxs" stays out.
	reRemux = regexp.MustCompile(`(?:^|` + nametoken.NonWordEdge + `)(?:` + nametoken.Literal("bd") + `[\s._-]?)?(?:` + nametoken.Alternation([]string{"premux", "remux"}) + `)(?:` + nametoken.Literal("ed") + `|` + nametoken.Literal("es") + `)?(?:$|` + nametoken.NonWordEdge + `)`)
	// reEncode matches a generic encode marker ("encode", "encoded", "encodes",
	// "BDRip", "BDRips" - the BD half accepting the same optional [\s._-]
	// separator reRemux's BD prefix does, so "BD-Rip"/"BD.Rip"/"BD_Rip"/"BD Rip"
	// classify like the compact spelling) with reRemux's delimiter-bounded token
	// style, so a bare substring inside
	// a longer word ("reencoded", "encoder") is never a marker — the compact
	// "reencode" spelling stays deliberately UNMATCHED while every
	// delimiter-separated form ("re-encode", "re encode") matches through the
	// "encode" token, because admitting a bare in-word substring is what would
	// make "encoder" a marker (l-f62 raised the asymmetry; only its plural half
	// is closed here). It is the
	// weakest encoder-marker rung in kindFromEvidence — checked after the remux
	// token and the codec/CRF/bitrate markers, so it only ever moves a release
	// from unknown to encode, never off remux. Live SeaDex data motivates it:
	// many isBest encodes state "encode"/"BDRip" in their name or notes
	// without any codec, CRF, or bitrate marker and previously classified
	// unknown.
	reEncode = regexp.MustCompile(`(?:^|` + nametoken.NonWordEdge + `)(?:` + nametoken.Literal("bd") + `[\s._-]?` + nametoken.Literal("rip") + nametoken.Literal("s") + `?|` + nametoken.Alternation([]string{"encoded", "encodes", "encode"}) + `)(?:$|` + nametoken.NonWordEdge + `)`)
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
// catalogue, so no boundary is applied). The h-prefixed spellings — dotted
// and undotted — are excluded here and matched by reDottedX265/reDottedX264
// instead, which require a non-alphanumeric left boundary: without it a
// title-glued episode number ("Bleach.264.1080p", "Bleach264") contains the
// substring "h.264"/"h264" and misclassifies the release as an x264 encode.
var (
	x265TextTokens = []string{codecX265, "hevc"}
	x264TextTokens = []string{codecX264, "avc"}
	// reTextX265 / reTextX264 apply the text-token lists to raw evidence in
	// place (ToLower-faithful case classes via nametoken.Alternation, no
	// boundary — see above). The alternations derive from the token lists to
	// keep the vocabulary single-homed.
	reTextX265 = regexp.MustCompile(nametoken.Alternation(x265TextTokens))
	reTextX264 = regexp.MustCompile(nametoken.Alternation(x264TextTokens))
	// reDottedX265 / reDottedX264 require a non-word left boundary (the same
	// shared raw-text word set the marker edges use). The
	// undotted h-spellings ride the same boundary: "h264"/"h265" glued to a
	// preceding word rune is a title-glued episode number ("Bleach264"), the
	// same failure class as the dotted form, not a codec marker.
	reDottedX265 = regexp.MustCompile(`(?:^|` + nametoken.NonWordEdge + `)(?:` + nametoken.Literal("h.265") + `|` + nametoken.Literal("h265") + `)`)
	reDottedX264 = regexp.MustCompile(`(?:^|` + nametoken.NonWordEdge + `)(?:` + nametoken.Literal("h.264") + `|` + nametoken.Literal("h264") + `)`)
)

// evidence accumulates the classification signals of one text source (the
// release names, or the entry notes) one observed piece at a time, so a large
// evidence set — up to thousands of upstream-controlled file names per SeaDex
// torrent — is never materialized as a single joined and normalized string
// (which cost several simultaneous evidence-sized allocations and could OOM a
// memory-limited container on a malformed page). Each piece is matched IN
// PLACE by the ToLower-faithful, underscore-aware marker regexes (built via
// nametoken.Literal) — no
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
// matchers.
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
	kind, reason := classifyKind(&nameEv, &notesEv)
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
		TrackerType: tracker.TypeOf(in.Tracker),
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
// per-file encode marker. Each rung reads only its own evidence's
// TEXT-observed codec family (evidence.textCodec) — the MediaInfo codec is
// deliberately not kind evidence and is structurally unreachable from here
// (see Classify).
// The remux decision stays name-and-notes based (never size/bitrate
// inference), so no operator-supplied group list is needed.
func classifyKind(nameEv, notesEv *evidence) (kind Kind, reason string) {
	if kind, reason := kindFromEvidence(nameEv); kind != KindUnknown {
		return kind, reason
	}
	return kindFromEvidence(notesEv)
}

// kindFromEvidence classifies one accumulated evidence source (names or notes)
// in isolation: a delimiter-bounded remux token (reRemux) wins, then an
// encoder marker (codec, CRF tag, bitrate, or a generic encode token —
// reEncode, the weakest rung), else unknown. It returns the kind and a short
// reason for observability.
func kindFromEvidence(e *evidence) (kind Kind, reason string) {
	if e.remux {
		return KindRemux, "name/notes marker: remux"
	}
	codec := e.textCodec()
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
// it sorts below every recognized height: the headline-candidate ordering
// (compare.betterCandidate) prefers a release whose resolution parsed, and no
// caller may read 0 as "passes any threshold" - it is the minimum of the
// domain, not an unknown-is-acceptable sentinel.
func ResolutionRank(res string) int {
	r := strings.ToLower(strings.TrimSpace(res))
	if !slices.Contains(resolutionHeights, r) {
		return 0
	}
	// r is one of resolutionHeights (the gate above), every entry of which is
	// "<digits>p", so the parse cannot fail - and Atoi returns 0 with its error
	// anyway, so a check here could only re-return the value below. The
	// vocabulary's rankability is enforced at build time instead, by
	// TestResolutionVocabularySingleHome.
	n, _ := strconv.Atoi(strings.TrimSuffix(r, "p"))
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
