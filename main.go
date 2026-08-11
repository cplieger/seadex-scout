// Package main is seadex-scout: a watcher that compares a Sonarr/Radarr anime
// library against SeaDex (releases.moe) and emits a structured slog line
// whenever SeaDex recommends a better release than the one on disk. It never
// downloads or touches a torrent client; it tells the operator what to go get.
//
// main.go is the composition root: it installs logging, handles the distroless
// `health` subcommand, loads and validates the YAML config (CONFIG_PATH,
// default /config/config.yaml; a starter is written on first boot), builds the
// scout (build.go), dispatches the subcommand, and maps the result to a slog
// level and an exit code. All logic lives in internal/* - including the cycle
// coordination every entry point runs through (internal/cycle owns the
// cross-process coalescing lock and the shutdown-interruption contract; this
// root only wires it and translates its error into the process result).
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
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/cplieger/atomicfile/v2"
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
// The config file is where the operator may paste arr API keys and the AB
// passkey (see README), and an in-place edit keeps the creation mode, so it is
// owner-only like the indexer feed snapshot (internal/indexer/writer.go).
const (
	starterFileMode = 0o600
	starterDirMode  = 0o700
)

// modePoll is the subcommand-only mode: run one compare cycle for an external
// scheduler (paired with poll_interval: off). Not a valid config `mode`.
const modePoll = "poll"

// validArgsHint lists the accepted invocations; shared by the two
// invalid-invocation error messages so they cannot drift when a
// subcommand is added or removed.
const validArgsHint = "(valid: health, daemon, report, poll, or no argument)"

// unknownModeMarker is the fixed value logged in place of an unrecognized
// run mode: the raw value may be an expanded ${VAR} secret placed by a
// config typo, so logConfig and loggableMode both emit this marker instead
// (config.validateRunMode is field-name-only for the same reason).
const unknownModeMarker = "invalid"

