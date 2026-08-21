// Package main is seadex-scout: a watcher that compares a Sonarr/Radarr anime
// library against SeaDex (releases.moe) and emits a structured slog line
// whenever SeaDex recommends a better release than the one on disk. It never
// downloads or touches a torrent client; it tells the operator what to go get.
//
// main.go is the composition root: it installs logging, handles the distroless
// `health` subcommand, loads and validates the YAML config (CONFIG_PATH,
// default /config/config.yaml; a starter is written on first boot), builds the
// scout (build.go), dispatches the subcommand, and maps the result to a slog
// level and an exit code.
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
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/cplieger/atomicfile/v3"
	"github.com/cplieger/health"
	"github.com/cplieger/seadex-scout/internal/config"
	"github.com/cplieger/seadex-scout/internal/cycle"
	"github.com/cplieger/seadex-scout/internal/pathredact"
	"github.com/cplieger/seadex-scout/internal/shutdown"
)

// exampleConfig is the starter written to CONFIG_PATH on first boot; it is also
// shipped as config.example.yaml in the repo root.
//
//go:embed config.example.yaml
var exampleConfig []byte

// starterFileMode / starterDirMode are applied to a generated starter config.
const (
	starterFileMode = 0o600
	starterDirMode  = 0o700
)

// modePoll is the subcommand-only mode: run one compare cycle for an external
// scheduler (paired with poll_interval: off). Not a valid config `mode`.
const modePoll = "poll"

// validArgsHint lists the accepted invocations, shared by both invalid-invocation errors.
const validArgsHint = "(valid: health, daemon, report, poll, or no argument)"

// unknownModeMarker is the fixed value logged in place of an unrecognized
// run mode: the raw value may be an expanded ${VAR} secret from a config typo.
const unknownModeMarker = "invalid"

// runModes is every mode resolveMode accepts. loggableMode's redaction gate reads
// the same list, so a new subcommand cannot log a valid mode as the unknown marker.
var runModes = []string{config.RunModeDaemon, config.RunModeReport, modePoll}

func main() {
	installLogger()

	args := os.Args[1:]
	if err := validateInvocation(args); err != nil {
		// Exit 2 = invalid invocation, matching the resolveMode contract below.
		slog.Error("invalid invocation", "error", err)
		os.Exit(2)
	}

	if runHealthProbe(args) {
		// health.RunProbe exits 0/1; a return means that contract changed, so fail
		// closed and name the cause - docker records only the probe's own output.
		slog.Error("health probe returned without exiting; reporting unhealthy",
			"path", health.DefaultPath)
		os.Exit(1)
	}

	// Resolved after the health fast path: the probe reads only the marker, so it
	// must not depend on anything the operator can set (see runHealthProbe).
	configPath := cmp.Or(strings.TrimSpace(os.Getenv("CONFIG_PATH")), config.DefaultConfigPath)
	cfg, err := loadRuntimeConfig(configPath)
	if err != nil {
		// The helper already logged every terminal outcome; main only owns the exit code.
		os.Exit(1)
	}
	configureLogger(cfg.LogLevel, cfg.LogFormat)

	mode, err := resolveMode(args, &cfg)
	if err != nil {
		slog.Error("invalid invocation", "error", err)
		os.Exit(2)
	}
	logConfig(&cfg, mode)

	if err := dispatch(mode, &cfg); err != nil {
		level, msg, code := dispatchOutcome(err)
		slog.Log(context.Background(), level, msg, "mode", loggableMode(mode), "error", err)
		if code != 0 {
			os.Exit(code)
		}
	}
}

// --- Startup: invocation validation, config bootstrap, mode resolution ---

// validateInvocation rejects a malformed invocation with trailing arguments, so a
// typo can never run a real poll or report healthy. main maps the error to exit 2.
func validateInvocation(args []string) error {
	if len(args) > 1 {
		return fmt.Errorf("too many arguments %q %s", args, validArgsHint)
	}
	return nil
}

// runHealthProbe handles the health subcommand, which backs the Docker
// healthcheck. It reads NOTHING but the marker: no config, no arguments, no
// derived policy, so nothing the operator writes can break it.
//
// health.RunProbe terminates via os.Exit(0/1), so the true return is reachable
// only if that contract changes - main then fails closed with exit 1.
func runHealthProbe(args []string) bool {
	if len(args) != 1 || args[0] != "health" {
		return false
	}
	health.RunProbe(health.DefaultPath)
	return true
}

// errStarterWritten distinguishes a successful first-boot starter write (the
// edit-and-restart WARN) from a genuine write or load failure; main exits 1 on both.
var errStarterWritten = errors.New("no config found; starter config written")

