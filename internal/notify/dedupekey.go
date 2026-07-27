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

// maxKeyBytes bounds the ASSEMBLED dedupe key. keyenc bounds each
// component at MaxComponentBytes of RAW bytes, but escaping can double a
// component and a key carries four of them, so the per-component bound
// alone admits a ~64 KiB key. Persisted keys are state.json map keys, so
// the aggregate has to be bounded too.
const maxKeyBytes = 2 * keyenc.MaxComponentBytes

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
	key, _ := dedupeKeyWithLegacy(f)
	return key
}

// dedupeKeyWithLegacy returns the canonical dedupe key plus, when the
// assembled key crossed the aggregate bound and was folded, the UNFOLDED key
// the same finding produced before the fold existed ("" otherwise). The
// aggregate bound is an in-place change to the PERSISTED identity format, so
// an instance that already stored one of the previously valid 16-64 KiB keys
// would otherwise re-alert an unchanged finding as new and emit a false
// resolution for the old key in the same cycle. Notify looks the legacy form
// up in the prior state and migrates the record onto the canonical key
// (original alert time preserved, no notification, no resolution line); the
// legacy key disappears with the next successful state save.
func dedupeKeyWithLegacy(f *compare.Finding) (canonical, legacy string) {
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
	if len(key) > maxKeyBytes {
		// Every component is individually bounded, but four escaped
		// in-bound components still assemble to ~64 KiB, and these keys
		// are the PERSISTED state map keys: N hostile findings push
		// state.json toward state's 32 MiB save cap, and a refused save
		// means ERROR + dedupe not advanced every cycle. Fold an oversized
		// assembled key onto the same fixed-size identity keyenc already
		// uses for an oversized component. The folded form has three
		// '|'-separated fields where every unfolded key has at least
		// five, so the two forms cannot collide. This bounds the KEY
		// side; the stored VALUE side is bounded separately, at
		// projection time by notify.capPersisted, so key and payload
		// together keep the persisted map bounded. The unfolded key
		// rides back as the legacy form so Notify can migrate a record
		// persisted before the fold instead of re-alerting it.
		return strconv.Itoa(f.AniListID) + "|" + string(f.Status) + "|" + keyenc.BoundedPart(key), key
	}
	return key, ""
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
		// Sorted for the same reason dedupeKey sorts the recommended set: the
		// on-disk group set is a SET, so its key contribution must not depend on
		// producer order (every current producer already sorts, so honest keys are
		// byte-identical and persisted suppression survives).
		groups := slices.Clone(f.CurrentGroups)
		slices.Sort(groups)
		return keyenc.BoundedJoinParts(groups)
	}
	return keyenc.BoundedPart(f.CurrentGroup)
}

// releaseIdentity returns the stable torrent identity used by finding dedupe,
// domain-tagged so the two identity sources can never alias each other: a
// VALIDATED 40-hex info hash ("hash:" + the lowercased hex), else the release
// page URL ("url:" + trimmed). The InfoHash is untrusted SeaDex data, so a
// crafted or garbled hash field must not key a finding unvalidated: it passes
// the same seadex.ValidInfoHash gate the indexer feed applies.
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
// torrent replacement (a new page URL) and an AnimeBytes toggle flip both
// change the set, where keying the headline identity alone left the first
// suppressed forever. Deduplicating by URL keeps
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
