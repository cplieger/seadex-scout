package mapping

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/animap/mapread"
	"github.com/cplieger/seadex-scout/internal/degradation"
)

// fourSeries is a usable cache of four series records from tag
// v2026.10.16.
func fourSeries() *Cache {
	prev := staleCache("v2026.10.16")
	prev.Records = []Record{
		{AniListID: 1, TvdbID: 100},
		{AniListID: 2, TvdbID: 200},
		{AniListID: 3, TvdbID: 300},
		{AniListID: 4, TvdbID: 400},
	}
	return prev
}

// TestLoader_refreshCache_rejectionStreakCountsAndResets pins the
// consecutive-rejection streak: an acceptance-guard refusal (here the
// below-half shrink guard) advances Cache.RejectedRefreshes - the ONE
// carrier, read by both the scout's escalation and its
// stale_consecutive_rejections attribute - and remembers the refused tag, so
// every later cycle on that tag advances it again with one bodiless request
// and no download, and the scout's WARN becomes an ERROR only at
// degradation.TickEscalationThreshold. A newer release that passes resets it.
func TestLoader_refreshCache_rejectionStreakCountsAndResets(t *testing.T) {
	rel := newRelease(t, testTag, entriesBody(t, entry{AniList: 9, Type: "tv", TVDB: 900}))
	prev := fourSeries()
	l := rel.loader(WithRefresh(time.Hour))
	for i := 1; i <= degradation.TickEscalationThreshold; i++ {
		next, err := l.refreshCache(t.Context(), prev)
		if _, ok := errors.AsType[*StaleMapError](err); !ok {
			t.Fatalf("rejection %d error = %v, want a *StaleMapError", i, err)
		}
		if next.RejectedRefreshes != i || next.RefusedTag != testTag || len(next.Records) != 4 || next.Tag != "v2026.10.16" {
			t.Fatalf("after %d rejections: streak %d, refused %q, %d records, tag %q, want %d, %q, the 4 stale records on their tag",
				i, next.RejectedRefreshes, next.RefusedTag, len(next.Records), next.Tag, i, testTag)
		}
		if escalates := next.RejectedRefreshes >= degradation.TickEscalationThreshold; escalates != (i >= degradation.TickEscalationThreshold) {
			t.Fatalf("after %d refusals the escalation gate = %v, want it only at the threshold", i, escalates)
		}
		*prev = next
	}
	if got := rel.downloads(); got != 1 {
		t.Errorf("downloads over %d refusals of one tag = %d, want 1", degradation.TickEscalationThreshold, got)
	}

	rel.set("v2026.10.18", entriesBody(t, seriesEntries(4)...))
	next, err := l.refreshCache(t.Context(), prev)
	if err != nil {
		t.Fatalf("accepted refresh after rejections returned error: %v", err)
	}
	if next.RejectedRefreshes != 0 || next.RefusedTag != "" || next.Tag != "v2026.10.18" || len(next.Records) != 4 {
		t.Errorf("accepted refresh = streak %d, refused %q, tag %q, %d records, want 0, none, v2026.10.18, 4",
			next.RejectedRefreshes, next.RefusedTag, next.Tag, len(next.Records))
	}
}

