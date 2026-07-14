package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExpandEnvSafe(t *testing.T) {
	t.Setenv("SONARR_API_KEY", "secret-sonarr")
	t.Setenv("RADARR_URL", "http://radarr:7878")
	t.Setenv("SEADEX_SCOUT_PROWLARR_KEY", "pk")
	t.Setenv("HOME", "/root")

	tests := []struct {
		name string
		key  string
		want string
	}{
		{"allowlisted set sonarr expands", "SONARR_API_KEY", "secret-sonarr"},
		{"allowlisted set radarr expands", "RADARR_URL", "http://radarr:7878"},
		{"allowlisted set seadex_scout expands", "SEADEX_SCOUT_PROWLARR_KEY", "pk"},
		{"allowlisted but unset stays literal", "SONARR_MISSING", "${SONARR_MISSING}"},
		{"non-allowlisted set stays literal", "HOME", "${HOME}"},
		{"non-allowlisted unset stays literal", "PATH_NOT_ALLOWED", "${PATH_NOT_ALLOWED}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := expandEnvSafe(tt.key); got != tt.want {
				t.Errorf("expandEnvSafe(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

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

func captureDefaultSlog(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

func TestLoadWarnsOnUnresolvedAllowlistedEnv(t *testing.T) {
	buf := captureDefaultSlog(t)
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
	got := buf.String()
	if !strings.Contains(got, "config references environment variables") || !strings.Contains(got, "SONARR_MISSING") {
		t.Errorf("Load unresolved-env warning = %q, want message and variable name", got)
	}
}

func TestParseLogLevelWarnsOnUnrecognizedValue(t *testing.T) {
	buf := captureDefaultSlog(t)

	if got := parseLogLevel("verbose"); got != slog.LevelInfo {
		t.Errorf("parseLogLevel() = %v, want info fallback", got)
	}
	got := buf.String()
	if !strings.Contains(got, "unrecognized log.level") || !strings.Contains(got, "verbose") {
		t.Errorf("parseLogLevel warning = %q, want message and rejected value", got)
	}
}
