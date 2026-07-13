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
	return parseTorznab(body)
}

// setHeaders sets the User-Agent, Accept, and the Prowlarr API key header.
func (u *upstream) setHeaders(req *http.Request) {
	req.Header.Set("User-Agent", appinfo.UserAgent)
	req.Header.Set("Accept", "application/rss+xml, application/xml")
	if u.apiKey != "" {
		req.Header.Set("X-Api-Key", u.apiKey)
	}
}
