// Package textsafe classifies runes unsafe in untrusted text bound for
// logs, JSON, or rendered output.
package textsafe

import "strings"

// IsBidiControl reports whether r is one of Unicode's Bidi_Control format
// characters: the singleton marks U+061C (ALM) and U+200E/U+200F (LRM/RLM),
// the override/embedding range U+202A-U+202E (LRE/RLE/PDF/LRO/RLO), and the
// isolate range U+2066-U+2069 (LRI/RLI/FSI/PDI). Any of them in untrusted
// text can visually reorder rendered output (report/link spoofing), so every
// output sanitizer treats the full set as unsafe.
func IsBidiControl(r rune) bool {
	return r == '\u061c' || r == '\u200e' || r == '\u200f' ||
		(r >= '\u202a' && r <= '\u202e') ||
		(r >= '\u2066' && r <= '\u2069')
}

// IsUnsafe reports whether r is unsafe in untrusted text bound for a log,
// JSON, or rendered-output sink: a C0 control (CR and LF excepted when
// keepCRLF is true, for sinks whose encoder escapes them), DEL, a C1 control
// (U+0080-U+009F, single-rune terminal-escape introducers), a Unicode bidi
// control (IsBidiControl), or the U+2028/U+2029 line separators.
func IsUnsafe(r rune, keepCRLF bool) bool {
	switch {
	case r < 0x20:
		return !keepCRLF || (r != '\n' && r != '\r')
	case r == 0x7f:
		return true
	case r >= 0x80 && r <= 0x9f:
		return true
	case IsBidiControl(r) || r == '\u2028' || r == '\u2029':
		return true
	}
	return false
}

// SanitizeLogText makes an untrusted string safe for the slog/JSON sinks: C0
// controls (except CR/LF, which JSON encoders escape), DEL, C1 controls
// (U+0080-U+009F, single-rune terminal-escape introducers emitted raw by
// slog's JSONHandler), Unicode bidi controls, and the U+2028/U+2029 line
// separators are each replaced with a space. One policy shared by the audit
// report's renderers, the daemon finding emitter, and the anilist
// upstream-message sanitizer.
func SanitizeLogText(s string) string {
	return strings.Map(func(r rune) rune {
		if IsUnsafe(r, true) {
			return ' '
		}
		return r
	}, s)
}
