package mapping

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/slogx/capture"
)

// listServer serves the fixture body (or whatever handler the case supplies)
// and records whether the request carried conditional validators.
type listServer struct {
	*httptest.Server
	sawValidators atomic.Bool
	requests      atomic.Int32
}

func newListServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *listServer {
	t.Helper()
	ls := &listServer{}
	ls.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ls.requests.Add(1)
		if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
			ls.sawValidators.Store(true)
		}
		handler(w, r)
	}))
	t.Cleanup(ls.Close)
	return ls
}

// fixtureHandler answers a 200 with the fixture body and both validators.
func fixtureHandler(t *testing.T) func(w http.ResponseWriter, r *http.Request) {
	t.Helper()
	body, err := os.ReadFile("testdata/anime-list-fixture.xml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"list-v2"`)
		w.Header().Set("Last-Modified", "Tue, 02 Sep 2026 00:00:00 GMT")
		_, _ = w.Write(body)
	}
}

// populatedListCache is a Cache holding an already-accepted list with validators.
func populatedListCache() *Cache {
	return &Cache{
		FetchedAt:            time.Now(),
		Records:              []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
		Mappings:             map[int]Mapping{5: {SpecialEpisode: 3}},
		MappingsETag:         `"list-v1"`,
		MappingsLastModified: "Mon, 01 Sep 2026 00:00:00 GMT",
		MappingsFetchedAt:    time.Now().Add(-time.Hour),
	}
}

// TestListLoader_refresh_200PopulatesTheMapAndValidators pins the accept path:
// a 200 replaces the map with the body's mappings, records both validators and
// stamps the timestamp.
func TestListLoader_refresh_200PopulatesTheMapAndValidators(t *testing.T) {
	ts := newListServer(t, fixtureHandler(t))
	next := &Cache{Records: []Record{{AniListID: 1, Type: "TV", TvdbID: 100}}}
	before := time.Now()
	NewListLoader(ts.Client(), ts.URL, discardLogger()).refresh(t.Context(), next)
	if got := next.Mappings[12276].SpecialEpisode; got != 8 {
		t.Errorf("Mappings[12276].SpecialEpisode = %d, want 8 (the fixture's No Game No Life Zero)", got)
	}
	if len(next.Mappings[69].Seasons) != 23 {
		t.Errorf("Mappings[69].Seasons = %d ranges, want 23", len(next.Mappings[69].Seasons))
	}
	if next.MappingsETag != `"list-v2"` || next.MappingsLastModified != "Tue, 02 Sep 2026 00:00:00 GMT" {
		t.Errorf("validators = (%q, %q), want the 200's", next.MappingsETag, next.MappingsLastModified)
	}
	if next.MappingsFetchedAt.Before(before) {
		t.Errorf("MappingsFetchedAt = %v, want stamped at or after %v", next.MappingsFetchedAt, before)
	}
	if len(next.Records) != 1 || next.ETag != "" {
		t.Errorf("Fribb fields touched: Records=%d ETag=%q, want untouched", len(next.Records), next.ETag)
	}
}

// TestListLoader_refresh_heldValidatorsWithAnEmptyMapAreNotSent pins the guard
// that keeps a 304 from affirming a map nothing holds. The state is reachable:
// a 200 whose body carries <anime> nodes but no non-empty Mapping stores both
// validators beside an empty map. From there the refresh must ask
// unconditionally and re-download the list in full, so a server that would
// answer 304 for the held ETag never gets the chance to leave the map empty.
func TestListLoader_refresh_heldValidatorsWithAnEmptyMapAreNotSent(t *testing.T) {
	serve := fixtureHandler(t)
	ts := newListServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"list-v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		serve(w, r)
	})
	next := populatedListCache()
	next.Mappings = map[int]Mapping{}
	NewListLoader(ts.Client(), ts.URL, discardLogger()).refresh(t.Context(), next)
	if ts.sawValidators.Load() {
		t.Error("an empty map sent validators; a 304 then affirms a map nothing holds")
	}
	if got := next.Mappings[12276].SpecialEpisode; got != 8 {
		t.Errorf("Mappings[12276].SpecialEpisode = %d, want 8 (the list re-downloaded in full)", got)
	}
	if next.MappingsETag != `"list-v2"` {
		t.Errorf("MappingsETag = %q, want the 200's", next.MappingsETag)
	}
}

