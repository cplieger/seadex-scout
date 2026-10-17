// Package mapping bridges AniList IDs (what SeaDex keys on) to the arr IDs
// Sonarr and Radarr key on (TVDB, TMDB, IMDb), using the animap dataset plus a
// local overrides file the operator can pin misses in.
//
// The latest animap release's animap.json is downloaded when its tag changes
// and cached; overrides are re-read every load and overlaid on top, so an
// operator entry always wins.
package mapping

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cplieger/animap/mapread"
	"github.com/cplieger/atomicfile/v4"
	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/jsoncap/v2"
	"github.com/cplieger/runesafe/v3"
	"github.com/cplieger/seadex-scout/internal/appinfo"
	"github.com/cplieger/seadex-scout/internal/degradation"
	"github.com/cplieger/seadex-scout/internal/mediatype"
)

const (
	// DefaultRefresh is the reuse-if-fresh window for the map. 0 asks for the
	// latest release's tag on every Loader.Load, one bodiless request, and
	// downloads only a new tag. A failed check is harmless - the persisted
	// cache is reused stale-on-error and the next load retries.
	DefaultRefresh = 0

	// maxMapBytes bounds the release asset before decode: animap's own encode
	// bound, so no document animap can publish is refused, and 2.5 times the
	// 13.4 MB version 2 document of 2026-10. Fetch holds the body while its
	// decode takes about 2.2 bytes of heap per byte, so a refresh at the cap
	// peaks near 110 MB.
	maxMapBytes = 32 << 20
	// maxOverrideBytes bounds the local overrides file.
	maxOverrideBytes = 4 << 20
	maxAttempts      = 3
	baseDelay        = time.Second
)

// Loader fetches and caches animap.json and overlays the overrides file.
type Loader struct {
	http          *http.Client
	log           *slog.Logger
	source        mapread.Source
	overridesPath string
	refresh       time.Duration
}

// SeasonKind reports whether the entry's TVDB placement names a season at
// all, which is a different question from which season it names: an absent
// season and a mapped zero mean opposite things and both leave SeasonTvdb 0.
//
// Three constants, so the pair (mapped-positive, SeasonTvdb 0) is unrepresentable
// rather than merely forbidden: WHICH season stays SeasonTvdb.
type SeasonKind string

const (
	// seasonUnknown is the zero value: a Record no upstream record produced (an
	// override naming no season_kind, a format-only Record). The animap decoder
	// sets the kind explicitly on both arms, so unknown never means "the
	// upstream was odd".
	seasonUnknown SeasonKind = ""
	// SeasonPresent means the entry's TVDB placement names its season, so
	// SeasonTvdb is it - zero for an entry landing only on season 0 or nowhere.
	SeasonPresent SeasonKind = "present"
	// SeasonAbsent means it names none: an entry with no single TVDB series,
	// on two or more positive seasons, with an episode the map states nothing
	// for, or with no episodes.
	SeasonAbsent SeasonKind = "absent"
)

// Record is the resolved mapping for one AniList entry: its media type and the
// arr IDs it corresponds to.
//
// The json tags below are read by TWO decoders: the persisted Cache decodes
// through encoding/json, while overrides.json is walked key-by-key by
// decodeOverrideRecord, whose switch restates these eight names as literals.
// Adding, renaming or removing a field here REQUIRES the matching edit there.
type Record struct {
	Type       string     `json:"type"`
	SeasonKind SeasonKind `json:"season_kind,omitempty"`
	IMDbIDs    []string   `json:"imdb_ids,omitempty"`
	// TmdbMovies is the TMDB movies of the anime the entry is wholly, read
	// for EVERY record type by the ID bridge regardless of the type label: a
	// TMDB movie id is a Radarr id by construction. Deliberately NOT the IMDb
	// list, since TVDB reuses a film's IMDb id on the parent series. Canonical
	// form (positive ids only) is a Record invariant, so readers take the
	// slice as-is.
	TmdbMovies []int `json:"tmdb_movies,omitempty"`
	AniListID  int   `json:"anilist_id"`
	TvdbID     int   `json:"tvdb_id,omitempty"`
	// AniDBID is the anime the entry is all the regular episodes of, the join
	// key into that anime's facts (Cache.Mappings); 0 means absent. It routes
	// nothing: an override naming it points at the anime's facts, it never
	// carries them.
	AniDBID    int `json:"anidb_id,omitempty"`
	SeasonTvdb int `json:"season_tvdb,omitempty"`
}

// IsMovie reports whether the entry maps to a Radarr movie (type MOVIE).
// Every other type maps to a Sonarr series.
func (r *Record) IsMovie() bool { return mediatype.IsMovie(r.Type) }

// RoutedIDs returns the identifiers the record's routed arr consumes, per the
// HasArrIdentifier routing decision: a MOVIE record yields its TMDB-movie and
// IMDb ids (tvdb zero); every other type yields its TVDB id. The TYPE LABEL is
// what routes here - a record's unambiguous movie TMDB ids stay separately
// reachable as Record.TmdbMovies regardless of that label.
func (r *Record) RoutedIDs() (tvdbID int, tmdbMovies []int, imdbIDs []string) {
	if r.IsMovie() {
		return 0, r.TmdbMovies, r.IMDbIDs
	}
	// Zero out a non-usable TVDB id here so the usability rule has ONE home:
	// callers do a presence check, never a policy check.
	return max(r.TvdbID, 0), nil, nil
}

// AllIDs returns every canonical identifier the record carries, with no type gate:
// the TVDB id when positive, the movie TMDB ids, the IMDb ids. TVDB files anime
// films under their parent series, so a MOVIE record's TVDB id is evidence.
//
// The licensed consumers are match.LibIndex's findMovie (FindByID's MOVIE arm),
// otherCopy and filmCopy, which hold both arr indexes so the map miss is still
// the arr gate. RoutedIDs must NOT widen: NewCatalogue has no index gate,
// HasArrIdentifier has five consumers.
func (r *Record) AllIDs() (tvdbID int, tmdbMovies []int, imdbIDs []string) {
	return max(0, r.TvdbID), r.TmdbMovies, r.IMDbIDs
}

// HasArrIdentifier reports whether the record carries a USABLE identifier
// consumed by the arr selected by its type: TMDB-movie/IMDb for movies, TVDB
// for series. The canonical arr-routing predicate shared by the refresh
// acceptance guard, the matcher and the report's reverse catalogue. Usability
// is a Record INVARIANT (canonicalize), so this is a presence check.
func (r *Record) HasArrIdentifier() bool {
	tvdb, tmdbMovies, imdbIDs := r.RoutedIDs()
	return tvdb > 0 || len(tmdbMovies) > 0 || len(imdbIDs) > 0
}

// IsSpecial reports whether the entry is an OVA/ONA/special/music video rather
// than a standard TV season or movie, so it can be excluded when the operator
// turns specials off. A match with no type is treated as non-special.
func (r *Record) IsSpecial() bool { return mediatype.IsSpecial(r.Type) }

