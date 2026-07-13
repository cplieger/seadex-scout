package release

import "testing"

// TestGroupNoGroupFallback covers the NoGroup fallback: a release with no group
// classifies and normalizes to NoGroup on both sides, so a group-less library
// file and a group-less SeaDex release (or SeaDex's own literal "NOGRP") compare
// as the same group rather than being skipped.
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

// TestClassifyDualAudioMarker covers the dual-audio marker detection: an
// explicit flag, a "[Dual Audio]" tag, and a bare "dual" token all classify as
// dual-audio, while an ordinary word such as "individual" does not.
func TestClassifyDualAudioMarker(t *testing.T) {
	tests := []struct {
		name string
		in   Input
		want bool
	}{
		{name: "explicit flag", in: Input{DualAudio: true}, want: true},
		{name: "dual audio tag", in: Input{Names: []string{"Show [Dual Audio]"}}, want: true},
		{name: "dual token", in: Input{Notes: "dual"}, want: true},
		{name: "individual is not dual audio", in: Input{Names: []string{"Individual Circumstances 1080p"}}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(&tt.in).DualAudio; got != tt.want {
				t.Errorf("DualAudio = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestGroupsIntersectNormalizesAndHandlesNoGroup pins GroupsIntersect's
// normalized-overlap contract: case/whitespace insensitivity, the NoGroup
// fallback for empty groups, and false for disjoint or empty sets.
func TestGroupsIntersectNormalizesAndHandlesNoGroup(t *testing.T) {
	tests := []struct {
		name string
		a    []string
		b    []string
		want bool
	}{
		{name: "case and whitespace insensitive", a: []string{" SubsPlease "}, b: []string{"subsplease"}, want: true},
		{name: "missing group matches NoGroup", a: []string{""}, b: []string{NoGroup}, want: true},
		{name: "one overlapping normalized group", a: []string{"", "PMR"}, b: []string{"LostYears", " pmr "}, want: true},
		{name: "disjoint groups do not intersect", a: []string{"PMR"}, b: []string{"LostYears"}, want: false},
		{name: "empty side does not intersect", a: nil, b: []string{NoGroup}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := GroupsIntersect(tc.a, tc.b); got != tc.want {
				t.Errorf("GroupsIntersect(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestClassifyKind covers the ordered remux -> encode -> unknown classification
// in classifyKind: a name/notes "remux" marker wins, then an encoder marker
// (codec token, CRF, or bitrate), else unknown. The remux-vs-encode decision is
// name-and-notes based, so these are the branches the daemon and report both key
// alignment on.
func TestClassifyKind(t *testing.T) {
	tests := []struct {
		name       string
		in         Input
		wantKind   Kind
		wantReason string
	}{
		{name: "remux from name", in: Input{Names: []string{"Show 1080p BDRemux"}}, wantKind: KindRemux, wantReason: "name/notes marker: remux"},
		{name: "remux from notes", in: Input{Notes: "best remux available"}, wantKind: KindRemux, wantReason: "name/notes marker: remux"},
		{name: "encode from codec token", in: Input{Names: []string{"Show 1080p x265"}}, wantKind: KindEncode, wantReason: "encoder marker: x265"},
		{name: "encode from crf", in: Input{Names: []string{"Show CRF18"}}, wantKind: KindEncode, wantReason: "encoder marker: crf"},
		{name: "encode from bitrate", in: Input{Names: []string{"Show 4500 kbps"}}, wantKind: KindEncode, wantReason: "encoder marker: bitrate"},
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
// wins over a conflicting name token, and an absent marker yields "".
func TestClassifyCodec(t *testing.T) {
	tests := []struct {
		name string
		in   Input
		want string
	}{
		{name: "x265 from name", in: Input{Names: []string{"Show 1080p x265"}}, want: "x265"},
		{name: "hevc token maps to x265", in: Input{Names: []string{"Show HEVC"}}, want: "x265"},
		{name: "x264 from name", in: Input{Names: []string{"Show 720p x264"}}, want: "x264"},
		{name: "avc token maps to x264", in: Input{Names: []string{"Show AVC"}}, want: "x264"},
		{name: "mediainfo codec wins over name", in: Input{Names: []string{"Show x264"}, VideoCodec: "HEVC"}, want: "x265"},
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
		{tracker: "AB", want: TrackerPrivate},
		{tracker: "AnimeBytes", want: TrackerPrivate},
		{tracker: "  ", want: TrackerUnknown},
		{tracker: "SomeRandomTracker", want: TrackerUnknown},
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