// TestListLoader_refresh_304KeepsTheMapAndBumpsOnlyTheTimestamp pins the
// revalidate path: the request carries the held validators, and a 304 leaves
// the map and validators as they were while moving the timestamp forward.
func TestListLoader_refresh_304KeepsTheMapAndBumpsOnlyTheTimestamp(t *testing.T) {
	ts := newListServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != `"list-v1"` {
			t.Errorf("If-None-Match = %q, want the held ETag", r.Header.Get("If-None-Match"))
		}
		w.WriteHeader(http.StatusNotModified)
	})
	prev := populatedListCache()
	next := *prev
	NewListLoader(ts.Client(), ts.URL, discardLogger()).refresh(t.Context(), &next)
	if !ts.sawValidators.Load() {
		t.Error("a populated map sent no validators, want the conditional GET")
	}
	if next.Mappings[5].SpecialEpisode != 3 || len(next.Mappings) != 1 {
		t.Errorf("Mappings after 304 = %+v, want the held map", next.Mappings)
	}
	if next.MappingsETag != prev.MappingsETag || next.MappingsLastModified != prev.MappingsLastModified {
		t.Errorf("validators after 304 = (%q, %q), want held (%q, %q)", next.MappingsETag, next.MappingsLastModified, prev.MappingsETag, prev.MappingsLastModified)
	}
	if !next.MappingsFetchedAt.After(prev.MappingsFetchedAt) {
		t.Errorf("MappingsFetchedAt after 304 = %v, want after %v", next.MappingsFetchedAt, prev.MappingsFetchedAt)
	}
}

// TestListLoader_refresh_failuresKeepEveryField pins stale-on-error over every
// failure class: a 500, a body the decoder refuses, a body over the size cap,
// and a body a bound refuses all leave the four list fields byte-identical and
// WARN with the stale list's context.
func TestListLoader_refresh_failuresKeepEveryField(t *testing.T) {
	tests := []struct {
		name    string
		handler func(w http.ResponseWriter, r *http.Request)
		wantMsg string
	}{
		{name: "server_error", handler: func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}, wantMsg: "mapping: mapping-list refresh failed"},
		{name: "malformed_body", handler: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`<anime-list><anime anidbid="1"`))
		}, wantMsg: "mapping: mapping-list parse failed"},
		{name: "no_nodes", handler: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`<anime-list/>`))
		}, wantMsg: "mapping: mapping-list parse failed"},
		{name: "over_cap_body", handler: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(make([]byte, maxListBytes+1))
		}, wantMsg: "mapping: mapping-list refresh failed"},
		{name: "bound_breach", handler: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`<!DOCTYPE anime-list><anime-list><anime anidbid="1"/></anime-list>`))
		}, wantMsg: "mapping: mapping-list parse failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := newListServer(t, tc.handler)
			prev := populatedListCache()
			next := *prev
			logger, rec := capture.New()
			NewListLoader(ts.Client(), ts.URL, logger).refresh(t.Context(), &next)
			if next.Mappings[5].SpecialEpisode != 3 || len(next.Mappings) != 1 {
				t.Errorf("Mappings = %+v, want the held map", next.Mappings)
			}
			if next.MappingsETag != prev.MappingsETag || next.MappingsLastModified != prev.MappingsLastModified || !next.MappingsFetchedAt.Equal(prev.MappingsFetchedAt) {
				t.Errorf("list fields = (%q, %q, %v), want held (%q, %q, %v)", next.MappingsETag, next.MappingsLastModified, next.MappingsFetchedAt,
					prev.MappingsETag, prev.MappingsLastModified, prev.MappingsFetchedAt)
			}
			if n := rec.CountLevel(slog.LevelWarn, tc.wantMsg); n != 1 {
				t.Errorf("WARN %q count = %d, want 1: %v", tc.wantMsg, n, rec.Messages())
			}
			if !rec.HasAttr(tc.wantMsg, "stale_mappings", "1") {
				t.Errorf("WARN attrs lack stale_mappings=1: %v", rec.Messages())
			}
		})
	}
}

