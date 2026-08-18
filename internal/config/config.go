// Package config loads seadex-scout configuration from a single YAML file
// (default /config/config.yaml). The file is the whole settings surface;
// string values may reference SONARR_*, RADARR_*, or SEADEX_SCOUT_* environment
// variables via ${VAR} expansion, so secrets can stay in an .env or Docker
// secret rather than in the file.
//
// The file exposes only user-facing settings; the upstream endpoints, cadences and
// internal /config paths are fixed package constants. The on-disk shape (fileConfig)
// is loaded onto a defaults baseline, ${VAR}-expanded, then flattened into the
// runtime Config. Call Validate to check the result is runnable. There is no hot
// reload: the file is read once at startup.
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

	"github.com/cplieger/atomicfile/v3"
	"github.com/cplieger/envx/yamlenv/v2"
	"github.com/cplieger/scheduler/v4"
	"github.com/cplieger/seadex-scout/internal/credname"
	"github.com/cplieger/seadex-scout/internal/displaylink"
	"github.com/cplieger/seadex-scout/internal/secretref"
	"github.com/cplieger/seadex-scout/internal/tagfilter"
	"github.com/cplieger/slogx"
	"github.com/cplieger/urlform"
)

// DefaultConfigDir is the single container mount every seadex-scout file lives
// under; every path constant below is derived from it.
const DefaultConfigDir = "/config"

// DefaultConfigPath is the container-internal config file path.
const DefaultConfigPath = DefaultConfigDir + "/config.yaml"

// maxConfigBytes bounds the config file read (it is a small document).
const maxConfigBytes = 1 << 20

// Fixed endpoints, cadences, internal /config file paths, and the default report
// directory: internal machinery wired at build time, deliberately NOT config-file
// keys. DefaultReportDir is the one baseline report.dir overrides.
//
// Each upstream's package owns its own DefaultURL and request cadence beside the
// decoder that embodies its contract; config is a leaf that cannot import them.
const (
	// DefaultMappingOverrides is the local alID->IDs override file; absent is fine.
	DefaultMappingOverrides = DefaultConfigDir + "/overrides.json"
	// DefaultStatePath is the atomic JSON cache/state file.
	DefaultStatePath = DefaultConfigDir + "/state.json"
	// DefaultCycleLockDir holds cycle.lock, the cross-process cycle coalescing lock.
	// It is the mount root, so the lock lives beside the writes it orders.
	DefaultCycleLockDir = DefaultConfigDir
	// DefaultIndexerFeedPath is the atomic JSON file the compare cycle writes the
	// indexer's materialized feed to and the indexer HTTP server reads; persisting it
	// lets a `poll` cycle refresh a resident daemon's feed across the process boundary.
	DefaultIndexerFeedPath = DefaultConfigDir + "/feed.json"
	// DefaultReportDir is the directory report mode writes timestamped report
	// pairs into (report-<UTC timestamp>.md / .json).
	DefaultReportDir = DefaultConfigDir + "/reports"

	// RunModeDaemon is the default: poll on a schedule and flag better releases.
	RunModeDaemon = "daemon"
	// RunModeReport is the one-shot audit: scan once, write the report, exit.
	RunModeReport = "report"

	// DefaultPollInterval is the loop's own interval, and it is the FRESHNESS knob
	// rather than the cost knob: most iterations are a cheap tick, so the upstream load
	// is proportional to the change RATE. 15m follows the consumer - Sonarr's own RSS
	// Sync Interval defaults to 15, so fetching faster cannot reach the arrs sooner.
	DefaultPollInterval = 15 * time.Minute
)

// Clamp bounds for poll_interval, the only file-provided duration. The floor is
// Sonarr's own 10-minute minimum: below it lies freshness no arr can read.
const (
	minPollInterval = 15 * time.Minute
	maxPollInterval = 30 * 24 * time.Hour
)

// Bounds on the filters.exclude_tags map, the one file key whose KEYS are
// operator-supplied free text. Generous ceilings only a paste error or a hostile file
// can reach; every release's tags are matched against the map on every surface.
const (
	maxExcludeTags   = 32
	maxExcludeTagLen = 64
)

