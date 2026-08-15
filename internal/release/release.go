// Package release classifies a media release from its names, notes, and
// metadata into a normalized fingerprint: release group, tracker kind,
// resolution, video codec, dual-audio, and remux-vs-encode. It is pure (it
// operates on strings, not on SeaDex or arr types) so both the SeaDex side and
// the library side can classify into one shared vocabulary and be compared.
package release

import (
	"cmp"
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

// Input is the raw material for Classify.
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
// vocabulary).
var resolutionHeights = []string{"2160p", "1440p", "1080p", "720p", "480p"}

// The marker edges below are defined against the shared release-name word
// alphabet (nametoken.NonWordEdge: the ASCII alphanumerics plus the two runes
// strings.ToLower folds onto an ASCII letter, with underscore a delimiter), and
// the marker tokens against the shared strings.ToLower-faithful case classes
// (nametoken.Literal, nametoken.Alternation).
var (
	// reResolution matches a known resolution height with hand-built edges
	// instead of \b: Go regexp word boundaries require a non-word character
	// before the first digit, which misses compact spellings such as
	// "BD1080p" and "1920x1080p" that the live SeaDex catalogue uses.
	reResolution = regexp.MustCompile(`(?:^|[^0-9])(` + nametoken.Alternation(resolutionHeights) + `)(?:$|` + nametoken.NonWordEdge + `)`)
	// reBitrate / reCRF / reRemux / reEncode match the raw evidence text in
	// place, with explicit edges instead of \b.
	reBitrate = regexp.MustCompile(`(?:^|` + nametoken.NonWordEdge + `)\d+[\s._-]?(?:` + nametoken.Alternation([]string{"kbps", "mbps"}) + `)(?:$|` + nametoken.NonWordEdge + `)`)
	// reCRF matches an x264/x265 CRF tag such as "crf18", "crf 20", or "crf.18".
	reCRF = regexp.MustCompile(`(?:^|` + nametoken.NonWordEdge + `)` + nametoken.Literal("crf") + `[\s._-]?\d+(?:$|` + nametoken.NonWordEdge + `)`)
	// reRemux matches a remux marker as a delimiter-bounded token ("remux",
	// "BDRemux", "BD-Remux"), never a bare substring inside a longer word.
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
	// make "encoder" a marker. It is the
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

// The codec vocabulary has ONE home per family, split by the matching RULE each
// spelling needs rather than by consumer - the same single-home rule
// resolutionHeights and internal/tracker's table apply, so a spelling added for
// one reader cannot silently miss the other.
var (
	x265TextTokens   = []string{codecX265, "hevc"}
	x264TextTokens   = []string{codecX264, "avc"}
	x265DottedTokens = []string{"h.265", "h265"}
	x264DottedTokens = []string{"h.264", "h264"}
	x265Tokens       = slices.Concat(x265TextTokens, x265DottedTokens)
	x264Tokens       = slices.Concat(x264TextTokens, x264DottedTokens)
	// reTextX265 / reTextX264 apply the text-token lists to raw evidence in
	// place (ToLower-faithful case classes via nametoken.Alternation, no
	// boundary - see above).
	reTextX265   = regexp.MustCompile(nametoken.Alternation(x265TextTokens))
	reTextX264   = regexp.MustCompile(nametoken.Alternation(x264TextTokens))
	reDottedX265 = regexp.MustCompile(`(?:^|` + nametoken.NonWordEdge + `)(?:` + nametoken.Alternation(x265DottedTokens) + `)`)
	reDottedX264 = regexp.MustCompile(`(?:^|` + nametoken.NonWordEdge + `)(?:` + nametoken.Alternation(x264DottedTokens) + `)`)
)

// evidence accumulates the classification signals of one text source (the
// release names, or the entry notes) one observed piece at a time, so a large
// evidence set — up to thousands of upstream-controlled file names per SeaDex
// torrent — is never materialized as a single joined and normalized string
// (which cost several simultaneous evidence-sized allocations and could OOM a
// memory-limited container on a malformed page).
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
	// only fill the gap).
	codec := cmp.Or(
		canonicalCodec(strings.ToLower(strings.TrimSpace(in.VideoCodec))),
		nameEv.textCodec(),
		notesEv.textCodec(),
	)
	kind, reason := classifyKind(&nameEv, &notesEv)
	resolution := cmp.Or(nameEv.resolution, notesEv.resolution)

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
// per-file encode marker.
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
// empty string that gets skipped.
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
// NormalizeGroup folds onto noGroupNormalized (the lowercased NoGroup), so a
// SeaDex side or library side using any variant compares equal to a
// group-less release.
var noGroupVariants = map[string]bool{
	"nogrp": true, "nogroup": true, "no-group": true, "no_group": true, "no group": true,
}

// NormalizeGroup lowercases and trims a release-group name for override and
// comparison lookups (SeaDex and arr casing differ), so the compare layer keys
// group-membership sets the same way Classify keys overrides. An empty group
// and every no-group spelling variant (NOGRP, NoGroup, no-group, ...)
// normalizes to the LOWERCASED unknown-evidence token ("nogrp", i.e.
func NormalizeGroup(group string) string {
	g := strings.ToLower(strings.TrimSpace(group))
	if g == "" || noGroupVariants[g] {
		return noGroupNormalized
	}
	return g
}

// Overlap is the three-valued outcome of comparing two release-group sets.
type Overlap int

const (
	// OverlapNone means every member on both sides is known evidence and no
	// group is shared: a proven divergence.
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
// (NormalizeGroup) so the overlap decision lives in exactly one place.
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
// in pixels; higher is better).
func ResolutionRank(res string) int {
	r := strings.ToLower(strings.TrimSpace(res))
	if !slices.Contains(resolutionHeights, r) {
		return 0
	}
	// r is one of resolutionHeights (the gate above), every entry of which is
	// "<digits>p", so the parse cannot fail - and Atoi returns 0 with its error
	// anyway, so a check here could only re-return the value below.
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
