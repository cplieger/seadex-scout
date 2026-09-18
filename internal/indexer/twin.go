package indexer

import (
	"strings"

	"github.com/cplieger/seadex-scout/internal/seadex"
)

// twinFragment is the fragment the film twin's GUID carries beside the original
// release's GUID. A fragment is invisible to trackerKeyFromURL and both tracker
// id extractors (they read the path and the torrentid query only), so the twin
// keys to the SAME tracker identity while Prowlarr, which dedupes on the GUID
// string, and Sonarr, which groups by GUID, keep the two items apart.
const twinFragment = "sonarr"

// twinGUID derives the film twin's GUID from the original's: the one home of the
// fragment rule, used by the journal write and derived again at search render
// time from the Prowlarr GUID. Empty for an empty GUID.
func twinGUID(guid string) string {
	if guid == "" {
		return ""
	}
	return guid + "#" + twinFragment
}

// twinTitle builds the title the film twin is served under - the Sonarr series
// the film is filed under, the S00Exx token the mapping-list names, and the same
// release flags the original title carries (Sonarr's quality parser needs them) -
// or "" when the entry is not a film offered to a Sonarr series with a named
// special episode.
func twinTitle(t *seadex.Torrent, info *EntryInfo) string {
	series := strings.TrimSpace(info.SeriesTitle)
	if info.Target != TargetSonarr || info.SpecialEpisode <= 0 || series == "" {
		return ""
	}
	flags := releaseFlags(t)
	parts := make([]string, 0, 2+len(flags))
	parts = append(parts, series, seasonLabel(0)+episodeLabel(info.SpecialEpisode))
	return strings.Join(append(parts, flags...), " ")
}

// sonarrTwin expands a stored item carrying a film twin into the second wire
// item the RSS render serves: a copy under the twin's title and GUID, Anime only
// (the category the series' Sonarr subscribes to), with the two twin fields
// blank so the copy is not itself expandable. Everything else - the tvdb id (the
// same series id), size, download URL, info hash, marker, peers, dates - is
// inherited, which is what lets Sonarr's quality parser read it.
func sonarrTwin(orig *item) item {
	twin := *orig
	twin.Title = orig.SonarrTitle
	twin.GUID = orig.SonarrGUID
	twin.Categories = []int{catAnime}
	twin.SonarrTitle, twin.SonarrGUID = "", ""
	return twin
}

// searchTwin is sonarrTwin's search-side counterpart for a proxied Prowlarr
// result whose owners agree on a twin title: a copy under that title, Anime only,
// with the twin GUID derived from the result's own identity at render time (a
// Prowlarr GUID is Prowlarr's, so nothing stored can carry it). The original
// keeps Prowlarr's own categories; the Movies-only fold is the app's and does not
// apply to a tracker's.
func searchTwin(orig *item, title string) item {
	twin := *orig
	twin.Title = title
	twin.GUID = twinGUID(orig.guid())
	twin.Categories = []int{catAnime}
	return twin
}

// twinVote is the three-state holders-agree fold over the twin title of one
// identity: every holder with a title must name the same one. A holder with no
// title abstains; two different titles veto; the fold is in memory at every
// site, so a contested title can never read as an abstention.
type twinVote struct {
	title      string
	conflict   bool
	candidates int
}

// add folds one holder's twin title in: empty abstains, the first non-empty sets
// the title, a different non-empty vetoes, and every non-empty counts as a
// candidate so a veto stays distinguishable from nobody voting.
func (v *twinVote) add(title string) {
	if title == "" {
		return
	}
	v.candidates++
	switch {
	case v.title == "":
		v.title = title
	case v.title != title:
		v.conflict = true
	}
}

// merge folds another vote in, conflict and all.
func (v *twinVote) merge(other twinVote) {
	v.candidates += other.candidates
	v.conflict = v.conflict || other.conflict
	if other.title == "" {
		return
	}
	switch {
	case v.title == "":
		v.title = other.title
	case v.title != other.title:
		v.conflict = true
	}
}

// resolve returns the agreed twin title, or "" when no holder named one or the
// holders disagree.
func (v twinVote) resolve() string {
	if v.conflict {
		return ""
	}
	return v.title
}
