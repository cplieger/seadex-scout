// Package config loads seadex-scout configuration from a single YAML file
// (default /config/config.yaml). The file is the whole settings surface;
// string values may reference SONARR_*, RADARR_*, or SEADEX_SCOUT_* environment
// variables via ${VAR} expansion, so secrets can stay in an .env or Docker
// secret rather than in the file.
//
// The file exposes only user-facing settings (arrs, mode, schedule, filters,
// tags, report dir, logging). Internal machinery - the upstream endpoints, the
// politeness/refresh/rate cadences, and the /config file paths (state,
// overrides, reports) - are fixed package constants, not file keys. The on-disk
// shape (fileConfig) is loaded onto a defaults baseline, ${VAR}-expanded, then
// flattened into the runtime Config the rest of the app reads. Call Validate to
// check the result is runnable. There is no hot reload: the file is read once
// at startup.
package config

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/scheduler"
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
	// DefaultReportDir is the directory report mode writes timestamped report
	// pairs into (report-<UTC timestamp>.md / .json).
	DefaultReportDir = "/config/reports"
	// DefaultIndexerListen is the default LAN bind address for the Torznab feed
	// server (the `indexer` subcommand).
	DefaultIndexerListen = ":9118"
	// DefaultMinResolution is the recommendation resolution floor.
	DefaultMinResolution = "1080p"

	// RunModeDaemon is the default: poll on a schedule and flag better releases.
	RunModeDaemon = "daemon"
	// RunModeReport is the one-shot audit: scan once, write the report, exit.
	RunModeReport = "report"

	// DefaultPollInterval is the gap between compare cycles (also runs on start).
	DefaultPollInterval = 12 * time.Hour
	// DefaultSeaDexPageDelay is the politeness delay between SeaDex pages.
	DefaultSeaDexPageDelay = 2 * time.Second
	// DefaultMappingRefresh is the conditional re-download cadence for the map.
	DefaultMappingRefresh = 24 * time.Hour
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
	Indexer       indexerFile `yaml:"indexer"`
	Log           logFile     `yaml:"log"`
	Report        reportFile  `yaml:"report"`
	PollInterval  string      `yaml:"poll_interval"`
	Mode          string      `yaml:"mode"`
	Radarr        arrFile     `yaml:"radarr"`
	Sonarr        arrFile     `yaml:"sonarr"`
	Tags          tagsFile    `yaml:"tags"`
	Filters       filtersFile `yaml:"filters"`
	SeasonScoping bool        `yaml:"season_scoping"`
}

// indexerFile configures the `indexer` subcommand: a Torznab feed of SeaDex
// releases for Sonarr/Radarr. The feed sources real release data from Prowlarr's
// per-indexer Torznab endpoints (Nyaa + AnimeBytes) and filters them to SeaDex's
// curation, so no tracker credentials live here - only the Prowlarr API key.
type indexerFile struct {
	Listen         string `yaml:"listen"`
	APIKey         string `yaml:"api_key"`
	NyaaTorznabURL string `yaml:"nyaa_torznab_url"`
	ABTorznabURL   string `yaml:"ab_torznab_url"`
	ProwlarrAPIKey string `yaml:"prowlarr_api_key"`
}

type arrFile struct {
	URL       string `yaml:"url"`
	APIKey    string `yaml:"api_key"`
	PublicURL string `yaml:"public_url"`
	Enabled   bool   `yaml:"enabled"`
}

