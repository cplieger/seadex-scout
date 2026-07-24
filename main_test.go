package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/arrapi"
	"github.com/cplieger/health"
	"github.com/cplieger/scheduler/v2"
	"github.com/cplieger/seadex-scout/internal/audit"
	"github.com/cplieger/seadex-scout/internal/config"
	"github.com/cplieger/slogx"
	"github.com/cplieger/slogx/capture"
)

// TestResolveMode covers the subcommand-vs-config mode resolution: no argument
// falls back to the config's mode, the three subcommands are accepted verbatim,
// and anything else is an invocation error (exit 2 in main).
func TestResolveMode(t *testing.T) {
	cfg := &config.Config{RunMode: config.RunModeReport}
	tests := []struct {
		name    string
		want    string
		args    []string
		wantErr bool
	}{
		{"no args falls back to the config mode", config.RunModeReport, nil, false},
		{"daemon subcommand", config.RunModeDaemon, []string{"daemon"}, false},
		{"report subcommand", config.RunModeReport, []string{"report"}, false},
		{"poll subcommand", modePoll, []string{"poll"}, false},
		{"unknown subcommand errors", "", []string{"indexer"}, true},
		{"health is not resolved here (handled before config load)", "", []string{"health"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveMode(tt.args, cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveMode(%v) err = %v, wantErr %v", tt.args, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("resolveMode(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

// TestValidateInvocation covers the trailing-argument gate that runs before
// the health fast path: at most one subcommand is accepted (a `poll typo` must
// never run a real poll or report healthy), and the error names the valid
// invocations. main maps a non-nil error to exit 2.
func TestValidateInvocation(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"no arguments", nil, false},
		{"one subcommand", []string{"poll"}, false},
		{"trailing argument", []string{"poll", "typo"}, true},
		{"trailing argument after health", []string{"health", "typo"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateInvocation(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateInvocation(%v) = %v, wantErr %v", tt.args, err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), validArgsHint) {
				t.Errorf("err = %q, want it to carry the valid-invocations hint", err)
			}
		})
	}
}

// TestRunHealthProbeNotApplicable covers the fast path's dispatch test: any
// invocation other than exactly `health` reports false so main continues with
// normal startup. The true branch cannot be exercised here - health.RunProbe
// terminates the process by contract - which is exactly why the dispatch test
// is extracted and pinned separately.
func TestRunHealthProbeNotApplicable(t *testing.T) {
	for _, args := range [][]string{nil, {"poll"}, {"report"}, {"daemon"}, {"health", "typo"}} {
		if runHealthProbe(args, filepath.Join(t.TempDir(), "config.yaml")) {
			t.Errorf("runHealthProbe(%v) = true, want false (not the health subcommand)", args)
		}
	}
}

// TestLoadRuntimeConfig covers the config-bootstrap sequence main exits 1 on:
// a first boot writes the starter and returns the typed errStarterWritten
// sentinel (an expected outcome, not a fault), a starter write failure and a
// load failure return ordinary errors, and a present valid config loads.
func TestLoadRuntimeConfig(t *testing.T) {
	t.Run("first boot writes the starter and returns the sentinel", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		_, err := loadRuntimeConfig(path)
		if !errors.Is(err, errStarterWritten) {
			t.Fatalf("loadRuntimeConfig(missing config) = %v, want errStarterWritten", err)
		}
		got, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("reading starter: %v", readErr)
		}
		if !bytes.Equal(got, exampleConfig) {
			t.Errorf("starter content differs from embedded example (%d vs %d bytes)", len(got), len(exampleConfig))
		}
	})
	t.Run("starter write failure is not the sentinel", func(t *testing.T) {
		// A dangling parent symlink makes os.Stat report the config missing
		// while the starter's parent creation fails deterministically for
		// every UID (root-safe, unlike a read-only-dir chmod).
		dir := t.TempDir()
		missingTarget := filepath.Join(dir, "missing-target")
		blockedParent := filepath.Join(dir, "blocked-parent")
		if err := os.Symlink(missingTarget, blockedParent); err != nil {
			t.Fatal(err)
		}
		_, err := loadRuntimeConfig(filepath.Join(blockedParent, "config.yaml"))
		if err == nil {
			t.Fatal("loadRuntimeConfig(blocked starter path) = nil, want error")
		}
		if errors.Is(err, errStarterWritten) {
			t.Errorf("err = %v, must not read as a successfully written starter", err)
		}
		if !strings.Contains(err.Error(), "write starter config") {
			t.Errorf("err = %q, want the starter-write failure, not a config-load failure", err)
		}
	})
	t.Run("present valid config loads", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, exampleConfig, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadRuntimeConfig(path); err != nil {
			t.Fatalf("loadRuntimeConfig(example config) = %v, want nil", err)
		}
	})
	t.Run("malformed config is a load failure", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("{not yaml"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := loadRuntimeConfig(path)
		if err == nil {
			t.Fatal("loadRuntimeConfig(malformed config) = nil, want error")
		}
		if errors.Is(err, errStarterWritten) {
			t.Errorf("err = %v, must not read as a written starter", err)
		}
	})
}

// TestIndexerConfigured covers the daemon's HTTP-surface gate: the Torznab feed
// starts iff at least one Prowlarr Torznab URL is set (the shared
// config.IndexerConfigured decision the composition root and validation read).
func TestIndexerConfigured(t *testing.T) {
	tests := []struct {
		name string
		nyaa string
		ab   string
		want bool
	}{
		{"both empty stays socket-less", "", "", false},
		{"nyaa URL alone enables the feed", "http://prowlarr:9696/22/api", "", true},
		{"ab URL alone enables the feed", "", "http://prowlarr:9696/2/api", true},
		{"both URLs enable the feed", "http://prowlarr:9696/22/api", "http://prowlarr:9696/2/api", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{IndexerNyaaTorznabURL: tt.nyaa, IndexerABTorznabURL: tt.ab}
			if got := cfg.IndexerConfigured(); got != tt.want {
				t.Errorf("IndexerConfigured(nyaa=%q, ab=%q) = %v, want %v", tt.nyaa, tt.ab, got, tt.want)
			}
		})
	}
}

// TestArrClientHelpersReturnNilInterface pins the typed-nil guard: passing a
// nil *arrapi.Sonarr/*arrapi.Radarr straight into the interface field would
// produce a NON-nil interface holding a nil pointer, and the walker would then
// call through it and panic. The helpers exist to return a true nil interface.
func TestArrClientHelpersReturnNilInterface(t *testing.T) {
	if got := sonarrClient(nil); got != nil {
		t.Errorf("sonarrClient(nil) = %v, want nil interface", got)
	}
	if got := radarrClient(nil); got != nil {
		t.Errorf("radarrClient(nil) = %v, want nil interface", got)
	}
}

// TestLogConfigNeverLogsSecrets pins the security-log contract documented on
// logConfig ("API keys are never logged"): the startup config line must not
// contain any configured API key or passkey value. Serial (swaps slog.Default).
func TestLogConfigNeverLogsSecrets(t *testing.T) {
	prev := slog.Default()
	defer slog.SetDefault(prev)
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))

	// Fixture values stay under 10 characters so the CI secret scanner's
	// generic-api-key rule (which needs a >=10-char secret-shaped value next
	// to a *APIKey field) does not flag them; they only need to be distinct
	// strings the assertion below can look for.
	cfg := &config.Config{
		SonarrURL: "http://sonarr:8989", SonarrAPIKey: "sekrit-1s",
		RadarrURL: "http://radarr:7878", RadarrAPIKey: "sekrit-2r",
		IndexerProwlarrAPIKey: "sekrit-3p",
		IndexerABPasskey:      "sekrit-4a",
		RunMode:               config.RunModeDaemon,
	}
	logConfig(cfg)

	out := buf.String()
	if out == "" {
		t.Fatal("logConfig emitted nothing, want a configuration line")
	}
	for _, secret := range []string{"sekrit-1s", "sekrit-2r", "sekrit-3p", "sekrit-4a"} {
		if strings.Contains(out, secret) {
			t.Errorf("startup config log leaks secret %q: %s", secret, out)
		}
	}
	if !strings.Contains(out, "sonarr_enabled") {
		t.Errorf("startup config log missing sonarr_enabled: %s", out)
	}
}

// TestWriteStarterConfig covers the first-boot path: the starter is written at
// the given path (parent directories created) with the embedded example bytes.
func TestWriteStarterConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "config.yaml")
	if err := writeStarterConfig(path); err != nil {
		t.Fatalf("writeStarterConfig(%q) = %v, want nil", path, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading starter: %v", err)
	}
	if !bytes.Equal(got, exampleConfig) {
		t.Errorf("starter content differs from embedded example (%d vs %d bytes)", len(got), len(exampleConfig))
	}
}

