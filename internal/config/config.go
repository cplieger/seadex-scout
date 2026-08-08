// Package config loads seadex-scout configuration from a single YAML file
// (default /config/config.yaml). The file is the whole settings surface;
// string values may reference SONARR_*, RADARR_*, or SEADEX_SCOUT_* environment
// variables via ${VAR} expansion, so secrets can stay in an .env or Docker
// secret rather than in the file.
//
// The file exposes only user-facing settings (arrs, mode, schedule, filters,
// arr_tags, report dir, logging, the indexer feed). Internal machinery - the
// upstream endpoints, the politeness/refresh/rate cadences, and the internal
// /config file paths (state, overrides, feed snapshot) - are fixed package
// constants, not file keys (the indexer bind address is fixed too, in
// internal/indexer). The on-disk shape
// (fileConfig) is loaded onto a defaults baseline, ${VAR}-expanded, then
// flattened into the runtime Config the rest of the app reads. Call Validate
// to check the result is runnable. There is no hot reload: the file is read
// once at startup.
package config

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/envx/yamlenv"
	"github.com/cplieger/scheduler/v2"
	"github.com/cplieger/seadex-scout/internal/credname"
	"github.com/cplieger/seadex-scout/internal/displaylink"
	"github.com/cplieger/seadex-scout/internal/secretref"
	"github.com/cplieger/seadex-scout/internal/tagfilter"
	"github.com/cplieger/slogx"
	"github.com/cplieger/urlform"
)

// DefaultConfigDir is the single container mount every seadex-scout file lives
// under; every path constant below is derived from it, and it is the directory
// the cross-process cycle lock is created in.
const DefaultConfigDir = "/config"

// DefaultConfigPath is the container-internal config file path.
const DefaultConfigPath = DefaultConfigDir + "/config.yaml"

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
	// The SeaDex (releases.moe) site base deliberately has NO constant here:
	// internal/seadex owns the releases.moe contract (seadex.DefaultBaseURL,
	// beside EntryURL/ValidInfoHash), and config - a dependency leaf that
	// cannot import it - must not keep an equal literal that can silently
	// drift. The wiring site (build.go) references the seadex constant.
	// The Fribb and AniList endpoints follow the same rule: internal/mapping
	// and internal/anilist export their own DefaultURL beside the decoders
	// that embody each upstream contract. Each upstream's request cadence is
	// the same class of contract knowledge and lives beside its endpoint too
	// (seadex.DefaultPageDelay, mapping.DefaultRefresh, anilist.DefaultRate),
	// so retuning one upstream touches only the package that knows it.
	// DefaultMappingOverrides is the local alID->IDs override file: drop one in
	// at this path to pin mappings; absent is fine.
	DefaultMappingOverrides = DefaultConfigDir + "/overrides.json"
	// DefaultStatePath is the atomic JSON cache/state file.
	DefaultStatePath = DefaultConfigDir + "/state.json"
	// DefaultCycleLockDir is the directory holding cycle.lock, the cross-process
	// cycle coalescing lock. It is the mount root so the lock lives beside the
	// state.json and feed.json writes it orders (see internal/cycle), rather
	// than depending on where either of those files happens to live.
	DefaultCycleLockDir = DefaultConfigDir
	// DefaultIndexerFeedPath is the atomic JSON file the compare cycle writes the
	// indexer's materialized feed to (the search curation set, the synthesized
	// per-tracker RSS journals with their publication log, and the harvested-title
	// cache) and the indexer HTTP server reads. One
	// data engine (the cycle) produces both the findings and this feed, and
	// persisting it lets a cycle run by the `poll` subcommand refresh a resident
	// daemon's feed across the process boundary.
	DefaultIndexerFeedPath = DefaultConfigDir + "/feed.json"
	// DefaultReportDir is the directory report mode writes timestamped report
	// pairs into (report-<UTC timestamp>.md / .json).
	DefaultReportDir = DefaultConfigDir + "/reports"

	// RunModeDaemon is the default: poll on a schedule and flag better releases.
	RunModeDaemon = "daemon"
	// RunModeReport is the one-shot audit: scan once, write the report, exit.
	RunModeReport = "report"

	// DefaultPollInterval is the loop's own interval. Most iterations are a
	// cheap TICK - one ~88-byte probe plus, when something changed, one request
	// of a few tens of KiB - and every 24h worth of them is a full reconcile
	// (the whole catalogue, the whole arr walk, the whole feed and curation-index
	// rebuild). So this is the FRESHNESS knob, not the cost knob: the upstream
	// load it drives is proportional to the change RATE, not to the interval.
	//
	// 15m follows the consumer rather than a guess. Sonarr's own RSS Sync
	// Interval is 10-120 minutes with a default of 15, so fetching faster than
	// that cannot reach the arrs any sooner. There is no natural knee to pick
	// instead: measured against 90 days of upstream history, the number of
	// productive passes per day is nearly flat across the whole range.
	DefaultPollInterval = 15 * time.Minute
)

// Clamp bounds for poll_interval, the only file-provided duration. The floor is
// the consumer's own floor for the same reason the default follows its default:
// below Sonarr's 10-minute minimum, a shorter interval buys freshness no arr can
// read, while still costing the upstream a probe every time.
const (
	minPollInterval = 15 * time.Minute
	maxPollInterval = 30 * 24 * time.Hour
)

// Bounds on the filters.exclude_tags map, the one file key whose KEYS are
// operator-supplied free text. SeaDex's own curation vocabulary is a handful of
// short words ("Broken", "Incomplete"), so both bounds are generous ceilings
// that only a paste error or a hostile file can reach - the point is that the
// policy map cannot grow unbounded from a 1 MiB config, since every release's
// tags are matched against it on every surface.
const (
	maxExcludeTags   = 32
	maxExcludeTagLen = 64
)

// maxIgnoreIDs bounds filters.ignore. Findings are reported as STATE, so every
// entry in this set is consulted once per finding on every emission pass; a
// generous ceiling keeps a 1 MiB config from turning that into unbounded work,
// and an operator with more than a few hundred deliberately-declined shows has
// a different problem than an alert filter can solve (CWE-400).
const maxIgnoreIDs = 512

