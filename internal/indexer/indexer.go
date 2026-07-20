// Package indexer serves a Torznab feed of SeaDex releases for Sonarr/Radarr.
//
// It answers two request kinds two different ways:
//
//   - A SEARCH (the arr's automatic/interactive search, which carries a query)
//     is proxied to Prowlarr's per-indexer Torznab endpoint for that tracker and
//     filtered to the releases SeaDex curates (matched by info hash / tracker id
//     against a cached SeaDex set), passing their real title/seeders/size/
//     download URL straight through - so a search rides Prowlarr's own tracker
//     parse and credentials, and needs no passkey here.
//
//   - A periodic RSS check (an empty-query "latest releases" fetch, which Sonarr
//     and Radarr issue on their sync interval) is answered from a synthesized
//     per-tracker JOURNAL of newly curated releases, built by the compare cycle
//     from its SeaDex fetch and persisted as a snapshot this server reads (see
//     FeedWriter): a release enters when it is new to SeaDex's curation set
//     (never seen before - the tracker post date is deliberately not the
//     trigger) and ages out after 14 days. Its title is synthesized from the
//     show's own arr/AniList title plus the release's real flags - upgraded to
//     the tracker's real title once the writer's Prowlarr title harvest matches
//     it - its size summed from the files, and its download link built
//     directly: a public Nyaa .torrent, or AnimeBytes via the operator's
//     passkey. A journal is the only sane RSS shape here: an empty-query proxy
//     would return the tracker's newest uploads (not what SeaDex curates), and
//     re-broadcasting the whole catalogue would make every poll a firehose.
//
// Every item - search or RSS - carries the SeaDex download-volume-factor marker:
// best -> 0.75 (Freeleech25), alt -> 0.25 (Freeleech75), which the operator maps
// to a Custom Format on their anime profile. The AnimeBytes RSS link embeds the
// operator's passkey, so it is a secret; the endpoint is apikey-gated and meant
// to bind LAN-only. The curation set and the two synthesized feeds are produced
// together by the compare cycle (one SeaDex fetch feeds both the findings and
// the feed), persisted atomically (see FeedWriter), and reloaded by the server
// when the snapshot file changes - the server never fetches SeaDex itself.
//
// The feed is served per-tracker only, addressable by path or by subdomain:
// /nyaa (or a nyaa.* host) serves the Nyaa-sourced curated releases, /ab (or an
// ab.* host) the AnimeBytes ones, and any other path or host is 404 - there is
// no combined feed. Adding the two per-tracker feeds as separate indexers in
// Prowlarr/Sonarr/Radarr lets each carry its own sync profile and gate that
// tracker's RSS/automatic/interactive use independently - the arr is the only
// component that knows the search type (it is never carried in the Torznab
// request), so it owns that policy. The subdomain form lets a reverse proxy map
// per-tracker hostnames to the one port without rewriting paths, for when
// seadex-scout runs apart from the arrs.
package indexer

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/cplieger/webhttp"
)

const (
	// dvfBest / dvfAlt are the download-volume-factor markers: 0.75 -> Freeleech25
	// (SeaDex best), 0.25 -> Freeleech75 (SeaDex alt).
	dvfBest = "0.75"
	dvfAlt  = "0.25"

	// upstreamNyaa / upstreamAB name the two proxied Prowlarr indexers. They
	// double as the per-tracker path segments the feed serves (see scopeFromPath)
	// and as the scope values upstreamForScope matches on.
	upstreamNyaa = "nyaa"
	upstreamAB   = "ab"
)

// UpstreamConfig is the Prowlarr upstream wiring shared by the feed server
// (search proxying) and the feed writer (title harvesting) - the one home for
// the per-tracker vocabulary both halves configure identically. ProwlarrAPIKey
// and ABPasskey are secrets and are never logged. An empty Nyaa/AnimeBytes URL
// disables that upstream. ABPasskey is the operator's AnimeBytes passkey,
// appended to synthesized AB RSS download links (search links go through
// Prowlarr and need no passkey); empty leaves the AB RSS feed without grabbable
// links.
type UpstreamConfig struct {
	NyaaTorznabURL string
	ABTorznabURL   string
	ProwlarrAPIKey string
	ABPasskey      string
}

