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
// Obtainable additionally takes the release's raw upstream URL (exactly as
// SeaDex supplied it, BEFORE any label-trusting normalization such as
// seadex.Torrent.UsableURL) so the AnimeBytes URL-host cross-check (see
// ABVisible) inspects unmodified evidence rather than a rewritten link; pass
// "" when no URL is available.
func Obtainable(r *release.Release, rawURL string, opts Options) bool {
	switch r.TrackerType {
	case release.TrackerPublic:
		return ABVisible(r.Tracker, rawURL, opts.AnimeBytes)
	case release.TrackerPrivate:
		return release.IsAnimeBytes(r.Tracker) && ABVisible(r.Tracker, rawURL, opts.AnimeBytes)
	default:
		return false
	}
}

// hostFromRawURL extracts normalized host evidence from a release's raw
// upstream URL. The boolean is false when malformed or ambiguous input must be
// hidden conservatively; an empty host with ok=true means the URL carries no
// host evidence at all (an empty string, a rooted relative path, or a
// query/fragment-only form). The structural reading of the raw string lives
// in the shared release.ClassifyRawURL (which canonicalizes backslashes the
// way browsers do, so a `/\animebytes.tv/x` form reads protocol-relative, not
// as a host-less rooted path); this gate applies the extract-evidence-or-hide
// policy over those facts - the inverse fail direction of the seadex
// publisher's publish-or-drop over the same classifier.
func hostFromRawURL(rawURL string) (string, bool) {
	f := release.ClassifyRawURL(rawURL)
	switch f.Class {
	case release.URLFormEmpty, release.URLFormRelative:
		return "", true
	case release.URLFormAbsolute:
		return f.Host, true
	case release.URLFormProtocolRelative:
		// "//host/x" carries real host evidence; the three-or-more-slash form
		// (a browser authority, a Go rooted path) has none and is ambiguous,
		// so it hides conservatively rather than losing host evidence.
		return f.Host, f.Host != ""
	case release.URLFormSchemelessHost:
		// A schemeless absolute URL ("animebytes.tv/torrents.php?...") would
		// bypass a naive host check; the classifier's authority reparse keeps
		// the AnimeBytes host recognizable. A failed reparse means the host
		// evidence is unrecoverable: hide conservatively, like a parse
		// failure, rather than letting an unverifiable link surface while the
		// toggle is off.
		return f.Host, !f.HostUnrecoverable
	default:
		// URLFormMalformed and URLFormHiddenHost ("https:/animebytes.tv/..."
		// parses as scheme + path, "animebytes.tv:443/..." as an opaque
		// scheme, "https://:443/x" as a port-only authority) have hidden or
		// destroyed their host evidence: hide conservatively.
		return "", false
	}
}

// ABVisible reports whether a release on the given tracker may surface to the
// operator: always true when the operator has enabled AnimeBytes, and
// otherwise false when either the tracker label is AnimeBytes OR the release's
// raw upstream URL (as SeaDex supplied it, never a normalized/rewritten link)
// points at the AnimeBytes host (or a dot-delimited subdomain). The URL
// cross-check exists because the tracker label is untrusted upstream data: a
// torrent labeled "Nyaa" carrying an animebytes.tv URL must not surface as a
// clickable AnimeBytes link while the toggle is off. The URL-to-host evidence
// extraction (including the conservative hide of malformed or ambiguous
// forms) lives in hostFromRawURL; this function is the policy decision. It is
// the single home of the animebytes toggle's drop rule, shared by the
// daemon's obtainability filter and the audit report's release listing.
func ABVisible(tracker, rawURL string, animeBytes bool) bool {
	if animeBytes {
		return true
	}
	if release.IsAnimeBytes(tracker) {
		return false
	}
	host, ok := hostFromRawURL(rawURL)
	if !ok {
		return false
	}
	if !release.IsASCIIHost(host) {
		// A non-ASCII host is homoglyph territory (see release.IsASCIIHost,
		// the one home of the ASCII rule): browsers navigate
		// "animebytes<U+FF0E>tv" to animebytes.tv while a byte-wise check
		// cannot see it. The shared tracker predicate rejects such hosts too,
		// but its fail direction is inverted here - an unclassifiable host
		// reads as "not AnimeBytes" and would surface - so the gate hides
		// them explicitly, conservatively, like a parse failure.
		return false
	}
	return !release.IsAnimeBytesHost(host)
}

// ExcludeSpecial reports whether an entry classified special should be dropped
// under the exclude_specials filter; shared by compare and audit so the two
// consumers cannot drift.
func ExcludeSpecial(isSpecial, excludeSpecials bool) bool {
	return excludeSpecials && isSpecial
}
