// Package filter decides which SeaDex candidate releases an operator could use.
// It separates the content filters (remux policy, resolution floor, dual-audio)
// from the tracker allowlist: a release that passes the content filters but sits
// on a non-allowed tracker is still a valid recommendation the operator may want
// flagged (with a "not on your trackers" disclaimer), rather than silently
// dropped. Arr-side tag include/exclude happens earlier, in the library walk.
package filter

import (
	"github.com/cplieger/seadex-scout/internal/release"
)

// Options are the operator's release filters. A zero Options keeps everything
// except that AllowRemux defaults false, so remuxes are dropped unless enabled.
type Options struct {
	// Trackers is the lowercase-keyed allowlist of preferred trackers; empty
	// means all trackers are allowed.
	Trackers map[string]bool
	// MinResolution is the lowest resolution to keep (e.g. "1080p"); empty
	// disables the floor.
	MinResolution string
	// AllowRemux keeps releases classified remux when true.
	AllowRemux bool
	// RequireDualAudio drops releases that are not dual-audio when true.
	RequireDualAudio bool
}

// Dropped is a release excluded by a content filter, with the reason for logging.
type Dropped struct {
	Reason  string
	Release release.Release
}

// KeepNonTracker reports whether a release passes the content filters (remux
// policy, resolution floor, dual-audio), ignoring the tracker allowlist, and
// the drop reason otherwise. The tracker allowlist is applied separately via
// TrackerAllowed. An unknown-kind release is never dropped by the remux policy,
// and a release whose resolution could not be parsed is never dropped by the
// resolution floor.
func KeepNonTracker(r *release.Release, opts Options) (keep bool, reason string) {
	if r.Kind == release.KindRemux && !opts.AllowRemux {
		return false, "remux excluded (FILTER_ALLOW_REMUX=false)"
	}
	if opts.MinResolution != "" && r.Resolution != "" &&
		release.ResolutionRank(r.Resolution) < release.ResolutionRank(opts.MinResolution) {
		return false, "below minimum resolution " + opts.MinResolution
	}
	if opts.RequireDualAudio && !r.DualAudio {
		return false, "not dual-audio"
	}
	return true, ""
}

// TrackerAllowed reports whether the release's tracker is in the allowlist. An
// empty allowlist allows all trackers (no restriction).
func TrackerAllowed(r *release.Release, opts Options) bool {
	if len(opts.Trackers) == 0 {
		return true
	}
	return opts.Trackers[trackerKey(r.Tracker)]
}

// trackerKey lowercases a tracker name for allowlist lookup, matching how the
// config parses FILTER_TRACKERS into a lowercase-keyed set.
func trackerKey(tracker string) string {
	return release.NormalizeGroup(tracker) // lowercase + trim; shared normalizer
}
