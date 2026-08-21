package mapping

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/cplieger/jsoncap/v2"
	"github.com/cplieger/jsonx/v2"
	"github.com/cplieger/runesafe/v2"
	"github.com/cplieger/seadex-scout/internal/mediatype"
)

// Fribb type strings route the arr: MOVIE goes to Radarr (TMDB movie / IMDb);
// every other type goes to Sonarr (TVDB). The token vocabulary and its
// canonicalization live in the dependency-free internal/mediatype leaf.

// RecordFromFormat builds the type-only Record a consumer uses to reuse the
// arr/season routing decisions for an AniList format that has no Fribb record.
func RecordFromFormat(format string) Record { return Record{Type: mediatype.Normalize(format)} }

// nullLiteral is the JSON null token, checked before decoding tolerant fields.
const nullLiteral = "null"

// isNullOrEmpty reports whether b (already trimmed) is empty or the JSON null token.
func isNullOrEmpty(b []byte) bool {
	return len(b) == 0 || string(b) == nullLiteral
}

// fribbRecord mirrors one element of the Fribb anime-list-mini.json array.
// Every field whose upstream shape varies uses a tolerant decoder, so one odd
// field zeroes that field rather than failing the record.
type fribbRecord struct {
	Type      flexString   `json:"type"`
	IMDbID    stringList   `json:"imdb_id"`
	TmdbID    tmdbID       `json:"themoviedb_id"`
	Season    seasonObject `json:"season"`
	AniListID flexInt      `json:"anilist_id"`
	TvdbID    flexInt      `json:"tvdb_id"`
}

// toRecord converts a decoded Fribb record into a public Record, normalizing
// the type to upper case and consuming a bare-number themoviedb_id as the movie
// TMDB id when the record's own type is MOVIE (see tmdbID.movieIDs). It returns
// ok=false when the record has no positive AniList ID.
func (r *fribbRecord) toRecord() (Record, bool) {
	if r.AniListID <= 0 {
		return Record{}, false
	}
	typ := mediatype.Normalize(string(r.Type))
	rec := Record{
		IMDbIDs:    r.IMDbID,
		TmdbMovies: r.TmdbID.movieIDs(typ == mediatype.Movie),
		Type:       typ,
		AniListID:  int(r.AniListID),
		TvdbID:     int(r.TvdbID),
		SeasonTvdb: r.Season.tvdbOrZero(),
	}
	rec.canonicalize() // idempotent here; pins both producers to one rule
	return rec, true
}

// maxFribbRecords is a hard acceptance cap on the number of top-level Fribb
// array elements, not merely a preallocation hint: the 16MB body limit still
// admits ~1M tiny valid records. Real Fribb has ~40k, leaving headroom.
const maxFribbRecords = 1 << 16

// errRecordCapExceeded rejects a Fribb list exceeding maxFribbRecords. It is a
// sentinel (errors.Is-matched in acceptRefresh) because a permanently over-cap
// list never self-heals: it must advance the consecutive-rejection streak
// instead of degrading at WARN forever.
var errRecordCapExceeded = fmt.Errorf("mapping: Fribb list exceeds cap %d records", maxFribbRecords)

// errNotJSONArray rejects a Fribb body whose top-level value is not a JSON
// array. It is a sentinel because the class is CONTENT-SHAPE evidence, not
// transport damage: truncating a valid array body in flight cannot change its
// FIRST token, so a non-'[' document never self-heals. Mid-stream truncation
// stays transient - a partial download of an array-shaped body can succeed.
var errNotJSONArray = errors.New("mapping: Fribb list is not a JSON array")

// maxFribbRecordBytes bounds one encoded Fribb record before its tolerant
// decode: the document-level caps still admit a single record whose nested
// identifier arrays decode into a working set far larger than their wire size.
// A real record is well under 1 KiB; an oversized one is skipped as malformed.
const maxFribbRecordBytes = 64 << 10

// maxFribbIdentifiers caps the nested identifier lists retained per record
// (imdb_id entries, themoviedb_id.movie entries). A list above the cap rejects
// its record, so a compact wire-size array cannot amplify what is retained.
const maxFribbIdentifiers = 32

