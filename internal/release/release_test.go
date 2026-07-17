package release

import "testing"

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
		{name: "remux inside a longer word is not a marker", in: Input{Names: []string{"Show DreamRemuxer 1080p"}}, wantKind: KindUnknown, wantReason: "no remux or encode marker"},
		{name: "per-file encode marker wins over notes remux", in: Input{Names: []string{"Show 1080p x265"}, Notes: "grab the remux"}, wantKind: KindEncode, wantReason: "encoder marker: x265"},
		{name: "notes fill the gap when the name carries no marker", in: Input{Names: []string{"Show 1080p"}, Notes: "crf 18 encode"}, wantKind: KindEncode, wantReason: "encoder marker: crf"},
		{name: "encode from codec token", in: Input{Names: []string{"Show 1080p x265"}}, wantKind: KindEncode, wantReason: "encoder marker: x265"},
		{name: "encode from crf", in: Input{Names: []string{"Show CRF18"}}, wantKind: KindEncode, wantReason: "encoder marker: crf"},
		{name: "encode from bitrate", in: Input{Names: []string{"Show 4500 kbps"}}, wantKind: KindEncode, wantReason: "encoder marker: bitrate"},
		{name: "encode from generic encode token", in: Input{Names: []string{"Show S01 1080p encode"}}, wantKind: KindEncode, wantReason: "encoder marker: encode"},
		{name: "encode from encoded token", in: Input{Names: []string{"Show 1080p [Encoded]"}}, wantKind: KindEncode, wantReason: "encoder marker: encode"},
		{name: "encode from bdrip token", in: Input{Names: []string{"Show BDRip 1080p"}}, wantKind: KindEncode, wantReason: "encoder marker: encode"},
		{name: "notes fill the gap with a generic encode token", in: Input{Names: []string{"Show 1080p"}, Notes: "a solid encode"}, wantKind: KindEncode, wantReason: "encoder marker: encode"},
		{name: "remux token wins over a generic encode token", in: Input{Names: []string{"Show BD-Remux encode 1080p"}}, wantKind: KindRemux, wantReason: "name/notes marker: remux"},
		{name: "reencode is not an encode marker", in: Input{Names: []string{"Show reencode 1080p"}}, wantKind: KindUnknown, wantReason: "no remux or encode marker"},
		{name: "reencoded is not an encode marker", in: Input{Names: []string{"Show reencoded 1080p"}}, wantKind: KindUnknown, wantReason: "no remux or encode marker"},
		{name: "encoder is not an encode marker", in: Input{Names: []string{"Show 1080p encoder notes"}}, wantKind: KindUnknown, wantReason: "no remux or encode marker"},
		{name: "underscore-delimited bdremux", in: Input{Names: []string{"Show_1080p_BDRemux"}}, wantKind: KindRemux, wantReason: "name/notes marker: remux"},
		{name: "underscore-delimited premux", in: Input{Names: []string{"Show_S01_PREMUX"}}, wantKind: KindRemux, wantReason: "name/notes marker: remux"},
		{name: "underscore-delimited crf", in: Input{Names: []string{"Show_CRF18"}}, wantKind: KindEncode, wantReason: "encoder marker: crf"},
		{name: "underscore-delimited bitrate", in: Input{Names: []string{"Show_4500_kbps"}}, wantKind: KindEncode, wantReason: "encoder marker: bitrate"},
		{name: "underscore-delimited bdrip", in: Input{Names: []string{"Show_1080p_BDRip"}}, wantKind: KindEncode, wantReason: "encoder marker: encode"},
		{name: "unknown when no marker", in: Input{Names: []string{"Show 1080p"}}, wantKind: KindUnknown, wantReason: "no remux or encode marker"},
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

