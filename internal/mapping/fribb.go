package mapping

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/cplieger/jsonx"
	"github.com/cplieger/jsonx/bounded"
)

// Fribb type strings. MOVIE routes to Radarr (TMDB movie / IMDb); every other
// type routes to Sonarr (TVDB).
const typeMovie = "MOVIE"

// NormalizeType canonicalizes a raw Fribb/AniList type/format string to the
// upper-cased, trimmed form Record.Type invariants (IsMovie/IsSpecial) rely on.
func NormalizeType(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// nullLiteral is the JSON null token, checked before decoding tolerant fields.
const nullLiteral = "null"

// isNullOrEmpty reports whether b (already trimmed) is empty or the JSON null token.
func isNullOrEmpty(b []byte) bool {
	return len(b) == 0 || string(b) == nullLiteral
}

// fribbRecord mirrors one element of the Fribb anime-list-mini.json array.
// Every field whose upstream shape varies (an id that may be a number or a
// string, an imdb id that may be a scalar or an array, a themoviedb id that
// may be a {tv}/{movie[]} object, a season object or type string of an odd
// shape) uses a tolerant decoder so one odd field zeroes that field rather
// than failing the record - and one odd record cannot break the whole map.
type fribbRecord struct {
	Type      flexString `json:"type"`
	IMDbID    stringList `json:"imdb_id"`
	TmdbID    tmdbID     `json:"themoviedb_id"`
	Season    offsetPair `json:"season"`
	AniListID flexInt    `json:"anilist_id"`
	TvdbID    flexInt    `json:"tvdb_id"`
}

// toRecord converts a decoded Fribb record into a public Record, normalizing
// the type to upper case and consuming a bare-number themoviedb_id as the
// movie TMDB id when the record's own type is MOVIE (see tmdbID.movieIDs). It
// returns ok=false when the record has no AniList ID (nothing to key the
// SeaDex lookup on).
func (r *fribbRecord) toRecord() (Record, bool) {
	if r.AniListID == 0 {
		return Record{}, false
	}
	typ := NormalizeType(string(r.Type))
	return Record{
		IMDbIDs:    r.IMDbID,
		TmdbMovies: r.TmdbID.movieIDs(typ == typeMovie),
		Type:       typ,
		AniListID:  int(r.AniListID),
		TvdbID:     int(r.TvdbID),
		SeasonTvdb: r.Season.tvdbOrZero(),
	}, true
}

// --- Streaming parse: caps and the per-record tolerance boundary ---

// maxFribbRecords is a hard acceptance cap on the number of top-level Fribb
// array elements, not merely a preallocation hint. The 16MB body limit still
// admits ~1M tiny valid records, so without this guard an upstream-controlled
// body could amplify into a much larger in-memory record set. Real Fribb has
// ~40k records, leaving ample headroom below ~65k.
const maxFribbRecords = 1 << 16

// errRecordCapExceeded rejects a Fribb list exceeding maxFribbRecords. It is
// a sentinel (errors.Is-matched in acceptRefresh) because a permanently
// over-cap upstream list is a persistent guard refusal, not a transient parse
// failure: it re-downloads the multi-MB body and rejects it every cycle,
// never self-healing, so acceptRefresh routes it through rejectRefresh — the
// consecutive-rejection streak advances and the scout escalates at
// degradation.EscalationThreshold instead of degrading at WARN forever.
var errRecordCapExceeded = fmt.Errorf("mapping: Fribb list exceeds cap %d records", maxFribbRecords)

// maxFribbRecordBytes bounds one encoded Fribb record before its tolerant
// decode. The document-level maxMapBytes cap plus maxFribbRecords still admit
// a single record whose nested identifier arrays decode into a working set far
// larger than their wire size; a real record is well under 1 KiB, so 64 KiB
// leaves ample headroom while keeping the per-record decode allocation bounded.
// An oversized record is skipped as malformed, like any other bad element.
const maxFribbRecordBytes = 64 << 10

// maxFribbIdentifiers caps the nested identifier lists retained per record
// (imdb_id entries, themoviedb_id.movie entries). Real records carry a
// handful at most; a list above the cap rejects its record so a hostile body
// cannot amplify compact wire-size arrays into a large retained working set.
const maxFribbIdentifiers = 32

// fribbParseResult is parseFribbForRefresh's counted decode result: the
// surviving AniList-keyed records plus the number of top-level array elements
// they were distilled from (survivors + skipped-malformed + dropped-keyless).
// acceptRefresh validates identifier coverage against elements rather than
// len(records), so destructive filtering and deduplication cannot shrink the
// denominator along with the numerator — a first-boot body of 200 keyless
// rows must not be reinterpreted as a "healthy" 1/1 map.
type fribbParseResult struct {
	records  []Record
	elements int
}

// parseFribb decodes the Fribb list resiliently, returning only the surviving
// records. It is the stable surface the parser tests and fuzz targets
// exercise; the refresh acceptance path uses parseFribbForRefresh to also
// observe the top-level element count.
func parseFribb(data []byte, log *slog.Logger) ([]Record, error) {
	parsed, err := parseFribbForRefresh(data, log)
	return parsed.records, err
}

// parseFribbForRefresh decodes the Fribb list resiliently: it streams the
// top-level array element by element (never materializing all raw messages at
// once, so a bounded body of tiny elements cannot amplify into a huge
// transient allocation), decoding each element on its own so a single
// malformed record is skipped (counted) rather than failing the whole map. A
// list that exceeds maxFribbRecords is rejected outright with the
// errRecordCapExceeded sentinel — before the excess elements are ever decoded
// — so the caller keeps the stale cache rather than admitting an amplified
// record set (and can tell the cap breach apart from a transient parse
// failure). Trailing data after the closing bracket is rejected, matching the
// strictness of a whole-document json.Unmarshal. Alongside the surviving
// records it reports the top-level element count (see fribbParseResult), the
// denominator the refresh acceptance floors validate coverage against.
func parseFribbForRefresh(data []byte, log *slog.Logger) (fribbParseResult, error) {
	dec := bounded.NewDecoder(bytes.NewReader(data), 0)
	ok, err := dec.Open('[')
	if err != nil {
		return fribbParseResult{}, fmt.Errorf("mapping: Fribb list is not a JSON array: %w", err)
	}
	if !ok {
		// bounded.Open reports a JSON null as ok=false without error (the
		// Unmarshal null-into-slice no-op); for the Fribb map an absent list
		// is as unusable as a non-array.
		return fribbParseResult{}, errors.New("mapping: Fribb list is not a JSON array (got null)")
	}
	records, skipped, dropped, firstErr, err := decodeFribbRecords(dec)
	if err != nil {
		return fribbParseResult{}, err
	}
	if err := dec.Close(); err != nil { // consume the closing ']'
		return fribbParseResult{}, fmt.Errorf("mapping: Fribb list truncated or malformed at close: %w", err)
	}
	if err := dec.End(); err != nil {
		return fribbParseResult{}, fmt.Errorf("mapping: trailing data after Fribb list: %w", err)
	}
	if skipped > 0 {
		attrs := []any{"skipped", skipped, "parsed", len(records)}
		if firstErr != nil {
			attrs = append(attrs, "error", firstErr)
		}
		log.Warn("mapping: skipped malformed records", attrs...)
	}
	if dropped > 0 {
		log.Debug("mapping: dropped records without anilist_id", "dropped", dropped, "parsed", len(records))
	}
	return fribbParseResult{records: records, elements: len(records) + skipped + dropped}, nil
}

// decodeFribbRecords streams the array body element-by-element, decoding each
// on its own so one malformed record is skipped (counted) rather than failing
// the whole map, and rejecting a list that exceeds maxFribbRecords (the
// errRecordCapExceeded sentinel) before the excess elements are decoded. It
// leaves the decoder positioned on the array's closing token.
func decodeFribbRecords(dec *bounded.Decoder) (records []Record, skipped, dropped int, firstErr, err error) {
	seen := 0
	for dec.More() {
		if seen == maxFribbRecords {
			return nil, 0, 0, nil, errRecordCapExceeded
		}
		seen++
		rec, ok, decodeErr, streamErr := decodeNextFribbRecord(dec)
		if streamErr != nil {
			return nil, 0, 0, nil, streamErr
		}
		if decodeErr != nil {
			skipped++
			if firstErr == nil {
				firstErr = decodeErr
			}
			continue
		}
		if ok {
			records = append(records, rec)
		} else {
			dropped++
		}
	}
	return records, skipped, dropped, firstErr, nil
}

// decodeNextFribbRecord reads the next array element off the stream and
// decodes it. The two error results separate the tolerance boundary: the
// first (decodeErr) is a tolerated per-record decode failure the caller skips
// and counts; the second (streamErr) is a fatal RawMessage stream-decode
// failure that rejects the whole document.
func decodeNextFribbRecord(dec *bounded.Decoder) (rec Record, ok bool, decodeErr, streamErr error) {
	var msg json.RawMessage
	if err := dec.Decode(&msg); err != nil {
		return Record{}, false, nil, fmt.Errorf("mapping: Fribb stream decode: %w", err)
	}
	rec, ok, decodeErr = decodeFribbRecord(msg)
	return rec, ok, decodeErr, nil
}

// decodeFribbRecord validates and decodes one raw Fribb array element. An
// oversized record is a decoded-size amplification risk (millions of tiny
// nested identifiers fit under maxMapBytes), so it is rejected as malformed
// before the tolerant per-record decode ever allocates for it. ok=false with a
// nil error means the record decoded but carries no AniList ID.
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

// --- Tolerant field decoders (shape-variant upstream fields) ---

// offsetPair decodes the tvdb member of the season object; encoding/json
// intentionally ignores the unused tmdb member (the upstream episode_offset
// field shares the shape but is likewise not decoded - no consumer reads it).
// It sits inside the record's tolerance boundary: the object itself decodes
// tolerantly and the interior id reuses flexInt, so an odd upstream season
// shape (a bare number, a quoted interior value, a float) zeroes the field -
// SeasonTvdb 0 falls back to whole-series/season-0 scoping - while the record
// survives.
type offsetPair struct {
	Tvdb flexInt `json:"tvdb"`
}

// UnmarshalJSON decodes the object form and tolerates any other shape as
// absent (the interior flexInt fields already tolerate odd id shapes). The
// receiver is reset first: encoding/json reuses the same field receiver for
// duplicate object keys, so a later tolerated-odd value must clear an earlier
// decode rather than silently retain it.
func (o *offsetPair) UnmarshalJSON(b []byte) error {
	*o = offsetPair{}
	b = bytes.TrimSpace(b)
	if isNullOrEmpty(b) || b[0] != '{' {
		return nil
	}
	type alias offsetPair
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return nil //nolint:nilerr // tolerate an odd season shape rather than fail the record
	}
	*o = offsetPair(a)
	return nil
}