// maxFribbIdentifiersTotal bounds the identifiers retained across the WHOLE
// list. The per-record caps alone still admit ~4.2M retained ids from a body
// under maxMapBytes; this aggregate budget bounds that product.
const maxFribbIdentifiersTotal = 1 << 20

// errIdentifierBudgetExceeded rejects a Fribb list whose retained identifiers
// exceed maxFribbIdentifiersTotal. A sentinel, because this ceiling truncates
// the TAIL of the list: retaining the prefix publishes a knowably incomplete map
// that every count floor still passes, and it never self-heals.
var errIdentifierBudgetExceeded = fmt.Errorf("mapping: Fribb identifiers exceed cap %d", maxFribbIdentifiersTotal)

// fribbParseResult is parseFribbForRefresh's counted decode result: the
// surviving AniList-keyed records plus the number of top-level array elements
// they were distilled from, whatever each element's outcome. acceptRefresh
// validates identifier coverage against elements rather than len(records), so
// filtering and deduplication cannot shrink the denominator with the numerator.
type fribbParseResult struct {
	records  []Record
	elements int
}

// parseFribbForRefresh decodes the Fribb list resiliently: it streams the
// top-level array element by element, decoding each on its own so a single
// malformed record is skipped (counted) rather than failing the whole map. A
// list exceeding maxFribbRecords is rejected with the errRecordCapExceeded
// sentinel before the excess elements are decoded, and trailing data after the
// closing bracket is rejected. It also reports the top-level element count.
func parseFribbForRefresh(data []byte, log *slog.Logger) (fribbParseResult, error) {
	// Element budget 0 disables bounded's own aggregate cap deliberately:
	// maxFribbRecords below is the app-level ceiling, and it must reject with the
	// errRecordCapExceeded sentinel acceptRefresh matches on.
	dec := jsoncap.NewDecoder(bytes.NewReader(data), 0)
	ok, err := dec.Open('[')
	if err != nil {
		if errors.Is(err, io.EOF) {
			// An empty body has NO first token, so it is not the content-shape
			// evidence errNotJSONArray stands for: a zero-length 200 CAN succeed on
			// the next attempt, so it stays a transient parse failure.
			return fribbParseResult{}, fmt.Errorf("mapping: Fribb list body is empty: %w", err)
		}
		return fribbParseResult{}, fmt.Errorf("%w: %w", errNotJSONArray, err)
	}
	if !ok {
		// jsoncap.Open reports a JSON null as ok=false without error; for the Fribb
		// map an absent list is as unusable as a non-array.
		return fribbParseResult{}, fmt.Errorf("%w (got null)", errNotJSONArray)
	}
	counts, err := decodeFribbRecords(dec)
	if err != nil {
		return fribbParseResult{}, err
	}
	if err := dec.Close(); err != nil { // consume the closing ']'
		return fribbParseResult{}, fmt.Errorf("mapping: Fribb list truncated or malformed at close: %w", err)
	}
	if err := dec.End(); err != nil {
		return fribbParseResult{}, fmt.Errorf("mapping: trailing data after Fribb list: %w", err)
	}
	logFribbParseDiagnostics(log, &counts)
	return fribbParseResult{records: counts.records, elements: counts.elements}, nil
}

// logFribbParseDiagnostics emits the decode's tolerated-outcome diagnostics: the
// skipped-malformed WARN, the keyless-record Debug line, and the two advance
// warnings for the approaching record cap and identifier budget.
func logFribbParseDiagnostics(log *slog.Logger, counts *fribbDecodeCounts) {
	if counts.skipped > 0 {
		attrs := []any{"skipped", counts.skipped, "parsed", len(counts.records)}
		if counts.firstErr != nil {
			// The first skipped record's error is untrusted-input-derived, so it
			// passes the package's log-boundary policy (see maxLoggedErrorBytes).
			attrs = append(attrs, "error",
				errors.New(runesafe.SanitizeSingleLineBounded(counts.firstErr.Error(), maxLoggedErrorBytes)))
		}
		log.Warn("mapping: skipped malformed records", attrs...)
	}
	if counts.dropped > 0 {
		log.Debug("mapping: dropped records without anilist_id", "dropped", counts.dropped, "parsed", len(counts.records))
	}
	// Advance warning before the cap becomes a hard refusal: real Fribb is already
	// ~43k of the 65,536-element cap, and a breach is NOT self-healing - the map
	// stays frozen stale until the cap is raised.
	if counts.elements >= maxFribbRecords/4*3 {
		log.Warn("mapping: Fribb list approaching record cap", "elements", counts.elements, "cap", maxFribbRecords)
	}
	// Same advance-notice contract as the record cap above: an identifier-budget
	// breach never self-heals either.
	if counts.identifiers >= maxFribbIdentifiersTotal/4*3 {
		log.Warn("mapping: Fribb identifiers approaching budget", "identifiers", counts.identifiers, "cap", maxFribbIdentifiersTotal)
	}
}

