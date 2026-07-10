// Package main is seadex-scout: a report-only watcher that compares a
// Sonarr/Radarr anime library against SeaDex (releases.moe) and emits a
// structured slog line whenever SeaDex recommends a better release than the one
// on disk. It never downloads or touches a torrent client; it tells the operator
// what to go get. Observability is slog-only; there is no HTTP surface.
//
// main.go is the composition root: it installs logging, handles the distroless
// `health` subcommand, loads and validates the YAML config (CONFIG_PATH,
// default /config/config.yaml; a starter is written on first boot), builds the
// scout (build.go), and runs the poll loop. All logic lives in internal/*.
//
// Run modes: the daemon (no argument, or mode: daemon) runs a compare cycle on
// start and every poll_interval, or sits resident-idle when poll_interval is
// off/disabled/0 (an external scheduler drives cycles via the `poll`
// subcommand); the one-shot report (the `report` subcommand or mode: report)
// writes a SeaDex-alignment report and exits; the `poll` subcommand runs one
// cycle; and the `health` subcommand backs the Docker healthcheck. Both `poll`
// and `report` run via `docker exec <container> /seadex-scout <cmd>` while the
// daemon idles.
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

	"github.com/cplieger/health"
	"github.com/cplieger/scheduler"
	"github.com/cplieger/seadex-scout/internal/config"
	"github.com/cplieger/seadex-scout/internal/scout"
)

// exampleConfig is the starter written to CONFIG_PATH on first boot; it is also
// shipped as config.example.yaml in the repo root.
//
//go:embed config.example.yaml
var exampleConfig []byte

// starterFileMode / starterDirMode are applied to a generated starter config.
const (
	starterFileMode = 0o644
	starterDirMode  = 0o755
)

// modePoll is the subcommand-only mode: run one compare cycle for an external
// scheduler (paired with poll_interval: off). Not a valid config `mode`.
const modePoll = "poll"

func main() {
	installLogger()

	// The health subcommand backs the Docker healthcheck and must not require
	// configuration, so it is handled before config load.
	if len(os.Args) > 1 && os.Args[1] == "health" {
		health.RunProbe(health.DefaultPath)
		os.Exit(0)
	}

	configPath := cmp.Or(strings.TrimSpace(os.Getenv("CONFIG_PATH")), config.DefaultConfigPath)
	//nolint:gosec // G703: CONFIG_PATH is an operator-supplied path, not user input
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

	mode, err := resolveMode(os.Args[1:], &cfg)
	if err != nil {
		slog.Error("invalid invocation", "error", err)
		os.Exit(2)
	}
	if err := cfg.Validate(); err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	// Each body lives in a helper so its defers (signal stop, health-marker
	// cleanup, client cleanup) always execute; os.Exit is confined to main so it
	// never skips a pending defer.
	switch mode {
	case config.RunModeReport:
		if err := runReport(&cfg); err != nil {
			slog.Error("report failed", "error", err)
			os.Exit(1)
		}
	case modePoll:
		if err := runPoll(&cfg); err != nil {
			slog.Error("poll failed", "error", err)
			os.Exit(1)
		}
	default:
		if err := run(&cfg); err != nil {
			slog.Error("seadex-scout failed to start", "error", err)
			os.Exit(1)
		}
	}
}

// writeStarterConfig writes the embedded example config to path, creating the
// parent directory, so a fresh deployment gets an editable starter.
func writeStarterConfig(path string) error {
	//nolint:gosec // G703: CONFIG_PATH is an operator-supplied path, not user input
	if err := os.MkdirAll(filepath.Dir(path), starterDirMode); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	//nolint:gosec // G703: CONFIG_PATH is an operator-supplied path, not user input
	if err := os.WriteFile(path, exampleConfig, starterFileMode); err != nil {
		return fmt.Errorf("write starter config: %w", err)
	}
	return nil
}