// tvdbOrZero returns the tvdb season or 0 when absent or odd-shaped.
func (o offsetPair) tvdbOrZero() int { return int(o.Tvdb) }

// flexString decodes a JSON string; any other shape (a bare number, a float,
// an object) is tolerated as empty rather than failing the record. An empty
// Fribb type routes the record as a non-movie series, the safe default.
type flexString string

// UnmarshalJSON implements the tolerant string decode. The receiver is reset
// first so a duplicate key's later odd value clears an earlier decode (see
// offsetPair.UnmarshalJSON).
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

// tmdbID decodes the themoviedb_id field, which is a {"tv":int} or
// {"movie":[int]} object in the merged list; only the movie half feeds a
// lookup path (the unknown "tv" key is ignored on decode). A bare-number (or
// quoted-numeric) scalar, which some upstream rows carry instead of the
// object, is retained as the untyped Scalar: the number alone cannot be
// disambiguated into a tv-vs-movie id, but a MOVIE-typed record's own type
// does disambiguate it (a movie's tmdb id is necessarily a movie id), so
// movieIDs consumes it for MOVIE records — otherwise it is discarded (a
// series' scalar would be a TMDB tv id no lookup consumes; such an entry
// still matches via tvdb_id). Any other shape (the "unknown" string that
// appears in some upstream rows) is tolerated and left empty.
type tmdbID struct {
	Movie []flexInt `json:"movie"`
	// Scalar is the retained bare-number form; consumed only via movieIDs.
	Scalar flexInt `json:"-"`
}