// decodeFribbRecords streams the array body element-by-element, decoding each on
// its own so one malformed record is skipped (counted) rather than failing the
// whole map, and rejecting a list over maxFribbRecords or over
// maxFribbIdentifiersTotal. It leaves the decoder on the array's closing token.
func decodeFribbRecords(dec *jsoncap.Decoder) (fribbDecodeCounts, error) {
	var counts fribbDecodeCounts
	// The ordering is load-bearing: dec.More observes another element, the cap
	// guard fires on it, and only then is that element read and decoded.
	for seen := 0; dec.More(); seen++ {
		if seen == maxFribbRecords {
			return fribbDecodeCounts{}, errRecordCapExceeded
		}
		counts.elements++
		rec, ok, decodeErr, streamErr := decodeNextFribbRecord(dec)
		if streamErr != nil {
			return fribbDecodeCounts{}, streamErr
		}
		if err := counts.add(&rec, ok, decodeErr); err != nil {
			// Fail closed at the point the budget trips and let acceptRefresh
			// keep the stale cache.
			return fribbDecodeCounts{}, err
		}
	}
	return counts, nil
}

// fribbDecodeCounts accumulates decodeFribbRecords' TOLERATED per-record
// outcomes: the accepted records, the first skipped record's decode error, and
// the skipped/dropped counts the caller logs. Fatal outcomes are never counted.
type fribbDecodeCounts struct {
	firstErr error
	records  []Record
	// elements counts every top-level array element the loop OBSERVED - the
	// acceptance denominator (see fribbParseResult). The loop counts it rather
	// than the caller re-deriving it, so a new outcome class cannot shrink it.
	elements    int
	skipped     int
	dropped     int
	identifiers int
}

// add folds one record's decode outcome in: a tolerated decode failure counts as
// skipped (keeping the first error), a record without an AniList ID counts as
// dropped, a record breaching the aggregate identifier budget returns
// errIdentifierBudgetExceeded (fatal to the whole document), and anything else
// is accepted. rec is by pointer only because Record is a heavy value.
func (c *fribbDecodeCounts) add(rec *Record, ok bool, decodeErr error) error {
	if decodeErr != nil {
		c.skipped++
		if c.firstErr == nil {
			c.firstErr = decodeErr
		}
		return nil
	}
	if !ok {
		c.dropped++
		return nil
	}
	n := len(rec.IMDbIDs) + len(rec.TmdbMovies)
	if c.identifiers+n > maxFribbIdentifiersTotal {
		// The aggregate identifier budget is a whole-document guarantee, not a
		// per-record tolerance: retaining the prefix would publish a knowably
		// truncated map. Returned rather than counted, so it cannot be forgotten.
		return errIdentifierBudgetExceeded
	}
	c.identifiers += n
	c.records = append(c.records, *rec)
	return nil
}

// decodeNextFribbRecord reads the next array element off the stream and decodes
// it. The two error results separate the tolerance boundary: decodeErr is a
// tolerated per-record failure the caller skips and counts; streamErr is fatal.
func decodeNextFribbRecord(dec *jsoncap.Decoder) (rec Record, ok bool, decodeErr, streamErr error) {
	var msg json.RawMessage
	if err := dec.Decode(&msg); err != nil {
		return Record{}, false, nil, fmt.Errorf("mapping: Fribb stream decode: %w", err)
	}
	rec, ok, decodeErr = decodeFribbRecord(msg)
	return rec, ok, decodeErr, nil
}

