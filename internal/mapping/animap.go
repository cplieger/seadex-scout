package mapping

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"

	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/jsoncap/v2"
	"github.com/cplieger/runesafe/v2"
	"github.com/cplieger/seadex-scout/internal/mediatype"
)

// DefaultURL is the newest published animap.json, the AniList-to-arr ID map. The
// release-download URL redirects through the release tag to a CDN, which answers
// the conditional GET's validators; a new release is a new asset with a new ETag.
const DefaultURL = "https://github.com/cplieger/animap/releases/latest/download/animap.json"

// releaseRedirects admits DefaultURL's two hops: the release tag on github.com,
// then GitHub's release-asset host (objects. is the host it served from before).
var releaseRedirects = httpx.RedirectPolicyFunc(httpx.WithAllowedHosts("github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com"))

// errRedirectRefused marks a refused redirect hop. Persistent: the URL is
// constant, so a refused hop re-refuses on every load until the app is upgraded.
var errRedirectRefused = errors.New("mapping: redirect refused")

// RedirectPolicy is the CheckRedirect for the client that fetches DefaultURL. It
// follows GitHub's release-download redirect and refuses every other host.
func RedirectPolicy(req *http.Request, via []*http.Request) error {
	if err := releaseRedirects(req, via); err != nil {
		return fmt.Errorf("%w: %w", errRedirectRefused, err)
	}
	return nil
}

// supportedVersion is the animap.json schema version this decoder reads. The
// producer raises it only for a breaking format change, so any other version is
// refused rather than read with today's field meanings.
const supportedVersion = 1

// RecordFromFormat builds the type-only Record a consumer uses to reuse the
// arr/season routing decisions for an AniList format that has no mapping
// record. Its season kind stays UNKNOWN rather than absent: no upstream said
// anything about a season here, and absent would route a title-matched OVA to a
// whole-series comparison against every real season.
func RecordFromFormat(format string) Record { return Record{Type: mediatype.Normalize(format)} }

// SeasonRange is one TVDB season an absolute-numbered run's episodes fall
// into: episodes First..Last of the run are TVDB season Season. Last == 0 means
// the range is open-ended (the last row carries a start and no end while the
// show is airing).
type SeasonRange struct {
	Season int `json:"season"`
	First  int `json:"first"`
	Last   int `json:"last,omitempty"`
}

// Mapping is what a record's mapping list says beyond its ids: which TVDB
// season-0 episode a film filed in a series' specials IS (SpecialEpisode, 0
// when the list names none), and which TVDB seasons an absolute-numbered run's
// episodes fall into (Seasons, nil when it carries no ranged rows).
type Mapping struct {
	Seasons        []SeasonRange `json:"seasons,omitempty"`
	SpecialEpisode int           `json:"special_episode,omitempty"`
}

func (m *Mapping) empty() bool { return m.SpecialEpisode == 0 && len(m.Seasons) == 0 }

// maxRecords is a hard acceptance cap on the document's records array, not a
// preallocation hint: the body cap still admits ~1M tiny valid records.
const maxRecords = 1 << 16

// errRecordCapExceeded rejects a document over maxRecords. A sentinel, because a
// permanently over-cap document never self-heals and must advance the rejection
// streak instead of degrading at WARN forever.
var errRecordCapExceeded = fmt.Errorf("mapping: animap records exceed cap %d", maxRecords)

// errNotAnimapDocument rejects a body that is not an animap document: not a JSON
// object, no records array, or a member of the wrong shape. Content-shape
// evidence, so it never self-heals; mid-stream truncation stays transient.
var errNotAnimapDocument = errors.New("mapping: body is not an animap document")

// errUnsupportedVersion rejects a document whose schema version this decoder
// does not read.
var errUnsupportedVersion = errors.New("mapping: unsupported animap schema version")

// maxRecordBytes bounds one encoded record before its decode; a real record is
// under 4 KiB, and an oversized one is skipped as malformed.
const maxRecordBytes = 64 << 10

// maxRecordIdentifiers caps each record's IMDb and TMDB movie lists; a list over
// it rejects the record.
const maxRecordIdentifiers = 32

// maxRetainedTotal bounds the identifiers and season ranges retained across the
// whole document; the per-record caps alone still admit millions.
const maxRetainedTotal = 1 << 20

// errIdentifierBudgetExceeded rejects a document over maxRetainedTotal. A
// sentinel, because the ceiling truncates the TAIL: serving the prefix would
// publish a knowably incomplete map that every count floor still passes.
var errIdentifierBudgetExceeded = fmt.Errorf("mapping: animap retained values exceed cap %d", maxRetainedTotal)

