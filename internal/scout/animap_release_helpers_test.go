package scout

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cplieger/animap/mapread"
	"github.com/cplieger/seadex-scout/internal/mapping"
)

const animapTestTag = "v2026.10.17"

// seriesDocument is an animap.json whose entries are AniList ids, each all
// of an anime of the same AniDB id on TVDB series id*100, its one episode
// with nothing stated.
func seriesDocument(t *testing.T, ids ...int) []byte {
	t.Helper()
	doc := &mapread.Document{
		Version: mapread.Version, GeneratedAt: "2026-10-17T00:00:00Z",
		Sources:     mapread.Sources{Mapping: mapread.MappingSource{Revision: "r"}, AniDB: mapread.AniDBSource{TitlesDump: "2026-10-17"}},
		Attribution: mapread.Attribution{License: "ODbL-1.0", LicenseURL: "u", ContentsLicense: "DbCL-1.0", ContentsLicenseURL: "u", Notice: "n"},
		Anime:       []mapread.Anime{},
		Entries:     []mapread.Entry{},
	}
	for _, id := range ids {
		doc.Anime = append(doc.Anime, mapread.Anime{
			AniDBID: id, Type: "TV", TVDBID: id * 100, Last: mapread.Last{Regular: 1},
			Episodes: []mapread.Run{{Kind: mapread.Regular, Start: 1, End: 1}},
		})
		doc.Entries = append(doc.Entries, mapread.Entry{AniListID: id, Type: "TV", Parts: []mapread.Part{{AniDBID: id, Kind: mapread.Regular}}})
	}
	b, err := mapread.Encode(doc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// animapRelease serves body as the latest animap release of a GitHub-shaped
// TLS host, or answers releases/latest with latestStatus when it is not 0,
// and returns a client and source that reach it.
func animapRelease(t *testing.T, latestStatus int, body []byte) (*http.Client, mapread.Source) {
	t.Helper()
	sum := sha256.Sum256(body)
	prefix := "/" + mapping.DefaultRepo + "/releases/"
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case prefix + "latest":
			if latestStatus != 0 {
				http.Error(w, "boom", latestStatus)
				return
			}
			w.Header().Set("Location", srv.URL+prefix+"tag/"+animapTestTag)
			w.WriteHeader(http.StatusFound)
		case prefix + "download/" + animapTestTag + "/animap.json.sha256":
			_, _ = w.Write([]byte(hex.EncodeToString(sum[:]) + "  animap.json\n"))
		case prefix + "download/" + animapTestTag + "/animap.json":
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.Client(), mapread.Source{Repo: mapping.DefaultRepo, Base: srv.URL}
}

// noSource is the source of a loader whose client never reaches a network.
var noSource = mapread.Source{Repo: mapping.DefaultRepo}