type filtersFile struct {
	MinResolution    string   `yaml:"min_resolution"`
	RemuxGroups      []string `yaml:"remux_groups"`
	AllowRemux       bool     `yaml:"allow_remux"`
	RequireDualAudio bool     `yaml:"require_dual_audio"`
	AnimeBytes       bool     `yaml:"animebytes"`
	IncludeSpecials  bool     `yaml:"include_specials"`
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
// keep these values, so a partial config still runs.
func defaultFileConfig() fileConfig {
	return fileConfig{
		Sonarr: arrFile{URL: "http://sonarr:8989"},
		Radarr: arrFile{URL: "http://radarr:7878"},
		Mode:   RunModeDaemon,
		Filters: filtersFile{
			MinResolution:   DefaultMinResolution,
			IncludeSpecials: true,
		},
		Report:       reportFile{Dir: DefaultReportDir},
		Indexer:      indexerFile{Listen: DefaultIndexerListen},
		Log:          logFile{Level: "info", Format: "json"},
		PollInterval: "12h",
	}
}

// Config is the effective runtime configuration after loading. It holds only
// the user-configurable settings; the fixed endpoints, cadences, and /config
// file paths are package constants (see the const block), wired in build.go.
// Fields are ordered largest-alignment-first for govet fieldalignment.
type Config struct {
	// RemuxGroups pins release groups treated as remux, keyed lowercase.
	RemuxGroups map[string]bool

	RunMode   string // "daemon" (default) or "report" (one-shot audit).
	ReportDir string // directory for timestamped report-<ts>.md / .json pairs.

	SonarrURL       string // Sonarr instance URL the app queries.
	SonarrAPIKey    string
	SonarrPublicURL string // browser URL for deep-links; falls back to SonarrURL.
	RadarrURL       string
	RadarrAPIKey    string
	RadarrPublicURL string

	MinResolution string
	LogFormat     string

	// Indexer (Torznab feed) settings. IndexerAPIKey/IndexerProwlarrAPIKey are
	// secrets and are never logged. The feed proxies Prowlarr's per-indexer
	// Torznab endpoints for Nyaa and AnimeBytes.
	IndexerListen         string
	IndexerAPIKey         string
	IndexerNyaaTorznabURL string
	IndexerABTorznabURL   string
	IndexerProwlarrAPIKey string

	IncludeTags []string
	ExcludeTags []string

	PollInterval time.Duration
	LogLevel     slog.Level

	AllowRemux       bool
	RequireDualAudio bool
	SeasonScoping    bool
	// AnimeBytes includes AnimeBytes (private tracker) releases and links; the
	// public trackers (Nyaa, AnimeTosho, RuTracker) are always included.
	AnimeBytes      bool
	IncludeSpecials bool
	// PollExternal is set when poll_interval is off/disabled/0: no internal
	// timer, cycles are triggered out-of-band via the `poll` subcommand.
	PollExternal bool
}

// Load reads, ${VAR}-expands, and parses the YAML config at path into the
// runtime Config. It returns an error on a missing/oversized file or invalid
// YAML; call Validate for semantic checks.
func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config %s: %w", path, err)
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	if len(data) > maxConfigBytes {
		return Config{}, fmt.Errorf("config %s exceeds %d bytes", path, maxConfigBytes)
	}

	expanded := os.Expand(string(data), expandEnvSafe)
	fc := defaultFileConfig()
	if err := yaml.Unmarshal([]byte(expanded), &fc); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	return fc.toConfig(), nil
}

