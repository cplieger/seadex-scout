package mapping

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/seadex-scout/internal/appinfo"
)

// DefaultListURL is the Anime-Lists anime-list-master.xml endpoint, the second
// mapping upstream: the mapping-list Fribb's mini variant drops. The variant
// choice is Anime-Lists contract knowledge, so it lives here.
const DefaultListURL = "https://raw.githubusercontent.com/Anime-Lists/anime-lists/master/anime-list-master.xml"

// maxListBytes bounds the mapping-list download before decode (~4.5x the real
// 3.5 MB body).
const maxListBytes = 16 << 20

// SeasonRange is one TVDB season an absolute-numbered run's episodes fall
// into, read from the Anime-Lists mapping-list: episodes First..Last of the
// run are TVDB season Season. Last == 0 means the range is open-ended (the
// list's last row carries a start and no end while the show is airing).
type SeasonRange struct {
	Season int `json:"season"`
	First  int `json:"first"`
	Last   int `json:"last,omitempty"`
}

// Mapping is what the Anime-Lists mapping-list says about one AniDB entry
// beyond what Fribb carries: which TVDB season-0 episode a film filed in a
// series' specials IS (SpecialEpisode, 0 when the list names none), and which
// TVDB seasons an absolute-numbered run's episodes fall into (Seasons, nil when
// the node carries no ranged rows). Keyed by Record.AniDBID in Cache.Mappings.
type Mapping struct {
	Seasons        []SeasonRange `json:"seasons,omitempty"`
	SpecialEpisode int           `json:"special_episode,omitempty"`
}

// ListLoader fetches the Anime-Lists mapping-list with the same conditional-GET
// and stale-on-error shape as the Fribb Loader, and folds its facts into the four
// Mappings fields of the Cache the Loader carries. It is a separate type because
// the Fribb refresh pipeline (a JSON stream, a population census, a rejection
// streak) is Fribb-specific end to end; this upstream has its own body, its own
// validators and its own cadence.
type ListLoader struct {
	http *http.Client
	log  *slog.Logger
	url  string
}

// NewListLoader returns a mapping-list loader reading the XML source at url.
// httpClient must be non-nil; a nil log falls back to slog.Default().
func NewListLoader(httpClient *http.Client, url string, log *slog.Logger) *ListLoader {
	if log == nil {
		log = slog.Default()
	}
	return &ListLoader{http: httpClient, log: log, url: url}
}

// refresh revalidates the mapping-list and mutates ONLY the four Mappings fields
// of next: a 200 that parses replaces the map and both validators, a 304 bumps the
// timestamp, and every failure (fetch, size cap, parse, bound) keeps all four as
// they were, so the previous list stays served stale. It never returns an error:
// a list failure is logged at WARN and the Load it rides on is unaffected, since
// the Fribb map is what the cycle cannot run without. Validators are sent only
// while a populated map is held, so a 304 can never affirm a map nothing has.
func (l *ListLoader) refresh(ctx context.Context, next *Cache) {
	var validators httpx.Validators
	if len(next.Mappings) > 0 {
		validators = httpx.Validators{ETag: next.MappingsETag, LastModified: next.MappingsLastModified}
	}
	res, err := l.conditionalGet(ctx, validators)
	if err != nil {
		l.warnStale(ctx, "mapping: mapping-list refresh failed, serving the previous list", err, next)
		return
	}
	if res.NotModified {
		next.MappingsFetchedAt = time.Now()
		l.log.Debug("mapping: mapping-list not modified, reusing list", "mappings", len(next.Mappings))
		return
	}
	mappings, err := parseMappingList(res.Body)
	if err != nil {
		l.warnStale(ctx, "mapping: mapping-list parse failed, serving the previous list", logSafeCause(err), next)
		return
	}
	next.Mappings = mappings
	next.MappingsETag = res.Validators.ETag
	next.MappingsLastModified = res.Validators.LastModified
	next.MappingsFetchedAt = time.Now()
	l.log.Info("mapping: mapping-list refreshed",
		"mappings", len(mappings),
		"bytes", len(res.Body),
		"revalidatable", res.Validators.ETag != "" || res.Validators.LastModified != "")
}

// warnStale logs a list refresh failure at WARN with the stale list's age and
// mapping count, unless the caller's context ended, in which case the failure is
// the shutdown and not the upstream.
func (l *ListLoader) warnStale(ctx context.Context, msg string, err error, prev *Cache) {
	if ctx.Err() != nil {
		return
	}
	l.log.Warn(msg, "error", err, "stale_mappings", len(prev.Mappings),
		"stale_age_seconds", max(time.Duration(0), time.Since(prev.MappingsFetchedAt).Round(time.Second)).Seconds())
}

// conditionalGet issues the GET with validators via httpx.DoConditional under
// the same retry policy as the Fribb loader's conditionalGet.
func (l *ListLoader) conditionalGet(ctx context.Context, validators httpx.Validators) (httpx.ConditionalResult, error) {
	return httpx.Do(ctx,
		func(ctx context.Context) (httpx.ConditionalResult, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.url, http.NoBody)
			if err != nil {
				return httpx.ConditionalResult{}, err
			}
			req.Header.Set("User-Agent", appinfo.UserAgent)
			return httpx.DoConditional(l.http, req, validators, maxListBytes)
		},
		httpx.WithMaxAttempts(maxAttempts),
		httpx.WithBaseDelay(baseDelay),
		httpx.WithLabel("mapping-list"),
		httpx.WithLogger(l.log),
		// Demote httpx's terminal exhaustion line to Debug, as the Fribb loader does:
		// the failure surfaces again from refresh with the stale list's context.
		httpx.WithExhaustedLevel(slog.LevelDebug))
}
