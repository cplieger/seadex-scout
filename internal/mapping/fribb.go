package mapping

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
)

// Fribb type strings. MOVIE routes to Radarr (TMDB movie / IMDb); every other
// type routes to Sonarr (TVDB).
const typeMovie = "MOVIE"

// nullLiteral is the JSON null token, checked before decoding tolerant fields.
const nullLiteral = "null"

// fribbRecord mirrors one element of the Fribb anime-list-mini.json array. The
// fields whose upstream shape varies (an id that may be a number or a string,
// an imdb id that may be a scalar or an array, a themoviedb id that may be a
// {tv}/{movie[]} object) use tolerant decoders so one odd record cannot break
// the whole map.
type fribbRecord struct {
	Season        offsetPair `json:"season"`
	EpisodeOffset offsetPair `json:"episode_offset"`
	Type          string     `json:"type"`
	IMDbID        stringList `json:"imdb_id"`
	TmdbID        tmdbID     `json:"themoviedb_id"`
	AniListID     flexInt    `json:"anilist_id"`
	TvdbID        flexInt    `json:"tvdb_id"`
}

// toRecord converts a decoded Fribb record into a public Record, normalizing
// the type to upper case. It returns ok=false when the record has no AniList
// ID (nothing to key the SeaDex lookup on).
func (r *fribbRecord) toRecord() (Record, bool) {
	if int(r.AniListID) == 0 {
		return Record{}, false
	}
	return Record{
		IMDbIDs:    r.IMDbID,
		TmdbMovies: intSlice(r.TmdbID.Movie),
		Type:       strings.ToUpper(strings.TrimSpace(r.Type)),
		AniListID:  int(r.AniListID),
		TvdbID:     int(r.TvdbID),
		TmdbTV:     int(r.TmdbID.TV),
		SeasonTvdb: r.Season.tvdbOr(),
		OffsetTvdb: r.EpisodeOffset.tvdbOr(),
	}, true
}

// parseFribb decodes the Fribb list resiliently: it reads the top-level array
// as raw messages, then decodes each element on its own so a single malformed
// record is skipped (counted) rather than failing the whole map.
func parseFribb(data []byte, log *slog.Logger) ([]Record, error) {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	records := make([]Record, 0, len(raw))
	skipped := 0
	for _, msg := range raw {
		var fr fribbRecord
		if err := json.Unmarshal(msg, &fr); err != nil {
			skipped++
			continue
		}
		if rec, ok := fr.toRecord(); ok {
			records = append(records, rec)
		}
	}
	if skipped > 0 {
		log.Warn("mapping: skipped unparseable records", "skipped", skipped, "parsed", len(records))
	}
	return records, nil
}

// offsetPair is the {tvdb, tmdb} shape of the season and episode_offset fields.
type offsetPair struct {
	Tvdb *int `json:"tvdb"`
	Tmdb *int `json:"tmdb"`
}

// tvdbOr returns the tvdb value or 0 when absent.
func (o offsetPair) tvdbOr() int {
	if o.Tvdb != nil {
		return *o.Tvdb
	}
	return 0
}

// tmdbID decodes the themoviedb_id field, which is a {"tv":int} or
// {"movie":[int]} object in the merged list. A non-object shape (a bare number
// or the "unknown" string that appears in some upstream rows) is tolerated and
// left empty, since it cannot be disambiguated into a tv-vs-movie id; such an
// entry still matches via tvdb_id (TV) or imdb_id (movie).
type tmdbID struct {
	Movie []flexInt `json:"movie"`
	TV    flexInt   `json:"tv"`
}

// UnmarshalJSON decodes the object form and tolerates any other shape as empty.
func (t *tmdbID) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == nullLiteral || b[0] != '{' {
		return nil
	}
	type alias tmdbID
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return nil //nolint:nilerr // tolerate an odd themoviedb_id shape rather than fail the record
	}
	*t = tmdbID(a)
	return nil
}

// flexInt decodes a JSON number or numeric string into an int. A null, empty,
// "unknown", or non-numeric value decodes to 0 rather than erroring, so an
// upstream placeholder does not break the record.
type flexInt int

// UnmarshalJSON implements the tolerant number-or-string decode.
func (f *flexInt) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == nullLiteral {
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			*f = flexInt(n)
		}
		return nil
	}
	var n float64
	if err := json.Unmarshal(b, &n); err != nil {
		return nil //nolint:nilerr // tolerate a non-numeric id placeholder
	}
	*f = flexInt(int(n))
	return nil
}

// stringList decodes a JSON array of strings, a single string, or null into a
// []string, trimming blanks. The imdb_id field is an array in the merged list
// but a scalar in some upstream rows.
type stringList []string

// UnmarshalJSON implements the array-or-scalar decode.
func (s *stringList) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == nullLiteral {
		return nil
	}
	if b[0] == '[' {
		var arr []string
		if err := json.Unmarshal(b, &arr); err != nil {
			return err
		}
		*s = trimmed(arr)
		return nil
	}
	var one string
	if err := json.Unmarshal(b, &one); err != nil {
		return nil //nolint:nilerr // tolerate an odd imdb_id shape rather than fail the record
	}
	*s = trimmed([]string{one})
	return nil
}

// trimmed returns in with entries trimmed and blanks dropped.
func trimmed(in []string) []string {
	var out []string
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// intSlice converts a []flexInt to a []int, dropping zero entries.
func intSlice(in []flexInt) []int {
	var out []int
	for _, v := range in {
		if int(v) != 0 {
			out = append(out, int(v))
		}
	}
	return out
}