// maxIgnoreIDs bounds filters.ignore. Findings are reported as STATE, so every entry
// is consulted once per finding on every pass; the ceiling keeps a 1 MiB config from
// turning that into unbounded work (CWE-400).
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
	// Filters sits late because filtersFile ends in bools: keeping the non-pointer
	// tail late shortens the GC-scanned prefix (govet fieldalignment).
	Filters filtersFile `yaml:"filters"`
	// AnimeBytes adds AnimeBytes (private tracker) releases and links; it is a
	// tracker-access toggle, not a content filter, so it sits at the top level.
	AnimeBytes bool `yaml:"animebytes"`
}

// indexerFile configures the optional Torznab feed the daemon serves alongside the
// compare loop. An empty Nyaa/AnimeBytes URL disables that upstream; both empty
// disables the feed (the daemon then binds no HTTP port). An empty ab_passkey leaves
// the AnimeBytes RSS feed without grabbable links (search still works via Prowlarr).
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
	// ExcludeTags maps a SeaDex tag to the surfaces it is excluded from ("findings",
	// "report", "feed"). Absent or empty means NOTHING is filtered on any surface.
	ExcludeTags map[string][]string `yaml:"exclude_tags"`
	// Ignore lists AniList IDs whose findings are never emitted; absent or empty
	// reports everything. It suppresses EMISSION only: the report still shows the row
	// and the RSS feed is untouched, because a release is never withheld from the arrs.
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

// defaultFileConfig is the baseline the YAML document overlays. Absent keys keep
// these values, so a partial config still runs.
func defaultFileConfig() fileConfig {
	return fileConfig{
		Sonarr: arrFile{URL: "http://sonarr:8989"},
		Radarr: arrFile{URL: "http://radarr:7878"},
		Mode:   RunModeDaemon,
		Report: reportFile{Dir: DefaultReportDir},
		Log:    logFile{Level: "info", Format: "json"},
	}
}

