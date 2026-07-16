package textsafe_test

import (
	"testing"

	"github.com/cplieger/seadex-scout/internal/textsafe"
)

// TestSanitizeLogText pins the shared unsafe-rune policy for the slog/JSON
// sinks: every C0 control except CR/LF (which JSON encoders escape), DEL, C1
// controls, the Unicode Bidi_Control set, and the U+2028/U+2029 line
// separators become spaces, while plain ASCII and non-control Unicode pass
// through unchanged.
func TestSanitizeLogText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"C0 escape introducer", "a\x1b[2Jb", "a [2Jb"},
		{"C0 NUL", "a\x00b", "a b"},
		{"C0 BEL", "a\x07b", "a b"},
		{"tab", "a\tb", "a b"},
		{"LF preserved", "a\nb", "a\nb"},
		{"CR preserved", "a\rb", "a\rb"},
		{"DEL", "a\x7fb", "a b"},
		{"C1 CSI", "a\u009bb", "a b"},
		{"C1 OSC", "a\u009db", "a b"},
		{"C1 range start", "a\u0080b", "a b"},
		{"C1 range end", "a\u009fb", "a b"},
		{"bidi ALM", "a\u061cb", "a b"},
		{"bidi LRM", "a\u200eb", "a b"},
		{"bidi RLO", "a\u202eb", "a b"},
		{"bidi isolate FSI", "a\u2068b", "a b"},
		{"line separator", "a\u2028b", "a b"},
		{"paragraph separator", "a\u2029b", "a b"},
		{"plain ASCII unchanged", "Frieren: Beyond Journey's End", "Frieren: Beyond Journey's End"},
		{"plain unicode unchanged", "葬送のフリーレン", "葬送のフリーレン"},
		{"empty string", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := textsafe.SanitizeLogText(tt.in); got != tt.want {
				t.Errorf("SanitizeLogText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestIsUnsafeCRLFPolicy pins the keepCRLF switch: CR and LF are safe only
// when the sink's encoder escapes them (keepCRLF true); a single-line sink
// (keepCRLF false) treats them as unsafe like every other C0 control, and
// the other rune classes are unsafe under both policies.
func TestIsUnsafeCRLFPolicy(t *testing.T) {
	for _, r := range []rune{'\n', '\r'} {
		if textsafe.IsUnsafe(r, true) {
			t.Errorf("IsUnsafe(%U, keepCRLF=true) = true, want false", r)
		}
		if !textsafe.IsUnsafe(r, false) {
			t.Errorf("IsUnsafe(%U, keepCRLF=false) = false, want true", r)
		}
	}
	for _, r := range []rune{0x00, 0x1b, '\t', 0x7f, '\u009b', '\u202e', '\u2028'} {
		if !textsafe.IsUnsafe(r, true) || !textsafe.IsUnsafe(r, false) {
			t.Errorf("IsUnsafe(%U) = false under a keepCRLF policy, want true under both", r)
		}
	}
	for _, r := range []rune{'a', ' ', 'é', '葬'} {
		if textsafe.IsUnsafe(r, true) || textsafe.IsUnsafe(r, false) {
			t.Errorf("IsUnsafe(%U) = true, want false under both policies", r)
		}
	}
}

// TestIsBidiControl pins the complete Bidi_Control set (the U+061C/U+200E/
// U+200F singleton marks plus the override and isolate ranges) and the
// adjacent near-miss code points that must stay safe.
func TestIsBidiControl(t *testing.T) {
	unsafe := []rune{
		'\u061c', '\u200e', '\u200f',
		'\u202a', '\u202b', '\u202c', '\u202d', '\u202e',
		'\u2066', '\u2067', '\u2068', '\u2069',
	}
	for _, r := range unsafe {
		if !textsafe.IsBidiControl(r) {
			t.Errorf("IsBidiControl(%U) = false, want true", r)
		}
	}
	safe := []rune{'\u061b', '\u061d', '\u200d', '\u2029', '\u2065', '\u206a', 'a'}
	for _, r := range safe {
		if textsafe.IsBidiControl(r) {
			t.Errorf("IsBidiControl(%U) = true, want false", r)
		}
	}
}