// HasMappedSeason reports whether the record carries a positive TVDB
// season - the predicate season-exact comparison and the season floor key on.
func (r *Record) HasMappedSeason() bool { return r.SeasonTvdb > 0 }

// SeasonPresence returns the record's season kind in canonical form: any value
// but the two named spellings reads unknown. NewIndex canonicalizes every
// record it serves, so this differs from the field only for a Record that never
// passed through an Index - which is the path a scope dispatch must still read
// as unknown rather than as a fourth state.
func (r *Record) SeasonPresence() SeasonKind { return canonicalSeasonKind(r.SeasonKind) }

// canonicalSeasonKind is the one home of the normalization both Record
// producers and canonicalize share.
func canonicalSeasonKind(k SeasonKind) SeasonKind {
	switch k {
	case SeasonPresent, SeasonAbsent:
		return k
	default:
		return seasonUnknown
	}
}

// canonicalize applies Record's canonical field forms - the single home of the
// rule both producers must agree on, so exact-key lookups and the reverse
// arr-ID catalogue cannot see two differently-shaped Records for one anime.
// Idempotent.
func (r *Record) canonicalize() {
	r.Type = mediatype.Normalize(r.Type)
	r.IMDbIDs = trimmed(r.IMDbIDs)
	r.TmdbMovies = positiveInts(r.TmdbMovies)
	r.TvdbID = max(0, r.TvdbID)
	r.AniDBID = max(0, r.AniDBID)
	r.SeasonTvdb = max(0, r.SeasonTvdb)
	// Deliberately NOT clamping the (absent, positive season) pair: the kind wins
	// the scope dispatch, and the state that would be wrong, a season scope on the
	// season-0 bucket, is unreachable because HasMappedSeason IS SeasonTvdb > 0.
	r.SeasonKind = canonicalSeasonKind(r.SeasonKind)
}

// Cache is the persisted mapping state: the records and episode facts one
// animap release yields, its tag, and the refresh's refusal state. One
// document supplies all of it, so every accepted refresh replaces records and
// facts together.
type Cache struct {
	FetchedAt time.Time `json:"fetched_at"`
	// Mappings holds the facts of every anime on a TVDB series, keyed by AniDB
	// id, so a record that is one whole anime, or an override naming that
	// anime, joins them.
	Mappings map[int]Mapping `json:"anidb_facts,omitempty"`
	// AniListMappings holds the facts of every other entry, keyed by AniList
	// id: an overlay link, part of an anime, or several anime.
	AniListMappings map[int]Mapping `json:"anilist_facts,omitempty"`
	// Tag is the release the records came from; a refresh downloads only
	// another tag.
	Tag string `json:"tag,omitempty"`
	// RefusedTag is the release a refresh last REFUSED persistently. While the
	// latest release is still that tag, a usable cache is kept without the
	// download, so a persistent refusal costs one bodiless request per cycle.
	// Cleared by any accepted refresh and by a check finding the accepted tag.
	RefusedTag string   `json:"refused_tag,omitempty"`
	Records    []Record `json:"records,omitempty"`
	// RejectedRefreshes is the persisted streak of consecutive persistent refresh
	// refusals, reset by any accepted refresh or a check finding a USABLE
	// cache's tag still the latest. It is the ONE carrier: escalation and the
	// logged stale_consecutive_rejections both read this field, so the number an
	// operator reads is the number escalation acted on. It advances even when no
	// usable stale cache exists, because the streak describes the upstream
	// rather than the cache. WHICH failures advance it is
	// isPersistentRefreshFailure's call.
	RejectedRefreshes int `json:"rejected_refreshes,omitempty"`
}

// Index is an AniList-ID-keyed lookup over mapping records.
type Index struct {
	byAniList map[int]Record
	// seasonsByTvdb counts, per tvdb id, how many indexed records map each
	// POSITIVE season. Counts rather than a set, so SiblingSeasons can answer
	// "does a SIBLING map this season" for a record that maps it too, which is
	// the cour-split shape.
	seasonsByTvdb map[int]map[int]int
	// mappings and aniListMappings are Cache's two fact maps, reached only
	// through MappingFor so the join keys have one reader. aniListMappings is
	// the index's own copy, because an override deletes its entry.
	mappings        map[int]Mapping
	aniListMappings map[int]Mapping
}

// MappingFor returns what the map says about rec's episodes, and false when
// it says nothing. A record with an AniDB id joins on it; one without joins
// on its AniList id. An override joins through the AniDB id it names: it
// replaces the upstream record wholesale, its facts included. Facts belong
// to animap's series and season, so a record naming another tvdb_id loses
// them, and one naming another season loses the season episodes.
func (i *Index) MappingFor(rec *Record) (Mapping, bool) {
	if i == nil || rec == nil {
		return Mapping{}, false
	}
	var m Mapping
	if rec.AniDBID > 0 {
		m = i.mappings[rec.AniDBID]
	} else {
		m = i.aniListMappings[rec.AniListID]
	}
	if m.SeasonsTvdb != rec.TvdbID {
		m.Seasons, m.SeasonsTvdb = nil, 0
	}
	if m.SpecialsTvdb != rec.TvdbID {
		m.Specials, m.SpecialEpisode, m.SpecialsTvdb, m.SpecialsEpisodes = nil, 0, 0, 0
	}
	if rec.TvdbID != m.EpisodesTvdb || rec.SeasonTvdb != m.EpisodesSeason {
		m.Episodes, m.EpisodesTvdb, m.EpisodesSeason = nil, 0, 0
	}
	return m, !m.empty()
}

// Lookup returns the record for an AniList ID and whether it was present.
func (i *Index) Lookup(aniListID int) (Record, bool) {
	if i == nil {
		return Record{}, false
	}
	r, ok := i.byAniList[aniListID]
	return r, ok
}

// Len returns the number of indexed records.
func (i *Index) Len() int {
	if i == nil {
		return 0
	}
	return len(i.byAniList)
}