// runModes is every mode resolveMode accepts. It is the ONE list both the
// resolution switch and loggableMode's redaction gate read, so adding a
// subcommand cannot leave a valid mode logged as the unknown-mode marker.
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
		// health.RunProbe terminates via os.Exit(0/1); if it ever returns
		// (a contract change in the separately versioned health dependency),
		// fail closed: report unhealthy rather than a silently-green probe -
		// and name the cause, since the probe runs in its own process and
		// docker records only its output for the operator to read.
		slog.Error("health probe returned without exiting; reporting unhealthy",
			"path", health.DefaultPath)
		os.Exit(1)
	}

	// Resolved after the health fast path: the probe reads only the marker, so it
	// must not depend on anything the operator can set (see runHealthProbe).
	configPath := cmp.Or(strings.TrimSpace(os.Getenv("CONFIG_PATH")), config.DefaultConfigPath)
	cfg, err := loadRuntimeConfig(configPath)
	if err != nil {
		// Every terminal outcome (a starter written on first boot, a starter
		// write failure, a load failure) is already logged by the helper with
		// its original level and message; main only owns the exit code.
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

// validateInvocation rejects malformed invocations with trailing arguments
// (e.g. `poll typo`, `health typo`) before the health fast path, so a typo can
// never run a real poll or report healthy. main maps a non-nil error to exit 2.
func validateInvocation(args []string) error {
	if len(args) > 1 {
		return fmt.Errorf("too many arguments %q %s", args, validArgsHint)
	}
	return nil
}

// runHealthProbe handles the health subcommand, which backs the Docker
// healthcheck. It reads NOTHING but the marker: no config, no arguments, no
// derived policy.
//
// It used to read the config to size a freshness lease (max(3*poll_interval,
// coldReconcileAllowance)) and arm it with health.WithMaxAge. That coupled the
// healthcheck to a file it may be unable to read, and the failure was
// self-defeating: a config the probe cannot parse silently DISABLED the
// deadline, and said so only into the healthcheck process's own output, which is
// Docker's health log rather than the stream shipped to Loki. So the one signal
// explaining why wedge detection had stopped was invisible by construction. It
// bought little even when it worked: the 3h floor binds for every poll_interval
// up to an hour, including the 15m default, so for the deployed configuration
// the config read could not change the answer.
//
// The lease now lives where the cadence is already known - the daemon arms
// watchdogLease against its own interval (see startWedgeWatchdog) and sets the
// marker unhealthy itself. The probe is a pure marker read, so it cannot be
// broken by anything the operator writes.
//
// health.RunProbe terminates via os.Exit(0/1), so the true return is reachable
// only if that contract ever changes - main then fails closed with exit 1.
func runHealthProbe(args []string) bool {
	if len(args) != 1 || args[0] != "health" {
		return false
	}
	health.RunProbe(health.DefaultPath)
	return true
}

// errStarterWritten is returned by loadRuntimeConfig after a first boot
// successfully wrote the starter config (logged as the edit-and-restart WARN),
// distinguishing that expected outcome from a genuine write or load failure
// (logged at ERROR). main exits 1 on both; the typed sentinel keeps the
// classification testable.
var errStarterWritten = errors.New("no config found; starter config written")

// loadRuntimeConfig runs the startup config sequence: a missing config file
// writes the first-boot starter, a present one is loaded. Every terminal
// outcome is logged here with its original level and message; a non-nil error
// means main must exit 1.
func loadRuntimeConfig(configPath string) (config.Config, error) {
	//nolint:gosec // G703: CONFIG_PATH is an operator-supplied path, not user input
	if _, err := os.Stat(configPath); errors.Is(err, fs.ErrNotExist) {
		if werr := writeStarterConfig(configPath); werr != nil {
			// The one first-boot failure an operator cannot read their way out
			// of: a bind-mount directory Docker created is root-owned, while
			// the compose example runs the process as PUID:PGID. The raw
			// writer error names a temp path and "permission denied" and
			// nothing else, and the container exits 1 into a restart loop, so
			// the remedy has to be in this line - together with the uid:gid it
			// is talking about, which is not visible from outside.
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
// parent directory, so a fresh deployment gets an editable starter — with a
// freshly generated feed_api_key already in place (see config.SeedStarter).
func writeStarterConfig(path string) error {
	starter, err := config.SeedStarter(exampleConfig)
	if err != nil {
		return err
	}
	// Written atomically (temp file + rename, parent dir created via
	// WithMkdirMode) through atomicfile, matching the report and state writers,
	// so a crash or power loss mid-write cannot leave a truncated starter config.
	// CONFIG_PATH is an operator-supplied path, not user input.
	if _, err := atomicfile.WriteFile(context.Background(), path, starter,
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
	if slices.Contains(runModes, args[0]) {
		return args[0], nil
	}
	return "", fmt.Errorf("unknown subcommand %q %s", args[0], validArgsHint)
}

// --- Report mode ---

// runReport runs the one-shot audit: build components, generate the report,
// emit it to slog, and write the JSON + Markdown files. It never writes state,
// so a one-shot report cannot clobber a running daemon's cache.
//
// Every stage's error passes through shutdown.NormalizeShutdownError on the way out, so
// a stage that reports the cancellation CAUSE rather than context.Canceled (an
// early library walk or SeaDex request cut off by SIGTERM) still reaches main
// as a routine-shutdown WARN instead of the level=ERROR fault line that trips
// the cycle-error alert on every redeploy.
func runReport(cfg *config.Config) (err error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	defer func() { err = shutdown.NormalizeShutdownError(ctx, err) }()

	if dirErr := checkReportDir(cfg.ReportDir); dirErr != nil {
		return dirErr
	}

	// The whole generate+write is serialized on an exclusive flock in the
	// report dir: two report runs finishing within the same UTC second would
	// target the same report-<timestamp>.{md,json} pair, so a concurrent
	// second run returns ErrReportRunning instead of racing; dispatchOutcome
	// treats that designed coalescing result as WARN with exit 0.
	// The report is read-only on state, so the lock guards only the report dir.
	// internal/cycle owns the lock acquisition for every entry point (the daemon
	// tick, an exec'd poll, this report run); the report.dir redaction that
	// keeps a secret-capable config value out of main's error log is applied
	// here, from the shared internal/pathredact leaf.
	release, err := cycle.TryReportLock(cfg.ReportDir)
	if err != nil {
		return pathredact.Err(cfg.ReportDir, err)
	}
	defer release()

	// The reporter build is deliberately narrower than the daemon's: a
	// read-only state store (the report never saves state, and a corrupt
	// state.json must be left in place - not quarantined - for the daemon's
	// own Load to detect and report on the container's log stream), and none of
	// the compare-cycle components the report cannot reach.
	b, err := buildReporter(ctx, cfg)
	if err != nil {
		return err
	}
	defer b.cleanup()

	rep, err := b.reporter.Report(ctx)
	if err != nil {
		return err
	}
	// The report itself is the expensive artifact - a full arr walk plus a
	// SeaDex fetch, measured at ~25m on a real library - while writing the pair
	// is a few milliseconds. Log's per-row emission returns as soon as the
	// context is done, so a redeploy SIGTERM landing in the row-emission tail
	// used to discard the whole deliverable and leave nothing on disk (l-f117).
	// An interrupted Log therefore no longer aborts the write: the rows are
	// diagnostics, the pair is the product. The row error is still reported when
	// the write itself succeeds, so an interrupted run keeps its non-zero exit.
	logErr := rep.Log(ctx, slog.Default())
	writeCtx, cancel := shutdown.DetachedWriteContext(ctx)
	defer cancel()
	if werr := rep.WriteFiles(writeCtx, cfg.ReportDir, slog.Default()); werr != nil {
		return shutdown.DetachedWriteError(ctx, werr)
	}
	return logErr
}

// checkReportDir rejects a report.dir this run cannot write to, before the run
// spends anything. Every report write goes through atomicfile, whose path gate
// refuses a non-absolute path outright (ErrUnsafePath "not absolute"), and
// nothing between here and there absolutizes the configured value - so a
// relative report.dir used to pass config validation, take the report lock,
// create a stray directory tree under the process working directory, run the
// full arr walk plus SeaDex fetch (~25m on a real library), and only THEN fail
// with neither half of the pair written (l-f213).
//
// This is not an acceptance change: such a run already fails, and config
// deliberately keeps the value warn-only at load (config.warnRelativeReportDir)
// because a daemon that never writes a report is unaffected. The check belongs
// on the report path, which is the only entry point that will write one, and in
// the composition root that owns the subcommand's lifecycle. Field-name-only:
// report.dir is secret-capable (config.Load may expand an allowlisted
// ${SEADEX_SCOUT_*} reference into it, which is why internal/audit redacts it),
// so the error names the key and never echoes the value.
func checkReportDir(dir string) error {
	if !filepath.IsAbs(dir) {
		return errors.New("report.dir must be an absolute path: report files are written " +
			"through an absolute-path-only writer, so a relative value cannot produce either " +
			"half of the report pair - use an absolute path under the /config mount")
	}
	return nil
}

// runPoll runs one compare cycle for an external scheduler (poll_interval: off).
// It updates the health marker to the cycle's outcome, leaving it in place (no
// Cleanup) so the container healthcheck reads the last poll, and exits non-zero
// on an unhealthy cycle so the scheduler (Ofelia job-exec, cron) sees the fail.
// The cycle runs under the cross-process cycle lock in queue mode: a request
// arriving while another cycle is in flight (an overlapping poll, or a daemon
// tick) is queued for the active runner instead of racing it (see
// cycle.RunOnce).
//
// Interruption contract (uniform across every phase of poll): a shutdown
// cancellation observed at any point - during startup, mid-cycle, or after the
// cycle body (including the state save) - exits non-zero, classified as a routine
// shutdown (WARN, not the level=ERROR cycle-error alert) via the context.Canceled
// wrap main inspects. The shared health marker is CYCLE-scoped, not
// invocation-scoped: an interruption before or during the cycle leaves it
// untouched (nothing completed), but one observed after the cycle body does not
// withdraw the verdict that cycle already committed from inside the cycle lock -
// see cycle.RunOnce and shutdown.Interrupted, which own that rule.
func runPoll(cfg *config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	b, err := buildScout(ctx, cfg, nil)
	if err != nil {
		if ctx.Err() != nil {
			// Shutdown cancelled startup (pre-cycle phase of the uniform
			// interruption contract): wrap the cancellation cause so main
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

// dispatchOutcome classifies a dispatch error into the two operator-visible
// contracts main owns: the slog level a Loki alert keys on, and the process exit
// code a scheduler reads. Both follow one rule - a condition that will not clear
// without the operator is an ERROR; a transient or DESIGNED outcome is a WARN -
// so the classification is kept here as a pure function rather than inline in
// main, where neither contract could be pinned by a test.
//
//   - ErrReportRunning is the designed coalescing outcome: another run holds the
//     lock and is producing the report, so nothing is wrong and nothing is
//     owed. The poll path already treats its own busy case this way (queued,
//     exit 0); matching it keeps a routine skip off the cycle-error alert and
//     stops a scheduler from recording a healthy skip as a failed job.
//   - context.Canceled is a shutdown signal (signal.NotifyContext cancels with
//     it): routine, but the run did not deliver, so it still exits non-zero.
//     A DeadlineExceeded is a genuine operation timeout and falls through.
//   - everything else is a fault the operator must look at.
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

	// The process-level completion record is registered before the cleanup
	// defers below, so (defers run LIFO) it logs after the indexer's drain has
	// been waited out (bounded by the feed's own stop budget), the client
	// cleanup, and the health-marker removal. A drain that outruns that bound is
	// reported by the feed's own WARN and its goroutine may log after this line,
	// so the WARN - not the ordering - is what tells Loki the drain did not
	// complete.
	// normalShutdown guards it so a startup-error return does not log a
	// successful shutdown.
	normalShutdown := false
	defer func() {
		if normalShutdown {
			slog.Info("shutdown complete", "cause", context.Cause(ctx))
		}
	}()

	marker := health.NewMarker(health.DefaultPath)
	marker.Set(false)
	defer marker.Cleanup()

	// The Torznab feed runs alongside the compare loop in the same process, so
	// one daemon serves both features with no on/off knob. It is built BEFORE the
	// scout because the compare cycle publishes each completed snapshot straight
	// into it (buildScout threads it to the feed writer), which is what keeps the
	// snapshot file off the serving path here. It starts only when a Prowlarr
	// Torznab URL is configured (else the daemon binds no HTTP port), owns no
	// health marker (the compare loop does), and its failure is logged without
	// affecting the compare loop.
	bi := buildIndexer(cfg)

	b, err := buildScout(ctx, cfg, bi.indexer)
	if err != nil {
		bi.cleanup()
		return err
	}
	defer b.cleanup()

	// stopIndexer waits for the feed's graceful shutdown before releasing its
	// clients.
	stopIndexer := startIndexer(ctx, bi)
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
	// Ticks run under the cross-process cycle lock in skip mode, so a tick
	// arriving while an exec'd `poll` cycle is in flight skips instead of
	// racing it (see cycle.RunLoop).
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
// returning the func that stops it and waits for its graceful drain (the feed
// owns that supervision - the panic shield, the resource release and the drain
// budget - in internal/indexer.Supervise, beside the timeouts the budget is
// derived from). With no Prowlarr Torznab URL configured buildIndexer built
// nothing, so this starts nothing - the daemon binds no HTTP port - and returns
// a no-op.
func startIndexer(ctx context.Context, bi builtIndexer) func() {
	if bi.indexer == nil {
		return func() {}
	}
	return bi.indexer.Supervise(ctx, bi.cleanup)
}

// --- Logging helpers ---

// logConfig logs the effective configuration at startup, with mode the
// RESOLVED run mode (the subcommand when one was given, else the config's
// `mode`) rather than the config key alone: the documented one-shot
// `command: report` container leaves `mode: daemon` in the file, so an
// operator filtering that container's Loki stream on run_mode would
// otherwise read the mode the process is NOT running. API keys are never
// logged, only whether each is present.
func logConfig(cfg *config.Config, mode string) {
	pollInterval := cfg.PollInterval.String()
	if cfg.PollExternal {
		pollInterval = "external"
	}
	// loggableMode masks an unrecognized value with the fixed marker: it may
	// be an expanded ${VAR} secret placed by a config typo, and logConfig runs
	// before Validate rejects it (config.validateRunMode is field-name-only
	// for the same reason).
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
// "invalid" marker: an unrecognized value may be an expanded ${VAR} secret
// placed by a config typo (the same contract as logConfig and
// config.validateRunMode, which are both deliberately field-name-only).
func loggableMode(mode string) string {
	if slices.Contains(runModes, mode) {
		return mode
	}
	return unknownModeMarker
}