// TestNewArrClients covers the constructor gate: disabled arrs yield nil
// clients, a valid pair yields a client, and an invalid URL surfaces as a
// wrapped error naming the arr. arrapi constructors validate parameters
// without any network I/O, so this is hermetic.
func TestNewArrClients(t *testing.T) {
	t.Run("both disabled yields nil clients", func(t *testing.T) {
		s, r, err := newArrClients(&config.Config{})
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if s != nil || r != nil {
			t.Errorf("clients = (%v, %v), want (nil, nil)", s, r)
		}
	})
	t.Run("valid pairs yield clients", func(t *testing.T) {
		cfg := &config.Config{
			SonarrURL: "http://sonarr:8989", SonarrAPIKey: "k1",
			RadarrURL: "http://radarr:7878", RadarrAPIKey: "k2",
		}
		s, r, err := newArrClients(cfg)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if s == nil || r == nil {
			t.Fatalf("clients = (%v, %v), want both non-nil", s, r)
		}
		s.Close()
		r.Close()
	})
	t.Run("invalid sonarr URL errors with the arr name", func(t *testing.T) {
		cfg := &config.Config{SonarrURL: "not-a-url", SonarrAPIKey: "k"}
		_, _, err := newArrClients(cfg)
		if err == nil {
			t.Fatal("err = nil, want error for invalid sonarr URL")
		}
		if !strings.Contains(err.Error(), "sonarr client") {
			t.Errorf("err = %q, want it to name the sonarr client", err)
		}
	})
	t.Run("invalid radarr URL errors with the arr name", func(t *testing.T) {
		cfg := &config.Config{RadarrURL: "not-a-url", RadarrAPIKey: "k"}
		_, _, err := newArrClients(cfg)
		if err == nil {
			t.Fatal("err = nil, want error for invalid radarr URL")
		}
		if !strings.Contains(err.Error(), "radarr client") {
			t.Errorf("err = %q, want it to name the radarr client", err)
		}
	})
}

// TestDispatchRejectsInvalidConfig pins the validation gate: dispatch must
// refuse to run any mode on a config that fails Validate, wrapping the error.
func TestDispatchRejectsInvalidConfig(t *testing.T) {
	err := dispatch(config.RunModeReport, &config.Config{})
	if err == nil {
		t.Fatal("dispatch(report, zero config) = nil, want validation error")
	}
	if !strings.Contains(err.Error(), "invalid configuration") {
		t.Errorf("err = %q, want it wrapped as invalid configuration", err)
	}
}

// TestConfigureLoggerAppliesLevel pins the configured level onto the default
// logger. Serial (mutates slog.Default); the previous default is restored.
func TestConfigureLoggerAppliesLevel(t *testing.T) {
	prev := slog.Default()
	defer slog.SetDefault(prev)

	configureLogger(slog.LevelWarn, slogx.JSON)
	ctx := context.Background()
	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		t.Error("Debug enabled at level=warn, want disabled")
	}
	if !slog.Default().Enabled(ctx, slog.LevelWarn) {
		t.Error("Warn disabled at level=warn, want enabled")
	}

	configureLogger(slog.LevelDebug, slogx.Text)
	if !slog.Default().Enabled(ctx, slog.LevelDebug) {
		t.Error("Debug disabled at level=debug, want enabled")
	}
}

// TestInstallLoggerInitialLevel pins installLogger's documented contract: the
// pre-config default handler emits at Info (so first-boot and config-parse
// warnings are visible on the container log stream) and not at Debug, until
// configureLogger applies the configured level. Serial (swaps slog.Default);
// the previous default is restored.
func TestInstallLoggerInitialLevel(t *testing.T) {
	prev := slog.Default()
	defer slog.SetDefault(prev)

	installLogger()
	ctx := context.Background()
	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		t.Error("Debug enabled before config is read, want the documented Info floor")
	}
	if !slog.Default().Enabled(ctx, slog.LevelInfo) {
		t.Error("Info disabled before config is read, want enabled (config-parse warnings must emit)")
	}
}

// TestFeedWriter pins the nil-when-unconfigured contract: the compare cycle
// does feed work only when the Torznab feed is configured, and the returned
// cleanup is a callable no-op then.
func TestFeedWriter(t *testing.T) {
	log := slog.Default()
	fw, cleanup := feedWriter(&config.Config{}, log)
	cleanup()
	if fw != nil {
		t.Errorf("feedWriter(unconfigured) = %v, want nil (cycle must skip feed work)", fw)
	}
	cfg := &config.Config{IndexerNyaaTorznabURL: "http://prowlarr:9696/22/api"}
	fw, cleanup = feedWriter(cfg, log)
	defer cleanup()
	if fw == nil {
		t.Error("feedWriter(configured) = nil, want a FeedWriter")
	}
}

// TestFilterOptions pins the config-to-filter field mapping so a swapped or
// dropped field in the wiring cannot silently invert a content filter. The
// AnimeBytes tracker toggle is not part of filter.Options (it rides
// compare.Config / audit.Config directly).
func TestFilterOptions(t *testing.T) {
	cfg := &config.Config{ExcludeRemux: true, RequireDualAudio: false, AnimeBytes: true}
	got := filterOptions(cfg)
	if !got.ExcludeRemux {
		t.Error("ExcludeRemux = false, want true")
	}
	if got.RequireDualAudio {
		t.Error("RequireDualAudio = true, want false")
	}
}

// TestUpstreamConfig pins the config-to-indexer upstream field mapping shared
// by the feed writer and the feed server, so a swapped or dropped field in the
// wiring cannot route Nyaa searches at the AB endpoint or hand the wrong
// credential to an upstream.
func TestUpstreamConfig(t *testing.T) {
	cfg := &config.Config{
		IndexerNyaaTorznabURL: "http://prowlarr:9696/22/api",
		IndexerABTorznabURL:   "http://prowlarr:9696/2/api",
		IndexerProwlarrAPIKey: "pk-3x",
		IndexerABPasskey:      "ab-4y",
	}
	got := upstreamConfig(cfg)
	if got.NyaaTorznabURL != cfg.IndexerNyaaTorznabURL {
		t.Errorf("NyaaTorznabURL = %q, want %q", got.NyaaTorznabURL, cfg.IndexerNyaaTorznabURL)
	}
	if got.ABTorznabURL != cfg.IndexerABTorznabURL {
		t.Errorf("ABTorznabURL = %q, want %q", got.ABTorznabURL, cfg.IndexerABTorznabURL)
	}
	if got.ProwlarrAPIKey != cfg.IndexerProwlarrAPIKey {
		t.Errorf("ProwlarrAPIKey = %q, want %q", got.ProwlarrAPIKey, cfg.IndexerProwlarrAPIKey)
	}
	if got.ABPasskey != cfg.IndexerABPasskey {
		t.Errorf("ABPasskey = %q, want %q", got.ABPasskey, cfg.IndexerABPasskey)
	}
}

// panicCycler is a cycler whose cycle always panics, exercising the daemon
// panic shield in runCycle.
type panicCycler struct{}

func (panicCycler) Cycle(context.Context) bool { panic("boom") }

// boolCycler is a cycler returning a fixed health outcome.
type boolCycler bool

func (b boolCycler) Cycle(context.Context) bool { return bool(b) }

// TestRunCyclePanicShield pins the daemon crash shield: a panicking cycle is
// recovered and reported unhealthy instead of crashing the long-lived daemon,
// and a normal cycle outcome passes through unchanged.
func TestRunCyclePanicShield(t *testing.T) {
	ctx := context.Background()
	if healthy := runCycle(ctx, panicCycler{}); healthy {
		t.Error("runCycle(panicking cycle) = healthy, want unhealthy")
	}
	if healthy := runCycle(ctx, boolCycler(true)); !healthy {
		t.Error("runCycle(healthy cycle) = unhealthy, want healthy")
	}
	if healthy := runCycle(ctx, boolCycler(false)); healthy {
		t.Error("runCycle(unhealthy cycle) = healthy, want unhealthy")
	}
}

// TestLogConfigMasksInvalidRunMode pins the invalid-mode redaction contract
// documented in logConfig: an unrecognized run_mode (which may be an expanded
// ${VAR} secret placed by a config typo) is logged as the fixed marker
// "invalid", never the raw value. Serial (swaps slog.Default).
func TestLogConfigMasksInvalidRunMode(t *testing.T) {
	prev := slog.Default()
	defer slog.SetDefault(prev)
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))

	cfg := &config.Config{RunMode: "leaked-secret-value-9"}
	logConfig(cfg)

	out := buf.String()
	if strings.Contains(out, "leaked-secret-value-9") {
		t.Errorf("startup config log leaks the raw run_mode value: %s", out)
	}
	if !strings.Contains(out, `"run_mode":"invalid"`) {
		t.Errorf("run_mode not logged as the fixed marker %q: %s", "invalid", out)
	}
}