// animapRecord is one element of the document's records array, decoded
// strictly per field: a member of the wrong JSON type rejects its record.
type animapRecord struct {
	TVDBSeason   *int          `json:"tvdb_season"`
	AniDBParent  *animapParent `json:"anidb_parent"`
	Type         string        `json:"type"`
	IMDbIDs      []string      `json:"imdb_ids"`
	TMDBMovieIDs []int         `json:"tmdb_movie_ids"`
	MappingList  []animapRow   `json:"mapping_list"`
	AniListID    int           `json:"anilist_id"`
	AniDBID      int           `json:"anidb_id"`
	TVDBID       int           `json:"tvdb_id"`
}

// animapParent is a record's anidb_parent: the anime AniDB files it under as
// specials. Only its id is read; its presence is what marks the record.
type animapParent struct {
	AniDBID int `json:"anidb_id"`
}

func (r *animapRecord) parentSpecial() bool {
	return r.AniDBID <= 0 && r.AniListID > 0 && r.AniDBParent != nil && r.AniDBParent.AniDBID > 0
}

// animapRow is one mapping_list row. Each Episodes element is [anidb, target...]:
// one element means no counterpart, three mean one episode spans two targets.
type animapRow struct {
	TVDBSeason  *int    `json:"tvdb_season"`
	Episodes    [][]int `json:"episodes"`
	AniDBSeason int     `json:"anidb_season"`
	Start       int     `json:"start"`
	End         int     `json:"end"`
}

// toRecord converts a decoded record into its canonical Record. The season is
// present iff the document carries tvdb_season, which animap publishes only
// beside a tvdb_id; tvdb_absolute and episodes have no Record field and route
// nothing.
func (r *animapRecord) toRecord() Record {
	kind, season := SeasonAbsent, 0
	if r.TVDBSeason != nil && *r.TVDBSeason >= 0 {
		kind, season = SeasonPresent, *r.TVDBSeason
	}
	rec := Record{
		IMDbIDs:    r.IMDbIDs,
		TmdbMovies: r.TMDBMovieIDs,
		Type:       r.Type,
		SeasonKind: kind,
		AniListID:  r.AniListID,
		TvdbID:     r.TVDBID,
		AniDBID:    r.AniDBID,
		SeasonTvdb: season,
	}
	rec.canonicalize()
	return rec
}

func (r *animapRecord) mapping() Mapping {
	return Mapping{SpecialEpisode: r.filmEpisode(), Seasons: seasonRanges(r.MappingList)}
}

// filmEpisode reads which TVDB season-0 episode a film filed in a series'
// specials IS, and 0 when the record names none. The record gate is
// load-bearing: a series can carry an identically shaped season-0 row for one
// of its OWN specials, so only a record filed under the specials (tvdb_season 0)
// is read, or a false episode lands on a TV series. The rows are its regular
// episodes' single-episode rows (no start), and every one must name the same
// positive episode.
func (r *animapRecord) filmEpisode() int {
	if r.TVDBSeason == nil || *r.TVDBSeason != 0 {
		return 0
	}
	episode := 0
	for i := range r.MappingList {
		row := &r.MappingList[i]
		if row.AniDBSeason != 1 || row.TVDBSeason == nil || *row.TVDBSeason != 0 || row.Start != 0 {
			continue
		}
		for _, pair := range row.Episodes {
			if len(pair) != 2 || pair[1] <= 0 || (episode != 0 && pair[1] != episode) {
				return 0
			}
			episode = pair[1]
		}
	}
	return episode
}

