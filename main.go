// Package main is seadex-scout: a watcher that compares a Sonarr/Radarr anime
// library against SeaDex (releases.moe) and emits a structured slog line
// whenever SeaDex recommends a better release than the one on disk. It never
// downloads or touches a torrent client; it tells the operator what to go get.
//
// main.go is the composition root: it installs logging, handles the distroless
// `health` subcommand, loads and validates the YAML config (CONFIG_PATH,
// default /config/config.yaml; a starter is written on first boot), builds the
// scout (build.go), and runs the daemon. All logic lives in internal/*.
//
// Two run modes: the daemon (no argument, or mode: daemon) runs a compare cycle
// on start and every poll_interval - or sits resident-idle when poll_interval is
// off/disabled/0, with an external scheduler driving cycles via the `poll`
// subcommand - and, when a Prowlarr Torznab URL is configured, also serves the
// Torznab feed of SeaDex releases (both features in one process, no toggle); the
// one-shot report (the `report` subcommand or mode: report) writes a
// SeaDex-alignment report and exits. The `poll` subcommand runs one compare
// cycle and the `health` subcommand backs the Docker healthcheck; both run via
// `docker exec <container> /seadex-scout <cmd>` while the daemon idles.
package main

import (
	"cmp"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/health"
	"github.com/cplieger/scheduler"
	"github.com/cplieger/seadex-scout/internal/audit"
	"github.com/cplieger/seadex-scout/internal/config"
)

// exampleConfig is the starter written to CONFIG_PATH on first boot; it is also
// shipped as config.example.yaml in the repo root.
//
//go:embed config.example.yaml
var exampleConfig []byte

// starterFileMode / starterDirMode are applied to a generated starter config.
// The config file is where the operator may paste arr API keys and the AB
// passkey (see README), and an in-place edit keeps the creation mode, so it is
// owner-only like the indexer feed snapshot (internal/indexer/writer.go).
const (
	starterFileMode = 0o600
	starterDirMode  = 0o755
)

// modePoll is the subcommand-only mode: run one compare cycle for an external
// scheduler (paired with poll_interval: off). Not a valid config `mode`.
const modePoll = "poll"

// validArgsHint lists the accepted invocations; shared by the two
// invalid-invocation error messages so they cannot drift when a
// subcommand is added or removed.
const validArgsHint = "(valid: health, daemon, report, poll, or no argument)"

func main() {
	installLogger()

	// Reject malformed invocations with trailing arguments (e.g. `poll typo`,
	// `health typo`) before the health fast path, so a typo can never run a
	// real poll or report healthy. Exit 2 = invalid invocation, matching the
	// resolveMode contract below.
	args := os.Args[1:]
	if len(args) > 1 {
		slog.Error("invalid invocation", "error",
			fmt.Errorf("too many arguments %q %s", args, validArgsHint))
		os.Exit(2)
	}

	// The health subcommand backs the Docker healthcheck and must not require
	// configuration, so it is handled before config load.
	if len(args) == 1 && args[0] == "health" {
		health.RunProbe(health.DefaultPath)
		// health.RunProbe terminates via os.Exit(0/1); if it ever returns
		// (a contract change in the separately versioned health dependency),
		// fail closed: report unhealthy rather than a silently-green probe.
		os.Exit(1)
	}

	configPath := cmp.Or(strings.TrimSpace(os.Getenv("CONFIG_PATH")), config.DefaultConfigPath)
	//nolint:gosec // G304: CONFIG_PATH is an operator-supplied path, not user input
	if _, err := os.Stat(configPath); errors.Is(err, fs.ErrNotExist) {
		if werr := writeStarterConfig(configPath); werr != nil {
			slog.Error("no config found and could not write a starter", "path", configPath, "error", werr)
			os.Exit(1)
		}
		slog.Warn("no config found; wrote a starter config - set your Sonarr/Radarr url + api_key and restart", "path", configPath)
		os.Exit(1)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		slog.Error("failed to load config", "path", configPath, "error", err)
		os.Exit(1)
	}
	configureLogger(cfg.LogLevel, cfg.LogFormat)
	logConfig(&cfg)

	mode, err := resolveMode(args, &cfg)
	if err != nil {
		slog.Error("invalid invocation", "error", err)
		os.Exit(2)
	}

	if err := dispatch(mode, &cfg); err != nil {
		if errors.Is(err, context.Canceled) {
			// A shutdown signal interrupted the run (signal.NotifyContext
			// cancels with context.Canceled): routine, not a fault, so keep it
			// off the level=ERROR cycle-error alert. A DeadlineExceeded is a
			// genuine operation timeout and falls through to ERROR.
			slog.Warn("seadex-scout interrupted by shutdown", "mode", mode, "error", err)
		} else {
			slog.Error("seadex-scout failed", "mode", mode, "error", err)
		}
		os.Exit(1)
	}
}

