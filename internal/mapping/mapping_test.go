package mapping

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/slogx/capture"
)

func TestRecord_IsMovie(t *testing.T) {
	if !(&Record{Type: "MOVIE"}).IsMovie() {
		t.Error("Record{MOVIE}.IsMovie() = false, want true")
	}
	if (&Record{Type: "TV"}).IsMovie() {
		t.Error("Record{TV}.IsMovie() = true, want false")
	}
}

func TestRecord_IsSpecial(t *testing.T) {
	tests := map[string]bool{
		"OVA": true, "ONA": true, "SPECIAL": true, "MUSIC": true,
		"TV": false, "MOVIE": false, "": false,
	}
	for typ, want := range tests {
		if got := (&Record{Type: typ}).IsSpecial(); got != want {
			t.Errorf("Record{%q}.IsSpecial() = %v, want %v", typ, got, want)
		}
	}
}

// TestRecord_HasMappedSeason pins the season predicate to a POSITIVE Fribb
// season. Season 0 is what an unmapped record and a Fribb-typed special both
// carry, so admitting it would relabel a pack's season half to S00 on the
// strength of a season the upstream never stated, and would count every
// seasonless record toward the season-scoped population.
func TestRecord_HasMappedSeason(t *testing.T) {
	tests := map[int]bool{-1: false, 0: false, 1: true, 12: true}
	for season, want := range tests {
		if got := (&Record{SeasonTvdb: season}).HasMappedSeason(); got != want {
			t.Errorf("Record{SeasonTvdb: %d}.HasMappedSeason() = %v, want %v", season, got, want)
		}
	}
}

// TestRecord_SeasonPresence pins the three-valued season kind: the ZERO value
// reads unknown rather than absent (the one state a Record persisted before the
// field existed can carry, and reading it as absent would strand every
// mapped-zero special for a 304 window), an unrecognized string reads unknown
// too rather than failing the record, and the two named spellings survive.
func TestRecord_SeasonPresence(t *testing.T) {
	tests := map[SeasonKind]SeasonKind{
		"":            SeasonUnknown,
		SeasonPresent: SeasonPresent,
		SeasonAbsent:  SeasonAbsent,
		"mapped":      SeasonUnknown,
		"PRESENT":     SeasonUnknown,
	}
	for stored, want := range tests {
		if got := (&Record{SeasonKind: stored}).SeasonPresence(); got != want {
			t.Errorf("Record{SeasonKind: %q}.SeasonPresence() = %q, want %q", stored, got, want)
		}
	}
}

// TestNewIndex_canonicalizesSeasonKind pins the kind's normalization at the
// INDEX boundary, which is what lets the scope dispatch read the field directly:
// buildIndex canonicalizes every record on both the Fribb and the persisted-cache
// path, so an unrecognized string can never reach a consumer as a fourth state,
// while a record's real kind round-trips untouched.
func TestNewIndex_canonicalizesSeasonKind(t *testing.T) {
	idx := NewIndex([]Record{
		{AniListID: 1, Type: "TV", TvdbID: 100, SeasonKind: "mapped-positive", SeasonTvdb: 2},
		{AniListID: 2, Type: "TV", TvdbID: 200, SeasonKind: SeasonPresent},
		{AniListID: 3, Type: "OVA", TvdbID: 300, SeasonKind: SeasonAbsent},
		{AniListID: 4, Type: "TV", TvdbID: 400},
	})
	want := map[int]SeasonKind{1: SeasonUnknown, 2: SeasonPresent, 3: SeasonAbsent, 4: SeasonUnknown}
	for id, kind := range want {
		rec, ok := idx.Lookup(id)
		if !ok {
			t.Fatalf("Lookup(%d) missing", id)
		}
		if rec.SeasonKind != kind {
			t.Errorf("Lookup(%d).SeasonKind = %q, want %q", id, rec.SeasonKind, kind)
		}
	}
	// The garbage kind normalizes without touching the season NUMBER: the pair
	// (unknown, positive season) is legitimate and takes the union arm.
	if rec, _ := idx.Lookup(1); rec.SeasonTvdb != 2 {
		t.Errorf("Lookup(1).SeasonTvdb = %d, want 2 (normalizing the kind must not clamp the season)", rec.SeasonTvdb)
	}
}

// TestRecord_seasonKindRoundTrips pins the persisted json key: it is additive
// and omitempty, so an unknown kind writes nothing while the two named spellings
// survive a Cache write and read.
func TestRecord_seasonKindRoundTrips(t *testing.T) {
	for _, kind := range []SeasonKind{SeasonUnknown, SeasonPresent, SeasonAbsent} {
		encoded, err := json.Marshal(Record{AniListID: 7, Type: "TV", SeasonKind: kind})
		if err != nil {
			t.Fatalf("Marshal(%q) error: %v", kind, err)
		}
		if kind == SeasonUnknown && strings.Contains(string(encoded), "season_kind") {
			t.Errorf("Marshal(unknown) = %s, want no season_kind key (omitempty)", encoded)
		}
		var back Record
		if err := json.Unmarshal(encoded, &back); err != nil {
			t.Fatalf("Unmarshal(%s) error: %v", encoded, err)
		}
		if back.SeasonKind != kind {
			t.Errorf("round trip of %q = %q", kind, back.SeasonKind)
		}
	}
}

// TestParseOverrides_seasonKind pins the override door season-kind needs: without
// the key an operator cannot place an entry at all, since a hand-written override
// would decode to unknown forever. A mapped zero is what says "this
// title is filed under its parent's specials", and an unrecognized value reads
// unknown while still counting as a RECOGNIZED key.
func TestParseOverrides_seasonKind(t *testing.T) {
	set, err := parseOverrides([]byte(`[
		{"anilist_id":5,"type":"tv","tvdb_id":100,"season_kind":"present","season_tvdb":0},
		{"anilist_id":6,"type":"tv","tvdb_id":200,"season_kind":"absent"},
		{"anilist_id":7,"type":"tv","tvdb_id":300,"season_kind":"maybe"}
	]`))
	if err != nil {
		t.Fatalf("parseOverrides error: %v", err)
	}
	if len(set.records) != 3 {
		t.Fatalf("records = %d, want 3", len(set.records))
	}
	want := []SeasonKind{SeasonPresent, SeasonAbsent, SeasonUnknown}
	for i, kind := range want {
		if set.records[i].SeasonKind != kind {
			t.Errorf("records[%d].SeasonKind = %q, want %q", i, set.records[i].SeasonKind, kind)
		}
	}
	if set.records[0].SeasonTvdb != 0 {
		t.Errorf("records[0].SeasonTvdb = %d, want 0 (a mapped zero is the whole point of the key)", set.records[0].SeasonTvdb)
	}
	if set.unknown != 0 {
		t.Errorf("unknown keys = %d, want none (season_kind is canonical; an odd VALUE is not an odd KEY)", set.unknown)
	}
}