// Config is the indexer server's runtime settings: the embedded shared
// upstream wiring plus APIKey, the feed's own gate (a secret, never logged).
type Config struct {
	APIKey string
	UpstreamConfig
}

// Deps are the clients the indexer server needs: an HTTP client for the Prowlarr
// per-indexer Torznab endpoints a search proxies. HTTP must be non-nil when any
// Torznab URL is configured for the server (New wires the upstreams
// unconditionally, and a search through a nil client panics); only NewFeedWriter
// accepts a nil HTTP, treating it as harvest-disabled. The curation set and the
// synthesized RSS feeds are not built here - the compare cycle builds and
// persists them (see FeedWriter) and the server reads that snapshot - so the
// server needs no SeaDex or Fribb client of its own.
type Deps struct {
	HTTP   *http.Client
	Logger *slog.Logger
}

// Indexer serves searches by proxying Prowlarr filtered to SeaDex's curation,
// and periodic RSS checks from the two synthesized per-tracker feeds. Both come
// from snap, the materialized feed the compare cycle builds and persists (see
// FeedWriter); the server loads it on start and reloads it when the file changes
// (a cycle - in this process or the `poll` subcommand - rewrote it), reading it
// under mu. The server never fetches SeaDex or Fribb itself.
type Indexer struct {
	snapMod time.Time
	// snapInfo is the os.FileInfo of the successfully loaded snapshot file,
	// installed together with snap/snapMod (guarded by mu). The last-good skip
	// needs identity (os.SameFile), not just mtime: an atomic rename or a
	// backup restore can install a DIFFERENT file that preserves the loaded
	// timestamp, and that replacement must install, not be skipped.
	snapInfo os.FileInfo
	// failedFile identifies (mtime + os.SameFile identity) the last snapshot
	// file whose CONTENT failed to decode (malformed JSON), so an unchanged bad
	// file is not re-read and re-warned on every request; cleared on a
	// successful load. Only deterministic content failures are memoized: a
	// read failure (EIO, EACCES) can recover without changing inode or mtime
	// (a chmod, a transient filesystem repair), so it stays retryable.
	// Identity matters, not just mtime: an atomic rename or backup restore can
	// install a repaired file that preserves the failed file's timestamp, and
	// that replacement must be retried, not skipped. Guarded by reloadMu
	// (set/cleared only inside reload).
	failedFile os.FileInfo
	// log, cfg, and path are set once in New and read per request without a
	// lock: cfg is a by-value copy and none of the three is ever written
	// after construction (the same immutable-after-New contract as
	// upstreams and verifyKey below).
	log  *slog.Logger
	cfg  Config
	path string
	snap snapshot
	// authFailLast is the failed-auth token bucket's last-refill instant, part
	// of the bucket guarded by authFailMu below (see allowAuthFailure).
	authFailLast time.Time
	upstreams    []*upstream // wired once in New; immutable afterwards (not guarded by mu)
	// authFailTokens is the failed-auth token bucket's fill level, guarded by
	// authFailMu below together with authFailLast above (see allowAuthFailure).
	authFailTokens float64
	// mu guards the published snapshot fields read per request: snap,
	// snapMod, snapInfo, snapFailed, and snapFailedWarned (see the
	// per-field comments).
	mu sync.RWMutex
	// reloadMu coalesces concurrent snapshot refreshes: only one request runs
	// reload's stat/read/unmarshal at a time; the rest serve the current
	// immutable snapshot (see reload). It also guards the reload-only flags
	// failedFile / snapMissing / reloadDegraded.
	reloadMu sync.Mutex
	// authFailMu guards the failed-auth token bucket (authFailTokens above /
	// authFailLast at the top of the struct), which rate-limits responses to
	// bad apikey attempts (see allowAuthFailure).
	authFailMu sync.Mutex
	// verifyKey is the pre-hashed feed_api_key verifier, built once in New so
	// per-request verification hashes only the presented value (see
	// webhttp.NewStaticTokenVerifier). Immutable after New.
	verifyKey webhttp.StaticTokenVerifier
	// snapMissing records that the snapshot file disappeared AFTER one was
	// loaded (deleted file, incomplete restore, lost volume), so the
	// stale-feed WARN fires once per disappearance instead of on every
	// request; cleared (with one INFO recovery line) on the first successful
	// stat afterward. A fresh install with no prior snapshot stays silent.
	// Guarded by reloadMu (set/cleared only inside reload).
	snapMissing bool
	// reloadDegraded records that reloads are failing (a stat error or a
	// read failure of an unchanged-identity file), so the WARN fires once
	// per degradation onset instead of on every request; cleared with one
	// INFO recovery line on the next successful snapshot read, and cleared
	// SILENTLY when the file goes absent (statSnapshot's ENOENT arm - the
	// missing state has its own once-per-disappearance WARN) or when the
	// stat lands on the memoized malformed file (skipMemoizedMalformed -
	// access recovered, but nothing was reloaded). The retry itself is NOT
	// suppressed (both faults can recover without an mtime change). Guarded
	// by reloadMu (set/cleared only inside reload).
	reloadDegraded bool
	// snapFailed records that snapshot loading failed BEFORE any snapshot was
	// installed: a non-ENOENT stat or read fault, or a malformed or
	// structurally invalid file, at startup leaves the zero-value in-memory
	// snapshot indistinguishable from the intentional fresh-install state -
	// query would contact Prowlarr, filter every result against nil curation
	// maps, and serve a successful empty feed, so the arr records a clean
	// no-match during a local fault. While set, query answers with a
	// snapshot-unavailable flag (no Prowlarr query) and serve renders a
	// Torznab <error>, like an unavailable Prowlarr dependency. Set on those
	// failure paths only while snapInfo is nil (a fault AFTER a successful
	// load keeps serving the last-good snapshot); cleared by the first
	// successful installSnapshot, and by a genuinely absent file (deleting
	// the bad file returns to fresh-install semantics). Guarded by mu (read
	// per request by query, unlike the reloadMu-guarded flags above).
	snapFailed bool
	// snapFailedWarned bounds the snapshot-unavailable WARN to one per onset
	// instead of one per request; re-armed whenever snapFailed clears.
	// Guarded by mu.
	snapFailedWarned bool
}

