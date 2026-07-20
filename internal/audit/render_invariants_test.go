package audit

import "strings"

// failer is the assertion sink shared by the fuzz (*testing.T) and rapid
// (*rapid.T) twins of the render-sanitizer invariants.
type failer interface {
	Errorf(format string, args ...any)
}

// checkEscapedCellInvariants asserts escapeCell's bounded-output contract:
// no raw Markdown/HTML metacharacter, no C0/DEL/C1 control, no bidi
// override/isolate rune.
func checkEscapedCellInvariants(t failer, in, got string) {
	if strings.ContainsAny(got, "|[]\\<>\n\r") {
		t.Errorf("escapeCell(%q) = %q, contains a raw Markdown/HTML metacharacter", in, got)
	}
	for _, r := range got {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) ||
			(r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			t.Errorf("escapeCell(%q) = %q, contains control/bidi rune %U", in, got, r)
		}
	}
}

// checkMdLinkDestinationInvariants asserts the contained-destination
// contract of an emitted link: no raw URL metacharacter INCLUDING the
// backslash and backtick escapeLinkURL encodes as %5C/%60, no raw C1/bidi/
// separator rune, and an http/https scheme.
func checkMdLinkDestinationInvariants(t failer, label, rawURL, dest string) {
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
}
