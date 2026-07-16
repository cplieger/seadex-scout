// Package filter decides which SeaDex candidate releases an operator could use.
// It separates the content filters (remux policy, dual-audio) from tracker
// obtainability: a recommended release must both pass the content filters
// (KeepNonTracker) and sit on an obtainable tracker (Obtainable) - any public
// tracker, or AnimeBytes when the operator has enabled it. A release on a
// tracker the operator cannot use is simply absent, never flagged. Arr-side tag
// include/exclude happens earlier, in the library walk.
package filter

import (
	"net/url"
	"strings"

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

// ABVisible reports whether a release on the given tracker may surface to the
// operator: always true when the operator has enabled AnimeBytes, and
// otherwise false when either the tracker label is AnimeBytes OR the release's
// raw upstream URL (as SeaDex supplied it, never a normalized/rewritten link)
// points at the AnimeBytes host (or a dot-delimited subdomain). The URL
// cross-check exists because the tracker label is untrusted upstream data: a
// torrent labeled "Nyaa" carrying an animebytes.tv URL must not surface as a
// clickable AnimeBytes link while the toggle is off. A malformed URL - a parse
// failure, or a successful parse carrying a scheme but no host (which has
// swallowed its host evidence) - is treated conservatively as hidden rather
// than host-inferred from the unparsed string; an empty URL carries no link
// and passes. It is the single home of the animebytes toggle's drop rule,
// shared by the daemon's obtainability filter and the audit report's release
// listing.
func ABVisible(tracker, rawURL string, animeBytes bool) bool {
	if animeBytes {
		return true
	}
	if release.IsAnimeBytes(tracker) {
		return false
	}
	// Browsers (WHATWG URL parser) treat '\' as '/', so canonicalize the
	// host-evidence copy the same way; a '/\animebytes.tv/x' form is
	// protocol-relative in a browser and must not read as a host-less rooted
	// path.
	u := strings.ReplaceAll(strings.TrimSpace(rawURL), `\`, "/")
	if u == "" {
		return true
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return false
	}
	// Hostname() already drops the port and userinfo and ToLower folds case;
	// the FQDN trailing-dot form is handled inside the shared predicate.
	host := strings.ToLower(parsed.Hostname())
	if host == "" && parsed.Scheme != "" {
		// A successful parse with a scheme but no host has hidden its host
		// evidence: "https:/animebytes.tv/..." parses as scheme https + path,
		// and "animebytes.tv:443/..." parses as an opaque non-empty scheme.
		// Neither is reparse-recoverable below (the reparse fires only for the
		// truly schemeless form), so hide conservatively like a parse failure.
		return false
	}
	if host == "" && !strings.HasPrefix(u, "/") {
		// A schemeless absolute URL ("animebytes.tv/torrents.php?...") parses as
		// a bare path with no host, which would bypass the host check below;
		// re-parse it host-relative so the AnimeBytes host is still recognized.
		// A rooted relative path ("/local/path") is left alone.
		hostRel, herr := url.Parse("//" + u)
		if herr != nil {
			// The authority-form reparse failed (e.g. a backslash or space
			// before an "@"): the string's host evidence is unrecoverable, so
			// hide conservatively like a first-parse failure rather than
			// letting an unverifiable link surface while the toggle is off.
			return false
		}
		host = strings.ToLower(hostRel.Hostname())
	}
	for i := range len(host) {
		if host[i] >= 0x80 {
			// A non-ASCII host is homoglyph territory: browsers apply UTS46
			// host mapping (fullwidth U+FF0E / ideographic U+3002 dots become
			// '.', fullwidth letters fold to ASCII), so a host spelled
			// "animebytes<U+FF0E>tv" navigates to animebytes.tv while the
			// byte-wise suffix check cannot see it. Every legitimate SeaDex
			// tracker host is ASCII, so hide conservatively like a parse failure.
			return false
		}
	}
	return !release.IsAnimeBytesHost(host)
}

// ExcludeSpecial reports whether an entry classified special should be dropped
// under the exclude_specials filter; shared by compare and audit so the two
// consumers cannot drift.
func ExcludeSpecial(isSpecial, excludeSpecials bool) bool {
	return excludeSpecials && isSpecial
}
