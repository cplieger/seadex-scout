// Package logattr renders untrusted strings as bounded, sanitized slog
// attribute values. It is the ONE home for the app's STRUCTURED-attribute
// volume policy: the MaxBytes per-attribute byte budget, the "..." truncation
// marker, the rune-sanitization pass that keeps CR/LF (the keepCRLF policy a
// JSON sink needs), and the cap-before-sanitize order that makes the budget
// bound the WORK rather than just the output - including the multi-source
// Joiner, which streams a joined aggregate under that bound without
// materializing it first.
package logattr

import (
	"strings"

	"github.com/cplieger/runesafe/v2"
)

// MaxBytes is the per-attribute volume budget every untrusted value is
// rendered under. It mirrors keyenc.MaxComponentBytes - the bound the
// dedupe-key path already applies to the same SeaDex data - so one hostile
// entry cannot amplify memory in the 256 MiB container.
const MaxBytes = 8 << 10

// TruncMarker is the suffix a truncated value carries, so a reader can tell a
// cut value from an honest one.
const TruncMarker = "..."

// Cap renders one untrusted SINGLE-value attribute: an honest value passes
// byte-identical (sanitized), an oversized one is cut on a rune boundary and
// marked with TruncMarker.
func Cap(s string) string {
	j := NewJoiner()
	j.Write(s)
	return j.String()
}

// linkDestEscaper backs EscapeLinkDestination; built once, safe for
// concurrent use.
var linkDestEscaper = strings.NewReplacer(
	" ", "%20", "\t", "%09", "\\", "%5C", "`", "%60", `"`, "%22", "'", "%27",
	"\v", "%0B", "\f", "%0C", "(", "%28", ")", "%29", "<", "%3C", ">", "%3E",
	"|", "%7C", "\n", "%0A", "\r", "%0D",
)

// EscapeLinkDestination percent-encodes the ASCII characters an untrusted
// value must not carry into a Markdown link destination: the CommonMark inline
// metacharacters still active inside a destination, both quotes
// (attribute-context defense for a downstream MD-to-HTML conversion), the pipe
// (table-cell break), and every ASCII whitespace form.
func EscapeLinkDestination(s string) string { return linkDestEscaper.Replace(s) }

// Joiner renders a multi-source attribute under Cap's byte budget WITHOUT
// first materializing the untrusted aggregate: each piece is capped to the
// remaining budget BEFORE it is sanitized, so a hostile entry can never make
// the caller allocate more than the budget plus one bounded chunk - the
// joined-then-capped shape allocated the full aggregate first, a plausible OOM
// kill of the container. Honest values are byte-identical to the
// joined-then-capped form: runesafe.Sanitize is a per-rune map, so sanitizing
// each piece and writing the ASCII separators raw yields the same bytes.
type Joiner struct {
	b *runesafe.Budget
	// dropped records a unit WritePair refused whole. runesafe.Budget cannot
	// carry that fact: nothing was written, so its own cut stays unlatched, and
	// an unmarked aggregate would claim to hold every source.
	dropped bool
}

// NewJoiner returns a joiner with the full per-attribute budget. The
// cap-before-sanitize engine is runesafe.Budget's; the marker is charged
// OUTSIDE the budget here (see String), which is this package's own contract.
func NewJoiner() *Joiner { return &Joiner{b: runesafe.NewBudget(MaxBytes, "")} }

// Write appends the sanitized prefix of raw that still fits the budget and
// reports whether the joiner can still accept more. The pre-sanitize cap keeps
// the sanitizer from ever walking an unbounded string; sanitizing can grow a
// string (each invalid UTF-8 byte becomes the three-byte U+FFFD), so the
// result is re-capped on a rune boundary.
func (j *Joiner) Write(raw string) bool { return j.b.Write(raw) }

// WriteSep appends a fixed ASCII separator (never untrusted data) against the
// same budget, so a hostile piece count cannot grow the attribute past it
// either.
func (j *Joiner) WriteSep(sep string) bool { return j.Write(sep) }

// WritePair appends left+sep+right as ONE unit: the triple lands whole or not at
// all. Charged piece by piece the budget can run out mid-triple, and every state
// that leaves reads as an element that is not a pair - a truncated left, a left
// with no separator, a left+sep with no right - so a reader splitting the
// attribute on sep sees a key standing where a value belongs. Refusing the whole
// unit keeps the invariant that every element rendered IS a pair, and the refusal
// marks the aggregate truncated, so the elision stays visible.
//
// The fit is measured on the SANITIZED pieces, because sanitizing can grow a
// value (each invalid UTF-8 byte becomes the three-byte U+FFFD) and a raw-byte
// estimate would admit a pair the budget then cuts. Each piece is measured under
// the remaining budget, so the work stays bounded by it however long the inputs
// are.
func (j *Joiner) WritePair(left, sep, right string) bool {
	room := j.remaining()
	cleanLeft, leftCut := runesafe.SanitizeBudgeted(left, room, "")
	cleanRight, rightCut := runesafe.SanitizeBudgeted(right, room, "")
	if leftCut || rightCut || len(cleanLeft)+len(sep)+len(cleanRight) > room {
		j.dropped = true
		return false
	}
	// Sanitizing is idempotent and each piece is already inside the budget, so
	// these three writes cannot cut.
	return j.Write(cleanLeft) && j.WriteSep(sep) && j.Write(cleanRight)
}

// remaining reports the bytes the budget still accepts. Result reads the budget
// without spending it and returns exactly the bytes written so far, so the
// difference from MaxBytes is the budget's own remainder; a cut budget accepts
// nothing more.
func (j *Joiner) remaining() int {
	text, cut := j.b.Result()
	if cut {
		return 0
	}
	return MaxBytes - len(text)
}

// String returns the joined attribute, marked with TruncMarker when any source
// was cut or refused - the same truncation signal a single capped value carries.
func (j *Joiner) String() string {
	text, cut := j.b.Result()
	if cut {
		return text + TruncMarker
	}
	if j.dropped {
		return runesafe.CapBytes(text, max(0, MaxBytes-len(TruncMarker))) + TruncMarker
	}
	return text
}
