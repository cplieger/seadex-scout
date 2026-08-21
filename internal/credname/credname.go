// Package credname is the one home for the rule "which URL query-parameter NAME
// denotes a credential". Two of this app's packages judge that question with
// deliberately DIFFERENT policies, and both read them from here:
//
//   - internal/config's boot warning (urlEmbedsCredential) matches the canonical
//     names EXACTLY, via IsName. Over-matching a name there costs only a
//     spurious operator warning, and the message is field-name-only, so the
//     exact list is the honest scope for a diagnostic.
package credname

import "strings"

// names is the canonical credential-like parameter-name set: apikey/api_key and the
// apitoken/api_token/access_token/auth_token variants, the bare token,
// passkey/authkey/torrent_pass, and password/pass/secret/client_secret/rss_key.
var names = map[string]struct{}{
	"apikey": {}, "api_key": {}, "apitoken": {}, "api_token": {},
	"access_token": {}, "auth_token": {}, "passkey": {}, "token": {},
	"authkey": {}, "torrent_pass": {}, "password": {}, "pass": {},
	"secret": {}, "client_secret": {}, "rss_key": {},
}

// words are the credential word stems the canonical names are built from - the
// vocabulary the broad ContainsWord policy matches on. Every canonical name contains
// at least one, which is what makes ContainsWord a superset of IsName by construction.
var words = []string{"key", "token", "pass", "secret", "auth", "cred"}

// IsName reports whether name is one of the canonical credential parameter
// names, compared case-insensitively. It is the exact-match policy: the caller
// is expected to have already percent-decoded the name (net/url and
// urlform.RawQueryNames both do).
func IsName(name string) bool {
	_, ok := names[strings.ToLower(name)]
	return ok
}

// ContainsWord reports whether name CONTAINS a credential word stem, compared
// case-insensitively. It is the broad policy: every name IsName accepts is
// accepted here too, plus any not-yet-listed spelling built from the same
// vocabulary ("prowlarr_apikey", "x-api-key", "rsskey"). Callers that may hold a
// percent-encoded name decode it first, since the stems are ASCII.
func ContainsWord(name string) bool {
	lowered := strings.ToLower(name)
	for _, word := range words {
		if strings.Contains(lowered, word) {
			return true
		}
	}
	return false
}