// TestClassifyCodec covers detectCodec/canonicalCodec: x265/HEVC and x264/AVC
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
		{name: "mediainfo codec wins over name", in: Input{Names: []string{"Show x264"}, VideoCodec: "HEVC"}, want: "x265"},
		{name: "name marker wins over conflicting notes", in: Input{Names: []string{"Show 1080p x264"}, Notes: "an x265 encode also exists"}, want: "x264"},
		{name: "notes fill the gap when name has no marker", in: Input{Names: []string{"Show 1080p"}, Notes: "x265 encode"}, want: "x265"},
		{name: "no codec marker", in: Input{Names: []string{"Show 1080p"}}, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(&tc.in).Codec; got != tc.want {
				t.Errorf("Codec = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClassifyTrackerAndAnimeBytes covers classifyTracker's public/private/unknown
// mapping and IsAnimeBytes (the one private tracker gated behind the opt-in
// toggle), including case/whitespace insensitivity and the empty-string miss.
func TestClassifyTrackerAndAnimeBytes(t *testing.T) {
	trackerTests := []struct {
		tracker string
		want    TrackerType
	}{
		{tracker: "Nyaa", want: TrackerPublic},
		{tracker: "AnimeTosho", want: TrackerPublic},
		{tracker: "RuTracker", want: TrackerPublic},
		{tracker: "AB", want: TrackerPrivate},
		{tracker: "AnimeBytes", want: TrackerPrivate},
		{tracker: "  ", want: TrackerUnknown},
		{tracker: "SomeRandomTracker", want: TrackerUnknown},
		{tracker: "beyondhd", want: TrackerUnknown},
		{tracker: "ptp", want: TrackerUnknown},
	}
	for _, tc := range trackerTests {
		if got := Classify(&Input{Tracker: tc.tracker}).TrackerType; got != tc.want {
			t.Errorf("TrackerType(%q) = %q, want %q", tc.tracker, got, tc.want)
		}
	}
	abTests := []struct {
		tracker string
		want    bool
	}{
		{tracker: "AB", want: true},
		{tracker: " animebytes ", want: true},
		{tracker: "Nyaa", want: false},
		{tracker: "", want: false},
	}
	for _, tc := range abTests {
		if got := IsAnimeBytes(tc.tracker); got != tc.want {
			t.Errorf("IsAnimeBytes(%q) = %v, want %v", tc.tracker, got, tc.want)
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

// TestIsAnimeBytesHost pins the AB host gate consumed by the AnimeBytes link
// hider (filter.ABVisible) and the indexer's tracker-key routing
// (trackerKeyFromURL): the exact site host, its real dot-delimited
// subdomains, and the DNS-root trailing-dot form match; a suffix-confusion
// host, a parent-domain spoof, an empty-labeled host, a non-ASCII homograph
// label, and any other tracker do not. The gate resolves through the
// canonical tracker table (LookupTrackerByHost), which folds case, so a
// mixed-case host matches too.
func TestIsAnimeBytesHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{host: "animebytes.tv", want: true},
		{host: "www.animebytes.tv", want: true},
		{host: "tracker.animebytes.tv", want: true},
		{host: "animebytes.tv.", want: true},
		{host: "www.animebytes.tv.", want: true},
		{host: "maliciousanimebytes.tv", want: false},
		{host: "evil-animebytes.tv", want: false},
		{host: "animebytes.tv.evil.com", want: false},
		{host: "animebytes.tv..", want: false},
		{host: ".animebytes.tv", want: false},
		{host: "a..animebytes.tv", want: false},
		{host: "x\u00e9.animebytes.tv", want: false},
		{host: "nyaa.si", want: false},
		{host: "AnimeBytes.tv", want: true},
		{host: "", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.host, func(t *testing.T) {
			if got := IsAnimeBytesHost(tc.host); got != tc.want {
				t.Errorf("IsAnimeBytesHost(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

// TestLookupTracker pins the canonical tracker table contract every consumer
// (classification, seadex link building, indexer feed routing) resolves
// through: canonical names and aliases resolve case- and whitespace-
// insensitively to one entry with a non-empty base URL, and empty, unknown,
// or deliberately-stripped tracker names are not found.
func TestLookupTracker(t *testing.T) {
	found := []struct {
		in       string
		wantName string
		wantType TrackerType
	}{
		{in: "Nyaa", wantName: TrackerNameNyaa, wantType: TrackerPublic},
		{in: "nyaa", wantName: TrackerNameNyaa, wantType: TrackerPublic},
		{in: " AB ", wantName: TrackerNameAnimeBytes, wantType: TrackerPrivate},
		{in: "animebytes", wantName: TrackerNameAnimeBytes, wantType: TrackerPrivate},
		{in: "AnimeTosho", wantName: TrackerNameAnimeTosho, wantType: TrackerPublic},
		{in: "RuTracker", wantName: TrackerNameRuTracker, wantType: TrackerPublic},
	}
	for _, tc := range found {
		got, ok := LookupTracker(tc.in)
		if !ok {
			t.Errorf("LookupTracker(%q) not found, want %q", tc.in, tc.wantName)
			continue
		}
		if got.Name != tc.wantName || got.Type != tc.wantType {
			t.Errorf("LookupTracker(%q) = %q/%q, want %q/%q", tc.in, got.Name, got.Type, tc.wantName, tc.wantType)
		}
		if got.BaseURL == "" {
			t.Errorf("LookupTracker(%q) has an empty BaseURL; every table entry carries one", tc.in)
		}
	}
	for _, in := range []string{"", "   ", "beyondhd", "bhd", "passthepopcorn", "ptp", "broadcasthenet", "btn", "hdbits", "blutopia", "aither", "SomeRandomTracker"} {
		if _, ok := LookupTracker(in); ok {
			t.Errorf("LookupTracker(%q) found, want not found", in)
		}
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
