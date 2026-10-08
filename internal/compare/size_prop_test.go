package compare

import (
	"fmt"
	"testing"

	"github.com/cplieger/seadex-scout/internal/payload"
	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/tracker"
	"pgregory.net/rapid"
)

// TestDownloadSetSingleEpisodeProperty pins the single-episode branch of the
// one-download-set rule over random pools of single-episode torrents: a known
// set sums its torrents' full sizes, holds only the headline's release family,
// and holds every candidate of that family.
func TestDownloadSetSingleEpisodeProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 10).Draw(t, "n")
		pool := make([]candidate, 0, n)
		for i := range n {
			group := rapid.SampledFrom([]string{"SubsPlease", "Erai-raws"}).Draw(t, "group")
			codec := rapid.SampledFrom([]string{"x264", "x265"}).Draw(t, "codec")
			episode := rapid.IntRange(1, 12).Draw(t, "episode")
			length := rapid.Int64Range(1, 1<<34).Draw(t, "length")
			pool = append(pool, candidate{
				rel: release.Release{Group: group, Tracker: "Nyaa", Resolution: "1080p", Codec: codec, TrackerType: tracker.Public},
				torrent: seadex.Torrent{
					ReleaseGroup: group, Tracker: "Nyaa", InfoHash: fmt.Sprintf("%040x", i+1),
					Files: []seadex.File{{Name: fmt.Sprintf("[%s] Show - S01E%02d (%s).mkv", group, episode, codec), Length: length}},
				},
			})
		}
		set, ok := downloadSet(pool)
		if !ok {
			return
		}
		downloads, total, ok := downloadsOf(set)
		if !ok {
			t.Fatalf("downloadsOf(%d torrents) refused a set of known sizes", len(set))
		}
		var sum int64
		for i := range set {
			sum += payload.TotalSize(set[i].torrent.Files)
		}
		if total != sum || len(downloads) != len(set) {
			t.Fatalf("total = %d over %d records, want %d over %d", total, len(downloads), sum, len(set))
		}
		h := representative(pool)
		want := releaseFamily(&h.rel)
		inFamily := 0
		for i := range pool {
			if releaseFamily(&pool[i].rel) == want {
				inFamily++
			}
		}
		for i := range set {
			if releaseFamily(&set[i].rel) != want {
				t.Fatalf("set member %d is outside the headline's family", i)
			}
		}
		if len(set) != inFamily {
			t.Fatalf("set holds %d torrents, the headline's family has %d", len(set), inFamily)
		}
	})
}
