package arrwalk

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cplieger/arrapi/v3"
	"github.com/cplieger/arrapi/v3/sonarr"
	"github.com/cplieger/seadex-scout/internal/library"
)

// The versions and /api answers follow the recorded live Sonarr 4.0.20.3014 and
// Sonarr 5.0.0.2701; a Sonarr 5 still answers its deprecated /api/v3 view. The
// error answers Sonarr 4.0's exception handler writes, such as a list read naming
// a missing series, lack X-Application-Version (recorded live-v4/v3ef2188.headers).
// The expected walk outcomes follow sonarr.Pinned's contract: a pinned read
// returns only values from the pinned version, and a ServerChangedError with To
// set names another version, which ends the pass; one with To "" is that
// series' failure.
const (
	sonarr4Version = "4.0.20.3014"
	sonarr5Version = "5.0.0.2701"
)

const twoSeries = `[{"id":1,"title":"Code Geass","tvdbId":79525,"statistics":{"episodeFileCount":1}},` +
	`{"id":2,"title":"Monogatari","tvdbId":102261,"statistics":{"episodeFileCount":1}}]`

func serveSonarr4(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Application-Version", sonarr4Version)
	switch r.URL.Path {
	case "/api":
		_, _ = io.WriteString(w, `{"current":"v3","deprecated":[]}`)
	case "/api/v3/system/status":
		_, _ = io.WriteString(w, `{"appName":"Sonarr","version":"`+sonarr4Version+`"}`)
	case "/api/v3/series":
		_, _ = io.WriteString(w, twoSeries)
	case "/api/v3/episodefile":
		_, _ = io.WriteString(w, `[{"id":10,"seriesId":1,"seasonNumber":1,"releaseGroup":"PMR"}]`)
	case "/api/v3/episode":
		_, _ = io.WriteString(w, `[{"id":100,"seriesId":`+r.URL.Query().Get("seriesId")+`,"seasonNumber":1,"episodeNumber":1,"hasFile":true,"episodeFileId":10}]`)
	default:
		http.NotFound(w, r)
	}
}

func serveSonarr5(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Application-Version", sonarr5Version)
	switch {
	case r.URL.Path == "/api":
		_, _ = io.WriteString(w, `{"current":"v5","deprecated":["v3"]}`)
	case r.URL.Path == "/api/v5/system/status":
		_, _ = io.WriteString(w, `{"appName":"Sonarr","version":"`+sonarr5Version+`"}`)
	case r.URL.Path == "/api/v5/episodefile", r.URL.Path == "/api/v5/episode", strings.HasPrefix(r.URL.Path, "/api/v3/"):
		_, _ = io.WriteString(w, `[{"id":10,"seriesId":1,"seasonNumber":2,"releaseGroup":"FROM-SONARR-5"}]`)
	default:
		http.NotFound(w, r)
	}
}

// sonarrServer serves Sonarr 4.0, or Sonarr 5.0 while v5 is set. Answering a
// path in swaps flips the version once; a request URI in once gets that
// handler's answer the first time. It records every path it serves.
type sonarrServer struct {
	swaps map[string]bool
	once  map[string]http.HandlerFunc
	paths []string
	mu    sync.Mutex
	v5    bool
}

func (s *sonarrServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.paths = append(s.paths, r.URL.Path)
	h, scripted := s.once[r.URL.RequestURI()]
	delete(s.once, r.URL.RequestURI())
	v5 := s.v5
	if s.swaps[r.URL.Path] {
		delete(s.swaps, r.URL.Path)
		s.v5 = !s.v5
	}
	s.mu.Unlock()
	switch {
	case scripted:
		h(w, r)
	case v5:
		serveSonarr5(w, r)
	default:
		serveSonarr4(w, r)
	}
}

func (s *sonarrServer) served() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.paths)
}

