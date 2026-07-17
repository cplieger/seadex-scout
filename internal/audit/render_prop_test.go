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

// TestSanitizeDisplayTextPropertyBoundedAndIdempotent is the per-PR randomized
// net for the JSON/slog sanitizer, mirroring the escapeCell property: for any
// input the output carries no C0 control other than CR/LF, no DEL, no C1
// control, no Unicode bidi control (ranges hardcoded independently of the
// production textsafe.IsBidiControl classifier), and no U+2028/U+2029 separator; and
// sanitizing is idempotent (it is a normalizer).
func TestSanitizeDisplayTextPropertyBoundedAndIdempotent(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.String().Draw(t, "s")
		got := sanitizeDisplayText(s)
		for _, r := range got {
			if (r < 0x20 && r != '\n' && r != '\r') || r == 0x7f || (r >= 0x80 && r <= 0x9f) ||
				r == '\u061c' || r == '\u200e' || r == '\u200f' ||
				(r >= '\u202a' && r <= '\u202e') || (r >= '\u2066' && r <= '\u2069') ||
				r == '\u2028' || r == '\u2029' {
				t.Errorf("sanitizeDisplayText(%q) = %q, contains unsafe rune %U", s, got, r)
			}
		}
		if again := sanitizeDisplayText(got); again != got {
			t.Errorf("sanitizeDisplayText not idempotent: %q -> %q -> %q", s, got, again)
		}
		if !strings.ContainsFunc(s, isUnsafeForDisplay) && got != s {
			t.Errorf("sanitizeDisplayText(%q) = %q, changed a fully-safe string", s, got)
		}
	})
}

// isUnsafeForDisplay mirrors the property's hardcoded unsafe-rune set for the
// safe-string-unchanged check.
func isUnsafeForDisplay(r rune) bool {
	return (r < 0x20 && r != '\n' && r != '\r') || r == 0x7f || (r >= 0x80 && r <= 0x9f) ||
		r == '\u061c' || r == '\u200e' || r == '\u200f' ||
		(r >= '\u202a' && r <= '\u202e') || (r >= '\u2066' && r <= '\u2069') ||
		r == '\u2028' || r == '\u2029'
}

// TestMdLinkPropertyHTTPDestinationsStayContained complements
// TestMdLinkPropertyOnlyHTTPLinks by constructing a syntactically valid
// http/https URL on every trial (rapid.String almost never produces one, so
// the generic property mostly exercises the plain-label fallback): every
// dangerous destination character is embedded in random surrounding text, so
// the active-link destination escaping branch is exercised on every draw.
func TestMdLinkPropertyHTTPDestinationsStayContained(t *testing.T) {
	plain := rapid.StringOfN(rapid.RuneFrom([]rune("abcXYZ0123456789")), 0, 20, -1)
	const dangerous = " ()<>|\\`\u0085\u202e\u2028\u2029"
	rapid.Check(t, func(t *rapid.T) {
		label := rapid.String().Draw(t, "label")
		scheme := rapid.SampledFrom([]string{"http", "https", "HTTP", "HTTPS"}).Draw(t, "scheme")
		rawURL := scheme + "://example.test/" + plain.Draw(t, "prefix") + dangerous + plain.Draw(t, "suffix")

		got := mdLink(label, rawURL)
		if strings.ContainsAny(got, "|<>\n\r") {
			t.Errorf("mdLink(%q, %q) = %q, contains a raw pipe/angle/line break", label, rawURL, got)
		}
		idx := strings.Index(got, "](")
		if idx < 0 || !strings.HasSuffix(got, ")") {
			t.Fatalf("mdLink(%q, %q) = %q, want an HTTP Markdown link", label, rawURL, got)
		}
		dest := got[idx+2 : len(got)-1]
		if strings.ContainsAny(dest, " \t\v\f\n\r()<>|\\`") {
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
