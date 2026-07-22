package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/slogx"
	"github.com/cplieger/slogx/capture"
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
// sanitizeYAMLError.
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
		c.IndexerAPIKey = "fk"
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if !rec.Contains("indexer keys are set but no torznab url is configured") {
			t.Errorf("Validate() log = %v, want the half-configured indexer info", rec.Messages())
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
		IndexerABPasskey:      "passkey",
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
}

// TestValidateIndexerShortFeedKeyWarning pins the warn-only strength floor on
// indexer.feed_api_key: a key under 16 characters warns (it gates the
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
// sanitizeYAMLError like the decode errors.
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

// TestPollIntervalFromFile pins the health probe's freshness-deadline source:
// the effective interval (parse+clamp) from a well-formed config, 0 (deadline
// disabled) for external mode and for EVERY read or parse failure, the Load
// default when the key is absent, tolerance of unknown keys (strictness is
// Load's job, per the function's contract), and the same allowlisted ${VAR}
// expansion Load applies (an env-referenced interval must yield the expanded
// value, not the unparseable literal).
func TestPollIntervalFromFile(t *testing.T) {
	write := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	tests := []struct {
		name    string
		content string
		want    time.Duration
	}{
		{"scheduled interval is returned", "poll_interval: \"6h\"\n", 6 * time.Hour},
		{"below-minimum interval is clamped like Load", "poll_interval: \"30m\"\n", minPollInterval},
		{"external mode disables the deadline", "poll_interval: \"off\"\n", 0},
		{"disabled sentinel disables the deadline", "poll_interval: \"disabled\"\n", 0},
		{"absent key falls back to the default interval", "mode: \"daemon\"\n", DefaultPollInterval},
		{"empty file falls back to the default interval", "", DefaultPollInterval},
		{"unknown keys are tolerated", "not_a_real_key: 1\npoll_interval: \"6h\"\n", 6 * time.Hour},
		{"multi-document file is tolerated", "poll_interval: \"6h\"\n---\nignored: true\n", 6 * time.Hour},
		{"malformed YAML disables the deadline", "poll_interval: [\n", 0},
		{"wrong value type disables the deadline", "poll_interval: {h: 3}\n", 0},
		{"oversized file disables the deadline", "poll_interval: \"6h\"\n#" + strings.Repeat("x", maxConfigBytes) + "\n", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PollIntervalFromFile(write(t, tt.content)); got != tt.want {
				t.Errorf("PollIntervalFromFile = %v, want %v", got, tt.want)
			}
		})
	}
	t.Run("missing file disables the deadline", func(t *testing.T) {
		if got := PollIntervalFromFile(filepath.Join(t.TempDir(), "absent.yaml")); got != 0 {
			t.Errorf("PollIntervalFromFile(absent) = %v, want 0", got)
		}
	})
	t.Run("allowlisted ${VAR} reference is expanded like Load", func(t *testing.T) {
		t.Setenv("SEADEX_SCOUT_POLL_INTERVAL", "6h")
		path := write(t, "poll_interval: \"${SEADEX_SCOUT_POLL_INTERVAL}\"\n")
		got := PollIntervalFromFile(path)
		if got != 6*time.Hour {
			t.Errorf("PollIntervalFromFile = %v, want 6h from the expanded env value", got)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got != cfg.PollInterval {
			t.Errorf("PollIntervalFromFile = %v, Load().PollInterval = %v; the probe must apply the same expansion Load does", got, cfg.PollInterval)
		}
	})
	t.Run("expanded env value off disables the deadline", func(t *testing.T) {
		t.Setenv("SEADEX_SCOUT_POLL_INTERVAL", "off")
		path := write(t, "poll_interval: \"${SEADEX_SCOUT_POLL_INTERVAL}\"\n")
		if got := PollIntervalFromFile(path); got != 0 {
			t.Errorf("PollIntervalFromFile = %v, want 0 for an env-provided external mode", got)
		}
	})
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
	if !rec.Contains("sonarr.api_key is set but sonarr.enabled is false") {
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
// (Decoder.Decode returns io.EOF), exercising checkSingleDocument's
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
