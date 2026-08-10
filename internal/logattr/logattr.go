// Package logattr renders untrusted strings as bounded, sanitized slog
// attribute values. It is the ONE home for the app's STRUCTURED-attribute
// volume policy: the MaxBytes per-attribute byte budget, the "..." truncation
// marker, the rune-sanitization pass that keeps CR/LF (the keepCRLF policy a
// JSON sink needs), and the cap-before-sanitize order that makes the budget
// bound the WORK rather than just the output - including the multi-source
// Joiner, which streams a joined aggregate under that bound without
// materializing it first.
//
// Every slog emitter of upstream-derived text consumes it - the daemon's
// notification path (internal/notify), the season report's per-row lines
// (internal/audit), the matcher's ambiguous-title-fallback line
// (internal/match) and the walker's per-series failure warning
// (internal/arrwalk) - because the primitive is security-sensitive (CWE-400:
// SeaDex admits up to 512 torrents per entry, each with a multi-MB group name
// or URL) and a sanitization, invalid-UTF-8, truncation, or allocation-bound
// fix must not have to be reproduced in four packages.
//
// It is NOT the home for the SINGLE-LINE bounded preset. A value that lands
// inline in one message string (internal/indexer's capLogText over Torznab
// query params and upstream <error> text, internal/anilist's
// sanitizeUpstreamMessage over a GraphQL error message) must also lose CR/LF,
// and it takes a per-site byte budget rather than MaxBytes; that composition
// lives in the shared runesafe library
// (runesafe.SanitizeSingleLineBounded), which is where a fix to it belongs,
// and both app-side helpers are one-line delegates to it. The ORDER difference
// between the two homes is deliberate on both sides, not drift: runesafe's
// preset caps the SANITIZED form because its cap must survive
// sanitization-growth for a value already known to be small, while this
// package caps first because a multi-MB SeaDex attribute must never walk the
// sanitizer at all. Runner-up, if the two are ever unified: grow a
// Line(s string, maxBytes int) here carrying this package's order under the
// strict single-line policy, and reduce those two helpers to call sites of it -
// rejected for now because it hand-rolls a second composition beside the
// library preset and changes what both log sites emit for an oversized value.
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
// entry cannot amplify memory in the 256 MiB container.
//
// The bound is PER ATTRIBUTE, not per record: a record's worst case is its
// untrusted-attribute count times this budget plus one TruncMarker each
// (notify.findingKVs emits 17 such attributes - 7 capAttr, 4
// capAlertTextAttr, 4 capURLAttr and 2 full-budget Joiners - so ~139 KiB of
// attribute VALUES), and the JSON sink can double that on the wire -
// Sanitize keeps CR and LF (the keepCRLF policy a JSON encoder needs) and
// slog's appendEscapedJSONString expands each CR, LF, '"' and '\\' into two
// bytes, so a control-dense value emits at up to twice its capped size
// (~278 KiB per record). Adding an untrusted attribute therefore raises the record ceiling,
// and past the log pipeline's line limit the WHOLE record is dropped -
// suppressing the very finding an alert keys on. Check the record budget when
// adding one.
//
// It is declared here rather than imported so this package stays a
// dependency-free leaf; keyenc's constant is the sibling value it is kept
// equal to (pinned by TestMaxBytesMirrorsKeyencBudget).
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
// (table-cell break), and every ASCII whitespace form. It is the ONE home for
// this decision, shared by the daemon's alert attributes (internal/notify) and
// the report's Markdown links (internal/audit), so a newly-hostile character is
// added once.
//
// '[' and ']' are deliberately NOT encoded: they are not destination
// delimiters, and they are required syntax around an IPv6-literal host, so
// encoding them would break a legitimate arr deep link
// (http://[fd00::1]:8989/...).
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
//
// Truncation is judged on the RAW length, which diverges from a
// sanitize-then-cap composition for ONE input class: sanitizing can also SHRINK
// (strings.Map replaces each unsafe rune with a single-byte space, so a 2-byte
// C1 or 3-byte bidi control collapses), so a value over the raw budget whose
// SANITIZED form would have fitted emits cut and marked here where the other
// order emitted it whole and unmarked. That is deliberate on both counts. The
// pre-sanitize cap is the point of this package - the budget must bound the
// WORK, not merely the output, or one hostile multi-MB SeaDex value walks the
// sanitizer in a 256 MiB container (CWE-400) - and the marker stays honest
// because bytes really were dropped before sanitizing. Reaching the divergence
// needs an oversized value that is mostly control/bidi runes, i.e. exactly the
// hostile shape the bound exists for; an honest value (valid UTF-8, no unsafe
// runes) is byte-identical under either order. Both sides are pinned by test.
func (j *Joiner) Write(raw string) bool { return j.b.Write(raw) }

// WriteSep appends a fixed ASCII separator (never untrusted data) against the
// same budget, so a hostile piece count cannot grow the attribute past it
// either.
func (j *Joiner) WriteSep(sep string) bool { return j.Write(sep) }

// String returns the joined attribute, marked with TruncMarker when any source
// was cut - the same truncation signal a single capped value carries.
func (j *Joiner) String() string {
	text, cut := j.b.Result()
	if cut {
		return text + TruncMarker
	}
	return text
}
