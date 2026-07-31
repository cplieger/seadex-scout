package release

import (
	"strings"
	"testing"

	"github.com/cplieger/seadex-scout/internal/tracker"
)

// TestGroupNoGroupFallback covers the NoGroup fallback at the classification
// layer: a release with no group classifies and normalizes to the NoGroup
// sentinel on both sides, so a group-less library file and a group-less SeaDex
// release (or SeaDex's own literal "NOGRP") serialize as the same first-class
// token rather than being skipped. What the sentinel MEANS is the decision
// layer's business: GroupsOverlap treats it as unknown evidence, never as an
// identity (see TestGroupsOverlap).
func TestGroupNoGroupFallback(t *testing.T) {
	if got := Classify(&Input{Group: ""}).Group; got != NoGroup {
		t.Errorf("Classify empty group = %q, want %q", got, NoGroup)
	}
	if got := Classify(&Input{Group: "   "}).Group; got != NoGroup {
		t.Errorf("Classify blank group = %q, want %q", got, NoGroup)
	}
	if got := Classify(&Input{Group: "SubsPlease"}).Group; got != "SubsPlease" {
		t.Errorf("Classify must keep a real group, got %q", got)
	}
	if NormalizeGroup("") != NormalizeGroup(NoGroup) {
		t.Errorf("NormalizeGroup(empty)=%q must equal NormalizeGroup(NoGroup)=%q",
			NormalizeGroup(""), NormalizeGroup(NoGroup))
	}
	// A group-less library value and a group-less SeaDex value must match.
	if NormalizeGroup("") != NormalizeGroup(Classify(&Input{Group: ""}).Group) {
		t.Error("group-less library and SeaDex releases must normalize equal")
	}
}

