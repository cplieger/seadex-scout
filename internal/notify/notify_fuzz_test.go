package notify

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cplieger/runesafe"
	"github.com/cplieger/seadex-scout/internal/compare"
)

// FuzzCapAttrBoundedAndSanitized fuzzes the emit path's per-attribute
// boundary over arbitrary untrusted text: every finding-line attribute
// derived from SeaDex, tracker, or library data (title, groups, tracker,
// classification_reason, URLs, info hash) rides through capAttr, and SeaDex
// admits multi-MB values. The invariants are the three properties the emitted
// Loki record depends on and that no table test can cover exhaustively:
// bounded volume (at most the budget plus the "..." marker, so one hostile
// record can never exceed a downstream log-pipeline line limit and suppress
// the warn line the better-release alert keys on), valid UTF-8 with no unsafe
// rune (no C1 terminal-escape introducer, bidi override, or partial rune
// re-minted by a naive re-cap), and honest passthrough - a value that needs
// neither the cap nor the growth re-cap must emit byte-identical to
// runesafe.Sanitize, never gaining a spurious truncation marker.
func FuzzCapAttrBoundedAndSanitized(f *testing.F) {
	f.Add("")
	f.Add("Frieren")
	f.Add("a\u009bb\u202ec\x1bd")
	f.Add("\xff\xfe\xfd")
	f.Add("https://nyaa.si/view/1?q=a\u2028b")
	f.Add(strings.Repeat("\xff", 12<<10))
	f.Add(strings.Repeat("g", maxAttrBytes))
	f.Add(strings.Repeat("\u202e", 4<<10))
	f.Fuzz(func(t *testing.T, raw string) {
		got := capAttr(raw)

		if len(got) > maxAttrBytes+len("...") {
			t.Errorf("capAttr(%d bytes) = %d bytes, want <= %d", len(raw), len(got), maxAttrBytes+len("..."))
		}
		if !utf8.ValidString(got) {
			t.Errorf("capAttr(%q) = %q, want valid UTF-8", raw, got)
		}
		for _, r := range got {
			if runesafe.IsUnsafe(r, true) {
				t.Errorf("capAttr(%q) emits unsafe rune %U", raw, r)
			}
		}
		clean := runesafe.Sanitize(raw)
		if len(raw) <= maxAttrBytes && len(clean) <= maxAttrBytes && got != clean {
			t.Errorf("capAttr(%q) = %q, want the byte-identical sanitized form %q", raw, got, clean)
		}
	})
}

// FuzzJoinLinksAttrBounded fuzzes the aggregate attribute boundary: a finding
// carries up to 512 obtainable sources, each tracker label and URL untrusted,
// so release_urls is the attribute most exposed to volume amplification. The
// invariants mirror the single-value target - bounded output and no unsafe
// rune - across an arbitrary source COUNT, which is the axis the table tests
// fix at 512.
func FuzzJoinLinksAttrBounded(f *testing.F) {
	f.Add("Nyaa", "https://nyaa.si/view/1", 2)
	f.Add("AB\u009b", "https://animebytes.tv/t/1\u202e", 7)
	f.Add("\xff", "\xff\xff", 512)
	f.Add("", "", 0)
	f.Fuzz(func(t *testing.T, tracker, url string, n int) {
		count := n % 600
		if count < 0 {
			count = -count
		}
		links := make([]compare.ReleaseLink, count)
		for i := range links {
			links[i] = compare.ReleaseLink{Tracker: tracker, URL: url}
		}

		got := joinLinksAttr(links)

		if len(got) > maxAttrBytes+len("...") {
			t.Errorf("joinLinksAttr(%d links) = %d bytes, want <= %d", count, len(got), maxAttrBytes+len("..."))
		}
		if !utf8.ValidString(got) {
			t.Errorf("joinLinksAttr(%d links) = %q, want valid UTF-8", count, got)
		}
		for _, r := range got {
			if runesafe.IsUnsafe(r, true) {
				t.Errorf("joinLinksAttr(%d links) emits unsafe rune %U", count, r)
			}
		}
	})
}
