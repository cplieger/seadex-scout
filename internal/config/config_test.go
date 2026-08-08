package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/tagfilter"
	"github.com/cplieger/slogx"
	"github.com/cplieger/slogx/capture"
)

// The string-level expansion mechanics (braced-only matching, keep-literal on
// unknown/unset, bare-dollar safety) are yamlenv's contract, tested in
// github.com/cplieger/envx/yamlenv. Here the app tests its own allowlist
// policy plus the Load-level wiring (expansion, the unresolved-refs warning,
// keys-stay-literal, and the secret-redaction posture).

// testABPasskey is a well-shaped AnimeBytes passkey for the configs that only
// need a passkey PRESENT: 32 characters, the shortest of the three lengths
// validateABPasskey accepts. The gate is a shape gate, so any 32-character run
// with no whitespace passes - a fixture does not have to be a real credential,
// and it is assembled rather than written out so no secret scanner has to
// decide whether this line is one.
var testABPasskey = strings.Repeat("0f1e2d3c", 4)

func TestIsAllowedEnvVar(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{"SONARR_API_KEY", true},
		{"RADARR_URL", true},
		{"SEADEX_SCOUT_AB_PASSKEY", true},
		{"HOME", false},
		{"PATH", false},
		{"SONAR_TYPO", false},
	}
	for _, tt := range tests {
		if got := isAllowedEnvVar(tt.key); got != tt.want {
			t.Errorf("isAllowedEnvVar(%q) = %v, want %v", tt.key, got, tt.want)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"daemon with sonarr ok", Config{RunMode: RunModeDaemon, SonarrURL: "http://sonarr:8989", SonarrAPIKey: "k"}, false},
		{"report with radarr ok", Config{RunMode: RunModeReport, RadarrURL: "http://radarr:7878", RadarrAPIKey: "k"}, false},
		{"invalid mode", Config{RunMode: "watch", SonarrURL: "http://s", SonarrAPIKey: "k"}, true},
		{"no arr configured", Config{RunMode: RunModeDaemon}, true},
		{"sonarr url without key", Config{RunMode: RunModeDaemon, SonarrURL: "http://s"}, true},
		{"radarr key without url", Config{RunMode: RunModeDaemon, RadarrAPIKey: "k"}, true},
		{"non-http scheme rejected", Config{RunMode: RunModeDaemon, SonarrURL: "ftp://sonarr", SonarrAPIKey: "k"}, true},
		{"url with no host rejected", Config{RunMode: RunModeDaemon, SonarrURL: "not-a-url", SonarrAPIKey: "k"}, true},
		{"sonarr port-only authority rejected", Config{RunMode: RunModeDaemon, SonarrURL: "http://:8989", SonarrAPIKey: "k"}, true},
		{"indexer port-only authority rejected", Config{RunMode: RunModeDaemon, SonarrURL: "http://s", SonarrAPIKey: "k", IndexerNyaaTorznabURL: "http://:9696/22/api", IndexerAPIKey: "feedkey"}, true},
		{"nyaa indexer url without feed key rejected", Config{RunMode: RunModeDaemon, SonarrURL: "http://s", SonarrAPIKey: "k", IndexerNyaaTorznabURL: "http://prowlarr/22/api"}, true},
		{"ab indexer url without feed key rejected", Config{RunMode: RunModeDaemon, SonarrURL: "http://s", SonarrAPIKey: "k", IndexerABTorznabURL: "http://prowlarr/2/api"}, true},
		{"indexer url with feed key ok", Config{RunMode: RunModeDaemon, SonarrURL: "http://s", SonarrAPIKey: "k", IndexerNyaaTorznabURL: "http://prowlarr/22/api", IndexerAPIKey: "feedkey"}, false},
		{"no indexer url unaffected", Config{RunMode: RunModeDaemon, SonarrURL: "http://s", SonarrAPIKey: "k"}, false},
		{"enabled sonarr with url and key both empty rejected", Config{RunMode: RunModeDaemon, sonarrWanted: true, RadarrURL: "http://radarr:7878", RadarrAPIKey: "k"}, true},
		{"enabled radarr with url and key both empty rejected", Config{RunMode: RunModeDaemon, radarrWanted: true, SonarrURL: "http://sonarr:8989", SonarrAPIKey: "k"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestToConfigEnabledToggleAndTrim(t *testing.T) {
	fc := defaultFileConfig()
	fc.Sonarr = arrFile{Enabled: true, URL: "  http://sonarr:8989 ", APIKey: " key "}
	fc.Radarr = arrFile{Enabled: false, URL: "http://radarr", APIKey: "rk"}
	fc.ArrTags = tagsFile{Include: []string{" anime ", ""}, Exclude: []string{"skip"}}

	c := fc.toConfig()

	if !c.sonarrWanted {
		t.Error("sonarrWanted = false, want true (sonarr.enabled must transfer to the runtime Config)")
	}
	if c.radarrWanted {
		t.Error("radarrWanted = true, want false (radarr.enabled must transfer to the runtime Config)")
	}
	if c.SonarrURL != "http://sonarr:8989" || c.SonarrAPIKey != "key" {
		t.Errorf("sonarr not trimmed: url=%q key=%q", c.SonarrURL, c.SonarrAPIKey)
	}
	if c.RadarrURL != "" || c.RadarrAPIKey != "" {
		t.Errorf("disabled radarr should be empty, got url=%q key=%q", c.RadarrURL, c.RadarrAPIKey)
	}
	if len(c.IncludeTags) != 1 || c.IncludeTags[0] != "anime" {
		t.Errorf("include tags not trimmed/filtered: %v", c.IncludeTags)
	}
	if len(c.ExcludeTags) != 1 || c.ExcludeTags[0] != "skip" {
		t.Errorf("ExcludeTags = %v, want [skip] from arr_tags.exclude", c.ExcludeTags)
	}
	if c.ReportDir != DefaultReportDir {
		t.Errorf("ReportDir = %q, want default %q", c.ReportDir, DefaultReportDir)
	}
}

// TestToConfigInfoOnDisabledArrWithKey pins the half-configuration signal: a
// disabled arr whose api_key is set (always operator-written) logs an Info at
// flatten time, while the defaults baseline (disabled, key-less) stays silent
// so a plain config boots without noise.
func TestToConfigInfoOnDisabledArrWithKey(t *testing.T) {
	t.Run("disabled arr with key logs info", func(t *testing.T) {
		rec := capture.Default(t)
		fc := defaultFileConfig()
		fc.Sonarr = arrFile{Enabled: true, URL: "http://sonarr:8989", APIKey: "sk"}
		fc.Radarr = arrFile{Enabled: false, URL: "http://radarr:7878", APIKey: "rk"}

		c := fc.toConfig()

		if c.RadarrURL != "" || c.RadarrAPIKey != "" {
			t.Errorf("disabled radarr should still be dropped, got url=%q key=%q", c.RadarrURL, c.RadarrAPIKey)
		}
		if !rec.Contains("api_key is set but the arr is not enabled") ||
			!rec.AttrContains("api_key is set but the arr is not enabled", "field", "radarr.api_key") {
			t.Errorf("toConfig log = %v, want the disabled-radarr-with-key info", rec.Messages())
		}
	})
	t.Run("default key-less disabled arr stays silent", func(t *testing.T) {
		rec := capture.Default(t)
		fc := defaultFileConfig()
		fc.Sonarr = arrFile{Enabled: true, URL: "http://sonarr:8989", APIKey: "sk"}

		fc.toConfig()

		for _, msg := range rec.Messages() {
			if strings.Contains(msg, "will not be scanned") {
				t.Errorf("toConfig logged %q for a default key-less disabled arr", msg)
			}
		}
	})
}

// TestWarnOverlappingTags pins the include/exclude overlap diagnostic: a tag
// in both arr_tags lists warns (exclude wins, so the include entry is dead),
// disjoint lists stay silent, and the warning is field-name-only — it never
// echoes the tag value, which can carry an expanded ${VAR}.
func TestWarnOverlappingTags(t *testing.T) {
	tests := []struct {
		name     string
		include  []string
		exclude  []string
		wantWarn bool
	}{
		{"overlap warns", []string{"anime", "keep"}, []string{"anime"}, true},
		{"case and whitespace still overlap", []string{" Anime "}, []string{"anime"}, true},
		{"disjoint lists stay silent", []string{"anime"}, []string{"skip"}, false},
		{"empty lists stay silent", nil, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := capture.Default(t)
			c := Config{IncludeTags: tt.include, ExcludeTags: tt.exclude}
			c.warnOverlappingTags()
			got := rec.Contains("exclude wins, so items carrying it are never scanned")
			if got != tt.wantWarn {
				t.Errorf("overlap warning present = %v, want %v (messages %v)", got, tt.wantWarn, rec.Messages())
			}
			for _, msg := range rec.Messages() {
				if strings.Contains(msg, "anime") || strings.Contains(msg, "skip") {
					t.Errorf("warning echoes a tag value: %q", msg)
				}
			}
		})
	}
}

func TestWebBaseFallsBackToInternalURL(t *testing.T) {
	withPublic := Config{SonarrURL: "http://internal:8989", SonarrPublicURL: "https://sonarr.example.com"}
	if got := withPublic.SonarrWebBase(); got != "https://sonarr.example.com" {
		t.Errorf("SonarrWebBase() = %q, want public url", got)
	}
	noPublic := Config{RadarrURL: "http://internal:7878"}
	if got := noPublic.RadarrWebBase(); got != "http://internal:7878" {
		t.Errorf("RadarrWebBase() = %q, want internal url fallback", got)
	}
}

func TestParseInterval(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantDur time.Duration
		wantExt bool
	}{
		{"off is external", "off", 0, true},
		{"disabled is external", "disabled", 0, true},
		{"zero is external", "0", 0, true},
		{"zero duration 0s is external", "0s", 0, true},
		{"valid duration", "6h", 6 * time.Hour, false},
		{"empty is default", "", DefaultPollInterval, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dur, ext := parseInterval(tt.raw)
			if ext != tt.wantExt {
				t.Errorf("parseInterval(%q) external = %v, want %v", tt.raw, ext, tt.wantExt)
			}
			if dur != tt.wantDur {
				t.Errorf("parseInterval(%q) = %v, want %v", tt.raw, dur, tt.wantDur)
			}
		})
	}
}

func TestLoadExpandsAllowlistedEnv(t *testing.T) {
	t.Setenv("SONARR_API_KEY", "sk-123")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "sonarr:\n  enabled: true\n  url: http://sonarr:8989\n  api_key: ${SONARR_API_KEY}\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.SonarrAPIKey != "sk-123" {
		t.Errorf("SonarrAPIKey = %q, want expanded env value", c.SonarrAPIKey)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate on loaded config: %v", err)
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()

	invalid := filepath.Join(dir, "invalid.yaml")
	if err := os.WriteFile(invalid, []byte("sonarr: {enabled: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oversized := filepath.Join(dir, "oversized.yaml")
	if err := os.WriteFile(oversized, make([]byte, maxConfigBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
	}{
		{"missing file", filepath.Join(dir, "does-not-exist.yaml")},
		{"invalid yaml", invalid},
		{"oversized file", oversized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Load(tt.path); err == nil {
				t.Errorf("Load(%s) = nil error, want error", tt.name)
			}
		})
	}
}

func TestValidateRejectsMalformedURLs(t *testing.T) {
	base := func() Config {
		return Config{RunMode: RunModeDaemon, SonarrURL: "http://sonarr:8989", SonarrAPIKey: "k"}
	}
	tests := []struct {
		mutate func(*Config)
		name   string
	}{
		{func(c *Config) { c.SonarrURL = "http://[::1" }, "unparseable sonarr url"},
		{func(c *Config) { c.SonarrURL = "http://sonarr:99999" }, "out-of-range sonarr port"},
		{func(c *Config) { c.SonarrURL = "http://sonarr:0" }, "port-zero sonarr url (parses but is never dialable)"},
		{func(c *Config) { c.SonarrURL = "http://sonarr:8989#" }, "bare trailing fragment sonarr url"},
		{func(c *Config) { c.SonarrURL = "http://sonarr:8989?" }, "bare trailing query sonarr url"},
		{func(c *Config) {
			c.IndexerAPIKey = "fk"
			c.IndexerNyaaTorznabURL = "http://prowlarr:0/22/api"
		}, "port-zero nyaa indexer url"},
		{func(c *Config) {
			c.IndexerAPIKey = "fk"
			c.IndexerNyaaTorznabURL = "http://prowlarr:9696/22/api#copied"
		}, "fragment-bearing nyaa indexer url"},
		{func(c *Config) {
			c.IndexerAPIKey = "fk"
			c.IndexerNyaaTorznabURL = "http://[::1"
		}, "unparseable nyaa indexer url"},
		{func(c *Config) {
			c.IndexerAPIKey = "fk"
			c.IndexerABTorznabURL = "http://[::1"
		}, "unparseable ab indexer url"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base()
			tt.mutate(&c)
			if err := c.Validate(); err == nil {
				t.Errorf("Validate() = nil error, want error for %s", tt.name)
			}
		})
	}
}

// TestValidateHTTPURLErrorOmitsCredentials pins the field-name-only posture of
// validateHTTPURL errors: neither validation branch may echo the supplied URL,
// which can carry a userinfo password, a username-only token, or a query-string
// apikey destined for the startup log (l-f4).
func TestValidateHTTPURLErrorOmitsCredentials(t *testing.T) {
	sentinels := []string{"pw-sentinel", "user-token-sentinel", "query-token-sentinel"}
	tests := map[string]string{
		"embedded password":         "ftp://user:pw-sentinel@host/path",
		"username-only token":       "ftp://user-token-sentinel@host/path",
		"query-string token":        "ftp://host/path?apikey=query-token-sentinel",
		"unparseable with userinfo": "http://user:pw-sentinel@[::1",
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			err := validateHTTPURL("sonarr.url", raw)
			if err == nil {
				t.Fatal("validateHTTPURL() = nil, want error")
			}
			for _, s := range sentinels {
				if strings.Contains(err.Error(), s) {
					t.Errorf("validateHTTPURL() error = %q, leaks %q", err, s)
				}
			}
		})
	}
}

