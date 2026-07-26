package seadex

import (
	"strconv"
	"strings"
)

// DefaultBaseURL is the canonical releases.moe site base - the SINGLE home of
// the SeaDex site-base fact, beside the package's other releases.moe contract
// knowledge (EntryURL, ValidInfoHash). The indexer's fallback and the
// composition root (build.go) both reference it; config deliberately carries
// no equal literal (it is a dependency leaf and a second copy could silently
// drift).
const DefaultBaseURL = "https://releases.moe"

// EntryURL returns the SeaDex entry page for an AniList id under baseURL
// (the releases.moe site base), or "" when the id is unknown. The entry-page
// rule lives here, beside the package's other releases.moe contract knowledge
// (ValidInfoHash), so every consumer builds the same link
// from the same base.
//
// The TRACKER link is deliberately not this package's concern: whether an
// upstream torrent URL may be published as a clickable tracker link, and in
// what form, is trackerlink.Publish's policy - it reads the canonical tracker
// table, not the releases.moe contract, and it sits beside the hide half of
// that same concern in internal/filter (l-f86).
func EntryURL(baseURL string, aniListID int) string {
	if aniListID <= 0 {
		return ""
	}
	return strings.TrimRight(baseURL, "/") + "/" + strconv.Itoa(aniListID)
}
