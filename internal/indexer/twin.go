package indexer

import (
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/cplieger/seadex-scout/internal/payload"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"golang.org/x/text/unicode/norm"
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

func isTwinGUID(guid string) bool { return strings.HasSuffix(guid, "#"+twinFragment) }

// maxTwinEpisode is the highest season-0 episode a twin token may name: Sonarr's
// episode capture is \d{2,3}
// (https://github.com/Sonarr/Sonarr/blob/cab419ade8ac7fcab5bf80394ee492abd35d5f5a/src/NzbDrone.Core/Parser/Parser.cs#L175).
const maxTwinEpisode = 999

func twinTitle(t *seadex.Torrent, info *EntryInfo) string {
	title, _, _ := twinFor(t, info)
	return title
}

func twinFor(t *seadex.Torrent, info *EntryInfo) (title string, first, last int) {
	series := strings.TrimSpace(info.SeriesTitle)
	first, last, ok := twinRun(t, info)
	if !ok || series == "" {
		return "", 0, 0
	}
	return joinWithRevision([]string{series, twinToken(first, last)}, t), first, last
}

// twinToken is the season-0 token for one consecutive run: S00Eaa, or
// S00Eaa-Ebb, which Sonarr parses as exactly first..last: its multi-episode
// regexes take the first and last capture and expand the range between them
// (https://github.com/Sonarr/Sonarr/blob/cab419ade8ac7fcab5bf80394ee492abd35d5f5a/src/NzbDrone.Core/Parser/Parser.cs#L1048-L1057).
func twinToken(first, last int) string {
	token := seasonLabel(0) + episodeLabel(first)
	if last > first {
		token += "-" + episodeLabel(last)
	}
	return token
}

// twinRun is the run of season-0 episodes torrent t may be labeled as, ok false
// when the entry's episodes are not one consecutive run Sonarr can parse or t
// does not hold exactly that entry: a token claims the whole torrent is those
// episodes, so a season pack carrying the special, or one file of a
// multi-episode special, must not get it.
func twinRun(t *seadex.Torrent, info *EntryInfo) (first, last int, ok bool) {
	run := info.SpecialEpisodes
	if len(run) == 0 || run[0] < 1 || run[len(run)-1] > maxTwinEpisode {
		return 0, 0, false
	}
	for i := 1; i < len(run); i++ {
		if run[i] != run[i-1]+1 {
			return 0, 0, false
		}
	}
	if !holdsOnlyTheEntry(t, info, len(run)) {
		return 0, 0, false
	}
	return run[0], run[len(run)-1], true
}

// holdsOnlyTheEntry reports whether t's files are the entry's whole content and
// nothing else, for a run of runLen TVDB episodes answering anidb AniDB episodes
// (0 when unknown). For a run, the episode census must count one episode per
// AniDB or per TVDB episode, and only a census that recognised none falls back
// to the primary payload count: unrecognised files must not pad a partial run
// to its length. With the count unknown only a film can prove one file is the
// whole entry: a row may name one episode of a several-episode OVA.
func holdsOnlyTheEntry(t *seadex.Torrent, info *EntryInfo, runLen int) bool {
	anidb := info.SpecialsEpisodes
	files := len(payload.Names(t.Files))
	episodes := payload.DistinctEpisodes(payload.Census(t.Files))
	switch {
	case files < 1 || files > max(anidb, runLen, 1) || episodes > max(anidb, runLen):
		return false
	case anidb == 0 && !info.IsMovie:
		return false
	case runLen == 1:
		return true
	case episodes > 0:
		return episodes == anidb || episodes == runLen
	}
	return files == anidb || files == runLen
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

// twinVote is the unanimous fold over the twin title of one identity: every
// holder must name the same title. A holder with no title on this torrent
// vetoes, because another entry listing the torrent means it holds that
// entry's content too; two different titles veto. The fold is in memory at
// every site, so a contested title can never read as agreement.
type twinVote struct {
	title     string
	conflict  bool
	abstained bool
}

func (v *twinVote) add(title string) {
	switch {
	case title == "":
		v.abstained = true
	case v.title == "":
		v.title = title
	case v.title != title:
		v.conflict = true
	}
}

func (v *twinVote) merge(other twinVote) {
	v.abstained = v.abstained || other.abstained
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

func (v twinVote) resolve() string {
	if v.conflict || v.abstained {
		return ""
	}
	return v.title
}

type catalogueTwin struct {
	key, hash, url string
	size           int64
	alID           int
	first, last    int
}

// seriesQueryKey is Sonarr's clean title, case-folded, of a series title or a
// search's q: a leading "the" dropped, "&" read as "and", apostrophes and dots
// removed, each other run of .NET non-word runes one "+" (so "K-On!" is not
// "Kon"), accents folded, edge separators trimmed
// (https://github.com/Sonarr/Sonarr/blob/cab419ade8ac7fcab5bf80394ee492abd35d5f5a/src/NzbDrone.Core/IndexerSearch/Definitions/SearchCriteriaBase.cs#L28-L42).
func seriesQueryKey(title string) string {
	if rest, ok := cutLeadingThe(title); ok {
		title = rest
	}
	title = strings.ReplaceAll(title, "&", "and")
	var b strings.Builder
	for _, r := range title {
		switch {
		case strings.ContainsRune("'.\u0060\u00B4\u2018\u2019", r):
		case isDotNetWordRune(r):
			b.WriteRune(r)
		case !strings.HasSuffix(b.String(), "+"):
			b.WriteByte('+')
		}
	}
	return strings.ToLower(strings.Trim(removeAccents(b.String()), "+ "))
}

// cutLeadingThe is .NET's ^the\s under IgnoreCase, \s being any Unicode space.
func cutLeadingThe(title string) (string, bool) {
	if len(title) < 4 || !strings.EqualFold(title[:3], "the") {
		return title, false
	}
	r, size := utf8.DecodeRuneInString(title[3:])
	if !unicode.IsSpace(r) {
		return title, false
	}
	return title[3+size:], true
}

// isDotNetWordRune is .NET's \w, [\p{L}\p{Mn}\p{Nd}\p{Pc}], over UTF-16: a rune
// past the BMP is two surrogates to .NET, neither a word character
// (https://learn.microsoft.com/en-us/dotnet/standard/base-types/character-classes-in-regular-expressions#word-character-w).
func isDotNetWordRune(r rune) bool {
	return r < 0x10000 && (unicode.IsLetter(r) || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Nd, r) || unicode.Is(unicode.Pc, r))
}

// removeAccents is Sonarr's RemoveAccent: decompose, drop every non-spacing
// mark .NET sees in UTF-16 (none past the BMP), recompose
// (https://github.com/Sonarr/Sonarr/blob/cab419ade8ac7fcab5bf80394ee492abd35d5f5a/src/NzbDrone.Common/Extensions/StringExtensions.cs).
func removeAccents(s string) string {
	return norm.NFC.String(strings.Map(func(r rune) rune {
		if r < 0x10000 && unicode.Is(unicode.Mn, r) {
			return -1
		}
		return r
	}, norm.NFD.String(s)))
}

// animeSpecialsQuery matches the q Sonarr sends an Anime-type series for a
// special with no absolute number: the series, then 00. A specials-season or
// several-specials search sends only that, with no season or ep
// (https://github.com/Sonarr/Sonarr/blob/cab419ade8ac7fcab5bf80394ee492abd35d5f5a/src/NzbDrone.Core/Indexers/Newznab/NewznabRequestGenerator.cs#L401-L468).
var animeSpecialsQuery = regexp.MustCompile(`^(.*\S)\s+00$`)

// specialsRequest reads a Sonarr special search: the series it names and the
// episode it asks for, 0 for every special (the anime "<series> 00" query).
func specialsRequest(q url.Values) (series string, ep int, ok bool) {
	season, episode := strings.TrimSpace(q.Get("season")), strings.TrimSpace(q.Get("ep"))
	if isSpecialsSeason(season) {
		ep, err := strconv.Atoi(episode)
		return q.Get("q"), ep, err == nil && ep >= 1
	}
	m := animeSpecialsQuery.FindStringSubmatch(strings.TrimSpace(q.Get("q")))
	if season != "" || episode != "" || m == nil {
		return "", 0, false
	}
	return m[1], 0, true
}

// answerSpecial answers a Sonarr special search ahead of proxied, so the item
// cap cannot crowd it out: first the proxied twins of curated releases whose
// run holds the episode, which carry live peers, then the twins only the
// curation holds, since a tracker query for "<Series> S00Exx" cannot find a
// release named after its film. A catalogue twin is folded by lookup like a
// proxied result and needs a link this scope can build. added counts those.
func (c *curation) answerSpecial(q url.Values, scope, abPasskey string, proxied []item) (out []item, added int) {
	series, ep, ok := specialsRequest(q)
	if !ok {
		return proxied, 0
	}
	candidates := c.twins[seriesQueryKey(series)]
	if len(candidates) == 0 {
		return proxied, 0
	}
	promoted, rendered := c.specialAnswer(candidates, ep, scope, abPasskey, proxied)
	return frontLoad(proxied, promoted, rendered), len(rendered)
}

func (c *curation) specialAnswer(candidates []catalogueTwin, ep int, scope, abPasskey string, proxied []item) (promoted []bool, rendered []item) {
	byIdentity := proxiedIdentities(proxied)
	promoted = make([]bool, len(proxied))
	for i := range candidates {
		ct := &candidates[i]
		if scopeOfKey(ct.key) != scope || (ep > 0 && (ep < ct.first || ep > ct.last)) {
			continue
		}
		served := slices.Concat(byIdentity[ct.key], byIdentity[ct.hash])
		for _, j := range served {
			promoted[j] = promoted[j] || isTwinGUID(proxied[j].GUID)
		}
		if len(served) > 0 {
			continue
		}
		if it, ok := c.renderCatalogueTwin(ct, scope, abPasskey); ok {
			rendered = append(rendered, it)
		}
	}
	return promoted, rendered
}

func frontLoad(proxied []item, promoted []bool, rendered []item) []item {
	out := make([]item, 0, len(proxied)+len(rendered))
	for j := range proxied {
		if promoted[j] {
			out = append(out, proxied[j])
		}
	}
	out = append(out, rendered...)
	for j := range proxied {
		if !promoted[j] {
			out = append(out, proxied[j])
		}
	}
	return out
}

func (c *curation) renderCatalogueTwin(ct *catalogueTwin, scope, abPasskey string) (item, bool) {
	verdict := c.lookup(scope, ct.hash, ct.url, ct.url)
	dl, resolved := downloadURLForScope(scope, ct.url, abPasskey)
	if !verdict.matched || verdict.sonarrTitle == "" || !resolved {
		return item{}, false
	}
	// No PubDate: the curation holds no torrent date, and the zero date renders
	// as the epoch, which neither moves between snapshots nor holds a delay profile.
	it := item{
		Title: verdict.sonarrTitle, GUID: twinGUID(ct.url), DownloadURL: dl, InfoHash: ct.hash,
		Size: ct.size, TvdbID: verdict.tvdbID, Categories: []int{catAnime},
		DownloadVolumeFactor: dvfAlt,
	}
	if verdict.isBest {
		it.DownloadVolumeFactor = dvfBest
	}
	if ct.alID > 0 {
		it.InfoURL = entryURL(ct.alID)
	}
	return it, true
}

// proxiedIdentities indexes items by tracker key and info hash; a twin keys to
// its original's identity (twinFragment).
func proxiedIdentities(items []item) map[string][]int {
	ids := make(map[string][]int, 2*len(items))
	for i := range items {
		it := &items[i]
		for _, id := range []string{trackerKeyFromURL(it.InfoURL), trackerKeyFromURL(it.GUID), validInfoHash(it.InfoHash)} {
			if id != "" && !slices.Contains(ids[id], i) {
				ids[id] = append(ids[id], i)
			}
		}
	}
	return ids
}
