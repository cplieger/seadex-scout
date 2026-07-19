package release

import (
	"net/url"
	"strings"
)

// URLFormClass names the structural form of a raw, untrusted URL string -
// specifically the browser-vs-net/url parse quirks that decide whether the
// string really carries a host. It is the single home of that quirk
// vocabulary; see URLForm.
type URLFormClass int

const (
	// URLFormEmpty is a string that is empty after whitespace trimming.
	URLFormEmpty URLFormClass = iota
	// URLFormMalformed is a string the canonicalized parse rejected; no
	// structural facts (and no host evidence) can be extracted from it.
	URLFormMalformed
	// URLFormAbsolute is a scheme-and-host absolute URL ("https://host/x");
	// Host carries the parsed hostname.
	URLFormAbsolute
	// URLFormHiddenHost is a scheme-bearing parse with no hostname, where
	// net/url sees no host but a browser may navigate to one: a
	// path-relative scheme form ("https:/host/x" parses scheme + path), an
	// opaque host:port form ("host:443/x" parses the host as the scheme), or
	// a port-only authority ("https://:443/x"). The host evidence is hidden.
	URLFormHiddenHost
	// URLFormProtocolRelative is a network-path reference: "//host/x" (Host
	// carries the parsed host a browser would resolve against the ambient
	// scheme) or a three-or-more-slash form ("///x"), which Go parses as a
	// rooted path while browsers read an authority (Host stays empty - the
	// form is ambiguous).
	URLFormProtocolRelative
	// URLFormSchemelessHost is a scheme-free, non-rooted form ("host/x"):
	// net/url parses a bare path, but a browser address bar navigates to the
	// first segment as a host. Host carries that authority-reparse evidence
	// (empty for a query- or fragment-only form such as "?x:y");
	// HostUnrecoverable marks a failed reparse.
	URLFormSchemelessHost
	// URLFormRelative is a rooted, host-free relative path ("/x").
	URLFormRelative
)

// URLForm is the structural classification of one raw, untrusted URL string
// (as SeaDex supplies it). It names the browser-vs-net/url parse-quirk
// classes ONCE - backslash authorities, protocol-relative and schemeless-host
// forms, hidden-host parses - so every consumer branches on the same facts
// while keeping its own fail direction as policy: the seadex publisher drops
// what it cannot vouch for (publish-or-drop), while the AnimeBytes toggle
// gate hides what it cannot classify (extract-evidence-or-hide). Fields are
// ordered for govet fieldalignment.
type URLForm struct {
	// parsed is the canonicalized parse result (backslashes read as slashes,
	// like the WHATWG parser); nil for URLFormEmpty and URLFormMalformed. It
	// stays private to the classifier: consumers read the semantic facts
	// below (Scheme, Host, Port, HasUserInfo), never the parser
	// representation, so a parser/canonicalization change cannot cross the
	// package boundary. The nil-exactly-for-Empty/Malformed invariant is
	// pinned by the in-package fuzz test.
	parsed *url.URL
	// Trimmed is the whitespace-trimmed raw string the classification read,
	// with backslashes NOT canonicalized: it is what a publisher emits or
	// prefixes, never a rewritten form.
	Trimmed string
	// Host is the lowercased host evidence a browser would navigate to, when
	// extractable: the parsed hostname of an absolute or protocol-relative
	// form, or the authority reparse of a schemeless-host form. Empty when
	// the string carries none (or the form hides it; see Class).
	Host string
	// Scheme is the canonicalized parse's scheme, which url.Parse folds to
	// lowercase (an "HTTPS://" source reads "https", RFC 3986 canonical
	// form), so the value is already case-folded; empty when the string
	// carries none or did not parse. Case-insensitive comparison by consumers
	// remains correct as defense in depth.
	Scheme string
	// Port is the canonicalized parse's port string; empty when none is
	// present or the string did not parse. net/url only accepts an
	// all-digit port, but it does not range-check it - consumers that need
	// a valid 16-bit port (the seadex link publisher) validate the range.
	Port string
	// Class is the structural form.
	Class URLFormClass
	// HasBackslash records a '\' anywhere in the trimmed string. Browsers
	// (WHATWG URL parser) treat '\' as '/', so the parsed facts (Scheme/
	// Host/Port/Class) describe the canonicalized reading - a `/\host/x`
	// form classifies protocol-relative,
	// not as a host-less rooted path - while the flag lets a publisher that
	// must emit the raw string reject it outright.
	HasBackslash bool
	// HostUnrecoverable marks a URLFormSchemelessHost whose authority reparse
	// failed (e.g. a space before an "@"): browser-visible host evidence may
	// exist but cannot be extracted, so evidence-driven consumers treat the
	// form like a parse failure.
	HostUnrecoverable bool
	// HasUserInfo records a userinfo authority component ("user@host") in
	// the canonicalized parse - a visual-spoofing vector
	// ("https://trusted@evil/") the seadex link publisher rejects. For a
	// URLFormSchemelessHost the fact comes from the same authority reparse
	// that supplies Host (so "user@host/x" reports it alongside the
	// recovered host). Always false when the string did not parse.
	HasUserInfo bool
}

// ClassifyRawURL classifies a raw URL string into its structural URLForm. It
// never errors: every input lands in exactly one class, and unparseable input
// is URLFormMalformed. Consumers apply their own policy over the returned
// facts (see URLForm).
func ClassifyRawURL(raw string) URLForm {
	f := URLForm{Trimmed: strings.TrimSpace(raw)}
	f.HasBackslash = strings.Contains(f.Trimmed, `\`)
	if f.Trimmed == "" {
		f.Class = URLFormEmpty
		return f
	}
	canonical := strings.ReplaceAll(f.Trimmed, `\`, "/")
	parsed, err := url.Parse(canonical)
	if err != nil {
		f.Class = URLFormMalformed
		return f
	}
	f.parsed = parsed
	f.Scheme = parsed.Scheme
	f.Port = parsed.Port()
	f.HasUserInfo = parsed.User != nil
	// Hostname() drops the port and userinfo; ToLower folds case for the
	// byte-wise host predicates downstream.
	f.Host = strings.ToLower(parsed.Hostname())
	switch {
	case parsed.Scheme != "" && f.Host != "":
		f.Class = URLFormAbsolute
	case parsed.Scheme != "":
		f.Class = URLFormHiddenHost
	case f.Host != "":
		f.Class = URLFormProtocolRelative
	case strings.HasPrefix(canonical, "//"):
		// Three or more leading slashes: Go parsed a rooted path (no host),
		// browsers resolve a network-path authority.
		f.Class = URLFormProtocolRelative
	case strings.HasPrefix(canonical, "/"):
		f.Class = URLFormRelative
	default:
		f.Class = URLFormSchemelessHost
		rehost, rerr := url.Parse("//" + canonical)
		if rerr != nil {
			f.HostUnrecoverable = true
			return f
		}
		f.Host = strings.ToLower(rehost.Hostname())
		f.HasUserInfo = rehost.User != nil
	}
	return f
}
