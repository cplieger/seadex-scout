// Package keyenc provides a bounded, injective encoding of untrusted string
// components into a single key string. It is the shared primitive under two
// key builders that must not collide on hostile upstream data: notify's
// persisted finding dedupe keys and compare's headline tie-break key.
//
// Injectivity: the characters that participate in the key grammar (the '|'
// field and ',' list delimiters plus the '\' escape itself) are escaped
// element-wise, so a component containing a delimiter cannot collide two
// distinct component sets, while a delimiter-free component keeps its legacy
// byte-identical representation (persisted keys from earlier versions stay
// valid).
//
// Bounding (CWE-400): a component set whose raw size exceeds
// MaxComponentBytes is reduced to a fixed-size SHA-256 identity instead of
// being materialized into an ever larger key string, so hostile bulk upstream
// data (hundreds of oversized URLs per entry) cannot amplify key construction
// into an out-of-memory failure. The hashed identity is length-prefix
// encoded, so element boundaries survive hashing, and the raw and hashed
// output domains stay disjoint (a small component literally spelling
// "sha256:<hex>" is routed through the hash too).
package keyenc

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strings"
)

// MaxComponentBytes is the raw-size threshold above which a key component (or
// component set) is reduced to a fixed-size SHA-256 identity instead of an
// escaped join. Honest components run well under this bound, so persisted
// keys keep their legacy escaped representation and remain valid.
const MaxComponentBytes = 8 << 10

// hashedPrefix marks a hashed component identity; the raw encodings exclude
// it so the two output domains cannot collide (a small upstream component
// that literally spells "sha256:<hex>" would otherwise alias the hashed
// identity of a different, oversized component set).
const hashedPrefix = "sha256:"

// partEscaper escapes the characters that participate in the key grammar
// (the '|' field and ',' list delimiters, plus the '\' escape itself, escaped
// first so the mapping stays injective). Escaping only the reserved
// characters keeps every delimiter-free component byte-identical to its
// legacy unescaped form.
var partEscaper = strings.NewReplacer(
	`\`, `\\`,
	",", `\,`,
	"|", `\|`,
)

// escapePart makes an untrusted key component safe to join with the ',' and
// '|' delimiters (see partEscaper).
func escapePart(s string) string { return partEscaper.Replace(s) }

// escapeJoinParts escapes each part with escapePart BEFORE comma-joining, so
// element boundaries survive in the encoding: a part that itself contains a
// comma is escaped while the joining commas stay raw, making ["a,b"] and
// ["a","b"] encode differently. Delimiter-free parts stay byte-identical to
// their naive join.
func escapeJoinParts(parts []string) string {
	escaped := make([]string, len(parts))
	for i, p := range parts {
		escaped[i] = escapePart(p)
	}
	return strings.Join(escaped, ",")
}

// BoundedJoinParts returns the escaped join of parts when the components' raw
// size is within MaxComponentBytes, else the fixed-size hashed identity of
// the component set (see hashKeyParts). The threshold checks the raw sizes so
// an honest set's representation never depends on how many delimiters
// escaping added. An in-bound set whose escaped join itself begins with the
// hashed-identity prefix is routed through the hash too, keeping the raw and
// hashed output domains disjoint (injectivity across the size boundary).
func BoundedJoinParts(parts []string) string {
	total := 0
	for _, p := range parts {
		total += len(p)
	}
	if total <= MaxComponentBytes {
		if joined := escapeJoinParts(parts); !strings.HasPrefix(joined, hashedPrefix) {
			return joined
		}
	}
	return hashKeyParts(parts)
}

// BoundedPart is BoundedJoinParts for a single component: the escaped legacy
// form within the bound, the hashed identity above it (or when the escaped
// form would spell the hashed-identity prefix, keeping the domains disjoint).
func BoundedPart(s string) string { return BoundedJoinParts([]string{s}) }

// hashKeyParts streams each original component into SHA-256 under a
// length-prefixed encoding - element boundaries survive without ever joining
// the inputs into one allocation, so ["a,b"] and ["a","b"] hash differently -
// and returns the fixed-size "sha256:<hex>" identity.
func hashKeyParts(parts []string) string {
	h := sha256.New()
	var lenBuf [8]byte
	for _, p := range parts {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(p)))
		_, _ = h.Write(lenBuf[:])
		_, _ = h.Write([]byte(p))
	}
	return hashedPrefix + hex.EncodeToString(h.Sum(nil))
}
