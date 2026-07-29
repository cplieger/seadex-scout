// Package tagfilter owns the operator's SeaDex-tag exclusion policy: which
// tags remove a release from which recommendation surface. It is the ONE home
// of that decision, so the daemon's findings, the audit report, and the
// Torznab feed ask the same question instead of each applying its own
// hardcoded rule.
//
// The policy comes from the filters.exclude_tags config map and is empty by
// default: a zero Filter excludes nothing anywhere, so an absent config and a
// test literal behave identically. Nothing here knows the SeaDex tag
// vocabulary - any tag the operator names is filterable - and nothing here
// decides DISPLAY: release.CurationWarnings still annotates a warned release
// with its fixed broken/incomplete vocabulary whether or not this policy
// filters it.
//
// Matching is exact and case-insensitive, never substring, so a stray upstream
// tag that merely contains a configured tag cannot trip a gate.
package tagfilter

import (
	"maps"
	"slices"
	"strings"
)

// Surface is one recommendation surface a tag can be excluded from. The zero
// value is deliberately not a surface, so an unset field cannot silently mean
// "findings"; use ParseSurface or the exported constants.
type Surface int

const (
	surfaceNone Surface = iota
	// SurfaceFindings is the daemon's compare pass: the `better release
	// available` alerts shipped to Loki.
	SurfaceFindings
	// SurfaceReport is the on-demand audit report's best/alt classification.
	SurfaceReport
	// SurfaceFeed is the Torznab surface: the search curation set and the
	// synthesized RSS journal.
	SurfaceFeed
)

// surfaceOrder is the canonical surface order, and surfaceNames their wire
// (config-file) spellings. Both are the vocabulary config validation reports
// to the operator, so they live beside the constants rather than in the config
// package.
var (
	surfaceOrder = [...]Surface{SurfaceFindings, SurfaceReport, SurfaceFeed}
	surfaceNames = map[Surface]string{
		SurfaceFindings: "findings",
		SurfaceReport:   "report",
		SurfaceFeed:     "feed",
	}
)

// String returns the surface's config-file spelling, or "invalid" for a value
// that is not one of the three surfaces.
func (s Surface) String() string {
	if name, ok := surfaceNames[s]; ok {
		return name
	}
	return "invalid"
}

// SurfaceNames returns the valid surface spellings in canonical order. It is
// the valid set a configuration error names, so the vocabulary is stated once.
func SurfaceNames() []string {
	out := make([]string, 0, len(surfaceOrder))
	for _, s := range surfaceOrder {
		out = append(out, s.String())
	}
	return out
}

// ParseSurface resolves a config-file surface spelling (trimmed,
// case-insensitive) to its Surface. ok is false for anything else, which the
// config package turns into a startup error rather than a silent no-op.
func ParseSurface(name string) (s Surface, ok bool) {
	want := canonical(name)
	if want == "" {
		return surfaceNone, false
	}
	for _, candidate := range surfaceOrder {
		if candidate.String() == want {
			return candidate, true
		}
	}
	return surfaceNone, false
}

// key is one (tag, surface) exclusion. Flattening the policy into a single set
// keeps Excludes a single map lookup per tag and makes per-surface
// independence structural: excluding a tag on one surface says nothing about
// the others.
type key struct {
	tag     string
	surface Surface
}

// Filter is the exclusion policy. The zero value is valid and excludes
// nothing, which is both the default configuration and the behaviour every
// consumer gets when no policy is threaded through.
type Filter struct {
	excluded map[key]struct{}
}

// New builds a Filter from a per-tag surface list, as config.filters.exclude_tags
// spells it. Tag keys are canonicalized (trimmed, lowercased) and their surface
// lists unioned, so two case variants of one tag combine rather than one
// overwriting the other. A blank tag key, an unknown surface, and a tag with no
// surfaces are the config package's to reject; New simply ignores a blank key
// and a non-surface value so no input can produce a policy that matches a
// release's blank tag. An empty or nil map yields the zero Filter.
func New(bySurface map[string][]Surface) Filter {
	var excluded map[key]struct{}
	for _, tag := range slices.Sorted(maps.Keys(bySurface)) {
		canon := canonical(tag)
		if canon == "" {
			continue
		}
		for _, s := range bySurface[tag] {
			if _, ok := surfaceNames[s]; !ok {
				continue
			}
			if excluded == nil {
				excluded = make(map[key]struct{})
			}
			excluded[key{tag: canon, surface: s}] = struct{}{}
		}
	}
	return Filter{excluded: excluded}
}

// Excludes reports whether a release carrying these SeaDex tags is excluded
// from surface s. It is false for every surface when the policy is empty (the
// default), and matching is exact and case-insensitive per tag - a tag that
// merely contains a configured tag does not match.
func (f Filter) Excludes(tags []string, s Surface) bool {
	if len(f.excluded) == 0 {
		return false
	}
	for _, tag := range tags {
		canon := canonical(tag)
		if canon == "" {
			continue
		}
		if _, ok := f.excluded[key{tag: canon, surface: s}]; ok {
			return true
		}
	}
	return false
}

// canonical is the one normalization both sides of a match go through:
// trimmed and lowercased, so matching is exact and case-insensitive.
func canonical(tag string) string {
	return strings.ToLower(strings.TrimSpace(tag))
}
