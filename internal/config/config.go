// Package config loads seadex-scout configuration from a single YAML file
// (default /config/config.yaml). The file is the whole settings surface;
// string values may reference SONARR_*, RADARR_*, or SEADEX_SCOUT_* environment
// variables via ${VAR} expansion, so secrets can stay in an .env or Docker
// secret rather than in the file.
//
// The file exposes only user-facing settings (arrs, mode, schedule, filters,
// arr_tags, report dir, logging, the indexer feed). Internal machinery - the
// upstream endpoints, the politeness/refresh/rate cadences, the indexer bind
// address, and the /config file paths (state, overrides, reports) - are fixed
// package constants, not file keys. The on-disk shape (fileConfig) is loaded
// onto a defaults baseline, ${VAR}-expanded, then flattened into the runtime
// Config the rest of the app reads. Call Validate to check the result is
// runnable. There is no hot reload: the file is read once at startup.
package config

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/envx/yamlenv"
	"github.com/cplieger/scheduler/v2"
	"github.com/cplieger/slogx"
	"go.yaml.in/yaml/v3"
)

// DefaultConfigPath is the container-internal config file path.
const DefaultConfigPath = "/config/config.yaml"

// maxConfigBytes bounds the config file read (it is a small document).
const maxConfigBytes = 1 << 20

// Fixed endpoints, cadences, and /config file paths. These are internal
// machinery wired at build time, deliberately NOT exposed as config-file keys:
// the user should never need to point the app at a different SeaDex/Fribb/
// AniList, retune the politeness delays, or relocate the state/report files
// (everything lives under the single /config mount).
const (
	// DefaultSeaDexBaseURL is the SeaDex (releases.moe) API base.
	DefaultSeaDexBaseURL = "https://releases.moe"
	// DefaultMappingURL is the Fribb anime-lists AniList<->arr ID bridge.
	DefaultMappingURL = "https://raw.githubusercontent.com/Fribb/anime-lists/master/anime-list-mini.json"
	// DefaultAniListURL is the AniList GraphQL endpoint (title/format fallback).
	DefaultAniListURL = "https://graphql.anilist.co"
	// DefaultMappingOverrides is the local alID->IDs override file: drop one in
	// at this path to pin mappings; absent is fine.
	DefaultMappingOverrides = "/config/overrides.json"
	// DefaultStatePath is the atomic JSON cache/state file.
	DefaultStatePath = "/config/state.json"
	// DefaultIndexerFeedPath is the atomic JSON file the compare cycle writes the
	// indexer's materialized feed to (the search curation set plus the two
	// synthesized per-tracker RSS feeds) and the indexer HTTP server reads. One
	// data engine (the cycle) produces both the findings and this feed, and
	// persisting it lets a cycle run by the `poll` subcommand refresh a resident
	// daemon's feed across the process boundary.
	DefaultIndexerFeedPath = "/config/feed.json"
	// DefaultReportDir is the directory report mode writes timestamped report
	// pairs into (report-<UTC timestamp>.md / .json).
	DefaultReportDir = "/config/reports"

	// RunModeDaemon is the default: poll on a schedule and flag better releases.
	RunModeDaemon = "daemon"
	// RunModeReport is the one-shot audit: scan once, write the report, exit.
	RunModeReport = "report"

	// DefaultPollInterval is the gap between cycles (also runs on start). One
	// cycle drives both halves: the compare/findings pass and, when the Torznab
	// feed is configured, its curation set + RSS feed rebuild - so a notification
	// and what the arrs see in the feed come from the same fetch.
	DefaultPollInterval = 3 * time.Hour
	// DefaultSeaDexPageDelay is the politeness delay between SeaDex pages.
	DefaultSeaDexPageDelay = 2 * time.Second
	// DefaultMappingRefresh is the reuse-if-fresh window for the Fribb map. 0
	// revalidates every cycle: each cycle issues a conditional GET
	// (ETag/If-Modified-Since), so an unchanged map (the common case, since Fribb
	// updates ~weekly) is a cheap 304 with no re-download, while a change is picked
	// up within one cycle instead of lagging a fixed cadence. A failed
	// revalidation is harmless (the persisted cache is reused stale-on-error and
	// the next cycle retries), and the full ~5.9 MB download still happens only
	// when Fribb actually changes, so per-cycle revalidation stays cheap.
	DefaultMappingRefresh = 0
	// DefaultAniListRate is the AniList request/minute ceiling.
	DefaultAniListRate = 30
)