// --- On-disk YAML shape and defaults ---

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
	// Filters sits after the pointer-bearing members and before AnimeBytes
	// because filtersFile ends in bools: keeping its non-pointer tail late
	// shortens the struct's GC-scanned prefix (govet fieldalignment).
	Filters filtersFile `yaml:"filters"`
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
	// ExcludeTags maps a SeaDex tag to the recommendation surfaces it is
	// excluded from ("findings", "report", "feed"). Absent or empty means
	// NOTHING is filtered on any surface; see buildTagFilter for the bounds and
	// the surface vocabulary.
	ExcludeTags map[string][]string `yaml:"exclude_tags"`
	// Ignore lists AniList IDs whose findings are never emitted. Absent or
	// empty (the default) reports everything. It suppresses EMISSION only: the
	// report still shows the row and the RSS feed is untouched, because the
	// app's standing rule is that a release is never withheld from the arrs -
	// this withholds an ALERT the operator asked to stop, which is a different
	// thing. See buildIgnoreSet for the bound.
	Ignore           []int `yaml:"ignore"`
	ExcludeRemux     bool  `yaml:"exclude_remux"`
	RequireDualAudio bool  `yaml:"require_dual_audio"`
	ExcludeSpecials  bool  `yaml:"exclude_specials"`
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
// the user-configurable settings; the fixed endpoints, cadences, and /config
// file paths are package constants (see the const block), wired in build.go;
// the indexer bind address is fixed in internal/indexer. Fields are ordered
// largest-alignment-first for govet fieldalignment.
type Config struct {
	// tagFilterErr holds a rejected filters.exclude_tags map (unknown surface,
	// blank or over-long tag key, no surfaces, too many tags). It is recorded at
	// flatten time and returned by Validate, so the single parse happens where
	// the file shape is still in hand and the startup error still stops the app.
	// It leads the struct for fieldalignment, not for prominence.
	tagFilterErr error
	// TagFilter is the filters.exclude_tags policy: which SeaDex tags exclude a
	// release from which recommendation surface. The zero value (the default,
	// and what an absent or empty section yields) filters NOTHING anywhere - a
	// release SeaDex tagged Broken reaches the findings, the report and the
	// feed. It is the one policy all three surfaces read.
	TagFilter tagfilter.Filter
	// IgnoreFindings is the filters.ignore policy: AniList IDs whose findings
	// are never emitted. Nil (the default) reports everything.
	IgnoreFindings map[int]struct{}
	// ignoreErr holds a rejected filters.ignore list, recorded at flatten time
	// and returned by Validate beside tagFilterErr.
	ignoreErr error

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
	// sonarrWanted / radarrWanted record the file's enabled toggles so
	// Validate can reject an enabled arr left with neither url nor api_key.
	sonarrWanted bool
	radarrWanted bool
}

// --- Loading ---

// Load reads, ${VAR}-expands, and parses the YAML config at path into the
// runtime Config. It returns an error on a missing/oversized file, invalid
// YAML, a file containing more than one YAML document, or an unknown
// configuration key (a misspelled or misplaced key fails loudly at startup
// rather than being silently ignored); call Validate for semantic checks.
//
// The strict pipeline is yamlenv.Load: the single-document and unknown-key
// checks on the raw pre-expansion bytes, allowlisted post-parse ${VAR}
// expansion of string scalar values, the decode onto the defaults baseline
// (an empty document keeps it), and fail-closed sanitization of every yaml
// error so an expanded — or pasted literal — secret never reaches the
// startup log. The one policy choice made here is WithUnknownKeyEcho: the
// unknown-key name is kept — it IS the diagnostic the operator needs to fix
// a typo, and the strict probe runs on the pre-expansion bytes so the name
// cannot carry an expanded secret — while duplicate-key names and scalar
// excerpts stay redacted per the library default.
func Load(path string) (Config, error) {
	// The shared atomicfile bounded reader (the same primitive
	// writeStarterConfig and internal/state use) enforces the size cap and
	// returns the atomicfile.ErrFileTooLarge sentinel on an oversized file.
	// Config load is a synchronous startup step with no cancellation point,
	// so it passes context.Background(), matching writeStarterConfig.
	raw, err := atomicfile.ReadBounded(context.Background(), path, maxConfigBytes)
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	warnConfigPermissions(path)
	fc := defaultFileConfig()
	refs, err := yamlenv.Load(raw, &fc, isAllowedEnvVar,
		yamlenv.WithSanitizeOptions(yamlenv.WithUnknownKeyEcho()))
	if len(refs) > 0 {
		slog.Warn("config references environment variables that are not set; "+
			"the literal ${VAR} is kept and will likely fail authentication",
			"vars", strings.Join(refs, ","))
	}
	if err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	return fc.toConfig(), nil
}

// --- Flattening to the runtime Config ---

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
		sonarrWanted:          fc.Sonarr.Enabled,
		radarrWanted:          fc.Radarr.Enabled,
	}
	c.SonarrURL, c.SonarrAPIKey, c.SonarrPublicURL = applyArr("sonarr", fc.Sonarr)
	c.RadarrURL, c.RadarrAPIKey, c.RadarrPublicURL = applyArr("radarr", fc.Radarr)
	if c.ReportDir == "" {
		c.ReportDir = DefaultReportDir
	}
	c.PollInterval, c.PollExternal = parseInterval(fc.PollInterval)
	c.TagFilter, c.tagFilterErr = buildTagFilter(fc.Filters.ExcludeTags)
	c.IgnoreFindings, c.ignoreErr = buildIgnoreSet(fc.Filters.Ignore)
	warnAllBlankTagList("arr_tags.include", fc.ArrTags.Include, c.IncludeTags)
	warnAllBlankTagList("arr_tags.exclude", fc.ArrTags.Exclude, c.ExcludeTags)
	return c
}

// applyArr flattens one arr section: an enabled arr's trimmed connection
// details, or empty strings plus the half-configuration Info signal (a set
// api_key is always operator-written, so a disabled-but-keyed arr almost
// always means the enabled toggle was left off; Info, not Warn, so a
// deliberate temporary disable raises no Loki alert noise).
func applyArr(name string, af arrFile) (arrURL, key, publicURL string) {
	if af.Enabled {
		return strings.TrimSpace(af.URL), strings.TrimSpace(af.APIKey), strings.TrimSpace(af.PublicURL)
	}
	if strings.TrimSpace(af.APIKey) != "" {
		slog.Info("api_key is set but the arr is not enabled; it will not be scanned",
			"field", name+".api_key")
	}
	return "", "", ""
}

