package mapping

import (
	"bytes"
	"errors"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/animap/mapread"
	"github.com/cplieger/seadex-scout/internal/appinfo"
)

func freshCache() *Cache {
	return &Cache{
		FetchedAt: time.Now(),
		Records:   []Record{{AniListID: 1, TvdbID: 100}},
	}
}

// staleCache is a usable cache of one series record, fetched two hours ago
// from release tag.
func staleCache(tag string) *Cache {
	return &Cache{
		FetchedAt: time.Now().Add(-2 * time.Hour),
		Tag:       tag,
		Records:   []Record{{AniListID: 1, TvdbID: 100}},
	}
}

// unused is a loader that must not reach the network.
func unused(opts ...Option) *Loader {
	return NewLoader(nil, mapread.Source{Repo: DefaultRepo}, append([]Option{WithLogger(discardLogger())}, opts...)...)
}

func TestLoader_refreshCache_reusesFreshCache(t *testing.T) {
	next, err := unused(WithRefresh(time.Hour)).refreshCache(t.Context(), freshCache())
	if err != nil {
		t.Fatalf("refreshCache error: %v", err)
	}
	if len(next.Records) != 1 {
		t.Errorf("fresh reuse kept %d records, want 1 (no fetch)", len(next.Records))
	}
}

// TestLoader_refreshCache_downloadsANewTag pins the refresh: it asks which
// release is the latest, then reads that tag's sidecar and body, both pinned
// to the tag, and persists the tag beside the records.
func TestLoader_refreshCache_downloadsANewTag(t *testing.T) {
	rel := newRelease(t, testTag, entriesBody(t, entry{AniList: 42, Type: "tv", TVDB: 100, Lands: targets(at(1, 1))}))
	next, err := rel.loader(WithRefresh(time.Hour)).refreshCache(t.Context(), staleCache("v2026.10.16"))
	if err != nil {
		t.Fatalf("refreshCache error: %v", err)
	}
	if len(next.Records) != 1 || next.Records[0].AniListID != 42 || next.Records[0].SeasonTvdb != 1 {
		t.Errorf("refresh records = %+v, want one record id 42 on season 1", next.Records)
	}
	if next.Tag != testTag || next.RefusedTag != "" || next.RejectedRefreshes != 0 {
		t.Errorf("refresh tag %q, refused %q, streak %d, want %q, none, 0", next.Tag, next.RefusedTag, next.RejectedRefreshes, testTag)
	}
	want := []string{"/" + DefaultRepo + "/releases/latest", assetPath(testTag, ".sha256"), assetPath(testTag, "")}
	if got := rel.paths(); !slices.Equal(got, want) {
		t.Errorf("requests = %v, want %v", got, want)
	}
}

