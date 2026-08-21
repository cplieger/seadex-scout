package library

import (
	"net/url"

	"github.com/cplieger/seadex-scout/internal/displaylink"
	"github.com/cplieger/urlform"
)

// SafeLogURL returns a copy of rawURL safe to emit across the logging trust
// boundary: userinfo, query, and fragment are stripped so reverse-proxy Basic
// Auth credentials (https://user:pass@host) or query tokens configured in the
// arr base URL never reach Loki or downstream notifications.
func SafeLogURL(rawURL string) string {
	f := urlform.Classify(rawURL)
	if !displaylink.VouchSanitizingForm(&f) || f.Host == "" {
		return ""
	}
	// Re-parse the classifier's preprocessed string to perform the strip.
	u, err := url.Parse(f.Trimmed)
	if err != nil {
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