// buildIgnoreSet turns filters.ignore into the emission-suppression set,
// rejecting an over-long list and a non-positive AniList ID (SeaDex's own IDs
// start at 1, so a zero or negative entry is a typo that would silently match
// nothing). Duplicates are accepted and collapse, since a set is what the
// caller wants. An empty list yields nil, the same as an absent key: there is
// one unambiguous way to suppress nothing.
func buildIgnoreSet(raw []int) (map[int]struct{}, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > maxIgnoreIDs {
		return nil, fmt.Errorf("filters.ignore lists more than %d AniList IDs", maxIgnoreIDs)
	}
	out := make(map[int]struct{}, len(raw))
	for _, id := range raw {
		if id <= 0 {
			return nil, fmt.Errorf(
				"filters.ignore holds a non-positive AniList ID (%d); IDs start at 1", id)
		}
		out[id] = struct{}{}
	}
	return out, nil
}

// buildTagFilter turns the filters.exclude_tags map into the one tagfilter
// policy every recommendation surface reads, or an error the caller records for
// Validate to return. An absent or empty map yields the zero Filter, which
// filters nothing anywhere - the default.
//
// Four rejections, all hard errors rather than silent no-ops, because each one
// is an operator asking for filtering that would not happen: more tags than
// maxExcludeTags, a blank or over-long tag key, an unknown surface name, and a
// tag listing NO surfaces. The last is a judgement call: `broken: []` reads as
// an intent to filter but means nothing, and the file already has one
// unambiguous way to filter nothing (an empty exclude_tags), so a second,
// contradictory spelling of it is refused. Diagnostics are field-name-only like
// every other config error - never the tag key or the rejected surface - since a
// ${VAR} typo can place an expanded secret in either position.
func buildTagFilter(raw map[string][]string) (tagfilter.Filter, error) {
	if len(raw) == 0 {
		return tagfilter.Filter{}, nil
	}
	if len(raw) > maxExcludeTags {
		return tagfilter.Filter{}, fmt.Errorf(
			"filters.exclude_tags lists more than %d tags", maxExcludeTags)
	}
	valid := strings.Join(tagfilter.SurfaceNames(), ", ")
	bySurface := make(map[string][]tagfilter.Surface, len(raw))
	// Sorted keys keep the reported defect deterministic when a map holds more
	// than one: map iteration order would otherwise pick a different error per
	// run for the same file.
	for _, tag := range slices.Sorted(maps.Keys(raw)) {
		switch key := strings.TrimSpace(tag); {
		case key == "":
			return tagfilter.Filter{}, errors.New(
				"filters.exclude_tags holds a blank tag key")
		case len(key) > maxExcludeTagLen:
			return tagfilter.Filter{}, fmt.Errorf(
				"a filters.exclude_tags tag key is longer than %d characters", maxExcludeTagLen)
		case len(raw[tag]) == 0:
			return tagfilter.Filter{}, fmt.Errorf(
				"a filters.exclude_tags tag lists no surfaces; list at least one of %s, "+
					"or remove the tag (an empty exclude_tags filters nothing)", valid)
		}
		surfaces := make([]tagfilter.Surface, 0, len(raw[tag]))
		for _, name := range raw[tag] {
			s, ok := tagfilter.ParseSurface(name)
			if !ok {
				return tagfilter.Filter{}, fmt.Errorf(
					"filters.exclude_tags lists an unknown surface; valid surfaces are %s", valid)
			}
			surfaces = append(surfaces, s)
		}
		bySurface[tag] = surfaces
	}
	return tagfilter.New(bySurface), nil
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
		scheduler.WithRedactedValue(),
		scheduler.WithIntervalLogger(slog.Default()))
	if s.Mode == scheduler.ModeExternal {
		return 0, true
	}
	return s.Interval, false
}

// --- Accessors ---

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

// --- Validation and diagnostics ---

// Validate reports the first configuration problem that would stop the app from
// running, or nil when runnable. It is deliberately not a pure query: on the way
// through the checks it also emits the config-time diagnostics that need the
// assembled Config (the field-name-only warn/info lines for
// suspicious-but-runnable values), so calling it twice duplicates them and a
// path that skips it loses them. Two limits on that: it stops at the FIRST hard
// error, so a rejected config surfaces only the diagnostics ahead of the check
// that failed (the operator sees the rest after fixing it and restarting), and
// the load-time diagnostics - a config file readable beyond its owner, unresolved
// ${VAR} references, a disabled-but-keyed arr, an all-blank tag list, an
// unrecognized log.level or log.format - are emitted by Load/toConfig, not here.
func (c *Config) Validate() error {
	if err := validateRunMode(c.RunMode); err != nil {
		return err
	}
	// The filters.exclude_tags map and filters.ignore list are parsed once at
	// flatten time (toConfig); this is where a rejection becomes the startup
	// error.
	if c.tagFilterErr != nil {
		return c.tagFilterErr
	}
	if c.ignoreErr != nil {
		return c.ignoreErr
	}
	if err := validateArrPair("sonarr", c.SonarrURL, c.SonarrAPIKey); err != nil {
		return err
	}
	if err := validateArrPair("radarr", c.RadarrURL, c.RadarrAPIKey); err != nil {
		return err
	}
	c.warnArrURLCredentials()
	c.warnIdenticalArrURLs()
	if err := c.validateEnabledArrs(); err != nil {
		return err
	}
	c.warnPublicURLProblems()
	c.warnUnexpandedSecretRefs()
	c.warnRelativeReportDir()
	c.warnOverlappingTags()
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
	if c.sonarrWanted && c.SonarrURL == "" && c.SonarrAPIKey == "" {
		return errors.New("sonarr.enabled is true but sonarr.url and sonarr.api_key are both empty")
	}
	if c.radarrWanted && c.RadarrURL == "" && c.RadarrAPIKey == "" {
		return errors.New("radarr.enabled is true but radarr.url and radarr.api_key are both empty")
	}
	if !c.SonarrEnabled() && !c.RadarrEnabled() {
		return errors.New("no arr configured: enable sonarr and/or radarr with a url + api_key")
	}
	return nil
}

