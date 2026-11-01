package mapping

import (
	"cmp"
	"fmt"
	"maps"
	"slices"

	"github.com/cplieger/animap/mapread"
)

// DefaultRepo is the repository whose dated releases carry animap.json, the
// AniList-to-arr ID map.
const DefaultRepo = "cplieger/animap"

// SeasonRange is entry episodes First to Last, all landing on TVDB season
// Season.
type SeasonRange struct {
	Season int `json:"season"`
	First  int `json:"first"`
	Last   int `json:"last"`
}

// Mapping is where an entry's episodes land on its one TVDB series beyond
// the season its Record names. Episode lists ascend.
type Mapping struct {
	// Seasons are each positive season's maximal runs of entry episodes
	// landing on SeasonsTvdb, default placements included; an episode the
	// map states nothing for is in none.
	Seasons []SeasonRange `json:"seasons,omitempty"`
	// Specials are the season-0 episodes of SpecialsTvdb the entry is, when
	// every entry episode is stated there or lands nowhere; SpecialsEpisodes
	// counts the entry episodes they answer, 0 when some land nowhere.
	Specials []int `json:"specials,omitempty"`
	// Episodes are the episodes of positive season EpisodesSeason of
	// EpisodesTvdb the entry is, when every entry episode is stated there,
	// and none lands on season 0.
	Episodes         []int `json:"episodes,omitempty"`
	SeasonsTvdb      int   `json:"seasons_tvdb,omitempty"`
	SpecialsTvdb     int   `json:"specials_tvdb,omitempty"`
	SpecialsEpisodes int   `json:"specials_episodes,omitempty"`
	EpisodesTvdb     int   `json:"episodes_tvdb,omitempty"`
	EpisodesSeason   int   `json:"episodes_season,omitempty"`
	// SpecialEpisode is the one season-0 episode of SpecialsTvdb that every
	// landing names, when every entry episode lands there, at least one
	// landing is stated, and a default among them withholds Specials.
	SpecialEpisode int `json:"special_episode,omitempty"`
}

func (m *Mapping) empty() bool {
	return m.SpecialEpisode == 0 && len(m.Seasons) == 0 && len(m.Specials) == 0 && len(m.Episodes) == 0
}

// SpecialRun is the one reader of which TVDB season-0 episodes the whole
// entry is, with the entry episode count they answer: Specials with its
// count, else SpecialEpisode with a count of 0 (unknown); nil when the map
// names neither or some entry episode lands nowhere.
func (m *Mapping) SpecialRun() (episodes []int, entryEpisodes int) {
	switch {
	case len(m.Specials) > 0 && m.SpecialsEpisodes > 0:
		return slices.Clone(m.Specials), m.SpecialsEpisodes
	case m.SpecialEpisode > 0:
		return []int{m.SpecialEpisode}, 0
	}
	return nil, 0
}

// projection is a decoded document as the Index serves it: a Record per
// AniList entry; the facts of each anime on a TVDB series, keyed by AniDB
// id, which an entry that is one whole anime and an override naming that id
// join on; the facts of every other entry, keyed by AniList id; the keys of
// each space read fully stated on their series (tvdbView.fullyStated); and
// the regular episodes its anime on TVDB place, which maxAnimeEpisodes caps.
type projection struct {
	mappings           map[int]Mapping
	aniListMappings    map[int]Mapping
	fullyStated        map[int]struct{}
	aniListFullyStated map[int]struct{}
	records            []Record
	animeEpisodes      int
}

// maxAnimeEpisodes bounds the regular episodes the anime facts expand, which
// mapread's MaxEpisodes, a cap on the entries' parts, does not: 2^21, about
// 16 times the 134,060 of the 2026-10 document's anime on TVDB.
const maxAnimeEpisodes = 1 << 21