// TestLoader_refreshCache_refusalsKeepTheCacheAndAdvanceTheStreak pins every
// persistent arm of the refresh: each mapread refusal is asked once, never
// retried, keeps the cache unchanged, advances the streak, names its
// stale_reason class, and, once a tag was resolved, remembers it.
func TestLoader_refreshCache_refusalsKeepTheCacheAndAdvanceTheStreak(t *testing.T) {
	good := entriesBody(t, seriesEntries(4)...)
	tests := []struct {
		serve      func(r *release)
		sentinel   error
		name       string
		reason     string
		refusedTag string
	}{
		{
			name: "latest answers 200", sentinel: mapread.ErrRelease, reason: "refresh unexpected release answer",
			serve: func(r *release) {
				r.latest = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
			},
		},
		{
			name: "latest names a tag that is not a dated release", sentinel: mapread.ErrRelease, reason: "refresh unexpected release answer",
			serve: func(r *release) {
				r.latest = func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Location", r.srv.URL+"/"+DefaultRepo+"/releases/tag/mapread/v0.1.0")
					w.WriteHeader(http.StatusFound)
				}
			},
		},
		{
			name: "the tag lacks the asset", sentinel: mapread.ErrRelease, reason: "refresh unexpected release answer", refusedTag: testTag,
			serve: func(r *release) {
				r.asset = func(w http.ResponseWriter, req *http.Request) bool {
					if req.URL.Path != assetPath(testTag, "") {
						return false
					}
					http.NotFound(w, req)
					return true
				}
			},
		},
		{
			name: "a redirect to a foreign host", sentinel: mapread.ErrRedirect, reason: "refresh redirect refused", refusedTag: testTag,
			serve: func(r *release) { r.asset = redirectAsset("https://example.invalid/animap.json") },
		},
		{
			name: "a redirect to plain HTTP", sentinel: mapread.ErrRedirect, reason: "refresh redirect refused", refusedTag: testTag,
			serve: func(r *release) {
				r.asset = redirectAsset(strings.Replace(r.srv.URL, "https://", "http://", 1) + "/asset")
			},
		},
		{
			name: "a sidecar naming another body", sentinel: mapread.ErrChecksum, reason: "refresh checksum mismatch", refusedTag: testTag,
			serve: func(r *release) { r.sidecar = []byte(strings.Repeat("0", 64) + "  animap.json\n") },
		},
		{
			name: "a malformed sidecar", sentinel: mapread.ErrChecksum, reason: "refresh checksum mismatch", refusedTag: testTag,
			serve: func(r *release) { r.sidecar = []byte("not a checksum\n") },
		},
		{
			name: "a version 1 document", sentinel: mapread.ErrVersion, reason: "refresh unsupported schema version", refusedTag: testTag,
			serve: func(r *release) {
				r.publish(testTag, []byte(`{"version":1,"records":[{"anilist_id":1,"tvdb_id":100}]}`))
			},
		},
		{
			name: "not an animap document", sentinel: mapread.ErrNotDocument, reason: "refresh not an animap document", refusedTag: testTag,
			serve: func(r *release) { r.publish(testTag, []byte(`null`)) },
		},
		{
			name: "an invalid row", sentinel: mapread.ErrInvalid, reason: "refresh invalid document", refusedTag: testTag,
			serve: func(r *release) {
				r.publish(testTag, encode(t, document(nil, []mapread.Entry{{AniListID: 1, Parts: []mapread.Part{{AniDBID: 7, Kind: mapread.Regular}}}})))
			},
		},
		{
			name: "a row over the row cap", sentinel: mapread.ErrTooLarge, reason: "refresh exceeded size cap", refusedTag: testTag,
			serve: func(r *release) {
				r.publish(testTag, []byte(strings.Replace(string(good), `{"anidb_id":1,`, `{"anidb_id":1,"pad":"`+strings.Repeat("x", 300<<10)+`",`, 1)))
			},
		},
		{
			name: "anime placing more episodes than the projection reads", sentinel: mapread.ErrTooLarge, reason: "refresh exceeded size cap", refusedTag: testTag,
			serve: func(r *release) {
				var anime []mapread.Anime
				for aid := 1; aid <= 3; aid++ {
					anime = append(anime, mapread.Anime{AniDBID: aid, TVDBID: aid, Last: mapread.Last{Regular: maxAnimeEpisodes / 2}})
				}
				r.publish(testTag, encode(t, document(anime, nil)))
			},
		},
		{
			name: "a body over maxMapBytes", sentinel: mapread.ErrTooLarge, reason: "refresh exceeded size cap", refusedTag: testTag,
			serve: func(r *release) { r.publish(testTag, append(good, make([]byte, maxMapBytes)...)) },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rel := newRelease(t, testTag, good)
			rel.edit(tc.serve)
			prev := fourSeries()
			prev.RejectedRefreshes = 2
			next, err := rel.loader().refreshCache(t.Context(), prev)
			stale, ok := errors.AsType[*StaleMapError](err)
			if !ok || !errors.Is(err, tc.sentinel) {
				t.Fatalf("refresh error = %v, want a *StaleMapError wrapping %v", err, tc.sentinel)
			}
			if next.RejectedRefreshes != 3 || next.RefusedTag != tc.refusedTag {
				t.Errorf("streak %d, refused tag %q, want 3 and %q", next.RejectedRefreshes, next.RefusedTag, tc.refusedTag)
			}
			if len(next.Records) != 4 || next.Tag != "v2026.10.16" || !next.FetchedAt.Equal(prev.FetchedAt) {
				t.Errorf("cache = %d records on %q fetched %v, want the stale cache unchanged", len(next.Records), next.Tag, next.FetchedAt)
			}
			if got := stale.LogAttrs(); !attrsContain(got, "stale_reason", tc.reason) {
				t.Errorf("stale attrs = %v, want stale_reason %q", got, tc.reason)
			}
			if paths := rel.paths(); len(paths) != len(slices.Compact(slices.Sorted(slices.Values(paths)))) {
				t.Errorf("requests = %v, want each asked once: a refusal is not retried", paths)
			}
		})
	}
}