// TestLoader_refreshCache_sendsTheUserAgent pins appinfo.UserAgent on every
// request of a refresh, which mapread does not set itself.
func TestLoader_refreshCache_sendsTheUserAgent(t *testing.T) {
	rel := newRelease(t, testTag, entriesBody(t, seriesEntries(1)...))
	var (
		mu     sync.Mutex
		agents []string
	)
	record := func(r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		agents = append(agents, r.UserAgent())
	}
	rel.edit(func(rr *release) {
		rr.asset = func(_ http.ResponseWriter, r *http.Request) bool {
			record(r)
			return false
		}
		rr.latest = func(w http.ResponseWriter, r *http.Request) {
			record(r)
			w.Header().Set("Location", rr.srv.URL+"/"+DefaultRepo+"/releases/tag/"+testTag)
			w.WriteHeader(http.StatusFound)
		}
	})
	if _, err := rel.loader().refreshCache(t.Context(), nil); err != nil {
		t.Fatalf("refreshCache error: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(agents) != 3 || slices.ContainsFunc(agents, func(a string) bool { return a != appinfo.UserAgent }) {
		t.Errorf("User-Agents = %q, want %q on all three requests", agents, appinfo.UserAgent)
	}
}

// TestLoader_Load_factsTravelWithTheirRecords pins that the episode facts
// share the records' fate on every refresh path: an accepted release
// replaces both with its own, and every other outcome keeps the cached pair.
func TestLoader_Load_factsTravelWithTheirRecords(t *testing.T) {
	doc, err := mapread.Decode(bytes.NewReader(entriesBody(t,
		entry{AniList: 1, Type: "tv", TVDB: 100, Lands: targets(at(0, 3))},
		entry{AniList: 2, Type: "tv", TVDB: 200, Lands: targets(at(2, 5))},
	)), mapread.DefaultLimits())
	if err != nil {
		t.Fatalf("decoding the fixture: %v", err)
	}
	// AniList 3 is anime 1's episode as a ranged part, so its facts are
	// AniList-keyed.
	doc.Entries = append(doc.Entries, mapread.Entry{AniListID: 3, Type: "OVA", Parts: []mapread.Part{{AniDBID: 1, Kind: mapread.Regular, Start: 1, End: 1}}})
	body := encode(t, document(doc.Anime, doc.Entries))
	prevMappings := map[int]Mapping{1: {Specials: []int{9}, SpecialsTvdb: 100, SpecialsEpisodes: 1}}
	prevAniList := map[int]Mapping{3: {Specials: []int{9}, SpecialsTvdb: 100, SpecialsEpisodes: 1}}
	tests := []struct {
		serve      func(r *release)
		want       map[int]Mapping
		wantAniLst map[int]Mapping
		name       string
		refresh    time.Duration
		fresh      bool
	}{
		{name: "accepted release", serve: func(*release) {}, want: map[int]Mapping{
			1: {Specials: []int{3}, SpecialsTvdb: 100, SpecialsEpisodes: 1},
			2: {Seasons: []SeasonRange{{Season: 2, First: 1, Last: 1}}, SeasonsTvdb: 200, Episodes: []int{5}, EpisodesTvdb: 200, EpisodesSeason: 2},
		}, wantAniLst: map[int]Mapping{3: {Specials: []int{3}, SpecialsTvdb: 100, SpecialsEpisodes: 1}}},
		{name: "release unchanged", serve: func(r *release) { r.publish("v2026.10.16", body) }, want: prevMappings, wantAniLst: prevAniList},
		{name: "server error", serve: func(r *release) {
			r.latest = func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) }
		}, want: prevMappings, wantAniLst: prevAniList},
		{name: "persistent refusal", serve: func(r *release) { r.publish(testTag, []byte(`{"version":1,"records":[]}`)) }, want: prevMappings, wantAniLst: prevAniList},
		{name: "fresh cache", serve: func(r *release) {
			r.latest = func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "must not be asked", http.StatusTeapot) }
		}, refresh: time.Hour, fresh: true, want: prevMappings, wantAniLst: prevAniList},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rel := newRelease(t, testTag, body)
			rel.edit(tc.serve)
			prev := staleCache("v2026.10.16")
			if tc.fresh {
				prev.FetchedAt = time.Now()
			}
			prev.Mappings, prev.AniListMappings = maps.Clone(prevMappings), maps.Clone(prevAniList)
			next, _, _ := rel.loader(WithRefresh(tc.refresh)).Load(t.Context(), prev)
			if !maps.EqualFunc(next.Mappings, tc.want, sameMapping) {
				t.Errorf("Load().Mappings = %+v, want %+v", next.Mappings, tc.want)
			}
			if !maps.EqualFunc(next.AniListMappings, tc.wantAniLst, sameMapping) {
				t.Errorf("Load().AniListMappings = %+v, want %+v", next.AniListMappings, tc.wantAniLst)
			}
		})
	}
}

// TestLoader_refreshCache_unchangedTagSkipsTheDownload pins the tag
// comparison: a usable cache of the latest tag is kept with a bumped
// timestamp, ends any rejection streak and clears a remembered refusal,
// after one bodiless request.
func TestLoader_refreshCache_unchangedTagSkipsTheDownload(t *testing.T) {
	rel := newRelease(t, testTag, entriesBody(t, seriesEntries(1)...))
	prev := staleCache(testTag)
	prev.RejectedRefreshes, prev.RefusedTag = 3, "v2026.10.01"
	next, err := rel.loader(WithRefresh(time.Hour)).refreshCache(t.Context(), prev)
	if err != nil {
		t.Fatalf("refreshCache error: %v", err)
	}
	if rel.downloads() != 0 || len(rel.paths()) != 1 {
		t.Errorf("requests = %v, want only the latest-release check", rel.paths())
	}
	if len(next.Records) != 1 || !next.FetchedAt.After(prev.FetchedAt) {
		t.Errorf("kept cache = %+v, want the cached record with a bumped FetchedAt", next)
	}
	if next.RejectedRefreshes != 0 || next.RefusedTag != "" || next.Tag != testTag {
		t.Errorf("streak %d, refused %q, tag %q, want 0, none, %q", next.RejectedRefreshes, next.RefusedTag, next.Tag, testTag)
	}
}

