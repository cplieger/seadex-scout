package notify

import (
	"slices"
	"strconv"
	"strings"

	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/keyenc"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// This file owns the finding dedupe-key policy: WHEN a finding should
// re-surface across cycles is a notification/suppression concern (the keys
// are persisted as notify.Alerted map keys in state.json), so the key is
// derived here at the start of Notify and Baseline from the semantic Finding
// the compare package produces. The key format is pinned
// byte-for-byte by TestDedupeKey: any format change invalidates every
// persisted key and re-alerts the whole backlog as a one-time burst, so
// change it only deliberately (the 2026-07 validated-identity/link-set
// hardening accepted exactly that burst).

// dedupeKey keys a finding by AniList ID, status, recommended-group set, current
// group, release identity, and the full obtainable-source link set, so a
// same-group quality swap (new identity), a changed library state, or ANY
// change to the recommended sources re-surfaces while an unchanged finding is
// suppressed. The link-set component covers what the headline identity alone
// cannot: a NON-headline candidate's torrent replacement (a new tracker page
// URL) and an AnimeBytes toggle flip (AB links joining or leaving the set)
// both change the key, where previously only the headline candidate and the
// AB subset were keyed and a replaced secondary public source stayed
// suppressed forever.
// The untrusted components (group names, the current group, the release
// identity, and the link URLs - all parsed from SeaDex data or library file
// names) have their delimiter characters escaped before joining
// (keyenc.BoundedJoinParts), so a value that itself contains the ',' or '|'
// delimiter cannot collide two distinct findings onto one key (which would
// suppress the second as already alerted), while a delimiter-free value keeps
// its plain unescaped representation in the key.
// Every untrusted component is also size-bounded: a component set larger than
// keyenc.MaxComponentBytes is reduced to a fixed-size SHA-256 identity
// instead of being materialized into the key, so hostile bulk SeaDex data
// (hundreds of oversized URLs per entry) cannot amplify key construction into
// an out-of-memory failure.
func dedupeKey(f *compare.Finding) string {
	groups := slices.Clone(f.RecommendedGroups)
	slices.Sort(groups)
	key := strings.Join([]string{
		strconv.Itoa(f.AniListID),
		string(f.Status),
		keyenc.BoundedJoinParts(groups),
		currentGroupKey(f),
		keyenc.BoundedPart(releaseIdentity(f)),
	}, "|")
	if linkSet := obtainableLinkKey(f.Links); linkSet != "" {
		key += "|links=" + linkSet
	}
	return key
}

// currentGroupKey encodes the finding's current-group component for the dedupe
// key. When the structured group slice is present (production findings built
// by compare's baseFinding), each element is escaped before joining so
// distinct group sets whose display joins collide (["a,b","c"] vs
// ["a","b,c"], or ["A","B"] vs the literal ["A,B"]) keep distinct keys. A
// manually constructed finding (nil CurrentGroups) falls back to escaping the
// flattened CurrentGroup; delimiter-free production keys are byte-identical
// either way.
func currentGroupKey(f *compare.Finding) string {
	if f.CurrentGroups != nil {
		return keyenc.BoundedJoinParts(f.CurrentGroups)
	}
	return keyenc.BoundedPart(f.CurrentGroup)
}

// releaseIdentity returns the stable torrent identity used by finding dedupe,
// domain-tagged so the two identity sources can never alias each other: a
// VALIDATED 40-hex info hash ("hash:" + the lowercased hex), else the release
// page URL ("url:" + trimmed). The InfoHash is untrusted SeaDex data - the
// previous code trusted any non-redacted value verbatim as the identity, so a
// crafted or garbled hash field keyed the finding unvalidated; dedupe now
// applies the same seadex.ValidInfoHash gate the indexer feed already uses.
// SeaDex redacts AnimeBytes info hashes (ValidInfoHash rejects the redaction
// marker along with everything else non-hex), so every same-group AB
// replacement keys on its unique torrent page URL, as before.
func releaseIdentity(f *compare.Finding) string {
	if h := seadex.ValidInfoHash(f.InfoHash); h != "" {
		return "hash:" + h
	}
	return "url:" + strings.TrimSpace(f.ReleaseURL)
}

// obtainableLinkKey returns a finding's full obtainable-source URL set
// (deduplicated by trimmed URL, sorted, bounded) as a single key component,
// or "" when the finding carries no links. Folding EVERY obtainable source
// into the key - not just the headline candidate's identity - re-surfaces a
// finding when any recommended source changes: a non-headline public-tracker
// torrent replacement (a new page URL) previously kept the key unchanged and
// was suppressed forever, and an AnimeBytes toggle flip changes the set
// exactly as the retired AB-only component did. Deduplicating by URL keeps
// the key label-insensitive: one source arriving twice (once mislabeled)
// keys once, so correcting the label later never re-alerts an unchanged
// source. The sorted raw set goes through keyenc.BoundedJoinParts, matching
// dedupeKey's collision-proofing and size-bounding: a SeaDex-supplied URL
// containing ',' or '|' cannot collide two link sets, and an oversized set
// (SeaDex admits up to 512 arbitrarily long URLs per entry) reduces to a
// fixed-size hash instead of one huge joined allocation.
func obtainableLinkKey(links []compare.ReleaseLink) string {
	seen := make(map[string]struct{}, len(links))
	var urls []string
	for i := range links {
		u := strings.TrimSpace(links[i].URL)
		if u == "" {
			continue
		}
		if _, dup := seen[u]; dup {
			continue
		}
		seen[u] = struct{}{}
		urls = append(urls, u)
	}
	if len(urls) == 0 {
		return ""
	}
	slices.Sort(urls)
	return keyenc.BoundedJoinParts(urls)
}
