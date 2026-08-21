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
	"github.com/cplieger/seadex-scout/internal/tracker"
)

// Options are the operator's content filters - exactly the set KeepNonTracker
// consumes. A zero Options keeps everything: ExcludeRemux and RequireDualAudio
// default false. The AnimeBytes tracker toggle is deliberately NOT an Options
// field: obtainability is a separate concern (the package's content-vs-tracker
// split), so consumers pass the toggle explicitly to Obtainable/ABVisible.
type Options struct {
	// ExcludeRemux drops releases classified remux when true. Default false, so
	// remuxes (often the best release) are kept unless the operator opts out.
	ExcludeRemux bool
	// RequireDualAudio drops releases that are not dual-audio when true.
	RequireDualAudio bool
}

// KeepNonTracker reports whether a release passes the content filters (remux
// policy, dual-audio), ignoring the tracker. Tracker obtainability is applied
// separately via Obtainable. An unknown-kind release is never dropped by the
// remux policy.
func KeepNonTracker(r *release.Release, opts Options) bool {
	if r.Kind == release.KindRemux && opts.ExcludeRemux {
		return false
	}
	if opts.RequireDualAudio && !r.DualAudio {
		return false
	}
	return true
}

// Obtainable reports whether the operator could actually get this release: a
// public tracker (Nyaa, AnimeTosho, RuTracker) is obtainable unless the ABVisible
// cross-check hides it (an AnimeBytes-hosted or malformed URL with the toggle
// off); AnimeBytes is obtainable only when the operator enables it.
func Obtainable(r *release.Release, rawURL, usableURL string, animeBytes bool) bool {
	if usableURL == "" {
		return false
	}
	switch r.TrackerType {
	case tracker.Public:
		return ABVisible(r.Tracker, rawURL, animeBytes)
	case tracker.Private:
		return tracker.IsAnimeBytes(r.Tracker) && ABVisible(r.Tracker, rawURL, animeBytes)
	default:
		return false
	}
}

// ABVisible reports whether a release may surface to the operator: the
// animebytes toggle's fail-closed drop rule, and the single home of it.
//
// With the toggle ON everything surfaces.
func ABVisible(trackerName, rawURL string, animeBytes bool) bool {
	return animeBytes || tracker.ClassifyAB(trackerName, rawURL) == tracker.ABNone
}