// UnmarshalJSON decodes the object form, retains a numeric scalar as Scalar
// (see the type comment), and tolerates any other shape as empty. The
// receiver is reset first so a duplicate key's later odd value clears an
// earlier decode (see offsetPair.UnmarshalJSON).
func (t *tmdbID) UnmarshalJSON(b []byte) error {
	*t = tmdbID{}
	b = bytes.TrimSpace(b)
	if isNullOrEmpty(b) {
		return nil
	}
	if b[0] != '{' {
		// The tolerant flexInt decode keeps a bare or quoted number and
		// zeroes every other scalar shape, so an "unknown" placeholder
		// stays empty.
		return t.Scalar.UnmarshalJSON(b)
	}
	type alias tmdbID
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return nil //nolint:nilerr // tolerate an odd themoviedb_id shape rather than fail the record
	}
	// The transient decode above is bounded by maxFribbRecordBytes; the cap
	// here bounds what is RETAINED, rejecting the record so a hostile body
	// cannot accumulate huge per-record identifier sets.
	if len(a.Movie) > maxFribbIdentifiers {
		return fmt.Errorf("themoviedb_id.movie list exceeds cap %d", maxFribbIdentifiers)
	}
	*t = tmdbID(a)
	return nil
}

// movieIDs returns the movie TMDB ids a record contributes: the object form's
// movie list, or — only when the record's own type is MOVIE (isMovie) — the
// retained untyped Scalar, which the type disambiguates into a movie id. A
// non-movie record's scalar stays discarded. The two forms are mutually
// exclusive (UnmarshalJSON resets the receiver per decode), so the list wins
// merely by being checked first.
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
// "unknown", non-numeric, fractional, negative, or out-of-range value decodes
// to 0 rather than erroring or truncating, so an upstream placeholder or odd
// value does not break the record or masquerade as a valid id. The decode is
// a thin shim over jsonx under the TolerantZero policy (which this decoder
// originated): both wire forms parse identically ("9.0" → 9, "1e3" → 1000,
// "1.5" → 0) — the number/string equivalence the Fribb id fields rely on —
// real ids are bounded to [0, MaxInt32], fractional values zero rather than
// truncate (9.9 truncated to 9 would silently point at a different anime),
// and only a malformed JSON string propagates an error.
type flexInt int