// TestLoggableModeMasksUnknownMode pins the same redaction contract at main's
// terminal log sites: loggableMode passes the known run modes through and maps
// anything else (which may be an expanded ${VAR} secret placed by a config
// typo) to the fixed marker "invalid", so the dispatch-failure lines never
// echo the raw value. Serial (swaps slog.Default).
func TestLoggableModeMasksUnknownMode(t *testing.T) {
	for _, mode := range []string{config.RunModeDaemon, config.RunModeReport, modePoll} {
		if got := loggableMode(mode); got != mode {
			t.Errorf("loggableMode(%q) = %q, want the known mode passed through", mode, got)
		}
	}
	const secret = "leaked-secret-value-9"
	if got := loggableMode(secret); got != "invalid" {
		t.Errorf("loggableMode(%q) = %q, want the fixed marker %q", secret, got, "invalid")
	}

	prev := slog.Default()
	defer slog.SetDefault(prev)
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))

	// The exact failure line main emits when dispatch rejects the mode.
	slog.Error("seadex-scout failed", "mode", loggableMode(secret), "error", errors.New("invalid configuration"))

	out := buf.String()
	if strings.Contains(out, secret) {
		t.Errorf("dispatch-failure log leaks the raw mode value: %s", out)
	}
	if !strings.Contains(out, `"mode":"invalid"`) {
		t.Errorf("mode not logged as the fixed marker %q: %s", "invalid", out)
	}
}

// TestLogConfigExternalPollInterval pins the resident-idle rendering: with
// poll_interval off (PollExternal), the startup line reports "external", not a
// zero duration. Serial (swaps slog.Default).
func TestLogConfigExternalPollInterval(t *testing.T) {
	prev := slog.Default()
	defer slog.SetDefault(prev)
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))

	cfg := &config.Config{PollExternal: true, RunMode: config.RunModeDaemon}
	logConfig(cfg)

	if !strings.Contains(buf.String(), `"poll_interval":"external"`) {
		t.Errorf("poll_interval not rendered as external: %s", buf.String())
	}
}

// TestArrClientHelpersPassThrough pins the other half of the typed-nil guard:
// a real client must pass through as a non-nil interface, otherwise the walker
// would treat an enabled arr as disabled.
func TestArrClientHelpersPassThrough(t *testing.T) {
	s, err := arrapi.NewSonarr("http://sonarr:8989", "k1")
	if err != nil {
		t.Fatalf("NewSonarr: %v", err)
	}
	defer s.Close()
	if got := sonarrClient(s); got == nil {
		t.Error("sonarrClient(non-nil) = nil, want the client as a non-nil interface")
	}
	r, err := arrapi.NewRadarr("http://radarr:7878", "k2")
	if err != nil {
		t.Fatalf("NewRadarr: %v", err)
	}
	defer r.Close()
	if got := radarrClient(r); got == nil {
		t.Error("radarrClient(non-nil) = nil, want the client as a non-nil interface")
	}
}

// TestWriteStarterConfigError pins the first-boot failure contract: a write
// failure (here a parent path component that is a regular file) surfaces as a
// wrapped error rather than being swallowed, so main exits 1 with the
// could-not-write-a-starter log instead of claiming success.
func TestWriteStarterConfigError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(blocker, "config.yaml")
	err := writeStarterConfig(path)
	if err == nil {
		t.Fatalf("writeStarterConfig(%q) = nil, want error (parent is a regular file)", path)
	}
	if !strings.Contains(err.Error(), "write starter config") {
		t.Errorf("err = %q, want it wrapped as write starter config", err)
	}
}

// TestBuildScout pins the composition wiring hermetically: with both arrs
// disabled the full component graph builds without any network I/O (pingArrs
// is a no-op on nil clients) and cleanup is callable, and an invalid arr URL
// propagates as a build error instead of being swallowed.
func TestBuildScout(t *testing.T) {
	t.Run("disabled arrs build hermetically", func(t *testing.T) {
		b, err := buildScout(context.Background(), &config.Config{}, false)
		if err != nil {
			t.Fatalf("buildScout(zero config) = %v, want nil", err)
		}
		if b.scout == nil {
			t.Fatal("scout = nil, want a wired scout")
		}
		b.cleanup()
	})
	t.Run("read-only state store builds hermetically", func(t *testing.T) {
		b, err := buildScout(context.Background(), &config.Config{}, true)
		if err != nil {
			t.Fatalf("buildScout(zero config, read-only state) = %v, want nil", err)
		}
		if b.scout == nil {
			t.Fatal("scout = nil, want a wired scout")
		}
		b.cleanup()
	})
	t.Run("invalid sonarr URL propagates", func(t *testing.T) {
		cfg := &config.Config{SonarrURL: "not-a-url", SonarrAPIKey: "k"}
		if _, err := buildScout(context.Background(), cfg, false); err == nil {
			t.Fatal("buildScout(invalid sonarr URL) = nil, want error")
		}
	})
}

// TestPingArrs pins the startup-diagnostics contract: pinging is never fatal,
// a reachable arr logs an INFO reachable line, and an erroring arr logs the
// WARN ping-failed line (not a false reachable). The arr endpoints are faked
// with httptest (arrapi.Ping GETs /api/v3/system/status). Serial (swaps
// slog.Default).
func TestPingArrs(t *testing.T) {
	rec := capture.Default(t)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer down.Close()

	s, err := arrapi.NewSonarr(up.URL, "k")
	if err != nil {
		t.Fatalf("NewSonarr: %v", err)
	}
	defer s.Close()
	r, err := arrapi.NewRadarr(down.URL, "k")
	if err != nil {
		t.Fatalf("NewRadarr: %v", err)
	}
	defer r.Close()

	pingArrs(context.Background(), s, r)

	if !rec.Contains("sonarr reachable") {
		t.Errorf("missing sonarr reachable info line: %v", rec.Messages())
	}
	if !rec.Contains("radarr ping failed at startup") {
		t.Errorf("missing radarr ping-failed warn line: %v", rec.Messages())
	}
}

// cancelCycler cancels the poll context during the cycle and returns the
// configured outcome, simulating a shutdown signal landing mid-cycle (cycle
// reports unhealthy) or during the end-of-cycle save (cycle still completed
// healthy).
type cancelCycler struct {
	cancel  context.CancelFunc
	healthy bool
}

func (c cancelCycler) Cycle(context.Context) bool {
	c.cancel()
	return c.healthy
}

// testCycleExclusiveIn builds a cycle coalescer for tests in the given dir,
// wired exactly like production (newCycleExclusive, including the shutdown
// gate on ctx), so the tests exercise the real gate and lock wiring. Takes
// the dir explicitly so lock-contention tests can share it with holdCycleLock
// or a seeded queue file.
func testCycleExclusiveIn(t *testing.T, ctx context.Context, dir string) *scheduler.Exclusive {
	t.Helper()
	ex, err := newCycleExclusive(ctx, dir)
	if err != nil {
		t.Fatalf("newCycleExclusive: %v", err)
	}
	return ex
}

// testCycleExclusive builds a cycle coalescer for tests in a temp dir, wired
// exactly like production (newCycleExclusive, including the shutdown gate on
// ctx), so the tests exercise the real gate and lock wiring.
func testCycleExclusive(t *testing.T, ctx context.Context) *scheduler.Exclusive {
	t.Helper()
	return testCycleExclusiveIn(t, ctx, t.TempDir())
}

// seedSentinelMarker writes a pre-existing health marker standing in for the
// daemon's last real state and returns its path; assertMarkerUntouched is its
// paired check that the code under test never touched it.
func seedSentinelMarker(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".healthy")
	if err := os.WriteFile(path, []byte("sentinel-untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// assertMarkerUntouched fails the test when the marker content no longer
// matches the seeded sentinel, i.e. the code under test touched the marker.
func assertMarkerUntouched(t *testing.T, path string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("marker file: %v", err)
	}
	if string(got) != "sentinel-untouched" {
		t.Errorf("marker content = %q, want the pre-existing state untouched", got)
	}
}

// TestPollCycleUniformInterruption pins poll's uniform interruption contract:
// a cancellation observed at ANY phase - before the cycle starts (the shutdown
// gate refuses the run), mid-cycle, or after a cycle that still completed
// healthy (the signal landed during the save) - returns an error wrapping
// context.Canceled (which main classifies as a routine-shutdown WARN, never
// ERROR, and maps to exit 1) and never touches the health marker, leaving it
// at the daemon's last real state.
func TestPollCycleUniformInterruption(t *testing.T) {
	tests := []struct {
		cycler    func(t *testing.T, cancel context.CancelFunc) cycler
		name      string
		preCancel bool
	}{
		{func(t *testing.T, _ context.CancelFunc) cycler { return mustNotRunCycler{t: t} }, "pre-cycle cancellation", true},
		{func(_ *testing.T, cancel context.CancelFunc) cycler { return cancelCycler{cancel: cancel} }, "mid-cycle cancellation", false},
		{func(_ *testing.T, cancel context.CancelFunc) cycler {
			return cancelCycler{cancel: cancel, healthy: true}
		}, "post-cycle cancellation after a healthy cycle", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ex := testCycleExclusive(t, ctx)
			if tt.preCancel {
				cancel()
			}
			// A pre-existing marker stands in for the daemon's last real state;
			// its content must survive the interrupted poll byte-for-byte.
			path := seedSentinelMarker(t)

			err := pollCycle(ctx, ex, tt.cycler(t, cancel), health.NewMarker(path))

			if err == nil {
				t.Fatal("pollCycle = nil, want the interruption error (exit 1)")
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("err = %v, want it to wrap context.Canceled (main classifies the interruption WARN, not ERROR)", err)
			}
			assertMarkerUntouched(t, path)
		})
	}
}