// TestLoadTypeErrorOmitsScalarExcerpt pins the field-name-only posture of
// Load's strict pre-decode rejection: a literal scalar placed in a bool field
// is rejected by the raw-document check before expansion, and yaml.v3's
// type-mismatch error embeds a quoted excerpt of that scalar — which can be a
// pasted secret. The error must keep line/type info but never any fragment of
// the rejected value.
func TestLoadTypeErrorOmitsScalarExcerpt(t *testing.T) {
	const scalar = "super-secret-api-key-sentinel"
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "sonarr:\n  enabled: " + scalar + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() = nil error, want type-mismatch error")
	}
	if strings.Contains(err.Error(), scalar) || strings.Contains(err.Error(), "super-s") {
		t.Errorf("Load() error = %q, leaks the rejected scalar", err)
	}
	if !strings.Contains(err.Error(), "cannot unmarshal !!str <redacted> into bool") {
		t.Errorf("Load() error = %q, want the redacted wrong-type entry shape", err)
	}
}

// TestLoadTypeErrorOmitsBacktickScalar pins the value-independent redaction:
// yaml.v3 embeds the scalar excerpt with any backtick in the value unchanged,
// so a rejected scalar containing a backtick defeats backtick-pair matching
// and would leak a prefix. No fragment of the rejected value may survive
// yamlenv.SanitizeDecodeError.
func TestLoadTypeErrorOmitsBacktickScalar(t *testing.T) {
	const scalar = "zq9`vw7-secret-sentinel"
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "sonarr:\n  enabled: \"" + scalar + "\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() = nil error, want type-mismatch error")
	}
	for _, fragment := range []string{scalar, "zq9", "vw7", "secret-sentinel"} {
		if strings.Contains(err.Error(), fragment) {
			t.Errorf("Load() error leaks scalar fragment %q: %q", fragment, err)
		}
	}
	if !strings.Contains(err.Error(), "cannot unmarshal !!str <redacted> into bool") {
		t.Errorf("Load() error = %q, want the redacted wrong-type entry shape", err)
	}
}

func TestToConfigRadarrEnabledAndReportDirFallback(t *testing.T) {
	fc := defaultFileConfig()
	fc.Radarr = arrFile{Enabled: true, URL: " http://radarr:7878 ", APIKey: " rk ", PublicURL: " https://radarr.example.com "}
	fc.Report = reportFile{Dir: "   "}

	c := fc.toConfig()

	if c.RadarrURL != "http://radarr:7878" || c.RadarrAPIKey != "rk" {
		t.Errorf("enabled radarr not trimmed: url=%q key=%q", c.RadarrURL, c.RadarrAPIKey)
	}
	if c.RadarrPublicURL != "https://radarr.example.com" {
		t.Errorf("radarr public_url = %q, want trimmed", c.RadarrPublicURL)
	}
	if c.ReportDir != DefaultReportDir {
		t.Errorf("blank report dir should fall back to default, got %q", c.ReportDir)
	}
}

func TestLoadWarnsOnUnresolvedAllowlistedEnv(t *testing.T) {
	rec := capture.Default(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "sonarr:\n  enabled: true\n  url: http://sonarr:8989\n  api_key: ${SONARR_MISSING}\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SonarrAPIKey != "${SONARR_MISSING}" {
		t.Errorf("SonarrAPIKey = %q, want unresolved literal", cfg.SonarrAPIKey)
	}
	if !rec.Contains("config references environment variables") || !rec.AttrContains("", "vars", "SONARR_MISSING") {
		t.Errorf("Load unresolved-env warning = %v, want message and variable name", rec.Messages())
	}
}

// TestLoadStaysSilentWhenAllEnvResolved pins the absence side of Load's
// unresolved-${VAR}-refs warning: when every allowlisted reference resolves,
// the warning must not fire (kills the lived CONDITIONALS_BOUNDARY mutant on
// the len(refs) > 0 guard).
func TestLoadStaysSilentWhenAllEnvResolved(t *testing.T) {
	rec := capture.Default(t)
	t.Setenv("SONARR_API_KEY", "sk-123")
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "sonarr:\n  enabled: true\n  url: http://sonarr:8989\n  api_key: ${SONARR_API_KEY}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if rec.Contains("config references environment variables") {
		t.Errorf("Load logged the unresolved-env warning for a fully resolved config: %v", rec.Messages())
	}
}

func TestParseLogLevelWarnsOnUnrecognizedValue(t *testing.T) {
	rec := capture.Default(t)

	if got := parseLogLevel("verbose"); got != slog.LevelInfo {
		t.Errorf("parseLogLevel() = %v, want info fallback", got)
	}
	if !rec.Contains("unrecognized log.level") {
		t.Errorf("parseLogLevel warning = %v, want message", rec.Messages())
	}
	// Field-name-only: the rejected value may be an expanded ${VAR} secret and
	// must never ride the warning (h-f13).
	if rec.AttrContains("", "", "verbose") {
		t.Errorf("parseLogLevel warning echoes the rejected value: %v", rec.Messages())
	}
}

// TestParseLogLevelAcceptedValues pins the accepted half of log.level's parse:
// every spelling an operator actually configures (including the long-form
// "warning" alias, case folding, and surrounding whitespace) must come back as
// its own level, silently. Without these rows the level parse could discard
// slogx.ParseLevel's result and return a constant Info while the whole suite
// stayed green, so log.level would silently stop working.
func TestParseLogLevelAcceptedValues(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want slog.Level
	}{
		{"debug", "debug", slog.LevelDebug},
		{"info", "info", slog.LevelInfo},
		{"mixed case and padding", " WARN ", slog.LevelWarn},
		{"long-form warning alias", "warning", slog.LevelWarn},
		{"error", "error", slog.LevelError},
		{"empty defaults silently", "", slog.LevelInfo},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := capture.Default(t)
			if got := parseLogLevel(tt.in); got != tt.want {
				t.Errorf("parseLogLevel(%q) = %v, want %v", tt.in, got, tt.want)
			}
			if rec.Contains("unrecognized log.level") {
				t.Errorf("parseLogLevel(%q) warned on an accepted value: %v", tt.in, rec.Messages())
			}
		})
	}
}

func TestParseLogFormatWarnsOnUnrecognizedValue(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		want     slogx.Format
		wantWarn bool
	}{
		{"json accepted", "json", slogx.JSON, false},
		{"text accepted", "text", slogx.Text, false},
		{"mixed case trimmed and normalized", " TEXT ", slogx.Text, false},
		{"empty defaults silently", "", slogx.JSON, false},
		{"unrecognized warns and falls back", "txt", slogx.JSON, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := capture.Default(t)
			if got := parseLogFormat(tt.in); got != tt.want {
				t.Errorf("parseLogFormat(%q) = %v, want %v", tt.in, got, tt.want)
			}
			if tt.wantWarn && !rec.Contains("unrecognized log.format") {
				t.Errorf("parseLogFormat warning = %v, want message", rec.Messages())
			}
			// Field-name-only: the rejected value may be an expanded ${VAR}
			// secret and must never ride the warning (h-f13).
			if tt.wantWarn && rec.AttrContains("", "", "txt") {
				t.Errorf("parseLogFormat warning echoes the rejected value: %v", rec.Messages())
			}
			if !tt.wantWarn && rec.Contains("unrecognized log.format") {
				t.Errorf("parseLogFormat(%q) warned unexpectedly: %v", tt.in, rec.Messages())
			}
		})
	}
}

// TestConfigDiagnosticsOmitExpandedSecrets pins the field-name-only posture of
// every diagnostic a misplaced ${VAR} credential can reach: a secret expanded
// into log.level, log.format, mode, or poll_interval must never appear in the
// warning/error corpus, while each field still falls back per its contract
// (h-f13, CWE-532).
func TestConfigDiagnosticsOmitExpandedSecrets(t *testing.T) {
	const secret = "credential-sentinel-7f3a"
	t.Setenv("SONARR_API_KEY", secret)
	const arr = "sonarr:\n  enabled: true\n  url: http://sonarr:8989\n  api_key: k\n"

	tests := []struct {
		check       func(t *testing.T, c *Config)
		name        string
		content     string
		wantInvalid bool
	}{
		{name: "log.level", content: arr + "log:\n  level: ${SONARR_API_KEY}\n", check: func(t *testing.T, c *Config) {
			t.Helper()
			if c.LogLevel != slog.LevelInfo {
				t.Errorf("LogLevel = %v, want info fallback", c.LogLevel)
			}
		}},
		{name: "log.format", content: arr + "log:\n  format: ${SONARR_API_KEY}\n", check: func(t *testing.T, c *Config) {
			t.Helper()
			if c.LogFormat != slogx.JSON {
				t.Errorf("LogFormat = %v, want JSON fallback", c.LogFormat)
			}
		}},
		{name: "mode", content: arr + "mode: ${SONARR_API_KEY}\n", wantInvalid: true},
		{name: "poll_interval", content: arr + "poll_interval: ${SONARR_API_KEY}\n", check: func(t *testing.T, c *Config) {
			t.Helper()
			if c.PollInterval != DefaultPollInterval || c.PollExternal {
				t.Errorf("PollInterval = %v external=%v, want default built-in", c.PollInterval, c.PollExternal)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := capture.Default(t)
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			verr := cfg.Validate()
			if tt.wantInvalid && verr == nil {
				t.Error("Validate() = nil error, want rejection")
			}
			if !tt.wantInvalid && verr != nil {
				t.Errorf("Validate() = %v, want nil (field falls back, config still loads)", verr)
			}
			if tt.check != nil {
				tt.check(t, &cfg)
			}
			var corpus strings.Builder
			corpus.WriteString(strings.Join(rec.Messages(), "\n"))
			for _, r := range rec.Records() {
				r.Attrs(func(a slog.Attr) bool {
					corpus.WriteByte('\n')
					corpus.WriteString(a.Key)
					corpus.WriteByte('=')
					corpus.WriteString(a.Value.String())
					return true
				})
			}
			if verr != nil {
				corpus.WriteByte('\n')
				corpus.WriteString(verr.Error())
			}
			text := corpus.String()
			if strings.Contains(strings.ToLower(text), secret) {
				t.Errorf("%s diagnostic corpus leaks the expanded secret:\n%s", tt.name, text)
			}
		})
	}
}

// TestValidateWarnsOnMalformedPublicURL pins the documented non-fatal contract
// for malformed sonarr/radarr public_url values: Validate warns that report
// deep-links will be broken but still accepts the config (l-f6).
func TestValidateWarnsOnMalformedPublicURL(t *testing.T) {
	tests := map[string]Config{
		"sonarr public url": {
			RunMode: RunModeDaemon, SonarrURL: "http://sonarr:8989", SonarrAPIKey: "k",
			SonarrPublicURL: "://bad",
		},
		"radarr public url": {
			RunMode: RunModeReport, RadarrURL: "http://radarr:7878", RadarrAPIKey: "k",
			RadarrPublicURL: "http://[::1",
		},
	}
	for name, cfg := range tests {
		t.Run(name, func(t *testing.T) {
			rec := capture.Default(t)
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate() error = %v, want malformed public_url to remain non-fatal", err)
			}
			if !rec.Contains("public_url is malformed; report deep-links will be broken") {
				t.Errorf("Validate() log = %v, want malformed-public-url warning", rec.Messages())
			}
		})
	}
}

// TestValidateSilentOnCleanOrEmptyPublicURL pins the absence side of the
// malformed-public_url warning: a clean or empty public_url must not warn
// (kills the lived CONDITIONALS_NEGATION mutant on the err != nil guard,
// which would warn on every clean config).
func TestValidateSilentOnCleanOrEmptyPublicURL(t *testing.T) {
	tests := map[string]Config{
		"empty public urls": {RunMode: RunModeDaemon, SonarrURL: "http://sonarr:8989", SonarrAPIKey: "k"},
		"well-formed public url": {
			RunMode: RunModeDaemon, SonarrURL: "http://sonarr:8989", SonarrAPIKey: "k",
			SonarrPublicURL: "https://sonarr.example.com",
		},
	}
	for name, cfg := range tests {
		t.Run(name, func(t *testing.T) {
			rec := capture.Default(t)
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if rec.Contains("public_url is malformed") {
				t.Errorf("Validate() warned malformed public_url: %v", rec.Messages())
			}
		})
	}
}