// TestListLoader_refresh_cancelledContextIsSilent pins that a refresh cut short
// by shutdown keeps every field and does not WARN about the upstream.
func TestListLoader_refresh_cancelledContextIsSilent(t *testing.T) {
	ts := newListServer(t, fixtureHandler(t))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	next := populatedListCache()
	logger, rec := capture.New()
	NewListLoader(ts.Client(), ts.URL, logger).refresh(ctx, next)
	if len(next.Mappings) != 1 {
		t.Errorf("Mappings after a cancelled refresh = %+v, want the held map", next.Mappings)
	}
	if rec.CountLevel(slog.LevelWarn, "mapping-list") != 0 {
		t.Errorf("a cancelled refresh warned: %v", rec.Messages())
	}
}

// TestLoader_Load_attachesTheListWhateverTheFribbOutcome pins the composition:
// a Fribb 200 plus a list 200 in one Load yields an Index whose MappingFor
// answers from the fresh list, and a Fribb 5xx plus a list 200 still attaches
// the fresh list to the STALE index - the two upstreams degrade independently.
func TestLoader_Load_attachesTheListWhateverTheFribbOutcome(t *testing.T) {
	tests := []struct {
		name      string
		fribb     func(w http.ResponseWriter, r *http.Request)
		wantStale bool
	}{
		{name: "fribb_200", fribb: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`[{"anilist_id":1,"type":"movie","themoviedb_id":{"movie":[9]},"anidb_id":12276}]`))
		}},
		{name: "fribb_5xx", fribb: func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}, wantStale: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fribb := newListServer(t, tc.fribb)
			list := newListServer(t, fixtureHandler(t))
			prev := &Cache{
				FetchedAt: time.Now().Add(-2 * time.Hour),
				Records:   []Record{{AniListID: 1, Type: "MOVIE", TmdbMovies: []int{9}, AniDBID: 12276}},
			}
			l := NewLoader(fribb.Client(), fribb.URL, WithLogger(discardLogger()),
				WithMappingList(NewListLoader(list.Client(), list.URL, discardLogger())))
			next, idx, err := l.Load(t.Context(), prev)
			if (err != nil) != tc.wantStale {
				t.Fatalf("Load error = %v, want stale=%v", err, tc.wantStale)
			}
			rec, ok := idx.Lookup(1)
			if !ok {
				t.Fatal("Lookup(1) missing")
			}
			m, ok := idx.MappingFor(&rec)
			if !ok || m.SpecialEpisode != 8 {
				t.Errorf("MappingFor(1) = %+v ok=%v, want the fresh list's episode 8", m, ok)
			}
			if next.MappingsETag != `"list-v2"` {
				t.Errorf("Cache.MappingsETag = %q, want the list's validator persisted", next.MappingsETag)
			}
			if list.requests.Load() != 1 {
				t.Errorf("list requests = %d, want 1", list.requests.Load())
			}
		})
	}
}

// TestLoader_Load_withoutAListServesThePersistedMap pins the default: no
// ListLoader attached, and Load serves whatever Mappings the cache carries.
func TestLoader_Load_withoutAListServesThePersistedMap(t *testing.T) {
	ts := newListServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"anilist_id":1,"type":"tv","tvdb_id":100,"anidb_id":5}]`))
	})
	prev := populatedListCache()
	prev.FetchedAt = time.Now().Add(-2 * time.Hour)
	l := NewLoader(ts.Client(), ts.URL, WithLogger(discardLogger()))
	_, idx, err := l.Load(t.Context(), prev)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	rec, _ := idx.Lookup(1)
	if m, ok := idx.MappingFor(&rec); !ok || m.SpecialEpisode != 3 {
		t.Errorf("MappingFor(1) = %+v ok=%v, want the persisted map's episode 3", m, ok)
	}
}
