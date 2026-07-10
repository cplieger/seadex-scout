// Package config loads seadex-scout configuration from a single YAML file
// (default /config/config.yaml). The file is the whole settings surface;
// string values may reference SONARR_*, RADARR_*, or SEADEX_SCOUT_* environment
// variables via ${VAR} expansion, so secrets can stay in an .env or Docker
// secret rather than in the file.
//
// The on-disk shape (fileConfig) is a grouped, commented document; it is loaded
// onto a defaults baseline, ${VAR}-expanded, then flattened into the runtime
// Config the rest of the app reads. Call Validate to check the result is
// runnable. There is no hot reload: the file is read once at startup.
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

// Default endpoints, paths, and cadences applied before the YAML overlay.
const (
	// DefaultSeaDexBaseURL is the SeaDex (releases.moe) API base.
	DefaultSeaDexBaseURL = "https://releases.moe"
	// DefaultMappingURL is the Fribb anime-lists AniList<->arr ID bridge.
	DefaultMappingURL = "https://raw.githubusercontent.com/Fribb/anime-lists/master/anime-list-mini.json"
	// DefaultAniListURL is the AniList GraphQL endpoint (title/format fallback).
	DefaultAniListURL = "https://graphql.anilist.co"
	// DefaultMappingOverrides is the local alID->IDs override file.
	DefaultMappingOverrides = "/config/overrides.json"
	// DefaultStatePath is the atomic JSON cache/state file.
	DefaultStatePath = "/config/state.json"
	// DefaultReportPath is where report mode writes the Markdown report.
	DefaultReportPath = "/config/report.md"
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

// Clamp bounds guarding against nonsense configuration.
const (
	minPollInterval    = time.Hour
	maxPollInterval    = 30 * 24 * time.Hour
	maxSeaDexPageDelay = 60 * time.Second
	minMappingRefresh  = time.Hour
	maxMappingRefresh  = 30 * 24 * time.Hour
	minAniListRate     = 1
	maxAniListRate     = 90
)

// Duration is a YAML string duration ("12h", "2s") decoded via time.ParseDuration.
type Duration struct {
	D time.Duration
}

// UnmarshalYAML decodes a duration string into D.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.D = parsed
	return nil
}

// fileConfig is the on-disk YAML shape.
type fileConfig struct {
	Log           logFile     `yaml:"log"`
	PollInterval  string      `yaml:"poll_interval"`
	StatePath     string      `yaml:"state_path"`
	Report        reportFile  `yaml:"report"`
	Mode          string      `yaml:"mode"`
	Radarr        arrFile     `yaml:"radarr"`
	Sonarr        arrFile     `yaml:"sonarr"`
	Tags          tagsFile    `yaml:"tags"`
	Mapping       mappingFile `yaml:"mapping"`
	AniList       anilistFile `yaml:"anilist"`
	SeaDex        seadexFile  `yaml:"seadex"`
	Filters       filtersFile `yaml:"filters"`
	SeasonScoping bool        `yaml:"season_scoping"`
}

type arrFile struct {
	URL       string `yaml:"url"`
	APIKey    string `yaml:"api_key"`
	PublicURL string `yaml:"public_url"`
	Enabled   bool   `yaml:"enabled"`
}

type filtersFile struct {
	MinResolution            string   `yaml:"min_resolution"`
	Trackers                 []string `yaml:"trackers"`
	RemuxGroups              []string `yaml:"remux_groups"`
	AllowRemux               bool     `yaml:"allow_remux"`
	RequireDualAudio         bool     `yaml:"require_dual_audio"`
	NotifyUnavailableTracker bool     `yaml:"notify_unavailable_tracker"`
	IncludeSpecials          bool     `yaml:"include_specials"`
}

type tagsFile struct {
	Include []string `yaml:"include"`
	Exclude []string `yaml:"exclude"`
}

type reportFile struct {
	Path string `yaml:"path"`
}

type seadexFile struct {
	BaseURL   string   `yaml:"base_url"`
	PageDelay Duration `yaml:"page_delay"`
}

type mappingFile struct {
	URL           string   `yaml:"url"`
	OverridesPath string   `yaml:"overrides_path"`
	Refresh       Duration `yaml:"refresh"`
}

type anilistFile struct {
	URL  string `yaml:"url"`
	Rate int    `yaml:"rate"`
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
			MinResolution:            DefaultMinResolution,
			NotifyUnavailableTracker: true,
			IncludeSpecials:          true,
		},
		Report:       reportFile{Path: DefaultReportPath},
		SeaDex:       seadexFile{BaseURL: DefaultSeaDexBaseURL, PageDelay: Duration{D: DefaultSeaDexPageDelay}},
		Mapping:      mappingFile{URL: DefaultMappingURL, OverridesPath: DefaultMappingOverrides, Refresh: Duration{D: DefaultMappingRefresh}},
		AniList:      anilistFile{URL: DefaultAniListURL, Rate: DefaultAniListRate},
		Log:          logFile{Level: "info", Format: "json"},
		StatePath:    DefaultStatePath,
		PollInterval: "12h",
	}
}

