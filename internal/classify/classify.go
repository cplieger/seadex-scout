// Package classify houses the shared SeaDex-to-release classification glue: the
// single construction of a release.Release from a seadex.Torrent (in the
// context of its entry) that both the compare (findings) and audit (report)
// flows depend on. Keeping it in one place means the two flows classify an
// identical SeaDex release identically and cannot silently diverge if the
// release.Input contract gains a field. It is a seadex-aware adapter so the
// release package can stay a pure, seadex-free leaf.
package classify

import (
	"github.com/cplieger/seadex-scout/internal/filter"
	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// ABVisible reports whether a SeaDex torrent may surface under the operator's
// AnimeBytes toggle. It owns the raw-URL invariant shared by compare and audit:
// the guard inspects the RAW upstream URL (t.URL), never t.UsableURL(), because
// that normalization trusts the tracker label and would rewrite or erase the
// very host evidence the cross-check needs. Obtainability re-checks the label
// downstream as defense in depth.
func ABVisible(t *seadex.Torrent, includeAnimeBytes bool) bool {
	return filter.ABVisible(t.Tracker, t.URL, includeAnimeBytes)
}

// Torrent classifies one SeaDex torrent, in the context of its entry (for the
// shared notes), into a normalized release.Release. This is the one place the
// release.Input for a SeaDex torrent is built, so compare and audit classify
// the same release identically.
func Torrent(entry *seadex.Entry, t *seadex.Torrent) release.Release {
	return release.Classify(&release.Input{
		Names:     torrentFileNames(t.Files),
		Notes:     entry.Notes,
		Group:     t.ReleaseGroup,
		Tracker:   t.Tracker,
		DualAudio: t.DualAudio,
	})
}

// torrentFileNames returns the non-empty file names of a SeaDex torrent, the
// name list the classifier parses.
func torrentFileNames(files []seadex.File) []string {
	names := make([]string, 0, len(files))
	for i := range files {
		if files[i].Name != "" {
			names = append(names, files[i].Name)
		}
	}
	return names
}