// New builds the Torznab feed server from cfg and deps, wiring one upstream per
// configured Prowlarr Torznab URL. snapshotPath is where the compare cycle
// persists the materialized feed (config.DefaultIndexerFeedPath in production);
// it is loaded now so a restart serves the last feed immediately, and reloaded
// on change while running. An empty path serves an empty feed (used in tests).
func New(cfg *Config, deps Deps, snapshotPath string) *Indexer {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	ix := &Indexer{
		log:       log,
		path:      snapshotPath,
		cfg:       *cfg,
		verifyKey: webhttp.NewStaticTokenVerifier(cfg.APIKey),
	}
	// One upstream per configured Prowlarr Torznab URL. An empty URL means that
	// tracker is off: it is simply not wired, so the feed never queries it. (The
	// daemon only starts the feed at all when at least one URL is set.)
	ix.upstreams = wireUpstreams(deps.HTTP, log, cfg.UpstreamConfig)
	// Warm the feed from the last persisted snapshot so a restart serves
	// immediately rather than empty until the next cycle.
	ix.reload(context.Background())
	return ix
}

// wireUpstreams builds one upstream per configured Prowlarr per-indexer
// Torznab URL, shared by the server (search proxying) and the feed writer
// (title harvesting) so both query the exact tracker set the operator
// configured, with the same client, headers, and retry discipline.
func wireUpstreams(httpClient *http.Client, log *slog.Logger, cfg UpstreamConfig) []*upstream {
	var ups []*upstream
	if cfg.NyaaTorznabURL != "" {
		ups = append(ups, &upstream{http: httpClient, log: log, name: upstreamNyaa, feed: cfg.NyaaTorznabURL, apiKey: cfg.ProwlarrAPIKey})
	}
	if cfg.ABTorznabURL != "" {
		ups = append(ups, &upstream{http: httpClient, log: log, name: upstreamAB, feed: cfg.ABTorznabURL, apiKey: cfg.ProwlarrAPIKey})
	}
	return ups
}
