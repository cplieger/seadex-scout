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
	// TrackerPrivate is a private tracker requiring membership (AnimeBytes,
	// BeyondHD, ...).
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
// for SeaDex); DualAudio is an explicit hint; RemuxGroups pins groups the
// operator knows to be remux.
type Input struct {
	RemuxGroups map[string]bool
	Notes       string
	Group       string
	Tracker     string
	VideoCodec  string
	Names       []string
	DualAudio   bool
}

var (
	reResolution = regexp.MustCompile(`(?i)\b(2160p|1440p|1080p|720p|480p)\b`)
	reBitrate    = regexp.MustCompile(`(?i)\b\d+\s?(kbps|mbps)\b`)
	// reCRF matches an x264/x265 CRF tag such as "crf18" or "crf 20".
	reCRF = regexp.MustCompile(`(?i)\bcrf\s?\d+\b`)
)

// Canonical codec families the classifier normalizes video codecs to.
const (
	codecX265 = "x265"
	codecX264 = "x264"
)

// x265Tokens / x264Tokens are the codec markers detected in names.
var (
	x265Tokens = []string{codecX265, "h265", "h.265", "hevc"}
	x264Tokens = []string{codecX264, "h264", "h.264", "avc"}
)

// publicTrackers / privateTrackers classify a tracker name (lowercased).
var (
	publicTrackers  = map[string]bool{"nyaa": true, "animetosho": true, "rutracker": true}
	privateTrackers = map[string]bool{
		"ab": true, "animebytes": true, "beyondhd": true, "bhd": true,
		"passthepopcorn": true, "ptp": true, "broadcasthenet": true, "btn": true,
		"hdbits": true, "blutopia": true, "aither": true,
	}
)

// Classify converts raw release material into a normalized Release. It never
// errors: an unclassifiable release is KindUnknown with a recorded reason.
func Classify(in *Input) Release {
	text := strings.ToLower(strings.Join(in.Names, " ") + " " + in.Notes)
	codec := detectCodec(text, in.VideoCodec)
	kind, reason := classifyKind(text, in.Group, in.RemuxGroups, codec)

	return Release{
		Group:       strings.TrimSpace(in.Group),
		Tracker:     strings.TrimSpace(in.Tracker),
		Resolution:  reResolution.FindString(text),
		Codec:       codec,
		Kind:        kind,
		TrackerType: classifyTracker(in.Tracker),
		Reason:      reason,
		DualAudio:   in.DualAudio || strings.Contains(text, "dual"),
	}
}

// classifyKind applies the ordered remux -> encode -> unknown rules and returns
// the kind and a short reason for observability.
func classifyKind(text, group string, remuxGroups map[string]bool, codec string) (kind Kind, reason string) {
	if strings.Contains(text, "remux") {
		return KindRemux, "name/notes marker: remux"
	}
	if remuxGroups[normalizeGroup(group)] {
		return KindRemux, "group pinned as remux"
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

// classifyTracker maps a tracker name to its obtainability class.
func classifyTracker(tracker string) TrackerType {
	t := strings.ToLower(strings.TrimSpace(tracker))
	switch {
	case t == "":
		return TrackerUnknown
	case publicTrackers[t]:
		return TrackerPublic
	case privateTrackers[t]:
		return TrackerPrivate
	default:
		return TrackerUnknown
	}
}

// animeBytesNames are the SeaDex tracker strings (lowercased) for AnimeBytes;
// SeaDex uses "AB", but "animebytes" is accepted defensively.
var animeBytesNames = map[string]bool{"ab": true, "animebytes": true}

// IsAnimeBytes reports whether the tracker name is AnimeBytes. It is the one
// private tracker seadex-scout gates behind an opt-in toggle (the operator has
// an account), so obtainability can single it out from other private trackers.
func IsAnimeBytes(tracker string) bool {
	return animeBytesNames[strings.ToLower(strings.TrimSpace(tracker))]
}

// normalizeGroup lowercases and trims a release-group name for override and
// comparison lookups (SeaDex and arr casing differ).
func normalizeGroup(group string) string {
	return strings.ToLower(strings.TrimSpace(group))
}

// NormalizeGroup is the exported form of the group normalizer, so the compare
// layer keys group-membership sets the same way Classify keys overrides.
func NormalizeGroup(group string) string { return normalizeGroup(group) }

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