// TestPollCycleBusyLockPreCancelled pins the queue-insertion side of poll's
// uniform interruption contract: a poll arriving pre-cancelled while another
// process holds the cycle lock must NOT enqueue demand or report success —
// Exclusive's gate refuses the run, not the queue insertion, so without the
// pre-Run check the cancelled poll would still queue work after shutdown. It
// returns the interruption error (wrapping context.Canceled, so main
// classifies it WARN and exits non-zero), never runs a cycle, leaves the
// health marker untouched, and records no pending rerun for the lock holder.
func TestPollCycleBusyLockPreCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := t.TempDir()
	ex := testCycleExclusiveIn(t, ctx, dir)
	holdCycleLock(t, dir)
	path := seedSentinelMarker(t)

	err := pollCycle(ctx, ex, mustNotRunCycler{t: t}, health.NewMarker(path))

	if err == nil {
		t.Fatal("pollCycle = nil, want the interruption error (exit 1)")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled (main classifies the interruption WARN, not ERROR)", err)
	}
	assertMarkerUntouched(t, path)
	if pending, perr := ex.Pending(); perr != nil || pending != 0 {
		t.Errorf("Pending() = (%d, %v), want (0, nil): a cancelled poll must not enqueue demand", pending, perr)
	}
}

// TestPollCycleGatedRun pins the OutcomeGated leg of poll's uniform
// interruption contract: shutdown lands in the race window between
// pollCycle's pre-Run check and the Exclusive's gate evaluation (simulated
// deterministically by a gate that cancels the shared context exactly when
// it is consulted, mirroring newCycleExclusive's ctx.Err()==nil gate). The
// run is refused (the cycle never executes), pollCycle reports the
// interruption (wrapping context.Canceled so main classifies it WARN and
// exits non-zero), and the health marker is left at the daemon's last real
// state.
func TestPollCycleGatedRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ex := scheduler.NewExclusive(t.TempDir(), slog.Default(),
		scheduler.WithGate(func() bool {
			cancel() // shutdown arrives exactly as the gate is consulted
			return ctx.Err() == nil
		}))
	path := seedSentinelMarker(t)

	err := pollCycle(ctx, ex, mustNotRunCycler{t: t}, health.NewMarker(path))

	if err == nil {
		t.Fatal("pollCycle(gated run) = nil, want the interruption error (exit 1)")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled (main classifies the interruption WARN, not ERROR)", err)
	}
	assertMarkerUntouched(t, path)
	if pending, perr := ex.Pending(); perr != nil || pending != 0 {
		t.Errorf("Pending() = (%d, %v), want (0, nil): a gated fresh acquisition must not leave queued demand", pending, perr)
	}
}

// TestPollCycleUninterrupted pins poll's normal contract: a healthy cycle sets
// the marker healthy and exits 0; an unhealthy cycle sets it unhealthy and
// returns the ingest error (exit 1) without reading as an interruption.
func TestPollCycleUninterrupted(t *testing.T) {
	t.Run("healthy cycle sets the marker", func(t *testing.T) {
		marker := health.NewMarker(filepath.Join(t.TempDir(), ".healthy"))
		if err := pollCycle(context.Background(), testCycleExclusive(t, context.Background()), boolCycler(true), marker); err != nil {
			t.Fatalf("pollCycle(healthy) = %v, want nil", err)
		}
		if !marker.Healthy() {
			t.Error("marker not healthy after a healthy cycle")
		}
	})
	t.Run("unhealthy cycle sets the marker and errors", func(t *testing.T) {
		marker := health.NewMarker(filepath.Join(t.TempDir(), ".healthy"))
		err := pollCycle(context.Background(), testCycleExclusive(t, context.Background()), boolCycler(false), marker)
		if err == nil {
			t.Fatal("pollCycle(unhealthy) = nil, want the ingest error")
		}
		if errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, must not read as an interruption", err)
		}
		if marker.Healthy() {
			t.Error("marker healthy after an unhealthy cycle")
		}
	})
}

// queueThenCancelCycler drives the ran-plus-queued-rerun interruption leg:
// its first (own) run queues demand from a second process on the shared lock
// dir, and the queued rerun then cancels the shared context, simulating
// shutdown arriving while Exclusive services another process's demand after
// this invocation's own run completed.
type queueThenCancelCycler struct {
	t         *testing.T
	cancel    context.CancelFunc
	dir       string
	calls     *int
	queuedErr *error
}

func (c queueThenCancelCycler) Cycle(context.Context) bool {
	*c.calls++
	if *c.calls == 1 {
		// Another process requests a poll while this one holds the lock: it
		// must observe OutcomeQueued (its cycle never runs) and report
		// success — the demand is recorded for the active runner.
		exB := testCycleExclusiveIn(c.t, context.Background(), c.dir)
		marker := health.NewMarker(filepath.Join(c.t.TempDir(), ".healthy"))
		*c.queuedErr = pollCycle(context.Background(), exB, mustNotRunCycler{t: c.t}, marker)
		return true
	}
	c.cancel() // shutdown lands during the queued rerun
	return true
}

// TestPollCycleRanQueuedThenCancelled pins the ran-plus-queued-rerun leg of
// poll's uniform interruption contract: this process's own run completes
// healthy, Exclusive then services another process's queued rerun, and
// shutdown lands during that rerun. Run returns OutcomeRanQueued with a nil
// own result, but the cancellation observed by then must win — pollCycle
// returns the interruption error (wrapping context.Canceled, so main
// classifies it WARN and exits non-zero) instead of the own run's success —
// while the queued requester itself still returned nil.
func TestPollCycleRanQueuedThenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	ex := testCycleExclusiveIn(t, ctx, dir)
	var calls int
	var queuedErr error
	cy := queueThenCancelCycler{t: t, cancel: cancel, dir: dir, calls: &calls, queuedErr: &queuedErr}

	err := pollCycle(ctx, ex, cy, health.NewMarker(filepath.Join(t.TempDir(), ".healthy")))

	if calls != 2 {
		t.Fatalf("cycle calls = %d, want 2 (the own run plus the queued rerun)", calls)
	}
	if queuedErr != nil {
		t.Errorf("queued requester pollCycle = %v, want nil (recorded demand is success)", queuedErr)
	}
	if err == nil {
		t.Fatal("pollCycle = nil, want the interruption error (exit 1)")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled (main classifies the interruption WARN, not ERROR)", err)
	}
}

// TestRunSchedulerShutdownMidCycle pins the daemon twin of pollCycle's
// interruption contract: a shutdown-interrupted unhealthy cycle must not
// overwrite the health marker (the guard `if !healthy && ctx.Err() != nil`),
// while a cycle that still completed healthy during shutdown records its
// outcome. A regression here would flip the container unhealthy on every
// redeploy. Both paths run through the cycle lock's acquired path, so they
// also prove a tick executes normally under RunOrSkip.
func TestRunSchedulerShutdownMidCycle(t *testing.T) {
	t.Run("interrupted unhealthy cycle leaves the marker", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		marker := health.NewMarker(filepath.Join(t.TempDir(), ".healthy"))
		marker.Set(true)
		runScheduler(ctx, time.Hour, testCycleExclusive(t, ctx), cancelCycler{cancel: cancel}, marker)
		if !marker.Healthy() {
			t.Error("marker unhealthy after a shutdown-interrupted cycle")
		}
	})
	t.Run("healthy cycle finished during shutdown still records", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		marker := health.NewMarker(filepath.Join(t.TempDir(), ".healthy"))
		marker.Set(false)
		runScheduler(ctx, time.Hour, testCycleExclusive(t, ctx), cancelCycler{cancel: cancel, healthy: true}, marker)
		if !marker.Healthy() {
			t.Error("marker not healthy after a healthy cycle")
		}
	})
}

// mustNotRunCycler fails the test if a cycle executes; it pins the paths where
// the cycle lock must prevent any run (a busy skip, a queued request).
type mustNotRunCycler struct{ t *testing.T }

func (c mustNotRunCycler) Cycle(context.Context) bool {
	c.t.Error("cycle ran, want it not to run")
	return true
}

// holdCycleLock seeds a bare flock holder on dir's cycle.lock, simulating a
// cycle in flight in another process (flock contends per open file
// description, so an in-process holder exercises the same kernel path).
func holdCycleLock(t *testing.T, dir string) {
	t.Helper()
	holder, ok, err := scheduler.TryLock(filepath.Join(dir, scheduler.ExclusiveLockName))
	if err != nil || !ok {
		t.Fatalf("seed TryLock = (ok=%v, err=%v), want (true, nil)", ok, err)
	}
	t.Cleanup(holder.Unlock)
}

