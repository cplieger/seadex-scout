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
	"sync/atomic"
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

// torznabURL returns the Prowlarr per-indexer Torznab URL configured for scope,
// or "" when that tracker is off (an unknown scope is off). It is the scope ->
// config-field half of the per-tracker vocabulary whose other half is
// match.go's trackerScope: keeping it here is what stops wiring, feed serving,
// the AB passkey guard and the writer from each re-deciding which field a scope
// reads - a third tracker (SeaDex carries AnimeTosho and RuTracker torrents
// today) is then one case, not five switches, and a site missed there fails
// asymmetrically (a wired-but-never-served tracker, or the passkey-bearing /ab
// feed served for a tracker the operator turned off).
func (c UpstreamConfig) torznabURL(scope string) string {
	switch scope {
	case upstreamNyaa:
		return c.NyaaTorznabURL
	case upstreamAB:
		return c.ABTorznabURL
	}
	return ""
}

// enabled reports whether scope may be wired, queried, or served: an empty
// per-tracker Torznab URL is the README's documented off switch for that
// tracker, and an unknown scope is never enabled.
func (c UpstreamConfig) enabled(scope string) bool {
	return c.torznabURL(scope) != ""
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
// is disabled (synthesized titles only).
//
// ONE wired set per consumer is no longer the caller's job: New and
// NewFeedWriter each take their own copy of the upstream instances
// (ownUpstreams), so one wired set may be handed to both. Each upstream's
// once-per-onset diagnostic latches therefore stay per-consumer, and the
// server's request path can never suppress the writer's harvest WARNs.
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
// One wired set may be handed to more than one consumer (see Upstreams).
func WireUpstreams(client *http.Client, log *slog.Logger, cfg UpstreamConfig) Upstreams {
	if client == nil {
		return Upstreams{}
	}
	if log == nil {
		log = slog.Default()
	}
	// One upstream per configured Prowlarr per-indexer Torznab URL. An empty URL
	// means that tracker is off, so it is simply not wired and the feed never
	// queries it.
	var ups []*upstream
	for _, scope := range []string{upstreamNyaa, upstreamAB} {
		if !cfg.enabled(scope) {
			continue
		}
		ups = append(ups, &upstream{http: client, log: log, name: scope, feed: cfg.torznabURL(scope), apiKey: cfg.ProwlarrAPIKey})
	}
	return Upstreams{ups: ups}
}

// ownUpstreams returns this consumer's OWN upstream instances for the wired
// set, so Upstreams' once-per-consumer rule is structural rather than prose: an
// upstream's diagnostic latches (dropWarned / displayWarned) are per-instance,
// and handing one Upstreams value to both New and NewFeedWriter would otherwise
// let the server's request path and the writer's title harvest suppress each
// other's once-per-onset WARNs, silently. The client, endpoint, key, and logger
// are shared by value - reachability stays the caller's (see Upstreams) - and
// only the latch state is fresh. A whole-struct copy is not an option: the
// latches are sync/atomic values, which must not be copied.
func ownUpstreams(ups []*upstream) []*upstream {
	if len(ups) == 0 {
		return nil
	}
	own := make([]*upstream, 0, len(ups))
	for _, u := range ups {
		own = append(own, &upstream{http: u.http, log: u.log, name: u.name, feed: u.feed, apiKey: u.apiKey})
	}
	return own
}

// Indexer serves searches by proxying Prowlarr filtered to SeaDex's curation,
// and periodic RSS checks from the two synthesized per-tracker feeds. Both come
// from the persisted snapshot the compare cycle builds (see FeedWriter), owned
// by cache: Run warms it on start (see warmSnapshot; New is pure assembly and
// loads nothing) and cache.refresh reloads it when the file changes (a cycle -
// in this process or the `poll` subcommand - rewrote it), under the cache's own
// locks. The server never fetches SeaDex or Fribb itself.
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
	// feedGate bounds simultaneous synthesized-RSS renders (see
	// maxConcurrentFeeds). Separate from queryGate so a stalled Prowlarr, which
	// can park every search slot for the whole bounded retry budget, cannot
	// starve the RSS path - which serves the already-loaded snapshot and never
	// contacts an upstream.
	feedGate chan struct{}
	// log is set once in New and read per request without a lock, like apiKey
	// and enablement below; none of them is ever written after construction
	// (the same immutable-after-New contract as upstreams and verifyKey).
	log *slog.Logger
	// The field order below is govet fieldalignment's: the pointer-only fields
	// lead, then the string/slice/struct fields whose trailing words carry no
	// pointer, and finally the two pointer-free values (warmStarted,
	// verifyKey) - verifyKey last because it is a byte-array-plus-bool with no
	// alignment requirement, so any 8-aligned field after it would pay for its
	// padding.
	//
	// noUpstreamWarned bounds fetchRaw's standing-misconfiguration WARN to one
	// per scope per process. The condition is config-derived (a search reached
	// a scope with no wired Prowlarr upstream) and cannot clear without a
	// restart, so an arr left pointing at a turned-off tracker would otherwise
	// WARN once per search - the same per-query flood the upstream's
	// dropWarned/displayWarned latches exist to bound. Built in New and never
	// rewritten, so the map itself needs no lock.
	noUpstreamWarned map[string]*atomic.Bool
	// warmDone is closed when Run's warm snapshot load returns (see
	// warmSnapshot). Allocated in New so the request path can always read it;
	// warmStarted says whether anything will ever close it.
	warmDone chan struct{}
	// enablement is the per-tracker off switch (a non-empty Torznab URL) plus
	// the AB passkey gate the request path reads - the same narrowing
	// FeedWriter applies to the same input (writer.go), so the
	// process-lifetime server retains no ProwlarrAPIKey: that field is
	// reachability, consumed only inside the wired upstreams. It keeps the
	// UpstreamConfig type so the scope -> config-field vocabulary stays
	// single-homed (see UpstreamConfig.torznabURL); ProwlarrAPIKey is
	// deliberately left unset here.
	enablement UpstreamConfig
	// apiKey is the feed's own gate (a secret, never logged). The serving path
	// reads it only to answer "is a key configured at all"; verification of a
	// presented value goes through verifyKey.
	apiKey string
	// upstreams is wired once in New; immutable afterwards.
	upstreams []*upstream
	// warmStarted records that warmSnapshot ran, so warmPending can tell a
	// still-loading server from one that never started a warm load at all (a
	// New'd Indexer used without Run keeps the lazy per-request refresh).
	warmStarted atomic.Bool
	// verifyKey is the pre-hashed feed_api_key verifier, built once in New so
	// per-request verification hashes only the presented value (see
	// webhttp.NewStaticTokenVerifier). Immutable after New.
	verifyKey webhttp.StaticTokenVerifier
}