// Clamp bounds for poll_interval, the only file-provided duration.
const (
	minPollInterval = time.Hour
	maxPollInterval = 30 * 24 * time.Hour
)

// fileConfig is the on-disk YAML shape: only the user-facing settings.
type fileConfig struct {
	Indexer      indexerFile `yaml:"indexer"`
	Log          logFile     `yaml:"log"`
	Report       reportFile  `yaml:"report"`
	PollInterval string      `yaml:"poll_interval"`
	Mode         string      `yaml:"mode"`
	Radarr       arrFile     `yaml:"radarr"`
	Sonarr       arrFile     `yaml:"sonarr"`
	ArrTags      tagsFile    `yaml:"arr_tags"`
	Filters      filtersFile `yaml:"filters"`
	// AnimeBytes adds AnimeBytes (private tracker) releases and links to findings
	// and the report; it is a tracker-access toggle (do you have an account?),
	// not a content filter, so it sits at the top level rather than under filters.
	AnimeBytes bool `yaml:"animebytes"`
}

// indexerFile configures the optional Torznab feed the daemon serves alongside
// the compare loop. Searches proxy Prowlarr's per-indexer Torznab endpoints
// (Nyaa + AnimeBytes) filtered to SeaDex's curation, so they need only the
// Prowlarr API key. The periodic RSS feed is synthesized from the SeaDex list
// with directly-built download links; AnimeBytes links need the operator's
// passkey (ab_passkey), the one tracker credential here - public Nyaa links need
// none. An empty Nyaa/AnimeBytes URL disables that upstream; both empty disables
// the feed entirely (the daemon then binds no HTTP port). An empty ab_passkey
// leaves the AnimeBytes RSS feed without grabbable links (search still works via
// Prowlarr).
type indexerFile struct {
	FeedAPIKey     string `yaml:"feed_api_key"`
	NyaaTorznabURL string `yaml:"nyaa_torznab_url"`
	ABTorznabURL   string `yaml:"ab_torznab_url"`
	ProwlarrAPIKey string `yaml:"prowlarr_api_key"`
	ABPasskey      string `yaml:"ab_passkey"`
}

type arrFile struct {
	URL       string `yaml:"url"`
	APIKey    string `yaml:"api_key"`
	PublicURL string `yaml:"public_url"`
	Enabled   bool   `yaml:"enabled"`
}

type filtersFile struct {
	ExcludeRemux     bool `yaml:"exclude_remux"`
	RequireDualAudio bool `yaml:"require_dual_audio"`
	ExcludeSpecials  bool `yaml:"exclude_specials"`
}

type tagsFile struct {
	Include []string `yaml:"include"`
	Exclude []string `yaml:"exclude"`
}

type reportFile struct {
	Dir string `yaml:"dir"`
}

type logFile struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// defaultFileConfig is the baseline the YAML document overlays. Absent keys
// keep these values, so a partial config still runs. The filter toggles all
// default to their false zero value (nothing excluded), so they need no entry
// here.
func defaultFileConfig() fileConfig {
	return fileConfig{
		Sonarr: arrFile{URL: "http://sonarr:8989"},
		Radarr: arrFile{URL: "http://radarr:7878"},
		Mode:   RunModeDaemon,
		Report: reportFile{Dir: DefaultReportDir},
		Log:    logFile{Level: "info", Format: "json"},
	}
}