// TestLoader_refreshCache_crossesToTheNewShape pins the landing order of the
// new version 2 shape, read by a binary deployed before animap publishes it:
// a release still in the earlier shape is refused as another schema version
// and the cache held stays served, and the first release in the new shape is
// judged against that cache's records, so the acceptance guards still refuse
// one that loses a population and accept one that keeps them.
func TestLoader_refreshCache_crossesToTheNewShape(t *testing.T) {
	rel := newRelease(t, "v2026.10.17", earlierShape(t, entriesBody(t, seriesEntries(4)...)))
	l := rel.loader(WithRefresh(time.Hour))
	prev := fourSeries()

	next, err := l.refreshCache(t.Context(), prev)
	stale, ok := errors.AsType[*StaleMapError](err)
	if !ok || !errors.Is(err, mapread.ErrVersion) || !attrsContain(stale.LogAttrs(), "stale_reason", "refresh unsupported schema version") {
		t.Fatalf("refresh(earlier shape) error = %v, want the unsupported schema version refusal", err)
	}
	if next.Tag != "v2026.10.16" || len(next.Records) != 4 || next.RefusedTag != "v2026.10.17" || next.RejectedRefreshes != 1 {
		t.Fatalf("after the earlier shape: tag %q, %d records, refused %q, streak %d, want the 4 cached records on v2026.10.16, v2026.10.17 refused, streak 1",
			next.Tag, len(next.Records), next.RefusedTag, next.RejectedRefreshes)
	}

	rel.set("v2026.10.18", entriesBody(t, entry{AniList: 9, Type: "tv", TVDB: 900}))
	next, err = l.refreshCache(t.Context(), &next)
	if stale, ok := errors.AsType[*StaleMapError](err); !ok || !attrsContain(stale.LogAttrs(), "stale_reason", "refresh validation failed") {
		t.Fatalf("refresh(new shape losing 3 of 4 series) error = %v, want the acceptance guard's refusal", err)
	}
	if next.Tag != "v2026.10.16" || len(next.Records) != 4 || next.RejectedRefreshes != 2 {
		t.Fatalf("after the shrunk new shape: tag %q, %d records, streak %d, want the 4 cached records on v2026.10.16, streak 2",
			next.Tag, len(next.Records), next.RejectedRefreshes)
	}

	rel.set("v2026.10.19", entriesBody(t, seriesEntries(4)...))
	next, err = l.refreshCache(t.Context(), &next)
	if err != nil {
		t.Fatalf("refresh(new shape keeping every series) error = %v, want acceptance", err)
	}
	if next.Tag != "v2026.10.19" || len(next.Records) != 4 || next.RefusedTag != "" || next.RejectedRefreshes != 0 {
		t.Errorf("accepted new shape: tag %q, %d records, refused %q, streak %d, want v2026.10.19, 4, none, 0",
			next.Tag, len(next.Records), next.RefusedTag, next.RejectedRefreshes)
	}
}

