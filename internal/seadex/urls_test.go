package seadex

import (
	"testing"

	"github.com/cplieger/urlform"
)

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

// TestUsableRelative pins the relative-publisher helper's own contract,
// independent of urlform.Classify's routing: any value whose first colon
// precedes a slash is unusable as a relative path and drops - including the
// degenerate colon-at-index-0 form no Classify class currently routes here -
// while a colon safely inside a later path segment publishes, and a missing
// leading slash is added exactly once.
func TestUsableRelative(t *testing.T) {
	tests := []struct{ name, raw, want string }{
		{name: "leading colon drops", raw: ":8080/x", want: ""},
		{name: "colon before any slash drops", raw: "a:b/c", want: ""},
		{name: "query-leading colon drops", raw: "?x:y", want: ""},
		{name: "colon after slash publishes", raw: "path/a:b", want: "https://nyaa.si/path/a:b"},
		{name: "leading slash kept", raw: "/view/1", want: "https://nyaa.si/view/1"},
		{name: "missing slash added", raw: "view/1", want: "https://nyaa.si/view/1"},
		{name: "no colon no slash", raw: "view", want: "https://nyaa.si/view"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := usableRelative(tc.raw, "https://nyaa.si"); got != tc.want {
				t.Errorf("usableRelative(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestUsableSchemelessHostPortGate pins the schemeless-host publisher's
// fail-closed port gate directly, the way TestUsableRelative pins the relative
// publisher's own contract: urlform v1.1.0 never routes a ported authority to
// this branch, so neither the gate nor schemelessPortOK behind it is reachable
// through UsableURL. A canonical recovered host publishes canonicalized only
// with a publishable port (absent, or numeric and inside the 16-bit range
// usableAbsolute enforces); an out-of-range port or an unparsable authority
// drops; a non-canonical recovered host keeps the labeled tracker's path
// reading.
func TestUsableSchemelessHostPortGate(t *testing.T) {
	tests := []struct{ name, trimmed, host, want string }{
		{
			name:    "canonical host without a port publishes canonicalized",
			trimmed: "nyaa.si/view/1",
			host:    "nyaa.si",
			want:    "https://nyaa.si/view/1",
		},
		{
			name:    "canonical host with an in-range port publishes canonicalized",
			trimmed: "nyaa.si:8080/view/1",
			host:    "nyaa.si",
			want:    "https://nyaa.si:8080/view/1",
		},
		{
			name:    "canonical host with the maximum port publishes canonicalized",
			trimmed: "nyaa.si:65535/view/1",
			host:    "nyaa.si",
			want:    "https://nyaa.si:65535/view/1",
		},
		{
			name:    "canonical host with an out-of-range port drops",
			trimmed: "nyaa.si:65536/view/1",
			host:    "nyaa.si",
			want:    "",
		},
		{
			name:    "canonical host with an unparsable authority drops",
			trimmed: "nyaa.si:abc/view/1",
			host:    "nyaa.si",
			want:    "",
		},
		{
			name:    "non-canonical recovered host keeps the labeled tracker's path reading",
			trimmed: "evil.example/view/1",
			host:    "evil.example",
			want:    "https://nyaa.si/evil.example/view/1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := urlform.Form{
				Trimmed: tc.trimmed,
				Host:    tc.host,
				Class:   urlform.ClassSchemelessHost,
			}
			if got := usableSchemelessHost(&f, "https://nyaa.si"); got != tc.want {
				t.Errorf("usableSchemelessHost(%q, host %q) = %q, want %q", tc.trimmed, tc.host, got, tc.want)
			}
		})
	}
}
