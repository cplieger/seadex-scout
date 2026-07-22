package anilist

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzParseMedia exercises the single-media GraphQL decoder against arbitrary
// bytes (the AniList response is an untrusted network boundary). Beyond
// crash-freedom it asserts the title invariant callers rely on: the returned
// title list is free of empty and duplicate entries (what dedupeTitles
// guarantees), so a downstream normalized-title match never keys on "".
func FuzzParseMedia(f *testing.F) {
	f.Add([]byte(`{"data":{"Media":{"format":"TV","seasonYear":2023,"title":{"romaji":"A","english":"B","native":"C"}}}}`))
	f.Add([]byte(`{"data":{"Media":null}}`))
	f.Add([]byte(`{"data":{"Media":null},"errors":[{"message":"x"}]}`))
	f.Add([]byte(`{"data":{"Media":{"title":{"romaji":"A","english":"A"}}}}`))
	f.Add([]byte(``))
	f.Add([]byte(`{bad`))
	f.Add([]byte(`{"data":{"Media":{"format":"MOVIE","startDate":{"year":2020},"title":{"romaji":"A"}}}}`))
	f.Add([]byte(`{"data":{"Media":{"format":"TV","title":{"romaji":"` + strings.Repeat("a", maxTitleBytes+1) + `"}}}}`))
	f.Add([]byte(`{"data":{"Media":{"format":"` + strings.Repeat("F", maxFormatBytes+1) + `","title":{"romaji":"A"}}}}`))
	f.Add([]byte(`{"data":{"Media":null},"errors":[{"message":"Not Found.","status":404}]}`))
	f.Add([]byte(`{"data":{"Media":{"format":"TV","title":{"romaji":"A"}}},"errors":[{"message":"partial"}]}`))
	f.Add([]byte(`{"data":{"Media":{"title":{"romaji":" ","english":"\t"}}}}`))
	f.Add([]byte(`{"data":{"Media":{"format":"TV","title":{"romaji":"!!!"}}}}`))
	f.Add([]byte("{\"data\":{\"Media\":{\"format\":\"TV\",\"title\":{\"romaji\":\"A\xff\"}}}}"))
	f.Add([]byte("\xff\xfe"))
	f.Add([]byte(`{"data":{"Media":{"format":"TV","title":{"romaji":"A\ud800"}}}}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		m, err := parseMedia(raw)
		if err != nil {
			return
		}
		assertTitlesClean(t, m.Titles, raw)
	})
}

// FuzzParseMediaPage exercises the batched Page(media) decoder against arbitrary
// bytes, asserting the same title invariant across every returned id plus the
// id guard callers rely on: parseMediaPage rejects non-positive media IDs, so
// every key in the returned map must be positive.
func FuzzParseMediaPage(f *testing.F) {
	f.Add([]byte(`{"data":{"Page":{"media":[{"id":1,"title":{"romaji":"A","english":"A"}}]}}}`))
	f.Add([]byte(`{"data":{"Page":{"media":[]}}}`))
	f.Add([]byte(`{"errors":[{"message":"x"}]}`))
	f.Add([]byte(``))
	f.Add([]byte(`{bad`))
	f.Add([]byte(`{"data":{"Page":null}}`))
	f.Add([]byte(`{"data":{"Page":{"media":"nope"}}}`))
	f.Add([]byte(`{"data":{"Page":{"media":[{"id":"x","title":{"romaji":"A"}}]}}}`))
	f.Add([]byte(`{"data":{"Page":{"media":[{"id":0,"title":{"romaji":"missing"}}]}}}`))
	f.Add([]byte(`{"data":{"Page":{"media":[{"id":-1,"title":{"romaji":"negative"}}]}}}`))
	f.Add([]byte(`{"data":{"Page":{"media":[{"id":2,"format":"MOVIE","startDate":{"year":2019},"title":{"romaji":"B","english":"B"}}]}}}`))
	f.Add([]byte(`{"data":{"Page":{"media":[{"id":1,"title":{"romaji":"first"}},{"id":1,"title":{"romaji":"second"}},{"id":2,"title":{"romaji":"sibling"}}]}}}`))
	f.Add([]byte(`{"data":{"Page":{"media":[{"id":1,"title":{"romaji":"` + strings.Repeat("a", maxTitleBytes+1) + `"}},{"id":2,"title":{"romaji":"valid"}}]}}}`))
	f.Add([]byte(`{"data":{"Page":{"media":[{"id":1,"title":{"romaji":" "}}]}}}`))
	f.Add([]byte("{\"data\":{\"Page\":{\"media\":[{\"id\":1,\"title\":{\"romaji\":\"A\xff\"}}]}}}"))
	f.Add([]byte("\xff\xfe"))
	f.Add([]byte(`{"data":{"Page":{"media":[{"id":1,"title":{"romaji":"A\ud800"}}]}}}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		out, err := parseMediaPage(raw)
		if err != nil {
			return
		}
		for id, m := range out {
			if id <= 0 {
				t.Errorf("parseMediaPage(%q) returned non-positive id %d", raw, id)
			}
			assertTitlesClean(t, m.Titles, raw)
		}
	})
}

func assertTitlesClean(t *testing.T, titles []string, raw []byte) {
	t.Helper()
	seen := make(map[string]struct{}, len(titles))
	for _, title := range titles {
		if title == "" {
			t.Errorf("empty title from %q", raw)
		}
		if _, dup := seen[title]; dup {
			t.Errorf("duplicate title %q from %q", title, raw)
		}
		seen[title] = struct{}{}
	}
}

// FuzzSanitizeUpstreamMessage exercises the log-forging sanitizer against
// arbitrary upstream error messages (attacker-controllable via JSON \u escapes
// in a GraphQL error envelope). Invariants: output is always valid UTF-8, is
// bounded by the 200-byte retained cap plus the 3-byte ellipsis, retains none
// of the rune classes the sanitizer exists to strip, and passes a short clean
// message through unchanged.
func FuzzSanitizeUpstreamMessage(f *testing.F) {
	f.Add("Media not found.")
	f.Add("line1\nline2\x7f")
	f.Add("a\u009bb\u009dc")
	f.Add("a\u202eb\u2066c\u2069d")
	f.Add("a\u061cb\u200ec\u200fd")
	f.Add(strings.Repeat("a", 199) + "\u4e16\u754c")
	f.Add(strings.Repeat("\u00e9", 150))
	f.Add("")
	f.Fuzz(func(t *testing.T, in string) {
		out := sanitizeUpstreamMessage(in)
		if !utf8.ValidString(out) {
			t.Errorf("sanitizeUpstreamMessage(%q) = %q is not valid UTF-8", in, out)
		}
		if len(out) > 203 {
			t.Errorf("sanitizeUpstreamMessage(%q) length = %d, want <= 203 (200-byte cap + ellipsis)", in, len(out))
		}
		for _, r := range out {
			if isForbiddenLogRune(r) {
				t.Errorf("sanitizeUpstreamMessage(%q) retained forbidden rune %U", in, r)
			}
		}
		if utf8.ValidString(in) && len(in) <= 200 && !strings.ContainsFunc(in, isForbiddenLogRune) && out != in {
			t.Errorf("sanitizeUpstreamMessage(%q) = %q, want a short clean message passed through unchanged", in, out)
		}
	})
}

// isForbiddenLogRune restates the sanitizer's security contract: the rune
// classes that must never survive into a logged upstream message (C0/C1
// controls, DEL, line and paragraph separators, and every Bidi_Control rune).
func isForbiddenLogRune(r rune) bool {
	switch {
	case r < 0x20 || r == 0x7f,
		r >= 0x80 && r <= 0x9f,
		r == '\u2028' || r == '\u2029',
		r == '\u061c',
		r == '\u200e' || r == '\u200f',
		r >= '\u202a' && r <= '\u202e',
		r >= '\u2066' && r <= '\u2069':
		return true
	}
	return false
}