// TestLoader_refreshCache_unchangedTagOnAnUnusableCacheDownloads pins the
// usability gate on the tag comparison: a cache that serves no map is never
// affirmed by its tag, so the release is downloaded again.
func TestLoader_refreshCache_unchangedTagOnAnUnusableCacheDownloads(t *testing.T) {
	for name, records := range map[string][]Record{
		"no records":            nil,
		"a zero AniList id":     {{}},
		"no arr identifier":     {{AniListID: 1}},
		"non-canonical ids":     {{AniListID: 7, TmdbMovies: []int{0}}},
		"negative AniList keys": {{AniListID: -1, TvdbID: 100}},
	} {
		t.Run(name, func(t *testing.T) {
			rel := newRelease(t, testTag, entriesBody(t, entry{AniList: 42, Type: "tv", TVDB: 100, Lands: targets(at(1, 1))}))
			prev := &Cache{FetchedAt: time.Now(), Tag: testTag, RefusedTag: testTag, Records: records}
			next, idx, err := rel.loader(WithRefresh(time.Hour)).Load(t.Context(), prev)
			if err != nil {
				t.Fatalf("Load error: %v", err)
			}
			if rel.downloads() != 1 {
				t.Errorf("downloads = %d, want 1: an unusable cache must not be kept on its tag", rel.downloads())
			}
			if rec, ok := idx.Lookup(42); !ok || len(next.Records) != 1 || rec.TvdbID != 100 {
				t.Errorf("Load records = %+v, Lookup(42) = %+v %v, want the downloaded record", next.Records, rec, ok)
			}
			if len(records) > 0 && records[0].AniListID == 7 && records[0].TmdbMovies[0] != 0 {
				t.Errorf("Load mutated the caller's cache: %+v", records[0])
			}
		})
	}
}

// TestLoader_Load_nilCacheFetches pins the documented no-persisted-cache
// entry point: Load(ctx, nil) takes the ordinary initial-fetch route and
// returns the fetched records in both the persisted Cache and the Index.
func TestLoader_Load_nilCacheFetches(t *testing.T) {
	rel := newRelease(t, testTag, entriesBody(t, entry{AniList: 42, Type: "tv", TVDB: 100}))
	next, idx, err := rel.loader(WithRefresh(time.Hour)).Load(t.Context(), nil)
	if err != nil {
		t.Fatalf("Load(nil) error: %v", err)
	}
	if len(next.Records) != 1 || next.Records[0].AniListID != 42 {
		t.Errorf("Load(nil) cache records = %+v, want one record id 42", next.Records)
	}
	if rec, ok := idx.Lookup(42); !ok || rec.TvdbID != 100 || rec.SeasonKind != SeasonAbsent {
		t.Errorf("Load(nil) index Lookup(42) = %+v ok=%v, want the fetched record, its season unstated", rec, ok)
	}
}