// warmLoadTimeout bounds how long Run WAITS for the warm load of the persisted
// snapshot - not the load itself. The read is size-bounded (maxFeedBytes) but a
// slow or wedged /config mount has no bound of its own, and Run starts on the
// daemon's startup path (main.go's startIndexer, alongside the compare loop), so
// an unbounded WAIT holds the whole daemon down instead of one request. A
// context deadline cannot deliver this bound: refresh stats the file before any
// ctx check, and atomicfile's bounded read only tests ctx around its syscalls -
// it cannot interrupt an os.Open, File.Stat, or io.ReadAll already blocked in
// the filesystem. So the load runs asynchronously and Run stops waiting after
// the deadline; the load may finish in the background, which is safe because the
// cache is synchronized and refresh coalesces through reloadGate, so whoever
// finishes installs and the first request either sees the warmed snapshot or
// reloads itself.
//
// A var, not a const, ONLY so the warm-load test can exercise the wait-expired
// path (see queryGateWait for the pattern) without spending it in real time.
var warmLoadTimeout = 15 * time.Second

// New builds the Torznab feed server from cfg, log, and the wired upstream set.
// It is pure assembly and starts no work: the persisted feed snapshot named by
// cfg.SnapshotPath is warmed by Run (see warmSnapshot), so all background work
// begins under the explicit lifecycle method. A nil log falls back to
// slog.Default(); a zero ups serves the snapshot without proxying searches
// (see Upstreams). cfg is the one argument with no nil tolerance - it is
// dereferenced here, so a nil cfg panics rather than yielding a defaulted
// server.
func New(cfg *Config, log *slog.Logger, ups Upstreams) *Indexer {
	if log == nil {
		log = slog.Default()
	}
	ix := &Indexer{
		log:    log,
		apiKey: cfg.APIKey,
		enablement: UpstreamConfig{
			NyaaTorznabURL: cfg.NyaaTorznabURL,
			ABTorznabURL:   cfg.ABTorznabURL,
			ABPasskey:      cfg.ABPasskey,
		},
		verifyKey: webhttp.NewStaticTokenVerifier(cfg.APIKey),
		cache:     newSnapshotCache(cfg.SnapshotPath, cfg.ABPasskey, log),
		queryGate: make(chan struct{}, maxConcurrentQueries),
		feedGate:  make(chan struct{}, maxConcurrentFeeds),
		warmDone:  make(chan struct{}),
		upstreams: ownUpstreams(ups.ups),
		noUpstreamWarned: map[string]*atomic.Bool{
			upstreamNyaa: new(atomic.Bool),
			upstreamAB:   new(atomic.Bool),
		},
	}
	return ix
}

// warmSnapshot warms the served feed from the last persisted snapshot so a
// restart serves immediately rather than empty until the next cycle. Run calls
// it before binding, so the work begins under the explicit lifecycle boundary
// rather than during construction. The load runs asynchronously and only the
// WAIT is bounded: a wedged /config mount cannot be interrupted mid-syscall, so
// bounding the wait is the only bound that holds (see warmLoadTimeout). A
// request arriving while the load is still running does not park behind it (see
// warmPending).
func (ix *Indexer) warmSnapshot() {
	ix.warmStarted.Store(true)
	go func() {
		defer close(ix.warmDone)
		ix.cache.refresh(context.Background())
	}()
	warmTimer := time.NewTimer(warmLoadTimeout)
	defer warmTimer.Stop()
	select {
	case <-ix.warmDone:
	case <-warmTimer.C:
		ix.log.Warn("feed snapshot warm load still running; serving requests without it",
			"timeout", warmLoadTimeout)
	}
}

// warmPending reports whether Run's warm load was started and has not finished.
// While that holds, the initial loader owns the cache's reload gate and a
// request that entered refresh would block on it for as long as the filesystem
// does - net/http's WriteTimeout cannot cancel a handler, so a wedged /config
// mount would pin every request slot. Requests answer the snapshot-unavailable
// fault instead until the loader returns. A New'd Indexer that never ran (direct
// query users and tests) has never started a warm load, so it keeps the lazy
// per-request refresh path.
func (ix *Indexer) warmPending() bool {
	if !ix.warmStarted.Load() {
		return false
	}
	select {
	case <-ix.warmDone:
		return false
	default:
		return true
	}
}
