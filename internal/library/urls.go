package library

import "net/url"

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
	if err != nil {
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
