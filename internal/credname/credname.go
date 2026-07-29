// Package credname is the one home for the rule "which URL query-parameter NAME
// denotes a credential". Two of this app's packages judge that question with
// deliberately DIFFERENT policies, and both read them from here:
//
//   - internal/config's boot warning (urlEmbedsCredential) matches the canonical
//     names EXACTLY, via IsName. Over-matching a name there costs only a
//     spurious operator warning, and the message is field-name-only, so the
//     exact list is the honest scope for a diagnostic.
//   - internal/indexer's redaction of reflected upstream text (upstreamSecrets ->
//     redactSecrets) matches the credential WORDS those names are built from, via
//     ContainsWord. It is deliberately BROADER: a name added to the list later is
//     already redacted rather than silently leaked, because under-redacting
//     writes a credential to a log line (CWE-532) while over-redacting only
//     mangles a diagnostic.
//
// Why one home rather than a copy per policy: the two rules used to live in the
// two consumer packages independently, each doc comment describing the other.
// Their relation - every name config warns about is redacted by the indexer -
// was load-bearing (the asymmetric consequence sits on the redaction side) yet
// held only by coincidence of English word choice, with nothing linking the two
// sets. Here the containment is a test over both (credname_test.go), so a name
// added to the list without a matching word fails the build instead of leaking.
//
// The runner-up home was exporting the list from internal/config for the indexer
// to import, which loses: only the composition root imports internal/config, and
// internal/indexer deliberately takes no dependency on it, so a pure leaf both
// may import is the only home that keeps that rule intact (l-f22).
package credname

import "strings"

// names is the canonical credential-like parameter-name set: apikey/api_key and
// the apitoken/api_token/access_token/auth_token variants, the bare token,
// passkey/authkey/torrent_pass, and
// password/pass/secret/client_secret/rss_key. Compared case-insensitively
// through IsName.
//
// authkey and torrent_pass are AnimeBytes' own credential parameter names,
// carried by every AB direct download/announce URL - exactly the paste mistake
// (a real tracker URL where a Prowlarr per-indexer endpoint belongs) the
// operator warning exists to catch.
var names = map[string]struct{}{
	"apikey": {}, "api_key": {}, "apitoken": {}, "api_token": {},
	"access_token": {}, "auth_token": {}, "passkey": {}, "token": {},
	"authkey": {}, "torrent_pass": {}, "password": {}, "pass": {},
	"secret": {}, "client_secret": {}, "rss_key": {},
}

// words are the credential word stems the canonical names are built from - the
// vocabulary the broad ContainsWord policy matches on. Every canonical name
// contains at least one of them (pinned by the containment test), which is what
// makes ContainsWord a superset of IsName by construction rather than by
// coincidence.
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