// SiblingSeasons returns the positive TVDB seasons that OTHER records sharing
// rec's tvdb id map, in ascending order, and nil for a record with no tvdb id.
// The record's own season is excluded when it is the only record mapping it, and
// kept when a cour-split sibling maps it too.
//
// A whole-series comparison drops these seasons rather than judging one entry
// against a run it does not cover. Resolving it here keeps align from reading
// the map a second time.
func (i *Index) SiblingSeasons(rec *Record) []int {
	if i == nil || rec == nil {
		return nil
	}
	// NewIndex keys only positive tvdb ids, so a record without one finds no counts.
	counts := i.seasonsByTvdb[rec.TvdbID]
	if len(counts) == 0 {
		return nil
	}
	out := make([]int, 0, len(counts))
	for season, n := range counts {
		if season == rec.SeasonTvdb && n == 1 {
			// The record itself is the only holder, so no sibling maps it.
			continue
		}
		out = append(out, season)
	}
	slices.Sort(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// ForEachRecord calls fn once per indexed record, in unspecified order. It backs
// the report's reverse (arr-ID) catalogue without materializing a slice copy of
// all ~40k records.
func (i *Index) ForEachRecord(fn func(Record)) {
	if i == nil {
		return
	}
	for _, r := range i.byAniList {
		fn(r)
	}
}

// deduplicateRecords returns one effective record per AniList ID, preserving
// NewIndex's last-record-wins semantics and stable order. Records without a
// positive AniList ID are omitted, so the acceptance validators cannot count a
// larger population than the effective served index.
func deduplicateRecords(records []Record) []Record {
	last := make(map[int]int, len(records))
	for i := range records {
		last[records[i].AniListID] = i
	}
	out := make([]Record, 0, len(last))
	for i := range records {
		if records[i].AniListID > 0 && last[records[i].AniListID] == i {
			out = append(out, records[i])
		}
	}
	return out
}

// Source is what an Index is built from: the animap records, the two episode
// fact maps, and the operator's override records.
type Source struct {
	Records []Record
	// Mappings holds each anime's facts keyed by AniDB id.
	Mappings map[int]Mapping
	// AniListMappings holds the facts of a record that is not one whole
	// anime, keyed by its AniList id.
	AniListMappings map[int]Mapping
	// Overrides replace the record of their AniList id wholesale, facts
	// included, after the index is built. They are canonical already and leave
	// the sibling-season summary as the records built it.
	Overrides []Record
}

// NewIndex keys src.Records by AniList ID, admitting only positive IDs (real
// SeaDex lookups use positive AniList IDs, so a zero or negative key could
// never resolve an entry). A later record with the same ID overwrites an
// earlier one, and src.Overrides are applied on top. Mappings is stored as
// given and AniListMappings copied (nil is a valid empty map for either).
func NewIndex(src Source) *Index {
	byAniList := make(map[int]Record, len(src.Records))
	for _, r := range src.Records {
		if r.AniListID > 0 {
			// Canonical form is an INDEX invariant, applied once here rather than
			// re-checked by every accessor: a cache read back from state.json reaches
			// NewIndex through plain encoding/json.
			r.canonicalize()
			byAniList[r.AniListID] = r
		}
	}
	// The sibling summary is accumulated over the DEDUPLICATED set, so a record
	// an earlier duplicate overwrote cannot contribute a season nothing serves.
	seasonsByTvdb := make(map[int]map[int]int)
	for _, r := range byAniList {
		if r.TvdbID <= 0 || !r.HasMappedSeason() {
			continue
		}
		seasons := seasonsByTvdb[r.TvdbID]
		if seasons == nil {
			seasons = make(map[int]int)
			seasonsByTvdb[r.TvdbID] = seasons
		}
		seasons[r.SeasonTvdb]++
	}
	aniListMappings := maps.Clone(src.AniListMappings)
	for i := range src.Overrides {
		byAniList[src.Overrides[i].AniListID] = src.Overrides[i]
		delete(aniListMappings, src.Overrides[i].AniListID)
	}
	return &Index{
		byAniList: byAniList, seasonsByTvdb: seasonsByTvdb,
		mappings: src.Mappings, aniListMappings: aniListMappings,
	}
}

// indexedRecordCount returns how many records survive into the served index
// (distinct positive AniList IDs, per NewIndex). It is the single spelling of
// the count every records / stale_records attribute reports, so a cache written
// before deduplication cannot over-report the map consumers receive.
func indexedRecordCount(records []Record) int {
	seen := make(map[int]struct{}, len(records))
	for i := range records {
		if records[i].AniListID > 0 {
			seen[records[i].AniListID] = struct{}{}
		}
	}
	return len(seen)
}

// IndexedRecords reports how many of the cached records survive into the served
// index (distinct positive AniList IDs) - the one count every mapping log
// attribute reports.
func (c *Cache) IndexedRecords() int {
	if c == nil {
		return 0
	}
	return indexedRecordCount(c.Records)
}

// cacheUsable reports whether a cached record set is usable as an effective
// AniList-keyed mapping: after deduplication the effective set must be
// non-empty and meet the same conservative 1% arr-identifier coverage floor a
// newly accepted refresh must meet, without the previous-relative type and
// shrink checks. Every cache-state gate keys on this predicate, so "has cached
// bytes" can never diverge from "has a mapping the consumers can use".
func cacheUsable(records []Record) bool {
	records = deduplicateRecords(records)
	if len(records) == 0 {
		return false
	}
	return arrIdentifierCount(records) >= coverageFloor(len(records))
}

// coverageFloor returns the conservative 1% acceptance floor (ceiling division,
// minimum 1) shared by cacheUsable and validateRefreshedRecords.
func coverageFloor(n int) int { return max(1, (n+99)/100) }

// populationExtinct is the per-population EXTINCTION guard, deliberately
// without the significance gate its sibling carries: the previously accepted
// cache had a population at all (prevCount > 0) and the candidate has none of
// it. Going from N to exactly zero is never a sampling artifact, so this is the
// one guard that reaches BELOW previousMinimum; its sibling stays gated on that
// share, so a sparse population's PARTIAL shrink keeps its exemption.
func populationExtinct(prevCount, count int) bool { return prevCount > 0 && count == 0 }

// populationCollapsed is the per-population shrink guard the routing validator
// applies beside the extinction guard: the previously accepted cache carried a
// meaningful population and the candidate retains less than half of it
// (degradation.Shrunk). populationExtinct catches total loss; this catches the
// MID-BAND, where a corrupted refresh guts most of ONE population while every
// floor stays green. Deliberately NO auto-accept after a streak:
// the documented remedy is removing state.json to cold-start onto the new shape.
func populationCollapsed(prevCount, count, previousMinimum int) bool {
	return prevCount >= previousMinimum && degradation.Shrunk(count, prevCount)
}

// cfg holds the resolved tuning knobs for a Loader.
type cfg struct {
	logger        *slog.Logger
	overridesPath string
	refresh       time.Duration
}

// Option tunes a Loader built by NewLoader.
type Option func(*cfg)

// WithOverridesPath points the loader at the local operator override file,
// whose entries win over the animap records. Defaults to empty, which loads no
// overrides; a configured path that is absent is not an error.
func WithOverridesPath(path string) Option {
	return func(c *cfg) { c.overridesPath = path }
}

// WithRefresh sets the reuse-if-fresh window before the loader asks again
// which release is the latest. Defaults to DefaultRefresh (0: every load).
func WithRefresh(d time.Duration) Option {
	return func(c *cfg) { c.refresh = d }
}

// WithLogger sets the logger the loader's diagnostics go to. Defaults to
// slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(c *cfg) { c.logger = l }
}

// NewLoader returns a mapping loader reading the latest release of src.
// httpClient must be non-nil for any loader that will fetch; the loader sends
// its requests through a copy carrying seadex-scout's User-Agent.
func NewLoader(httpClient *http.Client, src mapread.Source, opts ...Option) *Loader {
	c := &cfg{refresh: DefaultRefresh}
	for _, o := range opts {
		if o != nil {
			o(c)
		}
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	if httpClient != nil {
		agent := *httpClient
		agent.Transport = userAgent{base: httpClient.Transport}
		httpClient = &agent
	}
	return &Loader{
		http:          httpClient,
		log:           c.logger,
		source:        src,
		overridesPath: c.overridesPath,
		refresh:       c.refresh,
	}
}

// userAgent sets appinfo.UserAgent on every request it sends; mapread builds
// its requests without one.
type userAgent struct{ base http.RoundTripper }

func (u userAgent) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("User-Agent", appinfo.UserAgent)
	base := u.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// Load returns the mapping index to use this cycle and the cache to persist. It
// reuses prev when it is still fresh, otherwise asks which release is the
// latest and downloads it when its tag is new. Overrides are always
// re-read and applied on top. When a refresh fails but prev holds a usable
// record set (cacheUsable), it returns the stale index with a *StaleMapError
// (match with errors.As); any other non-nil error means no usable map at all.
// Records are canonicalized on a private copy here, so every refresh decision
// and the served Index read ONE representation.
func (l *Loader) Load(ctx context.Context, prev *Cache) (Cache, *Index, error) {
	canonicalPrev := prev
	if prev != nil {
		clone := *prev
		clone.Records = slices.Clone(prev.Records)
		for i := range clone.Records {
			clone.Records[i].canonicalize()
		}
		canonicalPrev = &clone
	}
	next, err := l.refreshCache(ctx, canonicalPrev)
	idx := NewIndex(Source{
		Records:         next.Records,
		Mappings:        next.Mappings,
		AniListMappings: next.AniListMappings,
		Overrides:       l.effectiveOverrides(ctx),
	})
	return next, idx, err
}

// StaleMapError reports a refresh failure where a stale-but-usable cached map
// was returned, so the cycle may still compare against the stale index.
// Consumers discriminate a degraded-but-comparable load with errors.As rather
// than probing the Cache's Records: the loader owns the usability judgment.
type StaleMapError struct {
	// cause is the underlying refresh failure; nil for the shrunk-refresh guard.
	cause error
	// msg describes which refresh step failed (e.g. "refresh failed").
	msg string
	// age is how long ago the stale map was fetched, rounded for logging.
	age time.Duration
	// records is the size of the stale-but-usable record set.
	records int
	// shrunkReturned/shrunkPrevious carry the shrink guard's counts as structured
	// facts, zero for every other class: keeping them OUT of msg keeps stale_reason
	// an equality-queryable, fixed-cardinality class discriminator.
	shrunkReturned int
	shrunkPrevious int
}

// Error renders the degradation facts as the single prose line the degraded
// cycle logs. The message shape is a pinned log contract (stale_map_error_test).
func (e *StaleMapError) Error() string {
	reason := e.msg
	if e.shrunkPrevious > 0 {
		reason = fmt.Sprintf("%s (returned %d, previous %d)", e.msg, e.shrunkReturned, e.shrunkPrevious)
	}
	if e.cause != nil {
		return fmt.Sprintf("mapping: %s, using stale map (%d records, fetched %s ago): %v", reason, e.records, e.age, e.cause)
	}
	return fmt.Sprintf("mapping: %s, using stale map (%d records, fetched %s ago)", reason, e.records, e.age)
}

// Unwrap exposes the underlying refresh failure for errors.Is/As chains.
func (e *StaleMapError) Unwrap() error { return e.cause }

// LogAttrs returns the degradation facts Error() flattens into prose as
// structured slog key/value pairs, so callers can emit a queryable degraded-
// cycle line without parsing the message text. It carries no rejection streak:
// that lives on Cache.RejectedRefreshes, which is what escalation reads.
func (e *StaleMapError) LogAttrs() []any {
	attrs := []any{
		"stale_reason", e.msg,
		"stale_age_seconds", e.age.Seconds(),
		"stale_records", e.records,
	}
	if e.shrunkPrevious > 0 {
		attrs = append(attrs, "stale_returned", e.shrunkReturned, "stale_previous", e.shrunkPrevious)
	}
	return attrs
}

// staleOrFail returns the stale cache wrapped in a *StaleMapError when prev
// holds a usable record set (cacheUsable; carrying cause when non-nil),
// otherwise the no-cache error - an unusable cache must degrade like no cache
// at all, so the scout preserves findings instead of comparing against an empty
// map. The age is clamped to zero, so a future FetchedAt cannot report a
// misleading negative age.
func staleOrFail(prev *Cache, staleMsg string, cause, noCache error) (Cache, error) {
	if cacheUsable(prev.Records) {
		return *prev, &StaleMapError{
			cause:   cause,
			msg:     staleMsg,
			age:     max(time.Duration(0), time.Since(prev.FetchedAt).Round(time.Second)),
			records: indexedRecordCount(prev.Records),
		}
	}
	return *prev, noCache
}

// rejectRefresh degrades a persistent refresh refusal to the stale map via
// staleOrFail, additionally advancing the persisted consecutive-rejection
// streak (Cache.RejectedRefreshes) that escalation reads. Reached ONLY through
// degradeRefresh. The streak advances even when there is NO usable stale cache
// to return: it is state about the upstream, so gating it would freeze it at 0
// on a first boot whose every refresh is refused.
func rejectRefresh(prev *Cache, staleMsg string, cause, noCache error) (Cache, error) {
	next, err := staleOrFail(prev, staleMsg, cause, noCache)
	next.RejectedRefreshes = prev.RejectedRefreshes + 1
	return next, err
}

// refreshFailureClass names WHICH step of a refresh failed. It is the
// vocabulary isPersistentRefreshFailure grades, so a call site says what
// happened and never what the streak should do about it.
type refreshFailureClass int

const (
	// failureFetch is a latest-release check, a release download or the
	// projection that refused the release; the error value decides the
	// disposition.
	failureFetch refreshFailureClass = iota
	// failureValidation is a decoded document the acceptance invariants
	// refused (validateRefreshedRecords, validateFacts).
	failureValidation
	// failureShrunk is a decoded document the below-half whole-map shrink
	// guard refused. It carries no cause; its live counts ride as structured
	// fields.
	failureShrunk
	// failureRefusedUnchanged is a latest release that is still the one a
	// previous refresh refused: the refusal stands and the streak keeps
	// advancing.
	failureRefusedUnchanged
)

// isPersistentRefreshFailure is the ONE home of the transient-vs-persistent
// refresh classification; every failure arm reaches the streak through it. A
// PERSISTENT failure re-refuses identically on every load and advances
// Cache.RejectedRefreshes: a mapread refusal (refusal names each), an
// unchanged refused release, and an acceptance or shrink refusal. Everything
// else - a transport failure, a truncated body, a come-back-later status - is
// TRANSIENT.
func isPersistentRefreshFailure(class refreshFailureClass, cause error) bool {
	switch class {
	case failureValidation, failureShrunk, failureRefusedUnchanged:
		return true
	case failureFetch:
		return refusal(cause) != ""
	}
	return false
}

// refusal names the stale_reason class of a persistent mapread refusal, ""
// for a transient failure. mapread returns a transient failure as a plain
// error, so only its sentinels are refusals.
func refusal(err error) string {
	switch {
	case errors.Is(err, mapread.ErrRelease):
		return "refresh unexpected release answer"
	case errors.Is(err, mapread.ErrRedirect):
		return "refresh redirect refused"
	case errors.Is(err, mapread.ErrChecksum):
		return "refresh checksum mismatch"
	case errors.Is(err, mapread.ErrVersion):
		return "refresh unsupported schema version"
	case errors.Is(err, mapread.ErrNotDocument):
		return "refresh not an animap document"
	case errors.Is(err, mapread.ErrInvalid):
		return "refresh invalid document"
	case errors.Is(err, mapread.ErrTooLarge):
		return "refresh exceeded size cap"
	}
	return ""
}

// degradeRefresh is the single exit from every refreshCache failure arm: it
// degrades to the stale map (or returns noCache when there is no usable one)
// and lets isPersistentRefreshFailure - never the call site - decide whether
// the failure advances the persisted rejection streak.
func degradeRefresh(prev *Cache, class refreshFailureClass, staleMsg string, cause, noCache error) (Cache, error) {
	if isPersistentRefreshFailure(class, cause) {
		return rejectRefresh(prev, staleMsg, cause, noCache)
	}
	return staleOrFail(prev, staleMsg, cause, noCache)
}

// logSafeCause reduces an untrusted-input-derived parse error to log-safe text
// (single-line, control-stripped, capped at maxLoggedErrorBytes) WITHOUT
// discarding its wrap chain, so isPersistentRefreshFailure can still recognize
// a sentinel class on the value the caller passes it.
func logSafeCause(err error) error {
	return &sanitizedError{err: err, text: runesafe.SanitizeSingleLineBounded(err.Error(), maxLoggedErrorBytes)}
}

// sanitizedError renders pre-sanitized log-safe text while preserving the
// wrapped cause for errors.Is/As classification. Only Error() reaches a log
// boundary, so the untrusted original text stays out of the log stream.
type sanitizedError struct {
	err  error
	text string
}

func (e *sanitizedError) Error() string { return e.text }
func (e *sanitizedError) Unwrap() error { return e.err }

// refreshCache decides whether to reuse the cache or download the latest
// release, and returns the cache to persist.
func (l *Loader) refreshCache(ctx context.Context, prev *Cache) (Cache, error) {
	// Load(ctx, nil) is the natural representation of "no persisted cache"; an empty
	// Cache makes it take the ordinary initial-fetch route.
	if prev == nil {
		prev = &Cache{}
	}
	age := time.Since(prev.FetchedAt)
	// age >= 0 rejects a future FetchedAt (clock skew or a corrupt state file): a
	// negative age is never fresh, forcing a check.
	if age >= 0 && age < l.refresh && cacheUsable(prev.Records) {
		l.log.Debug("mapping: cache fresh, skipping fetch", "records", indexedRecordCount(prev.Records), "age", age.Round(time.Second))
		return *prev, nil
	}
	rel, err := retried(ctx, l, func(ctx context.Context) (mapread.Release, error) { return l.source.Latest(ctx, l.http) })
	if err != nil {
		return degradeFetch(prev, err)
	}
	// An unusable cache is never kept on its tag: comparing it would affirm a
	// map the loader refuses to serve, so it downloads again.
	if cacheUsable(prev.Records) {
		switch rel.Tag {
		case prev.Tag:
			return l.reuseCachedRecords(prev), nil
		case prev.RefusedTag:
			return degradeRefresh(prev, failureRefusedUnchanged, "refused body unchanged", nil,
				errors.New("mapping: refused body unchanged and no cache available"))
		}
	}
	next, err := l.acceptRelease(ctx, prev, rel)
	if err != nil && next.RejectedRefreshes > prev.RejectedRefreshes {
		next.RefusedTag = rel.Tag
	}
	return next, err
}

// reuseCachedRecords keeps a usable cache whose tag is still the latest
// release, with a bumped timestamp. That affirms the cached map is current, so
// any rejection streak ends here.
func (l *Loader) reuseCachedRecords(prev *Cache) Cache {
	l.log.Debug("mapping: release unchanged, reusing cache", "records", indexedRecordCount(prev.Records), "tag", prev.Tag)
	if prev.RejectedRefreshes > 0 {
		l.log.Info("mapping: rejection streak ended, the cached release is the latest", "ended_rejection_streak", prev.RejectedRefreshes, "records", indexedRecordCount(prev.Records))
	}
	refreshed := *prev
	refreshed.FetchedAt = time.Now()
	refreshed.RejectedRefreshes = 0
	refreshed.RefusedTag = ""
	return refreshed
}

// retried runs one mapread request under httpx.Do's retry policy. mapread
// returns a come-back-later status as a plain *mapread.StatusError, which
// httpx cannot classify, so one whose code httpx retries is marked
// transient; a transport failure is httpx's own call.
func retried[T any](ctx context.Context, l *Loader, fn func(context.Context) (T, error)) (T, error) {
	return httpx.Do(ctx,
		func(ctx context.Context) (T, error) {
			v, err := fn(ctx)
			if se, ok := errors.AsType[*mapread.StatusError](err); ok && refusal(err) == "" && httpx.IsRetryableStatus(se.Code) {
				err = httpx.MarkTransient(err)
			}
			return v, err
		},
		httpx.WithMaxAttempts(maxAttempts),
		httpx.WithBaseDelay(baseDelay),
		httpx.WithLabel("mapping"),
		httpx.WithLogger(l.log),
		// Demoted to Debug: a refresh whose retries ran out surfaces again from the
		// caller with more context, and demoting keeps the per-attempt diagnostics.
		httpx.WithExhaustedLevel(slog.LevelDebug))
}

// degradeFetch degrades a failed check, download or projection, naming its fixed
// stale_reason class: a mapread refusal's own, else "refresh failed". The
// cause carries text a server chose (a redirect target, a key), so it is
// made log-safe first.
func degradeFetch(prev *Cache, err error) (Cache, error) {
	err = logSafeCause(err)
	reason := refusal(err)
	if reason == "" {
		reason = "refresh failed"
	}
	return degradeRefresh(prev, failureFetch, reason, err, fmt.Errorf("mapping: %s and no cache available: %w", reason, err))
}

// acceptRelease downloads rel and runs the cache-acceptance
// invariants (the validation floors and the shrink guard), degrading to
// the stale map when any step rejects the refresh.
func (l *Loader) acceptRelease(ctx context.Context, prev *Cache, rel mapread.Release) (Cache, error) {
	fetched, err := retried(ctx, l, func(ctx context.Context) (mapread.Fetched, error) {
		return rel.Fetch(ctx, l.http, mapread.Limits{MaxDocumentBytes: maxMapBytes})
	})
	if err != nil {
		return degradeFetch(prev, err)
	}
	// A body past maxMapBytes is a PERSISTENT refresh refusal that never
	// self-heals, so warn while refreshes still succeed.
	if degradation.ApproachingLimit(fetched.Bytes, maxMapBytes) {
		l.log.Warn("mapping: animap body approaching the download size cap; a body past it refuses every refresh and freezes the map stale",
			"bytes", fetched.Bytes, "cap", maxMapBytes)
	}
	parsed, err := project(fetched.Document, maxAnimeEpisodes)
	if err != nil {
		return degradeFetch(prev, err)
	}
	if degradation.ApproachingLimit(int64(parsed.animeEpisodes), maxAnimeEpisodes) {
		l.log.Warn("mapping: animap anime episodes approaching the projection cap; a document past it refuses every refresh and freezes the map stale",
			"episodes", parsed.animeEpisodes, "cap", maxAnimeEpisodes)
	}
	// Already one record per AniList id: mapread.Decode refuses a repeated one.
	records := parsed.records
	if validationErr := validateRefreshedRecords(prev.Records, records, parsed.entries); validationErr != nil {
		return degradeRefresh(prev, failureValidation, "refresh validation failed", validationErr,
			fmt.Errorf("mapping: %w and no cache available", validationErr))
	}
	// A syntactically valid but sharply truncated refresh (one record replacing
	// ~21k) can pass the coverage floor above yet silently erase most mappings, so
	// a below-half refresh (degradation.Shrunk) keeps the stale map.
	if prevCount := indexedRecordCount(prev.Records); cacheUsable(prev.Records) && degradation.Shrunk(len(records), prevCount) {
		// The noCache argument is unreachable here (cacheUsable guarantees the stale
		// branch). The reason string stays FIXED so it is class-queryable.
		next, err := degradeRefresh(prev, failureShrunk, "refresh shrank below half of previous",
			nil, errors.New("mapping: refresh shrank unexpectedly and no cache available"))
		if stale, ok := errors.AsType[*StaleMapError](err); ok {
			stale.shrunkReturned, stale.shrunkPrevious = len(records), prevCount
		}
		return next, err
	}
	if factsErr := validateFacts(prev, &parsed); factsErr != nil {
		return degradeRefresh(prev, failureValidation, "refresh validation failed", factsErr,
			fmt.Errorf("mapping: %w and no cache available", factsErr))
	}
	// previous_records is the baseline the absolute count needs: degradation.Shrunk
	// rejects only BELOW half, so an accepted refresh may legitimately retain exactly
	// half of the previous map and would otherwise read like any other success. The
	// census attributes carry the same reasoning to each population the two guards are
	// defined over, so a routing loss is one queryable line rather than an inference.
	pop := censusOf(records)
	attrs := []any{
		"tag", rel.Tag,
		"records", len(records),
		"previous_records", indexedRecordCount(prev.Records),
		"elements", parsed.entries,
		"routed_identifiers", pop.identifiers,
		"movie_routed", pop.movieRouted,
		"series_routed", pop.seriesRouted,
		"typed_records", pop.typed,
		"season_scoped_records", pop.positiveSeason,
		"special_records", pop.special,
		"season_range_records", countFacts(hasSeasonRanges, parsed.mappings, parsed.aniListMappings),
		"placed_special_records", countFacts(hasPlacedSpecials, parsed.mappings, parsed.aniListMappings),
		"placed_episode_records", countFacts(hasPlacedEpisodes, parsed.mappings, parsed.aniListMappings),
		"single_special_records", countFacts(hasSingleSpecial, parsed.mappings, parsed.aniListMappings),
	}
	if prev.RejectedRefreshes > 0 {
		attrs = append(attrs, "ended_rejection_streak", prev.RejectedRefreshes)
	}
	l.log.Info("mapping: refreshed", attrs...)
	// The zero RejectedRefreshes and RefusedTag are the accepted refresh's
	// resets; the rules are on Cache.
	return Cache{
		FetchedAt:       time.Now(),
		Tag:             rel.Tag,
		Records:         records,
		Mappings:        parsed.mappings,
		AniListMappings: parsed.aniListMappings,
	}, nil
}

// validateRefreshedRecords is acceptRelease's acceptance invariant for a
// fresh document: it rejects a refresh below the AniList-key or arr-identifier
// coverage floors, and one whose routing populations collapse below half of the
// previously accepted cache's (populationCollapsed) or vanish entirely
// (populationExtinct). The conservative 1% floor has ~21x headroom against the
// real document (8126 routed of 38323 entries, 2026-10). records MUST already
// be deduplicated, and sourceElements is the document's entry count, so
// destructive filtering cannot shrink numerator and denominator together.
func validateRefreshedRecords(previous, records []Record, sourceElements int) error {
	// No zero-record special case: coverageFloor is >= 1 for every input, so
	// the AniList-key floor below refuses an empty candidate on its own.
	keyMinimum := coverageFloor(sourceElements)
	if len(records) < keyMinimum {
		return fmt.Errorf("AniList-key coverage %d/%d is below minimum %d", len(records), sourceElements, keyMinimum)
	}
	// Anchor the arr floor on the SOURCE element count, like the AniList-key floor
	// above: deriving it from the already-key-filtered candidate would let the two
	// floors compose multiplicatively (0.01% coverage reading as healthy).
	if covered := arrIdentifierCount(records); covered < keyMinimum {
		return fmt.Errorf("arr identifier coverage %d/%d is below minimum %d", covered, sourceElements, keyMinimum)
	}
	// An unusable previous cache must degrade like no cache here too: the loader
	// refuses to serve it, so it must not anchor the loss-relative guards either -
	// a corrupted state file could otherwise reject a healthy smaller refresh.
	if !cacheUsable(previous) {
		return nil
	}
	previous = deduplicateRecords(previous)
	// The previously accepted cache's own 1% floor, derived once, gates the
	// below-half collapse guard; extinction is deliberately ungated.
	previousMinimum := coverageFloor(len(previous))
	// One census per side, counted once: both routing guards below read it, so
	// separate passes over ~40k records collapse into two.
	prevPop, pop := censusOf(previous), censusOf(records)
	return validateRoutingCoverage(prevPop, pop, previousMinimum)
}

// populations is the per-refresh census of the six semantic populations the
// acceptance guards and the accepted-refresh log line are defined over, counted
// in ONE pass. Both consumers read it: the routing guards compare candidate
// against previous, and the log line reports the candidate's census, which is
// what makes a loss below each guard's threshold visible.
type populations struct {
	typed          int
	positiveSeason int
	special        int
	movieRouted    int
	seriesRouted   int
	identifiers    int
}

// censusOf counts every population in one pass over records. The routed
// populations count only records that can actually RESOLVE in their arr
// (HasArrIdentifier), so a candidate that keeps every type but loses one side's
// usable ids reads as a collapse of that side. identifiers is their sum.
func censusOf(records []Record) populations {
	var p populations
	for i := range records {
		r := &records[i]
		if r.Type != "" {
			p.typed++
		}
		if r.HasMappedSeason() {
			p.positiveSeason++
		}
		if r.IsSpecial() {
			p.special++
		}
		if r.HasArrIdentifier() {
			p.identifiers++
			if r.IsMovie() {
				p.movieRouted++
			} else {
				p.seriesRouted++
			}
		}
	}
	return p
}

// validatePopulation applies the shared guards every semantic population is
// checked with: the extinction guard and the below-half shrink guard.
// collapseNoun carries each population's own error vocabulary.
func validatePopulation(collapseNoun string, prevCount, count, previousMinimum int) error {
	if populationExtinct(prevCount, count) {
		return fmt.Errorf("%s records went extinct (previous cache carried %d)", collapseNoun, prevCount)
	}
	if populationCollapsed(prevCount, count, previousMinimum) {
		return fmt.Errorf("%s records collapsed below half of previous (%d of previous %d)", collapseNoun, count, prevCount)
	}
	return nil
}

// validateRoutingCoverage rejects a candidate refresh that collapsed a routing
// population relative to the previously accepted cache. Routing recognizes only
// MOVIE, so a wrong-but-string schema change (every movie renamed FILM) keeps
// every record typed while routing an entire side of the catalogue to the wrong
// arr. Guard the operational invariant instead: preservation of both routing
// populations, so future non-movie labels stay legal.
func validateRoutingCoverage(previous, candidate populations, previousMinimum int) error {
	if err := validatePopulation("movie-routed", previous.movieRouted, candidate.movieRouted, previousMinimum); err != nil {
		return err
	}
	return validatePopulation("series-routed", previous.seriesRouted, candidate.seriesRouted, previousMinimum)
}

// factPopulations are the episode facts validateFacts guards, has naming a
// key that carries the fact. A single special restated as Specials for every
// entry episode is kept through completeness: Specials need every landing
// stated.
var factPopulations = []struct {
	has  func(*Mapping) bool
	noun string
}{
	{noun: "season-range", has: hasSeasonRanges},
	{noun: "special placement", has: hasPlacedSpecials},
	{noun: "season-episode placement", has: hasPlacedEpisodes},
	{noun: "single-special placement", has: hasSingleSpecial},
}

// validateFacts is the extinction and below-half shrink guard over each
// episode-fact population, so a document losing placements cannot drop them
// silently. A population is the previous cache's keys carrying the fact, in
// each key space apart, because a large one hides a small one's loss; a key
// is kept while the candidate serves its entry the fact through MappingFor's
// join, or reads it fully stated (tvdbView.fullyStated). The 1% floor gates
// only the below-half collapse; extinction is deliberately ungated.
func validateFacts(prev *Cache, parsed *projection) error {
	if !cacheUsable(prev.Records) {
		return nil
	}
	previousMinimum := coverageFloor(indexedRecordCount(prev.Records))
	aniDBOf := make(map[int]int)
	for i := range parsed.records {
		if r := &parsed.records[i]; r.AniDBID > 0 {
			aniDBOf[r.AniListID] = r.AniDBID
		}
	}
	byAniDB := func(aid int) (Mapping, bool) {
		_, stated := parsed.fullyStated[aid]
		return parsed.mappings[aid], stated
	}
	byAniList := func(al int) (Mapping, bool) {
		if aid, ok := aniDBOf[al]; ok {
			return byAniDB(aid)
		}
		_, stated := parsed.aniListFullyStated[al]
		return parsed.aniListMappings[al], stated
	}
	for _, p := range factPopulations {
		prevCount, count := keptFacts(p.has, prev.Mappings, byAniDB)
		if err := validatePopulation(p.noun, prevCount, count, previousMinimum); err != nil {
			return err
		}
		prevCount, count = keptFacts(p.has, prev.AniListMappings, byAniList)
		if err := validatePopulation("AniList-keyed "+p.noun, prevCount, count, previousMinimum); err != nil {
			return err
		}
	}
	return nil
}

// keptFacts counts the keys of prev whose fact has names, and how many of
// them candidate still gives the fact or reads fully stated.
func keptFacts(has func(*Mapping) bool, prev map[int]Mapping, candidate func(key int) (Mapping, bool)) (prevCount, count int) {
	for key, f := range prev {
		if !has(&f) {
			continue
		}
		prevCount++
		if m, stated := candidate(key); stated || has(&m) {
			count++
		}
	}
	return prevCount, count
}

func hasSeasonRanges(f *Mapping) bool   { return len(f.Seasons) > 0 }
func hasPlacedSpecials(f *Mapping) bool { return len(f.Specials) > 0 }
func hasPlacedEpisodes(f *Mapping) bool { return len(f.Episodes) > 0 }
func hasSingleSpecial(f *Mapping) bool  { return f.SpecialEpisode > 0 }

func countFacts(has func(*Mapping) bool, fact ...map[int]Mapping) int {
	n := 0
	for _, m := range fact {
		for _, f := range m {
			if has(&f) {
				n++
			}
		}
	}
	return n
}

// arrIdentifierCount returns how many records retain an arr identifier the
// lookup paths actually consume (per HasArrIdentifier). It backs
// acceptRelease's acceptance guard: an entry with no id is still a record.
func arrIdentifierCount(records []Record) int {
	n := 0
	for i := range records {
		if records[i].HasArrIdentifier() {
			n++
		}
	}
	return n
}

// maxLoggedErrorBytes bounds untrusted-input-derived parse-error text before
// it reaches a log emit boundary (the anilist sanitizeUpstreamMessage policy).
const maxLoggedErrorBytes = 200

// effectiveOverrides reads the operator overrides file (if present) and returns
// its effective records for NewIndex to overlay, keyed by AniList ID. A missing
// file is not an error; an unreadable or malformed file is logged at ERROR and
// ignored. The overlay is WHOLESALE, not a merge, so a record carrying no
// identifier its routed arr consumes replaces a mapped upstream record with one
// that resolves to nothing. That is left applied - an operator entry wins by
// design - but it is reported, because a mistyped id key is otherwise invisible.
func (l *Loader) effectiveOverrides(ctx context.Context) []Record {
	if l.overridesPath == "" {
		return nil
	}
	set, ok := l.readOverrides(ctx)
	if !ok {
		return nil
	}
	unroutable := 0
	for i := range set.records {
		if !set.records[i].HasArrIdentifier() {
			unroutable++
		}
	}
	if set.skipped > 0 {
		l.log.Warn("mapping: overrides with missing or invalid anilist_id skipped", "skipped", set.skipped, "path", l.overridesPath)
	}
	if unroutable > 0 {
		l.log.Warn("mapping: overrides carry no arr identifier and un-map their entry; check for a mistyped tvdb_id/tmdb_movies/imdb_ids key, and restate the ids when overriding only a type or season",
			"count", unroutable, "path", l.overridesPath)
	}
	if set.applied > 0 {
		l.log.Info("mapping: applied overrides", "count", set.applied)
	}
	return set.records
}

// readOverridesFile reads the overrides file through an os.Root over its own
// directory, so the read can neither be redirected out of /config nor block:
// atomicfile.ReadBoundedInRoot opens with O_NONBLOCK and refuses a non-regular
// inode, where a plain os.Open follows a symlink and blocks indefinitely on a
// writer-less FIFO, past its only context check. A missing file still surfaces
// as fs.ErrNotExist.
func (l *Loader) readOverridesFile(ctx context.Context) ([]byte, error) {
	dir := filepath.Dir(l.overridesPath)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() {
		if clErr := root.Close(); clErr != nil {
			l.log.Warn("mapping: could not close overrides directory handle", "dir", dir, "error", clErr)
		}
	}()
	return atomicfile.ReadBoundedInRoot(ctx, root, filepath.Base(l.overridesPath), maxOverrideBytes)
}

// readOverrides reads and parses the overrides file, returning ok=false for
// every ignored outcome: a cancelled read, a missing file (silently), an
// unreadable or malformed file (logged at ERROR). Unknown keys are counted in a
// WARN but never reject the file. A file-level refusal is an ERROR because the
// file is opt-in - its existence means the operator intends those mappings to
// apply - and the failure persists until they act. NOT wired into
// Cache.RejectedRefreshes, which counts UPSTREAM refresh refusals.
func (l *Loader) readOverrides(ctx context.Context) (overrideSet, bool) {
	data, err := l.readOverridesFile(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return overrideSet{}, false
		}
		if !errors.Is(err, fs.ErrNotExist) {
			l.log.Error("mapping: overrides.json unreadable, pinned mappings not applied; fix the file's permissions or contents, or remove it", "path", l.overridesPath, "error", err)
		}
		return overrideSet{}, false
	}
	set, err := parseOverrides(data)
	if err != nil {
		l.log.Error("mapping: overrides.json malformed, pinned mappings not applied; fix the file's JSON, or remove it", "path", l.overridesPath,
			"error", errors.New(runesafe.SanitizeSingleLineBounded(err.Error(), maxLoggedErrorBytes)))
		return overrideSet{}, false
	}
	if set.unknown > 0 {
		l.log.Warn("mapping: overrides contain unknown keys, ignored", "unknown_key_count", set.unknown, "path", l.overridesPath)
	}
	return set, true
}

// overrideSet is parseOverrides' result: the effective overlay plus the
// diagnostics effectiveOverrides logs. records holds only effective records
// (positive AniList ID, deduplicated last-record-wins); applied counts the
// positive-ID transport rows; skipped counts the non-positive-ID rows; unknown
// counts the non-canonical keys seen.
type overrideSet struct {
	records []Record
	applied int
	skipped int
	unknown int
}

// maxOverrideRecords caps the effective records parseOverrides retains: the
// 4 MiB wire bound caps the file, not the retained amplification of ~250k
// tiny distinct-ID records. An over-cap file routes through readOverrides'
// malformed-file ERROR, refusing the whole overlay.
const maxOverrideRecords = 1 << 16

// decodeOverrideRecord walks one override object off the token stream in a
// single bounded pass: the eight canonical keys decode directly into the Record
// and an unknown key is counted and skipped. The literals restate Record's json
// tags, which is the edit Record's own doc requires.
func decodeOverrideRecord(dec *jsoncap.Decoder, set *overrideSet) (record Record, err error) {
	err = dec.Object(func(key string) error {
		switch {
		case strings.EqualFold(key, "anilist_id"):
			return dec.Decode(&record.AniListID)
		case strings.EqualFold(key, "type"):
			return dec.Decode(&record.Type)
		case strings.EqualFold(key, "tvdb_id"):
			return dec.Decode(&record.TvdbID)
		case strings.EqualFold(key, "anidb_id"):
			return dec.Decode(&record.AniDBID)
		case strings.EqualFold(key, "season_tvdb"):
			return dec.Decode(&record.SeasonTvdb)
		case strings.EqualFold(key, "season_kind"):
			// A value that is neither named spelling reads unknown through
			// canonicalize, so it is a recognized KEY carrying an unusable value
			// rather than an unknown key.
			return dec.Decode(&record.SeasonKind)
		case strings.EqualFold(key, "tmdb_movies"):
			return dec.Decode(&record.TmdbMovies)
		case strings.EqualFold(key, "imdb_ids"):
			return dec.Decode(&record.IMDbIDs)
		default:
			set.unknown++
			return dec.Skip()
		}
	})
	return record, err
}

// applyRecord decodes the next override record from the token stream and folds
// it into the set, canonicalized: a zero-AniList-ID record counts as skipped,
// and a duplicate ID replaces its earlier record.
func (set *overrideSet) applyRecord(dec *jsoncap.Decoder, position map[int]int) error {
	record, err := decodeOverrideRecord(dec, set)
	if err != nil {
		return err
	}
	record.canonicalize()
	if record.AniListID <= 0 {
		// Zero (missing) and negative alike: an indexed negative key matches no
		// SeaDex lookup while still leaking into the reverse arr-ID catalogue.
		set.skipped++
		return nil
	}
	set.applied++
	if at, dup := position[record.AniListID]; dup {
		set.records[at] = record
		return nil
	}
	// Reject BEFORE retaining: a new distinct record allocates both a set.records
	// slot and a position map entry, so the cardinality cap has to fire here. A
	// duplicate at the cap still replaces its earlier record, retaining nothing new.
	if len(set.records) >= maxOverrideRecords {
		return fmt.Errorf("mapping: overrides exceed cap %d records", maxOverrideRecords)
	}
	position[record.AniListID] = len(set.records)
	set.records = append(set.records, record)
	return nil
}

func trimmed(in []string) []string {
	var out []string
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// positiveInts returns in with non-positive entries dropped, matching the
// canonical TmdbMovies form, so an override record and an animap record agree
// on the exact TMDB keys downstream lookups use.
func positiveInts(in []int) []int {
	var out []int
	for _, v := range in {
		if v > 0 {
			out = append(out, v)
		}
	}
	return out
}

// parseOverrides decodes the overrides file - a JSON array of Record objects,
// each keyed by its AniList ID - streaming one record at a time so the peak
// allocation tracks the effective overlay, not the transport row count.
// maxOverrideBytes bounds the wire size, but a compact array of semantically
// empty records or a single near-cap record fits under it while whole-value
// materialization would multiply it past the container's memory budget. The
// top-level value must be a JSON array with no trailing data: encoding/json
// would otherwise accept a literal null as a valid empty overlay.
func parseOverrides(data []byte) (overrideSet, error) {
	trimmedData := bytes.TrimSpace(data)
	if len(trimmedData) == 0 || trimmedData[0] != '[' {
		return overrideSet{}, errors.New("mapping: overrides must be a JSON array")
	}
	dec := jsoncap.NewDecoder(bytes.NewReader(trimmedData), 0)
	if _, err := dec.Open('['); err != nil { // the '['-first-byte guard above rules out null
		return overrideSet{}, err
	}
	var set overrideSet
	position := make(map[int]int) // AniList ID -> index in set.records
	for dec.More() {
		if err := set.applyRecord(dec, position); err != nil {
			return overrideSet{}, err
		}
	}
	if err := dec.Close(); err != nil { // the closing ']'
		return overrideSet{}, err
	}
	if err := dec.End(); err != nil {
		return overrideSet{}, fmt.Errorf("mapping: overrides carry data after the JSON array: %w", err)
	}
	return set, nil
}
