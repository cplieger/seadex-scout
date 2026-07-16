package audit

import (
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestEscapeCellPropertyBoundedOutput is the per-PR randomized twin of
// FuzzEscapeCell: for any input, the escaped cell never contains a raw
// Markdown table/link metacharacter, a line break, a C0/DEL/C1 control rune,
// or a Unicode bidi override/isolate rune, so a crafted title cannot break out
// of its table cell, smuggle raw HTML, or reorder the rendered text.
func TestEscapeCellPropertyBoundedOutput(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.String().Draw(t, "s")
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

// TestMdLinkPropertyOnlyHTTPLinks is the per-PR randomized twin of FuzzMdLink:
// for any label and destination, the output carries no raw pipe/angle/line
// break; when a link is emitted its destination is http/https with no
// metacharacter that could close the ](...) syntax and no raw C1 control,
// bidi override/isolate, or U+2028/U+2029 rune; otherwise the output is
// exactly the escaped label, so an active javascript:/data: link never
// survives.
func TestMdLinkPropertyOnlyHTTPLinks(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		label := rapid.String().Draw(t, "label")
		rawURL := rapid.String().Draw(t, "url")
		got := mdLink(label, rawURL)
		if strings.ContainsAny(got, "|<>\n\r") {
			t.Errorf("mdLink(%q, %q) = %q, contains a raw pipe/angle/line break", label, rawURL, got)
		}
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