func newSonarrServer(t *testing.T, s *sonarrServer) *sonarr.Client {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	c, err := sonarr.New(srv.URL, "test-key")
	if err != nil {
		t.Fatalf("sonarr.New: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

func (s *sonarrServer) wantNoEngineSwitch(t *testing.T) {
	t.Helper()
	for _, p := range s.served() {
		if p == "/api/v5/episodefile" || p == "/api/v5/episode" || p == "/api/v5/series" {
			t.Errorf("served %v: a read of the walk moved onto Sonarr v5 (API v5)", s.served())
			return
		}
	}
}

func wantVersionChangeRefused(t *testing.T, snap []library.Item, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("Walk published %d items read across a Sonarr version change, want an error", len(snap))
	}
	if got := WalkErrArr(err); got != ArrSonarr {
		t.Errorf("WalkErrArr = %q, want %q", got, ArrSonarr)
	}
	for _, v := range []string{sonarr4Version, sonarr5Version} {
		if !strings.Contains(err.Error(), v) {
			t.Errorf("error %q does not name version %s", err, v)
		}
	}
	if len(snap) != 0 {
		t.Errorf("snapshot holds %d items, want none", len(snap))
	}
}

func TestWalkRefusesASonarrUpgradedMidWalk(t *testing.T) {
	s := &sonarrServer{swaps: map[string]bool{"/api/v3/series": true}}
	c := newSonarrServer(t, s)

	snap, _, err := NewWalker(&Config{Sonarr: FromSonarr(c), Logger: discardLogger()}).Walk(t.Context())

	wantVersionChangeRefused(t, snap.Items, err)
	s.wantNoEngineSwitch(t)
}

// Sonarr 4.0, then 5.0 for the episode reads and the detection they lead, then
// 4.0 again: a check of the version at each end of the walk sees the same
// version twice, and the walk still publishes nothing.
func TestWalkRefusesAnABASwap(t *testing.T) {
	s := &sonarrServer{swaps: map[string]bool{"/api/v3/series": true, "/api/v5/system/status": true}}
	c := newSonarrServer(t, s)

	snap, _, err := NewWalker(&Config{Sonarr: FromSonarr(c), Logger: discardLogger()}).Walk(t.Context())

	wantVersionChangeRefused(t, snap.Items, err)
	s.wantNoEngineSwitch(t)
	info, infoErr := c.Info(t.Context())
	if infoErr != nil || info.Version != sonarr4Version {
		t.Fatalf("Info after the walk = (%+v, %v), want Sonarr %s again", info, infoErr, sonarr4Version)
	}
}

func TestWalkPublishesAnUnchangedSonarr(t *testing.T) {
	s := &sonarrServer{}
	c := newSonarrServer(t, s)

	snap, _, err := NewWalker(&Config{Sonarr: FromSonarr(c), Logger: discardLogger()}).Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(snap.Items) != 2 || snap.Partial {
		t.Errorf("items = %d, partial %v; want 2 and false", len(snap.Items), snap.Partial)
	}
	if got := s.served(); slices.ContainsFunc(got, func(p string) bool { return strings.HasPrefix(p, "/api/v5/") }) {
		t.Errorf("served %v, want no /api/v5 path from a Sonarr 4.0", got)
	}
}

func answer(code int, version string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if version != "" {
			w.Header().Set("X-Application-Version", version)
		}
		w.WriteHeader(code)
	}
}

// One transient failure anywhere in the walk is retried within arrapi's read
// budget, the walk's first detection request included, and the walk publishes
// as if it had not happened.
func TestWalkRetriesOneTransientSonarrFailure(t *testing.T) {
	for name, once := range map[string]map[string]http.HandlerFunc{
		"a 503 to the first /api":                   {"/api": answer(503, sonarr4Version)},
		"a header-less 503 to an episode-file read": {"/api/v3/episodefile?seriesId=1": answer(503, "")},
		"a header-less 503 to an episode read":      {"/api/v3/episode?seriesId=2": answer(503, "")},
	} {
		t.Run(name, func(t *testing.T) {
			s := &sonarrServer{once: once}
			c := newSonarrServer(t, s)

			snap, _, err := NewWalker(&Config{Sonarr: FromSonarr(c), Logger: discardLogger()}).Walk(t.Context())
			if err != nil {
				t.Fatalf("Walk: %v", err)
			}
			if len(snap.Items) != 2 || snap.Partial || snap.Items[0].Failed || snap.Items[1].Failed {
				t.Errorf("items = %+v, partial %v; want two unfailed items in a complete snapshot", snap.Items, snap.Partial)
			}
			if len(s.once) != 0 {
				t.Errorf("the scripted failure %v was never sent", s.once)
			}
		})
	}
}

// A series deleted during the walk answers Sonarr 4.0's header-less 404; it is
// that series' failure, and the reads sent after it still come from the pass.
func TestWalkKeepsASeriesDeletedMidWalkAsAFailedPlaceholder(t *testing.T) {
	deleted := make(chan struct{})
	s := &sonarrServer{once: map[string]http.HandlerFunc{
		"/api/v3/episodefile?seriesId=2": func(w http.ResponseWriter, r *http.Request) {
			answer(404, "")(w, r)
			close(deleted)
		},
		"/api/v3/episodefile?seriesId=1": func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-deleted:
			case <-r.Context().Done():
				return
			}
			serveSonarr4(w, r)
		},
	}}
	c := newSonarrServer(t, s)

	snap, _, err := NewWalker(&Config{Sonarr: FromSonarr(c), Logger: discardLogger()}).Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(snap.Items) != 2 || !snap.Partial {
		t.Fatalf("items = %d, partial %v; want 2 and true", len(snap.Items), snap.Partial)
	}
	for _, it := range snap.Items {
		if it.Failed != (it.TvdbID == 102261) {
			t.Errorf("item tvdb %d Failed = %v, want only the deleted series failed", it.TvdbID, it.Failed)
		}
		if !it.Failed && it.SeasonEpisodes == nil {
			t.Errorf("item tvdb %d has no episode reading: its episode read, sent after the 404, failed", it.TvdbID)
		}
	}
}