// Config is the effective runtime configuration after loading. It holds only
// the user-configurable settings; the fixed endpoints, cadences, bind address,
// and /config file paths are package constants (see the const block), wired in
// build.go. Fields are ordered largest-alignment-first for govet fieldalignment.
type Config struct {
	RunMode   string // "daemon" (default) or "report" (one-shot audit).
	ReportDir string // directory for timestamped report-<ts>.md / .json pairs.

	SonarrURL       string // Sonarr instance URL the app queries.
	SonarrAPIKey    string
	SonarrPublicURL string // browser URL for deep-links; falls back to SonarrURL.
	RadarrURL       string
	RadarrAPIKey    string
	RadarrPublicURL string

	// Indexer (Torznab feed) settings. IndexerAPIKey (the feed's own gate),
	// IndexerProwlarrAPIKey, and IndexerABPasskey are secrets and are never
	// logged. Searches proxy Prowlarr's per-indexer Torznab endpoints for Nyaa
	// and AnimeBytes (an empty URL disables that upstream); the RSS feed is
	// synthesized from SeaDex, and IndexerABPasskey builds its AnimeBytes
	// download links (empty leaves the AB RSS feed without grabbable links).
	IndexerAPIKey         string
	IndexerNyaaTorznabURL string
	IndexerABTorznabURL   string
	IndexerProwlarrAPIKey string
	IndexerABPasskey      string

	IncludeTags []string
	ExcludeTags []string

	PollInterval time.Duration
	LogLevel     slog.Level
	// LogFormat is the typed slogx handler encoding (JSON default), parsed from
	// log.format by parseLogFormat.
	LogFormat slogx.Format

	// ExcludeRemux drops releases classified remux (default false: remuxes kept).
	ExcludeRemux     bool
	RequireDualAudio bool
	// AnimeBytes includes AnimeBytes (private tracker) releases and links; the
	// public trackers (Nyaa, AnimeTosho, RuTracker) are always included.
	AnimeBytes bool
	// ExcludeSpecials drops OVA/ONA/special entries (default false: kept).
	ExcludeSpecials bool
	// PollExternal is set when poll_interval is off/disabled/0: no internal
	// timer, cycles are triggered out-of-band via the `poll` subcommand.
	PollExternal bool
}

// Load reads, ${VAR}-expands, and parses the YAML config at path into the
// runtime Config. It returns an error on a missing/oversized file, invalid
// YAML, or an unknown configuration key (a misspelled or misplaced key fails
// loudly at startup rather than being silently ignored); call Validate for
// semantic checks.
func Load(path string) (Config, error) {
	// Read through the shared atomicfile bounded reader (the same primitive
	// writeStarterConfig and internal/state use), which enforces the size cap and
	// returns the atomicfile.ErrFileTooLarge sentinel on an oversized file.
	// Config load is a synchronous startup step with no cancellation point, so it
	// passes context.Background(), matching writeStarterConfig.
	data, err := atomicfile.ReadBounded(context.Background(), path, maxConfigBytes)
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		// Same fail-closed sanitizer as the decode errors below: a parse error
		// can embed operator-written text adjacent to a secret (e.g. an
		// unquoted literal secret read as an alias yields "unknown anchor
		// '<secret>' referenced"), and main logs this error at startup.
		return Config{}, fmt.Errorf("parse config %s: %s", path, sanitizeYAMLError(err))
	}
	if err := checkUnknownKeys(data); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %s", path, sanitizeYAMLError(err))
	}
	if refs := yamlenv.Expand(&doc, isAllowedEnvVar); len(refs) > 0 {
		slog.Warn("config references environment variables that are not set; "+
			"the literal ${VAR} is kept and will likely fail authentication",
			"vars", strings.Join(refs, ","))
	}
	fc := defaultFileConfig()
	if err := doc.Decode(&fc); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %s", path, sanitizeYAMLError(err))
	}
	return fc.toConfig(), nil
}

// checkUnknownKeys re-decodes the raw document with KnownFields(true) into a
// throwaway fileConfig so a key the on-disk shape does not declare fails the
// load ("line N: field X not found in type ..."), instead of being silently
// ignored (e.g. a top-level anime_bytes or a filters.animebytes leaving the
// real animebytes toggle false). It runs on the pre-expansion bytes - Load's
// yaml.Node path has no KnownFields switch - so line numbers point at the file
// the operator wrote, and expansion (string values only, keys stay literal)
// cannot change which keys exist. Any accompanying type-error entries carry
// the literal ${VAR}, not an expanded secret, and every entry still passes
// through sanitizeYAMLError at the Load call site.
func checkUnknownKeys(data []byte) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var probe fileConfig
	if err := dec.Decode(&probe); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// Value-independent markers of a yaml.v3 TypeError entry ("line N: cannot
