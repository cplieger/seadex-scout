package library

import (
	"net/url"
	"slices"
)

// SafeLogURL returns a copy of rawURL safe to emit across the logging trust
// boundary: userinfo, query, and fragment are stripped so reverse-proxy Basic
// Auth credentials (https://user:pass@host) or query tokens configured in the
// arr base URL never reach Loki or downstream notifications. An ordinary
// credential-free host/path deep-link passes through unchanged and stays
// clickable; an unparseable URL yields an empty string. It lives beside the
// ArrURL construction it guards so every slog emitter of a config-derived arr
// URL shares one sanitization rule.
func SafeLogURL(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Opaque != "" ||
		(u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		// An opaque (non-hierarchical) URL - e.g. a scheme-less credentialed
		// base like "user:pass@host/..." parsed as scheme "user" with the
		// userinfo inside Opaque - keeps its credential where the strips
		// below cannot reach it. Malformed hierarchical forms (e.g. the
		// single-slash "https:/user:pass@host/..." or the four-slash
		// "https:////user:pass@host/...") parse with an empty Host and carry
		// the credential text in Path, equally out of reach — as does a
		// port-only authority ("https://:443/user:pass@host/..."), whose
		// Host is non-empty but whose Hostname is empty, hence the
		// Hostname() check. None of these is ever a valid arr deep-link
		// (which is always absolute http(s) with a hostname), so all are
		// dropped like an unparseable URL.
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// SanitizedForStorage returns a copy of the snapshot whose per-item ArrURLs
// have passed SafeLogURL, so a credentialed public_url never lands in
// state.json (mirroring report.storedFinding on the finding-dedupe path).
func (s Snapshot) SanitizedForStorage() Snapshot {
	out := s
	out.Items = slices.Clone(s.Items)
	for i := range out.Items {
		out.Items[i].ArrURL = SafeLogURL(out.Items[i].ArrURL)
	}
	return out
}
