package config

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/slogx"
	"github.com/cplieger/slogx/capture"
	"go.yaml.in/yaml/v3"
)

// The string-level expansion mechanics (braced-only matching, keep-literal on
// unknown/unset, bare-dollar safety) are yamlenv's contract, tested in
// github.com/cplieger/envx/yamlenv. Here the app tests its own allowlist
// policy plus the Load-level wiring (expansion, the unresolved-refs warning,
// keys-stay-literal, and the secret-redaction posture).

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
		{"nyaa indexer url without feed key rejected", Config{RunMode: RunModeDaemon, SonarrURL: "http://s", SonarrAPIKey: "k", IndexerNyaaTorznabURL: "http://prowlarr/22/api"}, true},
		{"ab indexer url without feed key rejected", Config{RunMode: RunModeDaemon, SonarrURL: "http://s", SonarrAPIKey: "k", IndexerABTorznabURL: "http://prowlarr/2/api"}, true},
		{"indexer url with feed key ok", Config{RunMode: RunModeDaemon, SonarrURL: "http://s", SonarrAPIKey: "k", IndexerNyaaTorznabURL: "http://prowlarr/22/api", IndexerAPIKey: "feedkey"}, false},
		{"no indexer url unaffected", Config{RunMode: RunModeDaemon, SonarrURL: "http://s", SonarrAPIKey: "k"}, false},
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

	if c.SonarrURL != "http://sonarr:8989" || c.SonarrAPIKey != "key" {
		t.Errorf("sonarr not trimmed: url=%q key=%q", c.SonarrURL, c.SonarrAPIKey)
	}
	if c.RadarrURL != "" || c.RadarrAPIKey != "" {
		t.Errorf("disabled radarr should be empty, got url=%q key=%q", c.RadarrURL, c.RadarrAPIKey)
	}
	if len(c.IncludeTags) != 1 || c.IncludeTags[0] != "anime" {
		t.Errorf("include tags not trimmed/filtered: %v", c.IncludeTags)
	}
	if c.ReportDir != DefaultReportDir {
		t.Errorf("ReportDir = %q, want default %q", c.ReportDir, DefaultReportDir)
	}
}