// TestRunSchedulerSkipsBusyTick pins the daemon's skip mode: a tick arriving
// while another process holds the cycle lock is skipped - the cycle never
// runs, the health marker is untouched, and the library's pinned busy WARN is
// emitted. Serial (capture swaps slog.Default).
func TestRunSchedulerSkipsBusyTick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := captureAndCancelOn(t, cancel, "cycle lock busy; skipping tick")
	defer cancel()
	dir := t.TempDir()
	ex := testCycleExclusiveIn(t, ctx, dir)
	holdCycleLock(t, dir)

	markerPath := seedSentinelMarker(t)

	// FireOnStart executes the first tick immediately; it skips (the lock is
	// busy), then the loop waits out the interval until cancelled.
	done := make(chan struct{})
	go func() {
		defer close(done)
		runScheduler(ctx, time.Hour, ex, mustNotRunCycler{t: t}, health.NewMarker(markerPath))
	}()
	<-done

	if !rec.Contains("cycle lock busy; skipping tick") {
		t.Errorf("missing the library's busy-skip line: %v", rec.Messages())
	}
	assertMarkerUntouched(t, markerPath)
}

// cancelHandler records logs and cancels the scheduler context after the
// expected record, providing deterministic event-driven synchronization.
type cancelHandler struct {
	next    slog.Handler
	cancel  context.CancelFunc
	message string
}

func (h *cancelHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *cancelHandler) Handle(ctx context.Context, record slog.Record) error {
	err := h.next.Handle(ctx, record)
	if record.Message == h.message {
		h.cancel()
	}
	return err
}

func (h *cancelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &cancelHandler{next: h.next.WithAttrs(attrs), cancel: h.cancel, message: h.message}
}

func (h *cancelHandler) WithGroup(name string) slog.Handler {
	return &cancelHandler{next: h.next.WithGroup(name), cancel: h.cancel, message: h.message}
}

// captureAndCancelOn installs a recording default logger whose handler cancels
// the given context after the expected message is recorded, replacing
// wall-clock polling with event-driven synchronization. Serial (swaps
// slog.Default; restored via t.Cleanup).
func captureAndCancelOn(t *testing.T, cancel context.CancelFunc, message string) *capture.Recorder {
	t.Helper()
	_, rec := capture.New()
	prev := slog.Default()
	slog.SetDefault(slog.New(&cancelHandler{next: rec, cancel: cancel, message: message}))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return rec
}

// TestPollCycleQueuedWhenBusy pins poll's queue mode against a busy cycle
// lock: the request is queued for the active runner, the cycle does NOT run in
// this process, the marker stays untouched, and pollCycle exits 0 (nil) with
// the coalescing log lines. Serial (capture swaps slog.Default).
func TestPollCycleQueuedWhenBusy(t *testing.T) {
	rec := capture.Default(t)
	ctx := context.Background()
	dir := t.TempDir()
	ex := testCycleExclusiveIn(t, ctx, dir)
	holdCycleLock(t, dir)

	markerPath := seedSentinelMarker(t)

	if err := pollCycle(ctx, ex, mustNotRunCycler{t: t}, health.NewMarker(markerPath)); err != nil {
		t.Fatalf("pollCycle(busy) = %v, want nil (queued is success, exit 0)", err)
	}

	if pending, perr := ex.Pending(); perr != nil || pending != 1 {
		t.Errorf("Pending = (%d, %v), want (1, nil)", pending, perr)
	}
	if !rec.Contains("cycle lock busy; queued rerun request") {
		t.Errorf("missing the library's queued line: %v", rec.Messages())
	}
	if !rec.Contains("compare cycle already in flight; demand queued for the active runner") {
		t.Errorf("missing poll's own coalescing line: %v", rec.Messages())
	}
	assertMarkerUntouched(t, markerPath)
}

// TestPollCycleQueuedThenCancelled pins the queued-then-cancelled branch of
// poll's uniform interruption contract: the library's pinned "cycle lock
// busy; queued rerun request" line fires after the demand is recorded and
// before Run returns, so cancelling on that record deterministically lands
// the shutdown between queueing and pollCycle's post-queue check. The
// invocation must report the interruption (exit non-zero, wrapping
// context.Canceled) with the marker untouched, while the recorded demand
// still stands for the active runner. Serial (capture swaps slog.Default).
func TestPollCycleQueuedThenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := captureAndCancelOn(t, cancel, "cycle lock busy; queued rerun request")
	defer cancel()
	dir := t.TempDir()
	ex := testCycleExclusiveIn(t, ctx, dir)
	holdCycleLock(t, dir)
	path := seedSentinelMarker(t)

	err := pollCycle(ctx, ex, mustNotRunCycler{t: t}, health.NewMarker(path))

	if err == nil {
		t.Fatal("pollCycle(queued, then cancelled) = nil, want the interruption error (exit 1)")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled (main classifies the interruption WARN)", err)
	}
	assertMarkerUntouched(t, path)
	if pending, perr := ex.Pending(); perr != nil || pending != 1 {
		t.Errorf("Pending = (%d, %v), want (1, nil): the recorded demand must still stand", pending, perr)
	}
	if !rec.Contains("cycle lock busy; queued rerun request") {
		t.Errorf("missing the library's queued line: %v", rec.Messages())
	}
}

// TestPollCycleDiscardedWhenQueueFull pins the queue-full path: with a rerun
// already queued (depth 1), a second busy poll is discarded - still exit 0,
// no run, marker untouched - because the queued rerun already guarantees a
// run starts after this request arrived. Serial (capture swaps slog.Default).
func TestPollCycleDiscardedWhenQueueFull(t *testing.T) {
	rec := capture.Default(t)
	ctx := context.Background()
	dir := t.TempDir()
	ex := testCycleExclusiveIn(t, ctx, dir)
	holdCycleLock(t, dir)
	marker := health.NewMarker(filepath.Join(t.TempDir(), ".healthy"))

	if err := pollCycle(ctx, ex, mustNotRunCycler{t: t}, marker); err != nil {
		t.Fatalf("first busy pollCycle = %v, want nil (queued)", err)
	}
	if err := pollCycle(ctx, ex, mustNotRunCycler{t: t}, marker); err != nil {
		t.Fatalf("second busy pollCycle = %v, want nil (discarded)", err)
	}

	if pending, perr := ex.Pending(); perr != nil || pending != 1 {
		t.Errorf("Pending = (%d, %v), want (1, nil) (discard must not grow the queue)", pending, perr)
	}
	if !rec.Contains("cycle lock busy; rerun already queued; discarding request") {
		t.Errorf("missing the library's discard line: %v", rec.Messages())
	}
}

// signalCycler counts cycle executions; the first execution signals started
// and blocks until release is closed (later executions pass straight
// through), so a test can deterministically hold a cycle in flight.
type signalCycler struct {
	started chan struct{}
	release chan struct{}
	runs    *atomic.Int32
}

func (c *signalCycler) Cycle(context.Context) bool {
	if c.runs.Add(1) == 1 {
		close(c.started)
	}
	<-c.release
	return true
}

// TestPollCycleExecutesQueuedRerun pins the queue-of-1 coalescing end to end
// within one process: a second poll arriving mid-cycle queues and exits 0
// immediately (never blocking for the job's duration), and the active runner
// executes exactly one rerun for it at cycle end - so the queued demand gets
// a run that started after it arrived. Serial (capture swaps slog.Default).
func TestPollCycleExecutesQueuedRerun(t *testing.T) {
	rec := capture.Default(t)
	ctx := context.Background()
	dir := t.TempDir()
	ex := testCycleExclusiveIn(t, ctx, dir)
	marker := health.NewMarker(filepath.Join(t.TempDir(), ".healthy"))

	sc := &signalCycler{
		started: make(chan struct{}),
		release: make(chan struct{}),
		runs:    &atomic.Int32{},
	}
	holderErr := make(chan error, 1)
	go func() { holderErr <- pollCycle(ctx, ex, sc, marker) }()
	<-sc.started // the holder is mid-cycle

	// The overlapping poll queues its demand and returns without running.
	if err := pollCycle(ctx, ex, mustNotRunCycler{t: t}, marker); err != nil {
		t.Fatalf("overlapping pollCycle = %v, want nil (queued)", err)
	}

	close(sc.release) // let the holder finish; its consume loop reruns once
	if err := <-holderErr; err != nil {
		t.Fatalf("holder pollCycle = %v, want nil", err)
	}

	if runs := sc.runs.Load(); runs != 2 {
		t.Errorf("cycle ran %d times, want 2 (own run + one queued rerun)", runs)
	}
	if pending, perr := ex.Pending(); perr != nil || pending != 0 {
		t.Errorf("Pending after rerun = (%d, %v), want (0, nil)", pending, perr)
	}
	if !rec.Contains("running queued cycle request") {
		t.Errorf("missing the library's rerun line: %v", rec.Messages())
	}
	if !marker.Healthy() {
		t.Error("marker not healthy after the rerun recorded its outcome")
	}
}

