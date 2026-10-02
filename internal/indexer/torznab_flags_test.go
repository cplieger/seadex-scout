package indexer

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/seadex-scout/internal/seadex"
)

// arrIndexerFlags derives the indexer flags Sonarr and Radarr record for one
// Torznab item, by the arrs' own rules (TorznabRssParser.GetFlags): the download
// volume factor names at most one leech flag, an upload factor of 2 adds
// DoubleUpload, and each tag attr of internal or scene adds that flag.
func arrIndexerFlags(it *servarrItem) []string {
	var flags []string
	for _, dvf := range it.attrs("downloadvolumefactor") {
		switch dvf {
		case "0":
			flags = append(flags, "Freeleech")
		case "0.5":
			flags = append(flags, "Halfleech")
		case "0.75":
			flags = append(flags, "Freeleech25")
		case "0.25":
			flags = append(flags, "Freeleech75")
		}
	}
	if slices.Contains(it.attrs("uploadvolumefactor"), "2") {
		flags = append(flags, "DoubleUpload")
	}
	for _, tag := range it.attrs("tag") {
		switch strings.ToLower(tag) {
		case "internal":
			flags = append(flags, "Internal")
		case "scene":
			flags = append(flags, "Scene")
		}
	}
	slices.Sort(flags)
	return flags
}

// markerIndexer serves one curated Nyaa release, best or alt, on both render
// paths: the synthesized RSS feed and a proxied Prowlarr search.
func markerIndexer(t *testing.T, isBest bool) *Indexer {
	t.Helper()
	entries := []seadex.Entry{{
		AniListID: 123,
		Torrents: []seadex.Torrent{{
			Tracker: "Nyaa", URL: "https://nyaa.si/view/1234567", InfoHash: "ABCDEF1234567890abcdef1234567890abcdef12", IsBest: isBest,
			Files: []seadex.File{{Length: 100, Name: "Some Anime - S01E01 (1080p) [PMR].mkv"}},
		}},
	}}
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	info := func(int) EntryInfo { return EntryInfo{Title: "Some Anime", Target: TargetSonarr} }
	if err := newTestWriter(path, "", false).Rebuild(t.Context(), entries, info); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, strings.ReplaceAll(sampleFeed, "http://prowlarr:9696", "http://"+r.Host))
	}))
	t.Cleanup(srv.Close)
	return warmedIndexer(&Config{APIKey: "k", SnapshotPath: path, NyaaTorznabURL: srv.URL, ProwlarrAPIKey: "pk"}, nil, srv.Client())
}

// TestServedItemsCarryTheMarkerPair decodes served items with the arrs' flag
// rules and pins the exact flag set each one records: a SeaDex pick carries its
// tier flag plus Scene, on RSS and on a proxied search alike, and an unmarked
// item or the first-start placeholder carries nothing, so no stray Internal or
// DoubleUpload ever rides along.
func TestServedItemsCarryTheMarkerPair(t *testing.T) {
	const search = "t=tvsearch&q=Some+Anime"
	tests := []struct {
		desc  string
		build func(t *testing.T) *Indexer
		query string
		want  []string
	}{
		{desc: "best on RSS", build: func(t *testing.T) *Indexer { return markerIndexer(t, true) }, want: []string{"Freeleech25", "Scene"}},
		{desc: "alt on RSS", build: func(t *testing.T) *Indexer { return markerIndexer(t, false) }, want: []string{"Freeleech75", "Scene"}},
		{desc: "best on a proxied search", build: func(t *testing.T) *Indexer { return markerIndexer(t, true) }, query: search, want: []string{"Freeleech25", "Scene"}},
		{desc: "alt on a proxied search", build: func(t *testing.T) *Indexer { return markerIndexer(t, false) }, query: search, want: []string{"Freeleech75", "Scene"}},
		{desc: "unmarked RSS item", build: func(t *testing.T) *Indexer {
			ix := freshNyaaIndexer(t, &Config{}, nil)
			seedJournal(t, ix, upstreamNyaa, realItem("42", catAnime))
			return ix
		}},
		{desc: "first-start placeholder", build: func(t *testing.T) *Indexer { return freshNyaaIndexer(t, &Config{}, nil) }},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			feed := serveFeed(t, tc.build(t), "/nyaa", tc.query)
			if len(feed.Items) != 1 {
				t.Fatalf("served %d items, want 1, so the flag assertion cannot pass vacuously", len(feed.Items))
			}
			if got := arrIndexerFlags(&feed.Items[0]); !slices.Equal(got, tc.want) {
				t.Errorf("indexer flags of %q = %v, want exactly %v", feed.Items[0].Title, got, tc.want)
			}
		})
	}
}