// decodeFribbRecord validates and decodes one raw Fribb array element. An
// oversized record is rejected as malformed before the tolerant decode ever
// allocates for it. ok=false with a nil error means it carries no AniList ID.
func decodeFribbRecord(msg json.RawMessage) (Record, bool, error) {
	if len(msg) > maxFribbRecordBytes {
		return Record{}, false, fmt.Errorf("record exceeds %d bytes", maxFribbRecordBytes)
	}
	var fr fribbRecord
	if err := json.Unmarshal(msg, &fr); err != nil {
		return Record{}, false, err
	}
	rec, ok := fr.toRecord()
	return rec, ok, nil
}

// seasonObject decodes the tvdb member of the season object; the unused tmdb
// member and the upstream episode_offset are deliberately not decoded. An odd
// season SHAPE zeroes the field - SeasonTvdb 0 falls back to whole-series or
// season-0 scoping - while the record survives. The interior flexInt is
// narrower: "2", " 2 " and 2.0 still decode, only 1.5 or negative zeroes.
type seasonObject struct {
	Tvdb flexInt `json:"tvdb"`
}

// UnmarshalJSON decodes the object form and tolerates any other shape as absent.
// The receiver is reset first: encoding/json reuses the same field receiver for
// duplicate object keys, so a later odd value must clear an earlier decode.
func (o *seasonObject) UnmarshalJSON(b []byte) error {
	*o = seasonObject{}
	b = bytes.TrimSpace(b)
	if isNullOrEmpty(b) || b[0] != '{' {
		return nil
	}
	type alias seasonObject
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return nil //nolint:nilerr // tolerate an odd season shape rather than fail the record
	}
	*o = seasonObject(a)
	return nil
}

// tvdbOrZero returns the tvdb season or 0 when absent or odd-shaped.
func (o seasonObject) tvdbOrZero() int { return int(o.Tvdb) }

// flexString decodes a JSON string; any other shape is tolerated as empty rather
// than failing the record. An empty Fribb type routes as a non-movie series.
type flexString string

// UnmarshalJSON implements the tolerant string decode. The receiver is reset
// first so a duplicate key's later odd value clears an earlier decode.
func (s *flexString) UnmarshalJSON(b []byte) error {
	*s = ""
	b = bytes.TrimSpace(b)
	if isNullOrEmpty(b) || b[0] != '"' {
		return nil
	}
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*s = flexString(v)
	return nil
}

// tmdbID decodes the themoviedb_id field, a {"tv":int} or {"movie":[int]}
// object in the merged list; only the movie half feeds a lookup path. A
// bare-number scalar is retained as the untyped Scalar and consumed only for a
// MOVIE-typed record, whose own type disambiguates it into a movie id. Any other
// shape (the "unknown" string some rows carry) is tolerated and left empty.
type tmdbID struct {
	// Movie holds the object form's movie ids. Neither field carries a json tag:
	// UnmarshalJSON below owns the whole decode, so a tag here would be inert.
	Movie []flexInt
	// Scalar is the retained bare-number form; consumed only via movieIDs.
	Scalar flexInt
}

