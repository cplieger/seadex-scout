package tracker

import "github.com/cplieger/urlform"

// hostFromRawURL extracts normalized host evidence from a release's raw
// upstream URL. The boolean is false when malformed or ambiguous input must be
// hidden conservatively; an empty host with ok=true means the URL carries no
// host evidence at all (an empty string, a rooted relative path, or a
// query/fragment-only form).
func hostFromRawURL(rawURL string) (string, bool) {
	f := urlform.Classify(rawURL)
	switch f.Class {
	case urlform.ClassEmpty, urlform.ClassRelative:
		return "", true
	case urlform.ClassAbsolute:
		return f.Host, true
	case urlform.ClassProtocolRelative:
		// "//host/x" carries real host evidence; the three-or-more-slash form
		// (a browser authority, a Go rooted path) has none and is ambiguous,
		// so it hides conservatively rather than losing host evidence.
		return f.Host, f.Host != ""
	case urlform.ClassSchemelessHost:
		// A schemeless absolute URL ("animebytes.tv/torrents.php?...") would
		// bypass a naive host check; the classifier's authority reparse keeps
		// the AnimeBytes host recognizable.
		return f.Host, !f.HostUnrecoverable
	case urlform.ClassHiddenHost:
		// Special schemes recover the browser's authority reading
		// ("https:/animebytes.tv/x", "https:animebytes.tv/x" both navigate to
		// animebytes.tv), so recovered evidence is recognized like an
		// absolute form's; a form with no recovered host has genuinely hidden
		// or destroyed its evidence and hides conservatively.
		return f.Host, f.Host != "" && !f.HostUnrecoverable
	default:
		// urlform.ClassMalformed has no facts at all: hide conservatively.
		return "", false
	}
}

// ABEvidence grades how strongly a release's untrusted (tracker, rawURL) pair
// identifies AnimeBytes. It exists because that grading is a THREE-valued domain
// fact, and modelling it as separate booleans left the relationship between them
// (definite is a strict subset of gated; the difference is the ambiguous band)
// stated only in prose and reconstructed by each consumer through CALL ORDER.
type ABEvidence uint8

// The three grades. Ordered from weakest to strongest evidence, so a consumer
// that only cares whether ANY AnimeBytes evidence exists can compare against
// ABNone.
const (
	// ABNone means the pair carries no AnimeBytes evidence: the label is another
	// tracker AND the URL either yields usable non-AnimeBytes host evidence or carries
	// none at all. This is the only grade safe to surface with the toggle off.
	ABNone ABEvidence = iota
	// ABAmbiguous means the pair MIGHT be AnimeBytes and the evidence cannot settle it:
	// a malformed URL, a smuggled or hidden-host form, or a non-ASCII host (homograph
	// territory a byte-wise check cannot see).
	ABAmbiguous
	// ABDefinite means the pair proves AnimeBytes: the label says so, the extracted
	// canonical ASCII host resolves to the AnimeBytes host or a subdomain of it, or the
	// URL carries the documented AB torrent-page relative shape.
	ABDefinite
)

// ClassifyAB grades the AnimeBytes evidence in one release's untrusted
// (tracker, rawURL) pair. It is total: every input lands in exactly one grade,
// and it takes no view of what the caller should DO about it - the operator's
// animebytes toggle is policy, applied by filter.ABVisible.
func ClassifyAB(trackerName, rawURL string) ABEvidence {
	if IsAnimeBytes(trackerName) {
		return ABDefinite
	}
	// A relative URL carries no host evidence, but the AB torrent-page shape is tracker
	// identity in its own right: a mislabeled entry publishing it is AnimeBytes.
	if inferred, ok := LookupByRelativeURL(rawURL); ok && inferred.Name == NameAnimeBytes {
		return ABDefinite
	}
	host, ok := hostFromRawURL(rawURL)
	if !ok {
		// The evidence was destroyed or is ambiguous: malformed, smuggled, or
		// a hidden-host form whose authority could not be recovered.
		return ABAmbiguous
	}
	if host == "" {
		// No host evidence AT ALL - an empty URL, or a relative reference that
		// matched no tracker shape above. Nothing points at AnimeBytes and
		// nothing was hidden, so this is not AnimeBytes evidence.
		return ABNone
	}
	if !urlform.IsASCIIHost(host) {
		// Homograph territory. urlform.IsASCIIHost is the one home of the ASCII
		// rule; a host that fails it settles nothing either way.
		return ABAmbiguous
	}
	if IsAnimeBytesHost(host) {
		return ABDefinite
	}
	return ABNone
}