// TestValidateWarnsOnSmuggledPublicURL pins the urlform-backed half of
// warnPublicURLProblems: a public_url carrying a backslash or an embedded
// tab/newline parses fine for net/url but is refused outright by the deep-link
// publisher (library.SafeLogURL reads urlform.Classify), so the config warns
// that report rows will carry no arr link. A clean value stays silent.
func TestValidateWarnsOnSmuggledPublicURL(t *testing.T) {
	const want = "public_url carries a backslash or an embedded tab/newline"
	tests := map[string]struct {
		cfg  Config
		warn bool
	}{
		"path backslash": {cfg: Config{
			RunMode: RunModeDaemon, SonarrURL: "http://sonarr:8989", SonarrAPIKey: "k",
			SonarrPublicURL: `http://sonarr.example.com/base\x`,
		}, warn: true},
		"clean public url": {cfg: Config{
			RunMode: RunModeDaemon, SonarrURL: "http://sonarr:8989", SonarrAPIKey: "k",
			SonarrPublicURL: "https://sonarr.example.com/base",
		}, warn: false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			rec := capture.Default(t)
			if err := tc.cfg.Validate(); err != nil {
				t.Fatalf("Validate() error = %v, want a smuggled public_url to remain non-fatal", err)
			}
			if got := rec.Contains(want); got != tc.warn {
				t.Errorf("Validate() warned %v, want %v; log = %v", got, tc.warn, rec.Messages())
			}
		})
	}
}

// TestValidateWarnsOnIdenticalArrURLs pins warnIdenticalArrURLs' warn-only
// contract: identical sonarr.url/radarr.url values warn (a paste error - one
// client queries the wrong application), distinct values stay silent, and the
// warning is field-name-only (never echoes a URL).
func TestValidateWarnsOnIdenticalArrURLs(t *testing.T) {
	t.Run("identical arr urls warn", func(t *testing.T) {
		rec := capture.Default(t)
		cfg := Config{
			RunMode:   RunModeDaemon,
			SonarrURL: "http://arr:8989", SonarrAPIKey: "sk",
			RadarrURL: "http://arr:8989", RadarrAPIKey: "rk",
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate() error = %v, want identical arr urls to remain warn-only", err)
		}
		if !rec.Contains("sonarr.url and radarr.url are identical") {
			t.Errorf("Validate() log = %v, want the identical-arr-url warning", rec.Messages())
		}
		for _, m := range rec.Messages() {
			if strings.Contains(m, "http://arr:8989") {
				t.Errorf("Validate() log echoes the URL: %q", m)
			}
		}
		if rec.AttrContains("", "", "http://arr:8989") {
			t.Errorf("Validate() structured attributes echo the URL: %v", rec.Messages())
		}
	})
	t.Run("distinct arr urls stay silent", func(t *testing.T) {
		rec := capture.Default(t)
		cfg := Config{
			RunMode:   RunModeDaemon,
			SonarrURL: "http://sonarr:8989", SonarrAPIKey: "sk",
			RadarrURL: "http://radarr:7878", RadarrAPIKey: "rk",
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if rec.Contains("sonarr.url and radarr.url are identical") {
			t.Errorf("Validate() log = %v, want no identical-arr-url warning", rec.Messages())
		}
	})
}

func TestValidateIndexerProwlarrKeyWarning(t *testing.T) {
	base := Config{
		RunMode: RunModeDaemon, SonarrURL: "http://s", SonarrAPIKey: "k",
		IndexerAPIKey: "fk", IndexerNyaaTorznabURL: "http://prowlarr:9696/22/api",
	}

	t.Run("empty prowlarr key warns", func(t *testing.T) {
		rec := capture.Default(t)
		c := base
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if !rec.Contains("prowlarr_api_key is empty") {
			t.Errorf("Validate() log = %v, want the empty prowlarr_api_key warning", rec.Messages())
		}
	})
	t.Run("set prowlarr key does not warn", func(t *testing.T) {
		rec := capture.Default(t)
		c := base
		c.IndexerProwlarrAPIKey = "pk"
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if rec.Contains("prowlarr_api_key") {
			t.Errorf("Validate() log = %v, want no prowlarr_api_key warning", rec.Messages())
		}
	})
}

// TestValidateIndexerHalfConfiguredInfo pins the half-configuration signal:
// indexer secrets set without any torznab URL log an Info naming the missing
// URLs (the feed would otherwise silently not start), while a fully-empty
// indexer section stays silent. Info, not Warn - deliberately parked keys
// must not raise Loki alert noise.
func TestValidateIndexerHalfConfiguredInfo(t *testing.T) {
	base := Config{RunMode: RunModeDaemon, SonarrURL: "http://s", SonarrAPIKey: "k"}

	t.Run("keys without torznab url log info", func(t *testing.T) {
		rec := capture.Default(t)
		c := base
		c.IndexerProwlarrAPIKey = "pk"
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if !rec.Contains("indexer keys are set but no torznab url is configured") {
			t.Errorf("Validate() log = %v, want the half-configured indexer info", rec.Messages())
		}
	})
	t.Run("a starter-seeded feed key alone stays silent", func(t *testing.T) {
		rec := capture.Default(t)
		c := base
		c.IndexerAPIKey = strings.Repeat("a", 32) // what seedFeedAPIKey writes
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if rec.Contains("indexer keys are set but no torznab url is configured") {
			t.Errorf("Validate() log = %v, want no half-configured indexer info", rec.Messages())
		}
	})
	t.Run("empty indexer section stays silent", func(t *testing.T) {
		rec := capture.Default(t)
		c := base
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if rec.Contains("indexer keys are set but no torznab url is configured") {
			t.Errorf("Validate() log = %v, want no half-configured indexer info", rec.Messages())
		}
	})
	// The mode/feature half-configuration: a torznab URL with mode report - the
	// feed is served only by the daemon, so it silently never starts. Info,
	// same no-Loki-noise posture as the other half-configuration signals.
	feedBase := base
	feedBase.IndexerNyaaTorznabURL = "http://prowlarr:9696/22/api"
	feedBase.IndexerAPIKey = strings.Repeat("a", 32)
	feedBase.IndexerProwlarrAPIKey = "pk"
	t.Run("torznab url with mode report logs info", func(t *testing.T) {
		rec := capture.Default(t)
		c := feedBase
		c.RunMode = RunModeReport
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if !rec.Contains("indexer torznab urls are set but mode is report") {
			t.Errorf("Validate() log = %v, want the report-mode indexer info", rec.Messages())
		}
	})
	t.Run("torznab url with mode daemon stays silent", func(t *testing.T) {
		rec := capture.Default(t)
		c := feedBase
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if rec.Contains("indexer torznab urls are set but mode is report") {
			t.Errorf("Validate() log = %v, want no report-mode indexer info", rec.Messages())
		}
	})
}

// TestValidateIndexerParkedABPasskeyInfo pins the inverse half-configuration
// signal inside a configured feed: indexer.ab_passkey set while
// indexer.ab_torznab_url is empty (the feed otherwise configured via
// nyaa_torznab_url) logs an Info naming the inert passkey - the AB URL is the
// AnimeBytes on switch, so the passkey is otherwise silently unused. Info,
// not Warn, mirroring infoDisabledIndexerKeys: a deliberately parked passkey
// must not raise Loki alert noise. Silent when ab_torznab_url is also set.
func TestValidateIndexerParkedABPasskeyInfo(t *testing.T) {
	base := Config{
		RunMode: RunModeDaemon, SonarrURL: "http://s", SonarrAPIKey: "k",
		IndexerNyaaTorznabURL: "http://prowlarr:9696/22/api",
		IndexerAPIKey:         strings.Repeat("a", 32),
		IndexerProwlarrAPIKey: "pk",
		IndexerABPasskey:      testABPasskey,
	}

	t.Run("passkey without ab url logs info", func(t *testing.T) {
		rec := capture.Default(t)
		c := base
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if !rec.Contains("indexer.ab_passkey is set but indexer.ab_torznab_url is empty") {
			t.Errorf("Validate() log = %v, want the parked-passkey info", rec.Messages())
		}
	})
	t.Run("passkey with ab url stays silent", func(t *testing.T) {
		rec := capture.Default(t)
		c := base
		c.IndexerABTorznabURL = "http://prowlarr:9696/2/api"
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if rec.Contains("indexer.ab_passkey is set but indexer.ab_torznab_url is empty") {
			t.Errorf("Validate() log = %v, want no parked-passkey info", rec.Messages())
		}
	})
	// The third AB half-configuration: the indexer's AB endpoint configured
	// while the top-level animebytes toggle stays at its false default. The feed
	// then serves AnimeBytes releases the findings and the report both drop.
	t.Run("ab url with animebytes false logs info", func(t *testing.T) {
		rec := capture.Default(t)
		c := base
		c.IndexerABTorznabURL = "http://prowlarr:9696/2/api"
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if !rec.Contains("indexer.ab_torznab_url is set but animebytes is false") {
			t.Errorf("Validate() log = %v, want the animebytes-off info", rec.Messages())
		}
	})
	t.Run("ab url with animebytes true stays silent", func(t *testing.T) {
		rec := capture.Default(t)
		c := base
		c.IndexerABTorznabURL = "http://prowlarr:9696/2/api"
		c.AnimeBytes = true
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if rec.Contains("indexer.ab_torznab_url is set but animebytes is false") {
			t.Errorf("Validate() log = %v, want no animebytes-off info", rec.Messages())
		}
	})
}

func TestValidateIndexerWarnsOnIdenticalTorznabURLs(t *testing.T) {
	rec := capture.Default(t)
	const upstream = "http://prowlarr:9696/22/api"
	cfg := Config{
		RunMode: RunModeDaemon, SonarrURL: "http://sonarr:8989", SonarrAPIKey: "k",
		IndexerAPIKey: strings.Repeat("a", 16), IndexerProwlarrAPIKey: "pk", IndexerABPasskey: testABPasskey,
		IndexerNyaaTorznabURL: upstream, IndexerABTorznabURL: upstream,
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want identical endpoints to remain warn-only", err)
	}
	if !rec.Contains("indexer.nyaa_torznab_url and indexer.ab_torznab_url are identical") {
		t.Errorf("Validate() log = %v, want the identical-endpoint warning", rec.Messages())
	}
}

// TestValidateIndexerDistinctTorznabURLsStaySilent pins the absence side of
// the identical-torznab-endpoints warning: two distinct per-indexer Prowlarr
// endpoints (the correct configuration) must not fire it.
func TestValidateIndexerDistinctTorznabURLsStaySilent(t *testing.T) {
	rec := capture.Default(t)
	cfg := Config{
		RunMode: RunModeDaemon, SonarrURL: "http://sonarr:8989", SonarrAPIKey: "k",
		IndexerAPIKey: strings.Repeat("a", 16), IndexerProwlarrAPIKey: "pk", IndexerABPasskey: testABPasskey,
		IndexerNyaaTorznabURL: "http://prowlarr:9696/22/api",
		IndexerABTorznabURL:   "http://prowlarr:9696/2/api",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want distinct per-indexer endpoints to validate", err)
	}
	if rec.Contains("are identical") {
		t.Errorf("Validate() log = %v, want no identical-endpoint warning for distinct per-indexer URLs", rec.Messages())
	}
}

// TestValidateIndexerShortFeedKeyWarning pins the two strength rules on
// indexer.feed_api_key: the warn-only floor (a key under 16 characters warns
// because it gates the AnimeBytes-passkey-bearing feed, a strong key stays
// silent) and the hard rejection of an unresolved ${VAR} placeholder, which is
// a guessable credential rather than a weak one. Both stay field-name-only: the
// key value never rides the log record or the error.
func TestValidateIndexerShortFeedKeyWarning(t *testing.T) {
	base := Config{
		RunMode: RunModeDaemon, SonarrURL: "http://s", SonarrAPIKey: "k",
		IndexerNyaaTorznabURL: "http://prowlarr:9696/22/api", IndexerProwlarrAPIKey: "pk",
	}

	t.Run("short key warns without value", func(t *testing.T) {
		const shortKey = "hunter2"
		rec := capture.Default(t)
		c := base
		c.IndexerAPIKey = shortKey
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if !rec.Contains("feed_api_key is shorter than 16 characters") {
			t.Errorf("Validate() log = %v, want the short feed_api_key warning", rec.Messages())
		}
		corpus := strings.Join(rec.Messages(), "\n")
		if strings.Contains(corpus, shortKey) || rec.AttrContains("", "", shortKey) {
			t.Errorf("Validate() log leaks the key value: %v", rec.Messages())
		}
	})
	t.Run("32-char key does not warn", func(t *testing.T) {
		rec := capture.Default(t)
		c := base
		c.IndexerAPIKey = strings.Repeat("a", 32)
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if rec.Contains("feed_api_key is shorter") {
			t.Errorf("Validate() log = %v, want no short-key warning", rec.Messages())
		}
	})
	// An unexpanded ${VAR} is not a weak key, it is a GUESSABLE one: the
	// placeholder spelling ships in the public config.example, and this key is
	// the only gate on the AnimeBytes-passkey-bearing feed. It fails the config
	// rather than warning, and the error stays field-name-only.
	t.Run("unresolved placeholder is an error", func(t *testing.T) {
		const placeholder = "${SEADEX_SCOUT_FEED_API_KEY}"
		c := base
		c.IndexerAPIKey = placeholder
		err := c.Validate()
		if err == nil {
			t.Fatal("Validate() = nil for an unresolved ${VAR} feed_api_key, want an error")
		}
		if !strings.Contains(err.Error(), "feed_api_key") {
			t.Errorf("Validate() error = %v, want it to name feed_api_key", err)
		}
		if strings.Contains(err.Error(), "SEADEX_SCOUT_FEED_API_KEY") {
			t.Errorf("Validate() error echoes the configured value: %v", err)
		}
	})
}

// TestValidateIndexerRejectsMalformedFeedKey pins the config boundary's ONE
// gate on indexer.feed_api_key (validateFeedAPIKey). This key IS the feed's
// authentication, and the /ab RSS body embeds the operator's AnimeBytes passkey
// in every download link, so a key that is not a key is refused at startup
// rather than warned about: internal/indexer refuses to bind behind an
// unexpanded reference (unusableFeedKey), and the app must not validate clean
// and then never serve the feed.
//
// The gate is POSITIVE - one run of printable characters, no whitespace, no
// '$' - so it refuses every unexpanded-reference spelling at once, including
// the unterminated "${NAME" paste no reference regex models. That makes config's
// acceptance set a SUBSET of what the runtime will serve behind, which is the
// safe direction for two gates on one credential. The cost, pinned here
// deliberately: a hand-typed key containing '$' is refused too, and the message
// says to generate one instead.
//
// Field-name-only on every arm: the key value never rides the error or the log.
func TestValidateIndexerRejectsMalformedFeedKey(t *testing.T) {
	base := Config{
		RunMode: RunModeDaemon, SonarrURL: "http://s", SonarrAPIKey: "k",
		IndexerNyaaTorznabURL: "http://prowlarr:9696/22/api", IndexerProwlarrAPIKey: "pk",
	}
	for name, tc := range map[string]struct {
		key       string
		wantError bool
	}{
		// Every spelling an operator can leave behind, whether or not the name
		// is allowlisted: yamlenv leaves a non-allowlisted name just as literal.
		"braced reference is refused":                {"${SEADEX_SCOUT_FEED_API_KEY}", true},
		"brace-less reference is refused":            {"$SEADEX_SCOUT_FEED_API_KEY", true},
		"brace-less non-allowlisted ref is refused":  {"$PROWLARR_FEED_API_KEY", true},
		"unterminated braced paste is refused":       {"${SEADEX_SCOUT_FEED_API_KEY", true},
		"a reference inside a longer value refused":  {"pre$SEADEX_SCOUT_FEED_API_KEY", true},
		"a key merely carrying a dollar is refused":  {"abc$def-0f1e2d3c4b5a6978", true},
		"an embedded space is refused":               {"0f1e2d3c4b5a6978 8796a5b4c3d2", true},
		"a real 32-char key passes":                  {strings.Repeat("a", 32), false},
		"a short key passes the gate and warns only": {"hunter2", false},
	} {
		t.Run(name, func(t *testing.T) {
			rec := capture.Default(t)
			c := base
			c.IndexerAPIKey = tc.key
			err := c.Validate()
			if (err != nil) != tc.wantError {
				t.Fatalf("Validate() error = %v, want an error: %v", err, tc.wantError)
			}
			if err != nil {
				if !strings.Contains(err.Error(), "indexer.feed_api_key") {
					t.Errorf("Validate() error = %v, want it to name indexer.feed_api_key", err)
				}
				if strings.Contains(err.Error(), tc.key) {
					t.Errorf("Validate() error echoes the configured feed_api_key: %v", err)
				}
				return
			}
			if corpus := strings.Join(rec.Messages(), "\n"); strings.Contains(corpus, tc.key) ||
				rec.AttrContains("", "", tc.key) {
				t.Errorf("Validate() log leaks the configured feed_api_key value: %v", rec.Messages())
			}
		})
	}
}

// TestValidateIndexerWarnsOnNonTorznabEndpoint pins the endpoint-shape
// diagnostic: a bare Prowlarr origin and Prowlarr's REST API path are the two
// documented paste errors that load cleanly and then fail every proxied
// search, so both warn (naming the field, never the URL) while a real
// per-indexer Torznab path stays silent.
func TestValidateIndexerWarnsOnNonTorznabEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		wantWarn bool
	}{
		{"bare Prowlarr origin warns", "http://prowlarr:9696", true},
		{"Prowlarr REST API path warns", "http://prowlarr:9696/api/v1/search", true},
		{"per-indexer Torznab path stays silent", "http://prowlarr:9696/22/api", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := capture.Default(t)
			cfg := Config{
				RunMode:               RunModeDaemon,
				SonarrURL:             "http://sonarr:8989",
				SonarrAPIKey:          "k",
				IndexerAPIKey:         strings.Repeat("a", 32),
				IndexerNyaaTorznabURL: tt.endpoint,
				IndexerProwlarrAPIKey: "pk",
			}

			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			got := rec.Contains("torznab url is not a Prowlarr per-indexer Torznab endpoint")
			if got != tt.wantWarn {
				t.Errorf("endpoint-shape warning present = %v, want %v (messages %v)", got, tt.wantWarn, rec.Messages())
			}
			if got && !rec.AttrContains("", "field", "indexer.nyaa_torznab_url") {
				t.Errorf("Validate() log = %v, want the warning to name indexer.nyaa_torznab_url", rec.Messages())
			}
		})
	}
}

