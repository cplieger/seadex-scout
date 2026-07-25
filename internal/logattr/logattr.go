// Package logattr renders untrusted strings as bounded, sanitized slog
// attribute values. It is the ONE home for the app's slog-attribute volume
// policy: the per-attribute byte budget, the "..." truncation marker, the
// rune-sanitization pass, and the cap-before-sanitize order that makes the
// budget bound the WORK rather than just the output.
//
// Both slog emitters consume it - the daemon's notification path
// (internal/notify) and the season report's per-row lines (internal/audit) -
// because the primitive is security-sensitive (CWE-400: SeaDex admits up to
// 512 torrents per entry, each with a multi-MB group name or URL) and a
// sanitization, invalid-UTF-8, truncation, or allocation-bound fix must not
// have to be reproduced in two packages.
//
// It is a dependency-free leaf: only runesafe (the shared rune policy) and the
// stdlib.
package logattr

import (
	"strings"

	"github.com/cplieger/runesafe"
)

// MaxBytes is the per-attribute volume budget every untrusted value is
// rendered under. It mirrors keyenc.MaxComponentBytes - the bound the
// dedupe-key path already applies to the same SeaDex data - so one hostile
// entry cannot balloon a Loki record past the pipeline's line limit (which
// would suppress the very finding an alert keys on) or amplify memory in the
// 256 MiB container. It is declared here rather than imported so this package
// stays a dependency-free leaf; keyenc's constant is the sibling value it is
// kept equal to.
const MaxBytes = 8 << 10

// TruncMarker is the suffix a truncated value carries, so a reader can tell a
// cut value from an honest one.
const TruncMarker = "..."

// Cap renders one untrusted SINGLE-value attribute: an honest value passes
// byte-identical (sanitized), an oversized one is cut on a rune boundary and
// marked with TruncMarker.
//
// A MULTI-SOURCE attribute (a joined group or link list) must never be
// materialized and handed to Cap - joining first allocates the whole untrusted
// aggregate before the bound applies. Stream those through a Joiner instead.
func Cap(s string) string {
	j := NewJoiner()
	j.Write(s)
	return j.String()
}

// Joiner renders a multi-source attribute under Cap's byte budget WITHOUT
// first materializing the untrusted aggregate: each piece is capped to the
// remaining budget BEFORE it is sanitized, so a hostile entry can never make
// the caller allocate more than the budget plus one bounded chunk - the
// joined-then-capped shape allocated the full aggregate first, a plausible OOM
// kill of the container. Honest values are byte-identical to the
// joined-then-capped form: runesafe.Sanitize is a per-rune map, so sanitizing
// each piece and writing the ASCII separators raw yields the same bytes.
type Joiner struct {
	b         strings.Builder
	remaining int
	truncated bool
}

// NewJoiner returns a joiner with the full per-attribute budget.
func NewJoiner() *Joiner { return &Joiner{remaining: MaxBytes} }

// Write appends the sanitized prefix of raw that still fits the budget and
// reports whether the joiner can still accept more. The pre-sanitize cap keeps
// the sanitizer from ever walking an unbounded string; sanitizing can grow a
// string (each invalid UTF-8 byte becomes the three-byte U+FFFD), so the
// result is re-capped on a rune boundary.
func (j *Joiner) Write(raw string) bool {
	if j.truncated || j.remaining <= 0 {
		j.truncated = j.truncated || raw != ""
		return false
	}
	chunk := runesafe.CapBytes(raw, j.remaining)
	if len(chunk) < len(raw) {
		j.truncated = true
	}
	clean := runesafe.Sanitize(chunk)
	if len(clean) > j.remaining {
		clean = runesafe.CapBytes(clean, j.remaining)
		j.truncated = true
	}
	j.b.WriteString(clean)
	j.remaining -= len(clean)
	return !j.truncated
}

// WriteSep appends a fixed ASCII separator (never untrusted data) against the
// same budget, so a hostile piece count cannot grow the attribute past it
// either.
func (j *Joiner) WriteSep(sep string) bool { return j.Write(sep) }

// String returns the joined attribute, marked with TruncMarker when any source
// was cut - the same truncation signal a single capped value carries.
func (j *Joiner) String() string {
	if j.truncated {
		return j.b.String() + TruncMarker
	}
	return j.b.String()
}
