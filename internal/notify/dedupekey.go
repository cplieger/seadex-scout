package notify

import (
	"slices"
	"strconv"
	"strings"

	"github.com/cplieger/keyenc"
	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// This file owns the finding-set IDENTITY policy: what makes two findings the same
// standing condition is a notification concern, so the key is derived here.

// dedupeKey keys a finding by AniList ID, status, recommended-group set, current
// group, release identity, and the full obtainable-source link set, so a
// same-group quality swap (new identity), a changed library state, or ANY
// change to the recommended sources becomes a DIFFERENT row - the old one
// resolves and the new one is announced - while an unchanged finding keeps
// its row and is re-emitted unchanged.
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
// key.
func currentGroupKey(f *compare.Finding) string {
	if f.CurrentGroups != nil {
		// Sorted for the same reason dedupeKey sorts the recommended set: the on-disk
		// group set is a SET, so its key must not depend on producer order.
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
func releaseIdentity(f *compare.Finding) string {
	if h := seadex.ValidInfoHash(f.InfoHash); h != "" {
		return keyenc.Join("hash", h)
	}
	return keyenc.Join("url", strings.TrimSpace(f.ReleaseURL))
}

// obtainableLinkKey returns a finding's full obtainable-source URL set
// (deduplicated by trimmed URL, sorted, bounded) as a single key component,
// or "" when the finding carries no links.
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