func TestParseIntervalBoundsAndFallback(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantDur time.Duration
		wantExt bool
	}{
		{"below minimum clamps up to the floor", "5m", minPollInterval, false},
		{"above maximum clamps down", "9000h", maxPollInterval, false},
		{"minimum itself passes unclamped", "15m", minPollInterval, false},
		{"above the minimum passes unclamped", "30m", 30 * time.Minute, false},
		{"negative falls back to default", "-5h", DefaultPollInterval, false},
		{"unparseable falls back to default", "every day", DefaultPollInterval, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dur, ext := parseInterval(tt.raw)
			if ext != tt.wantExt {
				t.Errorf("parseInterval(%q) external = %v, want %v", tt.raw, ext, tt.wantExt)
			}
			if dur != tt.wantDur {
				t.Errorf("parseInterval(%q) = %v, want %v", tt.raw, dur, tt.wantDur)
			}
		})
	}
}

func TestToConfigNormalizesModeAndLogFormat(t *testing.T) {
	fc := defaultFileConfig()
	fc.Mode = "  DAEMON "
	fc.Log.Format = " JSON "

	c := fc.toConfig()

	if c.RunMode != RunModeDaemon {
		t.Errorf("RunMode = %q, want normalized %q", c.RunMode, RunModeDaemon)
	}
	if c.LogFormat != slogx.JSON {
		t.Errorf("LogFormat = %v, want normalized JSON", c.LogFormat)
	}
}

// TestToConfigWiresLogLevel pins the flatten-site wiring of log.level into
// Config.LogLevel, the twin of the LogFormat assertion in
// TestToConfigNormalizesModeAndLogFormat: every other test only ever expects
// the Info fallback, so nothing currently fails if the assignment stops
// reading fc.Log.Level.
func TestToConfigWiresLogLevel(t *testing.T) {
	fc := defaultFileConfig()
	fc.Log.Level = " DEBUG "

	c := fc.toConfig()

	if c.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want debug from log.level", c.LogLevel)
	}
}

// TestToConfigWiresToggles pins the flatten-site wiring of every boolean the
// operator sets in the file: each toggle is set alone, so a dropped assignment
// AND a crossed one (exclude_remux reading require_dual_audio) both fail.
// Nothing else in the suite reads these four fields, so the whole wire can be
// cut while every test stays green and the configured filter does nothing.
func TestToConfigWiresToggles(t *testing.T) {
	tests := []struct {
		name string
		set  func(*fileConfig)
		get  func(Config) bool
	}{
		{"filters.exclude_remux", func(fc *fileConfig) { fc.Filters.ExcludeRemux = true }, func(c Config) bool { return c.ExcludeRemux }},
		{"filters.require_dual_audio", func(fc *fileConfig) { fc.Filters.RequireDualAudio = true }, func(c Config) bool { return c.RequireDualAudio }},
		{"filters.exclude_specials", func(fc *fileConfig) { fc.Filters.ExcludeSpecials = true }, func(c Config) bool { return c.ExcludeSpecials }},
		{"animebytes", func(fc *fileConfig) { fc.AnimeBytes = true }, func(c Config) bool { return c.AnimeBytes }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := defaultFileConfig()
			if tt.get(base.toConfig()) {
				t.Errorf("%s = true for a default config, want false", tt.name)
			}
			fc := defaultFileConfig()
			tt.set(&fc)
			c := fc.toConfig()
			if !tt.get(c) {
				t.Errorf("%s = false after setting it in the file, want true", tt.name)
			}
			for _, other := range tests {
				if other.name != tt.name && other.get(c) {
					t.Errorf("setting %s also flipped %s (crossed wire)", tt.name, other.name)
				}
			}
		})
	}
}

func TestExampleConfigMatchesLoader(t *testing.T) {
	path, err := filepath.Abs(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("resolve example path: %v", err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load(config.example.yaml): %v", err)
	}
	if err := c.Validate(); err == nil {
		t.Fatal("Validate() = nil, want the missing sonarr.api_key error the starter ships with")
	} else if !strings.Contains(err.Error(), "sonarr.api_key") {
		t.Errorf("Validate() error = %v, want it to name sonarr.api_key", err)
	}
	if c.PollInterval != DefaultPollInterval || c.PollExternal {
		t.Errorf("PollInterval = %v external=%v, want built-in %v", c.PollInterval, c.PollExternal, DefaultPollInterval)
	}
	if c.RunMode != RunModeDaemon {
		t.Errorf("RunMode = %q, want %q", c.RunMode, RunModeDaemon)
	}
	if c.ReportDir != DefaultReportDir {
		t.Errorf("ReportDir = %q, want %q", c.ReportDir, DefaultReportDir)
	}
	// The starter ships filters.exclude_tags empty, which must mean NOTHING is
	// filtered: a release SeaDex tagged Broken reaches all three surfaces until
	// the operator lists the tag.
	for _, s := range []tagfilter.Surface{
		tagfilter.SurfaceFindings, tagfilter.SurfaceReport, tagfilter.SurfaceFeed,
	} {
		if c.TagFilter.Excludes([]string{"Broken", "Incomplete"}, s) {
			t.Errorf("the shipped starter filters a warned release from %s", s)
		}
	}
}

// TestLoadEnvValueWithYAMLSyntax pins the ${VAR} contract for values carrying
// YAML syntax: expansion happens on parsed string nodes, so a quote or newline
// in an environment value stays scalar content instead of breaking the
// document structure (h-f4).
func TestLoadEnvValueWithYAMLSyntax(t *testing.T) {
	t.Setenv("SONARR_API_KEY", "key\"withquote\nand-newline")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "sonarr:\n  enabled: true\n  url: \"http://sonarr:8989\"\n  api_key: \"${SONARR_API_KEY}\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SonarrAPIKey != "key\"withquote\nand-newline" {
		t.Fatalf("SonarrAPIKey = %q", cfg.SonarrAPIKey)
	}
}

// TestLoadRejectsUnknownKeys pins the strict unknown-key contract of Load
// (h-f12): a misspelled or misplaced key fails the load with the offending key
// named and its line kept, instead of being silently ignored (the reproduced
// case: a config with top-level anime_bytes plus filters.animebytes loaded and
// validated while Config.AnimeBytes stayed false).
func TestLoadRejectsUnknownKeys(t *testing.T) {
	const arr = "sonarr:\n  enabled: true\n  url: http://sonarr:8989\n  api_key: k\n"
	tests := []struct {
		name    string
		content string
		wants   []string
	}{
		{
			name:    "misspelled top-level key",
			content: arr + "anime_bytes: true\n",
			wants:   []string{`line 5: unknown configuration key "anime_bytes"`},
		},
		{
			name:    "misplaced nested key",
			content: arr + "filters:\n  animebytes: true\n",
			wants:   []string{`line 6: unknown configuration key "animebytes"`},
		},
		{
			name:    "reproduced double miss reports both keys",
			content: arr + "anime_bytes: true\nfilters:\n  animebytes: true\n",
			wants: []string{
				`unknown configuration key "anime_bytes"`,
				`unknown configuration key "animebytes"`,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil {
				t.Fatal("Load() = nil error, want unknown-key rejection")
			}
			for _, want := range tt.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Load() error = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}

// TestLoadRejectsMistypedKeys pins the doc.Decode error branch of Load: a
// structurally valid YAML document whose value types do not fit the config
// shape must fail loudly, not half-load onto the defaults.
func TestLoadRejectsMistypedKeys(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name    string
		content string
	}{
		{"sequence where mapping expected", "sonarr: [1, 2]\n"},
		{"string where bool expected", "animebytes: definitely\n"},
		{"mapping where string expected", "poll_interval: {h: 3}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tt.name, " ", "-")+".yaml")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Errorf("Load(%s) = nil error, want decode error", tt.name)
			}
		})
	}
}

// TestLoadRejectsMultiDocumentConfig pins the single-document contract of
// Load (l-f66): yaml.Unmarshal and the strict unknown-key pre-decode both
// consume only the first YAML document, so a stray "---" separator used to
// silently drop every section below it. Load must reject a multi-document
// file loudly — including the empty trailing document a stray end-of-file
// separator produces — while trailing whitespace/comments and a leading
// document-start marker (both still single-document files) keep loading.
// The check itself is yamlenv.CheckSingleDocument; this is the consumer
// contract pin, asserting its static sentinel surfaces through Load's wrap.
func TestLoadRejectsMultiDocumentConfig(t *testing.T) {
	const arr = "sonarr:\n  enabled: true\n  url: http://sonarr:8989\n  api_key: k\n"
	const wantMsg = "more than one YAML document; remove the '---' separator"
	write := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	rejected := map[string]string{
		"second document":    arr + "---\nradarr:\n  enabled: true\n  url: http://radarr:7878\n  api_key: rk\n",
		"trailing separator": arr + "---\n",
	}
	for name, content := range rejected {
		t.Run(name+" rejected", func(t *testing.T) {
			_, err := Load(write(t, content))
			if err == nil {
				t.Fatal("Load() = nil error, want multi-document rejection")
			}
			if !strings.Contains(err.Error(), wantMsg) {
				t.Errorf("Load() error = %q, want it to contain %q", err, wantMsg)
			}
		})
	}

	loaded := map[string]string{
		"trailing whitespace and comments": arr + "\n\n# trailing comment\n   \n",
		"leading document-start marker":    "---\n" + arr,
	}
	for name, content := range loaded {
		t.Run(name+" still loads", func(t *testing.T) {
			c, err := Load(write(t, content))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if c.SonarrAPIKey != "k" {
				t.Errorf("SonarrAPIKey = %q, want %q (first document must load intact)", c.SonarrAPIKey, "k")
			}
		})
	}
}

// TestToConfigTrimsIndexerFields asserts the five indexer settings - secrets
// and URLs pasted into YAML - are trimmed like the arr fields.
func TestToConfigTrimsIndexerFields(t *testing.T) {
	fc := defaultFileConfig()
	fc.Indexer = indexerFile{
		FeedAPIKey:     " fk ",
		NyaaTorznabURL: "\thttp://prowlarr:9696/22/api ",
		ABTorznabURL:   " http://prowlarr:9696/2/api\n",
		ProwlarrAPIKey: " pk ",
		ABPasskey:      " passkey ",
	}

	c := fc.toConfig()

	if c.IndexerAPIKey != "fk" {
		t.Errorf("IndexerAPIKey = %q, want trimmed %q", c.IndexerAPIKey, "fk")
	}
	if c.IndexerNyaaTorznabURL != "http://prowlarr:9696/22/api" {
		t.Errorf("IndexerNyaaTorznabURL = %q, want trimmed", c.IndexerNyaaTorznabURL)
	}
	if c.IndexerABTorznabURL != "http://prowlarr:9696/2/api" {
		t.Errorf("IndexerABTorznabURL = %q, want trimmed", c.IndexerABTorznabURL)
	}
	if c.IndexerProwlarrAPIKey != "pk" {
		t.Errorf("IndexerProwlarrAPIKey = %q, want trimmed %q", c.IndexerProwlarrAPIKey, "pk")
	}
	if c.IndexerABPasskey != "passkey" {
		t.Errorf("IndexerABPasskey = %q, want trimmed %q", c.IndexerABPasskey, "passkey")
	}
}

// TestLoadExpandsEnvInSequenceValues pins that ${VAR} expansion reaches string
// scalars inside YAML sequences (the arr_tags lists), not just mapping values.
func TestLoadExpandsEnvInSequenceValues(t *testing.T) {
	t.Setenv("SEADEX_SCOUT_TAG", "anime")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "sonarr:\n  enabled: true\n  url: http://sonarr:8989\n  api_key: k\n" +
		"arr_tags:\n  include:\n    - ${SEADEX_SCOUT_TAG}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.IncludeTags) != 1 || c.IncludeTags[0] != "anime" {
		t.Errorf("IncludeTags = %v, want the expanded [anime]", c.IncludeTags)
	}
}