// toConfig flattens the on-disk shape into the runtime Config, applying
// normalization and the enabled toggles (a disabled arr leaves its URL/key
// empty, so it is simply skipped downstream).
func (fc *fileConfig) toConfig() Config {
	c := Config{
		RemuxGroups:           setFromList(fc.Filters.RemuxGroups),
		RunMode:               strings.ToLower(strings.TrimSpace(fc.Mode)),
		ReportDir:             strings.TrimSpace(fc.Report.Dir),
		MinResolution:         normalizeResolution(fc.Filters.MinResolution),
		LogFormat:             strings.ToLower(strings.TrimSpace(fc.Log.Format)),
		IncludeTags:           trimList(fc.Tags.Include),
		ExcludeTags:           trimList(fc.Tags.Exclude),
		LogLevel:              parseLogLevel(fc.Log.Level),
		AllowRemux:            fc.Filters.AllowRemux,
		RequireDualAudio:      fc.Filters.RequireDualAudio,
		SeasonScoping:         fc.SeasonScoping,
		AnimeBytes:            fc.Filters.AnimeBytes,
		IncludeSpecials:       fc.Filters.IncludeSpecials,
		IndexerListen:         cmp.Or(strings.TrimSpace(fc.Indexer.Listen), DefaultIndexerListen),
		IndexerAPIKey:         strings.TrimSpace(fc.Indexer.APIKey),
		IndexerNyaaTorznabURL: strings.TrimSpace(fc.Indexer.NyaaTorznabURL),
		IndexerABTorznabURL:   strings.TrimSpace(fc.Indexer.ABTorznabURL),
		IndexerProwlarrAPIKey: strings.TrimSpace(fc.Indexer.ProwlarrAPIKey),
	}
	if fc.Sonarr.Enabled {
		c.SonarrURL = strings.TrimSpace(fc.Sonarr.URL)
		c.SonarrAPIKey = strings.TrimSpace(fc.Sonarr.APIKey)
		c.SonarrPublicURL = strings.TrimSpace(fc.Sonarr.PublicURL)
	}
	if fc.Radarr.Enabled {
		c.RadarrURL = strings.TrimSpace(fc.Radarr.URL)
		c.RadarrAPIKey = strings.TrimSpace(fc.Radarr.APIKey)
		c.RadarrPublicURL = strings.TrimSpace(fc.Radarr.PublicURL)
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
// the default with a warning.
func parseInterval(raw string) (time.Duration, bool) {
	s := scheduler.ParseInterval(raw, DefaultPollInterval,
		scheduler.WithBounds(minPollInterval, maxPollInterval),
		scheduler.WithName("poll_interval"))
	if s.Mode == scheduler.ModeExternal {
		return 0, true
	}
	return s.Interval, false
}

// SonarrEnabled reports whether a complete Sonarr pair (URL + key) is set.
func (c *Config) SonarrEnabled() bool { return c.SonarrURL != "" && c.SonarrAPIKey != "" }

// RadarrEnabled reports whether a complete Radarr pair (URL + key) is set.
func (c *Config) RadarrEnabled() bool { return c.RadarrURL != "" && c.RadarrAPIKey != "" }

// SonarrWebBase is the base URL for Sonarr report deep-links: the public URL
// when set, else the internal URL.
func (c *Config) SonarrWebBase() string { return cmp.Or(c.SonarrPublicURL, c.SonarrURL) }

// RadarrWebBase is the base URL for Radarr report deep-links.
func (c *Config) RadarrWebBase() string { return cmp.Or(c.RadarrPublicURL, c.RadarrURL) }

// Validate reports the first configuration problem that would stop the app from
// running, or nil when runnable.
func (c *Config) Validate() error {
	if c.RunMode != RunModeDaemon && c.RunMode != RunModeReport {
		return fmt.Errorf("mode must be %q or %q, got %q", RunModeDaemon, RunModeReport, c.RunMode)
	}
	if err := c.validateArrPair("sonarr", c.SonarrURL, c.SonarrAPIKey); err != nil {
		return err
	}
	if err := c.validateArrPair("radarr", c.RadarrURL, c.RadarrAPIKey); err != nil {
		return err
	}
	if !c.SonarrEnabled() && !c.RadarrEnabled() {
		return errors.New("no arr configured: enable sonarr and/or radarr with a url + api_key")
	}
	return nil
}

// validateArrPair rejects a half-configured enabled arr (a URL with no key or a
// URL that is not an absolute http(s) URL with a host).
func (c *Config) validateArrPair(name, rawURL, key string) error {
	switch {
	case rawURL == "" && key == "":
		return nil
	case rawURL == "":
		return fmt.Errorf("%s.api_key is set but %s.url is empty", name, name)
	case key == "":
		return fmt.Errorf("%s.url is set but %s.api_key is empty", name, name)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%s.url is not a valid URL: %w", name, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s.url must be an absolute http(s) URL with a host, got %q", name, rawURL)
	}
	return nil
}

// isAllowedEnvVar reports whether an env var name is safe to expand in the
// config: only the app's own SONARR_*, RADARR_*, and SEADEX_SCOUT_* names, so a
// stray ${HOME} or ${PATH} in the file is left literal.
func isAllowedEnvVar(key string) bool {
	return strings.HasPrefix(key, "SONARR_") ||
		strings.HasPrefix(key, "RADARR_") ||
		strings.HasPrefix(key, "SEADEX_SCOUT_")
}

// expandEnvSafe expands an allowlisted, set env var, leaving anything else as
// the literal ${key} so os.Expand does not blank out unknown references.
func expandEnvSafe(key string) string {
	if !isAllowedEnvVar(key) {
		return "${" + key + "}"
	}
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return "${" + key + "}"
}

// setFromList builds a lowercase-keyed set for case-insensitive membership,
// dropping blanks. Empty input yields an empty set ("no restriction").
func setFromList(items []string) map[string]bool {
	out := make(map[string]bool)
	for _, s := range items {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out[s] = true
		}
	}
	return out
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

// normalizeResolution lowercases/trims a resolution and appends "p" to a bare
// height ("1080" -> "1080p"); "", "all", "none" disable the floor.
func normalizeResolution(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "", "all", "none":
		return ""
	}
	if _, err := strconv.Atoi(s); err == nil {
		return s + "p"
	}
	return s
}

// parseLogLevel converts a level string to slog.Level via slogx.ParseLevel
// (case-insensitive, trims, accepts the long-form "warning" alias and slog
// offset syntax), falling back to Info for an empty or unrecognized value.
func parseLogLevel(s string) slog.Level {
	lvl, _ := slogx.ParseLevel(s, slog.LevelInfo)
	return lvl
}
