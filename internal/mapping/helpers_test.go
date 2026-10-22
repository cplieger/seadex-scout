package mapping

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cplieger/animap/mapread"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func targets(ts ...mapread.Target) *[]mapread.Target {
	if ts == nil {
		ts = []mapread.Target{}
	}
	return &ts
}

func at(season, episode int) mapread.Target { return mapread.Target{Season: season, Episode: episode} }

func dflt(season, episode int) mapread.Target {
	return mapread.Target{Season: season, Episode: episode, Default: true}
}

// regular is regular episodes start to end landing on ts on TVDB; ts nil
// states nothing there.
func regular(start, end int, ts *[]mapread.Target) mapread.Run {
	return mapread.Run{Kind: mapread.Regular, Start: start, End: end, TVDB: ts}
}

// document wraps anime and entries in a valid version 2 document, each
// table sorted as the format requires.
func document(anime []mapread.Anime, entries []mapread.Entry) *mapread.Document {
	anime, entries = append([]mapread.Anime{}, anime...), append([]mapread.Entry{}, entries...)
	slices.SortFunc(anime, func(a, b mapread.Anime) int { return cmp.Compare(a.AniDBID, b.AniDBID) })
	slices.SortFunc(entries, func(a, b mapread.Entry) int { return cmp.Compare(a.AniListID, b.AniListID) })
	return &mapread.Document{
		Version:     mapread.Version,
		GeneratedAt: "2026-10-17T00:00:00Z",
		Sources:     mapread.Sources{Mapping: mapread.MappingSource{Revision: "r"}, AniDB: mapread.AniDBSource{TitlesDump: "2026-10-17"}},
		Attribution: mapread.Attribution{License: "ODbL-1.0", LicenseURL: "u", ContentsLicense: "DbCL-1.0", ContentsLicenseURL: "u", Notice: "n"},
		Anime:       anime,
		Entries:     entries,
	}
}

func earlierShape(t testing.TB, body []byte) []byte {
	t.Helper()
	const sources = `"sources":{"mapping":{"revision":"r"},"anidb":{"titles_dump":"2026-10-17"}}`
	sha, commit := strings.Repeat("0", 64), strings.Repeat("a", 40)
	earlier := `"sources":{"anime_offline_database":{"repository":"r","release":"2026-42","asset":"a","sha256":"` + sha + `"},` +
		`"anime_lists":{"repository":"r","commit":"` + commit + `","file":"f"},"anidb_mirror":{"repository":"r","commit":"` + commit + `"},` +
		`"overlay":{"sha256":"` + sha + `","entries":1}}`
	if !bytes.Contains(body, []byte(sources)) {
		t.Fatalf("body has no %s to rewrite", sources)
	}
	return bytes.Replace(body, []byte(sources), []byte(earlier), 1)
}

func encode(t testing.TB, doc *mapread.Document) []byte {
	t.Helper()
	b, err := mapread.Encode(doc)
	if err != nil {
		t.Fatalf("mapread.Encode: %v", err)
	}
	return b
}

// entry is one AniList id that is all the regular episodes of an anime of
// its own, written the way a Record reads it: TVDB is the anime's series, 0
// for none, and Lands where its one episode lands there, nil stating
// nothing; Movies are the TMDB movies that episode is, and IMDb the anime's.
type entry struct {
	Lands   *[]mapread.Target
	Type    string
	Movies  []int
	IMDb    []string
	AniList int
	TVDB    int
}