// resolveMode decides the run mode from the optional subcommand
// (daemon | report | poll) or, with no subcommand, the config's `mode`
// (daemon | report). `poll` runs one compare cycle for an external scheduler
// (used with poll_interval: off). The health subcommand is handled earlier.
func resolveMode(args []string, cfg *config.Config) (mode string, err error) {
	if len(args) == 0 {
		return cfg.RunMode, nil
	}
	switch args[0] {
	case config.RunModeDaemon, config.RunModeReport, modePoll:
		return args[0], nil
	default:
		return "", fmt.Errorf("unknown subcommand %q (valid: health, daemon, report, poll, or no argument)", args[0])
	}
}

// runReport runs the one-shot audit: build components, generate the report,
// emit it to slog, and write the Markdown + JSON files. It never writes state,
// so a one-shot report cannot clobber a running daemon's cache.
func runReport(cfg *config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
	return rep.WriteFiles(ctx, cfg.ReportPath, cfg.ReportJSONPath(), slog.Default())
}

// runPoll runs one compare cycle for an external scheduler (poll_interval: off).
// It updates the health marker to the cycle's outcome, leaving it in place (no
// Cleanup) so the container healthcheck reads the last poll, and exits non-zero
// on an unhealthy cycle so the scheduler (Ofelia job-exec, cron) sees the fail.
func runPoll(cfg *config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	b, err := buildScout(ctx, cfg)
	if err != nil {
		return err
	}
	defer b.cleanup()

	healthy := runCycle(ctx, b.scout)
	health.NewMarker(health.DefaultPath).Set(healthy)
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

	marker := health.NewMarker(health.DefaultPath)
	marker.Set(false)
	defer marker.Cleanup()

	b, err := buildScout(ctx, cfg)
	if err != nil {
		return err
	}
	defer b.cleanup()

	// Resident-idle (poll_interval: off): no internal timer; healthy on boot and
	// cycles are triggered out-of-band via the `poll` subcommand (e.g. an Ofelia
	// job-exec). Matches the fleet scheduler shape (github-scout, rsync, fclones).
	if cfg.PollExternal {
		marker.Set(true)
		slog.Info("seadex-scout started (resident-idle; trigger a cycle with the `poll` subcommand)")
		<-ctx.Done()
		slog.Info("shutdown complete", "cause", context.Cause(ctx))
		return nil
	}

	// Built-in scheduler. Healthy on boot: the first cycle runs as the loop's
	// first iteration (immediately), so a slow first cycle never gates startup
	// health. The marker thereafter reflects each cycle's library-ingest outcome.
	marker.Set(true)
	slog.Info("seadex-scout started", "poll_interval", cfg.PollInterval.String())

	runScheduler(ctx, cfg.PollInterval, b.scout, marker)
	slog.Info("shutdown complete", "cause", context.Cause(ctx))
	return nil
}

// runScheduler runs a cycle on each tick of a POLL_INTERVAL timer with ±10%
// jitter until ctx is cancelled. The first iteration fires immediately so a
// cycle runs promptly on boot; the marker is set to each cycle's health.
func runScheduler(ctx context.Context, interval time.Duration, sc *scout.Scout, marker *health.Marker) {
	scheduler.RunLoop(ctx, func(ctx context.Context) {
		marker.Set(runCycle(ctx, sc))
	}, scheduler.LoopOptions{Interval: interval, FireOnStart: true, Jitter: 0.10})
}

// runCycle runs one cycle, recovering from a panic so a single bad cycle cannot
// crash the long-lived daemon. A panic is reported as an unhealthy cycle.
func runCycle(ctx context.Context, sc *scout.Scout) (healthy bool) {
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
	slog.Info("configuration loaded",
		"sonarr_enabled", cfg.SonarrEnabled(),
		"radarr_enabled", cfg.RadarrEnabled(),
		"poll_interval", pollInterval,
		"seadex_base_url", cfg.SeaDexBaseURL,
		"mapping_refresh", cfg.MappingRefresh.String(),
		"anilist_rate", cfg.AniListRate,
		"allow_remux", cfg.AllowRemux,
		"min_resolution", cfg.MinResolution,
		"require_dual_audio", cfg.RequireDualAudio,
		"season_scoping", cfg.SeasonScoping,
		"tracker_allowlist", len(cfg.Trackers),
		"include_tags", len(cfg.IncludeTags),
		"exclude_tags", len(cfg.ExcludeTags),
		"state_path", cfg.StatePath,
		"run_mode", cfg.RunMode)
}