// queuedFailureCycler holds its first cycle in flight (signalling started,
// blocking until release) and reports healthy; any later execution - the
// queued rerun - fails, so a test can pin the rerun-failure warning.
type queuedFailureCycler struct {
	started chan struct{}
	release chan struct{}
	runs    atomic.Int32
}

func (c *queuedFailureCycler) Cycle(context.Context) bool {
	if c.runs.Add(1) == 1 {
		close(c.started)
		<-c.release
		return true
	}
	return false
}

// TestPollCycleLogsQueuedRerunFailure pins the failure signal of a queued
// rerun: the rerun has no requesting process left to receive an exit code, so
// the WARN line in executePollRuns is its only observable failure signal.
// Serial (capture swaps slog.Default).
func TestPollCycleLogsQueuedRerunFailure(t *testing.T) {
	rec := capture.Default(t)
	ctx := context.Background()
	ex := testCycleExclusive(t, ctx)
	marker := health.NewMarker(filepath.Join(t.TempDir(), ".healthy"))
	sc := &queuedFailureCycler{started: make(chan struct{}), release: make(chan struct{})}
	holderErr := make(chan error, 1)
	go func() { holderErr <- pollCycle(ctx, ex, sc, marker) }()
	<-sc.started

	if err := pollCycle(ctx, ex, mustNotRunCycler{t: t}, marker); err != nil {
		t.Fatalf("overlapping pollCycle = %v, want nil (queued)", err)
	}
	close(sc.release)
	if err := <-holderErr; err != nil {
		t.Fatalf("holder pollCycle = %v, want nil from its own healthy run", err)
	}

	if !rec.Contains("queued rerun cycle reported an error") {
		t.Errorf("missing queued-rerun error warning: %v", rec.Messages())
	}
}

// TestPollCycleCoordinationFailure pins the infrastructure-failure path:
// an unusable cycle lock (the lock path is a directory) means nothing ran and
// no demand was recorded, so pollCycle returns the error (exit 1) and never
// reads as an interruption.
func TestPollCycleCoordinationFailure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	ex := testCycleExclusiveIn(t, ctx, dir)
	if err := os.Mkdir(filepath.Join(dir, scheduler.ExclusiveLockName), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := health.NewMarker(filepath.Join(t.TempDir(), ".healthy"))

	err := pollCycle(ctx, ex, mustNotRunCycler{t: t}, marker)

	if err == nil {
		t.Fatal("pollCycle(unusable lock) = nil, want the coordination error (exit 1)")
	}
	if !strings.Contains(err.Error(), "cycle coordination failed") {
		t.Errorf("err = %q, want it wrapped as cycle coordination failed", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, must not read as an interruption", err)
	}
}

// TestNewCycleExclusiveMkdirError pins the fail-fast contract: an uncreatable
// lock directory surfaces as a wrapped error (the daemon and poll refuse to
// start uncoordinated) instead of degrading to per-tick failures.
func TestNewCycleExclusiveMkdirError(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := newCycleExclusive(context.Background(), filepath.Join(blocker, "sub"))

	if err == nil {
		t.Fatal("newCycleExclusive(uncreatable dir) = nil, want error")
	}
	if !strings.Contains(err.Error(), "create cycle lock dir") {
		t.Errorf("err = %q, want it wrapped as create cycle lock dir", err)
	}
}

// TestLogIndexerStopClassifiesShutdownAndFault pins the indexer feed's stop
// log contract: during a shutdown, an expired graceful-shutdown budget
// (DeadlineExceeded from webhttp.Run, meaning in-flight Torznab requests were
// cut off) gets its own WARN message distinct from the routine clean-shutdown
// WARN, and any error outside a shutdown stays the ERROR fault line. Serial
// (swaps slog.Default).
func TestLogIndexerStopClassifiesShutdownAndFault(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name      string
		ctx       context.Context
		err       error
		wantMsg   string
		wantLevel slog.Level
	}{
		{"budget expired during shutdown", canceled, context.DeadlineExceeded, "indexer shutdown budget expired; in-flight requests aborted", slog.LevelWarn},
		{"clean stop during shutdown", canceled, context.Canceled, "indexer feed stopped during shutdown", slog.LevelWarn},
		{"fault outside shutdown", context.Background(), errors.New("bind failed"), "indexer feed stopped", slog.LevelError},
		{"deadline exceeded outside shutdown stays a fault", context.Background(), context.DeadlineExceeded, "indexer feed stopped", slog.LevelError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := capture.Default(t)

			logIndexerStop(tt.ctx, slog.Default().With("component", "indexer"), tt.err)

			records := rec.Records()
			if len(records) != 1 {
				t.Fatalf("captured %d records, want 1 (%v)", len(records), rec.Messages())
			}
			if records[0].Message != tt.wantMsg {
				t.Errorf("msg = %q, want %q", records[0].Message, tt.wantMsg)
			}
			if records[0].Level != tt.wantLevel {
				t.Errorf("level = %v, want %v", records[0].Level, tt.wantLevel)
			}
		})
	}
}

// TestRunReportRefusesWhenLockHeld pins the report concurrency refusal end to
// end: with another run holding the report lock, runReport returns
// ErrReportRunning (exit 1) before building any component, so the refusal is
// hermetic (no network I/O) and two reports can never race onto the same
// timestamped filename pair.
func TestRunReportRefusesWhenLockHeld(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "reports")
	release, err := audit.AcquireReportLock(dir)
	if err != nil {
		t.Fatalf("holding the report lock: %v", err)
	}
	defer release()

	err = runReport(&config.Config{ReportDir: dir})
	if !errors.Is(err, audit.ErrReportRunning) {
		t.Fatalf("runReport with the lock held = %v, want ErrReportRunning", err)
	}
}

// TestLogPingClassifiesShutdownCancellation pins the shutdown-classification
// branch of logPing: a context-cancelled startup ping is a routine shutdown
// (DEBUG), not the operator-visible WARN arr-fault line. Serial (swaps
// slog.Default).
func TestLogPingClassifiesShutdownCancellation(t *testing.T) {
	rec := capture.Default(t)

	logPing("sonarr", fmt.Errorf("ping: %w", context.Canceled))

	records := rec.Records()
	if len(records) != 1 {
		t.Fatalf("captured %d records, want 1 (%v)", len(records), rec.Messages())
	}
	if records[0].Level != slog.LevelDebug {
		t.Errorf("level = %v, want DEBUG", records[0].Level)
	}
	if records[0].Message != "sonarr startup ping cancelled by shutdown" {
		t.Errorf("msg = %q, want the cancelled-by-shutdown line", records[0].Message)
	}
}

// TestDispatchRoutesReportMode pins the mode-routing switch: a valid
// report-mode config reaches runReport (proved hermetically by holding the
// report lock first, so runReport refuses with ErrReportRunning before any
// network I/O).
func TestDispatchRoutesReportMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "reports")
	release, err := audit.AcquireReportLock(dir)
	if err != nil {
		t.Fatalf("holding the report lock: %v", err)
	}
	defer release()

	cfg := &config.Config{
		RunMode:   config.RunModeReport,
		SonarrURL: "http://sonarr:8989", SonarrAPIKey: "k",
		ReportDir: dir,
	}
	err = dispatch(config.RunModeReport, cfg)
	if !errors.Is(err, audit.ErrReportRunning) {
		t.Fatalf("dispatch(report, valid config) = %v, want ErrReportRunning", err)
	}
}

// TestRunReportReleasesLockOnBuildFailure pins the deferred lock release: a
// failed buildScout (invalid sonarr URL, no network I/O) must not leak the
// report lock, or every subsequent report would refuse with ErrReportRunning
// until restart.
func TestRunReportReleasesLockOnBuildFailure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "reports")
	cfg := &config.Config{ReportDir: dir, SonarrURL: "not-a-url", SonarrAPIKey: "k"}

	err := runReport(cfg)
	if err == nil {
		t.Fatal("runReport(invalid sonarr URL) = nil, want a build error")
	}
	if errors.Is(err, audit.ErrReportRunning) {
		t.Fatalf("err = %v, want a build error, not a lock refusal", err)
	}

	release, err := audit.AcquireReportLock(dir)
	if err != nil {
		t.Fatalf("report lock still held after the failed run: %v", err)
	}
	release()
}

// TestBuildIndexer pins the Torznab feed server wiring hermetically: a
// configured feed builds a non-nil server (warm-loading the absent feed
// snapshot is the documented fresh-install no-op) and cleanup is callable.
func TestBuildIndexer(t *testing.T) {
	cfg := &config.Config{
		IndexerNyaaTorznabURL: "http://prowlarr:9696/22/api",
		IndexerAPIKey:         "feed-key",
	}
	bi := buildIndexer(cfg)
	if bi.indexer == nil {
		t.Fatal("indexer = nil, want a wired Torznab feed server")
	}
	bi.cleanup()
}

