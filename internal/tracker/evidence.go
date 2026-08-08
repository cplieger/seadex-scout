package tracker

import "github.com/cplieger/urlform"

// hostFromRawURL extracts normalized host evidence from a release's raw
// upstream URL. The boolean is false when malformed or ambiguous input must be
// hidden conservatively; an empty host with ok=true means the URL carries no
// host evidence at all (an empty string, a rooted relative path, or a
// query/fragment-only form). The structural reading of the raw string lives
// in the shared urlform.Classify (which canonicalizes backslashes the
// way browsers do, so a `/\animebytes.tv/x` form reads protocol-relative, not
// as a host-less rooted path); this gate applies the extract-evidence-or-hide
// policy over those facts - the inverse fail direction of
// trackerlink.Publish's publish-or-drop over the same classifier.
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
		// the AnimeBytes host recognizable. A failed reparse means the host
		// evidence is unrecoverable: hide conservatively, like a parse
		// failure, rather than letting an unverifiable link surface while the
		// toggle is off.
		return f.Host, !f.HostUnrecoverable
	case urlform.ClassHiddenHost:
		// The authority-carrying special schemes recover the browser's
		// reading ("https:/animebytes.tv/x" and "https:animebytes.tv/x" both
		// navigate to animebytes.tv - the WHATWG parser reads an authority
		// through any run of slashes), so recovered evidence participates
		// exactly like an absolute form's and a quirk-form AB URL is
		// recognized rather than merely hidden. A hidden-host form with no
		// recovered host (an opaque non-special scheme like
		// "animebytes.tv:443/x", a port-only authority, a failed reparse)
		// has genuinely hidden or destroyed its evidence: hide
		// conservatively.
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
// A consumer now switches on the value exhaustively and cannot get the order
// wrong, because there is no order.
type ABEvidence uint8

// The three grades. Ordered from weakest to strongest evidence, so a consumer
// that only cares whether ANY AnimeBytes evidence exists can compare against
// ABNone.
const (
	// ABNone means the pair carries no AnimeBytes evidence: the label is
	// another tracker AND the URL either yields usable non-AnimeBytes host
	// evidence or carries no host evidence at all (an empty URL, or a
	// relative reference matching no recognized tracker shape - which
	// resolves against the LABELED tracker's base, so it cannot become an
	// AnimeBytes link). This is the only grade that is safe to surface with
	// the toggle off.
	ABNone ABEvidence = iota
	// ABAmbiguous means the pair MIGHT be AnimeBytes and the evidence cannot
	// settle it: a malformed URL, a smuggled or hidden-host form whose
	// authority could not be recovered, or a non-ASCII host (homograph
	// territory - a browser navigates "animebytes<U+FF0E>tv" to animebytes.tv
	// while a byte-wise check cannot see it).
	//
	// It is its own grade rather than being folded into either neighbour
	// because the two fail directions in this app genuinely disagree about it:
	// a gate deciding whether to SURFACE a link must treat it as AnimeBytes
	// (fail closed), while a report deciding whether to LIST a row must not
	// erase it (fail open).
	ABAmbiguous
	// ABDefinite means the pair proves AnimeBytes: the tracker label says so,
	// the URL's extracted canonical ASCII host resolves to the AnimeBytes host
	// or a dot-delimited subdomain of it, or the URL carries the documented AB
	// torrent-page relative shape (rooted or slashless, which the resolver
	// reads as the same href) - tracker identity in its own right.
	ABDefinite
)

// ClassifyAB grades the AnimeBytes evidence in one release's untrusted
// (tracker, rawURL) pair. It is total: every input lands in exactly one grade,
// and it takes no view of what the caller should DO about it - the operator's
// animebytes toggle is policy, applied by filter.ABVisible.
//
// The tracker label alone is not trusted, because it is upstream data: a torrent
// labeled "Nyaa" carrying an animebytes.tv URL is AnimeBytes. The URL is read
// only as SeaDex supplied it, never normalized or rewritten first, so a
// smuggling form cannot be laundered into clean host evidence before grading.
func ClassifyAB(trackerName, rawURL string) ABEvidence {
	if IsAnimeBytes(trackerName) {
		return ABDefinite
	}
	// A relative URL carries no host evidence, but the AB torrent-page shape
	// ("/torrents.php?...&torrentid=..." and its slashless spelling, which the
	// resolver reads as the same rooted href) is tracker identity in its own
	// right: a mislabeled entry publishing that shape is AnimeBytes.
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
		// nothing was hidden, so this is not AnimeBytes evidence. It is
		// deliberately NOT ambiguous: a relative value resolves against the
		// LABELED tracker's base, so it cannot become an AnimeBytes link.
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