// TestClassifyDualAudioStructuredOnly pins the dual-audio sourcing contract:
// the structured Input.DualAudio flag is the only evidence, passed through
// regardless of what the text says. A "dual audio" mention in the names or
// the entry-wide notes never sets it — SeaDex notes describe every release in
// the entry and can even negate the marker ("lacks dual audio"), so text is
// unreliable per-release evidence.
func TestClassifyDualAudioStructuredOnly(t *testing.T) {
	tests := []struct {
		name string
		in   Input
		want bool
	}{
		{name: "structured flag with no text", in: Input{DualAudio: true}, want: true},
		{name: "structured flag wins over negating notes", in: Input{DualAudio: true, Notes: "lacks dual audio"}, want: true},
		{name: "name tag alone is not evidence", in: Input{Names: []string{"Show [Dual Audio]"}}, want: false},
		{name: "hyphenated name tag alone is not evidence", in: Input{Names: []string{"Show [Dual-Audio] 1080p"}}, want: false},
		{name: "notes mention alone is not evidence", in: Input{Notes: "dualaudio release"}, want: false},
		{name: "negated notes mention is not evidence", in: Input{Notes: "lacks dual audio"}, want: false},
		{name: "underscore-delimited name tag is not evidence", in: Input{Names: []string{"Show_1080p_Dual_Audio"}}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(&tt.in).DualAudio; got != tt.want {
				t.Errorf("DualAudio = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestGroupsOverlap pins the three-valued group-set comparison's contract:
// a known group shared between the sides (case/whitespace-insensitively) is
// proven overlap; all-known disjoint sets are proven divergence; an unknown
// member (the NoGroup sentinel or any of its spelling variants, including the
// empty string) makes an otherwise matchless comparison indeterminate rather
// than proving anything - sentinel∩sentinel is Unknown, never Known; a
// known-known match wins outright even with unknown members alongside; and an
// empty side is always None (nothing can overlap with an empty set, and an
// unknown member cannot hide a match against one).
func TestGroupsOverlap(t *testing.T) {
	tests := []struct {
		name string
		a    []string
		b    []string
		want Overlap
	}{
		{name: "case and whitespace insensitive known match", a: []string{" SubsPlease "}, b: []string{"subsplease"}, want: OverlapKnown},
		{name: "disjoint known groups are proven divergence", a: []string{"PMR"}, b: []string{"LostYears"}, want: OverlapNone},
		{name: "sentinel on both sides is unknown, not a match", a: []string{""}, b: []string{NoGroup}, want: OverlapUnknown},
		{name: "no-group spelling variants are unknown, not a match", a: []string{"no-group"}, b: []string{"nogroup"}, want: OverlapUnknown},
		{name: "unknown library side against a known set is unknown", a: []string{NoGroup}, b: []string{"LostYears"}, want: OverlapUnknown},
		{name: "known library side against an unknown set is unknown", a: []string{"SubsPlease"}, b: []string{NoGroup}, want: OverlapUnknown},
		{name: "unknown member beside a known miss is unknown", a: []string{"SubsPlease", NoGroup}, b: []string{"LostYears"}, want: OverlapUnknown},
		{name: "known-known match wins over unknown members", a: []string{NoGroup, "PMR"}, b: []string{"LostYears", " pmr "}, want: OverlapKnown},
		{name: "empty side never overlaps", a: nil, b: []string{"PMR"}, want: OverlapNone},
		{name: "unknown member against an empty side is none", a: []string{NoGroup}, b: nil, want: OverlapNone},
		{name: "both sides empty is none", a: nil, b: nil, want: OverlapNone},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := GroupsOverlap(tc.a, tc.b); got != tc.want {
				t.Errorf("GroupsOverlap(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestClassifyKind covers the per-file-evidence-first remux -> encode ->
// unknown classification in classifyKind: within one text source a
// delimiter-bounded remux token (remux/BDRemux/BD-Remux/PREMUX) wins, then an
// encoder marker (codec token, CRF, bitrate, or a generic encode token —
// encode/encoded/BDRip, the weakest rung); the release names win for the
// file and the entry-wide notes only fill the gap when the names carry no
// marker, so a notes remux cannot override a per-file encode marker. The
// generic encode tokens are delimiter-bounded like the remux tokens, so a
// bare substring inside a longer word (reencode/reencoded/encoder) is never
// a marker. These are the branches the daemon and report both key alignment
// on.
func TestClassifyKind(t *testing.T) {
	tests := []struct {
		name       string
		wantKind   Kind
		wantReason string
		in         Input
	}{
		{name: "remux from name", in: Input{Names: []string{"Show 1080p BDRemux"}}, wantKind: KindRemux, wantReason: "name/notes marker: remux"},
		{name: "remux from hyphenated bd-remux", in: Input{Names: []string{"Show 1080p BD-Remux"}}, wantKind: KindRemux, wantReason: "name/notes marker: remux"},
		{name: "remux from premux marker", in: Input{Names: []string{"Show S01 PREMUX 1080p"}}, wantKind: KindRemux, wantReason: "name/notes marker: remux"},
		{name: "remux from notes", in: Input{Notes: "best remux available"}, wantKind: KindRemux, wantReason: "name/notes marker: remux"},
		{name: "remuxed inflection from notes", in: Input{Notes: "remuxed from the JPBD"}, wantKind: KindRemux, wantReason: "name/notes marker: remux"},
		{name: "bd-remuxed inflection from name", in: Input{Names: []string{"Show 1080p BD-Remuxed"}}, wantKind: KindRemux, wantReason: "name/notes marker: remux"},
		{name: "remux inside a longer word is not a marker", in: Input{Names: []string{"Show DreamRemuxer 1080p"}}, wantKind: KindUnknown, wantReason: "no remux or encode marker"},
		{name: "per-file encode marker wins over notes remux", in: Input{Names: []string{"Show 1080p x265"}, Notes: "grab the remux"}, wantKind: KindEncode, wantReason: "encoder marker: x265"},
		{name: "notes fill the gap when the name carries no marker", in: Input{Names: []string{"Show 1080p"}, Notes: "crf 18 encode"}, wantKind: KindEncode, wantReason: "encoder marker: crf"},
		{name: "encode from codec token", in: Input{Names: []string{"Show 1080p x265"}}, wantKind: KindEncode, wantReason: "encoder marker: x265"},
		{name: "encode from crf", in: Input{Names: []string{"Show CRF18"}}, wantKind: KindEncode, wantReason: "encoder marker: crf"},
		{name: "encode from dot-separated crf", in: Input{Names: []string{"Show.CRF.18.720p"}}, wantKind: KindEncode, wantReason: "encoder marker: crf"},
		{name: "encode from bitrate", in: Input{Names: []string{"Show 4500 kbps"}}, wantKind: KindEncode, wantReason: "encoder marker: bitrate"},
		{name: "encode from hyphen-joined bitrate", in: Input{Names: []string{"Show 4500-kbps"}}, wantKind: KindEncode, wantReason: "encoder marker: bitrate"},
		{name: "encode from dot-joined bitrate", in: Input{Names: []string{"Show.4500.kbps"}}, wantKind: KindEncode, wantReason: "encoder marker: bitrate"},
		{name: "encode from mbps bitrate", in: Input{Names: []string{"Show 12 mbps"}}, wantKind: KindEncode, wantReason: "encoder marker: bitrate"},
		{name: "encode from compact bitrate without separator", in: Input{Names: []string{"Show 4500kbps"}}, wantKind: KindEncode, wantReason: "encoder marker: bitrate"},
		{name: "mediainfo codec alone is not encode evidence", in: Input{Names: []string{"Show S01E01"}, VideoCodec: "AVC"}, wantKind: KindUnknown, wantReason: "no remux or encode marker"},
		{name: "mediainfo codec does not shadow a written remux marker", in: Input{Names: []string{"Show 1080p Remux"}, VideoCodec: "HEVC"}, wantKind: KindRemux, wantReason: "name/notes marker: remux"},
		{name: "written codec token stays encode evidence with mediainfo present", in: Input{Names: []string{"Show 1080p x264"}, VideoCodec: "HEVC"}, wantKind: KindEncode, wantReason: "encoder marker: x264"},
		{name: "notes codec token fills the kind gap", in: Input{Names: []string{"Show 1080p"}, Notes: "x265"}, wantKind: KindEncode, wantReason: "encoder marker: x265"},
		{name: "encode from generic encode token", in: Input{Names: []string{"Show S01 1080p encode"}}, wantKind: KindEncode, wantReason: "encoder marker: encode"},
		{name: "encode from encoded token", in: Input{Names: []string{"Show 1080p [Encoded]"}}, wantKind: KindEncode, wantReason: "encoder marker: encode"},
		{name: "encode from bdrip token", in: Input{Names: []string{"Show BDRip 1080p"}}, wantKind: KindEncode, wantReason: "encoder marker: encode"},
		{name: "encode from hyphenated bd-rip", in: Input{Names: []string{"Show BD-Rip 1080p"}}, wantKind: KindEncode, wantReason: "encoder marker: encode"},
		{name: "encode from dot-joined bd.rip", in: Input{Names: []string{"Show.BD.Rip.1080p"}}, wantKind: KindEncode, wantReason: "encoder marker: encode"},
		{name: "encode from underscore-joined bd_rip", in: Input{Names: []string{"Show_BD_Rip"}}, wantKind: KindEncode, wantReason: "encoder marker: encode"},
		{name: "encode from space-separated bd rip", in: Input{Names: []string{"Show BD Rip 1080p"}}, wantKind: KindEncode, wantReason: "encoder marker: encode"},
		{name: "bd ripper is not an encode marker", in: Input{Names: []string{"Show BD Ripper 1080p"}}, wantKind: KindUnknown, wantReason: "no remux or encode marker"},
		{name: "bd ripped is not an encode marker", in: Input{Names: []string{"Show BDRipped"}}, wantKind: KindUnknown, wantReason: "no remux or encode marker"},
		{name: "notes fill the gap with a generic encode token", in: Input{Names: []string{"Show 1080p"}, Notes: "a solid encode"}, wantKind: KindEncode, wantReason: "encoder marker: encode"},
		{name: "remux token wins over a generic encode token", in: Input{Names: []string{"Show BD-Remux encode 1080p"}}, wantKind: KindRemux, wantReason: "name/notes marker: remux"},
		{name: "reencode is not an encode marker", in: Input{Names: []string{"Show reencode 1080p"}}, wantKind: KindUnknown, wantReason: "no remux or encode marker"},
		{name: "reencoded is not an encode marker", in: Input{Names: []string{"Show reencoded 1080p"}}, wantKind: KindUnknown, wantReason: "no remux or encode marker"},
		// Plural forms count: the notes field is prose, where the plural is the
		// natural spelling, and a release whose only kind evidence was a plural
		// statement used to classify unknown - understating the `kind` attribute
		// and the report row, and letting filters.exclude_remux pass a stated
		// remux through (l-f62).
		{name: "plural remuxes in notes is a remux marker", in: Input{Names: []string{"Show 1080p"}, Notes: "both remuxes are from the JPBD"}, wantKind: KindRemux, wantReason: "name/notes marker: remux"},
		{name: "plural premuxes is a remux marker", in: Input{Names: []string{"Show S01 PREMUXES"}}, wantKind: KindRemux, wantReason: "name/notes marker: remux"},
		{name: "plural bd remuxes is a remux marker", in: Input{Names: []string{"Show BD-Remuxes 1080p"}}, wantKind: KindRemux, wantReason: "name/notes marker: remux"},
		{name: "plural encodes in notes is an encode marker", in: Input{Names: []string{"Show 1080p"}, Notes: "the encodes are fine"}, wantKind: KindEncode, wantReason: "encoder marker: encode"},
		{name: "plural bdrips is an encode marker", in: Input{Names: []string{"Show BDRips 1080p"}}, wantKind: KindEncode, wantReason: "encoder marker: encode"},
		// The plural tail must not admit a bare in-word substring: these stay
		// unmatched exactly as the singular forms above do.
		{name: "remuxs is not a remux marker", in: Input{Names: []string{"Show remuxs 1080p"}}, wantKind: KindUnknown, wantReason: "no remux or encode marker"},
		{name: "encodesomething is not an encode marker", in: Input{Names: []string{"Show encodesomething 1080p"}}, wantKind: KindUnknown, wantReason: "no remux or encode marker"},
		{name: "bdripsomething is not an encode marker", in: Input{Names: []string{"Show BDRipsomething 1080p"}}, wantKind: KindUnknown, wantReason: "no remux or encode marker"},
		{name: "encoder is not an encode marker", in: Input{Names: []string{"Show 1080p encoder notes"}}, wantKind: KindUnknown, wantReason: "no remux or encode marker"},
		{name: "underscore-delimited bdremux", in: Input{Names: []string{"Show_1080p_BDRemux"}}, wantKind: KindRemux, wantReason: "name/notes marker: remux"},
		{name: "underscore-delimited premux", in: Input{Names: []string{"Show_S01_PREMUX"}}, wantKind: KindRemux, wantReason: "name/notes marker: remux"},
		{name: "underscore-delimited crf", in: Input{Names: []string{"Show_CRF18"}}, wantKind: KindEncode, wantReason: "encoder marker: crf"},
		{name: "underscore-delimited bitrate", in: Input{Names: []string{"Show_4500_kbps"}}, wantKind: KindEncode, wantReason: "encoder marker: bitrate"},
		{name: "underscore-delimited bdrip", in: Input{Names: []string{"Show_1080p_BDRip"}}, wantKind: KindEncode, wantReason: "encoder marker: encode"},
		// The Unicode rows pin nametoken.Literal's ToLower-faithful folding
		// against regexp's SimpleFold: U+0130 (İ) lowercases to ASCII i (a
		// word rune SimpleFold misses), U+212A (KELVIN SIGN) lowercases to k,
		// and U+017F (ſ) is a delimiter (ToLower never folds it onto s,
		// though SimpleFold does).
		{name: "U+0130 joins the digits to the word and blocks the bitrate edge", in: Input{Names: []string{"Show\u01304500 kbps"}}, wantKind: KindUnknown, wantReason: "no remux or encode marker"},
		{name: "U+017F is not an s in the bitrate suffix", in: Input{Names: []string{"Show 4500 kbp\u017f"}}, wantKind: KindUnknown, wantReason: "no remux or encode marker"},
		{name: "U+017F is a delimiter before the bitrate", in: Input{Names: []string{"Show\u017f4500 kbps"}}, wantKind: KindEncode, wantReason: "encoder marker: bitrate"},
		{name: "U+0130 folds onto i in the bdrip token", in: Input{Names: []string{"Show BDR\u0130P"}}, wantKind: KindEncode, wantReason: "encoder marker: encode"},
		{name: "kelvin sign folds onto k in the bitrate suffix", in: Input{Names: []string{"Show 4500 \u212abps"}}, wantKind: KindEncode, wantReason: "encoder marker: bitrate"},
		{name: "unknown when no marker", in: Input{Names: []string{"Show 1080p"}}, wantKind: KindUnknown, wantReason: "no remux or encode marker"},
		{name: "title-glued undotted episode number is not an encoder marker", in: Input{Names: []string{"Bleach264.mkv"}}, wantKind: KindUnknown, wantReason: "no remux or encode marker"},
		{name: "title-glued undotted h265 episode number is not an encoder marker", in: Input{Names: []string{"Bleach265.mkv"}}, wantKind: KindUnknown, wantReason: "no remux or encode marker"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(&tc.in)
			if got.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", got.Kind, tc.wantKind)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tc.wantReason)
			}
		})
	}
}

// TestClassifyCodec covers codec detection (evidence.textCodec/canonicalCodec):
// x265/HEVC and x264/AVC
// tokens normalize to the canonical family, the authoritative MediaInfo codec
// wins over a conflicting name token, a name marker wins over a conflicting
// entry-wide notes marker (matching the Kind reason's precedence), the notes
// fill the gap when the name carries no marker, and an absent marker yields "".
func TestClassifyCodec(t *testing.T) {
	tests := []struct {
		name string
		want string
		in   Input
	}{
		{name: "x265 from name", in: Input{Names: []string{"Show 1080p x265"}}, want: "x265"},
		{name: "hevc token maps to x265", in: Input{Names: []string{"Show HEVC"}}, want: "x265"},
		{name: "x264 from name", in: Input{Names: []string{"Show 720p x264"}}, want: "x264"},
		{name: "avc token maps to x264", in: Input{Names: []string{"Show AVC"}}, want: "x264"},
		{name: "bare h265 token maps to x265", in: Input{Names: []string{"Show 1080p h265"}}, want: "x265"},
		{name: "bare h264 token maps to x264", in: Input{Names: []string{"Show 720p h264"}}, want: "x264"},
		{name: "x265 family wins when both families are present", in: Input{Names: []string{"Show x264 x265"}}, want: "x265"},
		{name: "mediainfo codec wins over name", in: Input{Names: []string{"Show x264"}, VideoCodec: "HEVC"}, want: "x265"},
		{name: "name marker wins over conflicting notes", in: Input{Names: []string{"Show 1080p x264"}, Notes: "an x265 encode also exists"}, want: "x264"},
		{name: "notes fill the gap when name has no marker", in: Input{Names: []string{"Show 1080p"}, Notes: "x265 encode"}, want: "x265"},
		{name: "no codec marker", in: Input{Names: []string{"Show 1080p"}}, want: ""},
		{name: "dotted episode number is not a codec marker", in: Input{Names: []string{"One.Punch.264.1080p"}}, want: ""},
		{name: "title-glued undotted episode number is not a codec marker", in: Input{Names: []string{"Bleach264.mkv"}}, want: ""},
		{name: "title-glued undotted h265 episode number is not a codec marker", in: Input{Names: []string{"Bleach265.mkv"}}, want: ""},
		{name: "dotted h.265 token from name", in: Input{Names: []string{"Show 1080p H.265"}}, want: "x265"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(&tc.in).Codec; got != tc.want {
				t.Errorf("Codec = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClassifyTrackerType covers the obtainability class Classify reads from
// the canonical tracker vocabulary (tracker.TypeOf): public/private/unknown
// mapping, including case/whitespace insensitivity and the empty-string miss.
// The vocabulary's own contract (aliases, the AnimeBytes predicate, the host
// gates) is pinned in internal/tracker; this test pins the wiring.
func TestClassifyTrackerType(t *testing.T) {
	trackerTests := []struct {
		label string
		want  tracker.Type
	}{
		{label: "Nyaa", want: tracker.Public},
		{label: "AnimeTosho", want: tracker.Public},
		{label: "RuTracker", want: tracker.Public},
		{label: "AB", want: tracker.Private},
		{label: "AnimeBytes", want: tracker.Private},
		{label: "  ", want: tracker.Unknown},
		{label: "SomeRandomTracker", want: tracker.Unknown},
		{label: "beyondhd", want: tracker.Unknown},
		{label: "ptp", want: tracker.Unknown},
	}
	for _, tc := range trackerTests {
		if got := Classify(&Input{Tracker: tc.label}).TrackerType; got != tc.want {
			t.Errorf("TrackerType(%q) = %q, want %q", tc.label, got, tc.want)
		}
	}
}

// TestResolutionRank pins the resolution-floor comparator: each known height
// ranks by pixels, casing/whitespace is normalized, and an empty or
// unrecognized resolution ranks 0 so a floor never drops an unparsed release.
func TestResolutionRank(t *testing.T) {
	tests := []struct {
		res  string
		want int
	}{
		{res: "2160p", want: 2160},
		{res: "1440p", want: 1440},
		{res: "1080p", want: 1080},
		{res: "720p", want: 720},
		{res: "480p", want: 480},
		{res: " 1080P ", want: 1080},
		{res: "", want: 0},
		{res: "999p", want: 0},
	}
	for _, tc := range tests {
		if got := ResolutionRank(tc.res); got != tc.want {
			t.Errorf("ResolutionRank(%q) = %d, want %d", tc.res, got, tc.want)
		}
	}
}

// TestClassifyUnrecognizedVideoCodecFallsBackToText pins canonicalCodec's
// default arm: a non-empty MediaInfo codec outside the known x264/x265 families
// (AV1, VP9) must not short-circuit codec detection — the classifier falls back
// to the name/notes text, and with no text marker either the codec stays empty
// (KindUnknown, never a wrong guess).
func TestClassifyUnrecognizedVideoCodecFallsBackToText(t *testing.T) {
	got := Classify(&Input{Names: []string{"Show 1080p x265"}, VideoCodec: "AV1"})
	if got.Codec != "x265" {
		t.Errorf("Codec = %q, want x265 (unrecognized MediaInfo codec must fall back to name detection)", got.Codec)
	}

	got = Classify(&Input{Names: []string{"Show 1080p"}, VideoCodec: "VP9"})
	if got.Codec != "" {
		t.Errorf("Codec = %q, want empty for an unrecognized codec with no name marker", got.Codec)
	}
	if got.Kind != KindUnknown {
		t.Errorf("Kind = %q, want %q when no codec or remux marker is present", got.Kind, KindUnknown)
	}
}

// TestNormalizeGroupFoldsNoGroupVariants pins the no-group spelling fold: every
// documented variant (NOGRP, NoGroup, no-group, no_group, "no group", any
// casing) normalizes to the same value as the canonical NoGroup, so a SeaDex
// side and a library side spelling "no group" differently still compare equal;
// a real group is only lowercased and trimmed.
func TestNormalizeGroupFoldsNoGroupVariants(t *testing.T) {
	want := NormalizeGroup(NoGroup)
	variants := []string{
		"NOGRP", "nogrp", "NoGroup", "nogroup", "NOGROUP",
		"no-group", "No-Group", "no_group", "NO_GROUP", "no group", "No Group",
		" NOGRP ", "",
	}
	for _, v := range variants {
		if got := NormalizeGroup(v); got != want {
			t.Errorf("NormalizeGroup(%q) = %q, want %q", v, got, want)
		}
	}
	if got := NormalizeGroup(" SubsPlease "); got != "subsplease" {
		t.Errorf("NormalizeGroup(SubsPlease) = %q, want subsplease (real groups only fold case/space)", got)
	}
}

// TestClassifyResolution pins the Resolution extraction Classify performs via
// reResolution: each known height is extracted from the release name, the
// value is normalized to lowercase (the match runs on lowered text), the notes
// fill the gap when the names carry no resolution, the names win when both
// carry one (first match in name-then-notes order), and a release with no
// marker or only an unbounded substring yields "".
func TestClassifyResolution(t *testing.T) {
	tests := []struct {
		name string
		want string
		in   Input
	}{
		{name: "2160p from name", in: Input{Names: []string{"Show 2160p HEVC"}}, want: "2160p"},
		{name: "1440p from name", in: Input{Names: []string{"Show 1440p"}}, want: "1440p"},
		{name: "1080p from name", in: Input{Names: []string{"Show 1080p"}}, want: "1080p"},
		{name: "720p from name", in: Input{Names: []string{"Show 720p"}}, want: "720p"},
		{name: "480p from name", in: Input{Names: []string{"Show 480p"}}, want: "480p"},
		{name: "uppercase input normalizes to lowercase", in: Input{Names: []string{"Show 1080P"}}, want: "1080p"},
		{name: "notes fill the gap", in: Input{Names: []string{"Show"}, Notes: "720p encode"}, want: "720p"},
		{name: "name wins over notes", in: Input{Names: []string{"Show 1080p"}, Notes: "the 720p is better"}, want: "1080p"},
		{name: "no resolution marker", in: Input{Names: []string{"Show HEVC"}}, want: ""},
		{name: "resolution inside a longer token is not a marker", in: Input{Names: []string{"Show x1080py"}}, want: ""},
		{name: "underscore-delimited resolution", in: Input{Names: []string{"Show_1080p_x265"}}, want: "1080p"},
		{name: "bracketed BD_1080p tag", in: Input{Names: []string{"Show [BD_1080p]"}}, want: "1080p"},
		{name: "compact BD1080p spelling", in: Input{Names: []string{"Show [BD1080p]"}}, want: "1080p"},
		{name: "dimension form 1920x1080p", in: Input{Names: []string{"Show 1920x1080p"}}, want: "1080p"},
		{name: "preceding digit is not a resolution boundary", in: Input{Names: []string{"Show 21080p"}}, want: ""},
		// U+0130 lowercases to ASCII i (a word rune: it blocks the right
		// edge like any letter), while U+017F never folds onto an ASCII
		// alphanumeric under ToLower, so it delimits the height.
		{name: "U+0130 word rune blocks the right edge", in: Input{Names: []string{"Show 1080p\u0130"}}, want: ""},
		{name: "U+017F delimits the height", in: Input{Names: []string{"Show 1080p\u017f"}}, want: "1080p"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(&tc.in).Resolution; got != tc.want {
				t.Errorf("Resolution = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClassifyUnderscoreDelimitedName pins the underscore-normalization fix
// across the full fingerprint at once: a raw scene name delimited entirely by
// underscores (the shape Go regexp word boundaries cannot tokenize) must
// yield the same Release an equivalent space-delimited name does — resolution,
// kind, and reason together, not merely no-panic behavior. The Dual_Audio name
// tag stays inert: dual-audio is sourced only from the structured input flag,
// never from text.
func TestClassifyUnderscoreDelimitedName(t *testing.T) {
	got := Classify(&Input{Names: []string{"Show_1080p_BDRemux_Dual_Audio"}})
	if got.Resolution != "1080p" {
		t.Errorf("Resolution = %q, want 1080p", got.Resolution)
	}
	if got.Kind != KindRemux {
		t.Errorf("Kind = %q, want %q", got.Kind, KindRemux)
	}
	if got.Reason != "name/notes marker: remux" {
		t.Errorf("Reason = %q, want name/notes marker: remux", got.Reason)
	}
	if got.DualAudio {
		t.Error("DualAudio = true, want false: a name tag is not structured dual-audio evidence")
	}
}

// TestClassifyLargeUppercaseUnderscoreEvidence pins the in-place matching
// contract: a large, entirely-uppercase, underscore-heavy evidence value (the
// worst case for the removed lowercase+underscore normalization, which
// allocated two evidence-sized copies per piece) still classifies correctly —
// the case-insensitive, underscore-aware regexes must find every marker
// family in the raw text.
func TestClassifyLargeUppercaseUnderscoreEvidence(t *testing.T) {
	name := strings.Repeat("PADDING_", 1<<16) + "SHOW_1080P_BD_REMUX_X265_CRF_18_4500_KBPS_BDRIP"
	got := Classify(&Input{Names: []string{name}})
	if got.Resolution != "1080p" {
		t.Errorf("Resolution = %q, want 1080p", got.Resolution)
	}
	if got.Kind != KindRemux {
		t.Errorf("Kind = %q, want %q", got.Kind, KindRemux)
	}
	if got.Codec != "x265" {
		t.Errorf("Codec = %q, want x265", got.Codec)
	}
}

// TestClassifyTrackerPassthrough pins the Tracker field's passthrough
// contract in Classify: the raw source tracker label is whitespace-trimmed
// but otherwise preserved verbatim (casing and unknown names included), since
// the field is serialized into findings and log attributes as the source
// spelled it while classification happens separately via TrackerType.
func TestClassifyTrackerPassthrough(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: " Nyaa ", want: "Nyaa"},
		{in: "AB", want: "AB"},
		{in: "  ", want: ""},
		{in: "SomeRandomTracker", want: "SomeRandomTracker"},
	}
	for _, tc := range tests {
		if got := Classify(&Input{Tracker: tc.in}).Tracker; got != tc.want {
			t.Errorf("Classify(Tracker: %q).Tracker = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestClassifyMultiNameEvidence pins Classify's whole-Names-list evidence
// aggregation: markers carried only by a LATER element of the Names slice
// (the shape classify.TorrentFileNames and library's sceneName+relPath pair
// produce) must still classify - resolution, kind, and codec together - so a
// regression to reading only Names[0] cannot survive. The existing tables all
// pass a single name, and classify's own multi-file test carries its marker
// in the first element, so only this test covers the later-element direction.
func TestClassifyMultiNameEvidence(t *testing.T) {
	got := Classify(&Input{Names: []string{"Show S01E01", "Show S01E02 1080p BDRemux x265"}})
	if got.Resolution != "1080p" {
		t.Errorf("Resolution = %q, want 1080p from a later Names element", got.Resolution)
	}
	if got.Kind != KindRemux {
		t.Errorf("Kind = %q, want %q from a later Names element", got.Kind, KindRemux)
	}
	if got.Codec != "x265" {
		t.Errorf("Codec = %q, want x265 from a later Names element", got.Codec)
	}
}

// TestClassifyEarlierNameEvidenceRetained pins the evidence accumulator's
// retention contract: markers observed in an EARLIER Names element survive
// marker-less later elements (each observe call ORs into the accumulated
// flags, never overwrites them). TestClassifyMultiNameEvidence covers only
// the later-element pickup direction, so a regression that re-evaluates each
// flag per element (last element wins) passes every existing test; this
// covers the retention direction.
func TestClassifyEarlierNameEvidenceRetained(t *testing.T) {
	got := Classify(&Input{Names: []string{"Show S01E01 1080p BDRemux x265", "Show S01E02", "Show S01E03"}})
	if got.Resolution != "1080p" {
		t.Errorf("Resolution = %q, want 1080p retained from the first Names element", got.Resolution)
	}
	if got.Kind != KindRemux {
		t.Errorf("Kind = %q, want %q retained from the first Names element", got.Kind, KindRemux)
	}
	if got.Codec != "x265" {
		t.Errorf("Codec = %q, want x265 retained from the first Names element", got.Codec)
	}
}

// TestClassifyFirstResolutionWinsAcrossNames pins the documented
// first-resolution-in-observation-order contract (see the evidence struct
// doc): when two Names elements carry different resolutions, the first
// observed one wins - observe only fills an empty resolution, never
// overwrites an accumulated one.
func TestClassifyFirstResolutionWinsAcrossNames(t *testing.T) {
	got := Classify(&Input{Names: []string{"Show S01E01 720p", "Show S01E02 1080p"}})
	if got.Resolution != "720p" {
		t.Errorf("Resolution = %q, want 720p (first observed resolution wins)", got.Resolution)
	}
}

// TestResolutionVocabularySingleHome pins the single-home claim
// resolutionHeights documents: every height in the vocabulary must be BOTH
// detectable by Classify (reResolution's alternation derives from the slice)
// and rankable by ResolutionRank (its Atoi path derives from the same slice),
// the detected value must be the canonical spelling, and the slice must stay
// in the documented highest-first order the doc comment promises. The
// hand-enumerated rows in TestClassifyResolution and TestResolutionRank cover
// today's five heights only, so a sixth added in a shape one consumer cannot
// parse (a "4K"-style entry: detected as text, yet ranking 0) would silently
// zero the resolution floor with every existing test green.
func TestResolutionVocabularySingleHome(t *testing.T) {
	if len(resolutionHeights) == 0 {
		t.Fatal("resolutionHeights is empty; the resolution vocabulary must carry every recognized height")
	}
	prev := 0
	for i, h := range resolutionHeights {
		if h != strings.ToLower(h) {
			t.Errorf("resolutionHeights[%d] = %q, want the canonical lowercase spelling", i, h)
		}
		rank := ResolutionRank(h)
		if rank <= 0 {
			t.Errorf("ResolutionRank(%q) = %d, want > 0: every vocabulary height must rank", h, rank)
		}
		name := "Show " + h + " x265"
		if got := Classify(&Input{Names: []string{name}}).Resolution; got != h {
			t.Errorf("Classify(%q).Resolution = %q, want the vocabulary height %q", name, got, h)
		}
		if i > 0 && rank >= prev {
			t.Errorf("resolutionHeights[%d] = %q ranks %d, want strictly below the preceding entry's %d (documented highest-first order)", i, h, rank, prev)
		}
		prev = rank
	}
}

// TestClassifyReadsTheSharedNameVocabulary is the release-side half of the
// convergence pinned across both name parsers (the indexer's season/episode
// tokenizer carries the other half): the marker edges and case classes come
// from internal/nametoken, so dot and hyphen END a token, underscore ends one
// too, and the two homographs read as strings.ToLower reads them - U+0130 and
// U+212A CONTINUE a word while U+017F does not fold onto s. These rows are the
// table form of the Unicode rows in TestClassifyKind: they state the shared
// rule directly at the boundary, where a regression would otherwise only show
// up as a misclassified kind.
func TestClassifyReadsTheSharedNameVocabulary(t *testing.T) {
	for _, tc := range []struct {
		name       string
		in         string
		wantKind   Kind
		wantRes    string
		wantReason string
	}{
		// Dot and hyphen are token edges on BOTH sides of a marker, and are
		// also accepted as the marker's own internal separator.
		{name: "dot-bounded crf tag", in: "Show.CRF18.1080p", wantKind: KindEncode, wantRes: "1080p", wantReason: "encoder marker: crf"},
		{name: "dot-separated crf tag", in: "Show.CRF.18.1080p", wantKind: KindEncode, wantRes: "1080p", wantReason: "encoder marker: crf"},
		{name: "hyphen-bounded remux token", in: "Show-Remux-1080p", wantKind: KindRemux, wantRes: "1080p", wantReason: "name/notes marker: remux"},
		{name: "hyphen-separated bd remux", in: "Show BD-Remux 1080p", wantKind: KindRemux, wantRes: "1080p", wantReason: "name/notes marker: remux"},
		{name: "dot-separated bd remux", in: "Show.BD.Remux.1080p", wantKind: KindRemux, wantRes: "1080p", wantReason: "name/notes marker: remux"},
		{name: "hyphen-separated bitrate", in: "Show 4500-kbps", wantKind: KindEncode, wantReason: "encoder marker: bitrate"},
		{name: "underscore-bounded crf tag", in: "Show_CRF18_1080p", wantKind: KindEncode, wantRes: "1080p", wantReason: "encoder marker: crf"},
		{name: "dot-terminated resolution", in: "Show.1080p.WEB", wantKind: KindUnknown, wantRes: "1080p", wantReason: "no remux or encode marker"},
		// A word rune on either side of a marker is NOT an edge, so the
		// marker does not fire.
		{name: "letter-glued remux is no marker", in: "Show Remuxing 1080p", wantKind: KindUnknown, wantRes: "1080p", wantReason: "no remux or encode marker"},
		{name: "letter-glued resolution height", in: "Show 1080px WEB", wantKind: KindUnknown, wantReason: "no remux or encode marker"},
		// The homographs: U+0130 and U+212A are word runes (they block an
		// edge), U+017F is a delimiter and never an s.
		{name: "U+0130 blocks the remux right edge", in: "Show Remux\u0130 1080p", wantKind: KindUnknown, wantRes: "1080p", wantReason: "no remux or encode marker"},
		{name: "U+212A blocks the remux right edge", in: "Show Remux\u212a 1080p", wantKind: KindUnknown, wantRes: "1080p", wantReason: "no remux or encode marker"},
		{name: "U+017F is an edge, so the remux token stands", in: "Show Remux\u017f 1080p", wantKind: KindRemux, wantRes: "1080p", wantReason: "name/notes marker: remux"},
		{name: "U+017F never folds onto the mandatory s of kbps", in: "Show 4500 kbp\u017f", wantKind: KindUnknown, wantReason: "no remux or encode marker"},
		{name: "U+017F is an edge after the optional s of bdrips", in: "Show BDRip\u017f", wantKind: KindEncode, wantReason: "encoder marker: encode"},
		{name: "U+0130 folds onto the i of bdrip", in: "Show BDR\u0130P", wantKind: KindEncode, wantReason: "encoder marker: encode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(&Input{Names: []string{tc.in}})
			if got.Kind != tc.wantKind || got.Reason != tc.wantReason {
				t.Errorf("Classify(%+q) kind = %q (%q), want %q (%q)", tc.in, got.Kind, got.Reason, tc.wantKind, tc.wantReason)
			}
			if got.Resolution != tc.wantRes {
				t.Errorf("Classify(%+q) resolution = %q, want %q", tc.in, got.Resolution, tc.wantRes)
			}
		})
	}
}
