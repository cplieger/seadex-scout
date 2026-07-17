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
// dropped field in the wiring cannot silently invert a content filter.
func TestFilterOptions(t *testing.T) {
	cfg := &config.Config{ExcludeRemux: true, RequireDualAudio: false, AnimeBytes: true}
	got := filterOptions(cfg)
	if !got.ExcludeRemux {
		t.Error("ExcludeRemux = false, want true")
	}
	if got.RequireDualAudio {
		t.Error("RequireDualAudio = true, want false")
	}
	if !got.AnimeBytes {
		t.Error("AnimeBytes = false, want true")
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
		b, err := buildScout(context.Background(), &config.Config{})
		if err != nil {
			t.Fatalf("buildScout(zero config) = %v, want nil", err)
		}
		if b.scout == nil {
			t.Fatal("scout = nil, want a wired scout")
		}
		b.cleanup()
	})
	t.Run("invalid sonarr URL propagates", func(t *testing.T) {
		cfg := &config.Config{SonarrURL: "not-a-url", SonarrAPIKey: "k"}
		if _, err := buildScout(context.Background(), cfg); err == nil {
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

// testCycleExclusive builds a cycle coalescer for tests in a temp dir, wired
// exactly like production (newCycleExclusive, including the shutdown gate on
// ctx), so the tests exercise the real gate and lock wiring.
func testCycleExclusive(t *testing.T, ctx context.Context) *scheduler.Exclusive {
	t.Helper()
	ex, err := newCycleExclusive(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("newCycleExclusive: %v", err)
	}
	return ex
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
		cycler    func(cancel context.CancelFunc) cycler
		name      string
		preCancel bool
	}{
		{func(context.CancelFunc) cycler { return boolCycler(false) }, "pre-cycle cancellation", true},
		{func(cancel context.CancelFunc) cycler { return cancelCycler{cancel: cancel} }, "mid-cycle cancellation", false},
		{func(cancel context.CancelFunc) cycler { return cancelCycler{cancel: cancel, healthy: true} }, "post-cycle cancellation after a healthy cycle", false},
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
			path := filepath.Join(t.TempDir(), ".healthy")
			if err := os.WriteFile(path, []byte("sentinel-untouched"), 0o600); err != nil {
				t.Fatal(err)
			}

			err := pollCycle(ctx, ex, tt.cycler(cancel), health.NewMarker(path))

			if err == nil {
				t.Fatal("pollCycle = nil, want the interruption error (exit 1)")
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("err = %v, want it to wrap context.Canceled (main classifies the interruption WARN, not ERROR)", err)
			}
			got, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatalf("marker file after interruption: %v", readErr)
			}
			if string(got) != "sentinel-untouched" {
				t.Errorf("marker content = %q, want the pre-existing state untouched", got)
			}
		})
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
func holdCycleLock(t *testing.T, dir string) *scheduler.Lock {
	t.Helper()
	holder, ok, err := scheduler.TryLock(filepath.Join(dir, scheduler.ExclusiveLockName))
	if err != nil || !ok {
		t.Fatalf("seed TryLock = (ok=%v, err=%v), want (true, nil)", ok, err)
	}
	t.Cleanup(holder.Unlock)
	return holder
}

// TestRunSchedulerSkipsBusyTick pins the daemon's skip mode: a tick arriving
// while another process holds the cycle lock is skipped - the cycle never
// runs, the health marker is untouched, and the library's pinned busy WARN is
// emitted. Serial (capture swaps slog.Default).
func TestRunSchedulerSkipsBusyTick(t *testing.T) {
	rec := capture.Default(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	ex, err := newCycleExclusive(ctx, dir)
	if err != nil {
		t.Fatalf("newCycleExclusive: %v", err)
	}
	holdCycleLock(t, dir)

	markerPath := filepath.Join(t.TempDir(), ".healthy")
	if err := os.WriteFile(markerPath, []byte("sentinel-untouched"), 0o600); err != nil {
		t.Fatal(err)
	}

	// FireOnStart executes the first tick immediately; it skips (the lock is
	// busy), then the loop waits out the interval until cancelled.
	done := make(chan struct{})
	go func() {
		defer close(done)
		runScheduler(ctx, time.Hour, ex, mustNotRunCycler{t: t}, health.NewMarker(markerPath))
	}()
	waitFor(t, func() bool { return rec.Contains("cycle lock busy; skipping tick") })
	cancel()
	<-done

	got, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("marker file after skipped tick: %v", err)
	}
	if string(got) != "sentinel-untouched" {
		t.Errorf("marker content = %q, want untouched on a skipped tick", got)
	}
}

// waitFor polls cond until it holds or a deadline expires.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached within the deadline")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestPollCycleQueuedWhenBusy pins poll's queue mode against a busy cycle
// lock: the request is queued for the active runner, the cycle does NOT run in
// this process, the marker stays untouched, and pollCycle exits 0 (nil) with
// the coalescing log lines. Serial (capture swaps slog.Default).
func TestPollCycleQueuedWhenBusy(t *testing.T) {
	rec := capture.Default(t)
	ctx := context.Background()
	dir := t.TempDir()
	ex, err := newCycleExclusive(ctx, dir)
	if err != nil {
		t.Fatalf("newCycleExclusive: %v", err)
	}
	holdCycleLock(t, dir)

	markerPath := filepath.Join(t.TempDir(), ".healthy")
	if err := os.WriteFile(markerPath, []byte("sentinel-untouched"), 0o600); err != nil {
		t.Fatal(err)
	}

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
	got, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("marker file after queued poll: %v", err)
	}
	if string(got) != "sentinel-untouched" {
		t.Errorf("marker content = %q, want untouched on a queued poll", got)
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
	ex, err := newCycleExclusive(ctx, dir)
	if err != nil {
		t.Fatalf("newCycleExclusive: %v", err)
	}
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
	ex, err := newCycleExclusive(ctx, dir)
	if err != nil {
		t.Fatalf("newCycleExclusive: %v", err)
	}
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

// TestPollCycleCoordinationFailure pins the infrastructure-failure path:
// an unusable cycle lock (the lock path is a directory) means nothing ran and
// no demand was recorded, so pollCycle returns the error (exit 1) and never
// reads as an interruption.
func TestPollCycleCoordinationFailure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	ex, err := newCycleExclusive(ctx, dir)
	if err != nil {
		t.Fatalf("newCycleExclusive: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, scheduler.ExclusiveLockName), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := health.NewMarker(filepath.Join(t.TempDir(), ".healthy"))

	err = pollCycle(ctx, ex, mustNotRunCycler{t: t}, marker)

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

			logIndexerStop(tt.ctx, tt.err)

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