// project reads doc as the Index serves it, refusing with mapread.ErrTooLarge
// a document whose anime on TVDB place more than budget regular episodes.
func project(doc *mapread.Document, budget int) (projection, error) {
	p := projection{
		mappings: make(map[int]Mapping), aniListMappings: make(map[int]Mapping),
		fullyStated: make(map[int]struct{}), aniListFullyStated: make(map[int]struct{}),
	}
	var series []int
	for i := range doc.Anime {
		if len(doc.Anime[i].TVDBSeries()) == 0 {
			continue
		}
		if p.animeEpisodes += doc.Anime[i].Last.Regular; p.animeEpisodes > budget {
			return projection{}, fmt.Errorf("%w: the anime on TVDB place more than %d regular episodes", mapread.ErrTooLarge, budget)
		}
		series = append(series, doc.Anime[i].AniDBID)
	}
	x := mapread.NewIndex(doc)
	for _, aid := range series {
		p.addAnime(x, aid)
	}
	p.records = make([]Record, 0, len(doc.Entries))
	for i := range doc.Entries {
		p.addEntry(x, doc.Entries[i].AniListID)
	}
	return p, nil
}

// addAnime keys anime aid's facts by its AniDB id.
func (p *projection) addAnime(x *mapread.Index, aid int) {
	_, m, stated := entryFacts(x, mapread.Entry{Parts: []mapread.Part{{AniDBID: aid, Kind: mapread.Regular}}})
	if !m.empty() {
		p.mappings[aid] = m
	}
	if stated {
		p.fullyStated[aid] = struct{}{}
	}
}

// addEntry appends AniList entry al's Record, keying its facts by AniList id
// when it is not one whole anime, whose facts addAnime keys.
func (p *projection) addEntry(x *mapread.Index, al int) {
	e, _ := x.Entry(al)
	rec, m, stated := entryFacts(x, e)
	if rec.AniDBID == 0 {
		if !m.empty() {
			p.aniListMappings[al] = m
		}
		if stated {
			p.aniListFullyStated[al] = struct{}{}
		}
	}
	p.records = append(p.records, rec)
}

// entryFacts projects one entry onto its Record and Mapping, and reports
// whether the entry is fully stated on its series. The TVDB id, season and
// facts exist only for an entry whose parts reach exactly one TVDB series:
// the arrs key a series by one id.
func entryFacts(x *mapread.Index, e mapread.Entry) (rec Record, m Mapping, stated bool) {
	routes := x.Routes(e)
	rec = Record{SeasonKind: SeasonAbsent, AniListID: e.AniListID, AniDBID: wholeAnime(e), TmdbMovies: radarrRoute(routes)}
	if len(routes.TVDB) == 1 {
		rec.TvdbID = routes.TVDB[0]
		v := readTVDB(x, e, rec.TvdbID)
		rec.SeasonKind, rec.SeasonTvdb = v.season()
		m, stated = v.mapping(), v.fullyStated()
	}
	rec.canonicalize()
	return rec, m, stated
}

// radarrRoute is the one home of an entry's Radarr route: the TMDB movies
// every one of its episodes states, so the entry is wholly that film.
func radarrRoute(routes mapread.Routes) []int { return routes.TMDBMovies }

// wholeAnime is the AniDB anime e is all the regular episodes of, 0 when its
// parts say anything else.
func wholeAnime(e mapread.Entry) int {
	if len(e.Parts) == 1 && e.Parts[0].Identity() {
		return e.Parts[0].AniDBID
	}
	return 0
}

// tvdbView is an entry's episodes read on its one TVDB series, each landing
// on seasons there, unstated or landing nowhere, as mapread.TVDBReading.On
// reads it.
type tvdbView struct {
	x        *mapread.Index
	landings map[int][]int
	episodes []mapread.EntryEpisode
	series   int
	unstated int
	nowhere  int
	// implicit counts the nowhere episodes that are off TVDB or not
	// listedNowhere.
	implicit int
	// defaulted is a landing that rests on a default alone.
	defaulted bool
}

// readTVDB reads e on series, which must be the one series e's routes
// reach: targets takes every placement to be on it.
func readTVDB(x *mapread.Index, e mapread.Entry, series int) *tvdbView {
	v := &tvdbView{x: x, series: series, episodes: x.Episodes(e), landings: make(map[int][]int)}
	for _, ep := range v.episodes {
		r := x.OnTVDB(ep)
		on := r.On(series)
		switch {
		case len(on.Seasons) > 0:
			for _, b := range on.Seasons {
				v.landings[b.Season] = append(v.landings[b.Season], ep.Number)
				v.defaulted = v.defaulted || b.Default
			}
		case on.Unstated:
			v.unstated++
		default:
			v.nowhere++
			// Off TVDB lands nowhere but carries no TVDB statement, so it is
			// no evidence the map states it.
			if r.Off || !listedNowhere(x, ep) {
				v.implicit++
			}
		}
	}
	return v
}