// warnPublicURLProblems warns on a malformed or credentialed public_url.
// public_url only feeds report deep-links, so a malformed value warns (the
// links will be broken) but still loads; a hard rejection would newly reject
// configs that load today.
func (c *Config) warnPublicURLProblems() {
	for _, pu := range []struct{ name, val string }{
		{"sonarr.public_url", c.SonarrPublicURL},
		{"radarr.public_url", c.RadarrPublicURL},
	} {
		if err := validateHTTPURL(pu.name, pu.val); err != nil {
			slog.Warn("public_url is malformed; report deep-links will be broken",
				"field", pu.name, "error", err)
		} else if pu.val != "" {
			// Whether a public_url yields a usable deep-link is decided at publish
			// time by library.SafeLogURL, and this warning must not become a second
			// answer to that question: the refusal legs are read from
			// internal/displaylink, the one home of this app's structural vouch step
			// for a browser-destined URL, so the claim "your report deep-links will
			// be broken" cannot drift away from the rule that admits the link
			// (l-f208). Reaching this branch means validateHTTPURL already accepted
			// the value as an absolute http(s) URL with a host, so the only vouch
			// leg left to fail is the smuggling one - a backslash or an embedded
			// tab/newline, which net/url accepts anywhere after the authority while
			// the publisher refuses it outright. Field-name-only, warn-only,
			// matching every other check here.
			if f := urlform.Classify(pu.val); !displaylink.VouchSanitizingForm(&f) {
				slog.Warn("public_url carries a backslash or an embedded tab/newline; "+
					"the deep-link publisher refuses such a value outright, so report "+
					"rows carry no arr link at all - use plain forward slashes",
					"field", pu.name)
			}
		}
		// arrapi's WebURL joins the base and the route by string concatenation,
		// so a query in the base (or a bare trailing '?') puts the /series or
		// /movie route inside the query string and breaks every deep-link.
		// Warn-only, field-name-only: public_url only feeds deep-links, and the
		// query may carry a credential-like value that must never be echoed.
		if u, err := url.Parse(pu.val); err == nil && (u.RawQuery != "" || u.ForceQuery) {
			slog.Warn("public_url contains a query; report deep-links append the "+
				"route after it and will be broken - remove the query from the base",
				"field", pu.name)
		}
		if urlEmbedsCredential(pu.val) {
			slog.Warn("public_url embeds userinfo or a credential-like query parameter; "+
				"deep-links are credential-redacted in logs, state, and report files, "+
				"so the credential will never appear in the links",
				"field", pu.name)
		}
	}
}

// warnRelativeReportDir warns when report.dir is not an absolute path. Every
// report write goes through atomicfile, whose path gate rejects a relative
// path outright (ErrUnsafePath "not absolute"), and nothing absolutizes the
// configured value on the way there - so a relative report.dir loads and
// validates cleanly and then fails at the END of a report run, after the whole
// cycle has been spent, with neither half of the pair written. Warn-only: a
// daemon that never generates a report is unaffected, and rejecting would
// newly refuse configs that load today; field-name-only, since report.dir is
// secret-capable (internal/audit redacts it for that reason).
func (c *Config) warnRelativeReportDir() {
	if c.ReportDir != "" && !filepath.IsAbs(c.ReportDir) {
		slog.Warn("report.dir is not an absolute path; report writes are rejected "+
			"at the end of a report run and neither report file is written - use an "+
			"absolute path under the /config mount", "field", "report.dir")
	}
}

// warnUnexpandedSecretRefs warns when a secret field still holds a literal
// environment-variable reference. yamlenv expands only the ${VAR} form of an
// ALLOWLISTED name and reports only allowlisted-but-unset names (naming the
// VARIABLE, never the field holding it), so two operator spellings reach the
// runtime verbatim with no diagnostic anywhere: a non-allowlisted name
// (${AB_PASSKEY} instead of ${SEADEX_SCOUT_AB_PASSKEY}) and the brace-less
// shell form ($SEADEX_SCOUT_AB_PASSKEY, which docker compose itself accepts,
// making it a plausible paste). The literal placeholder is then sent as the
// credential and the operator sees only a downstream 401/403 - or, for
// indexer.ab_passkey, nothing at all: it is baked into every /ab RSS download
// link, so each arr grab fails at AnimeBytes while this app logs a served
// feed. indexer.feed_api_key is deliberately NOT in this list: it is the inbound
// gate rather than an outbound credential, so this message would misstate its
// failure - validateFeedAPIKey owns it, with one format gate that refuses every
// spelling outright. For a CONFIGURED indexer this warning is a breadcrumb
// ahead of that gate and validateABPasskey's: both fail the config for a
// malformed credential. It still carries a config with no indexer section,
// where neither gate runs and a parked placeholder would otherwise be silent.
// Warn-only (no real arr/Prowlarr/AnimeBytes credential takes a shape
// containing an env reference, but a false positive must not stop the daemon)
// and field-name-only; never echoes the value.
func (c *Config) warnUnexpandedSecretRefs() {
	for _, sf := range []struct{ name, val string }{
		{"sonarr.api_key", c.SonarrAPIKey},
		{"radarr.api_key", c.RadarrAPIKey},
		{"indexer.prowlarr_api_key", c.IndexerProwlarrAPIKey},
		{"indexer.ab_passkey", c.IndexerABPasskey},
	} {
		if secretref.Unexpanded(sf.val) {
			slog.Warn("a secret still holds a literal environment-variable reference; only "+
				"${VAR} names prefixed SONARR_/RADARR_/SEADEX_SCOUT_ are expanded, so the "+
				"literal placeholder is sent as the credential and every call to that "+
				"upstream fails to authenticate", "field", sf.name)
		}
	}
}

// warnOverlappingTags warns when a tag appears in both arr_tags.include and
// arr_tags.exclude. The library walk gives exclude precedence, so every item
// carrying such a tag is skipped and the include entry can never match --
// with a single overlapping include tag the config silently scans nothing.
// Warn-only (exclude-wins semantics are well defined, so the config still
// loads); field-name-only like every other config diagnostic, since a tag
// value can carry an expanded ${VAR} placed there by a config typo.
func (c *Config) warnOverlappingTags() {
	exclude := make(map[string]struct{}, len(c.ExcludeTags))
	for _, tag := range c.ExcludeTags {
		exclude[strings.ToLower(strings.TrimSpace(tag))] = struct{}{}
	}
	for _, tag := range c.IncludeTags {
		if _, ok := exclude[strings.ToLower(strings.TrimSpace(tag))]; ok {
			slog.Warn("a tag is listed in both arr_tags.include and arr_tags.exclude; " +
				"exclude wins, so items carrying it are never scanned")
			return
		}
	}
}

