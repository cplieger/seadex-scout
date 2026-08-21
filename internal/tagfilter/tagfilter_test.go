package tagfilter

import (
	"slices"
	"testing"
)

// TestZeroFilterExcludesNothing pins the default: an absent config yields the
// zero Filter, and the zero Filter filters nothing on any surface - including
// the tags SeaDex curators use to warn against a release.
func TestZeroFilterExcludesNothing(t *testing.T) {
	var f Filter
	for _, s := range surfaceOrder {
		if f.Excludes([]string{"Broken", "Incomplete"}, s) {
			t.Errorf("zero Filter excluded a warned release from %s", s)
		}
	}
	// New over an empty map must be indistinguishable from the zero value, so a
	// `exclude_tags: {}` config and an absent section behave identically.
	if empty := New(nil); empty.Excludes([]string{"broken"}, SurfaceFeed) {
		t.Error("New(nil) excluded a tag")
	}
	if empty := New(map[string][]Surface{}); empty.Excludes([]string{"broken"}, SurfaceFeed) {
		t.Error("New(empty map) excluded a tag")
	}
}

// TestExcludesMatchesExactlyAndCaseInsensitively pins the matching rule: exact
// and case-insensitive on both sides, never substring, so a stray upstream tag
// that merely contains a configured tag cannot trip the gate.
func TestExcludesMatchesExactlyAndCaseInsensitively(t *testing.T) {
	f := New(map[string][]Surface{"BrOkEn": {SurfaceFindings}})
	tests := []struct {
		name string
		tags []string
		want bool
	}{
		{"exact lowercase", []string{"broken"}, true},
		{"upstream mixed case", []string{"BROKEN"}, true},
		{"surrounding whitespace", []string{"  broken "}, true},
		{"one tag among several", []string{"dual-audio", "Broken"}, true},
		{"substring near-miss suffix", []string{"brokenish"}, false},
		{"substring near-miss prefix", []string{"notbroken"}, false},
		{"substring near-miss inner", []string{"broken-ish"}, false},
		{"unrelated tag", []string{"incomplete"}, false},
		{"no tags", nil, false},
		{"blank tag", []string{"", "   "}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := f.Excludes(tc.tags, SurfaceFindings); got != tc.want {
				t.Errorf("Excludes(%q) = %v, want %v", tc.tags, got, tc.want)
			}
		})
	}
}

// TestExcludesIsPerSurface pins per-surface independence: a tag filtered on the
// feed alone leaves the findings and the report untouched. This is the property
// the whole config shape exists for.
func TestExcludesIsPerSurface(t *testing.T) {
	f := New(map[string][]Surface{
		"broken":     {SurfaceFeed},
		"incomplete": {SurfaceFindings, SurfaceReport},
	})
	tests := []struct {
		tag     string
		surface Surface
		want    bool
	}{
		{"broken", SurfaceFeed, true},
		{"broken", SurfaceFindings, false},
		{"broken", SurfaceReport, false},
		{"incomplete", SurfaceFindings, true},
		{"incomplete", SurfaceReport, true},
		{"incomplete", SurfaceFeed, false},
	}
	for _, tc := range tests {
		if got := f.Excludes([]string{tc.tag}, tc.surface); got != tc.want {
			t.Errorf("Excludes(%q, %s) = %v, want %v", tc.tag, tc.surface, got, tc.want)
		}
	}
}

// TestNewUnionsCaseVariantTagKeys pins that two case spellings of one tag
// combine rather than one silently overwriting the other's surfaces.
func TestNewUnionsCaseVariantTagKeys(t *testing.T) {
	f := New(map[string][]Surface{
		"broken": {SurfaceFeed},
		"Broken": {SurfaceFindings},
	})
	for _, s := range []Surface{SurfaceFeed, SurfaceFindings} {
		if !f.Excludes([]string{"broken"}, s) {
			t.Errorf("union lost the %s surface", s)
		}
	}
	if f.Excludes([]string{"broken"}, SurfaceReport) {
		t.Error("union invented the report surface")
	}
}

// TestNewIgnoresUnusableEntries covers the inputs the config package rejects
// before New ever sees them: New must still not build a policy that could match
// a release's blank tag or answer for a non-surface value.
func TestNewIgnoresUnusableEntries(t *testing.T) {
	f := New(map[string][]Surface{
		"  ":    {SurfaceFeed},
		"tag":   {surfaceNone, Surface(99)},
		"other": nil,
	})
	if f.Excludes([]string{""}, SurfaceFeed) || f.Excludes([]string{"   "}, SurfaceFeed) {
		t.Error("a blank tag key produced a policy matching a blank release tag")
	}
	for _, s := range surfaceOrder {
		if f.Excludes([]string{"tag", "other"}, s) {
			t.Errorf("a non-surface value produced an exclusion on %s", s)
		}
	}
}

// TestParseSurface pins the config-file vocabulary: the three spellings,
// case-insensitive and trimmed, and nothing else.
func TestParseSurface(t *testing.T) {
	valid := map[string]Surface{
		"findings": SurfaceFindings,
		"REPORT":   SurfaceReport,
		"  feed ":  SurfaceFeed,
	}
	for name, want := range valid {
		got, ok := ParseSurface(name)
		if !ok || got != want {
			t.Errorf("ParseSurface(%q) = %v, %v; want %v, true", name, got, ok, want)
		}
	}
	for _, name := range []string{"", "  ", "alerts", "findings,report", "feeds", "invalid"} {
		if got, ok := ParseSurface(name); ok {
			t.Errorf("ParseSurface(%q) = %v, true; want not ok", name, got)
		}
	}
}

// TestSurfaceNames pins the valid set a config error names, in canonical order.
func TestSurfaceNames(t *testing.T) {
	want := []string{"findings", "report", "feed"}
	if got := SurfaceNames(); !slices.Equal(got, want) {
		t.Errorf("SurfaceNames() = %v, want %v", got, want)
	}
	if got := surfaceNone.String(); got != "invalid" {
		t.Errorf("surfaceNone.String() = %q, want %q", got, "invalid")
	}
}