// unmarshal !!str `...` into bool"): everything between the source tag and the
// final destination-type marker is the scalar excerpt and is dropped.
const (
	yamlUnmarshalMarker = "cannot unmarshal !!"
	yamlIntoMarker      = " into "
)

// Value-independent markers of the duplicate-key TypeError entry
// ("line N: mapping key "x" already defined at line M"): the key excerpt
// between them is dropped, the two line numbers are kept.
const (
	yamlDupKeyMarker    = ": mapping key "
	yamlDupKeyDefinedAt = " already defined at line "
)

// Value-independent markers of the strict-decode unknown-key entry
// ("line N: field X not found in type config.fileConfig", from
// checkUnknownKeys): the key name between them is kept - it IS the diagnostic
// the operator needs to fix the typo - and the Go type name after the second
// marker is dropped.
const (
	yamlUnknownKeyMarker = ": field "
	yamlUnknownKeyInType = " not found in type "
)

// sanitizeYAMLError rewrites a yaml decode error so an expanded secret never
// reaches the startup log; line numbers and target types are kept
// (field-name-only, the same posture as validateHTTPURL errors). The decode
// runs after ${VAR} expansion, so the excerpt yaml.v3 embeds can carry a
// prefix of an expanded secret (an api key placed in a non-string field by a
// config typo). Backtick-pair matching was rejected: yaml.v3 truncates the
// excerpt with any embedded backtick unchanged, so a secret containing a
// backtick defeats a delimiter regex and leaks a prefix. Instead each
// *yaml.TypeError entry is rebuilt from its value-independent structure, and
// an unrecognized error shape falls back to a generic message rather than
// risking a partial leak.
func sanitizeYAMLError(err error) string {
	var typeErr *yaml.TypeError
	if !errors.As(err, &typeErr) {
		return "configuration could not be decoded (details withheld: they may embed an expanded secret)"
	}
	entries := make([]string, 0, len(typeErr.Errors))
	for _, e := range typeErr.Errors {
		entries = append(entries, sanitizeTypeErrorEntry(e))
	}
	return "unmarshal errors: " + strings.Join(entries, "; ")
}

// sanitizeTypeErrorEntry rebuilds one TypeError entry keeping only its
// value-independent parts: the "line N: cannot unmarshal !!<tag>" prefix and
// the " into <type>" suffix. strings.LastIndex locates the suffix so backticks
// or newlines inside the scalar excerpt are irrelevant. A duplicate-mapping-key
// entry ("line N: mapping key "x" already defined at line M") is a second
// value-independent shape and keeps both line numbers; only the key excerpt is
// redacted (a misindented paste can put a secret in key position, so stay
// field-name-only like the rest of the file). The unknown-key entry from the
// strict checkUnknownKeys pre-decode ("line N: field X not found in type T")
// is a third shape: the key name is kept - it is the diagnostic the operator
// needs to fix the typo - and the isLinePrefix guard ensures a wrong-type
// scalar excerpt that happens to embed both of its markers is never mistaken
// for it (such an entry starts with the unmarshal shape, not a bare "line N",
// so it falls through to the redacting branches instead).
// lineEntryBounds locates one structured TypeError entry shape: startMarker
// must appear after a bare "line N" prefix (the isLinePrefix guard - a
// wrong-type scalar excerpt embedding the same marker pair starts with the
// unmarshal shape instead, so it never matches) and endMarker must follow it.
// It is the single home of the boundary validation both structured-entry
// branches of sanitizeTypeErrorEntry share.
func lineEntryBounds(entry, startMarker, endMarker string) (start, end int, ok bool) {
	start = strings.Index(entry, startMarker)
	if start < 0 || !isLinePrefix(entry[:start]) {
		return 0, 0, false
	}
	end = strings.LastIndex(entry, endMarker)
	return start, end, end > start
}