// warnArrURLCredentials warns (field-name-only, never echoing the URL)
// when an arr url embeds a credential-like userinfo or query parameter,
// which would otherwise leak into a library-walk-failure *url.Error log.
// For arr URLs the query half is defense-in-depth only: Validate's earlier
// validateArrPair no-query rejection fires first for any query-bearing url,
// so in practice only the userinfo form reaches this warning; the shared
// urlEmbedsCredential query scan stays live for torznab and public_url.
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

// warnIdenticalArrURLs warns when sonarr.url and radarr.url are identical -
// almost always a paste error: the two arrs are different applications, so a
// shared full URL points one client at the wrong service and every library
// walk on that side fails at runtime with confusing API errors. Warn-only
// (the config still loads), mirroring the identical-torznab-endpoint warning;
// field-name-only like every other config diagnostic.
func (c *Config) warnIdenticalArrURLs() {
	if c.SonarrURL != "" && c.SonarrURL == c.RadarrURL {
		slog.Warn("sonarr.url and radarr.url are identical; the two arrs are different " +
			"applications, so one of them points at the wrong service and its " +
			"library walk will fail at runtime")
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
	c.infoIndexerModeMismatch()
	if err := c.validateIndexerEndpoints(); err != nil {
		return err
	}
	c.warnIndexerEndpointProblems()
	c.warnTorznabURLCredentials()
	c.warnMissingProwlarrKey()
	return nil
}

// infoIndexerModeMismatch signals a configured feed in report mode. The
// Torznab feed is served only by the daemon; a file-level report mode exits
// after the one-shot audit, so a configured feed silently never starts.
// Info, mirroring the other half-configuration signals: a deliberately
// parked indexer section must not raise Loki alert noise.
func (c *Config) infoIndexerModeMismatch() {
	if c.RunMode == RunModeReport {
		slog.Info("indexer torznab urls are set but mode is report; " +
			"the Torznab feed is served only by a daemon run, so a mode-driven start " +
			"(no subcommand) exits after the one-shot audit without serving it - an " +
			"explicit `daemon` subcommand serves it regardless of this key")
	}
}

// The field names of the two per-indexer Torznab URL keys, shared by the
// validation and the warn batteries that each enumerate the pair.
const (
	fieldNyaaTorznabURL = "indexer.nyaa_torznab_url"
	fieldABTorznabURL   = "indexer.ab_torznab_url"
)

// torznabEndpoint pairs a per-indexer Torznab URL with its config key name.
type torznabEndpoint struct{ name, val string }

// torznabEndpoints is the single enumeration of the per-indexer Torznab URL
// fields, shared by the endpoint validator and the two warn batteries that
// each walk the pair - so adding or renaming an upstream touches one list
// instead of three.
func (c *Config) torznabEndpoints() []torznabEndpoint {
	return []torznabEndpoint{
		{fieldNyaaTorznabURL, c.IndexerNyaaTorznabURL},
		{fieldABTorznabURL, c.IndexerABTorznabURL},
	}
}

// validateIndexerEndpoints enforces the feed's authentication requirement,
// gates the two indexer credentials on their FORMAT, and validates the two
// upstream Torznab URLs, in the original diagnostic order.
//
// The two credential gates are where an operator-supplied string stops being
// untrusted text and becomes a value the app may use as a credential: it
// happens ONCE, here at the config boundary, so no downstream site has to ask
// again (parse, don't validate). Both are POSITIVE format checks - "is this the
// shape a credential takes" - rather than a catalogue of the ways a paste can
// go wrong, because a catalogue is open-ended: the app used to grade three
// spellings of one unexpanded reference three different ways (a braced ${VAR}
// an error, an unbraced $VAR a warning, a reference embedded in a longer value
// nothing at all) while internal/indexer refused to serve behind all three, so
// a config could validate clean, start, and then never serve the feed. A
// configured-but-unusable credential is now a HARD startup error, matching what
// the runtime refuses (unusableFeedKey / unusableABPasskey).
//
// Reference recognition survives only as a HINT inside the error message: it
// never decides pass or fail, so no regex has to model every spelling an
// operator can leave behind (including the unterminated "${NAME" paste that
// matches none of them).
func (c *Config) validateIndexerEndpoints() error {
	if err := c.validateFeedAPIKey(); err != nil {
		return err
	}
	if err := c.validateABPasskey(); err != nil {
		return err
	}
	for _, endpoint := range c.torznabEndpoints() {
		if err := validateHTTPURL(endpoint.name, endpoint.val); err != nil {
			return err
		}
	}
	return nil
}

// validateFeedAPIKey is the ONE gate on indexer.feed_api_key. The key is
// required (it is the only authentication on the feed, whose /ab RSS body
// embeds the operator's AnimeBytes passkey in every download link), and it must
// look like a credential: one run of printable characters with no whitespace,
// no control rune, and no '$'.
//
// The '$' rule is a charset rule, not placeholder pattern-matching, and it is
// the app's to make: this is seadex-scout's OWN key, generated the way the
// README says (openssl rand -hex 16), and no hex or base64 output contains a
// dollar sign. Refusing it therefore refuses every unexpanded-reference
// spelling at once - ${VAR}, $VAR, a reference embedded in a longer value, and
// the unterminated "${NAME" paste that no reference regex matches - without the
// app having to enumerate them, and it makes the config's acceptance set a
// SUBSET of what internal/indexer will serve behind (unusableFeedKey), which is
// the safe direction for two gates on one credential. The cost is a hand-typed
// passphrase containing '$' being refused with a message that says to generate
// a key instead; that is the intended trade for a gate on a LAN-reachable feed.
//
// Field-name-only on every arm: the key value never rides the error or the log.
func (c *Config) validateFeedAPIKey() error {
	if c.IndexerAPIKey == "" {
		return errors.New("indexer.feed_api_key is required when indexer.nyaa_torznab_url or indexer.ab_torznab_url is set")
	}
	if !wellFormedFeedKey(c.IndexerAPIKey) {
		msg := "indexer.feed_api_key is not a usable key: it must be one run of printable " +
			"characters with no spaces and no '$' - generate one with openssl rand -hex 16"
		if secretref.Unexpanded(c.IndexerAPIKey) || strings.ContainsRune(c.IndexerAPIKey, '$') {
			msg += "; it looks like an environment-variable reference left unexpanded, so the " +
				"variable is unset or not allowlisted (SONARR_/RADARR_/SEADEX_SCOUT_) and the feed " +
				"would be gated by that literal placeholder - a key guessable from the public " +
				"README and config.example"
		}
		return errors.New(msg)
	}
	// Presence is required above; strength is warn-only defense-in-depth. The
	// key is the only gate on the passkey-bearing /ab feed, so a trivially
	// guessable hand-typed key deserves a config-time signal without rejecting
	// a config that runs today. Field-name-only (never echo the key).
	if len(c.IndexerAPIKey) < 16 {
		slog.Warn("indexer.feed_api_key is shorter than 16 characters; it gates the " +
			"AnimeBytes-passkey-bearing feed - generate a strong key (openssl rand -hex 16)")
	}
	return nil
}

// validateABPasskey is the ONE gate on indexer.ab_passkey. AnimeBytes is off at
// EITHER half - an empty passkey, or an empty indexer.ab_torznab_url - and both
// off states pass (AnimeBytes searches still work through Prowlarr; the /ab RSS
// feed answers a Torznab error). A passkey configured BESIDE an AB endpoint must
// be the shape AnimeBytes issues, or the config fails: every AB download link is
// built from it, so a malformed value means each arr grab fails at the tracker
// while this app reports a served feed.
//
// The shape is length plus the absence of whitespace, and the lengths are
// upstream authority rather than an invention here: Jackett's AnimeBytes
// indexer rejects a passkey with "expected length: 32, 48, or 56" and
// Prowlarr's AnimeBytesSettingsValidator asserts the same three lengths, and
// both send the value as scrape.php's torrent_pass exactly as this app does.
// NEITHER constrains the charset, so this app must not either - no 32-hex
// assumption, and no Gazelle passkey convention (AnimeBytes is not
// Gazelle-based; it runs its own codebase with a chihaya fork as its tracker).
//
// The app validates SHAPE, never CORRECTNESS: a well-shaped but wrong passkey
// is the operator's to fix and must surface as a runtime auth failure at
// AnimeBytes, not as a config error. Field-name-only; never echoes the secret.
func (c *Config) validateABPasskey() error {
	// AnimeBytes is OFF at EITHER half: an empty passkey, or an empty
	// ab_torznab_url (README: "" = AB RSS off). With no AB endpoint nothing
	// builds an AB download link from this value, so a parked passkey - an
	// unexpanded ${VAR} this deployment never sets, or a truncated paste - must
	// not block the daemon the ALWAYS-ON compare loop rides in. This gate runs
	// for a nyaa-only feed too (IndexerConfigured is either URL), and the reason
	// it fails the config - every AB link is built from the passkey - does not
	// hold there. warnABPasskeyConfiguration's parked-passkey INFO is the signal
	// for that state.
	if c.IndexerABTorznabURL == "" || c.IndexerABPasskey == "" ||
		wellFormedABPasskey(c.IndexerABPasskey) {
		return nil
	}
	msg := "indexer.ab_passkey is not a usable AnimeBytes passkey: it must be 32, 48, or 56 " +
		"characters with no spaces (the lengths AnimeBytes issues, and the ones Jackett and " +
		"Prowlarr accept for the same credential) - copy it from your AnimeBytes profile, or " +
		"leave it empty to serve the feed without AnimeBytes download links"
	if secretref.Unexpanded(c.IndexerABPasskey) {
		msg += "; it looks like an environment-variable reference left unexpanded, so the variable " +
			"is unset or not allowlisted (SONARR_/RADARR_/SEADEX_SCOUT_)"
	}
	return errors.New(msg)
}

// wellFormedFeedKey reports whether v is the shape a generated feed key takes:
// non-empty, and one run of printable characters with no whitespace, no control
// rune and no '$'. See validateFeedAPIKey for why '$' is excluded.
func wellFormedFeedKey(v string) bool {
	if v == "" {
		return false
	}
	for _, r := range v {
		if r == '$' || unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// wellFormedABPasskey reports whether v is the shape an AnimeBytes passkey
// takes: one of the three lengths AnimeBytes issues, carrying no whitespace or
// control rune. The charset is deliberately unconstrained beyond that (see
// validateABPasskey).
func wellFormedABPasskey(v string) bool {
	switch len(v) {
	case 32, 48, 56:
	default:
		return false
	}
	for _, r := range v {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// warnIndexerEndpointProblems emits the warn/info diagnostics for suspicious
// but runnable endpoint combinations: a pasted-twice shared endpoint, a
// torznab url whose path cannot be a per-indexer Torznab endpoint, and the
// two AB passkey half-configurations. All field-name-only; never echo a URL
// or secret.
func (c *Config) warnIndexerEndpointProblems() {
	c.warnIdenticalIndexerEndpoints()
	c.warnNonPerIndexerEndpoints()
	c.warnABPasskeyConfiguration()
	c.warnReusedIndexerSecrets()
}

// warnIdenticalIndexerEndpoints warns when both per-indexer Torznab URLs hold
// the same value. Field-name-only; never echoes a URL.
func (c *Config) warnIdenticalIndexerEndpoints() {
	// The two upstream URLs are per-indexer Prowlarr Torznab endpoints
	// (/1/api vs /2/api); identical values are almost always a paste error.
	// The AB matcher is torrent-id-only, so pointing it at a Nyaa endpoint
	// yields wrong-tracker attribution and duplicate upstream queries.
	// Warn-only: the config still runs. Field-name-only; never echoes URLs.
	if c.IndexerNyaaTorznabURL != "" && c.IndexerNyaaTorznabURL == c.IndexerABTorznabURL {
		slog.Warn("indexer.nyaa_torznab_url and indexer.ab_torznab_url are identical; " +
			"they should be Prowlarr's per-indexer endpoints (e.g. /1/api vs /2/api) - " +
			"a shared endpoint double-queries one indexer and misattributes trackers")
	}
}

// warnNonPerIndexerEndpoints warns when a configured torznab url's path cannot
// be a Prowlarr per-indexer Torznab endpoint. Field-name-only; never echoes a
// URL.
func (c *Config) warnNonPerIndexerEndpoints() {
	// A Prowlarr per-indexer Torznab endpoint always carries a path (.../1/api).
	// A bare origin, or Prowlarr's REST API (/api/v1/...), is a paste error that
	// loads cleanly and then answers every proxied search with a Torznab
	// <error code="900">: internal/indexer/prowlarr.go appends the Torznab params
	// to whatever was configured, so a wrong base is visible only in
	// upstream-failure logs. The synthesized RSS feed is unaffected - an
	// empty-query check is served from the persisted journal and never contacts
	// Prowlarr. Warn-only (the config still runs) and field-name-only; never
	// echoes a URL.
	for _, tu := range c.torznabEndpoints() {
		if tu.val == "" {
			continue
		}
		u, err := url.Parse(tu.val)
		if err != nil {
			continue // validateIndexerEndpoints already rejected it
		}
		if p := strings.TrimSuffix(u.Path, "/"); p == "" || strings.HasPrefix(p, "/api/v1") {
			slog.Warn("torznab url is not a Prowlarr per-indexer Torznab endpoint "+
				"(expected a path like /1/api); every proxied search "+
				"fails upstream and answers the arr with a Torznab error",
				"field", tu.name)
		}
	}
}

// warnABPasskeyConfiguration emits the three AB half-configuration
// diagnostics. Field-name-only; never echoes a secret.
func (c *Config) warnABPasskeyConfiguration() {
	// The /ab RSS feed builds its download links from indexer.ab_passkey; a
	// stable AB-URL-without-passkey config makes that endpoint return a
	// Torznab <error> on every arr RSS check while searches (Prowlarr-proxied,
	// passkey-free) keep working. Warn at startup so the operator gets a
	// config-time signal instead of discovering it in downstream arr RSS
	// failures. Field-name-only; never echoes a secret.
	//
	// EMPTY, not "unusable": validateIndexerEndpoints has already failed the
	// config for a configured-but-malformed passkey (validateABPasskey, the one
	// format gate), so the only unusable state that can reach this diagnostic is
	// the documented off switch. Testing for a placeholder here too would be a
	// second spelling of a rule that already decided.
	if c.IndexerABTorznabURL != "" && c.IndexerABPasskey == "" {
		slog.Warn("indexer.ab_passkey is empty; AnimeBytes searches still work through Prowlarr, " +
			"but the /ab RSS feed returns a Torznab error until a passkey is configured")
	}
	// The inverse half-configuration: a passkey with no AB Torznab URL is
	// inert - the AB URL is the AnimeBytes on switch, so neither AB
	// journaling nor the /ab feed uses the passkey. Info, mirroring
	// infoDisabledIndexerKeys: a deliberately parked passkey must not raise
	// Loki alert noise. Field-name-only; never echoes the secret.
	if c.IndexerABTorznabURL == "" && c.IndexerABPasskey != "" {
		slog.Info("indexer.ab_passkey is set but indexer.ab_torznab_url is empty; " +
			"AnimeBytes is disabled and the passkey is unused (set indexer.ab_torznab_url to enable it)")
	}
	// The third AB half-configuration, and the only one that narrows the
	// MONITORING half: the top-level animebytes toggle and the indexer's AB
	// endpoint are separate surfaces (the README states the feed applies none of
	// the filter keys) sitting in different config sections, so configuring AB
	// for the feed while leaving animebytes at its false default is a plausible
	// miss. The feed then hands the arrs grabbable AnimeBytes releases while
	// compare and audit drop every AB release and link (classify.ABVisible /
	// filter.Obtainable), so a show whose only best release is on AB is never
	// alerted on and never appears in the report. Info, mirroring the other
	// half-configuration signals: the split is legitimate (an operator may want
	// arr-side grabs without AB alerts), so it must not raise Loki alert noise.
	// Field-name-only; echoes no value.
	if c.IndexerABTorznabURL != "" && !c.AnimeBytes {
		slog.Info("indexer.ab_torznab_url is set but animebytes is false; the Torznab feed " +
			"serves AnimeBytes releases while findings and the report drop every AB release " +
			"and link - set animebytes: true to alert on them too")
	}
}

// warnReusedIndexerSecrets warns when indexer.feed_api_key repeats another
// indexer secret. feed_api_key is the least protected of the three: the arrs
// send it as the apikey QUERY parameter on every request and store it in their
// own indexer configuration, so pasting the Prowlarr API key (header-only by
// design) or the AnimeBytes passkey into it widens that credential's exposure
// to anything that can read an arr's indexer settings or an intermediate
// access log. The two keys sit on adjacent lines of the same config section,
// which is what makes the paste plausible. Warn-only (the config still runs)
// and field-name-only; never echoes a secret.
func (c *Config) warnReusedIndexerSecrets() {
	for _, s := range []struct{ name, val string }{
		{"indexer.prowlarr_api_key", c.IndexerProwlarrAPIKey},
		{"indexer.ab_passkey", c.IndexerABPasskey},
	} {
		if s.val != "" && s.val == c.IndexerAPIKey {
			slog.Warn("indexer.feed_api_key repeats another indexer secret; the arrs send "+
				"feed_api_key as a query parameter and store it in their indexer config, so the "+
				"reused credential is exposed far more widely - give the feed its own key "+
				"(openssl rand -hex 16)", "field", s.name)
		}
	}
}

// warnMissingProwlarrKey warns on an empty Prowlarr API key. A search proxies
// Prowlarr using indexer.prowlarr_api_key in the X-Api-Key header. An empty
// key is accepted rather than rejected (it is valid when Prowlarr has auth
// "Disabled for Local Addresses"), but the common case is a misconfiguration:
// Prowlarr then answers 401 for every proxied search, and the feed reports
// each one to the arr as a Torznab <error code="900"> document (upstream
// query failed) rather than results. Warn so the operator gets a config-time
// signal without breaking the legitimate no-auth deployment.
func (c *Config) warnMissingProwlarrKey() {
	if c.IndexerProwlarrAPIKey == "" {
		slog.Warn("indexer.prowlarr_api_key is empty; searches proxy Prowlarr with no API key - " +
			"unless Prowlarr auth is disabled for local addresses they fail upstream (401) and " +
			"every search answers the arr with a Torznab <error code=\"900\"> instead of results")
	}
}

// infoDisabledIndexerKeys emits the half-configuration signal for indexer
// secrets set with no torznab URL, mirroring the disabled-arr-with-key Info
// in toConfig: the Prowlarr key and the AB passkey are always
// operator-written, so either one without a torznab URL almost always means
// the operator expected the feed to start.
// Info, not Warn - deliberately parked keys must not raise Loki alert noise.
func (c *Config) infoDisabledIndexerKeys() {
	// indexer.feed_api_key is deliberately NOT a trigger, unlike the other two:
	// the first-boot starter this app writes SEEDS it with a generated key
	// (seedFeedAPIKey), so every default no-indexer deployment carries one.
	// Including it made the signal fire on the correct default configuration at
	// every start, telling an operator who never asked for the feed to go
	// configure a torznab url - and destroyed the only thing the signal is for,
	// distinguishing an operator who wrote indexer secrets and expected the feed
	// from a stock install. The Prowlarr key and the AB passkey are still
	// operator-written and still trigger it.
	if c.IndexerProwlarrAPIKey != "" || c.IndexerABPasskey != "" {
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
	for _, tu := range c.torznabEndpoints() {
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
	if err := validateHTTPURL(name+".url", rawURL); err != nil {
		return err
	}
	// arrapi's base-URL contract forbids a query: a non-empty query passes
	// this load-time validation only to be rejected by arrapi.NewSonarr/
	// NewRadarr with an error that echoes the full configured URL (and any
	// credential-like query parameter with it), and a bare trailing '?'
	// (ForceQuery) is accepted by arrapi but turns every appended API path
	// into a query. Reject both here, field-name-only like the shared
	// validator's branches, so the failure is early and never URL-echoing.
	u, err := url.Parse(rawURL)
	if err != nil {
		// Unreachable after validateHTTPURL parsed the same string; kept for
		// the same field-name-only posture rather than a panic.
		return fmt.Errorf("%s.url is not a valid URL", name)
	}
	if u.RawQuery != "" || u.ForceQuery {
		return fmt.Errorf("%s.url must not contain a query", name)
	}
	return nil
}

// --- URL helpers ---

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
	// fragment survives the parse but is never sent over HTTP (the indexer's
	// request builder merges Torznab params into RawQuery component-wise -
	// see internal/indexer/prowlarr.go - precisely so a fragment cannot
	// swallow them, leaving a config fragment pure paste-error noise), and
	// an out-of-range port passes parsing but fails every later dial. Both
	// must fail at startup, not at first request.
	// Errors stay field-name-only, matching the branches above.
	// The raw string is scanned for the literal delimiter because u.Fragment
	// misses a bare trailing '#' (an empty fragment parses to ""), yet arrapi
	// would still append its API path after that delimiter as fragment data
	// and send every request to '/'. An encoded %23 remains valid path data.
	if strings.Contains(rawURL, "#") {
		return fmt.Errorf("%s must not contain a URL fragment", name)
	}
	if port := u.Port(); port != "" {
		// ParseUint bounds the range; port 0 parses but is never a dialable
		// destination (the wildcard "any port" in bind contexts only), so a
		// config carrying it would start cleanly and then fail every later
		// arr walk / proxied search at connect time - exactly the
		// runnable-but-broken class these checks exist to reject.
		if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
			return fmt.Errorf("%s has an invalid port", name)
		}
	}
	return nil
}

// urlEmbedsCredential reports whether rawURL carries a credential in userinfo
// or a credential-like query parameter (internal/credname owns the name set,
// shared with the indexer's broader redaction policy over the same vocabulary).
// Such a URL survives validation but leaks the credential to
// upstream-failure logs,
// which wrap the full request URL; validateIndexer warns on it field-name-only.
// The query is scanned on the raw string via urlform.RawQueryNames (split on
// both '&' and ';', each name percent-decoded): that is a strict superset of
// the parsed u.Query() view, which drops any malformed pair wholesale (an
// unescaped ';' in "?apikey=SECRET;foo=x" discards the entire pair while the
// secret stays in RawQuery for outgoing requests and logs). This consumer's
// fail direction is broad on purpose - over-matching a parameter name only
// costs a warning - which is why the library reports names and leaves the
// predicate here. Matches field names only, never values.
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
	for name := range urlform.RawQueryNames(u.RawQuery) {
		if credname.IsName(name) {
			return true
		}
	}
	return false
}

// --- Parse helpers and policies ---

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

// warnAllBlankTagList warns when a configured arr_tags list holds only
// blank entries: trimList drops them all, so that filter is silently OFF -
// the include side then admits every item past the operator's scoping and
// the exclude side excludes nothing, and no unmatched-tag warning fires
// from the walk (blank labels never reach it; arrapi.UnmatchedLabels
// ignores them too).
func warnAllBlankTagList(which string, raw, trimmed []string) {
	if len(raw) > 0 && len(trimmed) == 0 {
		slog.Warn("configured tag list holds only blank entries; the filter is off", "field", which)
	}
}

// warnConfigPermissions warns when the config file is readable beyond its
// owner. This file carries every secret the app has - the arr api_keys, the
// Prowlarr api key, and the AnimeBytes passkey - which is why the starter this
// app writes is 0600 (starterFileMode) and why the indexer's feed snapshot is
// owner-only too; an operator-authored or age-decrypted config commonly lands
// 0644, exposing all of them to any other uid on the host that mounts /config.
// Warn-only: a deliberately widened mode must not stop the daemon. The mode
// itself is not secret, so it is the one value this diagnostic echoes.
func warnConfigPermissions(path string) {
	info, err := os.Stat(path)
	if err != nil {
		// The bounded read already succeeded, so a stat failure here is a race
		// or an exotic filesystem - not worth a second startup diagnostic.
		return
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		slog.Warn("config file is readable beyond its owner; it holds the arr api keys, "+
			"the Prowlarr key and the AnimeBytes passkey - chmod 600 it",
			"field", "config file", "mode", strconv.FormatUint(uint64(perm), 8))
	}
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