// TestLoadParseErrorOmitsSecretAlias pins the fail-closed posture of Load's
// FIRST yaml.Unmarshal error (h-f18): a literal secret pasted unquoted where a
// string was expected can be read as a YAML alias, and yaml.v3's parse error
// ("unknown anchor 'X' referenced") embeds it verbatim. main logs Load's error
// at startup, so neither the returned error nor the captured log corpus may
// carry any fragment of the secret; the parse error must route through
// yamlenv.SanitizeDecodeError like the decode errors.
func TestLoadParseErrorOmitsSecretAlias(t *testing.T) {
	const sentinel = "LEAK-SENTINEL-a1b2"
	rec := capture.Default(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "sonarr:\n  enabled: true\n  url: http://sonarr:8989\n  api_key: *" + sentinel + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() = nil error, want unknown-anchor parse error")
	}
	corpus := err.Error() + "\n" + strings.Join(rec.Messages(), "\n")
	for _, frag := range []string{sentinel, "LEAK", "SENTINEL", "a1b2"} {
		if strings.Contains(corpus, frag) {
			t.Errorf("parse-error corpus leaks secret fragment %q: %q", frag, corpus)
		}
	}
}

// TestSanitizeYAMLErrorFallbacks and TestIsLinePrefix moved with the
// sanitizer to github.com/cplieger/envx/yamlenv (SanitizeDecodeError's
// fallback, collision-guard, and line-prefix tables live there); the
// Load-level tests above and below pin the app-visible posture end to end,
// including the WithUnknownKeyEcho policy (TestLoadRejectsUnknownKeys).

// TestLoadDuplicateKeyErrorKeepsLineNumbers pins the duplicate-mapping-key
// TypeError entry shape through the decode-error redaction: the most common
// hand-edit mistake a YAML config invites (a copy-pasted second block) must be
// reported as a duplicate key with both line numbers kept (they are
// value-independent), while the key excerpt - which a misindented paste can
// fill with a secret - is redacted.
func TestLoadDuplicateKeyErrorKeepsLineNumbers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "sonarr:\n  enabled: true\nsonarr:\n  enabled: true\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() = nil error, want duplicate-key error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "line 3: mapping key <redacted> already defined at line 1") {
		t.Errorf("Load() error = %q, want redacted duplicate-key entry keeping both line numbers", msg)
	}
	if strings.Contains(msg, "sonarr") {
		t.Errorf("Load() error = %q, leaks the duplicated key text", msg)
	}
}

// TestLoadLeavesMappingKeysLiteral pins the documented yamlenv.Expand contract
// that ${VAR} expansion touches only string VALUES: a mapping key carrying an
// allowlisted reference stays byte-for-byte literal, so an environment value
// can never rewrite the document structure the operator wrote. With strict
// unknown-key checking (h-f12) the literal key is now rejected by name - had
// it been expanded it would have materialized the real animebytes key and
// loaded silently with the toggle flipped.
func TestLoadLeavesMappingKeysLiteral(t *testing.T) {
	t.Setenv("SEADEX_SCOUT_KEY", "animebytes")
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "sonarr:\n  enabled: true\n  url: http://sonarr:8989\n  api_key: k\n" +
		"\"${SEADEX_SCOUT_KEY}\": true\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() = nil error: the mapping key ${SEADEX_SCOUT_KEY} was expanded into a real key; keys must stay literal")
	}
	if !strings.Contains(err.Error(), `unknown configuration key "${SEADEX_SCOUT_KEY}"`) {
		t.Errorf("Load() error = %q, want the literal ${SEADEX_SCOUT_KEY} rejected as an unknown key", err)
	}
}

// TestURLEmbedsCredential pins the sole trigger of the credential-leak config
// warning: userinfo (with or without a password), each credential-like query
// parameter, the case-insensitive fold, the raw-query scan that still flags a
// credential in a malformed semicolon-delimited pair that net/url.Query drops, and the silent
// parse-failure and clean-URL negatives.
func TestURLEmbedsCredential(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool
	}{
		{"empty", "", false},
		{"clean", "http://prowlarr:9696/22/api", false},
		{"benign query", "http://prowlarr:9696/22/api?t=caps", false},
		{"userinfo", "http://user:pw@prowlarr:9696/22/api", true},
		{"username-only userinfo", "http://token@prowlarr:9696/22/api", true},
		{"apikey", "http://prowlarr:9696/22/api?apikey=k", true},
		{"api_key", "http://prowlarr:9696/22/api?api_key=k", true},
		{"passkey", "http://prowlarr:9696/22/api?passkey=k", true},
		{"token", "http://prowlarr:9696/22/api?token=k", true},
		{"authkey", "http://prowlarr:9696/22/api?authkey=k", true},
		{"torrent_pass", "http://prowlarr:9696/22/api?torrent_pass=k", true},
		{"apitoken", "http://prowlarr:9696/22/api?apitoken=k", true},
		{"api_token", "http://prowlarr:9696/22/api?api_token=k", true},
		{"access_token", "http://prowlarr:9696/22/api?access_token=k", true},
		{"auth_token", "http://prowlarr:9696/22/api?auth_token=k", true},
		{"password", "http://prowlarr:9696/22/api?password=k", true},
		{"pass", "http://prowlarr:9696/22/api?pass=k", true},
		{"secret", "http://prowlarr:9696/22/api?secret=k", true},
		{"client_secret", "http://prowlarr:9696/22/api?client_secret=k", true},
		{"rss_key", "http://prowlarr:9696/22/api?rss_key=k", true},
		{"uppercase APIKEY", "http://prowlarr:9696/22/api?APIKEY=k", true},
		{"malformed semicolon pair keeps apikey flagged", "http://prowlarr:9696/22/api?apikey=k;foo=x", true},
		{"credential after semicolon in malformed pair", "http://prowlarr:9696/22/api?foo=x;passkey=k", true},
		{"uppercase credential in malformed pair", "http://prowlarr:9696/22/api?APIKEY=k;foo=x", true},
		{"percent-encoded credential in malformed pair", "http://prowlarr:9696/22/api?%61pikey=k;foo=x", true},
		{"malformed pair without credential", "http://prowlarr:9696/22/api?foo=x;bar=y", false},
		{"credential name in value position", "http://prowlarr:9696/22/api?mode=apikey", false},
		{"unparseable", "http://[::1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := urlEmbedsCredential(tt.url); got != tt.want {
				t.Errorf("urlEmbedsCredential(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}

// TestValidateWarnsOnCredentialBearingTorznabURL pins validateIndexer's
// credential-embedding torznab-URL diagnostic: a credential-like query
// parameter or userinfo in either torznab URL fires the warning naming ONLY
// the field (never the credential-bearing URL, which the warning exists to
// keep out of logs), and clean URLs stay silent.
func TestValidateWarnsOnCredentialBearingTorznabURL(t *testing.T) {
	base := func() Config {
		return Config{
			RunMode: RunModeDaemon, SonarrURL: "http://sonarr:8989", SonarrAPIKey: "k",
			IndexerAPIKey:         strings.Repeat("a", 16),
			IndexerProwlarrAPIKey: "pk",
			IndexerNyaaTorznabURL: "http://prowlarr:9696/22/api",
			IndexerABTorznabURL:   "http://prowlarr:9696/2/api",
		}
	}
	const warnMsg = "torznab url embeds a credential-like query parameter or userinfo"

	t.Run("apikey query param warns naming the nyaa field", func(t *testing.T) {
		const cred = "leaked-cred-sentinel"
		rec := capture.Default(t)
		c := base()
		c.IndexerNyaaTorznabURL = "http://prowlarr:9696/22/api?apikey=" + cred
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if !rec.Contains(warnMsg) || !rec.AttrContains("", "field", "indexer.nyaa_torznab_url") {
			t.Errorf("Validate() log = %v, want the credential warning naming indexer.nyaa_torznab_url", rec.Messages())
		}
		corpus := strings.Join(rec.Messages(), "\n")
		if strings.Contains(corpus, cred) || rec.AttrContains("", "", cred) {
			t.Errorf("Validate() log leaks the credential value: %v", rec.Messages())
		}
	})
	t.Run("userinfo credential warns naming the ab field", func(t *testing.T) {
		const cred = "userinfo-pw-sentinel"
		rec := capture.Default(t)
		c := base()
		c.IndexerABTorznabURL = "http://user:" + cred + "@prowlarr:9696/2/api"
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if !rec.Contains(warnMsg) || !rec.AttrContains("", "field", "indexer.ab_torznab_url") {
			t.Errorf("Validate() log = %v, want the credential warning naming indexer.ab_torznab_url", rec.Messages())
		}
		corpus := strings.Join(rec.Messages(), "\n")
		if strings.Contains(corpus, cred) || rec.AttrContains("", "", cred) {
			t.Errorf("Validate() log leaks the userinfo credential value: %v", rec.Messages())
		}
	})
	t.Run("clean torznab urls stay silent", func(t *testing.T) {
		rec := capture.Default(t)
		c := base()
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if rec.Contains(warnMsg) {
			t.Errorf("Validate() log = %v, want no credential warning for clean urls", rec.Messages())
		}
	})
}

// TestValidateWarnsOnCredentialBearingArrURL pins Validate's credential
// posture for arr URLs: a query-bearing url (where pasted credentials land)
// is rejected outright naming ONLY the field — arrapi's base-URL contract
// forbids a query, and its own rejection would echo the full URL — while a
// userinfo credential (which still loads) fires the field-name-only warning,
// and clean URLs stay silent. Neither path may leak the credential value.
func TestValidateWarnsOnCredentialBearingArrURL(t *testing.T) {
	const warnMsg = "arr url embeds a credential-like query parameter or userinfo"

	t.Run("apikey query param is rejected naming only the sonarr field", func(t *testing.T) {
		const cred = "leaked-arr-cred-sentinel"
		rec := capture.Default(t)
		c := Config{RunMode: RunModeDaemon, SonarrURL: "http://sonarr:8989?apikey=" + cred, SonarrAPIKey: "k"}
		err := c.Validate()
		if err == nil {
			t.Fatal("Validate() = nil, want the no-query rejection (arrapi's base-URL contract forbids a query)")
		}
		if !strings.Contains(err.Error(), "sonarr.url must not contain a query") {
			t.Errorf("Validate() error = %q, want the field-name-only no-query rejection", err)
		}
		if strings.Contains(err.Error(), cred) {
			t.Errorf("Validate() error leaks the credential value: %v", err)
		}
		corpus := strings.Join(rec.Messages(), "\n")
		if strings.Contains(corpus, cred) || rec.AttrContains("", "", cred) {
			t.Errorf("Validate() log leaks the credential value: %v", rec.Messages())
		}
	})
	t.Run("userinfo credential warns naming the radarr field", func(t *testing.T) {
		const cred = "arr-userinfo-pw-sentinel"
		rec := capture.Default(t)
		c := Config{RunMode: RunModeDaemon, RadarrURL: "http://user:" + cred + "@radarr:7878", RadarrAPIKey: "k"}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if !rec.Contains(warnMsg) || !rec.AttrContains("", "field", "radarr.url") {
			t.Errorf("Validate() log = %v, want the credential warning naming radarr.url", rec.Messages())
		}
		corpus := strings.Join(rec.Messages(), "\n")
		if strings.Contains(corpus, cred) || rec.AttrContains("", "", cred) {
			t.Errorf("Validate() log leaks the userinfo credential value: %v", rec.Messages())
		}
	})
	t.Run("clean arr urls stay silent", func(t *testing.T) {
		rec := capture.Default(t)
		c := Config{RunMode: RunModeDaemon, SonarrURL: "http://sonarr:8989", SonarrAPIKey: "k"}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if rec.Contains(warnMsg) {
			t.Errorf("Validate() log = %v, want no credential warning for clean urls", rec.Messages())
		}
	})
}

// TestToConfigInfoOnDisabledSonarrWithKey mirrors the radarr variant above for
// the sonarr half-configuration signal: a disabled sonarr with an api_key set
// logs the Info line and its URL/key are dropped from the runtime Config.
func TestToConfigInfoOnDisabledSonarrWithKey(t *testing.T) {
	rec := capture.Default(t)
	fc := defaultFileConfig()
	fc.Sonarr = arrFile{Enabled: false, URL: "http://sonarr:8989", APIKey: "sk"}
	fc.Radarr = arrFile{Enabled: true, URL: "http://radarr:7878", APIKey: "rk"}
	c := fc.toConfig()
	if c.SonarrURL != "" || c.SonarrAPIKey != "" {
		t.Errorf("disabled sonarr should be dropped, got url=%q key=%q", c.SonarrURL, c.SonarrAPIKey)
	}
	if !rec.Contains("api_key is set but the arr is not enabled") ||
		!rec.AttrContains("api_key is set but the arr is not enabled", "field", "sonarr.api_key") {
		t.Errorf("toConfig log = %v, want the disabled-sonarr-with-key info", rec.Messages())
	}
}

// TestLoadEmptyOrCommentOnlyConfig pins Load's contract for a config file
// that exists but carries no YAML document (an empty file, or comments only):
// the load succeeds on the pure defaults baseline (RunMode daemon, default
// poll interval, default report dir) and the failure surfaces at Validate
// with the no-arr error, so a `touch`ed-but-never-filled config fails loudly
// with an actionable message instead of a parse error or a silent half-boot.
// This is the one Load path where the yaml document node is the zero Node
// (Decoder.Decode returns io.EOF), exercising yamlenv.CheckSingleDocument's
// first-decode-error branch.
func TestLoadEmptyOrCommentOnlyConfig(t *testing.T) {
	tests := map[string]string{
		"empty file":        "",
		"comment-only file": "# fill me in\n\n# see config.example.yaml\n",
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			c, err := Load(path)
			if err != nil {
				t.Fatalf("Load() = %v, want nil (a document-less file loads the defaults)", err)
			}
			if c.RunMode != RunModeDaemon {
				t.Errorf("RunMode = %q, want default %q", c.RunMode, RunModeDaemon)
			}
			if c.PollInterval != DefaultPollInterval || c.PollExternal {
				t.Errorf("PollInterval = %v external=%v, want built-in default %v", c.PollInterval, c.PollExternal, DefaultPollInterval)
			}
			if c.ReportDir != DefaultReportDir {
				t.Errorf("ReportDir = %q, want default %q", c.ReportDir, DefaultReportDir)
			}
			verr := c.Validate()
			if verr == nil {
				t.Fatal("Validate() = nil, want the no-arr rejection")
			}
			if !strings.Contains(verr.Error(), "no arr configured") {
				t.Errorf("Validate() error = %q, want the no-arr-configured message", verr)
			}
		})
	}
}

// TestLoadLeavesNonAllowlistedEnvLiteral pins the negative half of the
// allowlist wiring at Load level: a set but non-allowlisted environment
// variable (${HOME}) referenced in the config is never expanded - the literal
// survives into the runtime Config - so an arbitrary host env value can never
// be injected into a config field through a ${VAR} reference.
func TestLoadLeavesNonAllowlistedEnvLiteral(t *testing.T) {
	t.Setenv("HOME", "/home/leaked-value")
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "sonarr:\n  enabled: true\n  url: http://sonarr:8989\n  api_key: ${HOME}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.SonarrAPIKey != "${HOME}" {
		t.Errorf("SonarrAPIKey = %q, want the literal ${HOME} (non-allowlisted vars must never expand)", c.SonarrAPIKey)
	}
}

