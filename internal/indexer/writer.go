package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

const (
	feedDirMode = 0o755
	// feed.json can embed the AB passkey in synthesized AnimeBytes download
	// URLs, so it is owner-only. The daemon and the `poll` subcommand both run
	// as the same container user, so 0o600 stays read/write-compatible.
	feedFileMode = 0o600
	// maxFeedBytes bounds the persisted feed snapshot, enforced on write and
	// read alike so a rebuild can never persist a snapshot the server's reload
	// would then reject.
	maxFeedBytes = 64 << 20
)

// snapshot is the materialized feed a cycle produces and the server serves: the
// search curation index (info hash / tracker key -> isBest, matched against
// Prowlarr results) and the two synthesized per-tracker RSS feeds. Persisting it
// is what lets one data engine (the compare cycle) feed both the findings and
// the Torznab feed from a single SeaDex fetch, and lets a cycle run by the
// `poll` subcommand refresh a resident daemon's feed across the process
// boundary. Field names are the on-disk JSON keys.
type snapshot struct {
	ByHash   map[string]bool `json:"by_hash"`
	ByKey    map[string]bool `json:"by_key"`
	NyaaFeed []item          `json:"nyaa_feed"`
	ABFeed   []item          `json:"ab_feed"`
}

// FeedWriter builds the feed snapshot from a SeaDex fetch and persists it
// atomically for the server to read. It holds no clients of its own: the compare
// cycle owns the shared SeaDex fetch and Fribb load and hands their results to
// Rebuild, so the feed is produced from the very snapshot the findings use.
type FeedWriter struct {
	log          *slog.Logger
	path         string
	abPasskey    string
	abConfigured bool
}

// NewFeedWriter returns a FeedWriter that persists the feed snapshot to path
// (config.DefaultIndexerFeedPath in production). abPasskey builds the AnimeBytes
// RSS download links (empty leaves the AB feed without grabbable links).
// abConfigured signals AnimeBytes intent (an AB Torznab URL is set): it gates
// the missing-passkey nudge, so a Nyaa-only deployment is not warned every
// cycle about a tracker it opted out of. logger may be nil.
func NewFeedWriter(abPasskey string, abConfigured bool, path string, logger *slog.Logger) *FeedWriter {
	if logger == nil {
		logger = slog.Default()
	}
	return &FeedWriter{log: logger, path: path, abPasskey: abPasskey, abConfigured: abConfigured}
}

// Rebuild builds the curation set and the two per-tracker feeds from the SeaDex
// entries (categorized by isMovie, a closure over the caller's Fribb map) and
// writes the snapshot atomically. It is the single producer of the feed the
// server serves; isMovie may be nil (every item is then categorized as
// anime/series, the safe default). The caller skips a failed SeaDex fetch, so
// this only errors when the write itself fails.
func (w *FeedWriter) Rebuild(ctx context.Context, entries []seadex.Entry, isMovie func(alID int) bool) error {
	snap, abSkippedNoPasskey, unresolvable := buildSnapshot(entries, w.abPasskey, movieClassifier(isMovie))
	data, err := json.Marshal(&snap)
	if err != nil {
		return fmt.Errorf("indexer: encode feed snapshot: %w", err)
	}
	// Mirror the reader's size bound before committing: a snapshot the reload
	// would reject must not replace the last-good file, or the next restart
	// starts with an empty feed.
	if int64(len(data)) > maxFeedBytes {
		return fmt.Errorf("indexer: feed snapshot %d bytes exceeds max %d; keeping previous feed", len(data), maxFeedBytes)
	}
	if _, err := atomicfile.WriteFile(ctx, w.path, data,
		atomicfile.WithMkdirMode(feedDirMode), atomicfile.WithMode(feedFileMode)); err != nil {
		return fmt.Errorf("indexer: write feed snapshot %s: %w", w.path, err)
	}
	w.log.Info("indexer feed snapshot written",
		"entries", len(entries), "hashes", len(snap.ByHash), "keys", len(snap.ByKey),
		"nyaa_feed", len(snap.NyaaFeed), "ab_feed", len(snap.ABFeed),
		"skipped_unresolvable", unresolvable)
	if abSkippedNoPasskey > 0 && w.abConfigured {
		w.log.Warn("ab RSS feed empty of grabbable links: set indexer.ab_passkey to serve AnimeBytes releases",
			"ab_releases_skipped", abSkippedNoPasskey)
	}
	return nil
}

// buildSnapshot builds the search curation index and, via buildFeeds, the two
// synthesized feeds from the SeaDex catalogue. classify resolves each entry's Torznab
// category from its real media type (see movieClassifier). It returns the count
// of AnimeBytes releases skipped solely for a missing passkey so the caller can
// nudge the operator once, and the count of in-scope torrents dropped for an
// unresolvable download id so the snapshot log line surfaces silent feed shrink.
func buildSnapshot(entries []seadex.Entry, abPasskey string, classify func(alID int) []int) (snap snapshot, abSkippedNoPasskey, unresolvable int) {
	set := curation{byHash: make(map[string]bool), byKey: make(map[string]bool)}
	for i := range entries {
		for j := range entries[i].Torrents {
			t := &entries[i].Torrents[j]
			if h := validInfoHash(t.InfoHash); h != "" {
				set.byHash[h] = set.byHash[h] || t.IsBest
			}
			if k := trackerKey(t.Tracker, t.URL); k != "" {
				set.byKey[k] = set.byKey[k] || t.IsBest
			}
		}
	}
	nyaaFeed, abFeed, abSkippedNoPasskey, unresolvable := buildFeeds(entries, abPasskey, classify)
	return snapshot{ByHash: set.byHash, ByKey: set.byKey, NyaaFeed: nyaaFeed, ABFeed: abFeed}, abSkippedNoPasskey, unresolvable
}

// movieClassifier returns the category function buildFeeds stamps onto each
// entry's items. It routes a Fribb-typed movie to the Movies category (Radarr)
// and everything else - TV, OVA, ONA, SPECIAL, or an unmapped entry - to Anime
// (Sonarr). Defaulting the unknown/unmapped case to anime is deliberate: a
// single-file OVA/special looks just like a movie by file name, so the failure
// that matters (a special mis-routed to Radarr, where it can never match) is
// avoided at the cost of a rare unmapped film not surfacing on Radarr's RSS.
// isMovie is the caller's closure over its Fribb map; nil (no mapping) routes
// everything to anime.
func movieClassifier(isMovie func(alID int) bool) func(alID int) []int {
	return func(alID int) []int {
		if isMovie != nil && isMovie(alID) {
			return []int{catMovies}
		}
		return []int{catAnime}
	}
}
