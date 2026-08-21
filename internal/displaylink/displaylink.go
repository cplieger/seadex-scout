// Package displaylink is the single home of this app's STRUCTURAL vouch step
// for a browser-destined absolute URL: the publish-or-drop reading of
// urlform.Classify's facts that every gate handing an untrusted URL to a
// renderer (an arr's clickable link, a report row, a Torznab <comments> field)
// applies before it looks at the host at all.
package displaylink

import "github.com/cplieger/urlform"

// VouchForm is Vouch over an already-classified form: an absolute http(s) URL,
// free of a userinfo authority and of the smuggling shapes a browser reads
// differently from net/url.
func VouchForm(f *urlform.Form) bool {
	if f.HasUserInfo {
		return false
	}
	return VouchSanitizingForm(f)
}

// VouchSanitizingForm is VouchForm for a caller that SANITIZES a userinfo
// authority instead of refusing it: every structural leg above except the
// userinfo refusal.
func VouchSanitizingForm(f *urlform.Form) bool {
	if f.Class != urlform.ClassAbsolute {
		return false
	}
	if f.HasBackslash || f.HasTabOrNewline {
		return false
	}
	return isHTTPScheme(f.Scheme)
}

// isHTTPScheme reports whether a classified scheme is http or https, matched
// case-insensitively through urlform.EqualASCIIFold - the library's own
// ASCII-ONLY fold, deliberately not strings.EqualFold.
func isHTTPScheme(scheme string) bool {
	return urlform.EqualASCIIFold(scheme, "http") || urlform.EqualASCIIFold(scheme, "https")
}
