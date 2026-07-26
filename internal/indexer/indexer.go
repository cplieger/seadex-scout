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
// Prowlarr and need no passkey); empty DROPS the AB RSS feed entirely (no
// grabbable link can be derived, see rebuildABDownloadURLs) and makes a
// configured /ab feed answer an empty-query RSS check with a Torznab <error>
// naming the missing passkey (see rejectMissingABPasskey), so Prowlarr's
// save-test fails with a reason instead of saving a link-less feed.
type UpstreamConfig struct {
	NyaaTorznabURL string
	ABTorznabURL   string
	ProwlarrAPIKey string
	ABPasskey      string
}

// Config is the indexer server's runtime settings: the embedded shared
// upstream wiring, APIKey (the feed's own gate - a secret, never logged), and
// SnapshotPath, where the compare cycle persists the materialized feed
// (config.DefaultIndexerFeedPath in production). SnapshotPath names the same
// file FeedWriterConfig.Path writes, the one contract binding the package's
// write half to its read half; it is loaded in New so a restart serves the last
// feed immediately, and reloaded on change while running. An empty SnapshotPath
// serves an empty feed (used in tests).
type Config struct {
	APIKey       string
	SnapshotPath string
	UpstreamConfig
}

// Upstreams is the wired set of Prowlarr per-indexer Torznab endpoints, built
// by WireUpstreams and handed to New (search proxying) or NewFeedWriter (title
// harvesting). It carries REACHABILITY - the HTTP client, the endpoint URLs, and
// the Prowlarr API key - and nothing else; which trackers are ENABLED stays
// Config/FeedWriterConfig policy (see UpstreamConfig), because the server must
// keep answering for a tracker whose snapshot it still holds.
//
// Taking the wired set rather than an *http.Client is what keeps outbound
// network policy at the composition root: this package never constructs a
// client, so it can never pick one whose redirect policy would forward the
// X-Api-Key header to another origin.
//
// The zero value is valid and wires nothing: RSS still serves from the
// persisted snapshot, a search whose scope has no wired upstream returns empty
// with a WARN (fetchRaw's standing-misconfiguration arm), and the title harvest
// is disabled (synthesized titles only). The curation set and the synthesized
// RSS feeds are not built here - the compare cycle builds and persists them
// (see FeedWriter) and the server reads that snapshot - so the server needs no
// SeaDex or Fribb client of its own.
type Upstreams struct {
	ups []*upstream
}

// WireUpstreams builds one upstream per configured Prowlarr per-indexer Torznab
// URL, for the server (search proxying) or the feed writer (title harvesting),
// so both query the exact tracker set the operator configured with the same
// client, headers, and retry discipline.
//
// The caller owns the client and therefore owns the outbound-network policy the
// Prowlarr API key depends on: the key rides an X-Api-Key header, which
// net/http forwards across redirects, so the client MUST carry a same-host,
// no-downgrade redirect policy (httpx.NewClient does). A nil client wires
// nothing. A nil log falls back to slog.Default().
//
// Call it once per consumer rather than sharing one set: an upstream's
// once-per-onset diagnostic latches are per-instance, and the server's request
// path and the writer's harvest must not suppress each other's WARNs.
func WireUpstreams(client *http.Client, log *slog.Logger, cfg UpstreamConfig) Upstreams {
	if client == nil {
		return Upstreams{}
	}
	if log == nil {
		log = slog.Default()
	}
	return Upstreams{ups: wireUpstreams(client, log, cfg)}
}

// Indexer serves searches by proxying Prowlarr filtered to SeaDex's curation,
// and periodic RSS checks from the two synthesized per-tracker feeds. Both come
// from snap, the materialized feed the compare cycle builds and persists (see
// FeedWriter); the server loads it on start and reloads it when the file changes
// (a cycle - in this process or the `poll` subcommand - rewrote it), reading it
// under mu. The server never fetches SeaDex or Fribb itself.
type Indexer struct {
	// cache owns the persisted-snapshot lifecycle and its two locking regimes
	// (see snapshotCache). The server reaches it through four methods only, so
	// nothing on the request path names a lock primitive or a reload-only flag.
	cache *snapshotCache
	// queryGate bounds the number of SIMULTANEOUS expensive requests. Auth,
	// caps, and the snapshot read are cheap; a SEARCH is not - each one holds
	// up to one bounded Prowlarr body per upstream (upstreamMaxBytes), the
	// encoding/xml token allocations to decode them, and up to
	// maxRenderedFeedBytes of render builder, for as long as the bounded retry
	// tree runs. net/http starts one goroutine per connection and the
	// bad-key throttle deliberately exempts a VALID key, so nothing else caps
	// how many of those run at once: a burst of correct-key searches (an arr's
	// library-wide search, a retry storm, a misconfigured client) could stack
	// that footprint until the 256 MiB container is OOM-killed - taking the
	// compare loop down with it. A buffered token channel rather than a
	// semaphore type for the same reason reloadGate is one: the wait must be
	// cancellable, so a request whose client has gone away abandons it instead
	// of parking a handler goroutine behind other requests' upstream calls.
	queryGate chan struct{}
	// log and cfg are set once in New and read per request without a lock: cfg
	// is a by-value copy and neither is ever written after construction (the
	// same immutable-after-New contract as upstreams and verifyKey below).
	log       *slog.Logger
	cfg       Config
	upstreams []*upstream // wired once in New; immutable afterwards
	// verifyKey is the pre-hashed feed_api_key verifier, built once in New so
	// per-request verification hashes only the presented value (see
	// webhttp.NewStaticTokenVerifier). Immutable after New.
	verifyKey webhttp.StaticTokenVerifier
}

// New builds the Torznab feed server from cfg, log, and the wired upstream set.
// The persisted feed snapshot named by cfg.SnapshotPath is loaded now so a
// restart serves the last feed immediately. A nil log falls back to
// slog.Default(); a zero ups serves the snapshot without proxying searches
// (see Upstreams).
func New(cfg *Config, log *slog.Logger, ups Upstreams) *Indexer {
	if log == nil {
		log = slog.Default()
	}
	ix := &Indexer{
		log:       log,
		cfg:       *cfg,
		verifyKey: webhttp.NewStaticTokenVerifier(cfg.APIKey),
		cache:     newSnapshotCache(cfg.SnapshotPath, cfg.ABPasskey, log),
		queryGate: make(chan struct{}, maxConcurrentQueries),
		upstreams: ups.ups,
	}
	// Warm the feed from the last persisted snapshot so a restart serves
	// immediately rather than empty until the next cycle.
	ix.cache.refresh(context.Background())
	return ix
}

// wireUpstreams is WireUpstreams' unexported builder: one upstream per
// configured Prowlarr per-indexer Torznab URL. An empty URL means that tracker
// is off, so it is simply not wired and the feed never queries it.
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