// seasonRanges reads which TVDB seasons an absolute-numbered run's episodes fall
// into: every row rangeOf admits, sorted by first episode. When the lowest range
// starts above episode 1 the uncovered leading run is prepended as the season
// below the lowest one named (floor 1): rows that start at season 2 state that
// the earlier episodes are TVDB season 1.
func seasonRanges(rows []animapRow) []SeasonRange {
	var out []SeasonRange
	for i := range rows {
		if r, ok := rangeOf(&rows[i]); ok {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil
	}
	slices.SortFunc(out, func(a, b SeasonRange) int {
		if c := a.First - b.First; c != 0 {
			return c
		}
		return a.Season - b.Season
	})
	lowestSeason := out[0].Season
	for _, r := range out[1:] {
		lowestSeason = min(lowestSeason, r.Season)
	}
	if first := out[0].First; first > 1 {
		out = slices.Insert(out, 0, SeasonRange{Season: max(1, lowestSeason-1), First: 1, Last: first - 1})
	}
	return out
}

// rangeOf reads one row as a TVDB season range, false for every row that is not
// one: a single-episode row, a TMDB row, a specials row or a season-0 target.
// TMDB rows are excluded because the two tables disagree on season boundaries.
func rangeOf(row *animapRow) (SeasonRange, bool) {
	if row.AniDBSeason != 1 || row.Start < 1 || row.TVDBSeason == nil || *row.TVDBSeason < 1 {
		return SeasonRange{}, false
	}
	return SeasonRange{Season: *row.TVDBSeason, First: row.Start, Last: row.End}, true
}

// animapParseResult is parseAnimap's counted decode result. elements counts every
// records-array element observed whatever its outcome, so the acceptance floors
// cannot shrink their denominator with their numerator.
type animapParseResult struct {
	mappings       map[int]Mapping
	parentMappings map[int]Mapping
	records        []Record
	elements       int
}

func (p *animapParseResult) listFacts() int { return len(p.mappings) + len(p.parentMappings) }

// parseAnimap decodes an animap.json body. The records array is streamed element
// by element, each decoded on its own so a malformed record is skipped (counted)
// rather than failing the map. A document over maxRecords or maxRetainedTotal,
// one that is not an animap document, and one of another schema version are
// rejected with their sentinels; trailing data after the document is rejected.
func parseAnimap(data []byte, log *slog.Logger) (animapParseResult, error) {
	dec := jsoncap.NewDecoder(bytes.NewReader(data), 0)
	ok, err := dec.Open('{')
	if err != nil {
		if errors.Is(err, io.EOF) {
			// No first token is not content-shape evidence: a zero-length 200 can
			// succeed on the next attempt, so it stays a transient parse failure.
			return animapParseResult{}, fmt.Errorf("mapping: animap body is empty: %w", err)
		}
		return animapParseResult{}, fmt.Errorf("%w: %w", errNotAnimapDocument, err)
	}
	if !ok {
		return animapParseResult{}, fmt.Errorf("%w (got null)", errNotAnimapDocument)
	}
	doc, err := decodeDocumentMembers(dec)
	if err != nil {
		return animapParseResult{}, err
	}
	if err := dec.Close(); err != nil {
		return animapParseResult{}, fmt.Errorf("mapping: animap document truncated or malformed at close: %w", err)
	}
	if err := dec.End(); err != nil {
		return animapParseResult{}, fmt.Errorf("mapping: trailing data after animap document: %w", err)
	}
	if !doc.sawRecords {
		return animapParseResult{}, fmt.Errorf("%w: no records array", errNotAnimapDocument)
	}
	if doc.version != supportedVersion {
		return animapParseResult{}, fmt.Errorf("%w %d, want %d", errUnsupportedVersion, doc.version, supportedVersion)
	}
	logParseDiagnostics(log, &doc.counts)
	c := &doc.counts
	return animapParseResult{records: c.records, mappings: c.mappings, parentMappings: c.parentMappings, elements: c.elements}, nil
}

type documentMembers struct {
	counts     decodeCounts
	version    int
	sawVersion bool
	sawRecords bool
}

func decodeDocumentMembers(dec *jsoncap.Decoder) (documentMembers, error) {
	var doc documentMembers
	for dec.More() {
		key, err := dec.Key()
		if err != nil {
			return documentMembers{}, fmt.Errorf("mapping: animap document key: %w", err)
		}
		if err := doc.member(dec, key); err != nil {
			return documentMembers{}, err
		}
	}
	return doc, nil
}

// member decodes one top-level member. A repeated records or version member is
// refused: two answers to one question is not a document.
func (doc *documentMembers) member(dec *jsoncap.Decoder, key string) error {
	switch key {
	case "version":
		if doc.sawVersion {
			return fmt.Errorf("%w: repeated version", errNotAnimapDocument)
		}
		doc.sawVersion = true
		if err := dec.Decode(&doc.version); err != nil {
			return fmt.Errorf("%w: version: %w", errNotAnimapDocument, err)
		}
	case "records":
		if doc.sawRecords {
			return fmt.Errorf("%w: repeated records", errNotAnimapDocument)
		}
		doc.sawRecords = true
		counts, err := decodeRecords(dec)
		if err != nil {
			return err
		}
		doc.counts = counts
	default:
		if err := dec.Skip(); err != nil {
			return fmt.Errorf("mapping: animap document member %q: %w", key, err)
		}
	}
	return nil
}

func logParseDiagnostics(log *slog.Logger, counts *decodeCounts) {
	if counts.skipped > 0 {
		attrs := []any{"skipped", counts.skipped, "parsed", len(counts.records)}
		if counts.firstErr != nil {
			// Untrusted-input-derived, so it passes maxLoggedErrorBytes.
			attrs = append(attrs, "error",
				errors.New(runesafe.SanitizeSingleLineBounded(counts.firstErr.Error(), maxLoggedErrorBytes)))
		}
		log.Warn("mapping: skipped malformed records", attrs...)
	}
	if counts.anidbOnly > 0 {
		log.Debug("mapping: records without anilist_id kept for their mapping list only", "records", counts.anidbOnly, "parsed", len(counts.records))
	}
	// Advance warnings before a cap becomes a hard refusal, which freezes the map
	// stale until the cap is raised.
	if counts.elements >= maxRecords/4*3 {
		log.Warn("mapping: animap records approaching record cap", "elements", counts.elements, "cap", maxRecords)
	}
	if counts.retained >= maxRetainedTotal/4*3 {
		log.Warn("mapping: animap retained values approaching budget", "identifiers", counts.retained, "cap", maxRetainedTotal)
	}
}

func decodeRecords(dec *jsoncap.Decoder) (decodeCounts, error) {
	ok, err := dec.Open('[')
	if err != nil {
		return decodeCounts{}, fmt.Errorf("%w: records: %w", errNotAnimapDocument, err)
	}
	if !ok {
		return decodeCounts{}, fmt.Errorf("%w: records is null", errNotAnimapDocument)
	}
	counts := decodeCounts{mappings: make(map[int]Mapping), parentMappings: make(map[int]Mapping)}
	for seen := 0; dec.More(); seen++ {
		if seen == maxRecords {
			return decodeCounts{}, errRecordCapExceeded
		}
		counts.elements++
		var msg json.RawMessage
		if err := dec.Decode(&msg); err != nil {
			return decodeCounts{}, fmt.Errorf("mapping: animap records stream decode: %w", err)
		}
		if err := counts.add(msg); err != nil {
			return decodeCounts{}, err
		}
	}
	if err := dec.Close(); err != nil {
		return decodeCounts{}, fmt.Errorf("mapping: animap records truncated or malformed at close: %w", err)
	}
	return counts, nil
}

type decodeCounts struct {
	firstErr       error
	mappings       map[int]Mapping
	parentMappings map[int]Mapping
	records        []Record
	elements       int
	skipped        int
	anidbOnly      int
	retained       int
}

// add folds one raw record in: a decode failure counts as skipped, keeping the
// first error; a record breaching the retained budget fails the document. A
// record's mapping-list facts are keyed by its AniDB id, or by its AniList id
// when it is a specials-of-parent record; any other record's facts join nothing.
func (c *decodeCounts) add(msg json.RawMessage) error {
	r, err := decodeRecord(msg)
	if err != nil {
		c.skipped++
		if c.firstErr == nil {
			c.firstErr = err
		}
		return nil
	}
	rec := r.toRecord()
	m := r.mapping()
	n := len(rec.IMDbIDs) + len(rec.TmdbMovies) + len(m.Seasons)
	if c.retained+n > maxRetainedTotal {
		return errIdentifierBudgetExceeded
	}
	c.retained += n
	if !m.empty() {
		switch {
		case rec.AniDBID > 0:
			c.mappings[rec.AniDBID] = m
		case r.parentSpecial():
			c.parentMappings[rec.AniListID] = m
		}
	}
	if rec.AniListID <= 0 {
		c.anidbOnly++
		return nil
	}
	c.records = append(c.records, rec)
	return nil
}

func decodeRecord(msg json.RawMessage) (animapRecord, error) {
	if len(msg) > maxRecordBytes {
		return animapRecord{}, fmt.Errorf("record exceeds %d bytes", maxRecordBytes)
	}
	var r animapRecord
	if err := json.Unmarshal(msg, &r); err != nil {
		return animapRecord{}, err
	}
	if len(r.IMDbIDs) > maxRecordIdentifiers || len(r.TMDBMovieIDs) > maxRecordIdentifiers {
		return animapRecord{}, fmt.Errorf("record identifier list exceeds cap %d", maxRecordIdentifiers)
	}
	return r, nil
}
