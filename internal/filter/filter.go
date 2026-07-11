// Package filter decides which SeaDex candidate releases an operator could use.
// It separates the content filters (remux policy, dual-audio) from tracker
// obtainability: a recommended release must both pass the content filters
// (KeepNonTracker) and sit on an obtainable tracker (Obtainable) - any public
// tracker, or AnimeBytes when the operator has enabled it. A release on a
// tracker the operator cannot use is simply absent, never flagged. Arr-side tag
// include/exclude happens earlier, in the library walk.
package filter

import (
	"github.com/cplieger/seadex-scout/internal/release"
)

// Options are the operator's release filters. A zero Options keeps everything:
// ExcludeRemux and RequireDualAudio default false, and AnimeBytes off only
// hides the one private tracker.
type Options struct {
	// ExcludeRemux drops releases classified remux when true. Default false, so
	// remuxes (often the best release) are kept unless the operator opts out.
	ExcludeRemux bool
	// RequireDualAudio drops releases that are not dual-audio when true.
	RequireDualAudio bool
	// AnimeBytes includes AnimeBytes (private tracker) releases; the public
	// trackers are always included. Off means AnimeBytes releases are invisible.
	AnimeBytes bool
}

// Dropped is a release excluded by a content filter, with the reason for logging.
type Dropped struct {
	Reason  string
	Release release.Release
}

// KeepNonTracker reports whether a release passes the content filters (remux
// policy, dual-audio), ignoring the tracker, and the drop reason otherwise.
// Tracker obtainability is applied separately via Obtainable. An unknown-kind
// release is never dropped by the remux policy.
func KeepNonTracker(r *release.Release, opts Options) (keep bool, reason string) {
	if r.Kind == release.KindRemux && opts.ExcludeRemux {
		return false, "remux excluded (exclude_remux is true)"
	}
	if opts.RequireDualAudio && !r.DualAudio {
		return false, "not dual-audio"
	}
	return true, ""
}

// Obtainable reports whether the operator could actually get this release: any
// public tracker (Nyaa, AnimeTosho, RuTracker) is always obtainable; AnimeBytes
// is obtainable only when the operator enables it (they have an account). Every
// other tracker (rare on SeaDex, and any unrecognized one) is treated as not
// obtainable, so a release the operator cannot grab never becomes a finding.
func Obtainable(r *release.Release, opts Options) bool {
	switch r.TrackerType {
	case release.TrackerPublic:
		return true
	case release.TrackerPrivate:
		return opts.AnimeBytes && release.IsAnimeBytes(r.Tracker)
	default:
		return false
	}
}