// TestLoadDefaultsArrURLWhenAbsent pins the defaults-baseline overlay
// contract ("absent keys keep these values, so a partial config still runs"):
// an enabled arr whose url key is absent inherits the baseline URL and the
// resulting config validates, so a minimal enabled+api_key config is runnable.
func TestLoadDefaultsArrURLWhenAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "sonarr:\n  enabled: true\n  api_key: k\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.SonarrURL != "http://sonarr:8989" {
		t.Errorf("SonarrURL = %q, want the defaults-baseline http://sonarr:8989 for an absent url key", c.SonarrURL)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil (default url + key is a runnable pair)", err)
	}
}

// TestValidateWarnsOnCredentialBearingPublicURL pins warnPublicURLProblems'
// credential-embedding diagnostic: userinfo or a credential-like query
// parameter in sonarr/radarr public_url fires the warning naming ONLY the
// field (deep-links are credential-redacted, so the value never rides the
// log), and clean public URLs stay silent.
func TestValidateWarnsOnCredentialBearingPublicURL(t *testing.T) {
	const warnMsg = "public_url embeds userinfo or a credential-like query parameter"

	t.Run("apikey query param warns naming the sonarr field", func(t *testing.T) {
		const cred = "public-url-cred-sentinel"
		rec := capture.Default(t)
		c := Config{
			RunMode: RunModeDaemon, SonarrURL: "http://sonarr:8989", SonarrAPIKey: "k",
			SonarrPublicURL: "https://sonarr.example.com?apikey=" + cred,
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if !rec.Contains(warnMsg) || !rec.AttrContains("", "field", "sonarr.public_url") {
			t.Errorf("Validate() log = %v, want the credential warning naming sonarr.public_url", rec.Messages())
		}
		corpus := strings.Join(rec.Messages(), "\n")
		if strings.Contains(corpus, cred) || rec.AttrContains("", "", cred) {
			t.Errorf("Validate() log leaks the credential value: %v", rec.Messages())
		}
	})
	t.Run("userinfo credential warns naming the radarr field", func(t *testing.T) {
		const cred = "public-url-pw-sentinel"
		rec := capture.Default(t)
		c := Config{
			RunMode: RunModeDaemon, RadarrURL: "http://radarr:7878", RadarrAPIKey: "k",
			RadarrPublicURL: "https://user:" + cred + "@radarr.example.com",
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if !rec.Contains(warnMsg) || !rec.AttrContains("", "field", "radarr.public_url") {
			t.Errorf("Validate() log = %v, want the credential warning naming radarr.public_url", rec.Messages())
		}
		corpus := strings.Join(rec.Messages(), "\n")
		if strings.Contains(corpus, cred) || rec.AttrContains("", "", cred) {
			t.Errorf("Validate() log leaks the userinfo credential value: %v", rec.Messages())
		}
	})
	t.Run("clean public urls stay silent", func(t *testing.T) {
		rec := capture.Default(t)
		c := Config{
			RunMode: RunModeDaemon, SonarrURL: "http://sonarr:8989", SonarrAPIKey: "k",
			SonarrPublicURL: "https://sonarr.example.com",
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if rec.Contains(warnMsg) {
			t.Errorf("Validate() log = %v, want no credential warning for a clean public_url", rec.Messages())
		}
	})
}

// TestValidateWarnsOnPublicURLQuery pins warnPublicURLProblems' distinct
// query-breaks-deep-links diagnostic: a query on sonarr/radarr public_url
// (including a bare trailing "?") fires the warning naming the field while
// Validate stays warn-only - the existing tests exercise this branch only
// incidentally beside the credential warning, so negating its condition
// would otherwise go unnoticed.
func TestValidateWarnsOnPublicURLQuery(t *testing.T) {
	tests := map[string]struct {
		cfg   Config
		field string
	}{
		"sonarr non-empty query": {
			cfg: Config{
				RunMode: RunModeDaemon, SonarrURL: "http://sonarr:8989", SonarrAPIKey: "k",
				SonarrPublicURL: "https://sonarr.example.com?theme=dark",
			},
			field: "sonarr.public_url",
		},
		"radarr bare trailing query": {
			cfg: Config{
				RunMode: RunModeDaemon, RadarrURL: "http://radarr:7878", RadarrAPIKey: "k",
				RadarrPublicURL: "https://radarr.example.com?",
			},
			field: "radarr.public_url",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			rec := capture.Default(t)
			if err := tt.cfg.Validate(); err != nil {
				t.Fatalf("Validate() error = %v, want query-bearing public_url to remain warn-only", err)
			}
			if !rec.Contains("public_url contains a query; report deep-links append the route after it") {
				t.Errorf("Validate() log = %v, want the query-bearing public_url warning", rec.Messages())
			}
			if !rec.AttrContains("", "field", tt.field) {
				t.Errorf("Validate() log = %v, want the warning to name %s", rec.Messages(), tt.field)
			}
		})
	}
}

// TestValidateIndexerFeedKeyLengthBoundary pins the exact floor of the
// short-feed-key warning: a key of exactly 16 characters meets the minimum
// the warning names ("shorter than 16 characters") and must stay silent,
// while a 15-character key still warns.
func TestValidateIndexerFeedKeyLengthBoundary(t *testing.T) {
	base := Config{
		RunMode: RunModeDaemon, SonarrURL: "http://s", SonarrAPIKey: "k",
		IndexerNyaaTorznabURL: "http://prowlarr:9696/22/api", IndexerProwlarrAPIKey: "pk",
	}
	tests := []struct {
		name     string
		keyLen   int
		wantWarn bool
	}{
		{"15-char key warns", 15, true},
		{"16-char key stays silent", 16, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := capture.Default(t)
			c := base
			c.IndexerAPIKey = strings.Repeat("a", tt.keyLen)
			if err := c.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if got := rec.Contains("feed_api_key is shorter than 16 characters"); got != tt.wantWarn {
				t.Errorf("short-key warning present = %v, want %v for a %d-character key", got, tt.wantWarn, tt.keyLen)
			}
		})
	}
}

// TestValidateIndexerEmptyABPasskeyWarning pins the empty-ab_passkey startup
// warning (indexer.ab_torznab_url set + indexer.ab_passkey empty): the /ab
// RSS feed builds its download links from the passkey, so the operator gets a
// config-time signal instead of discovering it in downstream arr RSS
// failures. Silent when the passkey is configured.
func TestValidateIndexerEmptyABPasskeyWarning(t *testing.T) {
	base := Config{
		RunMode: RunModeDaemon, SonarrURL: "http://s", SonarrAPIKey: "k",
		IndexerAPIKey:         strings.Repeat("a", 32),
		IndexerProwlarrAPIKey: "pk",
		IndexerABTorznabURL:   "http://prowlarr:9696/2/api",
	}
	t.Run("ab url with empty passkey warns", func(t *testing.T) {
		rec := capture.Default(t)
		c := base
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if !rec.Contains("indexer.ab_passkey is empty") {
			t.Errorf("Validate() log = %v, want the empty-ab_passkey warning", rec.Messages())
		}
	})
	t.Run("ab url with passkey stays silent", func(t *testing.T) {
		rec := capture.Default(t)
		c := base
		c.IndexerABPasskey = testABPasskey
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if rec.Contains("indexer.ab_passkey is empty") {
			t.Errorf("Validate() log = %v, want no empty-ab_passkey warning", rec.Messages())
		}
	})
}

// TestToConfigWarnsOnAllBlankTagLists pins the all-blank tag-list diagnostic
// on BOTH arr_tags sides: trimList drops every blank entry, so the filter is
// silently off, and the field-name-only warning (never the tag values, which
// can carry an expanded ${VAR}) is the operator's only signal that the
// configured list does nothing.
func TestToConfigWarnsOnAllBlankTagLists(t *testing.T) {
	t.Run("include list", func(t *testing.T) {
		rec := capture.Default(t)
		fc := defaultFileConfig()
		fc.ArrTags.Include = []string{" ", "\t"}

		cfg := fc.toConfig()

		if len(cfg.IncludeTags) != 0 {
			t.Errorf("IncludeTags = %v, want no effective tags", cfg.IncludeTags)
		}
		if !rec.Contains("configured tag list holds only blank entries; the filter is off") ||
			!rec.AttrContains("", "field", "arr_tags.include") {
			t.Errorf("toConfig() log = %v, want all-blank include-list warning", rec.Messages())
		}
	})

	t.Run("exclude list", func(t *testing.T) {
		rec := capture.Default(t)
		fc := defaultFileConfig()
		fc.ArrTags.Exclude = []string{" ", "\n"}

		cfg := fc.toConfig()

		if len(cfg.ExcludeTags) != 0 {
			t.Errorf("ExcludeTags = %v, want no effective tags", cfg.ExcludeTags)
		}
		if !rec.Contains("configured tag list holds only blank entries; the filter is off") ||
			!rec.AttrContains("", "field", "arr_tags.exclude") {
			t.Errorf("toConfig() log = %v, want all-blank exclude-list warning", rec.Messages())
		}
	})
}

// TestValidateWarnsOnRelativeReportDir pins the relative-report.dir
// diagnostic: atomicfile's path gate rejects a non-absolute path, so a
// relative report.dir validates cleanly and then loses both halves of the
// report pair at the end of a report run. Warn-only at config time (a daemon
// that never reports is unaffected), and the warning is field-name-only -
// report.dir is secret-capable via ${VAR} expansion, so the value is never
// echoed.
func TestValidateWarnsOnRelativeReportDir(t *testing.T) {
	t.Run("relative report dir warns", func(t *testing.T) {
		rec := capture.Default(t)
		cfg := Config{
			RunMode: RunModeDaemon, ReportDir: "./s3cret-dir",
			SonarrURL: "http://sonarr:8989", SonarrAPIKey: "k",
		}

		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate() error = %v, want a relative report.dir to remain warn-only", err)
		}
		if !rec.Contains("report.dir is not an absolute path") ||
			!rec.AttrContains("", "field", "report.dir") {
			t.Errorf("Validate() log = %v, want the relative-report.dir warning", rec.Messages())
		}
		for _, m := range rec.Messages() {
			if strings.Contains(m, "s3cret-dir") {
				t.Errorf("Validate() log echoes the configured value: %q", m)
			}
		}
		if rec.AttrContains("", "", "s3cret-dir") {
			t.Errorf("Validate() structured attributes echo the configured value: %v", rec.Messages())
		}
	})

	t.Run("absolute report dir stays silent", func(t *testing.T) {
		rec := capture.Default(t)
		cfg := Config{
			RunMode: RunModeDaemon, ReportDir: DefaultReportDir,
			SonarrURL: "http://sonarr:8989", SonarrAPIKey: "k",
		}

		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if rec.Contains("report.dir is not an absolute path") {
			t.Errorf("Validate() log = %v, want no relative-report.dir warning", rec.Messages())
		}
	})
}