// TestToConfigInfoOnDisabledArrWithKey pins the half-configuration signal: a
// disabled arr whose api_key is set (always operator-written) logs an Info at
// flatten time, while the defaults baseline (disabled, key-less) stays silent
// so a plain config boots without noise (l-f63).
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
		if !rec.Contains("radarr.api_key is set but radarr.enabled is false") {
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
		{"valid duration", "6h", 6 * time.Hour, false},
		{"empty is default", "", DefaultPollInterval, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dur, ext := parseInterval(tt.raw)
			if ext != tt.wantExt {
				t.Errorf("parseInterval(%q) external = %v, want %v", tt.raw, ext, tt.wantExt)
			}
			if !tt.wantExt && dur != tt.wantDur {
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

// TestLoadDecodeErrorOmitsExpandedSecret pins the field-name-only posture of
// Load's post-expansion decode error: yaml.v3 type-mismatch errors embed a
// backtick-quoted excerpt of the offending scalar value, which after ${VAR}
// expansion can be a prefix of a real secret (an api key placed in a non-string
// field by a config typo). The error must keep line/type info but never the
// expanded value (h-f3, the sibling gate to l-f4's validateHTTPURL fix).
func TestLoadDecodeErrorOmitsExpandedSecret(t *testing.T) {
	const secret = "super-secret-api-key-sentinel"
	t.Setenv("SONARR_API_KEY", secret)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "sonarr:\n  enabled: ${SONARR_API_KEY}\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() = nil error, want type-mismatch error")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "super-s") {
		t.Errorf("Load() error = %q, leaks expanded secret", err)
	}
	if !strings.Contains(err.Error(), "cannot unmarshal !!str <redacted> into bool") {
		t.Errorf("Load() error = %q, want the redacted wrong-type entry shape", err)
	}
}

// TestLoadDecodeErrorOmitsBacktickSecret pins the value-independent redaction:
// yaml.v3 embeds the scalar excerpt with any backtick in the value unchanged,
// so a secret containing a backtick defeats backtick-pair matching and would
// leak a prefix. No fragment of the expanded value may survive
// sanitizeYAMLError, in the returned error or the captured startup log (h-f14).
func TestLoadDecodeErrorOmitsBacktickSecret(t *testing.T) {
	const secret = "zq9`vw7-secret-sentinel"
	t.Setenv("SONARR_API_KEY", secret)
	rec := capture.Default(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "sonarr:\n  enabled: ${SONARR_API_KEY}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() = nil error, want type-mismatch error")
	}
	corpus := err.Error() + "\n" + strings.Join(rec.Messages(), "\n")
	for _, frag := range []string{secret, "zq9", "vw7", "secret-sentinel"} {
		if strings.Contains(corpus, frag) {
			t.Errorf("decode-error corpus leaks secret fragment %q: %q", frag, corpus)
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

// recordHasAttr reports whether any captured record carries an attribute with
// the given key whose string form contains sub (capture.Recorder.Contains
// matches messages only; warned-about values ride in attrs).
func recordHasAttr(rec *capture.Recorder, key, sub string) bool {
	for _, r := range rec.Records() {
		found := false
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == key && strings.Contains(a.Value.String(), sub) {
				found = true
				return false
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
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
	if !rec.Contains("config references environment variables") || !recordHasAttr(rec, "vars", "SONARR_MISSING") {
		t.Errorf("Load unresolved-env warning = %v, want message and variable name", rec.Messages())
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
	if recordHasAttr(rec, "value", "verbose") {
		t.Errorf("parseLogLevel warning echoes the rejected value: %v", rec.Messages())
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
			if tt.wantWarn && recordHasAttr(rec, "value", "txt") {
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

// TestValidateIndexerShortFeedKeyWarning pins the warn-only strength floor on
// indexer.feed_api_key (l-f64): a key under 16 characters warns (it gates the
// AnimeBytes-passkey-bearing feed), a strong key stays silent, and the key
// value never rides the log record (field-name-only posture).
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
		if strings.Contains(corpus, shortKey) || recordHasAttr(rec, "value", shortKey) {
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
}

func TestParseIntervalBoundsAndFallback(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantDur time.Duration
		wantExt bool
	}{
		{"below minimum clamps up to 1h", "30m", minPollInterval, false},
		{"above maximum clamps down", "9000h", maxPollInterval, false},
		{"minimum itself passes unclamped", "1h", minPollInterval, false},
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

// TestSanitizeYAMLErrorFallbacks pins the two value-independent fallback
// branches of the decode-error redaction: an error that is not a
// *yaml.TypeError, and a TypeError entry that does not match the expected
// "cannot unmarshal !!<tag> ... into <type>" structure. Both must fall back to
// a fixed message that cannot embed any fragment of the (potentially
// secret-bearing) original error text.
func TestSanitizeYAMLErrorFallbacks(t *testing.T) {
	const secret = "leaked-secret-sentinel"

	t.Run("non-TypeError falls back to generic message", func(t *testing.T) {
		got := sanitizeYAMLError(errors.New("decode blew up near " + secret))
		want := "configuration could not be decoded (details withheld: they may embed an expanded secret)"
		if got != want {
			t.Errorf("sanitizeYAMLError(non-TypeError) = %q, want %q", got, want)
		}
	})

	t.Run("wrapped TypeError is still recognized and sanitized", func(t *testing.T) {
		typeErr := &yaml.TypeError{Errors: []string{
			"line 3: cannot unmarshal !!str `" + secret + "` into bool",
		}}
		got := sanitizeYAMLError(errors.Join(typeErr))
		if strings.Contains(got, secret) {
			t.Errorf("sanitizeYAMLError(wrapped TypeError) leaks the scalar excerpt: %q", got)
		}
		if !strings.Contains(got, "line 3: cannot unmarshal !!str <redacted> into bool") {
			t.Errorf("sanitizeYAMLError(wrapped TypeError) = %q, want redacted line/type info kept", got)
		}
	})

	t.Run("marker-less TypeError entry falls back per entry", func(t *testing.T) {
		typeErr := &yaml.TypeError{Errors: []string{
			"line 9: some future entry shape mentioning " + secret,
		}}
		got := sanitizeYAMLError(typeErr)
		want := "unmarshal errors: configuration contains a value of the wrong type"
		if got != want {
			t.Errorf("sanitizeYAMLError(marker-less entry) = %q, want %q", got, want)
		}
	})

	t.Run("unknown-key markers inside a scalar excerpt stay redacted", func(t *testing.T) {
		// A wrong-type excerpt embedding the unknown-key marker pair must not be
		// mistaken for an unknown-key entry (whose rebuild keeps the text between
		// the markers): its prefix is the unmarshal shape, not a bare "line N",
		// so it takes the redacting wrong-type branch instead.
		typeErr := &yaml.TypeError{Errors: []string{
			"line 4: cannot unmarshal !!str `" + secret + ": field oops not found in type x` into bool",
		}}
		got := sanitizeYAMLError(typeErr)
		if strings.Contains(got, secret) || strings.Contains(got, "oops") {
			t.Errorf("sanitizeYAMLError(colliding excerpt) leaks excerpt content: %q", got)
		}
		if !strings.Contains(got, "line 4: cannot unmarshal !!str <redacted> into bool") {
			t.Errorf("sanitizeYAMLError(colliding excerpt) = %q, want the wrong-type redaction", got)
		}
	})

	t.Run("dup-key markers inside a scalar excerpt stay redacted", func(t *testing.T) {
		// A wrong-type excerpt embedding the duplicate-key marker pair must not
		// be mistaken for a duplicate-key entry (whose rebuild keeps the text
		// before its first and after its last marker): its prefix is the
		// unmarshal shape, not a bare "line N", so it takes the redacting
		// wrong-type branch instead.
		typeErr := &yaml.TypeError{Errors: []string{
			"line 4: cannot unmarshal !!str `" + secret + ": mapping key x already defined at line 9-" + secret + "` into bool",
		}}
		got := sanitizeYAMLError(typeErr)
		if strings.Contains(got, secret) {
			t.Errorf("sanitizeYAMLError(dup-key colliding excerpt) leaks excerpt content: %q", got)
		}
		if !strings.Contains(got, "line 4: cannot unmarshal !!str <redacted> into bool") {
			t.Errorf("sanitizeYAMLError(dup-key colliding excerpt) = %q, want the wrong-type redaction", got)
		}
	})

	t.Run("into-marker before unmarshal-marker falls back", func(t *testing.T) {
		got := sanitizeTypeErrorEntry(" into bool then cannot unmarshal !!str " + secret)
		want := "configuration contains a value of the wrong type"
		if got != want {
			t.Errorf("sanitizeTypeErrorEntry(reordered markers) = %q, want %q", got, want)
		}
	})
}

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

// TestIsLinePrefix pins the boundary cases of the "line <digits>" guard that
// gates the unknown-key rebuild in sanitizeTypeErrorEntry: only an exact bare
// "line N" prefix qualifies; empty input, a missing/non-numeric number, and an
// unmarshal-shaped prefix all fail.
func TestIsLinePrefix(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"line 4", true},
		{"line 123", true},
		{"", false},
		{"line", false},
		{"line ", false},
		{"line 4x", false},
		{"line x", false},
		{"LINE 4", false},
		{" line 4", false},
		{"line 4: cannot unmarshal !!str `x`", false},
	}
	for _, tt := range tests {
		if got := isLinePrefix(tt.in); got != tt.want {
			t.Errorf("isLinePrefix(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
