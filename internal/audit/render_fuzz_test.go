package audit

import (
	"strings"
	"testing"
)

// FuzzEscapeCell fuzzes the Markdown-table sanitizer over arbitrary untrusted
// text (titles and release groups arrive from the arrs, SeaDex, and AniList).
// The invariant is bounded output: the escaped cell may never contain a raw
// table or link metacharacter (| [ ] \ < >), a line break, any other C0
// control character, DEL, or a C1 control character (terminal-escape
// smuggling), or a Unicode bidi override/isolate character (visual
// reordering) — which is exactly what keeps
// a crafted title from breaking out of its cell, forging a link label,
// smuggling raw HTML, or manipulating the terminal/viewer that renders the
// report.
func FuzzEscapeCell(f *testing.F) {
	f.Add("plain title")
	f.Add("a|b\nc")
	f.Add("x\\]y\\|z")
	f.Add("<img src=x onerror=alert(1)>&")
	f.Add("[label](https://evil.example)")
	f.Add("&#124; pre-encoded entity")
	f.Add("a\x1b[2Jb")
	f.Add("a\u009bb")
	f.Add("x\u202Ey")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		got := escapeCell(s)
		if strings.ContainsAny(got, "|[]\\<>\n\r") {
			t.Errorf("escapeCell(%q) = %q, contains a raw Markdown/HTML metacharacter", s, got)
		}
		for _, r := range got {
			if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) ||
				(r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
				t.Errorf("escapeCell(%q) = %q, contains control/bidi rune %U", s, got, r)
			}
		}
	})
}

// FuzzMdLink fuzzes the Markdown link builder over arbitrary labels and
// destinations (tracker URLs are untrusted upstream data). Invariants: the
// output never contains a raw pipe, angle bracket, or line break (table and
// HTML safety); when a link is emitted its destination carries an http/https
// scheme and no character that could close or re-open the ](...) syntax; when
// no link is emitted the output is exactly the escaped label, so an active
// javascript:/data: link can never survive. The destination also never carries
// a raw C1 control, bidi override/isolate, or U+2028/U+2029 rune (terminal
// escape / visual reordering smuggling through the link destination).
func FuzzMdLink(f *testing.F) {
	f.Add("nyaa", "https://nyaa.si/view/1")
	f.Add("label", "javascript:alert(1)")
	f.Add("label", "data:text/html,<script>")
	f.Add("ab", "/torrents.php?id=1")
	f.Add("x|y", "https://x/a b(c)|d\ne")
	f.Add("", "")
	f.Add("]([evil](x))", "HTTPS://UPPER.example/path")
	f.Add("t", " https://leading.space/ok ")
	f.Add("t", "https://x.example/a\u202eb")
	f.Add("t", "https://x.example/a\u0085b")
	f.Fuzz(func(t *testing.T, label, rawURL string) {
		got := mdLink(label, rawURL)
		if strings.ContainsAny(got, "|<>\n\r") {
			t.Errorf("mdLink(%q, %q) = %q, contains a raw pipe/angle/line break", label, rawURL, got)
		}
		// escapeCell strips every raw ] from the label, so the first "](" in the
		// output can only be mdLink's own link syntax.
		idx := strings.Index(got, "](")
		if idx < 0 {
			if got != escapeCell(label) {
				t.Errorf("mdLink(%q, %q) = %q, want plain escaped label %q", label, rawURL, got, escapeCell(label))
			}
			return
		}
		dest := got[idx+2 : len(got)-1]
		if strings.ContainsAny(dest, " \t\v\f\n\r()<>|") {
			t.Errorf("mdLink(%q, %q) destination %q contains a raw URL metacharacter", label, rawURL, dest)
		}
		for _, r := range dest {
			if (r >= 0x80 && r <= 0x9f) || (r >= 0x202a && r <= 0x202e) ||
				(r >= 0x2066 && r <= 0x2069) || r == 0x2028 || r == 0x2029 {
				t.Errorf("mdLink(%q, %q) destination %q contains raw control/bidi rune %U", label, rawURL, dest, r)
			}
		}
		lower := strings.ToLower(dest)
		if !strings.HasPrefix(lower, "http:") && !strings.HasPrefix(lower, "https:") {
			t.Errorf("mdLink(%q, %q) emitted a non-http link destination %q", label, rawURL, dest)
		}
	})
}
