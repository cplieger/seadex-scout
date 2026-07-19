// Package config loads seadex-scout configuration from a single YAML file
// (default /config/config.yaml). The file is the whole settings surface;
// string values may reference SONARR_*, RADARR_*, or SEADEX_SCOUT_* environment
// variables via ${VAR} expansion, so secrets can stay in an .env or Docker
// secret rather than in the file.
//
// The file exposes only user-facing settings (arrs, mode, schedule, filters,
// arr_tags, report dir, logging, the indexer feed). Internal machinery - the
// upstream endpoints, the politeness/refresh/rate cadences, the indexer bind
// address, and the internal /config file paths (state, overrides, feed
// snapshot) - are fixed package constants, not file keys. The on-disk shape
// (fileConfig) is loaded onto a defaults baseline, ${VAR}-expanded, then
// flattened into the runtime Config the rest of the app reads. Call Validate
// to check the result is runnable. There is no hot reload: the file is read
// once at startup.
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
	"strconv"
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

// Fixed endpoints, cadences, internal /config file paths, and the default
// report directory. These are internal machinery wired at build time,
// deliberately NOT exposed as config-file keys: the user should never need to
// point the app at a different SeaDex/Fribb/AniList, retune the politeness
// delays, or relocate the state, overrides, or feed-snapshot files (everything
// lives under the single /config mount). DefaultReportDir is the one
// configurable baseline here: report.dir overrides it.
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
	// indexer's materialized feed to (the search curation set, the synthesized
	// per-tracker RSS journals with their seen ledger, and the harvested-title
	// cache) and the indexer HTTP server reads. One
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
	// SonarrWanted / RadarrWanted record the file's enabled toggles so
	// Validate can reject an enabled arr left with neither url nor api_key.
	SonarrWanted bool
	RadarrWanted bool
}

// Load reads, ${VAR}-expands, and parses the YAML config at path into the
// runtime Config. It returns an error on a missing/oversized file, invalid
// YAML, a file containing more than one YAML document, or an unknown
// configuration key (a misspelled or misplaced key fails loudly at startup
// rather than being silently ignored); call Validate for semantic checks.
func Load(path string) (Config, error) {
	doc, refs, data, err := loadExpandedDoc(path)
	if err != nil {
		if data == nil {
			return Config{}, fmt.Errorf("read config %s: %w", path, err)
		}
		// Same fail-closed sanitizer as the decode errors below: a parse error
		// can embed operator-written text adjacent to a secret (e.g. an
		// unquoted literal secret read as an alias yields "unknown anchor
		// '<secret>' referenced"), and main logs this error at startup.
		return Config{}, fmt.Errorf("parse config %s: %w", path, sanitizeYAMLError(err))
	}
	if err := checkSingleDocument(data); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := checkUnknownKeys(data); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, sanitizeYAMLError(err))
	}
	if len(refs) > 0 {
		slog.Warn("config references environment variables that are not set; "+
			"the literal ${VAR} is kept and will likely fail authentication",
			"vars", strings.Join(refs, ","))
	}
	fc := defaultFileConfig()
	if err := doc.Decode(&fc); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, sanitizeYAMLError(err))
	}
	return fc.toConfig(), nil
}

