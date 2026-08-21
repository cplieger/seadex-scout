package seadex

import "testing"

// TestEntryHasTheoreticalBest pins the theoretical-best predicate both
// consumers branch on (compare's theoretical_best info finding and audit's
// theoretical qualifier): a named theoretical best reports true, empty and
// whitespace-only false (untrusted PocketBase text names nothing).
func TestEntryHasTheoreticalBest(t *testing.T) {
	if (&Entry{}).HasTheoreticalBest() {
		t.Error("HasTheoreticalBest() = true for empty TheoreticalBest, want false")
	}
	if (&Entry{TheoreticalBest: " \t "}).HasTheoreticalBest() {
		t.Error("HasTheoreticalBest() = true for whitespace-only TheoreticalBest, want false")
	}
	if !(&Entry{TheoreticalBest: "a stated remux"}).HasTheoreticalBest() {
		t.Error("HasTheoreticalBest() = false with TheoreticalBest set, want true")
	}
}

// TestValidInfoHash pins the info-hash sanitizer at its home in the seadex
// package (the releases.moe contract: 40-char SHA-1 hex, lowercased, trimmed;
// anything else - the <redacted> placeholder, a wrong length, a non-hex byte -
// drops to ""). The indexer's TestValidInfoHash covers only its thin delegate.
func TestValidInfoHash(t *testing.T) {
	const valid = "143ed15e5e3df072ae91adaeb149973a887590dd"
	tests := []struct{ name, in, want string }{
		{name: "valid lowercase passes through", in: valid, want: valid},
		{name: "uppercase is lowercased", in: "143ED15E5E3DF072AE91ADAEB149973A887590DD", want: valid},
		{name: "surrounding whitespace trimmed", in: "  " + valid + "\t", want: valid},
		{name: "redacted placeholder drops", in: "<redacted>", want: ""},
		{name: "too short drops", in: valid[:39], want: ""},
		{name: "too long drops", in: valid + "0", want: ""},
		{name: "non-hex byte drops", in: valid[:39] + "g", want: ""},
		{name: "hex-adjacent uppercase G drops", in: valid[:39] + "G", want: ""},
		{name: "empty drops", in: "", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidInfoHash(tc.in); got != tc.want {
				t.Errorf("ValidInfoHash(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