func sanitizeTypeErrorEntry(entry string) string {
	if k, at, ok := lineEntryBounds(entry, yamlDupKeyMarker, yamlDupKeyDefinedAt); ok {
		return entry[:k] + ": mapping key <redacted>" + entry[at:]
	}
	if k, at, ok := lineEntryBounds(entry, yamlUnknownKeyMarker, yamlUnknownKeyInType); ok {
		return fmt.Sprintf("%s: unknown configuration key %q",
			entry[:k], entry[k+len(yamlUnknownKeyMarker):at])
	}
	start := strings.Index(entry, yamlUnmarshalMarker)
	end := strings.LastIndex(entry, yamlIntoMarker)
	if start < 0 || end < start {
		return "configuration contains a value of the wrong type"
	}
	tagEnd := start + len(yamlUnmarshalMarker)
	for tagEnd < len(entry) && entry[tagEnd] != ' ' {
		tagEnd++
	}
	return entry[:tagEnd] + " <redacted>" + entry[end:]
}

// isLinePrefix reports whether s is exactly "line <digits>", the prefix a
// genuine yaml.v3 TypeError entry carries before its first marker. It guards
// BOTH rebuilds that keep text from outside their markers - the duplicate-key
// branch (keeps entry[:k] and entry[at:]) and the unknown-key branch (keeps
// the key name between its markers) - against a wrong-type scalar excerpt
// embedding the same marker pair: that entry's prefix is the unmarshal shape
// ("line N: cannot unmarshal !!str `..."), never a bare "line N".
func isLinePrefix(s string) bool {
	digits, ok := strings.CutPrefix(s, "line ")
	if !ok || digits == "" {
		return false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// toConfig flattens the on-disk shape into the runtime Config, applying
// normalization and the enabled toggles (a disabled arr leaves its URL/key
// empty, so it is simply skipped downstream).
func (fc *fileConfig) toConfig() Config {
	c := Config{
		RunMode:               strings.ToLower(strings.TrimSpace(fc.Mode)),
		ReportDir:             strings.TrimSpace(fc.Report.Dir),
		LogFormat:             parseLogFormat(fc.Log.Format),
		IncludeTags:           trimList(fc.ArrTags.Include),
		ExcludeTags:           trimList(fc.ArrTags.Exclude),
		LogLevel:              parseLogLevel(fc.Log.Level),
		ExcludeRemux:          fc.Filters.ExcludeRemux,
		RequireDualAudio:      fc.Filters.RequireDualAudio,
		AnimeBytes:            fc.AnimeBytes,
		ExcludeSpecials:       fc.Filters.ExcludeSpecials,
		IndexerAPIKey:         strings.TrimSpace(fc.Indexer.FeedAPIKey),
		IndexerNyaaTorznabURL: strings.TrimSpace(fc.Indexer.NyaaTorznabURL),
		IndexerABTorznabURL:   strings.TrimSpace(fc.Indexer.ABTorznabURL),
		IndexerProwlarrAPIKey: strings.TrimSpace(fc.Indexer.ProwlarrAPIKey),
		IndexerABPasskey:      strings.TrimSpace(fc.Indexer.ABPasskey),
	}
	if fc.Sonarr.Enabled {
		c.SonarrURL = strings.TrimSpace(fc.Sonarr.URL)
		c.SonarrAPIKey = strings.TrimSpace(fc.Sonarr.APIKey)
		c.SonarrPublicURL = strings.TrimSpace(fc.Sonarr.PublicURL)
	} else if strings.TrimSpace(fc.Sonarr.APIKey) != "" {
		// A set api_key is always operator-written (the defaults baseline
		// carries none), so this is a half-configuration signal: the arr is
		// filled in but the enabled toggle was left off. Info, not Warn - the
		// deliberate temporary-disable case must not raise Loki alert noise.
		slog.Info("sonarr.api_key is set but sonarr.enabled is false; sonarr will not be scanned")
	}
	if fc.Radarr.Enabled {
		c.RadarrURL = strings.TrimSpace(fc.Radarr.URL)
		c.RadarrAPIKey = strings.TrimSpace(fc.Radarr.APIKey)
		c.RadarrPublicURL = strings.TrimSpace(fc.Radarr.PublicURL)
	} else if strings.TrimSpace(fc.Radarr.APIKey) != "" {
		slog.Info("radarr.api_key is set but radarr.enabled is false; radarr will not be scanned")
	}
	if c.ReportDir == "" {
		c.ReportDir = DefaultReportDir
	}
	c.PollInterval, c.PollExternal = parseInterval(fc.PollInterval)
	return c
}

// parseInterval reads the poll_interval value into a built-in cadence or the
// external (resident-idle) mode, following the fleet `*_INTERVAL` convention.
// It delegates to scheduler.ParseInterval (WithBounds clamps a built-in cadence
// to [minPollInterval, maxPollInterval]): off/disabled/0/0s -> external (no
// internal timer, cycles triggered via `poll`); empty -> the default; a valid
// positive duration -> built-in (clamped); a negative or unparseable value ->
// the default with a warning. WithRedactedValue keeps every scheduler warning
// field-name-only, because an expanded ${VAR} secret placed in poll_interval
// by a config typo must never reach the startup log.
func parseInterval(raw string) (time.Duration, bool) {
	s := scheduler.ParseInterval(raw, DefaultPollInterval,
		scheduler.WithBounds(minPollInterval, maxPollInterval),
		scheduler.WithName("poll_interval"),
		scheduler.WithRedactedValue())
	if s.Mode == scheduler.ModeExternal {
		return 0, true
	}
	return s.Interval, false
}

// PollIntervalFromFile reads ONLY the poll_interval key from the YAML
// config at path and returns the effective cycle interval (0 = external
// mode), applying the same parse+clamp rules Load uses. Every failure
// (missing file, oversized, invalid YAML) also returns 0: the health
// probe derives its freshness deadline from this and must never fail
// because configuration is absent or malformed — the daemon itself
// surfaces those loudly at startup. Unknown keys are deliberately
// tolerated here (no checkUnknownKeys): strictness is Load's job.
func PollIntervalFromFile(path string) time.Duration {
	data, err := atomicfile.ReadBounded(context.Background(), path, maxConfigBytes)
	if err != nil {
		return 0
	}
	var probe struct {
		PollInterval string `yaml:"poll_interval"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return 0
	}
	interval, external := parseInterval(probe.PollInterval)
	if external {
		return 0
	}
	return interval
}

// SonarrEnabled reports whether a complete Sonarr pair (URL + key) is set.
func (c *Config) SonarrEnabled() bool { return c.SonarrURL != "" && c.SonarrAPIKey != "" }

// RadarrEnabled reports whether a complete Radarr pair (URL + key) is set.
func (c *Config) RadarrEnabled() bool { return c.RadarrURL != "" && c.RadarrAPIKey != "" }

// SonarrWebBase is the base URL for Sonarr report deep-links: the public URL
// when set, else the internal URL. This is why an internal Docker hostname in
// url still yields a browser-usable link when public_url points at the reverse
// proxy - and why leaving public_url empty is fine (links fall back to url).
func (c *Config) SonarrWebBase() string { return cmp.Or(c.SonarrPublicURL, c.SonarrURL) }

// RadarrWebBase is the base URL for Radarr report deep-links (see SonarrWebBase).
func (c *Config) RadarrWebBase() string { return cmp.Or(c.RadarrPublicURL, c.RadarrURL) }

// IndexerConfigured reports whether the Torznab feed has an upstream to
// proxy: at least one Prowlarr Torznab URL is set. It is the single home of
// the feed-enablement decision, shared by config validation (validateIndexer)
// and the composition root.
func (c *Config) IndexerConfigured() bool {
	return c.IndexerNyaaTorznabURL != "" || c.IndexerABTorznabURL != ""
}

// Validate reports the first configuration problem that would stop the app from
// running, or nil when runnable.
func (c *Config) Validate() error {
	if c.RunMode != RunModeDaemon && c.RunMode != RunModeReport {
		// Field-name-only (do not echo the supplied mode): the value may be an
		// expanded ${VAR} secret placed here by a config typo, and this error
		// reaches the startup log.
		return fmt.Errorf("mode must be %q or %q", RunModeDaemon, RunModeReport)
	}
	if err := validateArrPair("sonarr", c.SonarrURL, c.SonarrAPIKey); err != nil {
		return err
	}
	if err := validateArrPair("radarr", c.RadarrURL, c.RadarrAPIKey); err != nil {
		return err
	}
	if !c.SonarrEnabled() && !c.RadarrEnabled() {
		return errors.New("no arr configured: enable sonarr and/or radarr with a url + api_key")
	}
	// public_url only feeds report deep-links, so a malformed value warns (the
	// links will be broken) but still loads; a hard rejection would newly reject
	// configs that load today.
	for _, pu := range []struct{ name, val string }{
		{"sonarr.public_url", c.SonarrPublicURL},
		{"radarr.public_url", c.RadarrPublicURL},
	} {
		if err := validateHTTPURL(pu.name, pu.val); err != nil {
			slog.Warn("public_url is malformed; report deep-links will be broken",
				"error", err)
		}
	}
	return c.validateIndexer()
}

// validateIndexer rejects an enabled Torznab feed with no feed API key. The
// feed is the only HTTP surface; it authenticates callers by the apikey query
// param against IndexerAPIKey, so an empty key would leave it unauthenticated
// (and able to leak the AnimeBytes passkey embedded in synthesized RSS download
// links). The feed is enabled when either upstream Torznab URL is set; a
// no-indexer config is unaffected.
func (c *Config) validateIndexer() error {
	if !c.IndexerConfigured() {
		return nil
	}
	if c.IndexerAPIKey == "" {
		return errors.New("indexer.feed_api_key is required when indexer.nyaa_torznab_url or indexer.ab_torznab_url is set")
	}
	// Presence is required above; strength is warn-only defense-in-depth. The
	// key is the only gate on the passkey-bearing /ab feed, so a trivially
	// guessable hand-typed key deserves a config-time signal without rejecting
	// a config that runs today. Field-name-only (never echo the key).
	if len(c.IndexerAPIKey) < 16 {
		slog.Warn("indexer.feed_api_key is shorter than 16 characters; it gates the " +
			"AnimeBytes-passkey-bearing feed - generate a strong key (openssl rand -hex 16)")
	}
	if err := validateHTTPURL("indexer.nyaa_torznab_url", c.IndexerNyaaTorznabURL); err != nil {
		return err
	}
	if err := validateHTTPURL("indexer.ab_torznab_url", c.IndexerABTorznabURL); err != nil {
		return err
	}
	// The header-based Prowlarr key posture (X-Api-Key, never in a logged URL)
	// is defeated when the operator pastes a Jackett-style URL with an embedded
	// credential: upstream failures log the request URL, shipping the pasted
	// key to the WARN log on every failed search. Warn field-name-only (never
	// echo the URL), matching the public_url warn-only posture.
	for _, tu := range []struct{ name, val string }{
		{"indexer.nyaa_torznab_url", c.IndexerNyaaTorznabURL},
		{"indexer.ab_torznab_url", c.IndexerABTorznabURL},
	} {
		if urlEmbedsCredential(tu.val) {
			slog.Warn("torznab url embeds a credential-like query parameter or userinfo; "+
				"move the key to indexer.prowlarr_api_key (sent as a header, never logged) "+
				"or it will appear in upstream-failure logs",
				"field", tu.name)
		}
	}
	// A search proxies Prowlarr using indexer.prowlarr_api_key in the X-Api-Key
	// header. An empty key is accepted rather than rejected (it is valid when
	// Prowlarr has auth "Disabled for Local Addresses"), but the common case is a
	// misconfiguration: Prowlarr then returns 401 for every search and the feed
	// silently serves nothing from a search. Warn so the operator gets a
	// config-time signal without breaking the legitimate no-auth deployment.
	if c.IndexerProwlarrAPIKey == "" {
		slog.Warn("indexer.prowlarr_api_key is empty; searches proxy Prowlarr with no API key and " +
			"will fail (401) unless Prowlarr auth is disabled for local addresses")
	}
	return nil
}

// validateArrPair rejects a half-configured enabled arr (a URL with no key or a
// URL that is not an absolute http(s) URL with a host).
func validateArrPair(name, rawURL, key string) error {
	switch {
	case rawURL == "" && key == "":
		return nil
	case rawURL == "":
		return fmt.Errorf("%s.api_key is set but %s.url is empty", name, name)
	case key == "":
		return fmt.Errorf("%s.url is set but %s.api_key is empty", name, name)
	}
	return validateHTTPURL(name+".url", rawURL)
}

// validateHTTPURL rejects a non-empty rawURL that is not an absolute http(s) URL
// with a host; an empty rawURL passes (the caller decides whether the field is
// required). Shared by the arr-pair and indexer Torznab-URL validators so a
// malformed URL fails at config load rather than at first request.
func validateHTTPURL(name, rawURL string) error {
	if rawURL == "" {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		// Do not wrap err: url.Error embeds the full raw URL (and any userinfo),
		// which would ship an embedded basic-auth password to the startup log.
		return fmt.Errorf("%s is not a valid URL", name)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		// Field-name-only, matching the parse-error branch: u.Redacted() masks only
		// a userinfo password, so echoing the URL would still ship a username-only
		// token or a query-string apikey to the startup log.
		return fmt.Errorf("%s must be an absolute http(s) URL with a host", name)
	}
	return nil
}

// urlEmbedsCredential reports whether rawURL carries a credential in userinfo
// or a credential-like query parameter (apikey/api_key/passkey/token). Such a
// URL survives validation but leaks the credential to upstream-failure logs,
// which wrap the full request URL; validateIndexer warns on it field-name-only.
func urlEmbedsCredential(rawURL string) bool {
	if rawURL == "" {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if u.User != nil {
		return true
	}
	for k := range u.Query() {
		switch strings.ToLower(k) {
		case "apikey", "api_key", "passkey", "token":
			return true
		}
	}
	return false
}

// isAllowedEnvVar reports whether an env var name is safe to expand in the
// config: only the app's own SONARR_*, RADARR_*, and SEADEX_SCOUT_* names, so a
// stray ${HOME} or ${PATH} in the file is left literal. It is the allowlist
// policy Load hands to yamlenv.Expand (the shared post-parse, string-values-only
// expansion engine).
func isAllowedEnvVar(key string) bool {
	return strings.HasPrefix(key, "SONARR_") ||
		strings.HasPrefix(key, "RADARR_") ||
		strings.HasPrefix(key, "SEADEX_SCOUT_")
}

// trimList trims entries and drops blanks, preserving order and case.
func trimList(items []string) []string {
	var out []string
	for _, s := range items {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// parseLogFormat normalizes log.format via slogx.ParseFormat into the typed
// slogx.Format the logger setup consumes, warning on an unrecognized value and
// falling back to JSON (the same diagnostic parseLogLevel gives log.level).
func parseLogFormat(s string) slogx.Format {
	f, ok := slogx.ParseFormat(s, slogx.JSON)
	if !ok {
		// Field-name-only: the rejected value may be an expanded ${VAR} secret
		// placed here by a config typo and must never reach the startup log.
		slog.Warn("unrecognized log.format; defaulting to json")
	}
	return f
}

// parseLogLevel converts a level string to slog.Level via slogx.ParseLevel
// (case-insensitive, trims, accepts the long-form "warning" alias and slog
// offset syntax), falling back to Info for an empty or unrecognized value.
func parseLogLevel(s string) slog.Level {
	// ParseLevel returns ok=true for an empty value (an unset level is not an
	// error), so ok=false is specifically a non-empty unrecognized level worth a
	// warning rather than a silent fallback to Info.
	lvl, ok := slogx.ParseLevel(s, slog.LevelInfo)
	if !ok {
		// Field-name-only: the rejected value may be an expanded ${VAR} secret
		// placed here by a config typo and must never reach the startup log.
		slog.Warn("unrecognized log.level; defaulting to info")
	}
	return lvl
}
