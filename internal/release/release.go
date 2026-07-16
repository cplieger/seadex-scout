// Package release classifies a media release from its names, notes, and
// metadata into a normalized fingerprint: release group, tracker kind,
// resolution, video codec, dual-audio, and remux-vs-encode. It is pure (it
// operates on strings, not on SeaDex or arr types) so both the SeaDex side and
// the library side can classify into one shared vocabulary and be compared.
//
// The remux-vs-encode decision is deliberately name-and-notes based, never a
// size or bitrate inference: on SeaDex a remux is stated in the release name or
// the entry notes, which is what makes name parsing reliable here. An
// unclassifiable release is Unknown, never silently dropped.
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
// for SeaDex); DualAudio is an explicit hint.
type Input struct {
	Notes      string
	Group      string
	Tracker    string
	VideoCodec string
	Names      []string
	DualAudio  bool
}

var (
	reResolution = regexp.MustCompile(`(?i)\b(2160p|1440p|1080p|720p|480p)\b`)
	reBitrate    = regexp.MustCompile(`(?i)\b\d+\s?(kbps|mbps)\b`)
	// reCRF matches an x264/x265 CRF tag such as "crf18" or "crf 20".
	reCRF = regexp.MustCompile(`(?i)\bcrf\s?\d+\b`)
	// reDualAudio matches an explicit dual-audio marker ("dual audio",
	// "dual-audio", "dualaudio") as a whole token. A bare "dual" token is
	// deliberately NOT a marker: a series title containing the word "Dual"
	// (e.g. "Dual! Parallel Trouble Adventure") is not a dual-audio release,
	// and an ordinary word such as "individual" is likewise not misread.
	reDualAudio = regexp.MustCompile(`(?i)\bdual[\s._-]*audio\b`)
	// reRemux matches a remux marker as a delimiter-bounded token ("remux",
	// "BDRemux", "BD-Remux"), never a bare substring inside a longer word.
	// "PREMUX" is included deliberately: SeaDex uses it for pre-muxed
	// releases, and token-bounding alone would lose it (no word boundary
	// between the "p" and "remux").
	reRemux = regexp.MustCompile(`(?i)\b(?:bd[\s._-]?remux|premux|remux)\b`)
)

// Canonical codec families the classifier normalizes video codecs to.
const (
	codecX265 = "x265"
	codecX264 = "x264"
)

// x265Tokens / x264Tokens are the codec markers detected in names.
// The x265 family takes precedence when input contains markers from both families.
var (
	x265Tokens = []string{codecX265, "h265", "h.265", "hevc"}
	x264Tokens = []string{codecX264, "h264", "h.264", "avc"}
)

// Classify converts raw release material into a normalized Release. It never
// errors: an unclassifiable release is KindUnknown with a recorded reason.
func Classify(in *Input) Release {
	nameText := strings.ToLower(strings.Join(in.Names, " "))
	notesText := strings.ToLower(in.Notes)
	text := nameText + " " + notesText
	codec := detectCodec(text, in.VideoCodec)
	kind, reason := classifyKind(nameText, notesText,
		detectCodec(nameText, in.VideoCodec), detectCodec(notesText, ""))

	return Release{
		Group:       groupOrNoGroup(in.Group),
		Tracker:     strings.TrimSpace(in.Tracker),
		Resolution:  reResolution.FindString(text),
		Codec:       codec,
		Kind:        kind,
		TrackerType: classifyTracker(in.Tracker),
		Reason:      reason,
		DualAudio:   in.DualAudio || reDualAudio.MatchString(text),
	}
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
// CRF tag, bitrate), else unknown. It returns the kind and a short reason for
// observability.
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
	}
	return KindUnknown, "no remux or encode marker"
}

// detectCodec returns the canonical codec ("x265"/"x264") from the MediaInfo
// video codec (preferred, authoritative) or the release text, else "".
func detectCodec(text, videoCodec string) string {
	if c := canonicalCodec(strings.ToLower(strings.TrimSpace(videoCodec))); c != "" {
		return c
	}
	if containsAny(text, x265Tokens) {
		return codecX265
	}
	if containsAny(text, x264Tokens) {
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

// IsAnimeBytesHost reports whether a lowercased URL host is the AnimeBytes
// site host or one of its dot-delimited subdomains. It is the URL-host twin
// of IsAnimeBytes (the tracker-name check), so the AB classification rule has
// one home. A DNS-root trailing dot ("animebytes.tv.", which resolves to the
// same site) is trimmed before comparing so the FQDN form cannot slip past.
func IsAnimeBytesHost(host string) bool {
	host = strings.TrimSuffix(host, ".")
	return host == "animebytes.tv" || strings.HasSuffix(host, ".animebytes.tv")
}

// NoGroup is the placeholder release group for a release that specifies none.
// SeaDex already tags some group-less releases with the literal "NOGRP", so
// falling back to it makes a group-less library file, a group-less SeaDex
// release, and a SeaDex "NOGRP" release all compare as the same group instead of
// vanishing from the comparison.
const NoGroup = "NOGRP"

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
// normalizeGroup folds onto the canonical NoGroup, so a SeaDex side or library
// side using any variant compares equal to a group-less release.
var noGroupVariants = map[string]bool{
	"nogrp": true, "nogroup": true, "no-group": true, "no_group": true, "no group": true,
}

// normalizeGroup lowercases and trims a release-group name for override and
// comparison lookups (SeaDex and arr casing differ). An empty group and every
// no-group spelling variant (NOGRP, NoGroup, no-group, ...) normalize to
// NoGroup so a missing group compares equal on both sides regardless of how it
// was spelled.
func normalizeGroup(group string) string {
	g := strings.ToLower(strings.TrimSpace(group))
	if g == "" || noGroupVariants[g] {
		return strings.ToLower(NoGroup)
	}
	return g
}

// NormalizeGroup is the exported form of the group normalizer, so the compare
// layer keys group-membership sets the same way Classify keys overrides.
func NormalizeGroup(group string) string { return normalizeGroup(group) }

// GroupsIntersect reports whether any group in a is present in b, comparing
// both sides normalized (normalizeGroup). It is the shared group-set overlap
// test the compare and audit layers key alignment on, so the "is a recommended
// group already present" decision lives in exactly one place. It operates only
// on []string, keeping release a pure, seadex-free leaf.
func GroupsIntersect(a, b []string) bool {
	set := make(map[string]struct{}, len(b))
	for _, g := range b {
		set[normalizeGroup(g)] = struct{}{}
	}
	for _, g := range a {
		if _, ok := set[normalizeGroup(g)]; ok {
			return true
		}
	}
	return false
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