// dispatch validates the config, then runs the resolved mode. Each run body
// lives in a helper so its defers (signal stop, health-marker cleanup, client
// cleanup) always execute; os.Exit stays in main so it never skips a pending
// defer.
func dispatch(mode string, cfg *config.Config) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	switch mode {
	case config.RunModeReport:
		return runReport(cfg)
	case modePoll:
		return runPoll(cfg)
	default:
		return run(cfg)
	}
}

// writeStarterConfig writes the embedded example config to path, creating the
// parent directory, so a fresh deployment gets an editable starter.
func writeStarterConfig(path string) error {
	// Written atomically (temp file + rename, parent dir created via
	// WithMkdirMode) through atomicfile, matching the report and state writers,
	// so a crash or power loss mid-write cannot leave a truncated starter config.
	// CONFIG_PATH is an operator-supplied path, not user input.
	if _, err := atomicfile.WriteFile(context.Background(), path, exampleConfig,
		atomicfile.WithMkdirMode(starterDirMode),
		atomicfile.WithMode(starterFileMode)); err != nil {
		return fmt.Errorf("write starter config: %w", err)
	}
	return nil
}

// resolveMode decides the run mode from the optional subcommand
// (daemon | report | poll) or, with no subcommand, the config's `mode`
// (daemon | report). `poll` runs one compare cycle for an external scheduler
// (used with poll_interval: off). The health subcommand is handled earlier.
// main rejects multi-argument invocations before the health fast path, so
// args holds at most one subcommand here.
func resolveMode(args []string, cfg *config.Config) (mode string, err error) {
	if len(args) == 0 {
		return cfg.RunMode, nil
	}
	switch args[0] {
	case config.RunModeDaemon, config.RunModeReport, modePoll:
		return args[0], nil
	default:
		return "", fmt.Errorf("unknown subcommand %q %s", args[0], validArgsHint)
	}
}

// runReport runs the one-shot audit: build components, generate the report,
// emit it to slog, and write the JSON + Markdown files. It never writes state,
// so a one-shot report cannot clobber a running daemon's cache.
func runReport(cfg *config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The whole generate+write is serialized on an exclusive flock in the
	// report dir: two report runs finishing within the same UTC second would
	// target the same report-<timestamp>.{md,json} pair, so a concurrent
	// second run refuses with ErrReportRunning (exit 1) instead of racing.
	// The report is read-only on state, so the lock guards only the report dir.
	release, err := audit.AcquireReportLock(cfg.ReportDir)
	if err != nil {
		return err
	}
	defer release()

	b, err := buildScout(ctx, cfg)
	if err != nil {
		return err
	}
	defer b.cleanup()

	rep, err := b.scout.Report(ctx)
	if err != nil {
		return err
	}
	rep.Log(slog.Default())
	return rep.WriteFiles(ctx, cfg.ReportDir, slog.Default())
}

