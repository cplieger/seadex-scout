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

// Obtainable reports whether the operator could actually get this release: a
// public tracker (Nyaa, AnimeTosho, RuTracker) is obtainable unless the ABVisible
// cross-check hides it (an AnimeBytes-hosted or malformed URL with the toggle
// off); AnimeBytes is obtainable only when the operator enables it. Every
// other tracker (rare on SeaDex, and any unrecognized one) is treated as not
// obtainable, so a release the operator cannot grab never becomes a finding.
// Obtainable additionally takes the release's raw upstream URL (exactly as
// SeaDex supplied it, BEFORE any label-trusting normalization such as
// trackerlink.Publish) so the AnimeBytes URL-host cross-check (see
// ABVisible) inspects unmodified evidence rather than a rewritten link; pass
// "" when no URL is available. It ALSO requires the canonical usable URL
// (trackerlink.Publish's output): a release whose usable URL is empty -
// no URL at all, or one the canonicalizer rejected as malformed, foreign-host,
// or unsafe - is never obtainable, because the operator has no link to act on,
// so it must not count as comparison evidence (the SeaDex client already warns
// about the unusable URL).
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
// With the toggle ON everything surfaces. With it OFF only tracker.ABNone
// surfaces, so ambiguous evidence is hidden alongside definite evidence: a
// torrent that MIGHT be an AnimeBytes link must not be rendered as a clickable
// one while the operator has said they have no AnimeBytes account. Used by the
// daemon's obtainability filter and the audit report's verdict eligibility. The
// audit report's row LISTING deliberately takes the other fail direction and
// gates on tracker.ClassifyAB == tracker.ABDefinite instead, so a release with
// no usable link is annotated unobtainable rather than erased.
//
// The grade itself is internal/tracker's (the single home of the untrusted
// (label, URL) identity gates, beside tracker.CanonicalName); what lives here is
// the operator POLICY over it.
func ABVisible(trackerName, rawURL string, animeBytes bool) bool {
	return animeBytes || tracker.ClassifyAB(trackerName, rawURL) == tracker.ABNone
}

// ExcludeSpecial reports whether an entry classified special should be dropped
// under the exclude_specials filter; shared by compare and audit so the two
// consumers cannot drift.
func ExcludeSpecial(isSpecial, excludeSpecials bool) bool {
	return excludeSpecials && isSpecial
}