// Config is the effective runtime configuration after loading. Fields are
// ordered largest-alignment-first for govet fieldalignment.
type Config struct {
	// Trackers is the preferred-tracker allowlist keyed lowercase; empty = all.
	Trackers map[string]bool
	// RemuxGroups pins release groups treated as remux, keyed lowercase.
	RemuxGroups map[string]bool

	StatePath  string // atomic JSON cache/state file.
	RunMode    string // "daemon" (default) or "report" (one-shot audit).
	ReportPath string // Markdown report path (a .json is written alongside).

	SonarrURL       string // Sonarr instance URL the app queries.
	SonarrAPIKey    string
	SonarrPublicURL string // browser URL for deep-links; falls back to SonarrURL.
	RadarrURL       string
	RadarrAPIKey    string
	RadarrPublicURL string

	SeaDexBaseURL    string
	MappingURL       string
	MappingOverrides string
	MinResolution    string
	LogFormat        string
	AniListURL       string

	IncludeTags []string
	ExcludeTags []string

	PollInterval    time.Duration
	SeaDexPageDelay time.Duration
	MappingRefresh  time.Duration

	AniListRate int
	LogLevel    slog.Level

	AllowRemux               bool
	RequireDualAudio         bool
	SeasonScoping            bool
	NotifyUnavailableTracker bool
	IncludeSpecials          bool
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

// toConfig flattens the on-disk shape into the runtime Config, applying clamps,
// normalization, and the enabled toggles (a disabled arr leaves its URL/key
// empty, so it is simply skipped downstream).
func (fc *fileConfig) toConfig() Config {
	c := Config{
		Trackers:                 setFromList(fc.Filters.Trackers),
		RemuxGroups:              setFromList(fc.Filters.RemuxGroups),
		StatePath:                fc.StatePath,
		RunMode:                  strings.ToLower(strings.TrimSpace(fc.Mode)),
		ReportPath:               fc.Report.Path,
		SeaDexBaseURL:            fc.SeaDex.BaseURL,
		MappingURL:               fc.Mapping.URL,
		MappingOverrides:         fc.Mapping.OverridesPath,
		MinResolution:            normalizeResolution(fc.Filters.MinResolution),
		LogFormat:                strings.ToLower(strings.TrimSpace(fc.Log.Format)),
		AniListURL:               fc.AniList.URL,
		IncludeTags:              trimList(fc.Tags.Include),
		ExcludeTags:              trimList(fc.Tags.Exclude),
		SeaDexPageDelay:          clampDuration("seadex.page_delay", fc.SeaDex.PageDelay.D, 0, maxSeaDexPageDelay),
		MappingRefresh:           clampDuration("mapping.refresh", fc.Mapping.Refresh.D, minMappingRefresh, maxMappingRefresh),
		AniListRate:              clampInt("anilist.rate", fc.AniList.Rate, minAniListRate, maxAniListRate),
		LogLevel:                 parseLogLevel(fc.Log.Level),
		AllowRemux:               fc.Filters.AllowRemux,
		RequireDualAudio:         fc.Filters.RequireDualAudio,
		SeasonScoping:            fc.SeasonScoping,
		NotifyUnavailableTracker: fc.Filters.NotifyUnavailableTracker,
		IncludeSpecials:          fc.Filters.IncludeSpecials,
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

// ReportJSONPath returns the sibling JSON path for the Markdown ReportPath
// (report.md -> report.json), else appends ".json".
func (c *Config) ReportJSONPath() string {
	if base, ok := strings.CutSuffix(c.ReportPath, ".md"); ok {
		return base + ".json"
	}
	return c.ReportPath + ".json"
}

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
	for _, e := range []struct{ name, raw string }{
		{"seadex.base_url", c.SeaDexBaseURL},
		{"anilist.url", c.AniListURL},
		{"mapping.url", c.MappingURL},
	} {
		if err := validateHTTPSURL(e.name, e.raw); err != nil {
			return err
		}
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

// validateHTTPSURL requires raw to be an absolute https URL with a host.
func validateHTTPSURL(name, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s is not a valid URL: %w", name, err)
	}
	if u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("%s must be an absolute https URL with a host, got %q", name, raw)
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

// clampDuration clamps d into [lo, hi], logging when it adjusts the value.
func clampDuration(key string, d, lo, hi time.Duration) time.Duration {
	clamped := max(lo, min(d, hi))
	if clamped != d {
		slog.Warn("duration clamped", "key", key, "requested", d.String(), "clamped_to", clamped.String())
	}
	return clamped
}

// clampInt clamps v into [lo, hi], logging when it adjusts the value.
func clampInt(key string, v, lo, hi int) int {
	clamped := max(lo, min(v, hi))
	if clamped != v {
		slog.Warn("value clamped", "key", key, "requested", v, "clamped_to", clamped)
	}
	return clamped
}