// runPoll runs one compare cycle for an external scheduler (poll_interval: off).
// It updates the health marker to the cycle's outcome, leaving it in place (no
// Cleanup) so the container healthcheck reads the last poll, and exits non-zero
// on an unhealthy cycle so the scheduler (Ofelia job-exec, cron) sees the fail.
//
// Interruption contract (uniform across every phase of poll): a shutdown
// cancellation observed at any point - during startup, mid-cycle, or after the
// cycle body (including the state save) - exits non-zero with the shared health
// marker untouched, classified as a routine shutdown (WARN, not the level=ERROR
// cycle-error alert) via the context.Canceled wrap main inspects.
func runPoll(cfg *config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	b, err := buildScout(ctx, cfg)
	if err != nil {
		if ctx.Err() != nil {
			// Shutdown cancelled startup (pre-cycle phase of the uniform
			// interruption contract): wrap the cancellation cause so main
			// classifies it WARN, and never touch the marker.
			return fmt.Errorf("poll interrupted; health marker left unchanged: %w", context.Cause(ctx))
		}
		return err
	}
	defer b.cleanup()

	return pollCycle(ctx, b.scout, health.NewMarker(health.DefaultPath))
}

// pollCycle runs poll's one cycle and applies the uniform interruption
// contract documented on runPoll: a cancellation observed at any point - even
// when the cycle still managed to complete healthy (e.g. the signal landed
// during the end-of-cycle save) - exits non-zero and leaves the shared marker
// at the daemon's last real state, since an interrupted run's outcome is not a
// trustworthy health verdict. Wrapping the cancellation cause lets main
// classify the interruption as a routine shutdown (WARN, not the level=ERROR
// cycle-error alert).
func pollCycle(ctx context.Context, sc cycler, marker *health.Marker) error {
	healthy := runCycle(ctx, sc)
	if ctx.Err() != nil {
		return fmt.Errorf("poll interrupted; health marker left unchanged: %w", context.Cause(ctx))
	}
	marker.Set(healthy)
	if !healthy {
		return errors.New("compare cycle failed (library ingest)")
	}
	return nil
}

// run wires up the daemon and polls until the context is cancelled. It returns
// an error only on a startup failure.
func run(cfg *config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The process-level completion record is registered before the cleanup
	// defers below, so (defers run LIFO) it logs only after the indexer's
	// graceful drain, the client cleanup, and the health-marker removal have
	// all finished - "shutdown complete" then truthfully reports shutdown
	// progress to Loki. normalShutdown guards it so a startup-error return
	// does not log a successful shutdown.
	normalShutdown := false
	defer func() {
		if normalShutdown {
			slog.Info("shutdown complete", "cause", context.Cause(ctx))
		}
	}()

	marker := health.NewMarker(health.DefaultPath)
	marker.Set(false)
	defer marker.Cleanup()

	b, err := buildScout(ctx, cfg)
	if err != nil {
		return err
	}
	defer b.cleanup()

	// The Torznab feed runs alongside the compare loop in the same process, so
	// one daemon serves both features with no on/off knob. It starts only when a
	// Prowlarr Torznab URL is configured (else the daemon binds no HTTP port),
	// owns no health marker (the compare loop does), and its failure is logged
	// without affecting the compare loop. stopIndexer waits for its graceful
	// shutdown before releasing its clients.
	stopIndexer := startIndexer(ctx, cfg)
	defer stopIndexer()

	// Resident-idle (poll_interval: off): no internal timer; healthy on boot and
	// cycles are triggered out-of-band via the `poll` subcommand (e.g. an Ofelia
	// job-exec). Matches the fleet scheduler shape (github-scout, rsync, fclones).
	if cfg.PollExternal {
		marker.Set(true)
		slog.Info("seadex-scout started (resident-idle; trigger a cycle with the `poll` subcommand)",
			"indexer", cfg.IndexerConfigured())
		<-ctx.Done()
		normalShutdown = true
		return nil
	}

	// Built-in scheduler. Healthy on boot: the first cycle runs as the loop's
	// first iteration (immediately), so a slow first cycle never gates startup
	// health. The marker thereafter reflects each cycle's library-ingest outcome.
	marker.Set(true)
	slog.Info("seadex-scout started", "poll_interval", cfg.PollInterval.String(), "indexer", cfg.IndexerConfigured())

	runScheduler(ctx, cfg.PollInterval, b.scout, marker)
	normalShutdown = true
	return nil
}

