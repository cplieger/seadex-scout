package indexer

import (
	"strings"
	"testing"

	"github.com/cplieger/seadex-scout/internal/seadex"
)

// FuzzFeedTitle_boundedAndTrimmed exercises the title synthesis on arbitrary
// SeaDex-supplied file names and release groups (untrusted upstream strings the
// regex pipeline parses). Invariants: it never panics, the result carries no
// leading/trailing whitespace (every return path trims), and the result never
// exceeds the longest input (the title is derived from one file name or the
// group, and every transformation - extension strip, episode collapse,
// whitespace collapse - only shrinks). A violation would mean the synthesized
// RSS title leaked padding or grew unboundedly from hostile catalogue data.
func FuzzFeedTitle_boundedAndTrimmed(f *testing.F) {
	f.Add("Frieren Beyond Journey's End - S01E07 (BD Remux 1080p) [PMR].mkv", "Frieren Beyond Journey's End - S01E08 (BD Remux 1080p) [PMR].mkv", "PMR")
	f.Add("[Grp] Some Show - 07 (1080p).mkv", "[Grp] Some Show - 08 (1080p).mkv", "")
	f.Add("NCED 01 (BD Remux 1080p AVC FLAC) [PMR].mkv", "Show Title - S02E01 (BD 1080p) [Grp].mkv", "Grp")
	f.Add("A Silent Voice (2016) (BD 1080p x264 FLAC) [Group].mkv", "", "Group")
	f.Add("", "", "  spaced group  ")
	f.Add("Scum.of.the.Brave.S01E05.1080p.CR.WEB-DL-VARYG.mkv", "", "VARYG")
	f.Add("[LostYears] Frieren - S01E15v2 (WEB 1080p) [3564C0AD].mkv", "[LostYears] Frieren - S01E16 (WEB 1080p) [06E8039D].mkv", "LostYears")
	f.Add("Show - 07.mkv", "Show - 08.mkv", "")
	f.Fuzz(func(t *testing.T, name1, name2, group string) {
		tor := &seadex.Torrent{
			ReleaseGroup: group,
			Files:        []seadex.File{{Name: name1}, {Name: name2}},
		}
		got := feedTitle(tor)
		if got != strings.TrimSpace(got) {
			t.Errorf("feedTitle(%q, %q, group %q) = %q, not trimmed", name1, name2, group, got)
		}
		if maxIn := max(len(name1), len(name2), len(group)); len(got) > maxIn {
			t.Errorf("feedTitle(%q, %q, group %q) = %q (len %d), exceeds longest input (len %d)",
				name1, name2, group, got, len(got), maxIn)
		}
	})
}

// FuzzFeedTitle_singleVideoPreservesName pins the single-video oracle the
// bounded/trimmed target above cannot: for a torrent holding exactly one
// recognized video file, the synthesized title is the trimmed base name, so a
// degenerate implementation that always returns "" cannot pass.
func FuzzFeedTitle_singleVideoPreservesName(f *testing.F) {
	f.Add("Show - S01E01 (1080p) [Grp]")
	f.Add("  Movie Title (2026)  ")
	f.Add("Season 1/Show - S01E01")
	f.Fuzz(func(t *testing.T, base string) {
		got := feedTitle(&seadex.Torrent{Files: []seadex.File{{Name: base + ".mkv"}}})
		want := strings.TrimSpace(base[strings.LastIndex(base, "/")+1:])
		if got != want {
			t.Errorf("feedTitle(single video %q) = %q, want %q", base, got, want)
		}
	})
}

// FuzzValidInfoHash_normalizedShapeAndIdempotent exercises the info-hash
// sanitizer on arbitrary untrusted input (SeaDex record InfoHash fields and
// Prowlarr torznab:attr values). Invariants: every non-empty result is exactly
// 40 lowercase-hex bytes, and the sanitizer is idempotent - so a value can
// never normalize differently between snapshot build (buildSnapshot) and
// lookup, which would silently break hash matching.
func FuzzValidInfoHash_normalizedShapeAndIdempotent(f *testing.F) {
	f.Add("143ed15e5e3df072ae91adaeb149973a887590dd")
	f.Add("143ED15E5E3DF072AE91ADAEB149973A887590DD")
	f.Add("  143ed15e5e3df072ae91adaeb149973a887590dd  ")
	f.Add("<redacted>")
	f.Add("g43ed15e5e3df072ae91adaeb149973a887590dd")
	f.Add("")
	f.Fuzz(func(t *testing.T, h string) {
		got := validInfoHash(h)
		if got != "" {
			if len(got) != 40 {
				t.Fatalf("validInfoHash(%q) = %q (len %d), want empty or 40 bytes", h, got, len(got))
			}
			for i := range len(got) {
				c := got[i]
				if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
					t.Fatalf("validInfoHash(%q) = %q, contains non-lowercase-hex byte %q", h, got, c)
				}
			}
		}
		if again := validInfoHash(got); again != got {
			t.Fatalf("validInfoHash not idempotent: validInfoHash(%q) = %q, re-applying gives %q", h, got, again)
		}
	})
}

// FuzzSynthesizeTitle_titledAndTrimmed exercises the assembled-title path on
// arbitrary show metadata and SeaDex file names (the untrusted strings the
// episode-marker regexes and the flag classifier parse). Invariants: it never
// panics, the result carries no leading/trailing whitespace, and with a
// non-blank show title the result begins with that trimmed title - so a
// degenerate implementation returning "" (or one that lets hostile file names
// displace the show title) cannot pass. The feedTitle fallback (blank title)
// has its own two targets above.
func FuzzSynthesizeTitle_titledAndTrimmed(f *testing.F) {
	f.Add("Frieren: Beyond Journey's End", 2023, 1, false, false, true,
		"Frieren - S01E07 (BD Remux 1080p) [PMR].mkv", "Frieren - S01E08 (BD Remux 1080p) [PMR].mkv", "PMR")
	f.Add("A Silent Voice", 2016, 0, true, false, false,
		"A Silent Voice (2016) (BD 1080p x264 FLAC) [Group].mkv", "", "Group")
	f.Add("Show OVA", 0, 0, false, true, false, "Show OVA - 01.mkv", "Show OVA - 02.mkv", "")
	f.Add("One Piece", 1999, 0, false, false, false, "[Grp] One Piece - 1085 (1080p).mkv", "", "Grp")
	f.Add("", 0, 0, false, false, false, "Show - S01E01.mkv", "NCED 01.mkv", "  spaced  ")
	f.Fuzz(func(t *testing.T, title string, year, season int, isMovie, isSpecial, dual bool, name1, name2, group string) {
		tor := &seadex.Torrent{
			ReleaseGroup: group,
			DualAudio:    dual,
			Files:        []seadex.File{{Name: name1}, {Name: name2}},
		}
		meta := EntryInfo{Title: title, Year: year, SeasonTvdb: season, IsMovie: isMovie, IsSpecial: isSpecial}
		got := synthesizeTitle(tor, meta)
		if got != strings.TrimSpace(got) {
			t.Errorf("synthesizeTitle(title %q) = %q, not trimmed", title, got)
		}
		if want := strings.TrimSpace(title); want != "" && !strings.HasPrefix(got, want) {
			t.Errorf("synthesizeTitle(title %q) = %q, want it to begin with the trimmed show title", title, got)
		}
	})
}
