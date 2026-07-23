package classify

import (
	"testing"

	"github.com/cplieger/seadex-scout/internal/seadex"
)

// TestFileResolution pins the shared per-torrent resolution classifier the
// indexer's RSS title synthesis consumes: resolution comes from the file
// names alone over the PayloadNames eligibility rule (a small sidecar-sized
// extra with a different resolution marker does not vote), an empty or
// nameless file list yields no resolution rather than a fabricated one, and
// marker-less names classify to the empty string.
func TestFileResolution(t *testing.T) {
	tests := []struct {
		name  string
		files []seadex.File
		want  string
	}{
		{"empty file list", nil, ""},
		{"nameless files only", []seadex.File{{Length: 100}}, ""},
		{"resolution from the primary payload", []seadex.File{
			{Name: "Show - 01 [1080p][HEVC].mkv", Length: 1_400_000_000},
		}, "1080p"},
		// The excluded file is listed FIRST in both eligibility cases:
		// release.Classify keeps the first observed resolution, so a
		// regression that lets every raw name vote would classify from the
		// excluded file and fail the case.
		{"small extra's resolution does not vote", []seadex.File{
			{Name: "Special - 01 [480p].mkv", Length: 200_000_000},
			{Name: "Show - 01 [1080p].mkv", Length: 1_400_000_000},
		}, "1080p"},
		{"creditless extra excluded by the type gate", []seadex.File{
			{Name: "Show - NCED01 [1080p].mkv", Length: 1000},
			{Name: "Show - 01 [720p].mkv", Length: 1000},
		}, "720p"},
		{"no resolution marker classifies empty", []seadex.File{
			{Name: "Show - 01.mkv", Length: 1000},
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FileResolution(tt.files); got != tt.want {
				t.Errorf("FileResolution(%+v) = %q, want %q", tt.files, got, tt.want)
			}
		})
	}
}