func TestLoader_Load_overrideWinsOverUpstream(t *testing.T) {
	overrides := filepath.Join(t.TempDir(), "overrides.json")
	if err := os.WriteFile(overrides, []byte(`[{"anilist_id":1,"tmdb_movies":[42]}]`), 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	_, idx, err := unused(WithOverridesPath(overrides), WithRefresh(time.Hour)).Load(t.Context(), freshCache())
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	rec, ok := idx.Lookup(1)
	if !ok || rec.TvdbID != 0 || !slices.Equal(rec.TmdbMovies, []int{42}) {
		t.Errorf("override not applied: got %+v ok=%v, want the override alone, TmdbMovies [42]", rec, ok)
	}
}

func TestLoader_Load_missingAndMalformedOverridesIgnored(t *testing.T) {
	dir := t.TempDir()
	_, idx, err := unused(WithOverridesPath(filepath.Join(dir, "nope.json")), WithRefresh(time.Hour)).Load(t.Context(), freshCache())
	if err != nil {
		t.Fatalf("Load with missing overrides error: %v", err)
	}
	if rec, ok := idx.Lookup(1); !ok || rec.TvdbID != 100 {
		t.Errorf("missing overrides changed the mapping record: %+v ok=%v", rec, ok)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{ not valid`), 0o644); err != nil {
		t.Fatalf("write bad overrides: %v", err)
	}
	if _, _, err := unused(WithOverridesPath(bad), WithRefresh(time.Hour)).Load(t.Context(), freshCache()); err != nil {
		t.Fatalf("Load with malformed overrides returned error, want ignored: %v", err)
	}
}

// TestLoader_refreshCache_noCacheAvailableErrors covers the first-boot
// degradation branches: a refused document, one with no entries and a
// missing release each return a no-cache error rather than a success.
func TestLoader_refreshCache_noCacheAvailableErrors(t *testing.T) {
	for name, serve := range map[string]func(*release){
		"invalid document": func(r *release) { r.publish(testTag, []byte(`{"version":2}`)) },
		"no entries":       func(r *release) { r.publish(testTag, encode(t, document(nil, nil))) },
		"no release": func(r *release) {
			r.latest = func(w http.ResponseWriter, req *http.Request) { http.NotFound(w, req) }
		},
	} {
		t.Run(name, func(t *testing.T) {
			rel := newRelease(t, testTag, nil)
			rel.edit(serve)
			next, err := rel.loader(WithRefresh(time.Hour)).refreshCache(t.Context(), &Cache{})
			if err == nil {
				t.Fatal("refresh with no prior cache returned nil error, want a no-cache-available error")
			}
			if _, ok := errors.AsType[*StaleMapError](err); ok || len(next.Records) != 0 {
				t.Errorf("refresh with no prior cache = %d records, %v, want none and the no-cache error", len(next.Records), err)
			}
		})
	}
}

func TestLoader_Load_degradedRefreshStillAppliesOverrides(t *testing.T) {
	rel := newRelease(t, testTag, []byte(`{}`))
	overrides := filepath.Join(t.TempDir(), "overrides.json")
	if err := os.WriteFile(overrides, []byte(`[{"anilist_id":1,"tmdb_movies":[42]}]`), 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	_, idx, err := rel.loader(WithOverridesPath(overrides), WithRefresh(time.Hour)).Load(t.Context(), staleCache(""))
	if err == nil {
		t.Fatal("Load with a refused refresh returned nil error, want a degraded error")
	}
	rec, ok := idx.Lookup(1)
	if !ok || rec.TvdbID != 0 || !slices.Equal(rec.TmdbMovies, []int{42}) {
		t.Errorf("degraded Load did not overlay overrides: got %+v ok=%v, want the override alone, TmdbMovies [42]", rec, ok)
	}
}

// TestLoader_Load_noOverridesPathServesUpstreamUnmodified pins
// effectiveOverrides' empty-path early return.
func TestLoader_Load_noOverridesPathServesUpstreamUnmodified(t *testing.T) {
	_, idx, err := unused(WithRefresh(time.Hour)).Load(t.Context(), freshCache())
	if err != nil {
		t.Fatalf("Load with no overrides path error: %v", err)
	}
	if rec, ok := idx.Lookup(1); idx.Len() != 1 || !ok || rec.TvdbID != 100 {
		t.Errorf("Load with no overrides path record = %+v ok=%v of %d, want unmodified TVDB 100", rec, ok, idx.Len())
	}
}

// TestLoader_refreshCache_futureFetchedAtForcesFetch pins the clock-skew guard
// in the fresh-reuse condition (age >= 0): a cache stamped in the future is
// never treated as fresh.
func TestLoader_refreshCache_futureFetchedAtForcesFetch(t *testing.T) {
	rel := newRelease(t, testTag, entriesBody(t, entry{AniList: 42, Type: "tv", TVDB: 100}))
	prev := staleCache("")
	prev.FetchedAt = time.Now().Add(2 * time.Hour)
	next, err := rel.loader(WithRefresh(time.Hour)).refreshCache(t.Context(), prev)
	if err != nil {
		t.Fatalf("refreshCache with future FetchedAt error: %v", err)
	}
	if len(next.Records) != 1 || next.Records[0].AniListID != 42 {
		t.Fatalf("future-FetchedAt cache was reused as fresh: records = %+v, want fetched record id 42", next.Records)
	}
}

// TestLoader_refreshCache_futureFetchedAtFailedFetchClampsStaleAge pins the
// stale-age clamp on the degradation telemetry when a future FetchedAt forces
// a check that then fails.
func TestLoader_refreshCache_futureFetchedAtFailedFetchClampsStaleAge(t *testing.T) {
	prev := staleCache("")
	prev.FetchedAt = time.Now().Add(2 * time.Hour)
	l := NewLoader(&http.Client{Transport: errTransport{}}, mapread.Source{Repo: DefaultRepo}, WithRefresh(time.Hour), WithLogger(discardLogger()))
	next, err := l.refreshCache(t.Context(), prev)
	if len(next.Records) != 1 {
		t.Errorf("future-FetchedAt failed refresh records = %+v, want stale record kept", next.Records)
	}
	stale, ok := errors.AsType[*StaleMapError](err)
	if !ok {
		t.Fatalf("future-FetchedAt failed refresh error = %v, want *StaleMapError", err)
	}
	if strings.Contains(stale.Error(), "fetched -") {
		t.Errorf("StaleMapError text = %q, want non-negative age", stale.Error())
	}
	attrs := stale.LogAttrs()
	foundAge := false
	for i := 0; i+1 < len(attrs); i += 2 {
		if attrs[i] == "stale_age_seconds" {
			foundAge = true
			if secs, isFloat := attrs[i+1].(float64); !isFloat || secs < 0 {
				t.Errorf("LogAttrs stale_age_seconds = %v, want non-negative float64", attrs[i+1])
			}
		}
	}
	if !foundAge {
		t.Error("LogAttrs carries no stale_age_seconds attribute; the clamp assertion never ran")
	}
}

// TestLoader_refreshCache_zeroRefreshAlwaysChecks pins the deployed
// configuration's contract (DefaultRefresh = 0): even a just-fetched cache
// asks which release is the latest every cycle.
func TestLoader_refreshCache_zeroRefreshAlwaysChecks(t *testing.T) {
	rel := newRelease(t, testTag, entriesBody(t, seriesEntries(1)...))
	prev := staleCache(testTag)
	prev.FetchedAt = time.Now()
	next, err := rel.loader(WithRefresh(0)).refreshCache(t.Context(), prev)
	if err != nil {
		t.Fatalf("zero-refresh check error: %v", err)
	}
	if got := len(rel.paths()); got != 1 || len(next.Records) != 1 {
		t.Errorf("zero-refresh loader made %d requests and kept %d records, want 1 and 1", got, len(next.Records))
	}
}

// TestLoader_refreshCache_unusableCacheFetchFailureErrors pins the
// cache-usability gate on the outage path: a cache whose records index to
// nothing degrades like no cache at all, never as a StaleMapError, so the
// scout preserves findings instead of comparing against an empty map.
func TestLoader_refreshCache_unusableCacheFetchFailureErrors(t *testing.T) {
	rel := newRelease(t, testTag, nil)
	rel.edit(func(r *release) {
		r.latest = func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", http.StatusBadGateway) }
	})
	prev := &Cache{FetchedAt: time.Now().Add(-2 * time.Hour), Records: []Record{{}}}
	_, err := rel.loader(WithRefresh(time.Hour)).refreshCache(t.Context(), prev)
	if err == nil {
		t.Fatal("fetch failure over an unusable cache returned nil error, want a no-cache-available error")
	}
	if _, ok := errors.AsType[*StaleMapError](err); ok {
		t.Fatalf("fetch failure over an unusable cache returned %v, want the no-cache error", err)
	}
}

// TestLoader_refreshCache_freshUnusableCacheStillFetches pins the
// cache-usability gate on the fresh-reuse fast path: a cache inside the
// refresh window that serves no map is not reused.
func TestLoader_refreshCache_freshUnusableCacheStillFetches(t *testing.T) {
	for name, records := range map[string][]Record{
		"a zero AniList id":                   {{}},
		"a zero-id record carrying the TVDB":  {{AniListID: 1}, {AniListID: 0, TvdbID: 100}},
		"keyed but with no arr identifier":    {{AniListID: 1}},
		"only negative AniList ids, mappable": {{AniListID: -1, TvdbID: 100}, {AniListID: -2, TvdbID: 200}},
	} {
		t.Run(name, func(t *testing.T) {
			if cacheUsable(records) {
				t.Fatalf("cacheUsable(%+v) = true, want false", records)
			}
			rel := newRelease(t, testTag, entriesBody(t, entry{AniList: 42, Type: "tv", TVDB: 100}))
			next, err := rel.loader(WithRefresh(time.Hour)).refreshCache(t.Context(), &Cache{FetchedAt: time.Now(), Records: records})
			if err != nil {
				t.Fatalf("refreshCache with a fresh-but-unusable cache error: %v", err)
			}
			if len(next.Records) != 1 || next.Records[0].AniListID != 42 {
				t.Fatalf("fresh-but-unusable cache was reused as fresh: records = %+v, want fetched record id 42", next.Records)
			}
		})
	}
}
