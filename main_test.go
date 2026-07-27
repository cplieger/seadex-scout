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
	"testing"
	"time"

	"github.com/cplieger/arrapi"
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
	rec := capture.Default(t)

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

	if rec.Count("configuration loaded") == 0 {
		t.Fatal("logConfig emitted nothing, want a configuration line")
	}
	for _, secret := range []string{"sekrit-1s", "sekrit-2r", "sekrit-3p", "sekrit-4a"} {
		// key "" scans every top-level attr of the record.
		if rec.AttrContains("configuration loaded", "", secret) {
			t.Errorf("startup config log leaks secret %q: %v", secret, rec.Records())
		}
	}
	if _, ok := rec.AttrValue("configuration loaded", "sonarr_enabled"); !ok {
		t.Errorf("startup config log missing sonarr_enabled: %v", rec.Records())
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

// TestLogConfigMasksInvalidRunMode pins the invalid-mode redaction contract
// documented in logConfig: an unrecognized run_mode (which may be an expanded
// ${VAR} secret placed by a config typo) is logged as the fixed marker
// "invalid", never the raw value. Serial (swaps slog.Default).
func TestLogConfigMasksInvalidRunMode(t *testing.T) {
	rec := capture.Default(t)

	cfg := &config.Config{RunMode: "leaked-secret-value-9"}
	logConfig(cfg)

	if rec.AttrContains("configuration loaded", "", "leaked-secret-value-9") {
		t.Errorf("startup config log leaks the raw run_mode value: %v", rec.Records())
	}
	if !rec.HasAttr("configuration loaded", "run_mode", unknownModeMarker) {
		t.Errorf("run_mode not logged as the fixed marker %q: %v", unknownModeMarker, rec.Records())
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

	rec := capture.Default(t)

	// The exact failure line main emits when dispatch rejects the mode.
	slog.Error("seadex-scout failed", "mode", loggableMode(secret), "error", errors.New("invalid configuration"))

	if rec.AttrContains("seadex-scout failed", "", secret) {
		t.Errorf("dispatch-failure log leaks the raw mode value: %v", rec.Records())
	}
	if !rec.HasAttr("seadex-scout failed", "mode", unknownModeMarker) {
		t.Errorf("mode not logged as the fixed marker %q: %v", unknownModeMarker, rec.Records())
	}
}

// TestLogConfigExternalPollInterval pins the resident-idle rendering: with
// poll_interval off (PollExternal), the startup line reports "external", not a
// zero duration. Serial (swaps slog.Default).
func TestLogConfigExternalPollInterval(t *testing.T) {
	rec := capture.Default(t)

	cfg := &config.Config{PollExternal: true, RunMode: config.RunModeDaemon}
	logConfig(cfg)

	// The library's attr assertion compares the RENDERED value, so it pins the
	// attribute itself rather than a substring of the serialized JSON - a
	// coincidental match elsewhere in the line cannot satisfy it (l-f34).
	if !rec.HasAttr("configuration loaded", "poll_interval", "external") {
		t.Errorf("poll_interval not rendered as external: %v", rec.Messages())
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

// TestRunIndexerPanicShield pins the daemon's feed crash shield (the twin of
// internal/cycle's compare-cycle panic shield): a panicking feed goroutine is recovered - it must not crash
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

// TestLogIndexerStopClassifiesCauseOnlyCancellation pins the sibling terminal
// boundary: net.ListenConfig.Listen can report a cancelled bind as the
// cancellation CAUSE rather than context.Canceled, and that stop is still a
// routine shutdown - it must log the WARN, never the ERROR fault line that
// fires the cycle-error alert on a redeploy. Serial (swaps slog.Default).
func TestLogIndexerStopClassifiesCauseOnlyCancellation(t *testing.T) {
	cause := errors.New("terminated signal received") // deliberately NOT wrapping context.Canceled
	ctx, cancelCause := context.WithCancelCause(context.Background())
	cancelCause(cause)
	rec := capture.Default(t)

	logIndexerStop(ctx, slog.Default().With("component", "indexer"), fmt.Errorf("listen: %w", cause))

	records := rec.Records()
	if len(records) != 1 {
		t.Fatalf("captured %d records, want 1 (%v)", len(records), rec.Messages())
	}
	if records[0].Message != "indexer feed stopped during shutdown" {
		t.Errorf("msg = %q, want the routine shutdown WARN", records[0].Message)
	}
	if records[0].Level != slog.LevelWarn {
		t.Errorf("level = %v, want WARN (a cause-only cancellation is not a fault)", records[0].Level)
	}
}

// TestDispatchOutcome pins the two operator-visible contracts main derives from
// a dispatch error: the slog level the cycle-error Loki alert keys on
// (level=ERROR) and the exit code a scheduler reads. The self-heal rule decides
// both - a designed outcome or a routine shutdown is a WARN, and only a refused
// concurrent report (which owes nothing, because the run holding the lock is
// producing the report) exits 0.
func TestDispatchOutcome(t *testing.T) {
	for name, tc := range map[string]struct {
		err       error
		wantLevel slog.Level
		wantMsg   string
		wantExit  int
	}{
		"refused concurrent report": {
			err:       fmt.Errorf("acquire report lock: %w", audit.ErrReportRunning),
			wantLevel: slog.LevelWarn,
			wantMsg:   "report skipped; another report is already running",
			wantExit:  0,
		},
		"shutdown": {
			err:       fmt.Errorf("cycle: %w", context.Canceled),
			wantLevel: slog.LevelWarn,
			wantMsg:   "seadex-scout interrupted by shutdown",
			wantExit:  1,
		},
		"operation timeout is a genuine fault": {
			err:       fmt.Errorf("walk: %w", context.DeadlineExceeded),
			wantLevel: slog.LevelError,
			wantMsg:   "seadex-scout failed",
			wantExit:  1,
		},
		"plain fault": {
			err:       errors.New("sonarr unreachable"),
			wantLevel: slog.LevelError,
			wantMsg:   "seadex-scout failed",
			wantExit:  1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			level, msg, exit := dispatchOutcome(tc.err)
			if level != tc.wantLevel {
				t.Errorf("level = %v, want %v", level, tc.wantLevel)
			}
			if msg != tc.wantMsg {
				t.Errorf("msg = %q, want %q", msg, tc.wantMsg)
			}
			if exit != tc.wantExit {
				t.Errorf("exit = %d, want %d", exit, tc.wantExit)
			}
		})
	}
}

func TestReportWriteContext(t *testing.T) {
	t.Run("a live context passes through unchanged", func(t *testing.T) {
		ctx := context.Background()
		got, cancel := reportWriteContext(ctx)
		defer cancel()
		if got != ctx {
			t.Errorf("reportWriteContext(live) = %v, want the caller's own context", got)
		}
		if got.Err() != nil {
			t.Errorf("Err() = %v, want nil", got.Err())
		}
	})
	t.Run("a cancelled context is detached, bounded, and keeps its values", func(t *testing.T) {
		parent := context.WithValue(context.Background(), reportCtxTestKey{}, "report")
		ctx, cancelParent := context.WithCancel(parent)
		cancelParent()

		got, cancel := reportWriteContext(ctx)
		defer cancel()
		if got.Err() != nil {
			t.Fatalf("Err() = %v, want nil (a shutdown must not cost the ~25m report artifact)", got.Err())
		}
		deadline, ok := got.Deadline()
		if !ok {
			t.Fatal("detached write context has no deadline, want one bounded by reportWriteGrace")
		}
		if d := time.Until(deadline); d <= 0 || d > reportWriteGrace {
			t.Errorf("deadline in %v, want within (0, %v]", d, reportWriteGrace)
		}
		if v, _ := got.Value(reportCtxTestKey{}).(string); v != "report" {
			t.Errorf("Value = %q, want %q (WithoutCancel keeps values)", v, "report")
		}
		cancel()
		if got.Err() == nil {
			t.Error("cancel() left the detached context live; the caller's defer must release it")
		}
	})
}

// reportCtxTestKey is the private key TestReportWriteContext threads through
// the detached write context to prove values survive context.WithoutCancel.
type reportCtxTestKey struct{}