// TestParseOverrides_anidbID pins the override door for the mapping-list join
// key: an operator names WHICH Anime-Lists node an entry is (the facts stay in
// the list), the key survives canonicalize, a negative value clamps to absent,
// and the key is recognized rather than counted unknown.
func TestParseOverrides_anidbID(t *testing.T) {
	set, err := parseOverrides([]byte(`[
		{"anilist_id":5,"type":"movie","anidb_id":12276},
		{"anilist_id":6,"type":"tv","tvdb_id":200,"anidb_id":-3}
	]`))
	if err != nil {
		t.Fatalf("parseOverrides error: %v", err)
	}
	if len(set.records) != 2 {
		t.Fatalf("records = %d, want 2", len(set.records))
	}
	if got := set.records[0].AniDBID; got != 12276 {
		t.Errorf("records[0].AniDBID = %d, want 12276", got)
	}
	if got := set.records[1].AniDBID; got != 0 {
		t.Errorf("records[1].AniDBID = %d, want 0 (a negative id clamps to absent)", got)
	}
	if set.unknown != 0 {
		t.Errorf("unknown keys = %d, want none (anidb_id is canonical)", set.unknown)
	}
}

// TestRecord_HasArrIdentifier pins the arr-routed identifier predicate: only
// the fields the record's routed arr consumes count (TMDB-movie/IMDb for
// movies, TVDB for series), so a wrong-arm identifier can neither satisfy the
// refresh coverage floor nor catalogue an item for the opposite arr.
func TestRecord_HasArrIdentifier(t *testing.T) {
	tests := []struct {
		name string
		rec  Record
		want bool
	}{
		{"series with tvdb", Record{Type: "TV", TvdbID: 100}, true},
		{"series with only movie ids", Record{Type: "TV", TmdbMovies: []int{4}, IMDbIDs: []string{"tt1"}}, false},
		{"movie with tmdb", Record{Type: "MOVIE", TmdbMovies: []int{4}}, true},
		{"movie with imdb", Record{Type: "MOVIE", IMDbIDs: []string{"tt1"}}, true},
		{"movie with only tvdb", Record{Type: "MOVIE", TvdbID: 100}, false},
		{"no ids", Record{Type: "TV"}, false},
		{"series with negative tvdb", Record{Type: "TV", TvdbID: -1}, false},
		{"movie with a canonicalized tmdb list", Record{Type: "MOVIE", TmdbMovies: []int{4}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rec.HasArrIdentifier(); got != tt.want {
				t.Errorf("HasArrIdentifier() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestRecord_CrossTypeMovieIDDoesNotWidenRouting guards the seam the ID bridge
// keeps: exposing cross-type movie evidence must NOT make a non-MOVIE record read as
// id-ful, because HasArrIdentifier is what gates the AniList title fallback -
// widening it there would strand such a record with no fallback at all.
func TestRecord_CrossTypeMovieIDDoesNotWidenRouting(t *testing.T) {
	rec := Record{Type: "OVA", TmdbMovies: []int{4}}
	if rec.HasArrIdentifier() {
		t.Error("HasArrIdentifier() = true for a non-MOVIE record carrying only movie ids, want false")
	}
	if tvdb, movies, imdb := rec.RoutedIDs(); tvdb != 0 || movies != nil || imdb != nil {
		t.Errorf("RoutedIDs() = (%d, %v, %v), want the empty series routing", tvdb, movies, imdb)
	}
}

// TestArrIdentifierCountIgnoresWrongArmIdentifiers pins the refresh coverage
// guard to the same arr-routed predicate the matcher uses: a TV record
// carrying only movie ids (or a MOVIE record carrying only a TVDB id) cannot
// count toward the acceptance floor, because FindByID would never consume
// those fields for that record's arr.
func TestArrIdentifierCountIgnoresWrongArmIdentifiers(t *testing.T) {
	records := []Record{
		{AniListID: 1, Type: "TV", TmdbMovies: []int{4}, IMDbIDs: []string{"tt1"}},
		{AniListID: 2, Type: "MOVIE", TvdbID: 100},
		{AniListID: 3, Type: "TV", TvdbID: 100},
		{AniListID: 4, Type: "MOVIE", IMDbIDs: []string{"tt2"}},
	}
	if got := arrIdentifierCount(records); got != 2 {
		t.Errorf("arrIdentifierCount = %d, want 2 (wrong-arm identifiers must not count)", got)
	}
}

func TestIndex_nilSafe(t *testing.T) {
	var idx *Index
	if _, ok := idx.Lookup(1); ok {
		t.Error("nil Index Lookup returned ok=true")
	}
	if idx.Len() != 0 {
		t.Error("nil Index Len != 0")
	}
	called := false
	idx.ForEachRecord(func(Record) { called = true })
	if called {
		t.Error("nil Index ForEachRecord invoked fn")
	}
	if _, ok := idx.MappingFor(&Record{AniDBID: 1}); ok {
		t.Error("nil Index MappingFor returned ok=true")
	}
}

// TestIndex_MappingFor pins the one reader of the Anime-Lists join: a record with
// no AniDB id never joins (0 is absent, not a key), an id the list lacks is
// false, a known id returns the stored value, and the served map is the one
// handed to the index rather than anything derived from Record.
func TestIndex_MappingFor(t *testing.T) {
	want := Mapping{SpecialEpisode: 8, Seasons: []SeasonRange{{Season: 1, First: 1, Last: 13}}}
	idx := NewIndexWithMappings(
		[]Record{{AniListID: 1, Type: "MOVIE", AniDBID: 12276}, {AniListID: 2, Type: "TV", AniDBID: 999}, {AniListID: 3, Type: "TV"}},
		map[int]Mapping{12276: want, 0: {SpecialEpisode: 99}},
	)
	tests := []struct {
		name    string
		rec     *Record
		want    Mapping
		wantHit bool
	}{
		{name: "nil_record", rec: nil},
		{name: "no_anidb_id_never_joins_the_zero_key", rec: &Record{AniListID: 3}},
		{name: "id_the_list_lacks", rec: &Record{AniListID: 2, AniDBID: 999}},
		{name: "known_id", rec: &Record{AniListID: 1, AniDBID: 12276}, want: want, wantHit: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := idx.MappingFor(tc.rec)
			if ok != tc.wantHit {
				t.Fatalf("MappingFor(%+v) ok = %v, want %v", tc.rec, ok, tc.wantHit)
			}
			if got.SpecialEpisode != tc.want.SpecialEpisode || !slices.Equal(got.Seasons, tc.want.Seasons) {
				t.Errorf("MappingFor(%+v) = %+v, want %+v", tc.rec, got, tc.want)
			}
		})
	}
	if _, ok := NewIndex([]Record{{AniListID: 1, AniDBID: 12276}}).MappingFor(&Record{AniDBID: 12276}); ok {
		t.Error("NewIndex (no list) MappingFor returned ok=true, want false")
	}
}

// TestLoader_Load_overrideAniDBIDJoinsTheSameList pins the override rule: an
// override that names an anidb_id joins the SAME persisted list the Fribb
// records join, so the operator supplies the key and never the facts.
func TestLoader_Load_overrideAniDBIDJoinsTheSameList(t *testing.T) {
	dir := t.TempDir()
	overrides := filepath.Join(dir, "overrides.json")
	if err := os.WriteFile(overrides, []byte(`[{"anilist_id":42,"type":"movie","tmdb_movies":[7],"anidb_id":12276}]`), 0o600); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"anilist_id":42,"type":"tv","tvdb_id":100}]`))
	}))
	defer ts.Close()
	prev := &Cache{
		FetchedAt: time.Now().Add(-2 * time.Hour),
		Records:   []Record{{AniListID: 42, Type: "TV", TvdbID: 100}},
		Mappings:  map[int]Mapping{12276: {SpecialEpisode: 8}},
	}
	l := NewLoader(ts.Client(), ts.URL, WithOverridesPath(overrides), WithLogger(discardLogger()))
	_, idx, err := l.Load(t.Context(), prev)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	rec, ok := idx.Lookup(42)
	if !ok || rec.AniDBID != 12276 || !rec.IsMovie() {
		t.Fatalf("Lookup(42) = %+v ok=%v, want the override record carrying anidb_id 12276", rec, ok)
	}
	m, ok := idx.MappingFor(&rec)
	if !ok || m.SpecialEpisode != 8 {
		t.Errorf("MappingFor(override) = %+v ok=%v, want the persisted list's episode 8", m, ok)
	}
}

func TestIndex_ForEachRecordAndNewIndex(t *testing.T) {
	idx := NewIndex([]Record{{AniListID: 1}, {AniListID: 2}})
	var got []int
	idx.ForEachRecord(func(r Record) { got = append(got, r.AniListID) })
	slices.Sort(got)
	if !slices.Equal(got, []int{1, 2}) {
		t.Errorf("ForEachRecord visited %v, want [1 2]", got)
	}
}

func TestParseOverrides(t *testing.T) {
	set, err := parseOverrides([]byte(`[{"anilist_id":5,"type":"  movie  ","imdb_ids":[" tt2222222 ",""],"tmdb_movies":[0,42,-5]}]`))
	if err != nil {
		t.Fatalf("parseOverrides error: %v", err)
	}
	if len(set.records) != 1 || set.records[0].Type != "MOVIE" {
		t.Fatalf("parseOverrides = %+v, want one record with Type MOVIE", set.records)
	}
	// IMDb ids must be normalized like Fribb's (trimmed, blanks dropped) so
	// HasArrIdentifier, findMovie, and the report catalogue agree on the
	// exact lookup key.
	if got := set.records[0].IMDbIDs; !slices.Equal(got, []string{"tt2222222"}) {
		t.Errorf("IMDbIDs = %v, want [tt2222222] (trimmed, blank dropped)", got)
	}
	// TMDB movie ids likewise: non-positive entries are dropped to match the
	// canonical form flexInt+intSlice guarantee on the Fribb path, so an
	// override cannot introduce a phantom zero/negative lookup key.
	if got := set.records[0].TmdbMovies; !slices.Equal(got, []int{42}) {
		t.Errorf("TmdbMovies = %v, want [42] (non-positive entries dropped)", got)
	}
	if set.unknown != 0 {
		t.Errorf("unknown keys = %d, want none for a well-formed override", set.unknown)
	}
	if _, err := parseOverrides([]byte(`{bad`)); err == nil {
		t.Error("parseOverrides(malformed) = nil error, want error")
	}
	if _, err := parseOverrides([]byte(`null`)); err == nil {
		t.Error("parseOverrides(null) = nil error, want error (a non-array top level must not read as an empty overlay)")
	}
	if _, err := parseOverrides([]byte(`[] trailing`)); err == nil {
		t.Error("parseOverrides(trailing data) = nil error, want error (json.Unmarshal parity)")
	}
}

// TestParseOverridesReportsUnknownKeys pins the unknown-key detection: an
// operator writing the upstream Fribb field names (imdb_id, themoviedb_id,
// season) instead of the override names gets them counted while the records
// still parse.
func TestParseOverridesReportsUnknownKeys(t *testing.T) {
	data := []byte(`[{"anilist_id":5,"imdb_id":"tt1","season":1},{"anilist_id":6,"imdb_id":"tt2","themoviedb_id":9}]`)
	set, err := parseOverrides(data)
	if err != nil {
		t.Fatalf("parseOverrides error: %v", err)
	}
	if len(set.records) != 2 {
		t.Errorf("records = %d, want 2 (unknown keys do not reject the record)", len(set.records))
	}
	if set.unknown != 4 {
		t.Errorf("unknown keys = %d, want 4 (every non-canonical key counted)", set.unknown)
	}
}

// TestParseOverridesAcceptsCaseVariantKeys pins the diagnostic's key matching
// to encoding/json's: a case-variant canonical key (e.g. "ANILIST_ID", "TYPE")
// is decoded and applied by the typed unmarshal, so it must not be reported as
// unknown and "ignored" - that would tell the operator an accepted field was
// discarded.
func TestParseOverridesAcceptsCaseVariantKeys(t *testing.T) {
	set, err := parseOverrides([]byte(`[{"ANILIST_ID":5,"TYPE":"movie"}]`))
	if err != nil {
		t.Fatalf("parseOverrides error: %v", err)
	}
	if len(set.records) != 1 || set.records[0].AniListID != 5 || set.records[0].Type != "MOVIE" {
		t.Errorf("parseOverrides = %+v, want one record with AniListID 5 and Type MOVIE", set.records)
	}
	if set.unknown != 0 {
		t.Errorf("unknown keys = %d, want none for case-variant canonical keys (encoding/json accepts them)", set.unknown)
	}
}

// TestNewIndex_ignoresZeroAndKeepsLastDuplicate pins the public NewIndex
// contract consumers rely on: non-positive AniList IDs are omitted
// (unkeyable; real AniList IDs are positive) and the last duplicate wins, so
// upstream ordering cannot silently retain a stale record.
func TestNewIndex_ignoresZeroAndKeepsLastDuplicate(t *testing.T) {
	idx := NewIndex([]Record{
		{AniListID: 0, Type: "TV", TvdbID: 99},
		{AniListID: -7, Type: "TV", TvdbID: 77},
		{AniListID: 42, Type: "TV", TvdbID: 100},
		{AniListID: 42, Type: "TV", TvdbID: 200},
	})

	if got := idx.Len(); got != 1 {
		t.Errorf("NewIndex length = %d, want 1", got)
	}
	got, ok := idx.Lookup(42)
	if !ok {
		t.Fatal("NewIndex lookup 42 missing")
	}
	if got.TvdbID != 200 {
		t.Errorf("NewIndex duplicate TVDB ID = %d, want last value 200", got.TvdbID)
	}
	if _, ok := idx.Lookup(0); ok {
		t.Error("NewIndex retained zero AniList ID")
	}
	if _, ok := idx.Lookup(-7); ok {
		t.Error("NewIndex retained negative AniList ID")
	}
}

// TestParseOverrides_duplicateIDKeepsLastRecord pins the effective set's
// duplicate rule: a repeated AniList ID replaces its earlier record during the
// stream, so the overlay is deduplicated last-record-wins while applied still
// counts every keyed transport row.
func TestParseOverrides_duplicateIDKeepsLastRecord(t *testing.T) {
	set, err := parseOverrides([]byte(`[
		{"anilist_id":1,"type":"TV","tvdb_id":10},
		{"anilist_id":1,"type":"TV","tvdb_id":11},
		{"anilist_id":1,"type":"TV","tvdb_id":12},
		{"anilist_id":2,"type":"TV","tvdb_id":20},
		{"anilist_id":2,"type":"TV","tvdb_id":21}
	]`))
	if err != nil {
		t.Fatalf("parseOverrides error: %v", err)
	}
	if set.applied != 5 {
		t.Errorf("applied = %d, want 5 (every keyed record applies)", set.applied)
	}
	if len(set.records) != 2 {
		t.Errorf("effective records = %d, want 2 (deduplicated during the stream)", len(set.records))
	}
	idx := NewIndex(set.records)
	if got, ok := idx.Lookup(1); !ok || got.TvdbID != 12 {
		t.Errorf("Lookup(1) = %+v, %v, want last record with TvdbID 12", got, ok)
	}
	if got, ok := idx.Lookup(2); !ok || got.TvdbID != 21 {
		t.Errorf("Lookup(2) = %+v, %v, want last record with TvdbID 21", got, ok)
	}
}

// TestParseOverrides_discardsSemanticallyEmptyRowsDuringStream pins the
// memory-amplification guard: a valid compact array of empty objects fits under
// maxOverrideBytes, so a parser materializing it whole allocates the array three
// times over before discarding every row. A large all-empty-object array must
// parse to an EMPTY effective overlay with the exact skipped count, allocating no
// []Record growth per transport row.
func TestParseOverrides_discardsSemanticallyEmptyRowsDuringStream(t *testing.T) {
	const rows = 100_000
	data := []byte("[" + strings.Repeat("{},", rows-1) + "{}]")
	set, err := parseOverrides(data)
	if err != nil {
		t.Fatalf("parseOverrides error: %v", err)
	}
	if len(set.records) != 0 || cap(set.records) != 0 {
		t.Errorf("effective records len=%d cap=%d, want 0/0 (zero-ID rows discarded during the stream)", len(set.records), cap(set.records))
	}
	if set.skipped != rows {
		t.Errorf("skipped = %d, want the exact discarded row count %d", set.skipped, rows)
	}
	if set.applied != 0 || set.unknown != 0 {
		t.Errorf("applied=%d unknown=%d, want both zero", set.applied, set.unknown)
	}
}

// TestRecord_RoutedIDsRoutesToTheSelectedArrArm pins the exported output
// contract internal/match's matcher and catalogue consume. The existing
// HasArrIdentifier test only observes a boolean, so it cannot see ids escaping
// from the wrong arr arm. Usability is NOT re-checked here: canonicalize owns it
// at both producers and again on insertion into the index, so the movie arm
// returns the record's own already-canonical lists.
func TestRecord_RoutedIDsRoutesToTheSelectedArrArm(t *testing.T) {
	tests := []struct {
		name        string
		record      Record
		wantTVDB    int
		wantTMDB    []int
		wantIMDbIDs []string
	}{
		{
			name:        "movie returns its canonical id lists and ignores the series arm",
			record:      Record{Type: "MOVIE", TvdbID: 100, TmdbMovies: []int{42}, IMDbIDs: []string{"tt1"}},
			wantTMDB:    []int{42},
			wantIMDbIDs: []string{"tt1"},
		},
		{
			name:     "series returns only a positive TVDB id",
			record:   Record{Type: "TV", TvdbID: 100, TmdbMovies: []int{42}, IMDbIDs: []string{"tt1"}},
			wantTVDB: 100,
		},
		{
			name:   "series drops a non-positive TVDB id",
			record: Record{Type: "TV", TvdbID: -1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotTVDB, gotTMDB, gotIMDbIDs := tt.record.RoutedIDs()
			if gotTVDB != tt.wantTVDB || !slices.Equal(gotTMDB, tt.wantTMDB) || !slices.Equal(gotIMDbIDs, tt.wantIMDbIDs) {
				t.Errorf("RoutedIDs() = (%d, %v, %v), want (%d, %v, %v)", gotTVDB, gotTMDB, gotIMDbIDs, tt.wantTVDB, tt.wantTMDB, tt.wantIMDbIDs)
			}
		})
	}
}

// TestIndex_SiblingSeasons pins the per-tvdb summary the whole-series aggregate
// consumes. "A SIBLING maps this season" has to be exact rather than "any record
// maps it", which is why the accumulator counts holders: a record must not report
// its OWN season back when it is that season's only holder, but must when a
// cour-split sibling shares it (Fire Force season 3 spans two AniList entries in
// one TVDB season).
func TestIndex_SiblingSeasons(t *testing.T) {
	idx := NewIndex([]Record{
		// Gintama's shape: one seasonless entry plus season-scoped siblings.
		{AniListID: 918, Type: "TV", TvdbID: 79895, SeasonKind: SeasonAbsent},
		{AniListID: 100, Type: "TV", TvdbID: 79895, SeasonKind: SeasonPresent, SeasonTvdb: 5},
		{AniListID: 101, Type: "TV", TvdbID: 79895, SeasonKind: SeasonPresent, SeasonTvdb: 6},
		// A cour split: two entries on one season.
		{AniListID: 200, Type: "TV", TvdbID: 12345, SeasonKind: SeasonPresent, SeasonTvdb: 3},
		{AniListID: 201, Type: "TV", TvdbID: 12345, SeasonKind: SeasonPresent, SeasonTvdb: 3},
		// A sole holder of its season, on its own tvdb id.
		{AniListID: 300, Type: "TV", TvdbID: 6789, SeasonKind: SeasonPresent, SeasonTvdb: 1},
		// No tvdb id at all.
		{AniListID: 400, Type: "MOVIE", TmdbMovies: []int{7}},
	})
	tests := []struct {
		name string
		id   int
		want []int
	}{
		{name: "a seasonless record reports every sibling season", id: 918, want: []int{5, 6}},
		{name: "a season-scoped record does not report its own sole season", id: 100, want: []int{6}},
		{name: "a cour-split record DOES report its shared season", id: 200, want: []int{3}},
		{name: "a sole holder on its own tvdb id reports nothing", id: 300},
		{name: "a record with no tvdb id reports nil", id: 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, ok := idx.Lookup(tt.id)
			if !ok {
				t.Fatalf("Lookup(%d) missing", tt.id)
			}
			if got := idx.SiblingSeasons(&rec); !slices.Equal(got, tt.want) {
				t.Errorf("SiblingSeasons(%d) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
	var nilIndex *Index
	if got := nilIndex.SiblingSeasons(&Record{TvdbID: 1}); got != nil {
		t.Errorf("nil index SiblingSeasons = %v, want nil", got)
	}
}

// TestRecord_AllIDsDiffersFromRoutedIDsOnlyByTheTypeGate pins the whole type
// gate: AllIDs and RoutedIDs differ EXACTLY on a MOVIE record's TVDB id and a
// non-MOVIE record's movie/IMDb lists, and HasArrIdentifier is byte-identical
// for every record, because widening the routed accessor would move the AniList
// fallback's gate for 38 entries. The fixture is drawn from the live shapes: a
// both-arr film, a season-scoped series, a seasonless OVA and an id-less row.
func TestRecord_AllIDsDiffersFromRoutedIDsOnlyByTheTypeGate(t *testing.T) {
	fixture := []Record{
		{Type: "MOVIE", AniListID: 11577, TvdbID: 78964, SeasonTvdb: 0, TmdbMovies: []int{5528}, IMDbIDs: []string{"tt0169858"}},
		{Type: "MOVIE", AniListID: 21519, TvdbID: 296666, IMDbIDs: []string{"tt5311514"}},
		{Type: "TV", AniListID: 154587, TvdbID: 424536, SeasonTvdb: 1},
		{Type: "OVA", AniListID: 820, TvdbID: 78964},
		{Type: "TV", AniListID: 1, IMDbIDs: []string{"tt0000001"}},
		{Type: "", AniListID: 2},
	}
	for i := range fixture {
		rec := &fixture[i]
		routedTVDB, routedTMDB, routedIMDb := rec.RoutedIDs()
		allTVDB, allTMDB, allIMDb := rec.AllIDs()
		wantTVDB, wantTMDB, wantIMDb := rec.TvdbID, rec.TmdbMovies, rec.IMDbIDs
		if allTVDB != wantTVDB || !slices.Equal(allTMDB, wantTMDB) || !slices.Equal(allIMDb, wantIMDb) {
			t.Errorf("record %d AllIDs() = (%d, %v, %v), want every id it carries (%d, %v, %v)",
				rec.AniListID, allTVDB, allTMDB, allIMDb, wantTVDB, wantTMDB, wantIMDb)
		}
		if rec.IsMovie() {
			if routedTVDB != 0 || allTVDB != rec.TvdbID {
				t.Errorf("movie %d: RoutedIDs tvdb = %d (want 0, the discarded id), AllIDs tvdb = %d (want %d)",
					rec.AniListID, routedTVDB, allTVDB, rec.TvdbID)
			}
			continue
		}
		if len(routedTMDB) != 0 || len(routedIMDb) != 0 {
			t.Errorf("series %d: RoutedIDs returned movie ids (%v, %v), want none", rec.AniListID, routedTMDB, routedIMDb)
		}
	}
	// The predicate five consumers share must not move: it is derived from
	// RoutedIDs, which this step deliberately leaves alone.
	want := []bool{true, true, true, true, false, false}
	for i := range fixture {
		if got := fixture[i].HasArrIdentifier(); got != want[i] {
			t.Errorf("record %d HasArrIdentifier() = %v, want %v (AllIDs must not widen the routing predicate)",
				fixture[i].AniListID, got, want[i])
		}
	}
}

// TestBuildIndexCanonicalizesRecords pins the boundary that OWNS the id-usability
// rule: the accessors do not filter on read, so a record reaching buildIndex
// through plain encoding/json - the persisted mapping cache replayed on the 304
// and stale-on-error paths - must be canonicalized on insertion, which is what
// makes RoutedIDs' presence check sound.
func TestBuildIndexCanonicalizesRecords(t *testing.T) {
	idx := NewIndex([]Record{{
		AniListID:  7,
		Type:       "movie",
		TvdbID:     -3,
		SeasonTvdb: -1,
		TmdbMovies: []int{0, -1, 42},
		IMDbIDs:    []string{"", "  ", " tt1 "},
	}})
	rec, ok := idx.Lookup(7)
	if !ok {
		t.Fatal("Lookup(7) = not found, want the indexed record")
	}
	if !slices.Equal(rec.TmdbMovies, []int{42}) {
		t.Errorf("TmdbMovies = %v, want [42] (canonicalized on insertion)", rec.TmdbMovies)
	}
	if !slices.Equal(rec.IMDbIDs, []string{"tt1"}) {
		t.Errorf("IMDbIDs = %v, want [tt1] (canonicalized on insertion)", rec.IMDbIDs)
	}
	if rec.TvdbID != 0 || rec.SeasonTvdb != 0 {
		t.Errorf("TvdbID/SeasonTvdb = %d/%d, want 0/0 (canonicalized on insertion)", rec.TvdbID, rec.SeasonTvdb)
	}
	if _, tmdb, imdb := rec.RoutedIDs(); !slices.Equal(tmdb, []int{42}) || !slices.Equal(imdb, []string{"tt1"}) {
		t.Errorf("RoutedIDs() = (_, %v, %v), want ([42], [tt1]) over the canonical record", tmdb, imdb)
	}
}

// overIdentifierBudgetFribbBody builds the smallest valid Fribb array that
// exceeds the aggregate identifier budget: every record retains both capped
// identifier lists (maxFribbIdentifiers each), so one record past
// maxFribbIdentifiersTotal/(2*maxFribbIdentifiers) trips the budget while the
// element count (16385 records) stays well under maxFribbRecords (65536) - the
// per-record caps and the record cap must not be what refuses this body.
func overIdentifierBudgetFribbBody() []byte {
	perRecord := 2 * maxFribbIdentifiers
	var b strings.Builder
	b.WriteByte('[')
	for i := range maxFribbIdentifiersTotal/perRecord + 1 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"anilist_id":%d,"type":"MOVIE","imdb_id":[`, i+1)
		for j := range maxFribbIdentifiers {
			if j > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"tt%d"`, j+1)
		}
		b.WriteString(`],"themoviedb_id":{"movie":[`)
		for j := range maxFribbIdentifiers {
			if j > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `%d`, j+1)
		}
		b.WriteString(`]}}`)
	}
	b.WriteByte(']')
	return []byte(b.String())
}

// TestAcceptRefresh_identifierBudgetFailsClosed pins the fail-closed contract of
// the aggregate identifier budget across the whole decode-to-acceptance path,
// which the counter-level fribbDecodeCounts.add test cannot reach: a body that
// trips the budget is refused WHOLE (the truncated prefix is never published)
// with errIdentifierBudgetExceeded, and acceptRefresh routes that sentinel
// through rejectRefresh - a first boot publishes no index, a usable previous
// cache is returned stale rather than replaced, and the persisted rejection
// streak advances instead of staying frozen as a transient parse failure would.
func TestAcceptRefresh_identifierBudgetFailsClosed(t *testing.T) {
	body := overIdentifierBudgetFribbBody()

	t.Run("parse refuses the whole body", func(t *testing.T) {
		parsed, err := parseFribbForRefresh(body, discardLogger())
		if !errors.Is(err, errIdentifierBudgetExceeded) {
			t.Fatalf("parseFribbForRefresh error = %v, want errIdentifierBudgetExceeded", err)
		}
		if len(parsed.records) != 0 {
			t.Errorf("budget breach retained %d records, want the whole body refused", len(parsed.records))
		}
	})

	t.Run("first boot publishes nothing", func(t *testing.T) {
		l := &Loader{log: discardLogger()}
		next, err := l.acceptRefresh(&Cache{}, httpx.ConditionalResult{Body: body})
		if !errors.Is(err, errIdentifierBudgetExceeded) {
			t.Fatalf("first-boot error = %v, want errIdentifierBudgetExceeded", err)
		}
		if _, ok := errors.AsType[*StaleMapError](err); ok {
			t.Errorf("first-boot error = %v, want the no-cache error rather than a *StaleMapError", err)
		}
		if len(next.Records) != 0 {
			t.Errorf("first boot published %d records, want none", len(next.Records))
		}
		if next.RejectedRefreshes != 1 {
			t.Errorf("first-boot RejectedRefreshes = %d, want 1 (a budget breach is a persistent guard refusal)", next.RejectedRefreshes)
		}
	})

	t.Run("usable cache is kept stale", func(t *testing.T) {
		prev := &Cache{
			Records:           []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
			RejectedRefreshes: 2,
		}
		l := &Loader{log: discardLogger()}
		next, err := l.acceptRefresh(prev, httpx.ConditionalResult{Body: body})
		if _, ok := errors.AsType[*StaleMapError](err); !ok {
			t.Fatalf("budget-breach error = %v, want a *StaleMapError guard rejection", err)
		}
		if !errors.Is(err, errIdentifierBudgetExceeded) {
			t.Errorf("budget-breach error does not match errIdentifierBudgetExceeded through the StaleMapError wrap: %v", err)
		}
		if len(next.Records) != 1 || next.Records[0].AniListID != 1 {
			t.Errorf("budget-breach records = %+v, want the stale record id 1 retained", next.Records)
		}
		if next.RejectedRefreshes != 3 {
			t.Errorf("budget-breach RejectedRefreshes = %d, want 3 (the prior streak advances)", next.RejectedRefreshes)
		}
	})
}

// TestAcceptRefresh_staleReasonClassVocabulary pins stale_reason as the
// fixed-cardinality degradation CLASS the operator queries in Loki (the
// discriminator StaleMapError deliberately keeps live counts out of, so the
// attribute stays equality-queryable). Five of the classes acceptRefresh can emit
// have no assertion anywhere, so a swapped or merged reason string would silently
// file a never-self-heals refusal (record cap, identifier budget, validation
// floor, moved schema) under the transient vocabulary, and the escalation runbook
// keys on exactly that distinction.
func TestAcceptRefresh_staleReasonClassVocabulary(t *testing.T) {
	var capBody strings.Builder
	capBody.WriteByte('[')
	for i := 0; i <= maxFribbRecords; i++ {
		if i > 0 {
			capBody.WriteByte(',')
		}
		fmt.Fprintf(&capBody, `{"anilist_id":%d}`, i+1)
	}
	capBody.WriteByte(']')

	tests := []struct {
		name string
		body []byte
		want string
	}{
		{name: "record cap", body: []byte(capBody.String()), want: "refresh exceeded record cap"},
		{name: "identifier budget", body: overIdentifierBudgetFribbBody(), want: "refresh exceeded identifier budget"},
		{name: "validation floor", body: []byte(`[{"anilist_id":1,"type":"tv"}]`), want: "refresh validation failed"},
		{name: "non-array document", body: []byte(`{"data":[]}`), want: "refresh not a JSON array"},
		{name: "malformed body", body: []byte(`[{"anilist_id":1,`), want: "parse failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prev := &Cache{Records: []Record{{AniListID: 1, Type: "TV", TvdbID: 100}}}
			l := &Loader{log: discardLogger()}
			_, err := l.acceptRefresh(prev, httpx.ConditionalResult{Body: tc.body})
			stale, ok := errors.AsType[*StaleMapError](err)
			if !ok {
				t.Fatalf("acceptRefresh error = %v, want a *StaleMapError over a usable cache", err)
			}
			if got := stale.LogAttrs(); !attrsContain(got, "stale_reason", tc.want) {
				t.Errorf("stale_reason attrs = %v, want %q", got, tc.want)
			}
		})
	}
}

// approachingSizeCapMessage is the fixed WARN message acceptRefresh emits
// before the download size cap becomes a hard refusal. It is a Loki-queryable
// log contract (the class is the message, the facts ride as structured
// fields), so the test pins it verbatim.
const approachingSizeCapMessage = "mapping: Fribb body approaching the download size cap; " +
	"a body past it refuses every refresh and freezes the map stale"

// TestAcceptRefresh_approachingDownloadSizeCapWarns pins the operator's only
// advance notice before maxMapBytes becomes a permanent refusal. A body past the
// download cap grades PERSISTENT (isPersistentRefreshFailure over
// *httpx.ResponseTooLargeError), so every later cycle re-downloads the multi-MB
// body, re-refuses it, and the map stays frozen stale while the persisted
// rejection streak escalates to ERROR - with no signal at all while refreshes were
// still succeeding. At exactly the threshold the WARN fires once (the shared guard
// is inclusive); one byte below it stays silent.
func TestAcceptRefresh_approachingDownloadSizeCapWarns(t *testing.T) {
	// The threshold is hardcoded rather than recomputed from the shared fraction:
	// a fixture derived from the expression under test moves with it, and this
	// test's whole subject is which byte count the warning starts at.
	// 13421768 is maxMapBytes/10*8 (16 MiB), truncated down.
	const warnThresholdBytes = 13_421_768
	prev := &Cache{Records: []Record{{AniListID: 1, Type: "TV", TvdbID: 100}}}

	logger, rec := capture.New()
	at := &Loader{log: logger}
	next, err := at.acceptRefresh(prev, httpx.ConditionalResult{Body: make([]byte, warnThresholdBytes)})
	if err == nil {
		t.Fatal("acceptRefresh(at-threshold body) = nil error, want the stale-map refusal")
	}
	if len(next.Records) != 1 || next.Records[0].AniListID != 1 {
		t.Fatalf("acceptRefresh kept %+v, want the stale record id 1", next.Records)
	}
	if n := rec.CountLevel(slog.LevelWarn, approachingSizeCapMessage); n != 1 {
		t.Fatalf("a body at the threshold warned %d times at WARN, want exactly 1 (the guard is inclusive, and a demoted level vanishes from the deployed info-level stream): %v", n, rec.Messages())
	}
	if !rec.HasAttr(approachingSizeCapMessage, "bytes", strconv.Itoa(warnThresholdBytes)) {
		t.Errorf("approaching-cap log = %v, want bytes=%d", rec.Messages(), warnThresholdBytes)
	}
	if !rec.HasAttr(approachingSizeCapMessage, "cap", strconv.Itoa(maxMapBytes)) {
		t.Errorf("approaching-cap log = %v, want cap=%d", rec.Messages(), maxMapBytes)
	}

	belowLogger, belowRec := capture.New()
	below := &Loader{log: belowLogger}
	if _, err := below.acceptRefresh(prev, httpx.ConditionalResult{Body: make([]byte, warnThresholdBytes-1)}); err == nil {
		t.Fatal("acceptRefresh(below-threshold body) = nil error, want the stale-map refusal")
	}
	if n := belowRec.CountExact(approachingSizeCapMessage); n != 0 {
		t.Errorf("a body one byte below the threshold warned %d times, want 0: %v", n, belowRec.Messages())
	}
}

// censusBody is a four-record Fribb body whose populations are all distinct, so
// a count attributed to the wrong population is visible: three records carry a
// type, one carries a positive TVDB season, one is a special, three resolve in
// their routed arr (one movie, two series), and the last record carries neither
// a type nor an identifier.
const censusBody = `[{"anilist_id":10,"type":"tv","tvdb_id":200},` +
	`{"anilist_id":11,"type":"movie","themoviedb_id":{"movie":[5]}},` +
	`{"anilist_id":12,"type":"ova","tvdb_id":300,"season":{"tvdb":2}},` +
	`{"anilist_id":13}]`

// TestAcceptRefresh_logsTheCandidateCensus pins the population census the
// accepted-refresh line carries. Each guard refuses only a below-half collapse,
// so a routing or type loss shallower than that is accepted, and these counts
// are the ONLY operator-visible record that it happened: a count that drifts
// from the records it describes retires the signal while still logging a line.
func TestAcceptRefresh_logsTheCandidateCensus(t *testing.T) {
	prev := &Cache{Records: []Record{{AniListID: 1, Type: "TV", TvdbID: 100}}}
	logger, rec := capture.New()
	at := &Loader{log: logger}
	if _, err := at.acceptRefresh(prev, httpx.ConditionalResult{Body: []byte(censusBody)}); err != nil {
		t.Fatalf("acceptRefresh(census body) = %v, want the refresh accepted", err)
	}
	if n := rec.CountExact("mapping: refreshed"); n != 1 {
		t.Fatalf("accepted refresh logged %d refreshed lines, want 1: %v", n, rec.Messages())
	}
	for key, want := range map[string]string{
		"records":               "4",
		"typed_records":         "3",
		"season_scoped_records": "1",
		"special_records":       "1",
		"routed_identifiers":    "3",
		"movie_routed":          "1",
		"series_routed":         "2",
	} {
		got, ok := rec.AttrValueExact("mapping: refreshed", key)
		if !ok {
			t.Errorf("refreshed log carries no %s attribute; logs = %v", key, rec.Messages())
			continue
		}
		if got != want {
			t.Errorf("refreshed log %s = %s, want %s", key, got, want)
		}
	}
}

// TestAcceptRefresh_revalidatableReportsAPersistedValidator pins the
// revalidatable attribute on the accepted-refresh line: it tells the operator
// whether the next cycle can revalidate cheaply or must re-download the whole
// list, and EITHER validator alone is enough to make it true.
func TestAcceptRefresh_revalidatableReportsAPersistedValidator(t *testing.T) {
	for name, tc := range map[string]struct {
		validators httpx.Validators
		want       string
	}{
		"etag only":          {httpx.Validators{ETag: `"v9"`}, "true"},
		"last-modified only": {httpx.Validators{LastModified: "Mon, 02 Jan 2006 15:04:05 GMT"}, "true"},
		"neither":            {httpx.Validators{}, "false"},
	} {
		t.Run(name, func(t *testing.T) {
			prev := &Cache{Records: []Record{{AniListID: 1, Type: "TV", TvdbID: 100}}}
			logger, rec := capture.New()
			at := &Loader{log: logger}
			res := httpx.ConditionalResult{Body: []byte(censusBody), Validators: tc.validators}
			if _, err := at.acceptRefresh(prev, res); err != nil {
				t.Fatalf("acceptRefresh(%+v) = %v, want the refresh accepted", tc.validators, err)
			}
			got, ok := rec.AttrValueExact("mapping: refreshed", "revalidatable")
			if !ok {
				t.Fatalf("refreshed log carries no revalidatable attribute; logs = %v", rec.Messages())
			}
			if got != tc.want {
				t.Errorf("acceptRefresh(%+v) logged revalidatable=%s, want %s", tc.validators, got, tc.want)
			}
		})
	}
}

// shrinkingBody replaces a four-record cache with one record, which is below
// half and so trips the shrink guard - a PERSISTENT refusal, because the same
// upstream body is refused identically on every cycle.
const shrinkingBody = `[{"anilist_id":9,"type":"tv","tvdb_id":900}]`

// fourRecordCache is the usable stale cache the shrink guard measures against.
func fourRecordCache() *Cache {
	return &Cache{Records: []Record{
		{AniListID: 1, Type: "TV", TvdbID: 100},
		{AniListID: 2, Type: "TV", TvdbID: 200},
		{AniListID: 3, Type: "TV", TvdbID: 300},
		{AniListID: 4, Type: "TV", TvdbID: 400},
	}}
}

// TestAcceptRefresh_persistentRefusalRemembersTheRefusedValidators pins the
// suppression the refusal memory buys: a body the acceptance pipeline refuses
// persistently is remembered by its validators, so the next cycle asks about
// that body with a conditional GET instead of re-downloading ~5.9 MB for as
// long as the refusal lasts. Both validators are remembered, since either one
// alone is enough for the upstream to answer 304.
func TestAcceptRefresh_persistentRefusalRemembersTheRefusedValidators(t *testing.T) {
	const lastModified = "Mon, 02 Jan 2006 15:04:05 GMT"
	prev := fourRecordCache()
	at := &Loader{log: discardLogger()}
	res := httpx.ConditionalResult{
		Body:       []byte(shrinkingBody),
		Validators: httpx.Validators{ETag: `"v9"`, LastModified: lastModified},
	}
	next, err := at.acceptRefresh(prev, res)
	if err == nil {
		t.Fatal("acceptRefresh(below-half body) = nil error, want the shrink-guard refusal")
	}
	if next.RejectedRefreshes != prev.RejectedRefreshes+1 {
		t.Fatalf("acceptRefresh RejectedRefreshes = %d, want %d (a shrink refusal is persistent)", next.RejectedRefreshes, prev.RejectedRefreshes+1)
	}
	if next.RefusedETag != `"v9"` {
		t.Errorf("acceptRefresh RefusedETag = %q, want %q", next.RefusedETag, `"v9"`)
	}
	if next.RefusedLastModified != lastModified {
		t.Errorf("acceptRefresh RefusedLastModified = %q, want %q", next.RefusedLastModified, lastModified)
	}
}

// TestAcceptRefresh_transientRefusalDoesNotRememberValidators pins the other
// side: a body that failed for a reason which can self-heal next cycle (a
// truncated download) must NOT be remembered as refused. Remembering it would
// make the next cycle ask about a body the upstream still serves happily, so a
// 304 would then classify as refused-unchanged and freeze the map stale on a
// failure that was never the body's fault.
func TestAcceptRefresh_transientRefusalDoesNotRememberValidators(t *testing.T) {
	prev := fourRecordCache()
	at := &Loader{log: discardLogger()}
	res := httpx.ConditionalResult{
		Body:       []byte(`[{"anilist_id":1,`), // truncated mid-record
		Validators: httpx.Validators{ETag: `"v9"`, LastModified: "Mon, 02 Jan 2006 15:04:05 GMT"},
	}
	next, err := at.acceptRefresh(prev, res)
	if err == nil {
		t.Fatal("acceptRefresh(truncated body) = nil error, want the stale-map refusal")
	}
	if next.RejectedRefreshes != prev.RejectedRefreshes {
		t.Fatalf("acceptRefresh RejectedRefreshes = %d, want %d (a truncated body is transient)", next.RejectedRefreshes, prev.RejectedRefreshes)
	}
	if next.RefusedETag != "" || next.RefusedLastModified != "" {
		t.Errorf("acceptRefresh remembered refused validators (%q, %q) for a transient failure, want neither",
			next.RefusedETag, next.RefusedLastModified)
	}
}