// UnmarshalJSON decodes the object form, retains a numeric scalar as Scalar (see
// the type comment), and tolerates any other shape as empty. Receiver reset.
func (t *tmdbID) UnmarshalJSON(b []byte) error {
	*t = tmdbID{}
	b = bytes.TrimSpace(b)
	if isNullOrEmpty(b) {
		return nil
	}
	if b[0] != '{' {
		// The tolerant flexInt decode zeroes every non-numeric scalar shape, so an
		// "unknown" placeholder stays empty.
		return t.Scalar.UnmarshalJSON(b)
	}
	// Capture the movie member as RAW bytes first. encoding/json continues past a
	// type mismatch, so decoding straight into []flexInt let a duplicate movie key
	// whose EARLIER value has the wrong shape return a type error ALONGSIDE the
	// valid later value, which the tolerant arm then threw away. A json.RawMessage
	// member has no shape to mismatch, so the last value always wins.
	var wire struct {
		Movie json.RawMessage `json:"movie"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		return nil //nolint:nilerr // tolerate an odd themoviedb_id shape rather than fail the record
	}
	movie := bytes.TrimSpace(wire.Movie)
	if isNullOrEmpty(movie) || movie[0] != '[' {
		// Absent, null, or a non-array last value: tolerated as empty.
		return nil
	}
	var movies []flexInt
	if err := json.Unmarshal(movie, &movies); err != nil {
		return nil //nolint:nilerr // tolerate an odd themoviedb_id.movie shape rather than fail the record
	}
	// The transient decode above is bounded by maxFribbRecordBytes; the cap here
	// bounds what is RETAINED, rejecting the record instead.
	if len(movies) > maxFribbIdentifiers {
		return fmt.Errorf("themoviedb_id.movie list exceeds cap %d", maxFribbIdentifiers)
	}
	t.Movie = movies
	return nil
}

// movieIDs returns the movie TMDB ids a record contributes: the object form's
// movie list, or - only when the record's own type is MOVIE - the retained
// untyped Scalar. The two forms are mutually exclusive per decode.
func (t tmdbID) movieIDs(isMovie bool) []int {
	if ids := intSlice(t.Movie); len(ids) > 0 {
		return ids
	}
	if isMovie && t.Scalar != 0 {
		return []int{int(t.Scalar)}
	}
	return nil
}

// flexInt decodes a JSON number or numeric string into an int. A null, empty,
// non-numeric, fractional, negative or out-of-range value decodes to 0 rather
// than erroring or truncating (9.9 truncated to 9 would silently point at a
// different anime). An alias of jsonx.TolerantInt, the policy this originated.
type flexInt = jsonx.TolerantInt

// stringList decodes a JSON array of strings, a single string, or null into a
// []string, trimming blanks: imdb_id is an array in the merged list but a scalar
// in some upstream rows. A mixed-type array keeps its valid string entries.
type stringList []string

// UnmarshalJSON implements the array-or-scalar decode. The receiver is reset
// first so a duplicate key's later odd value clears an earlier decode.
func (s *stringList) UnmarshalJSON(b []byte) error {
	*s = nil
	b = bytes.TrimSpace(b)
	if isNullOrEmpty(b) {
		return nil
	}
	if b[0] == '[' {
		out, err := decodeStringArray(b)
		if err != nil {
			return err
		}
		*s = out
		return nil
	}
	*s = decodeStringScalar(b)
	return nil
}

// decodeStringArray decodes the array form tolerantly: a malformed array yields
// nil, a non-string entry is dropped while its valid siblings survive, and a
// list over maxFribbIdentifiers errors so the record is rejected.
func decodeStringArray(b []byte) ([]string, error) {
	var arr []json.RawMessage
	if err := json.Unmarshal(b, &arr); err != nil {
		return nil, nil //nolint:nilerr // tolerate an odd imdb_id array rather than fail the record
	}
	// The transient decode above is bounded by maxFribbRecordBytes; the cap here
	// bounds what is RETAINED, rejecting the record instead.
	if len(arr) > maxFribbIdentifiers {
		return nil, fmt.Errorf("imdb_id list exceeds cap %d", maxFribbIdentifiers)
	}
	out := make([]string, 0, len(arr))
	for _, el := range arr {
		var v string
		if err := json.Unmarshal(el, &v); err != nil {
			continue // drop a non-string entry, keep the valid siblings
		}
		out = append(out, v)
	}
	return trimmed(out), nil
}

// decodeStringScalar decodes the tolerant single-string form; a malformed
// scalar yields nil rather than failing the record.
func decodeStringScalar(b []byte) []string {
	var one string
	if err := json.Unmarshal(b, &one); err != nil {
		return nil //nolint:nilerr // tolerate an odd imdb_id shape rather than fail the record
	}
	return trimmed([]string{one})
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

// intSlice converts a []flexInt to a []int, dropping non-positive entries - the
// same canonical positive-ids form the overrides path enforces.
func intSlice(in []flexInt) []int {
	var out []int
	for _, v := range in {
		if v > 0 {
			out = append(out, int(v))
		}
	}
	return out
}