// TestStartIndexerUnconfiguredIsNoOp pins the socket-less contract: with no
// Prowlarr Torznab URL configured, startIndexer builds no indexer and starts
// no goroutine (no log record from an indexer Run/stop path), and the
// returned stop func returns immediately instead of waiting on a goroutine.
// Serial (capture swaps slog.Default).
func TestStartIndexerUnconfiguredIsNoOp(t *testing.T) {
	rec := capture.Default(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	stop := startIndexer(ctx, &config.Config{})
	stop()

	if msgs := rec.Messages(); len(msgs) != 0 {
		t.Errorf("startIndexer(unconfigured) logged %v, want no indexer activity", msgs)
	}
}

// TestRunSchedulerCoordinationFailure pins the daemon's infrastructure-failure
// contract: an unusable cycle lock (the lock path is a directory) means the
// tick could not run at all, which must be logged at ERROR ("cycle
// coordination failed; tick did not run") so the level=ERROR Loki alert fires
// - cycles have stopped - while the cycle never runs and the health marker is
// untouched. Serial (capture swaps slog.Default).
func TestRunSchedulerCoordinationFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := captureAndCancelOn(t, cancel, "cycle coordination failed; tick did not run")
	defer cancel()
	dir := t.TempDir()
	ex := testCycleExclusiveIn(t, ctx, dir)
	if err := os.Mkdir(filepath.Join(dir, scheduler.ExclusiveLockName), 0o755); err != nil {
		t.Fatal(err)
	}
	markerPath := seedSentinelMarker(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		runScheduler(ctx, time.Hour, ex, mustNotRunCycler{t: t}, health.NewMarker(markerPath))
	}()
	<-done

	if !rec.Contains("cycle coordination failed; tick did not run") {
		t.Errorf("missing the coordination-failure ERROR: %v", rec.Messages())
	}
	assertMarkerUntouched(t, markerPath)
}

// TestPollCycleQueueErrorAfterRun pins poll's exit-code contract when the run
// succeeded but the queue bookkeeping is broken (the queue file is a
// directory): the cycle this invocation paid for completed healthy, so
// pollCycle exits 0 and the marker records the outcome, with the coordination
// error demoted to the after-run WARN instead of failing the poll. Serial
// (capture swaps slog.Default).
func TestPollCycleQueueErrorAfterRun(t *testing.T) {
	rec := capture.Default(t)
	ctx := context.Background()
	dir := t.TempDir()
	ex := testCycleExclusiveIn(t, ctx, dir)
	if err := os.Mkdir(filepath.Join(dir, scheduler.ExclusiveQueueName), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := health.NewMarker(filepath.Join(t.TempDir(), ".healthy"))

	if err := pollCycle(ctx, ex, boolCycler(true), marker); err != nil {
		t.Fatalf("pollCycle(healthy run, broken queue file) = %v, want nil (the paid-for cycle succeeded)", err)
	}
	if !marker.Healthy() {
		t.Error("marker not healthy after the healthy cycle")
	}
	if !rec.Contains("cycle coordination error after run") {
		t.Errorf("missing the after-run coordination WARN: %v", rec.Messages())
	}
}

// TestRunSchedulerQueueErrorAfterRun pins the daemon's alert-hygiene twin of
// poll's after-run demotion: when the tick's cycle ran (the lock was free) but
// the queue bookkeeping is broken (the queue file is a directory), the
// coordination error is the after-run WARN - never the ERROR that fires the
// cycle-error Loki alert on every tick - and the marker records the cycle's
// health. Serial (capture swaps slog.Default).
func TestRunSchedulerQueueErrorAfterRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := captureAndCancelOn(t, cancel, "cycle coordination error after run")
	defer cancel()
	dir := t.TempDir()
	ex := testCycleExclusiveIn(t, ctx, dir)
	if err := os.Mkdir(filepath.Join(dir, scheduler.ExclusiveQueueName), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := health.NewMarker(filepath.Join(t.TempDir(), ".healthy"))

	done := make(chan struct{})
	go func() {
		defer close(done)
		runScheduler(ctx, time.Hour, ex, boolCycler(true), marker)
	}()
	<-done

	if !rec.Contains("cycle coordination error after run") {
		t.Errorf("missing the after-run coordination WARN: %v", rec.Messages())
	}
	if !marker.Healthy() {
		t.Error("marker not healthy after the healthy cycle")
	}
	for _, r := range rec.Records() {
		if r.Level == slog.LevelError {
			t.Errorf("unexpected ERROR record %q; a queue error after a ran tick must stay WARN", r.Message)
		}
	}
}

// TestNewArrClientsRadarrErrorClosesSonarr pins the partial-construction
// cleanup contract: when Radarr's constructor fails after Sonarr's succeeded,
// the error names the radarr client and no half-built client pair escapes
// (both returned clients are nil; the already-built Sonarr client is closed
// on this path rather than leaked).
func TestNewArrClientsRadarrErrorClosesSonarr(t *testing.T) {
	cfg := &config.Config{
		SonarrURL: "http://sonarr:8989", SonarrAPIKey: "k1",
		RadarrURL: "not-a-url", RadarrAPIKey: "k2",
	}
	s, r, err := newArrClients(cfg)
	if err == nil {
		t.Fatal("err = nil, want error for an invalid radarr URL beside a valid sonarr")
	}
	if !strings.Contains(err.Error(), "radarr client") {
		t.Errorf("err = %q, want it to name the radarr client", err)
	}
	if s != nil || r != nil {
		t.Errorf("clients = (%v, %v), want (nil, nil) on a constructor error", s, r)
	}
}

// TestWriteStarterConfigOwnerOnlyMode pins the documented owner-only mode of
// the generated starter config (starterFileMode): the file is where the
// operator may paste arr API keys and the AB passkey, so it must be created
// 0600. atomicfile applies the mode via Chmod (umask-independent), so the
// assertion is deterministic.
func TestWriteStarterConfigOwnerOnlyMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := writeStarterConfig(path); err != nil {
		t.Fatalf("writeStarterConfig(%q) = %v, want nil", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat starter: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("starter config mode = %o, want 0600 (owner-only: the file is where API keys get pasted)", got)
	}
}

// TestStartIndexerLogsRunErrorAndStops pins the configured half of the
// startIndexer contract that TestStartIndexerUnconfiguredIsNoOp cannot reach:
// with a Prowlarr Torznab URL configured, the feed goroutine is launched, a
// Run failure is logged as the component=indexer ERROR fault line (via
// logIndexerStop's non-shutdown branch), and the returned stop func waits for
// the goroutine instead of deadlocking or returning before the record is
// written. The Run failure used is indexer.Run's own fail-closed refusal on an
// empty feed_api_key, which returns before any port bind - so the test is
// hermetic and deterministic (the refusal precedes every context check, so the
// message is stable even if stop's cancel wins the race with the goroutine).
// Serial (capture swaps slog.Default).
func TestStartIndexerLogsRunErrorAndStops(t *testing.T) {
	rec := capture.Default(t)
	ctx := t.Context()

	cfg := &config.Config{IndexerNyaaTorznabURL: "http://prowlarr:9696/22/api"}
	stop := startIndexer(ctx, cfg)
	stop() // must wait for the goroutine's terminal log, then return

	if !rec.Contains("indexer feed stopped") {
		t.Fatalf("missing the indexer feed stopped ERROR line: %v", rec.Messages())
	}
	for _, r := range rec.Records() {
		if r.Message == "indexer feed stopped" && r.Level != slog.LevelError {
			t.Errorf("level = %v, want ERROR (a Run failure outside shutdown is a fault)", r.Level)
		}
	}
}

// TestPollInterruptedClassifiesNonCanceledCause pins poll's interruption
// classification against the production signal path: Go 1.26's
// signal.NotifyContext cancels with a bare signalError cause that does NOT
// satisfy errors.Is(_, context.Canceled), so pollInterrupted must wrap the
// stable ctx.Err() for main's routine-shutdown WARN classification while the
// cause stays errors.Is-able for diagnostics. Mirrored here with
// context.WithCancelCause and a non-Canceled cause.
func TestPollInterruptedClassifiesNonCanceledCause(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("terminated signal received")
	cancel(cause)

	err := pollInterrupted(ctx)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled regardless of the cancellation cause", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("err = %v, want the cancellation cause to stay errors.Is-able", err)
	}
}

// TestPollCycleMarkerWriteFailure pins pollOnce's marker-write failure
// branch: the marker directory is present at construction (so the marker
// does not enter its degraded no-op mode) and is then replaced by a regular
// file, so SetChecked reaches its transient-error return for every UID
// (root-safe, unlike a read-only-dir chmod) and a healthy cycle still exits
// non-zero with the record-poll-health error - the external scheduler must
// see the fail rather than trusting an unrecorded outcome.
func TestPollCycleMarkerWriteFailure(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "marker-dir")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := health.NewMarker(filepath.Join(dir, ".healthy"))
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("blocker"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := pollCycle(ctx, testCycleExclusive(t, ctx), boolCycler(true), marker)

	if err == nil {
		t.Fatal("pollCycle(blocked marker path) = nil, want the record-poll-health error (exit 1)")
	}
	if !strings.Contains(err.Error(), "record poll health") {
		t.Errorf("err = %q, want it wrapped as record poll health", err)
	}
}