// startIndexer launches the Torznab feed in a goroutine when it is configured,
// returning a func that waits for its graceful shutdown (once ctx is cancelled)
// and then releases its clients. When no Prowlarr Torznab URL is set it starts
// nothing - the daemon binds no HTTP port - and returns a no-op.
func startIndexer(ctx context.Context, cfg *config.Config) func() {
	if !cfg.IndexerConfigured() {
		return func() {}
	}
	bi := buildIndexer(cfg)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				slog.Error("indexer feed panicked", "panic", r, "stack", string(debug.Stack()))
			}
		}()
		if err := bi.indexer.Run(ctx); err != nil {
			logIndexerStop(ctx, err)
		}
	}()
	return func() {
		<-done
		bi.cleanup()
	}
}

// logIndexerStop classifies the indexer feed's Run error for the shared slog
// stream. Both shutdown-path cases are routine on a redeploy (WARN, kept off
// the level=ERROR cycle-error alert, matching the walk/matching/save
// classification), but they carry distinct messages: webhttp.Run returns
// DeadlineExceeded specifically when its graceful-shutdown budget expired,
// meaning in-flight Torznab requests were cut off - information worth its own
// log line rather than vanishing into the clean-shutdown message. Any error
// outside a shutdown is a fault and stays ERROR.
func logIndexerStop(ctx context.Context, err error) {
	switch {
	case ctx.Err() != nil && errors.Is(err, context.DeadlineExceeded):
		slog.Warn("indexer shutdown budget expired; in-flight requests aborted", "error", err, "cause", context.Cause(ctx))
	case ctx.Err() != nil && errors.Is(err, context.Canceled):
		// Bind cancelled mid-startup, or a clean graceful drain: routine.
		slog.Warn("indexer feed stopped during shutdown", "error", err, "cause", context.Cause(ctx))
	default:
		slog.Error("indexer feed stopped", "error", err)
	}
}

// runScheduler runs a cycle on each tick of a POLL_INTERVAL timer with ±10%
// jitter until ctx is cancelled. The first iteration fires immediately so a
// cycle runs promptly on boot; the marker is set to each cycle's health.
func runScheduler(ctx context.Context, interval time.Duration, sc cycler, marker *health.Marker) {
	scheduler.RunLoop(ctx, func(ctx context.Context) {
		healthy := runCycle(ctx, sc)
		if !healthy && ctx.Err() != nil {
			return // shutdown mid-cycle: cancellation is not an ingest fault
		}
		marker.Set(healthy)
	}, scheduler.LoopOptions{Interval: interval, FireOnStart: true, Jitter: 0.10})
}

// cycler runs one compare cycle, reporting whether the library ingest was
// healthy. Satisfied by *scout.Scout; a consumer-side seam so the daemon's
// panic shield is testable without a real Scout.
type cycler interface {
	Cycle(ctx context.Context) bool
}

// runCycle runs one cycle, recovering from a panic so a single bad cycle cannot
// crash the long-lived daemon. A panic is reported as an unhealthy cycle.
func runCycle(ctx context.Context, sc cycler) (healthy bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("cycle panicked", "panic", r, "stack", string(debug.Stack()))
			healthy = false
		}
	}()
	return sc.Cycle(ctx)
}

// logConfig logs the effective configuration at startup. API keys are never
// logged, only whether each is present.
func logConfig(cfg *config.Config) {
	pollInterval := cfg.PollInterval.String()
	if cfg.PollExternal {
		pollInterval = "external"
	}
	runMode := cfg.RunMode
	if runMode != config.RunModeDaemon && runMode != config.RunModeReport {
		// logConfig runs before Validate rejects an unrecognized mode, and the
		// raw value may be an expanded ${VAR} secret placed here by a config
		// typo - emit a fixed marker, never the value (Validate's error is
		// field-name-only for the same reason).
		runMode = "invalid"
	}
	slog.Info("configuration loaded",
		"sonarr_enabled", cfg.SonarrEnabled(),
		"radarr_enabled", cfg.RadarrEnabled(),
		"poll_interval", pollInterval,
		"exclude_remux", cfg.ExcludeRemux,
		"require_dual_audio", cfg.RequireDualAudio,
		"exclude_specials", cfg.ExcludeSpecials,
		"animebytes", cfg.AnimeBytes,
		"include_tags", len(cfg.IncludeTags),
		"exclude_tags", len(cfg.ExcludeTags),
		"run_mode", runMode)
}
