// Package indexer serves a Torznab feed of SeaDex releases for Sonarr/Radarr.
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

	// upstreamNyaa / upstreamAB name the two proxied Prowlarr indexers. They double
	// as the per-tracker path segments the feed serves and as the scope values.
	upstreamNyaa = "nyaa"
	upstreamAB   = "ab"
)

// feedScopes is the closed, ordered set of tracker scopes the feed serves. It is
// the ONE enumeration: every site that iterates the scopes, tests membership or
// keys a per-scope map reads it, so adding SeaDex's third tracker is one entry
// here plus one arm in each per-scope FIELD switch.
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
// (search proxying) and the feed writer (title harvesting). ProwlarrAPIKey and
// ABPasskey are secrets and are never logged. An empty Nyaa/AnimeBytes URL
// disables that upstream. ABPasskey is appended to synthesized AB RSS download
// links (search links go through Prowlarr); empty DROPS the AB RSS feed entirely
// and makes a configured /ab feed answer with a Torznab <error> naming the
// missing passkey, so Prowlarr's save-test fails with a reason.
type UpstreamConfig struct {
	NyaaTorznabURL string
	ABTorznabURL   string
	ProwlarrAPIKey string
	ABPasskey      string
}

// torznabURL returns the Prowlarr per-indexer Torznab URL configured for scope,
// or "" when that tracker is off (an unknown scope is off). It is the scope ->
// config-field half of the per-tracker vocabulary whose other half is
// match.go's trackerScope, so a third tracker is one case rather than five
// switches, and a site missed there fails asymmetrically.
func (c UpstreamConfig) torznabURL(scope string) string {
	switch scope {
	case upstreamNyaa:
		return c.NyaaTorznabURL
	case upstreamAB:
		return c.ABTorznabURL
	}
	return ""
}

// enabled reports whether scope may be wired, queried or served: an empty
// per-tracker Torznab URL is the documented off switch, and an unknown scope is
// never enabled.
func (c UpstreamConfig) enabled(scope string) bool {
	return c.torznabURL(scope) != ""
}

// enablementOnly returns the ENABLEMENT half of c for a process-lifetime field:
// the per-tracker off switches plus the AB passkey gate, with ProwlarrAPIKey
// dropped. Both constructors hold their operator input through this, so neither
// retains the Prowlarr credential (that field is REACHABILITY, consumed only
// inside the wired upstreams). Stated as an EXCLUSION rather than a list of
// fields to keep, so a third tracker's URL is carried by both halves
// automatically instead of being silently omitted from one inclusion list.
func (c UpstreamConfig) enablementOnly() UpstreamConfig {
	c.ProwlarrAPIKey = ""
	return c
}

// Config is the indexer server's runtime settings: the embedded shared upstream
// wiring, APIKey (the feed's own gate - a secret, never logged), and
// SnapshotPath, where the compare cycle persists the materialized feed.
type Config struct {
	APIKey       string
	SnapshotPath string
	UpstreamConfig
}

// wireUpstreams builds one upstream per configured Prowlarr per-indexer Torznab
// URL, for the server (search proxying) or the feed writer (title harvesting), so
// both query the exact tracker set the operator configured with the same client,
// headers and retry discipline. It is called by New and NewFeedWriter from the
// SAME UpstreamConfig those constructors read their enablement from, which is
// what binds enablement (which trackers may be served) and reachability (where
// they are queried) to one operator input.
func wireUpstreams(client *http.Client, log *slog.Logger, cfg UpstreamConfig) []*upstream {
	if client == nil {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	// One upstream per configured Torznab URL. An empty URL means that tracker is
	// off, so it is simply not wired and the feed never queries it.
	var ups []*upstream
	for _, scope := range feedScopes {
		if !cfg.enabled(scope) {
			continue
		}
		ups = append(ups, newUpstream(client, log, scope, cfg.torznabURL(scope), cfg.ProwlarrAPIKey))
	}
	return ups
}

// Indexer serves searches by proxying Prowlarr filtered to SeaDex's curation, and
// periodic RSS checks from the two synthesized per-tracker feeds. Both come from
// the snapshot the compare cycle builds, owned by cache, which takes it either
// straight from a cycle in this process or from the persisted file on its own
// reload clock. Run starts that clock (New is pure assembly and loads nothing);
// nothing on the request path loads, and the server never fetches SeaDex.
type Indexer struct {
	// cache owns the served snapshot's lifecycle and its locking (see
	// snapshotCache). The serving path reaches it through three methods only, so
	// no request names a lock primitive or a reload-only flag.
	cache *snapshotCache
	// log is set once in New and read per request without a lock, like enablement
	// and keyUnusable below: none is ever written after construction.
	log *slog.Logger
	// The field order below is govet fieldalignment's: pointer-only fields lead,
	// then the fields whose trailing words carry no pointer, and finally the
	// pointer-free values, which have no alignment requirement to pay for.
	noUpstreamWarned map[string]*atomic.Bool
	// enablement is the per-tracker off switch (a non-empty Torznab URL) plus the AB
	// passkey gate the request path reads - the same narrowing FeedWriter applies, so
	// the server retains no ProwlarrAPIKey.
	enablement UpstreamConfig
	// upstreams is wired once in New; immutable afterwards.
	upstreams []*upstream
	// verifyKey is the pre-hashed feed_api_key verifier, built once in New so
	// per-request verification hashes only the presented value. Immutable.
	verifyKey webhttp.StaticTokenVerifier
	// keyUnusable is the precomputed answer to the only question the serving path
	// asks about indexer.feed_api_key - is the gate usable at all - decided once in
	// New. The plaintext key is deliberately NOT retained: verification goes
	// through verifyKey, which holds only a digest. Immutable after New.
	keyUnusable bool
}

// New builds the Torznab feed server from cfg, log, and the HTTP client its
// Prowlarr search proxy dials with. It is pure assembly and starts no work: the
// snapshot is loaded by Run, so all background work begins under the explicit
// lifecycle method. A nil log falls back to slog.Default(); a nil client serves
// the snapshot without proxying searches; a nil cfg panics. The upstreams are
// wired HERE, from cfg's own UpstreamConfig, so the server's enablement gates and
// its reachability can never describe different operator input.
func New(cfg *Config, log *slog.Logger, client *http.Client) *Indexer {
	if log == nil {
		log = slog.Default()
	}
	ix := &Indexer{
		log:              log,
		enablement:       cfg.enablementOnly(),
		verifyKey:        webhttp.NewStaticTokenVerifier(cfg.APIKey),
		keyUnusable:      unusableFeedKey(cfg.APIKey),
		cache:            newSnapshotCache(cfg.SnapshotPath, cfg.ABPasskey, log),
		upstreams:        wireUpstreams(client, log, cfg.UpstreamConfig),
		noUpstreamWarned: newScopeLatches(),
	}
	return ix
}
