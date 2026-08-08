// Package seadex is the releases.moe (SeaDex) vocabulary: the value objects
// and contract predicates every consumer of the SeaDex model reads.
//
// SeaDex curates the best available release per anime, keyed by AniList ID.
// This package is the STABLE half of what used to be one package: the model
// here changes with this app's comparison rules, while the volatile
// releases.moe PocketBase wire client that produces it (internal/seadexapi)
// changes with the upstream API. The model lives in this pure leaf (stdlib
// strings/time only) so the seven packages that consume only the vocabulary do
// not reach it through the client's httpx/jsonx closure; only the cycle
// orchestrator and the composition root depend on the client.
package seadex

import (
	"strings"
	"time"
)

// File is one file inside a SeaDex torrent (its name and byte length).
type File struct {
	Name   string `json:"name"`
	Length int64  `json:"length"`
}

// Torrent is a single release SeaDex tracks for an entry.
type Torrent struct {
	ReleaseGroup string   `json:"releaseGroup"`
	Tracker      string   `json:"tracker"`
	InfoHash     string   `json:"infoHash"`
	URL          string   `json:"url"`
	Files        []File   `json:"files"`
	Tags         []string `json:"tags"`
	IsBest       bool     `json:"isBest"`
	DualAudio    bool     `json:"dualAudio"`
}

// ValidInfoHash returns h lowercased when it is a 40-char SHA-1 hex info hash,
// else "" (covers the releases.moe "<redacted>" placeholder and any other
// junk value).
func ValidInfoHash(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if len(h) != 40 {
		return ""
	}
	for i := range len(h) {
		c := h[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ""
		}
	}
	return h
}

// Entry is a SeaDex entry: one anime (by AniList ID) and its tracked releases.
type Entry struct {
	Updated         time.Time
	Notes           string
	TheoreticalBest string
	Torrents        []Torrent
	AniListID       int
	Incomplete      bool
}

// HasTheoreticalBest reports whether the entry names a theoretical-best release
// that is not yet muxed (nothing concrete to grab). Like the package's other
// predicates over untrusted PocketBase text, surrounding whitespace is not a
// name: a whitespace-only value reports false.
func (e *Entry) HasTheoreticalBest() bool { return strings.TrimSpace(e.TheoreticalBest) != "" }