// UnmarshalJSON implements the tolerant number-or-string decode via
// jsonx.ParseInt64 with jsonx.TolerantZero. The receiver is reset first so a
// duplicate key's later odd value clears an earlier decode (see
// offsetPair.UnmarshalJSON), and it stays 0 on error (no partial value) —
// both invariants are also pinned inside the library (jsonx.TolerantInt).
func (f *flexInt) UnmarshalJSON(b []byte) error {
	*f = 0
	n, err := jsonx.ParseInt64(b, jsonx.TolerantZero())
	if err != nil {
		return err
	}
	*f = flexInt(n)
	return nil
}

// stringList decodes a JSON array of strings, a single string, or null into a
// []string, trimming blanks. The imdb_id field is an array in the merged list
// but a scalar in some upstream rows. Both branches are tolerant (matching the
// sibling flexInt/tmdbID decoders): a mixed-type array keeps its valid string
// entries and drops the rest, so an odd entry never fails the whole record.
type stringList []string

// UnmarshalJSON implements the array-or-scalar decode. The receiver is reset
// first so a duplicate key's later odd value clears an earlier decode (see
// offsetPair.UnmarshalJSON).
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

// decodeStringArray decodes the array form tolerantly: a malformed array
// yields nil (never an error), a non-string entry is dropped while its valid
// siblings survive, and a list over maxFribbIdentifiers errors so the record
// is rejected.
func decodeStringArray(b []byte) ([]string, error) {
	var arr []json.RawMessage
	if err := json.Unmarshal(b, &arr); err != nil {
		return nil, nil //nolint:nilerr // tolerate an odd imdb_id array rather than fail the record
	}
	// The transient decode above is bounded by maxFribbRecordBytes; the cap
	// here bounds what is RETAINED, rejecting the record so a hostile body
	// cannot accumulate huge per-record identifier sets.
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

// --- Small conversion helpers ---

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
