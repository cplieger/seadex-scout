package library

import (
	"net/url"
	"slices"

	"github.com/cplieger/urlform"
)

// SafeLogURL returns a copy of rawURL safe to emit across the logging trust
// boundary: userinfo, query, and fragment are stripped so reverse-proxy Basic
// Auth credentials (https://user:pass@host) or query tokens configured in the
// arr base URL never reach Loki or downstream notifications. An ordinary
// credential-free host/path deep-link passes through unchanged and stays
// clickable; any input that is not an absolute http(s) URL with a hostname
// yields an empty string, so a caller must treat "" as "no usable link" rather
// than as "unparseable". It lives beside the ArrURL construction it guards so
// every slog emitter of a config-derived arr URL shares one sanitization rule.
//
// It runs at CONSTRUCTION (seriesItem/movieItem build Item.ArrURL through it), so
// the credential-free property is a property of the stored value rather than of
// each emitter; the sink-side calls (SanitizedForStorage, the notify and audit
// render paths) stay as idempotent belt-and-braces for any Item built outside the
// walker.
//
// The ADMISSION half reads urlform, the app's classifier of record for the
// browser-vs-net/url divergence classes. ArrURL is a browser-destined deep-link
// published to humans through Loki and the report, which is exactly urlform's
// parser-of-record case and the same publish-or-drop pattern internal/seadex
// already runs. Reading the classifier instead of re-deriving the divergence
// taxonomy here is what keeps this gate current as urlform learns new classes;
// the forms it must keep dropping are the opaque schemeless-credential form
// ("user:pass@host/..."), the single- and four-slash hidden-host forms, the
// port-only authority, and the protocol-relative form (each covered by a case
// in TestSafeLogURL and by FuzzSafeLogURL's no-leak oracle).
// Only ClassAbsolute with an http(s) scheme and a real host is admitted, and a
// smuggling form (backslash authority, embedded tab/newline) is refused
// outright: a de-smuggled string is not vouchable, the stance trackerlink.Publish
// takes.
//
// The STRIP half stays app-side by design: urlform classifies and deliberately
// never rewrites, so removing userinfo/query/fragment remains a consumer
// concern.
func SafeLogURL(rawURL string) string {
	f := urlform.Classify(rawURL)
	if f.Class != urlform.ClassAbsolute || f.Host == "" {
		return ""
	}
	if f.HasBackslash || f.HasTabOrNewline {
		return ""
	}
	if f.Scheme != "http" && f.Scheme != "https" {
		return ""
	}
	// Re-parse the classifier's preprocessed string to perform the strip. It is
	// known-parseable (ClassAbsolute) and already free of the whitespace bytes a
	// browser would drop, so this cannot fail; the guard keeps the function
	// total rather than trusting that invariant across a library change.
	u, err := url.Parse(f.Trimmed)
	if err != nil {
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// SanitizedForStorage returns a copy of the snapshot whose per-item ArrURLs
// have passed SafeLogURL, so a credentialed public_url never lands in
// state.json. state.Store.Save applies it at the persistence boundary; the
// finding-dedupe path needs no counterpart, since notify.StoredFinding
// persists no URL at all.
func (s Snapshot) SanitizedForStorage() Snapshot {
	out := s
	out.Items = slices.Clone(s.Items)
	for i := range out.Items {
		out.Items[i].ArrURL = SafeLogURL(out.Items[i].ArrURL)
	}
	return out
}
