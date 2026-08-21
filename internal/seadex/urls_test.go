package seadex

import (
	"testing"
)

// TestEntryURL pins the releases.moe entry-page rule at its home (the seadex
// package owns the SeaDex site-base contract): a positive AniList id yields
// the entry page under DefaultBaseURL, and a zero or negative id yields no
// link at all.
func TestEntryURL(t *testing.T) {
	tests := []struct {
		name string
		want string
		id   int
	}{
		{name: "positive id", id: 154587, want: "https://releases.moe/154587"},
		{name: "single-digit id", id: 1, want: "https://releases.moe/1"},
		{name: "zero id yields no link", id: 0, want: ""},
		{name: "negative id yields no link", id: -3, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := EntryURL(tc.id); got != tc.want {
				t.Errorf("EntryURL(%d) = %q, want %q", tc.id, got, tc.want)
			}
		})
	}
}