// loadExpandedDoc reads the bounded config file at path and applies the
// allowlisted ${VAR} expansion, returning the expanded document node, the
// unresolved allowlisted refs, and the raw pre-expansion bytes (nil raw
// signals a read failure; non-nil raw alongside an error signals a parse
// failure). It is the single home of the read+expand pipeline shared by Load
// (strict) and PollIntervalFromFile (best-effort).
func loadExpandedDoc(path string) (doc *yaml.Node, refs []string, raw []byte, err error) {
	// Read through the shared atomicfile bounded reader (the same primitive
	// writeStarterConfig and internal/state use), which enforces the size cap and
	// returns the atomicfile.ErrFileTooLarge sentinel on an oversized file.
	// Config load is a synchronous startup step with no cancellation point, so it
	// passes context.Background(), matching writeStarterConfig.
	raw, err = atomicfile.ReadBounded(context.Background(), path, maxConfigBytes)
	if err != nil {
		return nil, nil, nil, err
	}
	var node yaml.Node
	if err := yaml.Unmarshal(raw, &node); err != nil {
		return nil, nil, raw, err
	}
	refs = yamlenv.Expand(&node, isAllowedEnvVar)
	return &node, refs, raw, nil
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

// checkSingleDocument rejects a config file containing more than one YAML
// document: Load's yaml.Unmarshal and checkUnknownKeys both consume only the
// first document, so everything below a stray "---" separator would otherwise
// be silently ignored — the opposite of the loader's fail-loud posture. Like
// checkUnknownKeys it runs on the raw pre-expansion bytes (expansion is
// post-parse and string-values only, so it cannot change how many documents
// exist). The error is fully static — it embeds no file content — so the Load
// call site returns it without sanitizeYAMLError.
func checkSingleDocument(data []byte) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		// Empty or unparseable first document: the parse and decode steps own
		// those diagnostics; this check owns only document multiplicity.
		return nil
	}
	if err := dec.Decode(&doc); !errors.Is(err, io.EOF) {
		// Anything but EOF — a second document (even the empty one a trailing
		// separator produces) or a syntax error inside it — means the file
		// carries content beyond the first document.
		return errors.New("config contains multiple YAML documents; remove the '---' separator")
	}
	return nil
}

