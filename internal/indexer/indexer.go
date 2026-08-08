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
	"log/slog"
	"net/http"
	"slices"
	"sync/atomic"

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

// feedScopes is the closed, ordered set of tracker scopes the feed serves.
// It is the ONE enumeration: every site that iterates the scopes, tests
// membership, or keys a per-scope map reads it, so adding SeaDex's third
// tracker is one entry here plus one arm in each per-scope FIELD switch -
// never a search for hand-written pairs the compiler cannot cross-check.
var feedScopes = []string{upstreamNyaa, upstreamAB}

// validScope reports whether s is one of the feed's tracker scopes.
func validScope(s string) bool { return slices.Contains(feedScopes, s) }

// newScopeLatches builds one once-per-process WARN latch per feed scope.
func newScopeLatches() map[string]*atomic.Bool {
	m := make(map[string]*atomic.Bool, len(feedScopes))
	for _, scope := range feedScopes {
		m[scope] = new(atomic.Bool)
	}
	return m
}

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
// write half to its read half; it is warmed by Run so a restart serves the last
// feed immediately, and reloaded on change while running. An empty SnapshotPath
// serves an empty feed (used in tests).
type Config struct {
	APIKey       string
	SnapshotPath string
	UpstreamConfig
}

// wireUpstreams builds one upstream per configured Prowlarr per-indexer Torznab
// URL, for the server (search proxying) or the feed writer (title harvesting),
// so both query the exact tracker set the operator configured with the same
// client, headers, and retry discipline.
//
// It is called by New and NewFeedWriter from the SAME UpstreamConfig those
// constructors read their enablement policy from, which is what binds the two
// halves to one operator input. That pairing is the point: enablement (which
// trackers may be served) and reachability (where they are queried) are two
// readings of one operator input, and a wiring step the caller performed
// separately let a caller hand a constructor an enablement config and a wired
// set built from a DIFFERENT one - a server whose gates and wiring disagree,
// serving an /ab feed with no AB upstream, or wiring an upstream for a scope
// feedFor refuses (l-f246). The composition root's real constraint is only that
// it owns the *http.Client, which taking the client here preserves.
//
// The caller owns the client and therefore owns the outbound-network policy the
// Prowlarr API key depends on: the key rides an X-Api-Key header, which
// net/http forwards across redirects, so the client MUST carry a same-host,
// no-downgrade redirect policy (httpx.NewClient does). A nil client wires
// nothing: RSS still serves from the persisted snapshot, a search whose scope
// has no wired upstream returns empty with a WARN (fetchRaw's
// standing-misconfiguration arm), and the title harvest is disabled
// (synthesized titles only).
//
// Each call produces FRESH upstream instances, which is what makes the
// once-per-consumer diagnostic rule structural: an upstream's latches
// (dropWarned / displayWarned) are per-instance, so a server and a feed writer
// built from the same client can never suppress each other's once-per-onset
// WARNs.
func wireUpstreams(client *http.Client, log *slog.Logger, cfg UpstreamConfig) []*upstream {
	if client == nil {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	// One upstream per configured Prowlarr per-indexer Torznab URL. An empty URL
	// means that tracker is off, so it is simply not wired and the feed never
	// queries it.
	var ups []*upstream
	for _, scope := range feedScopes {
		if !cfg.enabled(scope) {
			continue
		}
		ups = append(ups, newUpstream(client, log, scope, cfg.torznabURL(scope), cfg.ProwlarrAPIKey))
	}
	return ups
}

// Indexer serves searches by proxying Prowlarr filtered to SeaDex's curation,
// and periodic RSS checks from the two synthesized per-tracker feeds. Both come
// from the persisted snapshot the compare cycle builds (see FeedWriter), owned
// by cache: Run warms it on start (cache.warm; New is pure assembly and loads
// nothing) and cache.refresh reloads it when the file changes (a cycle - in this
// process or the `poll` subcommand - rewrote it), under the cache's own locks.
// The server never fetches SeaDex or Fribb itself.
type Indexer struct {
	// cache owns the persisted-snapshot lifecycle - the initial warm load
	// included - and its two locking regimes (see snapshotCache). The server
	// reaches it through its six methods only, so nothing here holds snapshot
	// state and nothing on the request path names a lock primitive or a
	// reload-only flag.
	cache *snapshotCache
	// log is set once in New and read per request without a lock, like
	// enablement and keyUnusable below; none of them is ever written after
	// construction (the same immutable-after-New contract as upstreams and
	// verifyKey).
	log *slog.Logger
	// The field order below is govet fieldalignment's: the pointer-only fields
	// lead, then the string/slice/struct fields whose trailing words carry no
	// pointer, and finally the pointer-free values (verifyKey, keyUnusable) -
	// last because a byte-array-plus-bool and a bare bool have no alignment
	// requirement, so any 8-aligned field after them would pay for their
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
	// enablement is the per-tracker off switch (a non-empty Torznab URL) plus
	// the AB passkey gate the request path reads - the same narrowing
	// FeedWriter applies to the same input (writer.go), so the
	// process-lifetime server retains no ProwlarrAPIKey: that field is
	// reachability, consumed only inside the wired upstreams. It keeps the
	// UpstreamConfig type so the scope -> config-field vocabulary stays
	// single-homed (see UpstreamConfig.torznabURL); ProwlarrAPIKey is
	// deliberately left unset here.
	enablement UpstreamConfig
	// upstreams is wired once in New; immutable afterwards.
	upstreams []*upstream
	// verifyKey is the pre-hashed feed_api_key verifier, built once in New so
	// per-request verification hashes only the presented value (see
	// webhttp.NewStaticTokenVerifier). Immutable after New.
	verifyKey webhttp.StaticTokenVerifier
	// keyUnusable is the precomputed answer to the only question the serving
	// path asks about indexer.feed_api_key - is the gate usable at all
	// (unusableFeedKey) - decided once in New from the cfg value. The plaintext
	// key is deliberately NOT retained, the same process-lifetime hygiene the
	// enablement field applies to ProwlarrAPIKey: verification of a presented
	// value goes through verifyKey, which holds only a digest. Immutable after
	// New, so it needs no lock. Last field: a bare bool has no alignment
	// requirement, so it sits in the trailing pointer-free band with verifyKey.
	keyUnusable bool
}

// New builds the Torznab feed server from cfg, log, and the HTTP client its
// Prowlarr search proxy dials with. It is pure assembly and starts no work: the
// persisted feed snapshot named by cfg.SnapshotPath is warmed by Run
// (cache.warm), so all background work begins under the explicit lifecycle
// method. A nil log falls back to slog.Default(); a nil client serves the
// snapshot without proxying searches. cfg is the one argument with no nil
// tolerance - it is dereferenced here, so a nil cfg panics rather than yielding
// a defaulted server.
//
// The upstreams are wired HERE, from cfg's own UpstreamConfig (see
// wireUpstreams), so the server's enablement gates and its reachability can
// never describe different operator input. The client stays a parameter because
// this package must never construct one: outbound redirect policy for the
// X-Api-Key-bearing Prowlarr request belongs to the composition root.
func New(cfg *Config, log *slog.Logger, client *http.Client) *Indexer {
	if log == nil {
		log = slog.Default()
	}
	ix := &Indexer{
		log: log,
		enablement: UpstreamConfig{
			NyaaTorznabURL: cfg.NyaaTorznabURL,
			ABTorznabURL:   cfg.ABTorznabURL,
			ABPasskey:      cfg.ABPasskey,
		},
		verifyKey:        webhttp.NewStaticTokenVerifier(cfg.APIKey),
		keyUnusable:      unusableFeedKey(cfg.APIKey),
		cache:            newSnapshotCache(cfg.SnapshotPath, cfg.ABPasskey, log),
		upstreams:        wireUpstreams(client, log, cfg.UpstreamConfig),
		noUpstreamWarned: newScopeLatches(),
	}
	return ix
}
