package seadex

import (
	"strconv"
)

// DefaultBaseURL is the canonical SeaDex site base - the SINGLE home of the
// SeaDex site-base fact, beside the package's other SeaDex contract knowledge
// (EntryURL, ValidInfoHash).
const DefaultBaseURL = "https://seadex.moe"

// EntryURL returns the SeaDex entry page for an AniList id under
// DefaultBaseURL, or "" when the id is unknown. The trailing slash is the
// site's canonical spelling (it redirects "/<id>" to "/<id>/"). The
// entry-page rule lives here, beside the package's other SeaDex contract
// knowledge (ValidInfoHash), so every consumer builds the same link from the
// same base.
func EntryURL(aniListID int) string {
	if aniListID <= 0 {
		return ""
	}
	return DefaultBaseURL + "/" + strconv.Itoa(aniListID) + "/"
}
