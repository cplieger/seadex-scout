// Package release classifies a media release from its names, notes, and
// metadata into a normalized fingerprint: release group, tracker kind,
// resolution, video codec, dual-audio, and remux-vs-encode. It is pure (it
// operates on strings, not on SeaDex or arr types) so both the SeaDex side and
// the library side can classify into one shared vocabulary and be compared.
// It also owns the group vocabulary that comparison rests on: NormalizeGroup
// (the canonical spelling, with every no-group variant folded onto the NoGroup
// sentinel) and GroupsOverlap, the three-valued group-set comparison in which
// a NoGroup member is unknown evidence — it can neither prove an overlap nor
// permit a divergence proof — rather than an identity token.
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
	"strings"
)

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

var (
	// reResolution matches a known resolution height with hand-built edges
	// instead of \b: Go regexp word boundaries require a non-word character
	// before the first digit, which misses compact spellings such as
	// "BD1080p" and "1920x1080p" that the live SeaDex catalogue uses. The
	// left edge rejects only a preceding digit (so "21080p" is not read as
	// 1080p) and the right edge rejects an alphanumeric continuation (so
	// "x1080py" stays unmatched); the height itself is captured in group 1
	// for detectResolution.
	reResolution = regexp.MustCompile(`(?i)(?:^|[^0-9])(2160p|1440p|1080p|720p|480p)(?:$|[^[:alnum:]])`)
	reBitrate    = regexp.MustCompile(`(?i)\b\d+\s?(kbps|mbps)\b`)
	// reCRF matches an x264/x265 CRF tag such as "crf18" or "crf 20".
	reCRF = regexp.MustCompile(`(?i)\bcrf\s?\d+\b`)
	// reRemux matches a remux marker as a delimiter-bounded token ("remux",
	// "BDRemux", "BD-Remux"), never a bare substring inside a longer word.
	// "PREMUX" is included deliberately: SeaDex uses it for pre-muxed
	// releases, and token-bounding alone would lose it (no word boundary
	// between the "p" and "remux").
	reRemux = regexp.MustCompile(`(?i)\b(?:bd[\s._-]?remux|premux|remux)\b`)
	// reEncode matches a generic encode marker ("encode", "encoded", "BDRip")
	// with reRemux's delimiter-bounded token style, so a bare substring inside
	// a longer word ("reencoded", "encoder") is never a marker. It is the
	// weakest encoder-marker rung in kindFromText — checked after the remux
	// token and the codec/CRF/bitrate markers, so it only ever moves a release
	// from unknown to encode, never off remux. Live SeaDex data motivates it:
	// many isBest encodes state "encode"/"BDRip" in their name or notes
	// without any codec, CRF, or bitrate marker and previously classified
	// unknown.
	reEncode = regexp.MustCompile(`(?i)\b(?:bdrip|encoded|encode)\b`)
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
	reDottedX265   = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])h\.265`)
	reDottedX264   = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])h\.264`)
)

// normalizeEvidence lowercases evidence text and replaces underscore
// delimiters with spaces before the token regexes run. Go regexp treats "_"
// as a word character, so an underscore-delimited scene name such as
// Show_1080p_BDRemux would otherwise hide every marker family (resolution,
// remux/kind, CRF, bitrate) behind a missing word boundary. SeaDex/arr names
// use underscores as delimiters, never inside a marker token, so the
// replacement is safe.
func normalizeEvidence(text string) string {
	return strings.ToLower(strings.ReplaceAll(text, "_", " "))
}

// Classify converts raw release material into a normalized Release. It never
// errors: an unclassifiable release is KindUnknown with a recorded reason.
// DualAudio passes through from the structured input flag untouched — text is
// never evidence for it (see the Input and package docs).
func Classify(in *Input) Release {
	nameText := normalizeEvidence(strings.Join(in.Names, " "))
	notesText := normalizeEvidence(in.Notes)
	text := nameText + " " + notesText
	// The Codec field uses the same name-first precedence classifyKind applies:
	// per-file evidence (names + MediaInfo) wins, the entry-wide notes only
	// fill the gap, so the logged codec cannot contradict the Kind reason when
	// the notes mention an alternative encode.
	nameCodec := detectCodec(nameText, in.VideoCodec)
	notesCodec := detectCodec(notesText, "")
	codec := nameCodec
	if codec == "" {
		codec = notesCodec
	}
	kind, reason := classifyKind(nameText, notesText, nameCodec, notesCodec)

	return Release{
		Group:       groupOrNoGroup(in.Group),
		Tracker:     strings.TrimSpace(in.Tracker),
		Resolution:  detectResolution(text),
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
// -> unknown rules. The release names (plus the per-file MediaInfo codec) are
// classified first and win for this release; the entry-wide SeaDex notes only
// fill the gap when the names and MediaInfo carry no marker, so a notes-level
// remux note cannot override a contradicting per-file encode marker. The remux
// decision stays name-and-notes based (never size/bitrate inference), so no
// operator-supplied group list is needed.
func classifyKind(nameText, notesText, nameCodec, notesCodec string) (kind Kind, reason string) {
	if kind, reason := kindFromText(nameText, nameCodec); kind != KindUnknown {
		return kind, reason
	}
	return kindFromText(notesText, notesCodec)
}

// kindFromText classifies one text source (names or notes) in isolation: a
// delimiter-bounded remux token (reRemux) wins, then an encoder marker (codec,
// CRF tag, bitrate, or a generic encode token — reEncode, the weakest rung),
// else unknown. It returns the kind and a short reason for observability.
func kindFromText(text, codec string) (kind Kind, reason string) {
	if reRemux.MatchString(text) {
		return KindRemux, "name/notes marker: remux"
	}
	switch {
	case codec != "":
		return KindEncode, "encoder marker: " + codec
	case reCRF.MatchString(text):
		return KindEncode, "encoder marker: crf"
	case reBitrate.MatchString(text):
		return KindEncode, "encoder marker: bitrate"
	case reEncode.MatchString(text):
		return KindEncode, "encoder marker: encode"
	}
	return KindUnknown, "no remux or encode marker"
}

// detectCodec returns the canonical codec ("x265"/"x264") from the MediaInfo
// video codec (preferred, authoritative) or the release text, else "".
func detectCodec(text, videoCodec string) string {
	if c := canonicalCodec(strings.ToLower(strings.TrimSpace(videoCodec))); c != "" {
		return c
	}
	if containsAny(text, x265TextTokens) || reDottedX265.MatchString(text) {
		return codecX265
	}
	if containsAny(text, x264TextTokens) || reDottedX264.MatchString(text) {
		return codecX264
	}
	return ""
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

// ResolutionRank returns a comparable rank for a resolution string (its height
// in pixels; higher is better). An empty or unrecognized resolution ranks 0, so
// a resolution floor never drops a release whose resolution could not be
// parsed.
func ResolutionRank(res string) int {
	switch strings.ToLower(strings.TrimSpace(res)) {
	case "2160p":
		return 2160
	case "1440p":
		return 1440
	case "1080p":
		return 1080
	case "720p":
		return 720
	case "480p":
		return 480
	default:
		return 0
	}
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
