// Package logattr renders untrusted strings as bounded, sanitized slog attribute
// values, and is the ONE home for the app's structured-attribute volume policy.
// Sanitization keeps CR/LF, which a JSON sink needs. Capping happens BEFORE
// sanitizing throughout, so the budget bounds the WORK and not just the output.
package logattr

import (
	"strings"

	"github.com/cplieger/runesafe/v2"
)

// MaxBytes is the per-attribute volume budget every untrusted value is rendered
// under. It mirrors keyenc.MaxComponentBytes, the bound the dedupe-key path
// already applies to the same SeaDex data, so one hostile entry cannot amplify
// memory in the 256 MiB container.
const MaxBytes = 8 << 10

// TruncMarker is the suffix a truncated value carries, so a reader can tell a cut
// value from an honest one.
const TruncMarker = "..."

// Cap renders one untrusted single-value attribute: an honest value passes
// byte-identical, an oversized one is cut on a rune boundary and marked.
func Cap(s string) string {
	j := NewJoiner()
	j.Write(s)
	return j.String()
}

// linkDestEscaper backs EscapeLinkDestination; built once, safe for concurrent use.
var linkDestEscaper = strings.NewReplacer(
	" ", "%20", "\t", "%09", "\\", "%5C", "`", "%60", `"`, "%22", "'", "%27",
	"\v", "%0B", "\f", "%0C", "(", "%28", ")", "%29", "<", "%3C", ">", "%3E",
	"|", "%7C", "\n", "%0A", "\r", "%0D",
)

// EscapeLinkDestination percent-encodes the ASCII characters an untrusted value
// must not carry into a Markdown link destination: the CommonMark inline
// metacharacters still active inside a destination, both quotes (attribute-context
// defense for a downstream MD-to-HTML conversion), the pipe (table-cell break),
// and every ASCII whitespace form.
func EscapeLinkDestination(s string) string { return linkDestEscaper.Replace(s) }

// Joiner renders a multi-source attribute under Cap's byte budget WITHOUT first
// materializing the untrusted aggregate: the joined-then-capped shape allocated
// the full aggregate, a plausible OOM kill of the container. Honest values stay
// byte-identical to that form, because runesafe.Sanitize is a per-rune map.
type Joiner struct {
	b *runesafe.Budget
	// dropped records a unit WritePair refused whole. Nothing was written, so
	// runesafe.Budget's own cut stays unlatched and cannot carry the fact.
	dropped bool
}

// NewJoiner returns a joiner with the full per-attribute budget. The marker is
// charged OUTSIDE the budget (see String), which is this package's own contract.
func NewJoiner() *Joiner { return &Joiner{b: runesafe.NewBudget(MaxBytes, "")} }

// Write appends the sanitized prefix of raw that still fits the budget and reports
// whether the joiner can still accept more. The cap runs first, so the sanitizer
// never walks an unbounded string; sanitizing can GROW a string (each invalid
// UTF-8 byte becomes the three-byte U+FFFD), so the result is re-capped.
func (j *Joiner) Write(raw string) bool { return j.b.Write(raw) }

// WriteSep appends a fixed ASCII separator (never untrusted data) against the same
// budget, so a hostile piece count cannot grow the attribute past it either.
func (j *Joiner) WriteSep(sep string) bool { return j.Write(sep) }

// WritePair appends left+sep+right as ONE unit: the triple lands whole or not at
// all. Charged piece by piece the budget can run out mid-triple, and a reader
// splitting the attribute on sep then sees a key standing where a value belongs.
// The refusal marks the aggregate truncated, so the elision stays visible.
//
// The fit is measured on the SANITIZED pieces under the remaining budget: a
// raw-byte estimate would admit a pair the growth Write documents then cuts.
func (j *Joiner) WritePair(left, sep, right string) bool {
	room := j.remaining()
	cleanLeft, leftCut := runesafe.SanitizeBudgeted(left, room, "")
	cleanRight, rightCut := runesafe.SanitizeBudgeted(right, room, "")
	if leftCut || rightCut || len(cleanLeft)+len(sep)+len(cleanRight) > room {
		j.dropped = true
		return false
	}
	// Sanitizing is idempotent and each piece already fits, so these cannot cut.
	return j.Write(cleanLeft) && j.WriteSep(sep) && j.Write(cleanRight)
}

// remaining reports the bytes the budget still accepts. Result reads the budget
// without spending it, so the difference from MaxBytes is its own remainder.
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
