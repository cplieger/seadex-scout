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
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/health"
	"github.com/cplieger/scheduler/v2"
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

	args := os.Args[1:]
	if err := validateInvocation(args); err != nil {
		// Exit 2 = invalid invocation, matching the resolveMode contract below.
		slog.Error("invalid invocation", "error", err)
		os.Exit(2)
	}

	configPath := cmp.Or(strings.TrimSpace(os.Getenv("CONFIG_PATH")), config.DefaultConfigPath)
	if runHealthProbe(args, configPath) {
		// health.RunProbe terminates via os.Exit(0/1); if it ever returns
		// (a contract change in the separately versioned health dependency),
		// fail closed: report unhealthy rather than a silently-green probe.
		os.Exit(1)
	}

	cfg, err := loadRuntimeConfig(configPath)
	if err != nil {
		// Every terminal outcome (a starter written on first boot, a starter
		// write failure, a load failure) is already logged by the helper with
		// its original level and message; main only owns the exit code.
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
// healthcheck and must not require configuration, so it runs before config
// load. It reports false when the invocation is not `health` (main continues
// with normal startup). The probe reads the config best-effort (never failing
// on absence or parse errors) to derive the freshness deadline: in scheduled
// mode each cycle refreshes the marker, so a marker older than 3 poll
// intervals means a wedged compare loop and a restart fixes it. External mode
// (poll_interval: off) and any config-read failure disable the deadline
// (WithMaxAge(0) is a no-op): idle-until-poll is healthy. health.RunProbe
// terminates via os.Exit(0/1), so the true return is reachable only if that
// contract ever changes - main then fails closed with exit 1.
func runHealthProbe(args []string, configPath string) bool {
	if len(args) != 1 || args[0] != "health" {
		return false
	}
	health.RunProbe(health.DefaultPath,
		health.WithMaxAge(3*config.PollIntervalFromFile(configPath)))
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
	//nolint:gosec // G304: CONFIG_PATH is an operator-supplied path, not user input
	if _, err := os.Stat(configPath); errors.Is(err, fs.ErrNotExist) {
		if werr := writeStarterConfig(configPath); werr != nil {
			slog.Error("no config found and could not write a starter", "path", configPath, "error", werr)
			return config.Config{}, fmt.Errorf("write starter config: %w", werr)
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

	// Read-only state store: the report never saves state, and a corrupt
	// state.json must be left in place (not quarantined) for the daemon's
	// own Load to detect and report on the container's log stream.
	b, err := buildScout(ctx, cfg, true)
	if err != nil {
		return err
	}
	defer b.cleanup()

	rep, err := b.scout.Report(ctx)
	if err != nil {
		return err
	}
	if err := rep.Log(ctx, slog.Default()); err != nil {
		return err
	}
	return rep.WriteFiles(ctx, cfg.ReportDir, slog.Default())
}

// cycleDirMode is applied when creating the cycle-lock directory (normally
// /config, which already exists as the mounted volume holding the config and
// state files this lock guards).
const cycleDirMode = 0o755

// newCycleExclusive builds the cross-process cycle coalescer shared by every
// cycle entry point: the daemon's RunLoop ticks (skip mode) and exec'd `poll`
// subcommands (queue mode) serialize on dir/cycle.lock, closing the
// last-writer-wins race two concurrent cycles run on state.json (AniList memo
// and finding-dedupe loss, duplicate alerts) and feed.json. dir is the state
// directory (filepath.Dir(config.DefaultStatePath)) so the lock lives beside
// the files it guards; the kernel releases the flock if a process dies, so
// there is no stale-lock state. The gate stops queued reruns (and a
// not-yet-started initial run) once shutdown is signalled; an in-flight run
// is never interrupted by the gate - context cancellation owns that.
func newCycleExclusive(ctx context.Context, dir string) (*scheduler.Exclusive, error) {
	if err := os.MkdirAll(dir, cycleDirMode); err != nil {
		return nil, fmt.Errorf("create cycle lock dir %s: %w", dir, err)
	}
	return scheduler.NewExclusive(dir, slog.Default(),
		scheduler.WithGate(func() bool { return ctx.Err() == nil })), nil
}

// pollInterrupted wraps the shutdown cancellation cause with poll's uniform
// interruption message, so main classifies it as a routine-shutdown WARN and
// the marker-untouched contract reads identically from every phase.
func pollInterrupted(ctx context.Context) error {
	return fmt.Errorf("poll interrupted; health marker left unchanged: %w", context.Cause(ctx))
}

// runPoll runs one compare cycle for an external scheduler (poll_interval: off).
// It updates the health marker to the cycle's outcome, leaving it in place (no
// Cleanup) so the container healthcheck reads the last poll, and exits non-zero
// on an unhealthy cycle so the scheduler (Ofelia job-exec, cron) sees the fail.
// The cycle runs under the cross-process cycle lock in queue mode: a request
// arriving while another cycle is in flight (an overlapping poll, or a daemon
// tick) is queued for the active runner instead of racing it (see pollCycle).
//
// Interruption contract (uniform across every phase of poll): a shutdown
// cancellation observed at any point - during startup, mid-cycle, or after the
// cycle body (including the state save) - exits non-zero with the shared health
// marker untouched, classified as a routine shutdown (WARN, not the level=ERROR
// cycle-error alert) via the context.Canceled wrap main inspects.
func runPoll(cfg *config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	b, err := buildScout(ctx, cfg, false)
	if err != nil {
		if ctx.Err() != nil {
			// Shutdown cancelled startup (pre-cycle phase of the uniform
			// interruption contract): wrap the cancellation cause so main
			// classifies it WARN, and never touch the marker.
			return pollInterrupted(ctx)
		}
		return err
	}
	defer b.cleanup()

	ex, err := newCycleExclusive(ctx, filepath.Dir(config.DefaultStatePath))
	if err != nil {
		return err
	}
	return pollCycle(ctx, ex, b.scout, health.NewMarker(health.DefaultPath))
}

// pollOnce is one execution of poll's cycle body under the cycle lock: run the
// cycle, apply the interruption contract (a cancellation observed at any point
// - even when the cycle still managed to complete healthy, e.g. the signal
// landed during the end-of-cycle save - leaves the shared marker at the
// daemon's last real state, since an interrupted run's outcome is not a
// trustworthy health verdict), then record the outcome on the marker. The
// active runner may execute it again for demand queued by other processes;
// each execution records its own cycle's health.
func pollOnce(ctx context.Context, sc cycler, marker *health.Marker) error {
	healthy := runCycle(ctx, sc)
	if ctx.Err() != nil {
		return pollInterrupted(ctx)
	}
	if err := marker.SetChecked(healthy); err != nil {
		return fmt.Errorf("record poll health: %w", err)
	}
	if !healthy {
		return errors.New("compare cycle failed (library ingest)")
	}
	return nil
}

// pollCycle runs poll's one cycle under the cross-process cycle lock (queue
// mode) and maps the coalescing outcome to poll's exit contract:
//
//   - Ran (or ran plus queued reruns): the exit code reflects this
//     invocation's OWN (first) run - a healthy cycle exits 0, an unhealthy or
//     interrupted one non-zero (see pollOnce). The closure can run again for
//     demand queued by OTHER processes; those cycles report through their own
//     log lines and marker updates, never this process's exit code.
//   - Queued / Discarded: a cycle is already in flight (an overlapping poll or
//     a daemon tick); the request was recorded for (or is already covered by)
//     the active runner, which is owed to start a run after it arrived. That
//     is success for this process: log and exit 0, marker untouched (the
//     active runner's cycle records its own outcome) — unless cancellation
//     was observed by then, in which case the uniform interruption contract
//     wins (exit non-zero; the recorded demand still stands).
//   - Gated: shutdown was signalled before the run started - the uniform
//     interruption contract applies (exit non-zero, WARN classification,
//     marker untouched).
//   - Nothing ran and no demand recorded (a cycle-lock infrastructure
//     failure): exit non-zero with the error.
func pollCycle(ctx context.Context, ex *scheduler.Exclusive, sc cycler, marker *health.Marker) error {
	// A pre-cancelled invocation must not enqueue demand: Exclusive's gate
	// refuses the RUN, not the queue insertion, so with the lock held by
	// another process a cancelled poll would still queue a rerun and report
	// success, adding work after shutdown was signalled. The uniform
	// interruption contract applies instead (exit non-zero, marker untouched).
	if ctx.Err() != nil {
		return pollInterrupted(ctx)
	}
	// Capture the first execution's outcome: it is this invocation's own run.
	// The closure returns nil to Exclusive so exErr stays purely a
	// coordination-infrastructure signal (job outcomes must not stop queued
	// demand or muddy the infra-error accounting below).
	var own error
	ran := false
	outcome, exErr := ex.Run(func() error {
		err := pollOnce(ctx, sc, marker)
		if !ran {
			own, ran = err, true
		}
		return nil
	})
	switch outcome {
	case scheduler.OutcomeQueued, scheduler.OutcomeDiscarded:
		if exErr != nil {
			slog.Warn("cycle coordination error after queueing; demand stands", "error", exErr)
		}
		if ctx.Err() != nil {
			// Cancellation arrived while Run coordinated with the busy owner:
			// the recorded demand stands for the active runner, but this
			// invocation still reports the uniform interruption contract
			// (exit non-zero, WARN classification, marker untouched) rather
			// than a success observed after shutdown was signalled.
			return pollInterrupted(ctx)
		}
		slog.Info("compare cycle already in flight; demand queued for the active runner",
			"outcome", outcome.String())
		return nil
	case scheduler.OutcomeGated:
		return pollInterrupted(ctx)
	case scheduler.OutcomeNone, scheduler.OutcomeRan, scheduler.OutcomeRanQueued, scheduler.OutcomeSkipped:
		// Fall through to the ran/own accounting below. OutcomeSkipped is
		// unreachable from queue-mode Run; it is listed for switch completeness.
	}
	if !ran {
		return fmt.Errorf("cycle coordination failed: %w", exErr)
	}
	if exErr != nil {
		// The run itself completed; a queue-file error only degrades the
		// demand-coalescing bookkeeping, so it is logged rather than failing
		// the cycle this invocation paid for.
		slog.Warn("cycle coordination error after run", "error", exErr)
	}
	return own
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

	b, err := buildScout(ctx, cfg, false)
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
	// Ticks run under the cross-process cycle lock in skip mode, so a tick
	// arriving while an exec'd `poll` cycle is in flight skips instead of
	// racing it (see runScheduler).
	ex, err := newCycleExclusive(ctx, filepath.Dir(config.DefaultStatePath))
	if err != nil {
		return err
	}
	marker.Set(true)
	slog.Info("seadex-scout started", "poll_interval", cfg.PollInterval.String(), "indexer", cfg.IndexerConfigured())

	runScheduler(ctx, cfg.PollInterval, ex, b.scout, marker)
	normalShutdown = true
	return nil
}

// startIndexer launches the Torznab feed in a goroutine when it is configured,
// returning a func that stops it (cancelling its context) and waits for its
// graceful shutdown. The goroutine releases its clients itself on every exit path -
// a Run return or a recovered panic - so the transport is freed immediately
// even if the daemon keeps running. When no Prowlarr Torznab URL is set it
// starts nothing - the daemon binds no HTTP port - and returns a no-op.
func startIndexer(ctx context.Context, cfg *config.Config) func() {
	if !cfg.IndexerConfigured() {
		return func() {}
	}
	// The goroutine runs on its own cancellable child context so the
	// returned stop func can force the feed down even when the parent
	// signal context is still live (a startup-error return unwinding the
	// defers before stop() cancels it) - otherwise the wait below would
	// deadlock the exiting daemon against a still-serving feed.
	ictx, cancel := context.WithCancel(ctx)
	bi := buildIndexer(cfg)
	done := make(chan struct{})
	// The goroutine's terminal records (a recovered panic, the Run stop
	// classification) carry the same component=indexer scope the feed's
	// request and lifecycle logs use (see buildIndexer), so the feed's most
	// important failure lines stay routable/queryable with the rest of its
	// stream in Loki.
	log := slog.Default().With("component", "indexer")
	go func() {
		defer close(done)
		defer bi.cleanup()
		defer func() {
			if r := recover(); r != nil {
				log.Error("indexer feed panicked", "panic", r, "stack", string(debug.Stack()))
			}
		}()
		if err := bi.indexer.Run(ictx); err != nil {
			logIndexerStop(ictx, log, err)
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// logIndexerStop classifies the indexer feed's Run error for the shared slog
// stream, emitting through the caller's indexer-scoped logger so the terminal
// record carries the same component=indexer context as the feed's normal logs.
// Both shutdown-path cases are routine on a redeploy (WARN, kept off
// the level=ERROR cycle-error alert, matching the walk/matching/save
// classification), but they carry distinct messages: webhttp.Run returns
// DeadlineExceeded specifically when its graceful-shutdown budget expired,
// meaning in-flight Torznab requests were cut off - information worth its own
// log line rather than vanishing into the clean-shutdown message. Any error
// outside a shutdown is a fault and stays ERROR.
func logIndexerStop(ctx context.Context, log *slog.Logger, err error) {
	switch {
	case ctx.Err() != nil && errors.Is(err, context.DeadlineExceeded):
		log.Warn("indexer shutdown budget expired; in-flight requests aborted", "error", err, "cause", context.Cause(ctx))
	case ctx.Err() != nil && errors.Is(err, context.Canceled):
		// Bind cancelled mid-startup, or a clean graceful drain: routine.
		log.Warn("indexer feed stopped during shutdown", "error", err, "cause", context.Cause(ctx))
	default:
		log.Error("indexer feed stopped", "error", err)
	}
}

// runScheduler runs a cycle on each tick of a POLL_INTERVAL timer with ±10%
// jitter until ctx is cancelled. The first iteration fires immediately so a
// cycle runs promptly on boot; the marker is set to each cycle's health.
// Each tick body runs under the cross-process cycle lock in skip mode: a tick
// arriving while a cycle is already in flight (an exec'd `poll` racing the
// loop) is skipped with a WARN and the marker untouched - the next tick
// provides freshness, and the in-flight cycle records its own outcome. An
// acquired tick also executes demand queued by `poll` requests that arrived
// during it (one rerun per queued request), each recording its own health.
// A coordination-infrastructure failure (the lock or queue file unusable)
// means the tick could not run at all and is logged at ERROR - cycles have
// stopped, which the operator must see (the level=ERROR Loki alert fires).
func runScheduler(ctx context.Context, interval time.Duration, ex *scheduler.Exclusive, sc cycler, marker *health.Marker) {
	scheduler.RunLoop(ctx, func(ctx context.Context) {
		outcome, err := ex.RunOrSkip(func() error {
			healthy := runCycle(ctx, sc)
			if !healthy && ctx.Err() != nil {
				return nil // shutdown mid-cycle: cancellation is not an ingest fault
			}
			marker.Set(healthy)
			return nil
		})
		switch {
		case err == nil:
		case outcome == scheduler.OutcomeNone:
			slog.Error("cycle coordination failed; tick did not run", "error", err)
		default:
			// The tick's cycle ran; a queue-file error only degrades the
			// demand-coalescing bookkeeping.
			slog.Warn("cycle coordination error after run", "outcome", outcome.String(), "error", err)
		}
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
