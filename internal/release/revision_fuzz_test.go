package release

import (
	"strings"
	"testing"
)

func FuzzParseRevision(f *testing.F) {
	for _, seed := range []string{
		"[UDF] 91 Days - 04v2 (BDRip 1080p x264 FLACx2) [dual-audio] [B7FF8FD2].mkv",
		"[JySzE] Cowboy Bebop - 22 [v3].mkv",
		"Clannad.After.Story.2008.s02e01.1080p.BluRay.Opus.2.0.x265.v2-UDF.mkv",
		"Show.S01E01v2.REPACK.1080p-Grp.mkv",
		"Show.S01E01.PROPER.REPACK9.1080p-Grp.mkv",
		"[Grp] Show - 01v9 PROPER (BD 1080p).mkv",
		"Show.S01E01.1080p.BluRay.Opus2.0.AV1-Tasok.mkv",
		"[Grp] Show - 01v10 (BD).mkv",
		"日本REPACK.mkv",
		`C:\lib\[Grp] Show - 01v2.mkv`,
		"Season 1/",
		"",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		got := ParseRevision(name)
		base := name[strings.LastIndexAny(name, `/\`)+1:]
		blank := strings.TrimSpace(strings.ReplaceAll(base, "_", " ")) == ""
		if got.Known() == blank {
			t.Fatalf("ParseRevision(%q) = %+v: known=%t but blank base name=%t", name, got, got.Known(), blank)
		}
		if got.Known() && (got.Version < 0 || got.Version > 10) {
			t.Fatalf("ParseRevision(%q) = %+v: version outside 0..10", name, got)
		}
		wantExplicit := got.Marker == RevisionVersion || got.Marker == RevisionRepack || got.Marker == RevisionProper
		if got.Explicit() != wantExplicit {
			t.Fatalf("ParseRevision(%q) = %+v: Explicit()=%t for marker %d", name, got, got.Explicit(), got.Marker)
		}
		if got.Marker == RevisionNone && got.Version != 1 {
			t.Fatalf("ParseRevision(%q) = %+v: an original release must read Version 1", name, got)
		}
		if again := ParseRevision(base); again != got {
			t.Fatalf("ParseRevision(%q) = %+v but its base name %q reads %+v", name, got, base, again)
		}
	})
}