// loadRuntimeConfig runs the startup config sequence: a missing config file writes
// the first-boot starter, a present one is loaded. It logs every terminal outcome
// at its original level; a non-nil error means main must exit 1.
func loadRuntimeConfig(configPath string) (config.Config, error) {
	//nolint:gosec // G703: CONFIG_PATH is an operator-supplied path, not user input
	if _, err := os.Stat(configPath); errors.Is(err, fs.ErrNotExist) {
		if werr := writeStarterConfig(configPath); werr != nil {
			// The one first-boot failure an operator cannot read their way out of: a
			// root-owned bind-mount directory against the compose PUID:PGID. The raw
			// writer error names neither, and the container exits into a restart loop.
			if errors.Is(werr, fs.ErrPermission) {
				slog.Error("no config found and could not write a starter: the config directory is not writable by this container's user - chown it on the host to this uid:gid (compose sets it via user: \"${PUID:-1000}:${PGID:-1000}\") and restart",
					"path", configPath, "uid", os.Getuid(), "gid", os.Getgid(), "error", werr)
				return config.Config{}, werr
			}
			slog.Error("no config found and could not write a starter", "path", configPath, "error", werr)
			return config.Config{}, werr
		}
		slog.Warn("no config found; wrote a starter config - set your Sonarr/Radarr url + api_key and restart", "path", configPath)
		return config.Config{}, errStarterWritten
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		slog.Error("failed to load config", "path", configPath, "error", err)
		return config.Config{}, fmt.Errorf("load config: %w", err)
	}
	return cfg, nil
}

// dispatch validates the config, then runs the resolved mode. Each run body lives
// in a helper so its defers always execute; os.Exit stays in main so it skips none.
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
// parent directory, with a freshly generated feed_api_key already in place.
func writeStarterConfig(path string) error {
	starter, err := config.SeedStarter(exampleConfig)
	if err != nil {
		return err
	}
	if _, err := atomicfile.WriteFile(context.Background(), path, starter,
		atomicfile.WithMkdirMode(starterDirMode),
		atomicfile.WithMode(starterFileMode)); err != nil {
		return fmt.Errorf("write starter config: %w", err)
	}
	return nil
}

// resolveMode decides the run mode from the optional subcommand
// (daemon | report | poll) or, with no subcommand, the config's `mode`
// (daemon | report). main rejects a multi-argument invocation earlier, so args
// holds at most one subcommand here.
func resolveMode(args []string, cfg *config.Config) (mode string, err error) {
	if len(args) == 0 {
		return cfg.RunMode, nil
	}
	if slices.Contains(runModes, args[0]) {
		return args[0], nil
	}
	return "", fmt.Errorf("unknown subcommand %q %s", args[0], validArgsHint)
}

// --- Report mode ---

// runReport runs the one-shot audit: build components, generate the report, emit
// it to slog, and write the JSON + Markdown files. It never writes state, so a
// one-shot report cannot clobber a running daemon's cache.
func runReport(cfg *config.Config) (err error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	defer func() { err = shutdown.Normalize(ctx, err) }()

	if dirErr := checkReportDir(cfg.ReportDir); dirErr != nil {
		return dirErr
	}

	// The whole generate+write is serialized on an exclusive flock in the report
	// dir: two runs finishing within the same UTC second would target the same
	// report-<timestamp>.{md,json} pair, so the second returns ErrReportRunning.
	release, err := cycle.TryReportLock(cfg.ReportDir)
	if err != nil {
		return pathredact.Err(cfg.ReportDir, err)
	}
	defer release()

	// Narrower than the daemon's build: a read-only state store, so a corrupt
	// state.json is left in place for the daemon's own Load to detect and report.
	b, err := buildReporter(ctx, cfg)
	if err != nil {
		return err
	}
	defer b.cleanup()

	rep, err := b.reporter.Report(ctx)
	if err != nil {
		return err
	}
	// The pair is the product and the rows are diagnostics, so an interrupted Log
	// does not abort the write (the artifact costs ~25m, the write milliseconds).
	// The row error still surfaces, so an interrupted run keeps its non-zero exit.
	logErr := rep.Log(ctx, slog.Default())
	writeCtx, cancel := shutdown.DetachedWriteContext(ctx)
	defer cancel()
	if werr := rep.WriteFiles(writeCtx, cfg.ReportDir, slog.Default()); werr != nil {
		return shutdown.DetachedWriteError(ctx, werr)
	}
	return logErr
}

// checkReportDir rejects a report.dir this run cannot write to, before the run
// spends anything: every report write goes through an absolute-path-only writer
// and nothing between here and there absolutizes the value. Field-name-only:
// report.dir is secret-capable, so the error names the key and never the value.
func checkReportDir(dir string) error {
	if !filepath.IsAbs(dir) {
		return errors.New("report.dir must be an absolute path: report files are written " +
			"through an absolute-path-only writer, so a relative value cannot produce either " +
			"half of the report pair - use an absolute path under the /config mount")
	}
	return nil
}

// runPoll runs one compare cycle for an external scheduler (poll_interval: off).
// It leaves the health marker in place (no Cleanup) so the container healthcheck
// reads the last poll, and exits non-zero on an unhealthy cycle so the scheduler
// sees the fail.
//
// The marker is CYCLE-scoped, not invocation-scoped: a shutdown observed before or
// during the cycle leaves it untouched, but one observed after the cycle body does
// not withdraw the verdict that cycle already committed.
func runPoll(cfg *config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	b, err := buildScout(ctx, cfg, nil)
	if err != nil {
		if ctx.Err() != nil {
			// Shutdown cancelled startup: wrap the cancellation cause so main
			// classifies it WARN, and never touch the marker.
			return shutdown.Interrupted(ctx)
		}
		return err
	}
	defer b.cleanup()

	ex, err := cycle.NewExclusive(ctx, config.DefaultCycleLockDir)
	if err != nil {
		return err
	}
	return cycle.RunOnce(ctx, ex, b.scout, health.NewMarker(health.DefaultPath))
}