// redirectAsset answers both assets with a 302 to target.
func redirectAsset(target string) func(http.ResponseWriter, *http.Request) bool {
	return func(w http.ResponseWriter, req *http.Request) bool {
		if !strings.Contains(req.URL.Path, "/releases/download/") {
			return false
		}
		http.Redirect(w, req, target, http.StatusFound)
		return true
	}
}

// TestLoader_refreshCache_sidecarMismatchKeepsTheCache pins the sidecar
// check end to end: both reads are pinned to the latest release's tag, and a
// body that does not match its sidecar changes nothing in the cache but the
// streak and the refused tag.
func TestLoader_refreshCache_sidecarMismatchKeepsTheCache(t *testing.T) {
	rel := newRelease(t, testTag, entriesBody(t, seriesEntries(4)...))
	rel.edit(func(r *release) { r.sidecar = []byte(strings.Repeat("f", 64) + "  animap.json\n") })
	prev := fourSeries()
	prev.Mappings = map[int]Mapping{1: {Seasons: []SeasonRange{{Season: 1, First: 1, Last: 1}}}}
	next, err := rel.loader().refreshCache(t.Context(), prev)
	if !errors.Is(err, mapread.ErrChecksum) {
		t.Fatalf("refresh error = %v, want a checksum refusal", err)
	}
	want := []string{"/" + DefaultRepo + "/releases/latest", assetPath(testTag, ".sha256"), assetPath(testTag, "")}
	if got := rel.paths(); !slices.Equal(got, want) {
		t.Errorf("requests = %v, want the latest check, then the sidecar and the body both at %s", got, testTag)
	}
	next.RejectedRefreshes, next.RefusedTag = prev.RejectedRefreshes, prev.RefusedTag
	if fmt.Sprint(next) != fmt.Sprint(*prev) {
		t.Errorf("cache after a mismatch = %+v, want %+v apart from the streak and refused tag", next, *prev)
	}
}

// TestLoader_refreshCache_transientFailuresKeepTheStreak pins every
// transient arm: a come-back-later status on any request, or a body cut
// short, keeps the cache and neither advances nor resets the streak, and
// remembers no tag, so the scout never escalates on an outage.
func TestLoader_refreshCache_transientFailuresKeepTheStreak(t *testing.T) {
	status := func(code int) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
	}
	onAsset := func(suffix string, h func(http.ResponseWriter, *http.Request)) func(*release) {
		return func(r *release) {
			r.asset = func(w http.ResponseWriter, req *http.Request) bool {
				if req.URL.Path != assetPath(testTag, suffix) {
					return false
				}
				h(w, req)
				return true
			}
		}
	}
	tests := map[string]func(*release){
		"latest 408":  func(r *release) { r.latest = status(http.StatusRequestTimeout) },
		"latest 425":  func(r *release) { r.latest = status(http.StatusTooEarly) },
		"latest 429":  func(r *release) { r.latest = status(http.StatusTooManyRequests) },
		"latest 500":  func(r *release) { r.latest = status(http.StatusInternalServerError) },
		"latest 503":  func(r *release) { r.latest = status(http.StatusServiceUnavailable) },
		"sidecar 502": onAsset(".sha256", status(http.StatusBadGateway)),
		"body 429":    onAsset("", status(http.StatusTooManyRequests)),
		"body 504":    onAsset("", status(http.StatusGatewayTimeout)),
		"a truncated body": onAsset("", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "1000")
			_, _ = w.Write([]byte(`{"version":2,`))
		}),
		"a truncated sidecar": onAsset(".sha256", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "200")
			_, _ = w.Write([]byte("0123"))
		}),
	}
	for name, serve := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rel := newRelease(t, testTag, entriesBody(t, seriesEntries(4)...))
			rel.edit(serve)
			prev := fourSeries()
			prev.RejectedRefreshes = 3
			next, err := rel.loader().refreshCache(t.Context(), prev)
			stale, ok := errors.AsType[*StaleMapError](err)
			if !ok || refusal(err) != "" {
				t.Fatalf("refresh error = %v, want a *StaleMapError carrying no refusal", err)
			}
			if next.RejectedRefreshes != 3 || next.RefusedTag != "" || len(next.Records) != 4 || next.Tag != "v2026.10.16" {
				t.Errorf("cache = streak %d, refused %q, %d records on %q, want the stale cache with its streak of 3", next.RejectedRefreshes,
					next.RefusedTag, len(next.Records), next.Tag)
			}
			if got := stale.LogAttrs(); !attrsContain(got, "stale_reason", "refresh failed") {
				t.Errorf("stale attrs = %v, want stale_reason \"refresh failed\"", got)
			}
		})
	}
}