// Config is the effective runtime configuration after loading. It holds only the
// user-configurable settings; the fixed endpoints, cadences and /config file paths
// are package constants. Fields are ordered largest-alignment-first (fieldalignment).
type Config struct {
	// tagFilterErr holds a rejected filters.exclude_tags map, recorded at flatten time
	// and returned by Validate, so the single parse happens where the file shape is
	// still in hand. It leads the struct for fieldalignment, not for prominence.
	tagFilterErr error
	// TagFilter is the filters.exclude_tags policy: which SeaDex tags exclude a
	// release from which recommendation surface. The zero value filters NOTHING
	// anywhere, and it is the one policy all three surfaces read.
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

	// Indexer (Torznab feed) settings. IndexerAPIKey, IndexerProwlarrAPIKey and
	// IndexerABPasskey are secrets and are never logged. An empty upstream URL disables
	// that upstream; IndexerABPasskey builds the AB RSS download links.
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

// Load reads, ${VAR}-expands, and parses the YAML config at path into the runtime
// Config. It returns an error on a missing/oversized file, invalid YAML, a file
// holding more than one YAML document, or an unknown configuration key; call Validate
// for semantic checks. The one policy choice made here is WithUnknownKeyEcho: the
// unknown-key NAME is kept - it IS the diagnostic the operator needs - and the strict
// probe runs on the pre-expansion bytes, so the name cannot carry an expanded secret.
func Load(path string) (Config, error) {
	raw, err := readConfigFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	fc := defaultFileConfig()
	refs, err := yamlenv.Load(raw, &fc, isAllowedEnvVar,
		yamlenv.WithSanitizeOptions(yamlenv.WithUnknownKeyEcho(true)))
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

// readConfigFile reads the config through an os.Root over its own directory, so the
// read can neither be redirected out of the config directory nor block: the open is
// O_NONBLOCK and refuses a directory, FIFO, device node or socket, where a plain
// os.Open follows a symlink and blocks indefinitely on a writerless FIFO - and this
// read runs at startup before any diagnostic, so a planted FIFO would wedge the
// process silently.
func readConfigFile(path string) (raw []byte, err error) {
	dir := filepath.Dir(path)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() {
		if clErr := root.Close(); clErr != nil {
			slog.Warn("could not close config directory handle",
				"field", "config dir", "error", clErr)
		}
	}()
	f, _, err := atomicfile.OpenRegularInRoot(root, filepath.Base(path))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// Config load is a synchronous startup step with no cancellation point,
	// so it passes context.Background(), matching writeStarterConfig.
	raw, err = atomicfile.ReadBoundedFile(context.Background(), f, maxConfigBytes)
	if err != nil {
		return nil, err
	}
	return raw, nil
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
	c.SonarrURL, c.SonarrAPIKey, c.SonarrPublicURL = applyArr(fc.Sonarr)
	c.RadarrURL, c.RadarrAPIKey, c.RadarrPublicURL = applyArr(fc.Radarr)
	if c.ReportDir == "" {
		c.ReportDir = DefaultReportDir
	}
	c.PollInterval, c.PollExternal = parseInterval(fc.PollInterval)
	c.TagFilter, c.tagFilterErr = buildTagFilter(fc.Filters.ExcludeTags)
	c.IgnoreFindings, c.ignoreErr = buildIgnoreSet(fc.Filters.Ignore)
	return c
}

// applyArr flattens one arr section: an enabled arr's trimmed connection details, or
// empty strings.
func applyArr(af arrFile) (arrURL, key, publicURL string) {
	if af.Enabled {
		return strings.TrimSpace(af.URL), strings.TrimSpace(af.APIKey), strings.TrimSpace(af.PublicURL)
	}
	return "", "", ""
}

// buildIgnoreSet turns filters.ignore into the emission-suppression set, rejecting an
// over-long list and a non-positive AniList ID (SeaDex's own IDs start at 1). An
// empty list yields nil, the same as an absent key.
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

// buildTagFilter turns the filters.exclude_tags map into the one tagfilter policy
// every recommendation surface reads, or an error the caller records for Validate to
// return. An absent or empty map yields the zero Filter, which filters nothing.
//
// Four rejections, all hard errors rather than silent no-ops - more tags than
// maxExcludeTags, a blank or over-long tag key, an unknown surface name, and a tag
// listing NO surfaces - because each is an operator asking for filtering that would
// not happen. Diagnostics are field-name-only: a ${VAR} typo can expand into either.
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
	// Sorted keys keep the reported defect deterministic when a map holds more than
	// one; map iteration order would otherwise vary per run for the same file.
	for _, tag := range slices.Sorted(maps.Keys(raw)) {
		switch key := strings.TrimSpace(tag); {
		case key == "":
			return tagfilter.Filter{}, errors.New(
				"filters.exclude_tags holds a blank tag key")
		case len(key) > maxExcludeTagLen:
			return tagfilter.Filter{}, fmt.Errorf(
				"a filters.exclude_tags tag key is longer than %d bytes", maxExcludeTagLen)
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

// parseInterval reads the poll_interval value into a built-in cadence or the external
// (resident-idle) mode, following the fleet `*_INTERVAL` convention: off/disabled/0 ->
// external, empty -> the default, a valid positive duration -> built-in (clamped to
// [minPollInterval, maxPollInterval]), anything else -> the default with a warning.
// Every scheduler warning stays field-name-only, since poll_interval can hold an
// expanded ${VAR} secret placed there by a config typo.
func parseInterval(raw string) (time.Duration, bool) {
	s := scheduler.ParseInterval(raw, DefaultPollInterval,
		scheduler.WithBounds(minPollInterval, maxPollInterval),
		scheduler.WithName("poll_interval"),
		scheduler.WithRedactedValue(true),
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

// SonarrWebBase is the base URL for Sonarr report deep-links: the public URL when
// set, else the internal URL, so an internal Docker hostname still links usefully.
func (c *Config) SonarrWebBase() string { return cmp.Or(c.SonarrPublicURL, c.SonarrURL) }

// RadarrWebBase is the base URL for Radarr report deep-links (see SonarrWebBase).
func (c *Config) RadarrWebBase() string { return cmp.Or(c.RadarrPublicURL, c.RadarrURL) }

// IndexerConfigured reports whether the Torznab feed has an upstream to proxy: at
// least one Prowlarr Torznab URL is set. It is the single home of that decision.
func (c *Config) IndexerConfigured() bool {
	return c.IndexerNyaaTorznabURL != "" || c.IndexerABTorznabURL != ""
}

// --- Validation and diagnostics ---

// Validate reports the first configuration problem that would stop the app from
// running, or nil when runnable. It is deliberately not a pure query: on the way
// through the checks it also emits the config-time diagnostics that need the
// assembled Config, so calling it twice duplicates them and a path that skips it
// loses them. It stops at the FIRST hard error, so the remaining diagnostics surface
// only after the operator fixes that one and restarts.
func (c *Config) Validate() error {
	if err := validateRunMode(c.RunMode); err != nil {
		return err
	}
	// The exclude_tags map and the ignore list are parsed once at flatten time; this is
	// where a rejection becomes the startup error.
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
	if err := c.validateEnabledArrs(); err != nil {
		return err
	}
	c.warnPublicURLProblems()
	if err := c.validateProwlarrAPIKey(); err != nil {
		return err
	}
	c.warnUnexpandedSecretRefs()
	c.warnRelativeReportDir()
	return c.validateIndexer()
}

// validateRunMode rejects an unknown run mode. Field-name-only (do not echo the
// supplied mode): the value may be an expanded ${VAR} secret from a config typo.
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

// warnPublicURLProblems warns on a malformed or credentialed public_url. It only
// feeds report deep-links, so a malformed value warns but still loads.
func (c *Config) warnPublicURLProblems() {
	for _, pu := range []struct{ name, val string }{
		{"sonarr.public_url", c.SonarrPublicURL},
		{"radarr.public_url", c.RadarrPublicURL},
	} {
		if err := validateHTTPURL(pu.name, pu.val); err != nil {
			slog.Warn("public_url is malformed; report deep-links will be broken",
				"field", pu.name, "error", err)
		} else if pu.val != "" {
			// The refusal legs are read from internal/displaylink, the one home of this
			// app's structural vouch step for a browser-destined URL, so the claim "your
			// deep-links will be broken" cannot drift from the rule that admits the link.
			// Warn-only and field-name-only, matching every other check here.
			if f := urlform.Classify(pu.val); !displaylink.VouchSanitizingForm(&f) {
				slog.Warn("public_url carries a backslash or an embedded tab/newline; "+
					"the deep-link publisher refuses such a value outright, so report "+
					"rows carry no arr link at all - use plain forward slashes",
					"field", pu.name)
			}
		}
		// arrapi's WebURL joins the base and the route by string concatenation, so a
		// query in the base (or a bare trailing '?') puts the route inside the query
		// string and breaks every deep-link. Warn-only, field-name-only.
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

// warnRelativeReportDir warns when report.dir is not an absolute path. Every report
// write goes through an absolute-path-only gate and nothing absolutizes the value on
// the way there, so a relative report.dir validates cleanly and then fails at the END
// of a report run. Warn-only (a daemon that never reports is unaffected) and
// field-name-only, since report.dir is secret-capable.
func (c *Config) warnRelativeReportDir() {
	// The predicate is atomicfile.ValidatePath, not filepath.IsAbs: it is the exported
	// face of the rule the report write applies later, and IsAbs admits an embedded NUL
	// byte. Error text is not echoed: report.dir is secret-capable.
	if c.ReportDir != "" && atomicfile.ValidatePath(c.ReportDir) != nil {
		slog.Warn("report.dir is not a usable absolute path; report writes are rejected "+
			"at the end of a report run and neither report file is written - use an "+
			"absolute path under the /config mount", "field", "report.dir")
	}
}

// warnUnexpandedSecretRefs warns when indexer.ab_passkey still holds a literal
// environment-variable reference. Two operator spellings reach the runtime verbatim
// with no diagnostic anywhere - a non-allowlisted name, and the brace-less $VAR form
// docker compose itself accepts - and the placeholder is then baked into every /ab RSS
// download link. The passkey is the ONLY field here because it is the only credential
// with no charset gate; a warning rather than an error because validateABPasskey
// deliberately passes a PARKED value. Field-name-only; never echoes the value.
func (c *Config) warnUnexpandedSecretRefs() {
	if secretref.Unexpanded(c.IndexerABPasskey) {
		slog.Warn("a secret still holds a literal environment-variable reference; only "+
			"${VAR} names prefixed "+envAllowlistSpelling+" are expanded, so the "+
			"literal placeholder is baked into every /ab RSS download link - this app "+
			"reports a served feed while every arr grab fails at AnimeBytes",
			"field", "indexer.ab_passkey")
	}
}

// warnArrURLCredentials warns (field-name-only, never echoing the URL) when an arr
// url embeds a credential-like userinfo or query parameter, which would otherwise
// leak into a library-walk-failure *url.Error log. For arr URLs the query half is
// defense-in-depth only: validateArrPair's no-query rejection fires first.
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

// validateIndexer rejects an enabled Torznab feed with no feed API key. The feed is
// the only HTTP surface and authenticates callers by the apikey query param, so an
// empty key would leave it unauthenticated - and able to leak the AnimeBytes passkey
// embedded in synthesized RSS download links.
func (c *Config) validateIndexer() error {
	if !c.IndexerConfigured() {
		return nil
	}
	if err := c.validateIndexerEndpoints(); err != nil {
		return err
	}
	c.warnABPasskeyConfiguration()
	c.warnTorznabURLCredentials()
	c.warnMissingProwlarrKey()
	return nil
}

// The field names of the two per-indexer Torznab URL keys, shared by the
// endpoint validator and the warn battery that each enumerate the pair.
const (
	fieldNyaaTorznabURL = "indexer.nyaa_torznab_url"
	fieldABTorznabURL   = "indexer.ab_torznab_url"
)

// torznabEndpoint pairs a per-indexer Torznab URL with its config key name.
type torznabEndpoint struct{ name, val string }

// torznabEndpoints is the single enumeration of the per-indexer Torznab URL fields,
// shared by the endpoint validator and the warn battery that walks the pair.
func (c *Config) torznabEndpoints() []torznabEndpoint {
	return []torznabEndpoint{
		{fieldNyaaTorznabURL, c.IndexerNyaaTorznabURL},
		{fieldABTorznabURL, c.IndexerABTorznabURL},
	}
}

// validateIndexerEndpoints enforces the feed's authentication requirement, gates the
// two indexer-owned credentials on their FORMAT, and validates the two upstream
// Torznab URLs, in the original diagnostic order. indexer.prowlarr_api_key is gated
// UNCONDITIONALLY from Validate precisely because this function does not always run.
// The gates are POSITIVE format checks at the config boundary - is this the shape a
// credential takes - so a configured-but-unusable credential is a hard startup error
// matching what the runtime refuses, rather than a catalogue of paste spellings.
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

// validateFeedAPIKey is the ONE gate on indexer.feed_api_key. The key is required (it
// is the only authentication on the feed, whose /ab RSS body embeds the operator's
// AnimeBytes passkey in every download link), and it must look like a credential: one
// run of printable characters with no whitespace, no control rune and no '$'. The
// shape rule is wellFormedCredential, shared with the arr and Prowlarr key gates.
// Field-name-only on every arm: the key value never rides the error or the log.
func (c *Config) validateFeedAPIKey() error {
	if c.IndexerAPIKey == "" {
		return errors.New("indexer.feed_api_key is required when indexer.nyaa_torznab_url or indexer.ab_torznab_url is set")
	}
	if !wellFormedCredential(c.IndexerAPIKey) {
		msg := "indexer.feed_api_key is not a usable key: it must be one run of printable " +
			"characters with no spaces and no '$' - generate one with openssl rand -hex 16"
		// Keyed on the CHARACTER, not on a reference regex: the charset rule is what
		// refused the value, and every reference spelling contains a '$'.
		if strings.ContainsRune(c.IndexerAPIKey, '$') {
			msg += unexpandedRefHint + " and the feed would be gated by that literal " +
				"placeholder - a key guessable from the public README and config.example"
		}
		return errors.New(msg)
	}
	return nil
}

// validateProwlarrAPIKey is the ONE gate on indexer.prowlarr_api_key. An EMPTY key
// passes (it is valid when Prowlarr has auth "Disabled for Local Addresses"), but a
// key that is SET must be a well-formed credential: it rides the X-Api-Key header on
// every proxied search, so a placeholder means Prowlarr 401s and every search answers
// the arr with a Torznab error instead of results. It runs UNCONDITIONALLY, from
// Validate, because validateIndexer runs only when a Torznab URL is configured - so a
// gate inside it would stay silent on exactly the config where nothing else looks.
// Field-name-only; never echoes the key.
func (c *Config) validateProwlarrAPIKey() error {
	if c.IndexerProwlarrAPIKey == "" {
		return nil
	}
	if err := checkAPIKeyShape("indexer.prowlarr_api_key", c.IndexerProwlarrAPIKey); err != nil {
		return err
	}
	warnUnexpectedAPIKeyShape("indexer.prowlarr_api_key", c.IndexerProwlarrAPIKey)
	return nil
}

// validateABPasskey is the ONE gate on indexer.ab_passkey. AnimeBytes is off at
// EITHER half - an empty passkey, or an empty indexer.ab_torznab_url - and both off
// states pass. A passkey configured BESIDE an AB endpoint must be the shape AnimeBytes
// issues, or the config fails: every AB download link is built from it. The lengths
// are upstream authority (Jackett's AnimeBytes indexer and Prowlarr's validator both
// assert 32, 48 or 56) while NEITHER constrains the charset, so this app must not
// either. It validates SHAPE, never CORRECTNESS; field-name-only.
func (c *Config) validateABPasskey() error {
	// AnimeBytes is OFF at EITHER half, so a parked passkey - an unexpanded ${VAR} this
	// deployment never sets, or a truncated paste - must not block the daemon the
	// always-on compare loop rides in. warnABPasskeyConfiguration signals that state.
	if c.IndexerABTorznabURL == "" || c.IndexerABPasskey == "" ||
		wellFormedABPasskey(c.IndexerABPasskey) {
		return nil
	}
	msg := "indexer.ab_passkey is not a usable AnimeBytes passkey: it must be 32, 48, or 56 " +
		"characters with no spaces (the lengths AnimeBytes issues, and the ones Jackett and " +
		"Prowlarr accept for the same credential) - copy it from your AnimeBytes profile, or " +
		"leave it empty to serve the feed without AnimeBytes download links"
	if secretref.Unexpanded(c.IndexerABPasskey) {
		msg += unexpandedRefHint
	}
	return errors.New(msg)
}

// wellFormedCredential reports whether v is the shape a machine-generated credential
// takes: non-empty, and one run of printable characters with no whitespace, no
// control rune and no '$'. It is the ONE shape rule every credential field in this
// config is gated on. The '$' rule is a charset rule, not placeholder pattern
// matching: every unexpanded-reference spelling contains a dollar sign, so refusing
// the character refuses all of them without enumerating them, and it keeps each
// gate's acceptance set a SUBSET of what the runtime will serve behind.
// indexer.ab_passkey is deliberately NOT gated on it (see validateABPasskey).
func wellFormedCredential(v string) bool {
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

// generatedAPIKeyLen is the length of the key Sonarr, Radarr and Prowlarr generate: a
// .NET Guid with the hyphens stripped, so 32 lower-case hex characters.
const generatedAPIKeyLen = 32

// generatedArrAPIKey reports whether v is exactly the shape Sonarr, Radarr and
// Prowlarr GENERATE: 32 lower-case hex characters (all three carry the identical
// Guid.NewGuid().ToString().Replace("-", "") generator).
//
// It is a GENERATOR, not a VALIDATOR, which is the whole reason nothing fails the
// config on it: each also reads its *__AUTH__APIKEY environment variable FIRST and
// checks it only for blank, so an operator-supplied key of ANY shape is a working key
// against a real arr. Do NOT "tighten" this into a refusal.
func generatedArrAPIKey(v string) bool {
	if len(v) != generatedAPIKeyLen {
		return false
	}
	for _, r := range v {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// checkAPIKeyShape is the HARD gate every arr/Prowlarr API key passes: the value must
// be a well-formed credential, or the config fails with a field-name-only error naming
// the real remedy. A value carrying a '$' gets the unexpanded-reference hint on top,
// keyed on the CHARACTER rather than on a reference regex.
func checkAPIKeyShape(field, v string) error {
	if wellFormedCredential(v) {
		return nil
	}
	msg := field + " is not a usable API key: it must be one run of printable characters " +
		"with no spaces and no '$' - Sonarr, Radarr and Prowlarr generate a " +
		"32-character hex key, shown under Settings -> General -> API Key"
	if strings.ContainsRune(v, '$') {
		msg += unexpandedRefHint + " and that literal placeholder would be sent as the credential"
	}
	return errors.New(msg)
}

// warnUnexpectedAPIKeyShape warns when a key that PASSED checkAPIKeyShape is not the
// 32-lower-case-hex shape all three upstreams generate. Never an error, because the
// upstreams accept an operator-supplied key of any shape. Field-name-only.
func warnUnexpectedAPIKeyShape(field, v string) {
	// v is non-empty by construction: both callers run this only after
	// checkAPIKeyShape returned nil, and wellFormedCredential refuses "".
	if generatedArrAPIKey(v) {
		return
	}
	slog.Warn("api key is not the shape Sonarr/Radarr/Prowlarr generate "+
		"(32 hex characters); it is accepted, but a truncated or mistyped paste looks "+
		"exactly like this and every call to that upstream would fail to authenticate",
		"field", field)
}

// wellFormedABPasskey reports whether v is one of the three lengths AnimeBytes
// issues, carrying no whitespace or control rune; the charset is unconstrained.
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

// warnABPasskeyConfiguration emits the three AB half-configuration
// diagnostics. Field-name-only; never echoes a secret.
func (c *Config) warnABPasskeyConfiguration() {
	// The /ab RSS feed builds its download links from indexer.ab_passkey, so an
	// AB-URL-without-passkey config returns a Torznab error on every arr RSS check while
	// Prowlarr-proxied searches keep working. EMPTY, not "unusable":
	// validateABPasskey has already failed a configured-but-malformed passkey.
	if c.IndexerABTorznabURL != "" && c.IndexerABPasskey == "" {
		slog.Warn("indexer.ab_passkey is empty; AnimeBytes searches still work through Prowlarr, "+
			"but the /ab RSS feed returns a Torznab error until a passkey is configured",
			"field", "indexer.ab_passkey")
	}
	// The inverse half-configuration: a passkey with no AB Torznab URL is inert, since
	// the AB URL is the on switch. Info - a parked passkey must not raise alert noise.
	if c.IndexerABTorznabURL == "" && c.IndexerABPasskey != "" {
		slog.Info("indexer.ab_passkey is set but indexer.ab_torznab_url is empty; "+
			"AnimeBytes is disabled and the passkey is unused (set indexer.ab_torznab_url to enable it)",
			"field", "indexer.ab_passkey")
	}
	// The third AB half-configuration, and the only one that narrows the MONITORING
	// half: configuring AB for the feed while animebytes stays at its false default
	// hands the arrs grabbable AnimeBytes releases while compare and audit drop every AB
	// release and link. Info: the split is legitimate, so it must not alert.
	if c.IndexerABTorznabURL != "" && !c.AnimeBytes {
		slog.Info("indexer.ab_torznab_url is set but animebytes is false; the Torznab feed "+
			"serves AnimeBytes releases while findings and the report drop every AB release "+
			"and link - set animebytes: true to alert on them too",
			"field", "animebytes")
	}
}

// warnMissingProwlarrKey warns on an empty Prowlarr API key. Empty is accepted (it is
// valid when Prowlarr has auth "Disabled for Local Addresses"), but the common case is
// a misconfiguration: Prowlarr then 401s every proxied search and the feed answers the
// arr with a Torznab error 900. EMPTY only - a key that is SET but malformed has
// already failed the config in validateProwlarrAPIKey.
func (c *Config) warnMissingProwlarrKey() {
	if c.IndexerProwlarrAPIKey == "" {
		slog.Warn("indexer.prowlarr_api_key is empty; searches proxy Prowlarr with no API key - "+
			"unless Prowlarr auth is disabled for local addresses they fail upstream (401) and "+
			"every search answers the arr with a Torznab <error code=\"900\"> instead of results",
			"field", "indexer.prowlarr_api_key")
	}
}

// warnTorznabURLCredentials warns (field-name-only, never echoing the URL) when a
// torznab url embeds a credential-like userinfo or query parameter. The header-based
// Prowlarr key posture is defeated when the operator pastes a Jackett-style URL with
// an embedded credential: upstream failures log the request URL.
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

// validateArrPair rejects a half-configured enabled arr (a URL with no key, or a URL
// that is not an absolute http(s) URL with a host), and gates the arr's API key on its
// FORMAT: the shared wellFormedCredential rule via checkAPIKeyShape, then a warn-only
// note when the value is not the 32-hex shape the arrs generate.
//
// The accepted cost is larger here than for this app's own feed key: an operator who
// DELIBERATELY set a custom arr key containing a dollar sign is refused, even though
// the arrs would accept it. The trade is taken because the alternative failure mode -
// every arr call 401ing - names the arr rather than the config typo that caused it.
func validateArrPair(name, rawURL, key string) error {
	switch {
	case rawURL == "" && key == "":
		return nil
	case rawURL == "":
		return fmt.Errorf("%s.api_key is set but %s.url is empty", name, name)
	case key == "":
		return fmt.Errorf("%s.url is set but %s.api_key is empty", name, name)
	}
	if err := checkAPIKeyShape(name+".api_key", key); err != nil {
		return err
	}
	warnUnexpectedAPIKeyShape(name+".api_key", key)
	if err := validateHTTPURL(name+".url", rawURL); err != nil {
		return err
	}
	// arrapi's base-URL contract forbids a query: a non-empty query would pass this
	// validation only to be rejected by the constructor with an error echoing the full
	// URL, and a bare trailing '?' turns every appended API path into a query. Reject
	// both here, field-name-only. validateHTTPURL parsed this same string above.
	u, _ := url.Parse(rawURL)
	if u.RawQuery != "" || u.ForceQuery {
		return fmt.Errorf("%s.url must not contain a query", name)
	}
	return nil
}

// --- URL helpers ---

// validateHTTPURL rejects a non-empty rawURL that is not an absolute http(s) URL with
// a host; an empty rawURL passes (the caller decides whether the field is required).
// Shared by the arr-pair and indexer Torznab-URL validators.
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
		// Field-name-only, matching the parse-error branch: u.Redacted() masks only a
		// userinfo password, so echoing would still ship a query-string apikey.
		return fmt.Errorf("%s must be an absolute http(s) URL with a host", name)
	}
	// url.Parse accepts URI shapes the base-URL consumers cannot use: a fragment
	// survives the parse but is never sent over HTTP, and an out-of-range port fails
	// every later dial. The raw string is scanned for the literal '#' because u.Fragment
	// misses a bare trailing one, after which arrapi would send every request to '/'.
	if strings.Contains(rawURL, "#") {
		return fmt.Errorf("%s must not contain a URL fragment", name)
	}
	if port := u.Port(); port != "" {
		// ParseUint bounds the range; port 0 parses but is never a dialable destination,
		// so a config carrying it would start cleanly and fail every later request.
		if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
			return fmt.Errorf("%s has an invalid port", name)
		}
	}
	return nil
}

// urlEmbedsCredential reports whether rawURL carries a credential in userinfo or a
// credential-like query parameter (internal/credname owns the name set). Such a URL
// survives validation but leaks the credential to upstream-failure logs, which wrap
// the full request URL. The query is scanned on the raw string, a strict superset of
// the parsed u.Query() view: that view drops a malformed pair wholesale while the
// secret stays in RawQuery for outgoing requests and logs. Matches names, never values.
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

// isAllowedEnvVar reports whether an env var name is safe to expand in the config:
// only the app's own SONARR_*, RADARR_* and SEADEX_SCOUT_* names, so a stray ${HOME}
// is left literal. allowedEnvPrefixes is the ONE place the set is written - the rule,
// its operator-facing rendering, and the clause three credential errors quote all
// read it, so renaming a prefix cannot leave a diagnostic naming a stale set.
var allowedEnvPrefixes = []string{"SONARR_", "RADARR_", "SEADEX_SCOUT_"}

// envAllowlistSpelling renders allowedEnvPrefixes the way every config
// diagnostic quotes it.
var envAllowlistSpelling = strings.Join(allowedEnvPrefixes, "/")

// unexpandedRefHint is the shared clause a credential error appends when the
// refused value carries a '$'. Callers append their own per-field tail.
var unexpandedRefHint = "; it looks like an environment-variable reference left unexpanded, so the " +
	"variable is unset or not allowlisted (" + envAllowlistSpelling + ")"

func isAllowedEnvVar(key string) bool {
	for _, prefix := range allowedEnvPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
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
		slog.Warn("unrecognized log.format; defaulting to json", "field", "log.format")
	}
	return f
}

// parseLogLevel converts a level string to slog.Level via slogx.ParseLevel
// (case-insensitive, trims, accepts the long-form "warning" alias and slog
// offset syntax), falling back to Info for an empty or unrecognized value.
func parseLogLevel(s string) slog.Level {
	// ParseLevel returns ok=true for an empty value, so ok=false is specifically a
	// non-empty unrecognized level worth a warning.
	lvl, ok := slogx.ParseLevel(s, slog.LevelInfo)
	if !ok {
		// Field-name-only: the rejected value may be an expanded ${VAR} secret
		// placed here by a config typo and must never reach the startup log.
		slog.Warn("unrecognized log.level; defaulting to info", "field", "log.level")
	}
	return lvl
}