// dispatchOutcome classifies a dispatch error into the slog level a Loki alert
// keys on and the exit code a scheduler reads. One rule: a condition that will not
// clear without the operator is an ERROR, a transient or DESIGNED outcome a WARN.
// ErrReportRunning is the designed coalescing outcome (exit 0, so a routine skip
// stays off the cycle-error alert); context.Canceled is shutdown - routine, but the
// run did not deliver, so it still exits non-zero, while a DeadlineExceeded is a
// genuine operation timeout and falls through.
func dispatchOutcome(err error) (level slog.Level, msg string, exit int) {
	switch {
	case errors.Is(err, cycle.ErrReportRunning):
		return slog.LevelWarn, "report skipped; another report is already running", 0
	case errors.Is(err, context.Canceled):
		return slog.LevelWarn, "seadex-scout interrupted by shutdown", 1
	default:
		return slog.LevelError, "seadex-scout failed", 1
	}
}

// --- Daemon mode: poll loop + indexer feed ---

// run wires up the daemon and polls until the context is cancelled. It returns
// an error only on a startup failure.
func run(cfg *config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Registered before the cleanup defers below, so (LIFO) it logs after the
	// indexer drain wait, the client cleanup and the health-marker removal.
	// normalShutdown guards it so a startup error does not log a clean shutdown.
	normalShutdown := false
	defer func() {
		if normalShutdown {
			slog.Info("shutdown complete", "cause", context.Cause(ctx))
		}
	}()

	marker := health.NewMarker(health.DefaultPath)
	marker.Set(false)
	defer marker.Cleanup()

	// Built BEFORE the scout because the compare cycle publishes each completed
	// snapshot straight into it, which keeps the snapshot file off the serving path.
	// It starts only when a Prowlarr Torznab URL is configured.
	bi := buildIndexer(cfg)

	b, err := buildScout(ctx, cfg, bi.indexer)
	if err != nil {
		bi.cleanup()
		return err
	}
	defer b.cleanup()

	stopIndexer := startIndexer(ctx, bi)
	defer stopIndexer()

	// Resident-idle (poll_interval: off): no internal timer; healthy on boot and
	// cycles are triggered out-of-band via the `poll` subcommand.
	if cfg.PollExternal {
		marker.Set(true)
		slog.Info("seadex-scout started (resident-idle; trigger a cycle with the `poll` subcommand)",
			"indexer", cfg.IndexerConfigured())
		<-ctx.Done()
		normalShutdown = true
		return nil
	}

	// Built-in scheduler. Healthy on boot: the first cycle runs as the loop's first
	// iteration, so a slow first cycle never gates startup health. Ticks run under
	// the cross-process cycle lock in skip mode.
	ex, err := cycle.NewExclusive(ctx, config.DefaultCycleLockDir)
	if err != nil {
		return err
	}
	marker.Set(true)
	slog.Info("seadex-scout started", "poll_interval", cfg.PollInterval.String(), "indexer", cfg.IndexerConfigured())

	stopWatchdog := cycle.StartWedgeWatchdog(ctx, marker, health.DefaultPath, cycle.WatchdogLease(cfg.PollInterval))
	defer stopWatchdog()

	cycle.RunLoop(ctx, cfg.PollInterval, ex, b.scout, marker)
	normalShutdown = true
	return nil
}

// startIndexer launches the Torznab feed in a goroutine when one was built,
// returning the func that stops it and waits for its graceful drain.
func startIndexer(ctx context.Context, bi builtIndexer) func() {
	if bi.indexer == nil {
		return func() {}
	}
	return bi.indexer.Supervise(ctx, bi.cleanup)
}

// --- Logging helpers ---

// logConfig logs the effective configuration at startup, with mode the RESOLVED
// run mode rather than the config key alone: the documented one-shot
// `command: report` container leaves `mode: daemon` in the file. API keys are
// never logged, only whether each is present.
func logConfig(cfg *config.Config, mode string) {
	pollInterval := cfg.PollInterval.String()
	if cfg.PollExternal {
		pollInterval = "external"
	}
	// An unrecognized value may be an expanded ${VAR} secret placed by a config
	// typo, and logConfig runs before Validate rejects it.
	runMode := loggableMode(mode)
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
		"tag_exclusions", cfg.TagFilter.Len(),
		"ignored_findings", len(cfg.IgnoreFindings),
		"run_mode", runMode)
}

// loggableMode returns mode when it is a known run mode, else the fixed
// "invalid" marker: an unrecognized value may be an expanded ${VAR} secret.
func loggableMode(mode string) string {
	if slices.Contains(runModes, mode) {
		return mode
	}
	return unknownModeMarker
}