// TestLoader_refreshCache_streakAdvancesWithNoUsableCache pins the streak as
// state about the UPSTREAM, not about the cache: on a first boot whose every
// refresh is refused there is no usable stale map to return, and gating the
// increment on one would freeze the streak at 0 and leave the loader
// degrading at WARN forever. With no usable cache the refused tag saves no
// download.
func TestLoader_refreshCache_streakAdvancesWithNoUsableCache(t *testing.T) {
	rel := newRelease(t, testTag, []byte(`{"version":1,"records":[]}`))
	prev := &Cache{}
	l := rel.loader(WithRefresh(time.Hour))
	for i := 1; i <= degradation.TickEscalationThreshold; i++ {
		next, err := l.refreshCache(t.Context(), prev)
		if err == nil {
			t.Fatalf("refresh %d returned nil error, want the no-cache error", i)
		}
		if _, ok := errors.AsType[*StaleMapError](err); ok {
			t.Fatalf("refresh %d returned a *StaleMapError, want the no-cache error (there is no usable map to serve)", i)
		}
		if next.RejectedRefreshes != i {
			t.Errorf("RejectedRefreshes after %d refusals = %d, want %d", i, next.RejectedRefreshes, i)
		}
		*prev = next
	}
	if got := rel.downloads(); got != degradation.TickEscalationThreshold {
		t.Errorf("downloads = %d, want one per cycle (%d) while no cache is usable", got, degradation.TickEscalationThreshold)
	}
}

// TestIsPersistentRefreshFailure is the table over the ONE home of the
// transient-vs-persistent refresh classification. Every class named
// persistent in isPersistentRefreshFailure's doc comment appears here.
func TestIsPersistentRefreshFailure(t *testing.T) {
	for name, tc := range map[string]struct {
		cause error
		class refreshFailureClass
		want  bool
	}{
		"unexpected release answer": {fmt.Errorf("latest: %w", mapread.ErrRelease), failureFetch, true},
		"refused redirect":          {logSafeCause(fmt.Errorf("get: %w", mapread.ErrRedirect)), failureFetch, true},
		"checksum mismatch":         {fmt.Errorf("%w: v1", mapread.ErrChecksum), failureFetch, true},
		"another version":           {&mapread.VersionError{Found: 3}, failureFetch, true},
		"not an animap document":    {mapread.ErrNotDocument, failureFetch, true},
		"invalid document":          {mapread.ErrInvalid, failureFetch, true},
		"over a cap":                {mapread.ErrTooLarge, failureFetch, true},
		"refused release unchanged": {nil, failureRefusedUnchanged, true},
		"acceptance invariant":      {errors.New("arr identifier coverage 1/200 is below minimum 2"), failureValidation, true},
		"whole-map shrink guard":    {nil, failureShrunk, true},
		"transport error":           {errors.New("transport refused by test"), failureFetch, false},
		"come-back-later status":    {errors.New("mapread: latest release answered HTTP 503"), failureFetch, false},
		"truncated body":            {fmt.Errorf("mapread: body: %w", errors.New("unexpected EOF")), failureFetch, false},
	} {
		if got := isPersistentRefreshFailure(tc.class, tc.cause); got != tc.want {
			t.Errorf("isPersistentRefreshFailure(%v, %v) = %v, want %v (%s)", tc.class, tc.cause, got, tc.want, name)
		}
	}
}

// attrsContain reports whether the flattened slog key/value pairs carry key
// with the wanted value.
func attrsContain(attrs []any, key, want string) bool {
	for i := 0; i+1 < len(attrs); i += 2 {
		if k, ok := attrs[i].(string); ok && k == key {
			if v, ok := attrs[i+1].(string); ok {
				return v == want
			}
		}
	}
	return false
}