// listedNowhere reports that AniDB's list holds every AniDB episode of ep,
// so each sits in a run, and one landing nowhere there names TVDB as [].
func listedNowhere(x *mapread.Index, ep mapread.EntryEpisode) bool {
	for _, ref := range ep.Refs {
		if st, _ := x.Episode(ref.Ref); st.Listing != mapread.Listed {
			return false
		}
	}
	return true
}

// fullyStated reports that the map states where every entry episode lands:
// no default, no unstated episode, and landing nowhere only through a
// listed run's tvdb []. A number no run covers is no evidence, because a
// document that lost its runs reads it the same way; this is the evidence
// validateFacts accepts for a fact the release no longer gives.
func (v *tvdbView) fullyStated() bool {
	return len(v.episodes) > 0 && !v.defaulted && v.unstated == 0 && v.implicit == 0
}

// positive is the positive seasons some entry episode lands on, ascending.
func (v *tvdbView) positive() []int {
	var out []int
	for s := range v.landings {
		if s > 0 {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

// season is the Record's season: one positive season scopes the entry to
// it, and an entry landing only on season 0 or nowhere is offered on season
// 0. An unstated episode, two positive seasons or no episode at all leave it
// to a whole-series comparison.
func (v *tvdbView) season() (kind SeasonKind, season int) {
	switch positive := v.positive(); {
	case len(v.episodes) == 0 || v.unstated > 0 || len(positive) > 1:
		return SeasonAbsent, 0
	case len(positive) == 1:
		return SeasonPresent, positive[0]
	}
	return SeasonPresent, 0
}

func (v *tvdbView) mapping() Mapping {
	m := v.seasons()
	if v.unstated > 0 {
		return m
	}
	positive := v.positive()
	_, onZero := v.landings[0]
	switch {
	case len(positive) == 0 && onZero && !v.defaulted:
		m.Specials, _ = v.targets(0)
		m.SpecialsTvdb = v.series
		if v.nowhere == 0 {
			m.SpecialsEpisodes = len(v.episodes)
		}
	case len(positive) == 0 && onZero && v.nowhere == 0:
		if eps, stated := v.targets(0); len(eps) == 1 && stated {
			m.SpecialEpisode, m.SpecialsTvdb = eps[0], v.series
		}
	case len(positive) == 1 && !onZero && v.nowhere == 0 && !v.defaulted:
		m.Episodes, _ = v.targets(positive[0])
		m.EpisodesTvdb, m.EpisodesSeason = v.series, positive[0]
	}
	return m
}

func (v *tvdbView) seasons() Mapping {
	r := v.ranges()
	if len(r) == 0 {
		return Mapping{}
	}
	return Mapping{Seasons: r, SeasonsTvdb: v.series}
}

// ranges splits each positive season's entry episodes into maximal runs, so
// a range never claims an episode that lands elsewhere, ordered by first
// episode.
func (v *tvdbView) ranges() []SeasonRange {
	var out []SeasonRange
	for _, s := range v.positive() {
		eps := v.landings[s]
		for i := 0; i < len(eps); {
			j := i
			for j+1 < len(eps) && eps[j+1] == eps[j]+1 {
				j++
			}
			out = append(out, SeasonRange{Season: s, First: eps[i], Last: eps[j]})
			i = j + 1
		}
	}
	slices.SortFunc(out, func(a, b SeasonRange) int {
		return cmp.Or(cmp.Compare(a.First, b.First), cmp.Compare(a.Season, b.Season))
	})
	return out
}

// targets is every TVDB episode of season on the view's series the entry's
// episodes land on, ascending, and whether one of those landings is stated.
func (v *tvdbView) targets(season int) (episodes []int, stated bool) {
	seen := make(map[int]struct{})
	for _, ep := range v.episodes {
		for _, ref := range ep.Refs {
			st, ok := v.x.Episode(ref.Ref)
			if !ok || st.TVDB == nil {
				continue
			}
			for _, p := range st.TVDB.Placements {
				if p.Season == season {
					seen[p.Episode] = struct{}{}
					stated = stated || !p.Default
				}
			}
		}
	}
	return slices.Sorted(maps.Keys(seen)), stated
}
