package seadex

import (
	"strings"
	"testing"
)

// asciiUpper upper-cases only the ASCII letters of s, leaving every other byte
// (including any multi-byte rune) untouched, so the case-invariance property
// below cannot be confounded by a Unicode case mapping that changes length.
func asciiUpper(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}

// FuzzValidInfoHash is the invariant net over the releases.moe info-hash
// sanitizer. Its output is not a display string: the indexer keys its curation
// set on it and matches Prowlarr results by it, so anything ValidInfoHash
// admits becomes a lookup key. Whatever the upstream value, an accepted result
// must therefore be the canonical form - exactly 40 lowercase hex bytes,
// stable under re-validation and under surrounding whitespace - and never a
// value carrying a byte the key format does not allow.
func FuzzValidInfoHash(f *testing.F) {
	const valid = "143ed15e5e3df072ae91adaeb149973a887590dd"
	seeds := []string{
		valid,
		asciiUpper(valid),
		"  " + valid + "\t",
		"<redacted>",
		valid[:39],
		valid + "0",
		valid[:39] + "g",
		"",
		"   ",
		// Non-ASCII runes whose case mapping changes the byte length, so a
		// length check placed on the wrong side of the fold shows up here.
		strings.Repeat("\u017f", 40),
		strings.Repeat("\u212a", 40),
		strings.Repeat("\u0130", 20),
		valid[:20] + "\x00" + valid[21:],
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got := ValidInfoHash(in)
		if got == "" {
			return
		}
		if len(got) != 40 {
			t.Fatalf("ValidInfoHash(%q) = %q (%d bytes), want empty or exactly 40", in, got, len(got))
		}
		for i := range len(got) {
			if c := got[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				t.Fatalf("ValidInfoHash(%q) = %q, which carries the non-hex byte %q at index %d", in, got, c, i)
			}
		}
		if again := ValidInfoHash(got); again != got {
			t.Errorf("ValidInfoHash(%q) = %q, but re-validating that yields %q (the canonical form must be stable)", in, got, again)
		}
		if padded := ValidInfoHash(" \t" + in + "\n"); padded != got {
			t.Errorf("ValidInfoHash(%q) = %q but the whitespace-padded value yields %q", in, got, padded)
		}
	})
}

// FuzzValidInfoHashIsCaseInsensitive pins the ACCEPTANCE half of the contract
// the canonical-form property cannot see: a hex string means the same thing in
// either case, so the case of an upstream value must never decide whether the
// hash is usable. Under-acceptance is the silent failure here - a rejected
// hash costs the indexer its only match key for a private-tracker release,
// with no error anywhere.
func FuzzValidInfoHashIsCaseInsensitive(f *testing.F) {
	const valid = "143ed15e5e3df072ae91adaeb149973a887590dd"
	for _, s := range []string{
		valid,
		asciiUpper(valid),
		valid[:20] + asciiUpper(valid[20:]),
		"<redacted>",
		"",
		"\u017f" + valid[1:],
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		if got, upper := ValidInfoHash(in), ValidInfoHash(asciiUpper(in)); got != upper {
			t.Errorf("ValidInfoHash(%q) = %q but ValidInfoHash(%q) = %q: the info hash is hex, so its case cannot decide acceptance",
				in, got, asciiUpper(in), upper)
		}
	})
}
