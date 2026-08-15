package seadex

import (
	"strconv"
)

// DefaultBaseURL is the canonical releases.moe site base - the SINGLE home of
// the SeaDex site-base fact, beside the package's other releases.moe contract
// knowledge (EntryURL, ValidInfoHash).
const DefaultBaseURL = "https://releases.moe"

// EntryURL returns the SeaDex entry page for an AniList id under
// DefaultBaseURL, or "" when the id is unknown. The entry-page rule lives
// here, beside the package's other releases.moe contract knowledge
// (ValidInfoHash), so every consumer builds the same link from the same base.
func EntryURL(aniListID int) string {
	if aniListID <= 0 {
		return ""
	}
	return DefaultBaseURL + "/" + strconv.Itoa(aniListID)
}
