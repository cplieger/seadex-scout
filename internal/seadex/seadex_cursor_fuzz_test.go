package seadex

import (
	"strings"
	"testing"
)

// unquoteFilterValue is the INVERSE of quoteFilterValue: it reads one
// double-quoted PocketBase filter literal from the head of s, treating "\x" as
// the literal x, and returns the decoded value plus whatever followed the
// literal. It decodes rather than re-implementing the escaper, so a dropped or
// inverted escape rule shows up as a value or a remainder that does not match.
func unquoteFilterValue(t *testing.T, s string) (value, rest string) {
	t.Helper()
	if s == "" || s[0] != '"' {
		t.Fatalf("filter literal %q does not open with a quote", s)
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
			if i == len(s) {
				t.Fatalf("filter literal %q ends inside an escape", s)
			}
			b.WriteByte(s[i])
		case '"':
			return b.String(), s[i+1:]
		default:
			b.WriteByte(s[i])
		}
	}
	t.Fatalf("filter literal %q is unterminated", s)
	return "", ""
}

// FuzzQuoteFilterValueRoundTrips pins the filter-literal escaping that is the
// belt to filterSafe's braces. Because filterSafe refuses a quote and a
// backslash outright, no test ever hands quoteFilterValue a metacharacter, so
// the escaper is unexercised today and a regression in it - a dropped
// backslash rule, an inverted pair - is invisible until someone relaxes
// filterSafe, at which point an upstream-controlled cursor value would close
// the literal early and append its own terms to the outbound PocketBase filter
// expression. The round-trip property holds the escaper to its contract
// independently of the validator in front of it.
func FuzzQuoteFilterValueRoundTrips(f *testing.F) {
	seeds := []string{
		"", "abc", "rec000001", "2026-01-02 03:04:05.000Z",
		`a"b`, `a\b`, `a\"b`, `"`, `\`, `\\`, `""`,
		`")||(1=1)||("`,
		"a\nb", "\u00e9",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		quoted := quoteFilterValue(v)
		got, rest := unquoteFilterValue(t, quoted)
		if got != v {
			t.Errorf("quoteFilterValue(%q) = %q, which decodes to %q: an upstream value can alter the filter expression", v, quoted, got)
		}
		if rest != "" {
			t.Errorf("quoteFilterValue(%q) = %q, whose literal closes early leaving %q as filter syntax", v, quoted, rest)
		}
	})
}