// TestRunPollBuildFailure pins runPoll's pre-cycle failure contract: a build
// failure with no shutdown signal propagates as the ordinary error (exit 1)
// and must never read as an interruption (an errors.Is(context.Canceled)
// result would make main demote a genuine misconfiguration to the
// routine-shutdown WARN, keeping it off the level=ERROR cycle-error alert).
// Hermetic: the invalid sonarr URL fails newArrClients before any network
// I/O, cycle-lock creation, or health-marker write.
func TestRunPollBuildFailure(t *testing.T) {
	err := runPoll(&config.Config{SonarrURL: "not-a-url", SonarrAPIKey: "k"})
	if err == nil {
		t.Fatal("runPoll(invalid sonarr URL) = nil, want a build error (exit 1)")
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, must not read as an interruption (main would demote the fault to WARN)", err)
	}
	if !strings.Contains(err.Error(), "sonarr client") {
		t.Errorf("err = %q, want the sonarr client build failure", err)
	}
}

// TestPollCycleQueueErrorThenCancelled pins the cancelled-with-coordination-
// error diagnostics leg of poll's uniform interruption contract: the tick's
// own run completes (the lock was free) but the queue bookkeeping is broken
// (the queue file is a directory) AND shutdown lands during the run. The
// coordination error must surface as the after-run WARN inside the cancelled
// path, and the interruption must still win over the own-run result: pollCycle
// returns the error wrapping context.Canceled (main classifies it WARN, exit
// non-zero) with the marker untouched. Serial (capture swaps slog.Default).
func TestPollCycleQueueErrorThenCancelled(t *testing.T) {
	rec := capture.Default(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	ex := testCycleExclusiveIn(t, ctx, dir)
	if err := os.Mkdir(filepath.Join(dir, scheduler.ExclusiveQueueName), 0o755); err != nil {
		t.Fatal(err)
	}
	path := seedSentinelMarker(t)

	err := pollCycle(ctx, ex, cancelCycler{cancel: cancel, healthy: true}, health.NewMarker(path))

	if err == nil {
		t.Fatal("pollCycle(cancelled run, broken queue bookkeeping) = nil, want the interruption error (exit 1)")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled (the interruption wins over the own-run result)", err)
	}
	const msg = "cycle coordination error after run"
	if got := rec.CountLevel(slog.LevelWarn, msg); got != 1 {
		t.Errorf("after-run coordination WARN count = %d, want 1: %v", got, rec.Messages())
	}
	if got := rec.CountLevel(slog.LevelError, msg); got != 0 {
		t.Errorf("after-run coordination ERROR count = %d, want 0: %v", got, rec.Messages())
	}
	assertMarkerUntouched(t, path)
}

// TestRunIndexerPanicShield pins the daemon's feed crash shield (the runCycle
// shield's twin): a panicking feed goroutine is recovered - it must not crash
// the long-lived daemon - logged as the component=indexer panic ERROR, its
// clients are still released (cleanup runs on the panic path), and done is
// closed so startIndexer's stop func cannot deadlock. Serial (capture swaps
// slog.Default).
func TestRunIndexerPanicShield(t *testing.T) {
	rec := capture.Default(t)
	done := make(chan struct{})
	cleaned := false

	runIndexer(context.Background(), done,
		func(context.Context) error { panic("boom") },
		func() { cleaned = true },
		slog.Default().With("component", "indexer"))
	<-done

	const msg = "indexer feed panicked"
	if got := rec.CountLevel(slog.LevelError, msg); got != 1 {
		t.Errorf("panic-shield ERROR count = %d, want 1: %v", got, rec.Messages())
	}
	if got := rec.CountLevel(slog.LevelWarn, msg); got != 0 {
		t.Errorf("panic-shield WARN count = %d, want 0: %v", got, rec.Messages())
	}
	if !rec.HasAttr(msg, "component", "indexer") {
		t.Errorf("panic-shield record missing component=indexer: %v", rec.Records())
	}
	if !cleaned {
		t.Error("cleanup not released on the panic path (the Prowlarr transport would leak)")
	}
}

// TestWarnCoordinationError pins the outcome-to-diagnostic mapping of the
// coordination-error WARN lines (operator-facing Loki diagnostics): recorded
// demand (Queued/Discarded) logs the demand-stands line, a completed run
// (Ran/RanQueued/Skipped) logs the after-run line, and every other outcome -
// reachable only from pollCycle's shutdown branch - logs the during-shutdown
// line. All three are WARN, never the ERROR that fires the cycle-error Loki
// alert. Serial (capture swaps slog.Default).
func TestWarnCoordinationError(t *testing.T) {
	tests := []struct {
		name    string
		outcome scheduler.Outcome
		wantMsg string
	}{
		{"queued demand stands", scheduler.OutcomeQueued, "cycle coordination error after queueing; demand stands"},
		{"discarded demand stands", scheduler.OutcomeDiscarded, "cycle coordination error after queueing; demand stands"},
		{"ran is after-run", scheduler.OutcomeRan, "cycle coordination error after run"},
		{"ran-plus-queued is after-run", scheduler.OutcomeRanQueued, "cycle coordination error after run"},
		{"skipped is after-run", scheduler.OutcomeSkipped, "cycle coordination error after run"},
		{"none is the shutdown diagnostic", scheduler.OutcomeNone, "cycle coordination failed during shutdown"},
		{"gated is the shutdown diagnostic", scheduler.OutcomeGated, "cycle coordination failed during shutdown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := capture.Default(t)

			warnCoordinationError(tt.outcome, errors.New("queue file unusable"))

			records := rec.Records()
			if len(records) != 1 {
				t.Fatalf("captured %d records, want 1 (%v)", len(records), rec.Messages())
			}
			if records[0].Message != tt.wantMsg {
				t.Errorf("msg = %q, want %q", records[0].Message, tt.wantMsg)
			}
			if records[0].Level != slog.LevelWarn {
				t.Errorf("level = %v, want WARN (a stands-anyway diagnostic must not fire the cycle-error alert)", records[0].Level)
			}
		})
	}
}

// TestPollNonRunResult pins the non-run outcome mapping of poll's exit
// contract at the helper seam: Queued/Discarded are success (exit 0) even
// when the coordination bookkeeping errored (the error is demoted to the
// demand-stands WARN, and each outcome logs its own coalescing Info line);
// Gated applies the uniform interruption contract (an error wrapping
// context.Canceled, so main classifies it WARN and exits non-zero); and every
// run-shaped outcome falls through unhandled to pollCycle's ran/own
// accounting. Serial (capture swaps slog.Default).
func TestPollNonRunResult(t *testing.T) {
	t.Run("queued with a coordination error is still success and logs demand-stands", func(t *testing.T) {
		rec := capture.Default(t)

		handled, err := pollNonRunResult(context.Background(), scheduler.OutcomeQueued, errors.New("queue bookkeeping broken"))

		if !handled {
			t.Fatal("handled = false, want true (queued ends the poll)")
		}
		if err != nil {
			t.Fatalf("err = %v, want nil (recorded demand is success, exit 0)", err)
		}
		if got := rec.CountLevel(slog.LevelWarn, "cycle coordination error after queueing; demand stands"); got != 1 {
			t.Errorf("demand-stands WARN count = %d, want 1: %v", got, rec.Messages())
		}
		if !rec.Contains("compare cycle already in flight; demand queued for the active runner") {
			t.Errorf("missing the queued coalescing line: %v", rec.Messages())
		}
	})
	t.Run("discarded logs the already-covered message", func(t *testing.T) {
		rec := capture.Default(t)

		handled, err := pollNonRunResult(context.Background(), scheduler.OutcomeDiscarded, nil)

		if !handled || err != nil {
			t.Fatalf("pollNonRunResult(discarded) = (handled=%v, err=%v), want (true, nil)", handled, err)
		}
		if !rec.Contains("compare cycle already in flight; demand already covered by the queued rerun") {
			t.Errorf("missing the discarded coalescing line: %v", rec.Messages())
		}
	})
	t.Run("gated applies the uniform interruption contract", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		handled, err := pollNonRunResult(ctx, scheduler.OutcomeGated, nil)

		if !handled {
			t.Fatal("handled = false, want true (a gated run ends the poll)")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want it to wrap context.Canceled (main classifies the interruption WARN, exit non-zero)", err)
		}
	})
	t.Run("run-shaped outcomes fall through unhandled", func(t *testing.T) {
		for _, outcome := range []scheduler.Outcome{scheduler.OutcomeNone, scheduler.OutcomeRan, scheduler.OutcomeRanQueued, scheduler.OutcomeSkipped} {
			if handled, err := pollNonRunResult(context.Background(), outcome, nil); handled || err != nil {
				t.Errorf("pollNonRunResult(%v) = (handled=%v, err=%v), want (false, nil): must fall through to the ran/own accounting", outcome, handled, err)
			}
		}
	})
}
