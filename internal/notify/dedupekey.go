package notify

import (
	"slices"
	"strconv"
	"strings"

	"github.com/cplieger/keyenc"
	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// This file owns the finding dedupe-key policy: WHEN a finding should
// re-surface across cycles is a notification/suppression concern, so the key is
// derived here at the start of Notify from the semantic Finding the compare
// package produces. The key format is pinned byte-for-byte by TestDedupeKey:
// any format change re-alerts the whole backlog as a one-time burst, so change
// it only deliberately (the 2026-07 validated-identity/link-set hardening
// accepted exactly that burst).

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
//
// Every level of the key - the outer component list, the group sets nested
// inside it, and the link set - is assembled with keyenc, so no component's
// content can forge a different component split and collide two distinct
// findings onto one key (which would suppress the second as already alerted).
// The untrusted components are group names, the current group, the release
// identity and the link URLs, all parsed from SeaDex data or library file
// names. Nesting is composition: an inner keyenc value is escaped again as it
// becomes an outer component, so a separator inside a group name cannot be read
// as an outer field boundary.
//
// The key is also size-bounded by construction. keyenc reduces a component set
// whose raw size exceeds keyenc.MaxComponentBytes to a fixed-size SHA-256
// identity, and because the OUTER assembly goes through keyenc too, that bound
// applies to the assembled key rather than only to each component: hostile bulk
// SeaDex data (hundreds of oversized URLs per entry) cannot amplify key
// construction into an out-of-memory failure, and the in-memory finding set
// these keys index stays bounded across N findings.
//
// It returns ONE key, and there is no second "legacy" form: nothing persists a
// key any more (findings are reported as state and held in memory), so a key
// format change costs one duplicate report on the pass after an upgrade rather
// than needing a conversion path.
func dedupeKey(f *compare.Finding) string {
	groups := slices.Clone(f.RecommendedGroups)
	slices.Sort(groups)
	parts := []string{
		strconv.Itoa(f.AniListID),
		string(f.Status),
		keyenc.Join(groups...),
		currentGroupKey(f),
		releaseIdentity(f),
	}
	if linkSet := obtainableLinkKey(f.Links); linkSet != "" {
		parts = append(parts, "links", linkSet)
	}
	return keyenc.Join(parts...)
}

// currentGroupKey encodes the finding's current-group component for the dedupe
// key. Both branches produce a keyenc value rather than one encoded set and one
// bare string, which is what keeps them distinguishable: a production finding
// carrying the group set ["a", "b"] and a manually constructed one whose
// flattened CurrentGroup is the literal "a:b" would otherwise encode
// identically once the outer assembly escaped them.
func currentGroupKey(f *compare.Finding) string {
	if f.CurrentGroups != nil {
		// Sorted for the same reason dedupeKey sorts the recommended set: the
		// on-disk group set is a SET, so its key contribution must not depend on
		// producer order (every current producer already sorts, so honest keys are
		// byte-identical and suppression survives).
		groups := slices.Clone(f.CurrentGroups)
		slices.Sort(groups)
		return keyenc.Join(groups...)
	}
	return keyenc.Join(f.CurrentGroup)
}

// releaseIdentity returns the stable torrent identity used by finding dedupe,
// domain-tagged so the two identity sources can never alias each other: a
// VALIDATED 40-hex info hash, else the release page URL. The tag is a keyenc
// component rather than a string prefix, so the two domains are kept apart by
// the encoding instead of by the tag happening not to occur in the value.
// The InfoHash is untrusted SeaDex data, so a crafted or garbled hash field
// must not key a finding unvalidated: it passes the same seadex.ValidInfoHash
// gate the indexer feed applies. SeaDex redacts AnimeBytes info hashes
// (ValidInfoHash rejects the redaction marker along with everything else
// non-hex), so every same-group AB replacement keys on its unique torrent page
// URL, as before.
func releaseIdentity(f *compare.Finding) string {
	if h := seadex.ValidInfoHash(f.InfoHash); h != "" {
		return keyenc.Join("hash", h)
	}
	return keyenc.Join("url", strings.TrimSpace(f.ReleaseURL))
}

// obtainableLinkKey returns a finding's full obtainable-source URL set
// (deduplicated by trimmed URL, sorted, bounded) as a single key component,
// or "" when the finding carries no links. Folding EVERY obtainable source
// into the key - not just the headline candidate's identity - re-surfaces a
// finding when any recommended source changes: a non-headline public-tracker
// torrent replacement (a new page URL) and an AnimeBytes toggle flip both
// change the set, where keying the headline identity alone left the first
// suppressed forever. Deduplicating by URL keeps the key label-insensitive:
// one source arriving twice (once mislabeled) keys once, so correcting the
// label later never re-alerts an unchanged source. The sorted raw set goes
// through keyenc, matching dedupeKey's collision-proofing and size-bounding: a
// SeaDex-supplied URL containing the separator cannot collide two link sets,
// and an oversized set (SeaDex admits up to 512 arbitrarily long URLs per
// entry) reduces to a fixed-size hash instead of one huge joined allocation.
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
	return keyenc.Join(urls...)
}
