package seadex

import "testing"

// TestEntryURL pins the releases.moe entry-page rule at its home (the seadex
// package owns the SeaDex site-base contract): a positive AniList id yields
// the entry page under the given base with any trailing slashes trimmed, and
// a zero or negative id yields no link at all.
func TestEntryURL(t *testing.T) {
	tests := []struct {
		name string
		base string
		want string
		id   int
	}{
		{name: "positive id", base: "https://releases.moe", id: 154587, want: "https://releases.moe/154587"},
		{name: "trailing slash trimmed", base: "https://releases.moe/", id: 154587, want: "https://releases.moe/154587"},
		{name: "multiple trailing slashes trimmed", base: "https://releases.moe//", id: 154587, want: "https://releases.moe/154587"},
		{name: "default base constant", base: DefaultBaseURL, id: 1, want: "https://releases.moe/1"},
		{name: "zero id yields no link", base: "https://releases.moe", id: 0, want: ""},
		{name: "negative id yields no link", base: "https://releases.moe", id: -3, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := EntryURL(tc.base, tc.id); got != tc.want {
				t.Errorf("EntryURL(%q, %d) = %q, want %q", tc.base, tc.id, got, tc.want)
			}
		})
	}
}