type pinFails struct{ err error }

func (p pinFails) Pin(context.Context) (SonarrPass, error) { return nil, p.err }

func TestWalkFailsWhenThePassCannotOpen(t *testing.T) {
	boom := errors.New("pin boom")
	_, _, err := NewWalker(&Config{Sonarr: pinFails{boom}, Logger: discardLogger()}).Walk(t.Context())
	if !errors.Is(err, boom) || WalkErrArr(err) != ArrSonarr {
		t.Fatalf("Walk error = %v, want the pin's error on the sonarr side", err)
	}
}

func TestWalkEndsOnAReadNamingAnotherVersion(t *testing.T) {
	changed := &arrapi.ServerChangedError{From: sonarr4Version, To: sonarr5Version}
	unconfirmed := &arrapi.ServerChangedError{From: sonarr4Version}
	series := []sonarr.Series{{ID: 1, Title: "Alpha"}, {ID: 2, Title: "Beta"}}
	files := map[int][]sonarr.EpisodeFile{1: {epFile(1, "PMR")}, 2: {epFile(1, "PMR")}}
	t.Run("on an episode-file read", func(t *testing.T) {
		fs := &fakeSonarr{series: series, files: files, epErr: map[int]error{1: changed}}
		snap, _, err := NewWalker(&Config{Sonarr: onePass{fs}, Logger: discardLogger()}).Walk(t.Context())
		wantVersionChangeRefused(t, snap.Items, err)
	})
	t.Run("on an episode read", func(t *testing.T) {
		fs := &fakeSonarr{series: series, files: files, episodesErr: map[int]error{2: changed}}
		snap, _, err := NewWalker(&Config{Sonarr: onePass{fs}, Logger: discardLogger()}).Walk(t.Context())
		wantVersionChangeRefused(t, snap.Items, err)
	})
	t.Run("an unconfirmed version is that series' failure", func(t *testing.T) {
		fs := &fakeSonarr{series: series, files: files, epErr: map[int]error{1: unconfirmed}}
		snap, _, err := NewWalker(&Config{Sonarr: onePass{fs}, Logger: discardLogger()}).Walk(t.Context())
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if len(snap.Items) != 2 || !snap.Partial || !snap.Items[0].Failed || snap.Items[1].Failed {
			t.Errorf("items = %+v, partial %v; want Alpha failed, Beta read, partial", snap.Items, snap.Partial)
		}
	})
}
