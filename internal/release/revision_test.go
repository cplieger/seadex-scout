package release

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseRevisionReadsExplicitTokens(t *testing.T) {
	tests := []struct {
		name string
		want Revision
	}{
		{"[UDF] 91 Days - 04v2 (BDRip 1080p x264 FLACx2) [dual-audio] [B7FF8FD2].mkv", Revision{2, RevisionVersion}},
		{"[Grp] Show - 01v2.mkv", Revision{2, RevisionVersion}},
		{"[vyral] Claymore - S01E26v2 (BD 1280x716 HEVC FLAC AC3).mkv", Revision{2, RevisionVersion}},
		{"Darling in the FranXX - S01E05v3 - (BD 1080p HEVC 10-bit Opus) [Dual Audio] [BlackRose].mkv", Revision{3, RevisionVersion}},
		{"s01e01V2.mkv", Revision{2, RevisionVersion}},
		{"[JySzE] Cowboy Bebop - 22 [v3].mkv", Revision{3, RevisionVersion}},
		{"Clannad.After.Story.2008.s02e01.1080p.BluRay.Opus.2.0.x265.v2-UDF.mkv", Revision{2, RevisionVersion}},
		{"Show_-_01v2_[1080p].mkv", Revision{2, RevisionVersion}},
		{"[Grp] Show - 01v0 (WEB 1080p).mkv", Revision{0, RevisionVersion}},
		{"[Grp] Show - 01 [v1].mkv", Revision{1, RevisionVersion}},
		{"[Grp] Show - 01v2 [v3].mkv", Revision{2, RevisionVersion}},
		{"Season 1/[Grp] Show - 01v2.mkv", Revision{2, RevisionVersion}},
		{`C:\lib\[Grp] Show - 01v2.mkv`, Revision{2, RevisionVersion}},
		{"86.Eighty.Six.S01E01.REPACK.1080p.Blu-ray.Opus2.0.x265-koala.mkv", Revision{2, RevisionRepack}},
		{"Given.2020.REPACK.1080p.BluRay.REMUX.DTS-HD.MA.5.1.AVC-PTP.mkv", Revision{2, RevisionRepack}},
		{"Show.S01E01.REPACK2.1080p.BluRay.x265-Grp.mkv", Revision{3, RevisionRepack}},
		{"Show.S01E01.REPACK3.1080p.BluRay.x265-Grp.mkv", Revision{4, RevisionRepack}},
		{"Show.S01E01.RERIP.1080p.BluRay.x265-Grp.mkv", Revision{2, RevisionRepack}},
		{"Show.S01E01.rerip2.1080p.BluRay.x265-Grp.mkv", Revision{3, RevisionRepack}},
		{"Show.S01E01.REPAC\u212a.1080p-Grp.mkv", Revision{2, RevisionRepack}},
		{"Show.S01E01v2.REPACK.1080p-Grp.mkv", Revision{3, RevisionRepack}},
		{"Show.S01E01.PROPER.REPACK.1080p-Grp.mkv", Revision{2, RevisionRepack}},
		{"Show.S01E01.PROPER.1080p.BluRay.x264-Grp.mkv", Revision{2, RevisionProper}},
		{"[Grp] Show - 01v2 PROPER (BD 1080p).mkv", Revision{3, RevisionProper}},
	}
	for _, tc := range tests {
		if got := ParseRevision(tc.name); got != tc.want {
			t.Errorf("ParseRevision(%q) = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestParseRevisionReadsOriginalWhenNoToken(t *testing.T) {
	original := Revision{1, RevisionNone}
	tests := []struct {
		name string
		desc string
	}{
		{"[Grp] Show - 01 (BD 1080p x265 10-bit FLAC) [ABCD1234].mkv", "codec and resolution tokens"},
		{"Show.S01E01.1080p.BluRay.DTS-HD.MA.5.1.x265-Grp.mkv", "audio channel layout"},
		{"Show.S01E01.1080p.BluRay.Opus2.0.AV1-Tasok.mkv", "AV1 after a digit-dot run"},
		{"[RigAV1] Show - 01 (BD 1080p AV1).mkv", "AV1 inside a group name"},
		{"[FLAV1N] Show - 01.mkv", "V1 inside a group name"},
		{"[Grp] Show - 01 (BD 3840x2160 FFV1 FLAC).mkv", "FFV1 codec"},
		{"[Grp] Show (BD 1080p HEVC-YUV420P10 FLAC).mkv", "pixel format"},
		{"[Grp] Lev2el Up - 01.mkv", "v2 inside a title word"},
		{"[Grp] Show Vol.2 - 01.mkv", "volume number"},
		{"Show.S01E01.PROPERTY.1080p-Grp.mkv", "proper as a word prefix"},
		{"Show.S01E01.REPACKED.1080p-Grp.mkv", "repack as a word prefix"},
		{"Show.S01E01.REAL.1080p-Grp.mkv", "REAL is not modeled"},
		{"Show.S01E01.RER\u0130P.1080p-Grp.mkv", "dotted capital I outside a Turkic culture"},
		// The arrs' grammar misses each shape below, so reading a token here would
		// make an already-current library file look older than SeaDex's.
		{"[Grp] Show - S01E01 (v2) (BD 1080p).mkv", "parenthesized version"},
		{"[Grp] Show [BD 1080p x264 10bit FLAC]v2.mkv", "version after a bracket"},
		{"[Grp] Hana no Tou v2 (BD 1080p).mkv", "version after a title word"},
		{"[Grp] Show - 01v10 (BD).mkv", "two-digit version"},
		{"[Grp] Show - OVAv1 [BD].mkv", "version after a letter"},
		{"Show v2/[Grp] Show - 01.mkv", "version in a directory component"},
		{"Show - S01E01 - Title [Bluray-1080p v2][x265]-Grp.mkv", "Sonarr-only resolution arm"},
		{"日本REPACK.mkv", "no word boundary after a CJK letter"},
	}
	for _, tc := range tests {
		if got := ParseRevision(tc.name); got != original {
			t.Errorf("ParseRevision(%q) [%s] = %+v, want %+v", tc.name, tc.desc, got, original)
		}
	}
}

func TestParseRevisionUnknownForBlankBaseName(t *testing.T) {
	for _, name := range []string{"", "   ", "Season 1/", `dir\`, "__"} {
		if got := ParseRevision(name); got != (Revision{}) {
			t.Errorf("ParseRevision(%q) = %+v, want the unknown reading", name, got)
		}
	}
}

func TestParseTitleRevisionReadsAcrossSlash(t *testing.T) {
	title := "Show v2 01v3 / Fate S01 1080p [Grp]"
	if got := ParseTitleRevision(title); got != (Revision{3, RevisionVersion}) {
		t.Errorf("ParseTitleRevision(%q) = %+v, want v3 read before the slash", title, got)
	}
	if got := ParseRevision(title); got != (Revision{1, RevisionNone}) {
		t.Errorf("ParseRevision(%q) = %+v, want v1 from the base name only", title, got)
	}
}

func TestArrRevision(t *testing.T) {
	tests := []struct {
		version  int
		isRepack bool
		want     Revision
	}{
		{1, false, Revision{1, RevisionNone}},
		{2, false, Revision{2, RevisionVersion}},
		{2, true, Revision{2, RevisionRepack}},
		{0, false, Revision{0, RevisionVersion}},
		{-1, false, Revision{}},
		{-1, true, Revision{}},
	}
	for _, tc := range tests {
		if got := ArrRevision(tc.version, tc.isRepack); got != tc.want {
			t.Errorf("ArrRevision(%d, %t) = %+v, want %+v", tc.version, tc.isRepack, got, tc.want)
		}
	}
}

func TestExplicitRevision(t *testing.T) {
	tests := []struct {
		desc  string
		names []string
		want  Revision
	}{
		{"no names", nil, Revision{}},
		{"only unversioned names", []string{"[G] Show - 01.mkv", "Season 1/[G] Show - 01.mkv"}, Revision{}},
		{"newest explicit wins", []string{"x 01v2.mkv", "y REPACK2.mkv"}, Revision{3, RevisionRepack}},
		{"explicit beside unversioned", []string{"[G] Show - 01.mkv", "[G] Show - 01v2.mkv"}, Revision{2, RevisionVersion}},
	}
	for _, tc := range tests {
		if got := ExplicitRevision(tc.names...); got != tc.want {
			t.Errorf("ExplicitRevision(%q) [%s] = %+v, want %+v", tc.names, tc.desc, got, tc.want)
		}
	}
}

func TestNewestRevision(t *testing.T) {
	none1 := Revision{1, RevisionNone}
	v1 := Revision{1, RevisionVersion}
	v2 := Revision{2, RevisionVersion}
	v3 := Revision{3, RevisionVersion}
	repack2 := Revision{2, RevisionRepack}
	tests := []struct {
		desc string
		revs []Revision
		want Revision
	}{
		{desc: "empty"},
		{desc: "any unknown", revs: []Revision{v2, {}}},
		{desc: "unknown first", revs: []Revision{{}, v2}},
		{desc: "original and v2", revs: []Revision{none1, v2}, want: v2},
		{desc: "one v2 among originals", revs: []Revision{none1, none1, v2, none1}, want: v2},
		{desc: "version tie", revs: []Revision{v2, repack2}, want: repack2},
		{desc: "version tie reversed", revs: []Revision{repack2, v2}, want: repack2},
		{desc: "descending", revs: []Revision{v3, v2}, want: v3},
		{desc: "original against explicit v1", revs: []Revision{v1, none1}, want: v1},
		{desc: "originals only", revs: []Revision{none1, none1}, want: none1},
	}
	for _, tc := range tests {
		if got := NewestRevision(tc.revs...); got != tc.want {
			t.Errorf("NewestRevision(%+v) [%s] = %+v, want %+v", tc.revs, tc.desc, got, tc.want)
		}
	}
}

func TestRevisionBehind(t *testing.T) {
	none1 := Revision{1, RevisionNone}
	tests := []struct {
		held, listed Revision
		want         bool
	}{
		{none1, Revision{2, RevisionVersion}, true},
		{Revision{2, RevisionVersion}, Revision{2, RevisionVersion}, false},
		{Revision{3, RevisionVersion}, Revision{2, RevisionVersion}, false},
		{Revision{}, Revision{2, RevisionVersion}, false},
		{none1, none1, false},
		{none1, Revision{2, RevisionRepack}, true},
		{Revision{0, RevisionVersion}, Revision{1, RevisionVersion}, true},
		{none1, Revision{1, RevisionVersion}, false},
		{none1, Revision{}, false},
		{none1, Revision{Version: 5}, false},
	}
	for _, tc := range tests {
		if got := RevisionBehind(tc.held, tc.listed); got != tc.want {
			t.Errorf("RevisionBehind(%+v, %+v) = %t, want %t", tc.held, tc.listed, got, tc.want)
		}
	}
}

func TestRevisionMarkerTextRoundTrip(t *testing.T) {
	for _, marker := range []RevisionMarker{RevisionUnknown, RevisionNone, RevisionVersion, RevisionRepack, RevisionProper} {
		text, err := marker.MarshalText()
		if err != nil {
			t.Fatalf("RevisionMarker(%d).MarshalText() error = %v", marker, err)
		}
		var decoded RevisionMarker
		if err := decoded.UnmarshalText(text); err != nil || decoded != marker {
			t.Errorf("UnmarshalText(%q) = %d, %v; want %d, nil", text, decoded, err, marker)
		}
	}
	if _, err := RevisionMarker(len(revisionMarkerNames)).MarshalText(); err == nil {
		t.Errorf("MarshalText on an undeclared marker returned nil error, want an error")
	}
}

func TestRevisionMarkerUnknownTextDecodesUnknown(t *testing.T) {
	decoded := RevisionProper
	if err := decoded.UnmarshalText([]byte("garbage")); err != nil || decoded != RevisionUnknown {
		t.Errorf(`UnmarshalText("garbage") = %d, %v; want RevisionUnknown, nil`, decoded, err)
	}
	var rev Revision
	if err := json.Unmarshal([]byte(`{"version":2,"marker":"retired"}`), &rev); err != nil || rev.Known() {
		t.Errorf("json.Unmarshal of a retired marker = %+v, %v; want an unknown reading and nil error", rev, err)
	}
}

func TestReleaseOmitsUnknownRevision(t *testing.T) {
	b, err := json.Marshal(Release{Group: "G"})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(b), "revision") {
		t.Errorf("json.Marshal(Release with zero Revision) = %s, want no revision key", b)
	}
	b, err = json.Marshal(Release{Group: "G", Revision: Revision{3, RevisionRepack}})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var back Release
	if err := json.Unmarshal(b, &back); err != nil || back.Revision != (Revision{3, RevisionRepack}) {
		t.Errorf("round trip of %s = %+v, %v; want revision {3 repack}", b, back.Revision, err)
	}
}

func TestRevisionString(t *testing.T) {
	tests := []struct {
		rev  Revision
		want string
	}{
		{Revision{}, ""},
		{Revision{1, RevisionNone}, "v1"},
		{Revision{2, RevisionVersion}, "v2"},
		{Revision{3, RevisionRepack}, "v3 (repack)"},
		{Revision{2, RevisionProper}, "v2 (proper)"},
	}
	for _, tc := range tests {
		if got := tc.rev.String(); got != tc.want {
			t.Errorf("%+v.String() = %q, want %q", tc.rev, got, tc.want)
		}
	}
}

func TestRevisionTokenReadsBackAtSameVersion(t *testing.T) {
	var explicit []Revision
	for v := range 10 {
		explicit = append(explicit, Revision{v, RevisionVersion})
	}
	for v := 2; v <= 10; v++ {
		explicit = append(explicit, Revision{v, RevisionRepack}, Revision{v, RevisionProper})
	}
	for _, rev := range explicit {
		token := rev.Token()
		if token == "" {
			if rev.Version <= maxBracketVersion {
				t.Errorf("%+v.Token() = \"\", want a token for a version a bracket spells", rev)
			}
			continue
		}
		title := "Fate/Zero S01 " + token + " 1080p [Grp]"
		if got := ParseTitleRevision(title); got.Version != rev.Version || !got.Explicit() {
			t.Errorf("ParseRevision(%q) = %+v, want an explicit reading at Version %d (from %+v)", title, got, rev.Version, rev)
		}
	}
	for _, rev := range []Revision{{}, {1, RevisionNone}} {
		if token := rev.Token(); token != "" {
			t.Errorf("%+v.Token() = %q, want \"\" for a reading with no token", rev, token)
		}
	}
}