// TestLoadWarnsOnWorldReadableConfig pins the permission diagnostic Load emits
// through warnConfigPermissions: the file holds every secret the app has (the
// arr api keys, the Prowlarr key, the AnimeBytes passkey), so a mode readable
// beyond the owner WARNs and names the octal mode, while an owner-only 0600
// file stays silent. The mode is the one value this diagnostic echoes, so the
// warning must never carry file content.
func TestLoadWarnsOnWorldReadableConfig(t *testing.T) {
	const content = "sonarr:\n  enabled: true\n  url: http://sonarr:8989\n  api_key: sk-sentinel\n"
	tests := []struct {
		name     string
		mode     os.FileMode
		wantMode string
		wantWarn bool
	}{
		{"group and world readable warns", 0o644, "644", true},
		{"group readable warns", 0o640, "640", true},
		{"owner-only stays silent", 0o600, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := capture.Default(t)
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(content), tt.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tt.mode); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err != nil {
				t.Fatalf("Load: %v", err)
			}
			const msg = "config file is readable beyond its owner"
			if got := rec.CountLevel(slog.LevelWarn, msg) > 0; got != tt.wantWarn {
				t.Errorf("permission warning present = %v, want %v (messages %v)", got, tt.wantWarn, rec.Messages())
			}
			if !tt.wantWarn {
				return
			}
			if !rec.HasAttr(msg, "mode", tt.wantMode) {
				t.Errorf("permission warning mode attr = %v, want %q", rec.Messages(), tt.wantMode)
			}
			if rec.AttrContains(msg, "", "sk-sentinel") {
				t.Errorf("permission warning echoes config content: %v", rec.Messages())
			}
		})
	}
}

// TestValidateWarnsOnNonTorznabABEndpoint covers the AnimeBytes half of the
// endpoint-shape diagnostic: warnNonPerIndexerEndpoints enumerates both
// per-indexer URL fields, and only the nyaa entry is exercised elsewhere, so
// dropping the ab entry would silently cost the operator the config-time
// signal for a pasted AB base while every test stayed green.
func TestValidateWarnsOnNonTorznabABEndpoint(t *testing.T) {
	base := Config{
		RunMode: RunModeDaemon, SonarrURL: "http://sonarr:8989", SonarrAPIKey: "k",
		IndexerAPIKey:         strings.Repeat("a", 32),
		IndexerProwlarrAPIKey: "pk",
		IndexerABPasskey:      testABPasskey,
		IndexerNyaaTorznabURL: "http://prowlarr:9696/22/api",
	}
	tests := []struct {
		name     string
		endpoint string
		wantWarn bool
	}{
		{"bare Prowlarr origin warns", "http://prowlarr:9696", true},
		{"Prowlarr REST API path warns", "http://prowlarr:9696/api/v1/search", true},
		{"per-indexer Torznab path stays silent", "http://prowlarr:9696/2/api", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := capture.Default(t)
			c := base
			c.IndexerABTorznabURL = tt.endpoint
			if err := c.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			const msg = "torznab url is not a Prowlarr per-indexer Torznab endpoint"
			got := rec.AttrContains(msg, "field", "indexer.ab_torznab_url")
			if got != tt.wantWarn {
				t.Errorf("ab endpoint-shape warning present = %v, want %v (messages %v)", got, tt.wantWarn, rec.Messages())
			}
		})
	}
}

// TestValidateWarnsOnUnexpandedSecretRef pins warnUnexpandedSecretRefs: a secret
// still holding a literal environment-variable reference warns in both spellings
// yamlenv leaves alone (a non-allowlisted braced name and the brace-less shell
// form), a plain secret stays silent, and the warning names the field while
// never echoing the value.
func TestValidateWarnsOnUnexpandedSecretRef(t *testing.T) {
	const msg = "still holds a literal environment-variable reference"
	tests := []struct {
		name     string
		apiKey   string
		wantWarn bool
	}{
		{"non-allowlisted braced ref warns", "${AB_PASSKEY}", true},
		{"brace-less shell ref warns", "$SEADEX_SCOUT_SONARR_KEY", true},
		// A plain key is built rather than written as a literal: a 16-hex
		// literal beside the word "key" reads as a real credential to the
		// gitleaks generic-api-key rule the CI secret scan runs, and the test
		// only needs a value carrying no environment-variable reference.
		{"plain key stays silent", strings.Repeat("a", 16), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := capture.Default(t)
			c := Config{RunMode: RunModeDaemon, SonarrURL: "http://sonarr:8989", SonarrAPIKey: tt.apiKey}
			if err := c.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if got := rec.AttrContains(msg, "field", "sonarr.api_key"); got != tt.wantWarn {
				t.Errorf("env-ref warning present = %v, want %v (messages %v)", got, tt.wantWarn, rec.Messages())
			}
			if rec.AttrContains(msg, "", tt.apiKey) {
				t.Errorf("env-ref warning echoes the configured value: %v", rec.Messages())
			}
		})
	}
}

// TestValidateWarnsOnReusedIndexerSecret pins warnReusedIndexerSecrets: reusing
// the Prowlarr key or the AnimeBytes passkey as indexer.feed_api_key warns and
// names the reused field (feed_api_key travels as a query parameter and is
// stored in each arr's indexer config), while distinct secrets stay silent. The
// warning never echoes a secret.
func TestValidateWarnsOnReusedIndexerSecret(t *testing.T) {
	const msg = "feed_api_key repeats another indexer secret"
	shared := strings.Repeat("b", 32)
	base := Config{
		RunMode: RunModeDaemon, SonarrURL: "http://sonarr:8989", SonarrAPIKey: "k",
		IndexerNyaaTorznabURL: "http://prowlarr:9696/22/api",
		IndexerAPIKey:         shared,
		IndexerProwlarrAPIKey: "pk",
	}
	tests := []struct {
		name      string
		mutate    func(*Config)
		wantField string
	}{
		{"prowlarr key reused warns", func(c *Config) { c.IndexerProwlarrAPIKey = shared }, "indexer.prowlarr_api_key"},
		{"ab passkey reused warns", func(c *Config) { c.IndexerABPasskey = shared }, "indexer.ab_passkey"},
		{"distinct secrets stay silent", func(*Config) {}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := capture.Default(t)
			c := base
			tt.mutate(&c)
			if err := c.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if tt.wantField == "" {
				if rec.Contains(msg) {
					t.Errorf("Validate() log = %v, want no reused-secret warning", rec.Messages())
				}
				return
			}
			if !rec.AttrContains(msg, "field", tt.wantField) {
				t.Errorf("Validate() log = %v, want the reused-secret warning naming %s", rec.Messages(), tt.wantField)
			}
			if rec.AttrContains(msg, "", shared) {
				t.Errorf("reused-secret warning echoes the secret: %v", rec.Messages())
			}
		})
	}
}

// TestToConfigTagFilterDefaultFiltersNothing pins the default the operator gets
// with no filters.exclude_tags at all, and with an explicit empty map: the two
// must be indistinguishable, and BOTH must filter nothing on every surface. A
// release SeaDex tagged Broken therefore reaches the findings, the report and
// the feed - the deliberate default, not an oversight.
func TestToConfigTagFilterDefaultFiltersNothing(t *testing.T) {
	tests := map[string]map[string][]string{
		"absent section": nil,
		"empty map":      {},
	}
	warned := []string{"Broken", "Incomplete"}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			fc := defaultFileConfig()
			fc.Filters.ExcludeTags = raw

			c := fc.toConfig()

			if c.tagFilterErr != nil {
				t.Fatalf("tagFilterErr = %v, want nil", c.tagFilterErr)
			}
			for _, s := range []tagfilter.Surface{
				tagfilter.SurfaceFindings, tagfilter.SurfaceReport, tagfilter.SurfaceFeed,
			} {
				if c.TagFilter.Excludes(warned, s) {
					t.Errorf("a warned release is excluded from %s by default", s)
				}
			}
		})
	}
}

// TestToConfigTagFilterPopulated pins the wiring of a real filters.exclude_tags
// map onto the runtime policy, including per-surface selectivity and the
// case-insensitive tag key the file may spell any way.
func TestToConfigTagFilterPopulated(t *testing.T) {
	fc := defaultFileConfig()
	fc.Filters.ExcludeTags = map[string][]string{
		"BROKEN":     {"findings", "report", "feed"},
		"incomplete": {" Feed "},
	}

	c := fc.toConfig()

	if c.tagFilterErr != nil {
		t.Fatalf("tagFilterErr = %v, want nil", c.tagFilterErr)
	}
	tests := []struct {
		tag     string
		surface tagfilter.Surface
		want    bool
	}{
		{"broken", tagfilter.SurfaceFindings, true},
		{"broken", tagfilter.SurfaceReport, true},
		{"broken", tagfilter.SurfaceFeed, true},
		{"incomplete", tagfilter.SurfaceFeed, true},
		{"incomplete", tagfilter.SurfaceFindings, false},
		{"incomplete", tagfilter.SurfaceReport, false},
		{"dual-audio", tagfilter.SurfaceFeed, false},
	}
	for _, tt := range tests {
		if got := c.TagFilter.Excludes([]string{tt.tag}, tt.surface); got != tt.want {
			t.Errorf("Excludes(%q, %s) = %v, want %v", tt.tag, tt.surface, got, tt.want)
		}
	}
}

// TestToConfigTagFilterRejections pins every filters.exclude_tags input that is
// a startup error rather than a silent no-op, and that each error keeps the
// config package's field-name-only posture: it names the field and the valid
// surface set, never the operator's tag key or the rejected surface value
// (either can hold a ${VAR}-expanded secret placed there by a typo).
func TestToConfigTagFilterRejections(t *testing.T) {
	many := make(map[string][]string, maxExcludeTags+1)
	for i := range maxExcludeTags + 1 {
		many["tag"+strconv.Itoa(i)] = []string{"feed"}
	}
	tests := []struct {
		name     string
		raw      map[string][]string
		wantErr  string
		wantAway string
	}{
		{
			name:     "unknown surface",
			raw:      map[string][]string{"broken": {"findings", "alerts-s3cret"}},
			wantErr:  "unknown surface",
			wantAway: "alerts-s3cret",
		},
		{
			name:     "no surfaces",
			raw:      map[string][]string{"broken-s3cret": {}},
			wantErr:  "lists no surfaces",
			wantAway: "broken-s3cret",
		},
		{
			name:    "blank tag key",
			raw:     map[string][]string{"   ": {"feed"}},
			wantErr: "blank tag key",
		},
		{
			name:     "over-long tag key",
			raw:      map[string][]string{strings.Repeat("s3cret", 20): {"feed"}},
			wantErr:  "longer than",
			wantAway: "s3crets3cret",
		},
		{
			name:    "too many tags",
			raw:     many,
			wantErr: "more than",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := defaultFileConfig()
			fc.Filters.ExcludeTags = tt.raw

			c := fc.toConfig()

			if c.tagFilterErr == nil {
				t.Fatal("tagFilterErr = nil, want an error")
			}
			msg := c.tagFilterErr.Error()
			if !strings.Contains(msg, tt.wantErr) {
				t.Errorf("error %q does not mention %q", msg, tt.wantErr)
			}
			if !strings.Contains(msg, "filters.exclude_tags") {
				t.Errorf("error %q does not name the field", msg)
			}
			if tt.wantAway != "" && strings.Contains(msg, tt.wantAway) {
				t.Errorf("error %q echoes the operator-supplied value", msg)
			}
			// A rejected map must not leave a half-built policy behind.
			if c.TagFilter.Excludes([]string{"broken"}, tagfilter.SurfaceFeed) {
				t.Error("a rejected exclude_tags map still produced exclusions")
			}
		})
	}
}