// sanitizeYAMLError rewrites a yaml parse/decode error via
// yamlenv.SanitizeDecodeError so an expanded secret never reaches the startup
// log; line numbers and target types are kept (field-name-only, the same
// posture as validateHTTPURL errors). The decode runs after ${VAR} expansion,
// so the excerpt yaml.v3 embeds can carry a prefix of an expanded secret (an
// api key placed in a non-string field by a config typo); the library rebuilds
// each *yaml.TypeError entry from its value-independent structure and
// withholds anything it cannot prove value-free. The one policy choice made
// here is WithUnknownKeyEcho: the unknown-key name from the strict
// checkUnknownKeys pre-decode is kept — it IS the diagnostic the operator
// needs to fix a typo, and that pre-decode runs on the pre-expansion bytes so
// the name cannot carry an expanded secret — while duplicate-key names and
// scalar excerpts stay redacted per the library default.
func sanitizeYAMLError(err error) error {
	return yamlenv.SanitizeDecodeError(err, yamlenv.WithUnknownKeyEcho())
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
		SonarrWanted:          fc.Sonarr.Enabled,
		RadarrWanted:          fc.Radarr.Enabled,
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
// mode), applying the same allowlisted ${VAR} expansion and the same
// parse+clamp rules Load uses (so an env-referenced interval yields the
// expanded value, not the literal ${VAR}). Every failure (missing file,
// oversized, invalid YAML) also returns 0: the health probe derives its
// freshness deadline from this and must never fail because configuration
// is absent or malformed — the daemon itself surfaces those loudly at
// startup. Unknown keys and extra YAML documents are deliberately tolerated
// here (no checkUnknownKeys / checkSingleDocument): strictness is Load's job.
func PollIntervalFromFile(path string) time.Duration {
	doc, _, _, err := loadExpandedDoc(path)
	if err != nil {
		return 0
	}
	var probe struct {
		PollInterval string `yaml:"poll_interval"`
	}
	if err := doc.Decode(&probe); err != nil {
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
	if err := validateRunMode(c.RunMode); err != nil {
		return err
	}
	if err := validateArrPair("sonarr", c.SonarrURL, c.SonarrAPIKey); err != nil {
		return err
	}
	if err := validateArrPair("radarr", c.RadarrURL, c.RadarrAPIKey); err != nil {
		return err
	}
	c.warnArrURLCredentials()
	if err := c.validateEnabledArrs(); err != nil {
		return err
	}
	c.warnMalformedPublicURLs()
	return c.validateIndexer()
}

// validateRunMode rejects an unknown run mode. Field-name-only (do not echo
// the supplied mode): the value may be an expanded ${VAR} secret placed here
// by a config typo, and this error reaches the startup log.
func validateRunMode(mode string) error {
	if mode != RunModeDaemon && mode != RunModeReport {
		return fmt.Errorf("mode must be %q or %q", RunModeDaemon, RunModeReport)
	}
	return nil
}

// validateEnabledArrs rejects an explicitly enabled arr with no connection
// details at all, and a config that enables no arr whatsoever.
func (c *Config) validateEnabledArrs() error {
	if c.SonarrWanted && c.SonarrURL == "" && c.SonarrAPIKey == "" {
		return errors.New("sonarr.enabled is true but sonarr.url and sonarr.api_key are both empty")
	}
	if c.RadarrWanted && c.RadarrURL == "" && c.RadarrAPIKey == "" {
		return errors.New("radarr.enabled is true but radarr.url and radarr.api_key are both empty")
	}
	if !c.SonarrEnabled() && !c.RadarrEnabled() {
		return errors.New("no arr configured: enable sonarr and/or radarr with a url + api_key")
	}
	return nil
}

// warnMalformedPublicURLs warns on a malformed public_url. public_url only
// feeds report deep-links, so a malformed value warns (the links will be
// broken) but still loads; a hard rejection would newly reject configs that
// load today.
func (c *Config) warnMalformedPublicURLs() {
	for _, pu := range []struct{ name, val string }{
		{"sonarr.public_url", c.SonarrPublicURL},
		{"radarr.public_url", c.RadarrPublicURL},
	} {
		if err := validateHTTPURL(pu.name, pu.val); err != nil {
			slog.Warn("public_url is malformed; report deep-links will be broken",
				"error", err)
		}
	}
}

// warnArrURLCredentials warns (field-name-only, never echoing the URL)
// when an arr url embeds a credential-like userinfo or query parameter,
// which would otherwise leak into a library-walk-failure *url.Error log.
// Mirrors the torznab-URL gate in validateIndexer.
func (c *Config) warnArrURLCredentials() {
	for _, au := range []struct{ name, val string }{
		{"sonarr.url", c.SonarrURL},
		{"radarr.url", c.RadarrURL},
	} {
		if urlEmbedsCredential(au.val) {
			slog.Warn("arr url embeds a credential-like query parameter or userinfo; "+
				"the api key belongs in api_key (sent as a header, never logged) "+
				"or it will appear in library-walk-failure logs",
				"field", au.name)
		}
	}
}

// validateIndexer rejects an enabled Torznab feed with no feed API key. The
// feed is the only HTTP surface; it authenticates callers by the apikey query
// param against IndexerAPIKey, so an empty key would leave it unauthenticated
// (and able to leak the AnimeBytes passkey embedded in synthesized RSS download
// links). The feed is enabled when either upstream Torznab URL is set; a
// no-indexer config is unaffected.
func (c *Config) validateIndexer() error {
	if !c.IndexerConfigured() {
		c.infoDisabledIndexerKeys()
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
	// The /ab RSS feed builds its download links from indexer.ab_passkey; a
	// stable AB-URL-without-passkey config makes that endpoint return a
	// Torznab <error> on every arr RSS check while searches (Prowlarr-proxied,
	// passkey-free) keep working. Warn at startup so the operator gets a
	// config-time signal instead of discovering it in downstream arr RSS
	// failures. Field-name-only; never echoes a secret.
	if c.IndexerABTorznabURL != "" && c.IndexerABPasskey == "" {
		slog.Warn("indexer.ab_passkey is empty; AnimeBytes searches still work through Prowlarr, " +
			"but the /ab RSS feed returns a Torznab error until a passkey is configured")
	}
	c.warnTorznabURLCredentials()
	// A search proxies Prowlarr using indexer.prowlarr_api_key in the X-Api-Key
	// header. An empty key is accepted rather than rejected (it is valid when
	// Prowlarr has auth "Disabled for Local Addresses"), but the common case is a
	// misconfiguration: Prowlarr then answers 401 for every proxied search, and
	// the feed reports each one to the arr as a Torznab <error code="900">
	// document (upstream query failed) rather than results. Warn so the operator
	// gets a config-time signal without breaking the legitimate no-auth
	// deployment.
	if c.IndexerProwlarrAPIKey == "" {
		slog.Warn("indexer.prowlarr_api_key is empty; searches proxy Prowlarr with no API key - " +
			"unless Prowlarr auth is disabled for local addresses they fail upstream (401) and " +
			"every search answers the arr with a Torznab <error code=\"900\"> instead of results")
	}
	return nil
}

// infoDisabledIndexerKeys emits the half-configuration signal for indexer
// secrets set with no torznab URL, mirroring the disabled-arr-with-key Info
// in toConfig: indexer secrets are always operator-written, so keys without a
// torznab URL almost always mean the operator expected the feed to start.
// Info, not Warn - deliberately parked keys must not raise Loki alert noise.
func (c *Config) infoDisabledIndexerKeys() {
	if c.IndexerAPIKey != "" || c.IndexerProwlarrAPIKey != "" || c.IndexerABPasskey != "" {
		slog.Info("indexer keys are set but no torznab url is configured; " +
			"the Torznab feed will not start (set indexer.nyaa_torznab_url and/or indexer.ab_torznab_url)")
	}
}

// warnTorznabURLCredentials warns (field-name-only, never echoing the URL)
// when a torznab url embeds a credential-like userinfo or query parameter.
// The header-based Prowlarr key posture (X-Api-Key, never in a logged URL)
// is defeated when the operator pastes a Jackett-style URL with an embedded
// credential: upstream failures log the request URL, shipping the pasted
// key to the WARN log on every failed search. Matches the public_url
// warn-only posture.
func (c *Config) warnTorznabURLCredentials() {
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
	// url.Parse accepts URI shapes the base-URL consumers cannot use: a
	// fragment survives the parse but is never sent over HTTP (and the Torznab
	// search path appends its query params after it, so the upstream would see
	// no parameters at all), and an out-of-range port passes parsing but fails
	// every later dial. Both must fail at startup, not at first request.
	// Errors stay field-name-only, matching the branches above.
	if u.Fragment != "" {
		return fmt.Errorf("%s must not contain a URL fragment", name)
	}
	if port := u.Port(); port != "" {
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return fmt.Errorf("%s has an invalid port", name)
		}
	}
	return nil
}

// urlEmbedsCredential reports whether rawURL carries a credential in userinfo
// or a credential-like query parameter (apikey/api_key/passkey/token). Such a
// URL survives validation but leaks the credential to upstream-failure logs,
// which wrap the full request URL; validateIndexer warns on it field-name-only.
// The query is scanned twice: u.Query() matches percent-decoded names in
// well-formed pairs, but drops any malformed pair wholesale (an unescaped ';'
// in "?apikey=SECRET;foo=x" discards the entire pair while the secret stays in
// RawQuery for outgoing requests and logs), so a raw scan splitting on both
// '&' and ';' catches the credential names the parsed scan lost. Both match
// field names only, never values.
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
		if isCredentialParam(k) {
			return true
		}
	}
	for pair := range strings.FieldsFuncSeq(u.RawQuery, func(r rune) bool { return r == '&' || r == ';' }) {
		if name, _, _ := strings.Cut(pair, "="); isCredentialParam(name) {
			return true
		}
	}
	return false
}

// isCredentialParam reports whether a query-parameter name is credential-like
// (apikey/api_key/passkey/token, case-insensitive) — the single key set both
// query scans of urlEmbedsCredential match against.
func isCredentialParam(name string) bool {
	switch strings.ToLower(name) {
	case "apikey", "api_key", "passkey", "token":
		return true
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