// entriesBody is a document holding one anime per entry, keyed by the
// entry's AniList id.
func entriesBody(t testing.TB, es ...entry) []byte {
	t.Helper()
	var anime []mapread.Anime
	var entries []mapread.Entry
	for _, e := range es {
		a := mapread.Anime{AniDBID: e.AniList, Type: strings.ToUpper(e.Type), TVDBID: e.TVDB, IMDbIDs: e.IMDb, Last: mapread.Last{Regular: 1}}
		if e.TVDB > 0 || e.Movies != nil {
			run := mapread.Run{Kind: mapread.Regular, Start: 1, End: 1, TMDBMovies: e.Movies}
			if e.TVDB > 0 {
				run.TVDB = e.Lands
			}
			a.Episodes = []mapread.Run{run}
		}
		anime = append(anime, a)
		entries = append(entries, mapread.Entry{AniListID: e.AniList, Type: strings.ToUpper(e.Type), Parts: []mapread.Part{{AniDBID: e.AniList, Kind: mapread.Regular}}})
	}
	return encode(t, document(anime, entries))
}

// seriesEntries is n entries, AniList 1 to n, each on its own TVDB series's
// season 1.
func seriesEntries(n int) []entry {
	out := make([]entry, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, entry{AniList: i, Type: "TV", TVDB: i, Lands: targets(at(1, 1))})
	}
	return out
}

// release is a GitHub-shaped release host over TLS: releases/latest answers
// a 302 to the current tag's page, and each tag serves animap.json and its
// sidecar. Tests swap the answers through the hooks.
type release struct {
	srv *httptest.Server
	// latest, when set, answers releases/latest instead.
	latest http.HandlerFunc
	// asset, when set, answers an asset request instead, returning false to
	// fall back to the stored assets.
	asset    func(w http.ResponseWriter, r *http.Request) bool
	sidecar  []byte
	body     []byte
	tag      string
	requests []string
	mu       sync.Mutex
}

const testTag = "v2026.10.17"

func newRelease(t *testing.T, tag string, body []byte) *release {
	t.Helper()
	r := &release{}
	r.publish(tag, body)
	r.srv = httptest.NewTLSServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

// set publishes body as tag, with its matching sidecar.
func (r *release) set(tag string, body []byte) { r.edit(func(r *release) { r.publish(tag, body) }) }

// publish is set for a caller already inside edit.
func (r *release) publish(tag string, body []byte) {
	sum := sha256.Sum256(body)
	r.tag, r.body, r.sidecar = tag, body, []byte(hex.EncodeToString(sum[:])+"  animap.json\n")
}

// edit changes the release's answers under its lock.
func (r *release) edit(fn func(r *release)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(r)
}

func (r *release) source() mapread.Source { return mapread.Source{Repo: DefaultRepo, Base: r.srv.URL} }

func (r *release) loader(opts ...Option) *Loader {
	return NewLoader(r.srv.Client(), r.source(), append([]Option{WithLogger(discardLogger())}, opts...)...)
}

// paths are the request paths served, in order.
func (r *release) paths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.requests)
}

// downloads counts the animap.json body requests served.
func (r *release) downloads() int {
	n := 0
	for _, p := range r.paths() {
		if strings.HasSuffix(p, "/animap.json") {
			n++
		}
	}
	return n
}

func (r *release) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.requests = append(r.requests, req.URL.Path)
	tag, body, sidecar, latest, asset := r.tag, r.body, r.sidecar, r.latest, r.asset
	r.mu.Unlock()
	prefix := "/" + DefaultRepo + "/releases/"
	switch p := req.URL.Path; {
	case p == prefix+"latest":
		if latest != nil {
			latest(w, req)
			return
		}
		w.Header().Set("Location", r.srv.URL+prefix+"tag/"+tag)
		w.WriteHeader(http.StatusFound)
	case asset != nil && asset(w, req):
	case p == prefix+"download/"+tag+"/animap.json.sha256":
		_, _ = w.Write(sidecar)
	case p == prefix+"download/"+tag+"/animap.json":
		_, _ = w.Write(body)
	default:
		http.NotFound(w, req)
	}
}

// assetPath is the request path of tag's animap.json, plus suffix.
func assetPath(tag, suffix string) string {
	return fmt.Sprintf("/%s/releases/download/%s/animap.json%s", DefaultRepo, tag, suffix)
}