// TestValidateSurfacesTagFilterError pins that a rejected filters.exclude_tags
// map stops the app at startup (Validate), naming the valid surface set so the
// operator can fix the file without reading the source.
func TestValidateSurfacesTagFilterError(t *testing.T) {
	fc := defaultFileConfig()
	fc.Sonarr = arrFile{Enabled: true, URL: "http://sonarr:8989", APIKey: "k"}
	fc.Filters.ExcludeTags = map[string][]string{"broken": {"alerts"}}

	c := fc.toConfig()

	err := c.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want the exclude_tags error")
	}
	for _, want := range []string{"findings", "report", "feed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name the valid surface %q", err, want)
		}
	}
	// The same config with a valid surface list must run.
	fc.Filters.ExcludeTags = map[string][]string{"broken": {"feed"}}
	ok := fc.toConfig()
	if err := ok.Validate(); err != nil {
		t.Errorf("Validate() with a valid exclude_tags = %v, want nil", err)
	}
}

// TestLoadTagFilterFromFile pins the whole path from YAML text to the runtime
// policy: the nested map decodes, an unknown key inside filters is still
// rejected by the strict loader, and the empty-map spelling in
// config.example.yaml loads to a policy that filters nothing.
func TestLoadTagFilterFromFile(t *testing.T) {
	dir := t.TempDir()
	populated := filepath.Join(dir, "populated.yaml")
	body := "sonarr:\n  enabled: true\n  url: \"http://sonarr:8989\"\n  api_key: \"k\"\n" +
		"filters:\n  exclude_tags:\n    Broken: [findings, feed]\n"
	if err := os.WriteFile(populated, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	c, err := Load(populated)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !c.TagFilter.Excludes([]string{"broken"}, tagfilter.SurfaceFindings) {
		t.Error("configured tag is not excluded from findings")
	}
	if c.TagFilter.Excludes([]string{"broken"}, tagfilter.SurfaceReport) {
		t.Error("an unlisted surface is filtered")
	}

	empty := filepath.Join(dir, "empty.yaml")
	emptyBody := "sonarr:\n  enabled: true\n  url: \"http://sonarr:8989\"\n  api_key: \"k\"\n" +
		"filters:\n  exclude_tags: {}\n"
	if err := os.WriteFile(empty, []byte(emptyBody), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	ec, err := Load(empty)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if err := ec.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if ec.TagFilter.Excludes([]string{"broken"}, tagfilter.SurfaceFeed) {
		t.Error("an empty exclude_tags map filtered a tag")
	}
}

// TestToConfigIgnoreSet pins filters.ignore's flattening onto the runtime
// emission-suppression set: the absent key and an explicit empty list are
// indistinguishable and both suppress nothing (one unambiguous spelling of
// "suppress nothing"), a real list becomes a set, and duplicates collapse
// because a set is what the notifier wants.
func TestToConfigIgnoreSet(t *testing.T) {
	tests := []struct {
		name string
		raw  []int
		want []int
	}{
		{name: "absent key", raw: nil},
		{name: "empty list", raw: []int{}},
		{name: "single id", raw: []int{154587}, want: []int{154587}},
		{name: "several ids", raw: []int{154587, 21519}, want: []int{154587, 21519}},
		{name: "duplicates collapse", raw: []int{7, 7, 7, 9}, want: []int{7, 9}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := defaultFileConfig()
			fc.Filters.Ignore = tt.raw

			c := fc.toConfig()

			if c.ignoreErr != nil {
				t.Fatalf("ignoreErr = %v, want nil", c.ignoreErr)
			}
			if len(tt.want) == 0 {
				if c.IgnoreFindings != nil {
					t.Errorf("IgnoreFindings = %v, want nil (absent and empty must be indistinguishable)", c.IgnoreFindings)
				}
				return
			}
			if len(c.IgnoreFindings) != len(tt.want) {
				t.Errorf("IgnoreFindings holds %d ids (%v), want %d", len(c.IgnoreFindings), c.IgnoreFindings, len(tt.want))
			}
			for _, id := range tt.want {
				if _, ok := c.IgnoreFindings[id]; !ok {
					t.Errorf("IgnoreFindings is missing %d (%v)", id, c.IgnoreFindings)
				}
			}
		})
	}
}

// TestToConfigIgnoreRejections pins every filters.ignore input that is a
// startup error rather than a silent no-op: a list past the bound, and any
// non-positive AniList ID (SeaDex IDs start at 1, so a 0 or negative entry is a
// typo that would suppress nothing while the operator believes it suppresses
// something). A rejected list must leave NO half-built set behind, otherwise a
// refused config would still suppress an emission.
func TestToConfigIgnoreRejections(t *testing.T) {
	many := make([]int, 0, maxIgnoreIDs+1)
	for i := range maxIgnoreIDs + 1 {
		many = append(many, i+1)
	}
	tests := []struct {
		name    string
		raw     []int
		wantErr string
	}{
		{name: "over the bound", raw: many, wantErr: "more than"},
		{name: "zero id", raw: []int{154587, 0}, wantErr: "non-positive"},
		{name: "negative id", raw: []int{-1}, wantErr: "non-positive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := defaultFileConfig()
			fc.Filters.Ignore = tt.raw

			c := fc.toConfig()

			if c.ignoreErr == nil {
				t.Fatal("ignoreErr = nil, want an error")
			}
			msg := c.ignoreErr.Error()
			if !strings.Contains(msg, tt.wantErr) {
				t.Errorf("error %q does not mention %q", msg, tt.wantErr)
			}
			if !strings.Contains(msg, "filters.ignore") {
				t.Errorf("error %q does not name the field", msg)
			}
			if c.IgnoreFindings != nil {
				t.Errorf("IgnoreFindings = %v after a rejected list, want nil", c.IgnoreFindings)
			}
		})
	}
}

// TestValidateSurfacesIgnoreError pins that a rejected filters.ignore list
// stops the app at startup rather than degrading into a silently inactive
// suppression set: the list is parsed once at flatten time, so Validate is the
// only place that error can surface.
func TestValidateSurfacesIgnoreError(t *testing.T) {
	fc := defaultFileConfig()
	fc.Sonarr = arrFile{Enabled: true, URL: "http://sonarr:8989", APIKey: "k"}
	fc.Filters.Ignore = []int{0}

	c := fc.toConfig()
	if err := c.Validate(); err == nil {
		t.Fatal("Validate() = nil, want the filters.ignore error")
	} else if !strings.Contains(err.Error(), "filters.ignore") {
		t.Errorf("Validate() error = %v, want it to name filters.ignore", err)
	}

	// The same config with a valid list must run.
	fc.Filters.Ignore = []int{154587}
	ok := fc.toConfig()
	if err := ok.Validate(); err != nil {
		t.Errorf("Validate() with a valid ignore list = %v, want nil", err)
	}
	if _, ignored := ok.IgnoreFindings[154587]; !ignored {
		t.Error("a validated config lost its ignore set")
	}
}

// TestLoadIgnoreFromFile pins the whole path from YAML text to the runtime set,
// including the `ignore: []` spelling config.example.yaml ships (which must
// load and suppress nothing) and the strict loader's rejection of a
// wrong-typed value.
func TestLoadIgnoreFromFile(t *testing.T) {
	const arrs = "sonarr:\n  enabled: true\n  url: \"http://sonarr:8989\"\n  api_key: \"k\"\n"
	dir := t.TempDir()
	write := func(t *testing.T, name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		return path
	}

	c, err := Load(write(t, "populated.yaml", arrs+"filters:\n  ignore: [154587, 21519]\n"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	for _, id := range []int{154587, 21519} {
		if _, ok := c.IgnoreFindings[id]; !ok {
			t.Errorf("configured ignore id %d missing from %v", id, c.IgnoreFindings)
		}
	}

	ec, err := Load(write(t, "empty.yaml", arrs+"filters:\n  ignore: []\n"))
	if err != nil {
		t.Fatalf("Load() of the shipped empty spelling error = %v", err)
	}
	if err := ec.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if ec.IgnoreFindings != nil {
		t.Errorf("IgnoreFindings = %v, want nil for an empty list", ec.IgnoreFindings)
	}

	if _, err := Load(write(t, "typed.yaml", arrs+"filters:\n  ignore: \"154587\"\n")); err == nil {
		t.Error("Load() of a string filters.ignore = nil error, want the strict decode rejection")
	}
}

// TestValidateRejectsUnusableABPasskey pins the config boundary's ONE format
// gate on indexer.ab_passkey (validateABPasskey). A configured passkey that is
// not the shape AnimeBytes issues cannot build a grabbable download link, so it
// is a HARD startup error rather than a warning: the alternative is a daemon
// that validates clean, starts, and then hands every arr a link that fails at
// the tracker. Empty stays the documented off state.
//
// The gate is POSITIVE - length plus no whitespace - which is why no case here
// is about a placeholder SPELLING. Every unexpanded reference is refused by the
// same rule that refuses a truncated paste, including the unterminated "${NAME"
// form no reference regex matches. Reference recognition survives only as a
// hint inside the message, so it never decides pass/fail.
//
// The lengths are upstream authority, not an invention: Jackett's AnimeBytes
// indexer rejects a passkey with "expected length: 32, 48, or 56" and
// Prowlarr's AnimeBytesSettingsValidator asserts the same three. Neither
// constrains the CHARSET, so a well-shaped non-hex value must pass - the app
// validates shape, never correctness.
func TestValidateRejectsUnusableABPasskey(t *testing.T) {
	base := Config{
		RunMode: RunModeDaemon, SonarrURL: "http://s", SonarrAPIKey: "k",
		IndexerNyaaTorznabURL: "http://prowlarr:9696/22/api",
		IndexerABTorznabURL:   "http://prowlarr:9696/2/api",
		IndexerAPIKey:         strings.Repeat("a", 32),
		IndexerProwlarrAPIKey: "pk",
	}
	for name, tc := range map[string]struct {
		passkey   string
		wantError bool
	}{
		"empty is the documented off state":               {"", false},
		"braced reference is refused":                     {"${SEADEX_SCOUT_AB_PASSKEY}", true},
		"brace-less reference is refused":                 {"$SEADEX_SCOUT_AB_PASSKEY", true},
		"unterminated braced paste is refused":            {"${SEADEX_SCOUT_AB_PASSKEY", true},
		"a reference inside a value is refused":           {"pre${SEADEX_SCOUT_AB_PASSKEY}post", true},
		"a short hand-typed value is refused":             {"0f1e2d3c4b5a6978", true},
		"a 32-character passkey passes":                   {"0f1e2d3c4b5a69788796a5b4c3d2e1f0", false},
		"a 48-character passkey passes":                   {strings.Repeat("b", 48), false},
		"a 56-character passkey passes":                   {strings.Repeat("c", 56), false},
		"31 characters is refused":                        {strings.Repeat("d", 31), true},
		"a non-hex charset is not the app's to constrain": {strings.Repeat("Zz+/=!", 5) + "aa", false},
		"an embedded space is refused":                    {strings.Repeat("e", 20) + " " + strings.Repeat("f", 11), true},
		"an embedded newline is refused":                  {strings.Repeat("g", 20) + "\n" + strings.Repeat("h", 11), true},
	} {
		t.Run(name, func(t *testing.T) {
			c := base
			c.IndexerABPasskey = tc.passkey
			err := c.Validate()
			if (err != nil) != tc.wantError {
				t.Fatalf("Validate() error = %v, want an error: %v", err, tc.wantError)
			}
			if err == nil {
				return
			}
			if !strings.Contains(err.Error(), "indexer.ab_passkey") {
				t.Errorf("Validate() error = %v, want it to name indexer.ab_passkey", err)
			}
			// Field-name-only: the passkey is a credential and must never ride
			// the error a startup failure prints.
			if strings.Contains(err.Error(), tc.passkey) {
				t.Errorf("Validate() error echoes the configured passkey: %v", err)
			}
		})
	}
}

// TestValidateABPasskeyGateIsShapeNotCorrectness documents the one thing the
// gate deliberately does NOT do: a value that HAPPENS to be a 32-character
// environment-variable reference passes, because correctness is the operator's
// and surfaces as an AnimeBytes auth failure rather than as a config error.
// Pinned so a future cycle does not "fix" it by re-adding the placeholder
// heuristic the one positive gate replaced.
func TestValidateABPasskeyGateIsShapeNotCorrectness(t *testing.T) {
	const wellShapedRef = "${SEADEX_SCOUT_AB_PASSKEY_NAMES}"
	if len(wellShapedRef) != 32 {
		t.Fatalf("fixture length = %d, want 32", len(wellShapedRef))
	}
	c := Config{
		RunMode: RunModeDaemon, SonarrURL: "http://s", SonarrAPIKey: "k",
		IndexerNyaaTorznabURL: "http://prowlarr:9696/22/api",
		IndexerAPIKey:         strings.Repeat("a", 32),
		IndexerProwlarrAPIKey: "pk",
		IndexerABPasskey:      wellShapedRef,
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want a well-shaped passkey to pass the SHAPE gate", err)
	}
}
