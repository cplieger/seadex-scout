package indexer

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cplieger/httpx/v2"
	"github.com/cplieger/seadex-scout/internal/appinfo"
)

const (
	// upstreamMaxAttempts / upstreamBaseDelay bound the per-query retry.
	upstreamMaxAttempts = 3
	upstreamBaseDelay   = time.Second
	// upstreamMaxBytes bounds a single Torznab response before decode.
	upstreamMaxBytes = 16 << 20
)

// upstream is one Prowlarr per-indexer Torznab endpoint (Nyaa or AnimeBytes).
// The feed proxies these to source real release data (title, seeders, size,
// Prowlarr-proxied download URL) and never talks to the trackers directly.
type upstream struct {
	http   *http.Client
	log    *slog.Logger
	name   string
	feed   string
	apiKey string
}

// search queries the Torznab endpoint with the forwarded params and returns the
// parsed items. The Prowlarr API key is sent as the X-Api-Key header (not a
// query param), so it never appears in a logged request URL.
func (u *upstream) search(ctx context.Context, params url.Values) ([]item, error) {
	reqURL := u.feed
	if enc := params.Encode(); enc != "" {
		if strings.Contains(reqURL, "?") {
			reqURL += "&" + enc
		} else {
			reqURL += "?" + enc
		}
	}

	body, err := httpx.Retry(ctx, u.http, reqURL,
		httpx.WithMaxAttempts(upstreamMaxAttempts),
		httpx.WithBaseDelay(upstreamBaseDelay),
		httpx.WithMaxBodyBytes(upstreamMaxBytes),
		httpx.WithHeaders(u.setHeaders),
		httpx.WithLogger(u.log),
	)
	if err != nil {
		return nil, err
	}
	items, err := parseTorznab(body)
	if err != nil {
		return nil, err
	}
	return u.filterDownloadURLs(items), nil
}

// filterDownloadURLs drops items whose download URL is not an absolute http(s)
// URL on the same origin as the configured Prowlarr Torznab endpoint. The
// curation lookup only proves an identifier is in the SeaDex snapshot; it does
// not bind the download target, so a tampered Prowlarr response could
// otherwise pair a curated id with an internal or attacker-controlled URL the
// arr then fetches as a curated release (SSRF / arbitrary download, CWE-918).
// A healthy Prowlarr hands out its own proxy links on the queried endpoint's
// origin, so same-origin is the safe default; the rejected URL itself is never
// logged.
func (u *upstream) filterDownloadURLs(items []item) []item {
	feedURL, err := url.Parse(u.feed)
	if err != nil {
		// An unparseable configured endpoint cannot anchor the origin check;
		// fail closed rather than passing unvalidated download targets through.
		u.log.Warn("upstream feed URL unparseable; dropping all items", "upstream", u.name)
		return nil
	}
	out := make([]item, 0, len(items))
	dropped := 0
	for i := range items {
		if !sameHTTPOrigin(items[i].DownloadURL, feedURL) {
			dropped++
			continue
		}
		// The two passthrough display-URL fields are not fetch targets, but
		// the arr renders <comments> as the item's clickable info link and a
		// URL that parses to no tracker key skips the curation gate entirely,
		// so a tampered upstream could attach a javascript:/data: or
		// foreign-host link to a legitimately curated item. Blank (never
		// drop) anything that is not an absolute http(s) URL: a healthy
		// Prowlarr always hands out absolute tracker page URLs here, and
		// curation matching is unaffected (a non-http URL could never
		// produce a tracker key).
		items[i].InfoURL = sanitizeDisplayURL(items[i].InfoURL)
		items[i].GUID = sanitizeDisplayURL(items[i].GUID)
		out = append(out, items[i])
	}
	if dropped > 0 {
		u.log.Warn("upstream items dropped: download URL not on the Prowlarr endpoint origin",
			"upstream", u.name, "dropped", dropped, "kept", len(out))
	}
	return out
}

// sameHTTPOrigin reports whether raw is an absolute http or https URL, free of
// userinfo, whose scheme and host (including port) match origin's.
func sameHTTPOrigin(raw string, origin *url.URL) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil {
		return false
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return false
	}
	return strings.EqualFold(scheme, origin.Scheme) && strings.EqualFold(parsed.Host, origin.Host)
}

// sanitizeDisplayURL returns raw when it is an absolute http(s) URL with a
// host, else "" - the item survives with the field blanked (writeItem omits an
// empty <comments> and item.guid() falls back to InfoHash/DownloadURL). Used
// on the passthrough display-URL fields (InfoURL, GUID) that neither the
// origin filter (fetch targets only) nor the curation gate (key-bearing URLs
// only) constrains.
func sanitizeDisplayURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	s := strings.ToLower(u.Scheme)
	if (s != "http" && s != "https") || u.Host == "" {
		return ""
	}
	return raw
}

// setHeaders sets the User-Agent, Accept, and the Prowlarr API key header.
func (u *upstream) setHeaders(req *http.Request) {
	req.Header.Set("User-Agent", appinfo.UserAgent)
	req.Header.Set("Accept", "application/rss+xml, application/xml")
	if u.apiKey != "" {
		req.Header.Set("X-Api-Key", u.apiKey)
	}
}
