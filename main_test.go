package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/arrapi/v3"
	"github.com/cplieger/arrapi/v3/radarr"
	"github.com/cplieger/arrapi/v3/sonarr"
	"github.com/cplieger/atomicfile/v4"
	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/seadex-scout/internal/config"
	"github.com/cplieger/seadex-scout/internal/cycle"
	"github.com/cplieger/seadex-scout/internal/logcontract"
	"github.com/cplieger/seadex-scout/internal/shutdown"
	"github.com/cplieger/slogx"
	"github.com/cplieger/slogx/capture"
	yaml "go.yaml.in/yaml/v3"
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
		if runHealthProbe(args) {
			t.Errorf("runHealthProbe(%v) = true, want false (not the health subcommand)", args)
		}
	}
}

// starterVars are the four environment variables the embedded starter references
// for its connection values.
var starterVars = []string{"SONARR_URL", "SONARR_API_KEY", "RADARR_URL", "RADARR_API_KEY"}

// setStarterEnv leaves exactly the given starter variables set for the test.
func setStarterEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for _, name := range starterVars {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
}

// sonarrEnv completes the starter from the environment; the key is the 32-hex
// shape the arrs generate so validation logs nothing about it.
var sonarrEnv = map[string]string{"SONARR_URL": "http://sonarr:8989", "SONARR_API_KEY": strings.Repeat("4b5a6978", 4)}

// TestLoadRuntimeConfig covers the config-bootstrap sequence main exits 1 on: a
// first boot writes the starter and loads it (marked as the app's own file), a
// starter write failure and a load failure return errors, and a present config
// loads as the operator's.
func TestLoadRuntimeConfig(t *testing.T) {
	t.Run("first boot writes the starter and loads it", func(t *testing.T) {
		setStarterEnv(t, sonarrEnv)
		path := filepath.Join(t.TempDir(), "config.yaml")
		boot, err := loadRuntimeConfig(path)
		if err != nil {
			t.Fatalf("loadRuntimeConfig(missing config) = %v, want nil", err)
		}
		if !boot.starter || boot.path != path {
			t.Errorf("boot = {starter %v, path %q}, want the starter marked at %q", boot.starter, boot.path, path)
		}
		if boot.cfg.SonarrURL != sonarrEnv["SONARR_URL"] || boot.cfg.SonarrAPIKey != sonarrEnv["SONARR_API_KEY"] {
			t.Errorf("sonarr = (%q, %q), want the values read from the environment", boot.cfg.SonarrURL, boot.cfg.SonarrAPIKey)
		}
		got, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("reading starter: %v", readErr)
		}
		if blanked := feedKeyLine.ReplaceAll(got, []byte(`feed_api_key: ""`)); !bytes.Equal(blanked, exampleConfig) {
			t.Errorf("starter differs from the embedded example beyond the generated feed_api_key (%d vs %d bytes)",
				len(blanked), len(exampleConfig))
		}
	})
	t.Run("starter write failure is an error", func(t *testing.T) {
		// A dangling parent symlink makes os.Stat report the config missing
		// while the starter's parent creation fails deterministically for
		// every UID (root-safe, unlike a read-only-dir chmod).
		dir := t.TempDir()
		missingTarget := filepath.Join(dir, "missing-target")
		blockedParent := filepath.Join(dir, "blocked-parent")
		if err := os.Symlink(missingTarget, blockedParent); err != nil {
			t.Fatal(err)
		}
		boot, err := loadRuntimeConfig(filepath.Join(blockedParent, "config.yaml"))
		if err == nil {
			t.Fatal("loadRuntimeConfig(blocked starter path) = nil, want error")
		}
		if boot.starter {
			t.Error("boot.starter = true after a failed write, want false")
		}
		if !strings.Contains(err.Error(), "write starter config") {
			t.Errorf("err = %q, want the starter-write failure, not a config-load failure", err)
		}
	})
	t.Run("present valid config loads as the operator's file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, exampleConfig, 0o600); err != nil {
			t.Fatal(err)
		}
		boot, err := loadRuntimeConfig(path)
		if err != nil {
			t.Fatalf("loadRuntimeConfig(example config) = %v, want nil", err)
		}
		if boot.starter {
			t.Error("boot.starter = true for a present file, want false")
		}
	})
	t.Run("malformed config is a load failure", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("{not yaml"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadRuntimeConfig(path); err == nil {
			t.Fatal("loadRuntimeConfig(malformed config) = nil, want error")
		}
	})
}

// starterHint is the two-remedy tail of the WARN a first boot ends on when the
// environment does not complete the starter.
const starterHint = "set SONARR_URL and SONARR_API_KEY in the container's environment, or edit the file"

// holdReportLock takes the report lock on a fresh directory for the rest of the
// test, so a dispatch into report mode refuses with ErrReportRunning before any
// component build or network I/O: the hermetic proof that the gate let it through.
func holdReportLock(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "reports")
	release, err := cycle.TryReportLock(dir)
	if err != nil {
		t.Fatalf("holding the report lock: %v", err)
	}
	t.Cleanup(release)
	return dir
}

// TestDispatchRejectsInvalidConfig pins the gate between the loaded config and the
// run bodies: an invalid config never reaches run, runReport or runPoll, and the
// error it returns is classified with whether this boot wrote the file, so the
// operator's file fails at ERROR with the plain line and a starter the environment
// did not complete fails at WARN naming both remedies. A valid config passes through, with
// exactly one Info line for a starter and nothing for an operator's file. Serial
// (capture swaps slog.Default).
func TestDispatchRejectsInvalidConfig(t *testing.T) {
	t.Run("an operator's invalid file is refused at ERROR", func(t *testing.T) {
		err := dispatch(config.RunModeReport, &bootConfig{path: "/config/config.yaml"})
		if err == nil {
			t.Fatal("dispatch(report, zero config) = nil, want the validation error")
		}
		if !errors.Is(err, errInvalidConfig) {
			t.Fatalf("dispatch(report, zero config) = %v, want errInvalidConfig in its chain", err)
		}
		if !strings.Contains(err.Error(), "invalid configuration in /config/config.yaml") {
			t.Errorf("err = %q, want it named as an invalid configuration at its path", err)
		}
		level, msg, exit := dispatchOutcome(err, false)
		if level != slog.LevelError || msg != "seadex-scout failed" || exit != 1 {
			t.Errorf("dispatchOutcome = (%v, %q, %d), want (ERROR, seadex-scout failed, 1)", level, msg, exit)
		}
	})
	t.Run("a starter the environment does not complete is refused at WARN with both remedies", func(t *testing.T) {
		setStarterEnv(t, nil)
		path := filepath.Join(t.TempDir(), "config.yaml")
		boot, err := loadRuntimeConfig(path)
		if err != nil {
			t.Fatalf("loadRuntimeConfig: %v", err)
		}
		// Report mode over a held lock: a gate that lets this through returns
		// ErrReportRunning at once instead of parking a daemon with no arrs.
		boot.cfg.ReportDir = holdReportLock(t)
		err = dispatch(config.RunModeReport, &boot)
		if err == nil {
			t.Fatal("dispatch(report, unedited starter) = nil with no variables set, want the no-arr error")
		}
		if !errors.Is(err, errInvalidConfig) {
			t.Fatalf("dispatch(report, unedited starter) = %v, want errInvalidConfig in its chain", err)
		}
		if !boot.starter {
			t.Error("bootConfig.starter = false for the file this boot wrote, want true")
		}
		if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "no arr configured") {
			t.Errorf("err = %q, want it to name the starter path and the no-arr validation error", err)
		}
		level, msg, exit := dispatchOutcome(err, boot.starter)
		if level != slog.LevelWarn || exit != 1 {
			t.Errorf("dispatchOutcome = (%v, %q, %d), want (WARN, the starter line, 1)", level, msg, exit)
		}
		if !strings.Contains(msg, "wrote a starter config, but it cannot start yet") || !strings.Contains(msg, starterHint) {
			t.Errorf("dispatchOutcome msg = %q, want the starter line naming both remedies", msg)
		}
	})
	t.Run("a starter the environment completes starts and says so once", func(t *testing.T) {
		setStarterEnv(t, sonarrEnv)
		rec := capture.Default(t)
		boot, err := loadRuntimeConfig(filepath.Join(t.TempDir(), "config.yaml"))
		if err != nil {
			t.Fatalf("loadRuntimeConfig: %v", err)
		}
		boot.cfg.ReportDir = holdReportLock(t)
		if err := dispatch(config.RunModeReport, &boot); !errors.Is(err, cycle.ErrReportRunning) {
			t.Fatalf("dispatch(report, completed starter) = %v, want ErrReportRunning past the gate", err)
		}
		if n := rec.CountLevel(slog.LevelInfo, "no config found; wrote a starter config and read its connection settings from the environment"); n != 1 {
			t.Errorf("Info starter lines = %d, want exactly 1: %v", n, rec.Messages())
		}
		// The two-variable start is the documented path, so nothing about it warns.
		if n := rec.CountLevel(slog.LevelWarn, "") + rec.CountLevel(slog.LevelError, ""); n != 0 {
			t.Errorf("first boot logged %d WARN/ERROR line(s): %v", n, rec.Messages())
		}
	})
	t.Run("an operator's valid file starts and logs nothing", func(t *testing.T) {
		rec := capture.Default(t)
		boot := bootConfig{path: "/config/config.yaml", cfg: config.Config{
			RunMode: config.RunModeReport, SonarrURL: "http://sonarr:8989", SonarrAPIKey: sonarrEnv["SONARR_API_KEY"],
			ReportDir: holdReportLock(t),
		}}
		if err := dispatch(config.RunModeReport, &boot); !errors.Is(err, cycle.ErrReportRunning) {
			t.Fatalf("dispatch(report, valid operator file) = %v, want ErrReportRunning past the gate", err)
		}
		if msgs := rec.Messages(); len(msgs) != 0 {
			t.Errorf("dispatch logged %v for a valid operator file, want nothing", msgs)
		}
	})
}

// TestArrClientHelpersReturnNilInterface pins the typed-nil guard: passing a
// nil *sonarr.Client/*radarr.Client straight into the interface field would
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
	logConfig(cfg, cfg.RunMode)

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
// the given path (parent directories created), byte-identical to the embedded
// example EXCEPT for the generated feed_api_key.
func TestWriteStarterConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "config.yaml")
	if err := writeStarterConfig(path); err != nil {
		t.Fatalf("writeStarterConfig(%q) = %v, want nil", path, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading starter: %v", err)
	}
	// The only difference from the embedded bytes is the seeded key, so
	// re-blanking it must reproduce the example exactly. That pins BOTH halves:
	// nothing else was rewritten, and the key really was substituted.
	blanked := feedKeyLine.ReplaceAll(got, []byte(`feed_api_key: ""`))
	if !bytes.Equal(blanked, exampleConfig) {
		t.Errorf("starter differs from the embedded example beyond feed_api_key (%d vs %d bytes)",
			len(blanked), len(exampleConfig))
	}
}

// feedKeyLine matches the starter's feed_api_key assignment for the tests that
// need to read or blank the generated value.
var feedKeyLine = regexp.MustCompile(`feed_api_key: "[^"]*"`)

// TestWriteStarterConfigSeedsAStrongFeedKey pins the reason the starter seeds a
// key at all: feed_api_key is the one credential in this config the operator
// invents rather than copies from another service, so a fresh install must not
// be able to start life with a weak or empty one. It asserts the properties that
// matter (present, 32 hex characters, and DIFFERENT on every write) rather than
// any particular value, and that the result still loads - a key the config
// validator would reject would make first boot unrecoverable.
func TestWriteStarterConfigSeedsAStrongFeedKey(t *testing.T) {
	keyOf := func(t *testing.T, path string) string {
		t.Helper()
		if err := writeStarterConfig(path); err != nil {
			t.Fatalf("writeStarterConfig(%q) = %v, want nil", path, err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading starter: %v", err)
		}
		m := feedKeyLine.FindSubmatch(b)
		if m == nil {
			t.Fatalf("starter has no feed_api_key line:\n%s", b)
		}
		return strings.Trim(strings.TrimPrefix(string(m[0]), "feed_api_key: "), `"`)
	}

	dir := t.TempDir()
	first := keyOf(t, filepath.Join(dir, "a.yaml"))
	second := keyOf(t, filepath.Join(dir, "b.yaml"))

	// The key's own shape (feedKeyBytes rendered as hex, regenerated per call)
	// is pinned beside config.SeedStarter, which owns the generation; what the
	// root owns is that the WRITTEN file carries a usable one.
	if _, err := hex.DecodeString(first); err != nil {
		t.Errorf("generated key is not hex: %v", err)
	}
	if first == second {
		t.Error("two starters share a feed_api_key; the key must be generated per write")
	}
	// The generated key must satisfy the validator, including the placeholder
	// refusal and the strength warning's 16-character floor.
	if strings.Contains(first, "${") {
		t.Errorf("generated key looks like an env reference: %d chars", len(first))
	}
	if len(first) < 16 {
		t.Errorf("generated key is %d chars, below the strength floor the config warns at", len(first))
	}
	// The written file must still be valid YAML with the key readable as a
	// string: the substitution quotes the value itself, so a quoting mistake
	// would break the very file first boot tells the operator to edit.
	raw, err := os.ReadFile(filepath.Join(dir, "a.yaml"))
	if err != nil {
		t.Fatalf("reading starter: %v", err)
	}
	var doc struct {
		Indexer struct {
			FeedAPIKey string `yaml:"feed_api_key"`
		} `yaml:"indexer"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("seeded starter is not valid YAML: %v", err)
	}
	if doc.Indexer.FeedAPIKey != first {
		t.Errorf("feed_api_key parsed as %q, want the generated %q", doc.Indexer.FeedAPIKey, first)
	}
}

// TestNewArrClients covers the constructor gate: disabled arrs yield nil
// clients, a valid pair yields a client, and an invalid URL surfaces as an
// error naming the arr. arrapi constructors validate parameters without any
// network I/O, so this is hermetic.
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

// TestArrKeyGateMatchesArrClient pins that the config's arr key gate and the arr
// client agree: a Sonarr or Radarr key Validate accepts is one the client accepts,
// and one Validate refuses the client refuses too, so no configuration passes
// validation and then fails at startup. The client is the oracle.
func TestArrKeyGateMatchesArrClient(t *testing.T) {
	keys := map[string]string{
		"generated":           "0f1e2d3c4b5a69788796a5b4c3d2e1f0",
		"hand-set in the set": "abc.DEF_1-2",
		"plus":                "my+key",
		"tilde":               "a~b",
		"non-ASCII":           "clé",
		"slash":               "a/b",
		"at sign":             "p@ss",
		"dollar":              "a$b",
		"inner space":         "two words",
	}
	for name, key := range keys {
		t.Run(name, func(t *testing.T) {
			cfg := config.Config{
				RunMode:   config.RunModeDaemon,
				SonarrURL: "http://sonarr:8989", SonarrAPIKey: key,
				RadarrURL: "http://radarr:7878", RadarrAPIKey: key,
			}
			validateErr := cfg.Validate()
			s, r, clientErr := newArrClients(&cfg)
			if s != nil {
				s.Close()
			}
			if r != nil {
				r.Close()
			}
			if (validateErr == nil) != (clientErr == nil) {
				t.Errorf("Validate() = %v but newArrClients() = %v; want both nil or both errors", validateErr, clientErr)
			}
		})
	}
}

// flakyArr serves routes plus path, which answers 503 with the detection's
// X-Application-Version (and Retry-After when set) to its first fails requests
// and 200 [] after; the counter is path's request count.
func flakyArr(t *testing.T, routes map[string]arrRoute, path string, fails int32, retryAfter string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var sends atomic.Int32
	version := routes["/api"].version
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == path {
			w.Header().Set("X-Application-Version", version)
			if sends.Add(1) <= fails {
				if retryAfter != "" {
					w.Header().Set("Retry-After", retryAfter)
				}
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = io.WriteString(w, "[]")
			return
		}
		rt, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-Application-Version", rt.version)
		w.WriteHeader(rt.code)
		_, _ = io.WriteString(w, rt.body)
	}))
	t.Cleanup(srv.Close)
	return srv, &sends
}

// TestNewArrClientsPassRetryTuning pins the retry policy newArrClients gives both
// clients, observed on a transient 503 to a data read. The base delay: arrapi
// draws the first backoff from [base/2, base] (httpx.JitteredBackoff), so a 5 s
// base cannot retry inside a 2 s deadline, where arrapi's default 1 s base would
// and would read the 200. The attempt bound: a persistent 503 is sent exactly
// three times; Retry-After: 1 replaces the backoff to keep the case short. Three
// is also arrapi's default, so this pins the count, not the option's presence.
func TestNewArrClientsPassRetryTuning(t *testing.T) {
	tests := []struct {
		routes func() map[string]arrRoute
		read   func(ctx context.Context, s *sonarr.Client, r *radarr.Client) error
		cfg    func(url string) *config.Config
		name   string
		path   string
	}{
		{
			name: "sonarr", routes: sonarr4Routes, path: "/api/v3/series",
			cfg: func(url string) *config.Config { return &config.Config{SonarrURL: url, SonarrAPIKey: "test-key"} },
			read: func(ctx context.Context, s *sonarr.Client, _ *radarr.Client) error {
				_, err := s.Series(ctx)
				return err
			},
		},
		{
			name: "radarr", routes: radarrRoutes, path: "/api/v3/movie",
			cfg: func(url string) *config.Config { return &config.Config{RadarrURL: url, RadarrAPIKey: "test-key"} },
			read: func(ctx context.Context, _ *sonarr.Client, r *radarr.Client) error {
				_, err := r.Movies(ctx)
				return err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			build := func(t *testing.T, url string) (*sonarr.Client, *radarr.Client) {
				t.Helper()
				s, r, err := newArrClients(tc.cfg(url))
				if err != nil {
					t.Fatalf("newArrClients: %v", err)
				}
				t.Cleanup(func() {
					if s != nil {
						s.Close()
					}
					if r != nil {
						r.Close()
					}
				})
				return s, r
			}
			t.Run("base delay outlasts a 2s deadline", func(t *testing.T) {
				t.Parallel()
				srv, sends := flakyArr(t, tc.routes(), tc.path, 1, "")
				s, r := build(t, srv.URL)
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()
				err := tc.read(ctx, s, r)
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("read = %v, want the deadline to expire during the 5 s base backoff", err)
				}
				if n := sends.Load(); n != 1 {
					t.Errorf("%s sends = %d, want 1: no retry fits before the deadline", tc.path, n)
				}
			})
			t.Run("a persistent 503 is sent three times", func(t *testing.T) {
				t.Parallel()
				srv, sends := flakyArr(t, tc.routes(), tc.path, 1<<20, "1")
				s, r := build(t, srv.URL)
				if err := tc.read(t.Context(), s, r); err == nil {
					t.Error("read = nil, want the 503 after the attempts are spent")
				}
				if n := sends.Load(); n != 3 {
					t.Errorf("%s sends = %d, want 3", tc.path, n)
				}
			})
		})
	}
}

// TestConfigureLoggerAppliesLevel pins the configured level onto the default
// logger. Serial: mutates slog.Default.
func TestConfigureLoggerAppliesLevel(t *testing.T) {
	capture.SwapDefault(t, slog.Default())

	configureLogger(slog.LevelWarn, slogx.JSON)
	ctx := t.Context()
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
// configureLogger applies the configured level. Serial: mutates slog.Default.
func TestInstallLoggerInitialLevel(t *testing.T) {
	capture.SwapDefault(t, slog.Default())

	installLogger()
	ctx := t.Context()
	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		t.Error("Debug enabled before config is read, want the documented Info floor")
	}
	if !slog.Default().Enabled(ctx, slog.LevelInfo) {
		t.Error("Info disabled before config is read, want enabled (config-parse warnings must emit)")
	}
}

// TestFeedWriter pins the nil-when-unconfigured contract through the real
// composition-root gate: the compare cycle does feed work iff at least one
// Prowlarr Torznab URL is configured, and the returned cleanup is a callable
// no-op when it is not. Driving feedWriter (rather than asserting
// config.IndexerConfigured directly, which the config package owns) is what
// makes an inverted or dropped guard in the root fail here.
func TestFeedWriter(t *testing.T) {
	tests := []struct {
		name    string
		nyaaURL string
		abURL   string
		wantNil bool
	}{
		{name: "both empty skips feed work", wantNil: true},
		{name: "Nyaa URL enables feed work", nyaaURL: "http://prowlarr:9696/22/api"},
		{name: "AnimeBytes URL enables feed work", abURL: "http://prowlarr:9696/2/api"},
		{name: "both URLs enable feed work", nyaaURL: "http://prowlarr:9696/22/api", abURL: "http://prowlarr:9696/2/api"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				IndexerNyaaTorznabURL: tt.nyaaURL,
				IndexerABTorznabURL:   tt.abURL,
			}
			fw, cleanup := feedWriter(cfg, slog.Default(), nil)
			t.Cleanup(cleanup)
			if gotNil := fw == nil; gotNil != tt.wantNil {
				t.Errorf("feedWriter(nyaa=%q, ab=%q) nil = %v, want %v", tt.nyaaURL, tt.abURL, gotNil, tt.wantNil)
			}
		})
	}
}

func prowlarrRedirect(t *testing.T, target func(sinkURL string) string) (sinkKey, finalKey string, err error) {
	t.Helper()
	sinkKeys := make(chan string, 1)
	sink := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		sinkKeys <- r.Header.Get("X-Api-Key")
	}))
	defer sink.Close()
	finalKeys := make(chan string, 1)
	prowlarr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/final" {
			finalKeys <- r.Header.Get("X-Api-Key")
			return
		}
		http.Redirect(w, r, target(sink.URL), http.StatusFound)
	}))
	defer prowlarr.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, prowlarr.URL+"/api", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", "prowlarr-key")
	resp, err := newProwlarrClient().Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	select {
	case sinkKey = <-sinkKeys:
	default:
	}
	select {
	case finalKey = <-finalKeys:
	default:
	}
	return sinkKey, finalKey, err
}

// TestProwlarrClientRefusesAnotherOrigin pins that the X-Api-Key header, which
// net/http forwards on every followed hop, never leaves the Prowlarr origin.
func TestProwlarrClientRefusesAnotherOrigin(t *testing.T) {
	tests := []struct {
		name   string
		target func(sinkURL string) string
	}{
		{name: "another port on the same host", target: func(sinkURL string) string { return sinkURL }},
		{name: "another spelling of the host", target: func(sinkURL string) string {
			return strings.Replace(sinkURL, "127.0.0.1", "localhost", 1)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sinkKey, _, err := prowlarrRedirect(t, tt.target)
			if refused, ok := errors.AsType[*httpx.RedirectRefusedError](err); !ok || refused.Reason != httpx.RedirectOtherOrigin {
				t.Errorf("redirect error = %v, want a RedirectOtherOrigin refusal", err)
			}
			if sinkKey != "" {
				t.Errorf("redirect target received X-Api-Key %q, want no request", sinkKey)
			}
		})
	}
}

// TestProwlarrClientFollowsItsOrigin pins that the policy still follows a
// redirect within the Prowlarr origin, rather than refusing every hop.
func TestProwlarrClientFollowsItsOrigin(t *testing.T) {
	_, finalKey, err := prowlarrRedirect(t, func(string) string { return "/final" })
	if err != nil {
		t.Fatalf("same-origin redirect: %v, want it followed", err)
	}
	if finalKey != "prowlarr-key" {
		t.Errorf("same-origin target X-Api-Key = %q, want %q", finalKey, "prowlarr-key")
	}
}

// RFC 4007 section 11.2 leaves zone-ID representation implementation-defined, so
// httpx compares it byte-for-byte and treats a case change as another origin.
func TestProwlarrClientComparesIPv6ZonesExactly(t *testing.T) {
	via, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://[fe80::1%25eth0]:9696/api", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://[fe80::1%25ETH0]:9696/api", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	err = newProwlarrClient().CheckRedirect(req, []*http.Request{via})
	if refused, ok := errors.AsType[*httpx.RedirectRefusedError](err); !ok || refused.Reason != httpx.RedirectOtherOrigin {
		t.Errorf("redirect to a zone differing only in case: %v, want a RedirectOtherOrigin refusal", err)
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
	logConfig(cfg, cfg.RunMode)

	if rec.AttrContains("configuration loaded", "", "leaked-secret-value-9") {
		t.Errorf("startup config log leaks the raw run_mode value: %v", rec.Records())
	}
	if !rec.HasAttr("configuration loaded", "run_mode", unknownModeMarker) {
		t.Errorf("run_mode not logged as the fixed marker %q: %v", unknownModeMarker, rec.Records())
	}
}

// TestLogConfigLogsResolvedMode pins the contract the resolved-mode
// parameter exists for: logConfig renders the mode it is PASSED, not the
// config key. The documented one-shot `report`/`poll` container leaves
// `mode: daemon` in config.yaml, so an operator filtering that container's
// Loki stream on run_mode must read the mode the process actually runs.
// Serial (swaps slog.Default).
func TestLogConfigLogsResolvedMode(t *testing.T) {
	rec := capture.Default(t)

	cfg := &config.Config{RunMode: config.RunModeDaemon}
	logConfig(cfg, modePoll)

	if !rec.HasAttr("configuration loaded", "run_mode", modePoll) {
		t.Errorf("run_mode not logged as the resolved mode %q: %v", modePoll, rec.Records())
	}
}

// TestLoggableModeMasksUnknownMode pins the redaction contract main applies at
// its terminal log sites: loggableMode passes the known run modes through and
// maps anything else (which may be an expanded ${VAR} secret placed by a config
// typo) to the fixed marker "invalid", so the dispatch-failure lines never echo
// the raw value.
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
}

// TestLogConfigExternalPollInterval pins the resident-idle rendering: with
// poll_interval off (PollExternal), the startup line reports "external", not a
// zero duration. Serial (swaps slog.Default).
func TestLogConfigExternalPollInterval(t *testing.T) {
	rec := capture.Default(t)

	cfg := &config.Config{PollExternal: true, RunMode: config.RunModeDaemon}
	logConfig(cfg, cfg.RunMode)

	// The library's attr assertion compares the RENDERED value, so it pins the
	// attribute itself rather than a substring of the serialized JSON: a
	// coincidental match elsewhere in the line cannot satisfy it.
	if !rec.HasAttr("configuration loaded", "poll_interval", "external") {
		t.Errorf("poll_interval not rendered as external: %v", rec.Messages())
	}
}

// TestArrClientHelpersPassThrough pins the other half of the typed-nil guard:
// a real client must pass through as a non-nil interface, otherwise the walker
// would treat an enabled arr as disabled.
func TestArrClientHelpersPassThrough(t *testing.T) {
	s, err := sonarr.New("http://sonarr:8989", "k1")
	if err != nil {
		t.Fatalf("sonarr.New: %v", err)
	}
	defer s.Close()
	if got := sonarrClient(s); got == nil {
		t.Error("sonarrClient(non-nil) = nil, want the client as a non-nil interface")
	}
	r, err := radarr.New("http://radarr:7878", "k2")
	if err != nil {
		t.Fatalf("radarr.New: %v", err)
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
// disabled each role's component graph builds without any network I/O (reportArrs
// is a no-op on nil clients) and cleanup is callable, and an invalid arr URL
// propagates as a build error instead of being swallowed.
func TestBuildScout(t *testing.T) {
	t.Run("disabled arrs build hermetically", func(t *testing.T) {
		b, err := buildScout(t.Context(), &config.Config{}, nil)
		if err != nil {
			t.Fatalf("buildScout(zero config) = %v, want nil", err)
		}
		if b.scout == nil {
			t.Fatal("scout = nil, want a wired scout")
		}
		b.cleanup()
	})
	t.Run("reporter role builds hermetically", func(t *testing.T) {
		b, err := buildReporter(t.Context(), &config.Config{})
		if err != nil {
			t.Fatalf("buildReporter(zero config) = %v, want nil", err)
		}
		if b.reporter == nil {
			t.Fatal("reporter = nil, want a wired reporter")
		}
		b.cleanup()
	})
	t.Run("invalid sonarr URL propagates", func(t *testing.T) {
		cfg := &config.Config{SonarrURL: "not-a-url", SonarrAPIKey: "k"}
		if _, err := buildScout(t.Context(), cfg, nil); err == nil {
			t.Fatal("buildScout(invalid sonarr URL) = nil, want error")
		}
		if _, err := buildReporter(t.Context(), cfg); err == nil {
			t.Fatal("buildReporter(invalid sonarr URL) = nil, want error")
		}
	})
}

// arrRoute is one canned answer of a fake arr: the status, the
// X-Application-Version header when version is set, and the body.
type arrRoute struct {
	version string
	body    string
	code    int
}

// newFakeArr serves routes by request path; any other path answers 404 with no
// version header.
func newFakeArr(t *testing.T, routes map[string]arrRoute) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rt, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if rt.version != "" {
			w.Header().Set("X-Application-Version", rt.version)
		}
		w.WriteHeader(rt.code)
		_, _ = io.WriteString(w, rt.body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The detection answers below follow recorded live GET /api and system/status
// answers of Sonarr 4.0.20.3014, Sonarr 5.0.0.2635 and Radarr 6.4.4.10685; a
// status body keeps appName and version.
func sonarr4Routes() map[string]arrRoute {
	const v = "4.0.20.3014"
	return map[string]arrRoute{
		"/api":                  {code: 200, version: v, body: `{"current":"v3","deprecated":[]}`},
		"/api/v3/system/status": {code: 200, version: v, body: `{"appName":"Sonarr","version":"` + v + `"}`},
	}
}

func sonarr5Routes() map[string]arrRoute {
	const v = "5.0.0.2635"
	return map[string]arrRoute{
		"/api":                  {code: 200, version: v, body: `{"current":"v5","deprecated":["v3"]}`},
		"/api/v5/system/status": {code: 200, version: v, body: `{"appName":"Sonarr","version":"` + v + `"}`},
	}
}

func radarrRoutes() map[string]arrRoute {
	const v = "6.4.4.10685"
	return map[string]arrRoute{
		"/api":                  {code: 200, version: v, body: `{"current":"v3","deprecated":[]}`},
		"/api/v3/system/status": {code: 200, version: v, body: `{"appName":"Radarr","version":"` + v + `"}`},
	}
}

func newTestSonarr(t *testing.T, url string) *sonarr.Client {
	t.Helper()
	c, err := sonarr.New(url, "test-key", sonarr.WithMaxAttempts(1))
	if err != nil {
		t.Fatalf("sonarr.New: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

func newTestRadarr(t *testing.T, url string) *radarr.Client {
	t.Helper()
	c, err := radarr.New(url, "test-key", radarr.WithMaxAttempts(1))
	if err != nil {
		t.Fatalf("radarr.New: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// reachableAttrKeys returns the sorted attribute keys of every record whose
// message is msg.
func reachableAttrKeys(rec *capture.Recorder, msg string) []string {
	var keys []string
	for _, r := range rec.Records() {
		if r.Message != msg {
			continue
		}
		r.Attrs(func(a slog.Attr) bool {
			keys = append(keys, a.Key)
			return true
		})
	}
	slices.Sort(keys)
	return keys
}

// TestReportArrsLogsAPIAndFeatures pins the startup line: each
// reachable arr logs "<arr> reachable" with its version, API generation and
// deprecation, and Sonarr also its four features, valued per engine as
// arrapi documents them, and no other attribute. Serial (swaps slog.Default).
func TestReportArrsLogsAPIAndFeatures(t *testing.T) {
	tests := []struct {
		routes func() map[string]arrRoute
		want   map[string]string
		name   string
	}{
		{
			name:   "Sonarr v4 (API v3)",
			routes: sonarr4Routes,
			want: map[string]string{
				"version": "4.0.20.3014", "api": "v3", "deprecated": "false",
				"series_orders": "false", "season_titles": "false", "translations": "false",
				"anime_ids": "false",
			},
		},
		{
			name:   "Sonarr v5 (API v5)",
			routes: sonarr5Routes,
			want: map[string]string{
				"version": "5.0.0.2635", "api": "v5", "deprecated": "false",
				"series_orders": "true", "season_titles": "true", "translations": "true",
				"anime_ids": "true",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := capture.Default(t)
			reportArrs(t.Context(), newTestSonarr(t, newFakeArr(t, tc.routes()).URL), nil)

			if n := rec.CountExact("sonarr reachable"); n != 1 {
				t.Fatalf("sonarr reachable lines = %d, want 1; messages = %q", n, rec.Messages())
			}
			for key, want := range tc.want {
				if got, _ := rec.AttrValueExact("sonarr reachable", key); got != want {
					t.Errorf("sonarr reachable %s = %q, want %q", key, got, want)
				}
			}
			if v, _ := rec.Attr("sonarr reachable", "api"); v.Kind() != slog.KindString {
				t.Errorf("api attr kind = %v, want a string", v.Kind())
			}
			if got, want := reachableAttrKeys(rec, "sonarr reachable"), slices.Sorted(maps.Keys(tc.want)); !slices.Equal(got, want) {
				t.Errorf("sonarr reachable attr keys = %q, want %q", got, want)
			}
		})
	}
	t.Run("Radarr (API v3)", func(t *testing.T) {
		rec := capture.Default(t)
		reportArrs(t.Context(), nil, newTestRadarr(t, newFakeArr(t, radarrRoutes()).URL))

		if n := rec.CountExact("radarr reachable"); n != 1 {
			t.Fatalf("radarr reachable lines = %d, want 1; messages = %q", n, rec.Messages())
		}
		for key, want := range map[string]string{"version": "6.4.4.10685", "api": "v3", "deprecated": "false"} {
			if got, _ := rec.AttrValueExact("radarr reachable", key); got != want {
				t.Errorf("radarr reachable %s = %q, want %q", key, got, want)
			}
		}
	})
}

// TestSonarrReachableAttrsNamesEachFeature pins each feature attribute to its
// own sonarr.Features flag: both engines report SeriesOrders with SeasonTitles
// and Translations with AnimeIDs (arrapi's Features), so only a one-hot Features
// value tells a swapped pair apart.
func TestSonarrReachableAttrsNamesEachFeature(t *testing.T) {
	flags := map[string]sonarr.Features{
		"series_orders": {SeriesOrders: true},
		"season_titles": {SeasonTitles: true},
		"translations":  {Translations: true},
		"anime_ids":     {AnimeIDs: true},
	}
	for set, features := range flags {
		t.Run(set, func(t *testing.T) {
			attrs := sonarrReachableAttrs(&sonarr.Info{Features: features})
			got := map[string]any{}
			for i := 0; i+1 < len(attrs); i += 2 {
				got[attrs[i].(string)] = attrs[i+1]
			}
			for key := range flags {
				if want := key == set; got[key] != want {
					t.Errorf("%s = %v, want %v", key, got[key], want)
				}
			}
		})
	}
}

// TestReportArrsUnsupportedAPIIsOneError pins the unsupported arm:
// a server arrapi refuses with an *arrapi.UnsupportedAPIError, whatever the
// reason, logs one ERROR "<arr> API unsupported" carrying the error's own
// one-line text, and no reachable or ping-failed line; the message is a
// log-contract row of docs/monitoring.md. Each fake reproduces
// the detection arm arrapi's internal/detect tests drive for that reason; the
// test first confirms the fake yields it. Serial (swaps slog.Default).
func TestReportArrsUnsupportedAPIIsOneError(t *testing.T) {
	contract, err := logcontract.Load(".")
	if err != nil {
		t.Fatalf("load the log contract: %v", err)
	}
	html := `<!doctype html><html><body>Sonarr</body></html>`
	tests := []struct {
		routes map[string]arrRoute
		name   string
		arr    string
		reason arrapi.UnsupportedReason
	}{
		{
			name: "Sonarr 5 with /api/v5 blocked", arr: "sonarr", reason: arrapi.ReasonV5Unreachable,
			routes: map[string]arrRoute{
				"/api":                  sonarr5Routes()["/api"],
				"/api/v5/system/status": {code: 404, body: "blocked"},
			},
		},
		{
			// Never Sonarr 5 over /api/v3: /api and /api/v5 hidden, only the v3 status answers.
			name: "Sonarr 5 reachable only over /api/v3", arr: "sonarr", reason: arrapi.ReasonV5Unreachable,
			routes: map[string]arrRoute{
				"/api/v3/system/status": {code: 200, version: "5.0.0.2635", body: `{"appName":"Sonarr","version":"5.0.0.2635"}`},
			},
		},
		{
			name: "Sonarr 3", arr: "sonarr", reason: arrapi.ReasonUnsupportedVersion,
			routes: map[string]arrRoute{
				"/api":                  {code: 200, version: "3.0.10.1567", body: `{"current":"v3","deprecated":[]}`},
				"/api/v3/system/status": {code: 200, version: "3.0.10.1567", body: `{"appName":"Sonarr","version":"3.0.10.1567"}`},
			},
		},
		{
			name: "a Sonarr announcing only API v6", arr: "sonarr", reason: arrapi.ReasonUnknownGeneration,
			routes: map[string]arrRoute{
				"/api":                  {code: 200, version: "6.0.0.1", body: `{"current":"v6","deprecated":[]}`},
				"/api/v6/system/status": {code: 200, version: "6.0.0.1", body: `{"appName":"Sonarr","version":"6.0.0.1"}`},
			},
		},
		{name: "Radarr behind the Sonarr URL", arr: "sonarr", reason: arrapi.ReasonWrongApp, routes: radarrRoutes()},
		{name: "Sonarr behind the Radarr URL", arr: "radarr", reason: arrapi.ReasonWrongApp, routes: sonarr4Routes()},
		{
			name: "a web page at every status path", arr: "sonarr", reason: arrapi.ReasonNotAPI,
			routes: map[string]arrRoute{
				"/api":                  {code: 200, body: html},
				"/api/v5/system/status": {code: 200, body: html},
				"/api/v3/system/status": {code: 200, body: html},
			},
		},
		{
			name: "a proxy stripping X-Application-Version", arr: "sonarr", reason: arrapi.ReasonNoVersionHeader,
			routes: map[string]arrRoute{
				"/api":                  {code: 200, body: `{"current":"v3","deprecated":[]}`},
				"/api/v3/system/status": {code: 200, body: `{"appName":"Sonarr","version":"4.0.20.3014"}`},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			url := newFakeArr(t, tc.routes).URL
			var sc *sonarr.Client
			var rc *radarr.Client
			var err error
			if tc.arr == "sonarr" {
				sc = newTestSonarr(t, url)
				_, err = sc.Info(t.Context())
			} else {
				rc = newTestRadarr(t, url)
				_, err = rc.Info(t.Context())
			}
			unsupported, ok := errors.AsType[*arrapi.UnsupportedAPIError](err)
			if !ok || unsupported.Reason != tc.reason {
				t.Fatalf("fake's Info error = %v, want an *arrapi.UnsupportedAPIError with reason %d", err, tc.reason)
			}

			rec := capture.Default(t)
			reportArrs(t.Context(), sc, rc)

			msg := tc.arr + " API unsupported"
			if n := rec.CountLevel(slog.LevelError, msg); n != 1 {
				t.Fatalf("%q ERROR lines = %d, want 1; messages = %q", msg, n, rec.Messages())
			}
			if got, _ := rec.AttrValueExact(msg, "error"); got != err.Error() || strings.ContainsAny(got, "\r\n") {
				t.Errorf("error attr = %q, want the error's one-line text %q", got, err.Error())
			}
			if rec.Contains(tc.arr+" reachable") || rec.Contains(tc.arr+" ping failed at startup") {
				t.Errorf("messages = %q, want no reachable or ping-failed line beside the ERROR", rec.Messages())
			}
			if !slices.Contains(contract.Messages[msg], "error") {
				t.Errorf("%s lists %q with attributes %q, want a log-contract row carrying error", logcontract.Path, msg, contract.Messages[msg])
			}
		})
	}
}

// TestReportArrsUnreachableArrWarns pins the transient arm: an arr that answers
// 500 everywhere logs the WARN ping-failed line, never the ERROR reserved for an
// unsupported API, and startup goes on. Serial (swaps slog.Default).
func TestReportArrsUnreachableArrWarns(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(down.Close)
	rec := capture.Default(t)

	reportArrs(t.Context(), newTestSonarr(t, down.URL), newTestRadarr(t, down.URL))

	for _, arr := range []string{"sonarr", "radarr"} {
		if n := rec.CountLevel(slog.LevelWarn, arr+" ping failed at startup"); n != 1 {
			t.Errorf("%s ping-failed WARN lines = %d, want 1; messages = %q", arr, n, rec.Messages())
		}
		if rec.Contains(arr + " API unsupported") {
			t.Errorf("%s logged the unsupported-API ERROR for a 500", arr)
		}
	}
}

// TestRunReportRefusesWhenLockHeld pins the report concurrency refusal end to
// end: with another run holding the report lock, runReport returns
// ErrReportRunning before building any component, so the refusal is
// hermetic (no network I/O) and two reports can never race onto the same
// timestamped filename pair.
func TestRunReportRefusesWhenLockHeld(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "reports")
	release, err := cycle.TryReportLock(dir)
	if err != nil {
		t.Fatalf("holding the report lock: %v", err)
	}
	defer release()

	err = runReport(&config.Config{ReportDir: dir})
	if !errors.Is(err, cycle.ErrReportRunning) {
		t.Fatalf("runReport with the lock held = %v, want ErrReportRunning", err)
	}
}

// TestRunReportRejectsRelativeReportDir pins the report-path guard on report.dir:
// every report write goes through an absolute-path-only writer, so a relative value
// cannot produce either half of the pair and the run fails before it spends the
// ~25m walk. Config still admits the value at load (a daemon never writes a
// report), so this is the only place the outcome changes. Hermetic: the refusal
// precedes the report lock and every component build, and the error names the key
// without echoing the secret-capable value.
func TestRunReportRejectsRelativeReportDir(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dir     string
		wantErr bool
	}{
		{"relative", "rel-report-dir", true},
		{"empty", "", true},
		{"dot-relative", "./reports", true},
		{"absolute", filepath.Join(t.TempDir(), "reports"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkReportDir(tc.dir)
			if (err != nil) != tc.wantErr {
				t.Fatalf("checkReportDir(%q) = %v, wantErr %v", tc.dir, err, tc.wantErr)
			}
		})
	}

	err := runReport(&config.Config{ReportDir: "rel-report-dir"})
	if err == nil {
		t.Fatal("runReport with a relative report.dir = nil, want an error")
	}
	if strings.Contains(err.Error(), "rel-report-dir") {
		t.Errorf("report.dir error echoes the configured value: %v", err)
	}
	if _, statErr := os.Stat("rel-report-dir"); statErr == nil {
		t.Error("runReport created the relative report dir before refusing")
	}
}

// TestRunReportProbesReportDirBeforeTheWalk pins that runReport refuses a
// report.dir the pair's owner-only writes would fail on BEFORE it builds anything.
// The probe is stubbed with the ACL-widening refusal, which root cannot provoke on
// a test filesystem. Serial (swaps probeWritable).
func TestRunReportProbesReportDirBeforeTheWalk(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "reports")
	probed := false
	probeWritable = func(_ context.Context, d string, _ ...atomicfile.Option) (atomicfile.ProbeResult, error) {
		probed = true
		return atomicfile.ProbeResult{
			Dir:   d,
			Stage: atomicfile.ProbeStageCreate,
			Err:   fmt.Errorf("%w: %s/.atomicfile-1.tmp: asked 0600, stored 0670", atomicfile.ErrModeNotStored, d),
		}, nil
	}
	t.Cleanup(func() { probeWritable = atomicfile.ProbeWritable })

	err := runReport(&config.Config{ReportDir: dir})
	if !probed {
		t.Fatalf("runReport(%q) never probed report.dir; err = %v", dir, err)
	}
	if !errors.Is(err, atomicfile.ErrModeNotStored) {
		t.Fatalf("runReport(%q) = %v, want the probe's ErrModeNotStored", dir, err)
	}
	for _, want := range []string{"report.dir", "ACL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("runReport(%q) error = %q, want it to name %q", dir, err, want)
		}
	}
	if strings.Contains(err.Error(), dir) {
		t.Errorf("report.dir probe error echoes the configured value: %v", err)
	}
}

// TestProbeReportDirAsksForOwnerOnlyMode pins the probe's mode intent. An
// Option is opaque, so the options the probe receives are replayed through a
// real write and the file it leaves is what they mean. Serial (swaps
// probeWritable).
func TestProbeReportDirAsksForOwnerOnlyMode(t *testing.T) {
	var got []atomicfile.Option
	probeWritable = func(_ context.Context, d string, opts ...atomicfile.Option) (atomicfile.ProbeResult, error) {
		got = opts
		return atomicfile.ProbeResult{Dir: d}, nil
	}
	t.Cleanup(func() { probeWritable = atomicfile.ProbeWritable })

	if err := probeReportDir(t.Context(), t.TempDir()); err != nil {
		t.Fatalf("probeReportDir() = %v, want nil", err)
	}
	replay := filepath.Join(t.TempDir(), "replay")
	if _, err := atomicfile.WriteFile(t.Context(), replay, []byte("x"), got...); err != nil {
		t.Fatalf("Setup: WriteFile(replay) with the probe's options: %v", err)
	}
	info, err := os.Stat(replay)
	if err != nil {
		t.Fatalf("Setup: Stat(replay): %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("probeReportDir probed with options that write mode %v, want -rw-------: the probe "+
			"must ask for the mode the report pair is written at", perm)
	}
}

// TestProbeReportDirAcceptsWritableDir pins the real probe's pass path: a
// writable report.dir passes and the probe leaves nothing behind in it.
func TestProbeReportDirAcceptsWritableDir(t *testing.T) {
	dir := t.TempDir()
	if err := probeReportDir(t.Context(), dir); err != nil {
		t.Fatalf("probeReportDir(%q) = %v, want nil", dir, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	if len(entries) != 0 {
		t.Errorf("probeReportDir(%q) left %d entries behind, want 0", dir, len(entries))
	}
}

// TestLogArrStartupFailureClassifies pins logArrStartupFailure's levels: a
// cancelled startup check is a routine shutdown (DEBUG), an unsupported API
// found anywhere in the chain is the ERROR an operator must fix, and any other
// failure is the WARN ping-failed line. Serial (swaps slog.Default).
func TestLogArrStartupFailureClassifies(t *testing.T) {
	tests := []struct {
		err   error
		name  string
		msg   string
		level slog.Level
	}{
		{
			name: "cancelled by shutdown", err: fmt.Errorf("ping: %w", context.Canceled),
			level: slog.LevelDebug, msg: "sonarr startup ping cancelled by shutdown",
		},
		{
			name: "a wrapped unsupported API", err: fmt.Errorf("info: %w", &arrapi.UnsupportedAPIError{Reason: arrapi.ReasonWrongApp, App: "Radarr", Want: "Sonarr"}),
			level: slog.LevelError, msg: "sonarr API unsupported",
		},
		{
			name: "a server error", err: &arrapi.StatusError{Path: "/api", Code: 503},
			level: slog.LevelWarn, msg: "sonarr ping failed at startup",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := capture.Default(t)

			logArrStartupFailure("sonarr", tc.err)

			records := rec.Records()
			if len(records) != 1 {
				t.Fatalf("captured %d records, want 1 (%v)", len(records), rec.Messages())
			}
			if records[0].Level != tc.level || records[0].Message != tc.msg {
				t.Errorf("record = %v %q, want %v %q", records[0].Level, records[0].Message, tc.level, tc.msg)
			}
		})
	}
}

// TestDispatchRoutesReportMode pins the mode-routing switch: a valid
// report-mode config reaches runReport (proved hermetically by holding the
// report lock first, so runReport refuses with ErrReportRunning before any
// network I/O).
func TestDispatchRoutesReportMode(t *testing.T) {
	boot := bootConfig{path: "/config/config.yaml", cfg: config.Config{
		RunMode:   config.RunModeReport,
		SonarrURL: "http://sonarr:8989", SonarrAPIKey: "k",
		ReportDir: holdReportLock(t),
	}}
	err := dispatch(config.RunModeReport, &boot)
	if !errors.Is(err, cycle.ErrReportRunning) {
		t.Fatalf("dispatch(report, valid config) = %v, want ErrReportRunning", err)
	}
}

// TestRunReportReleasesLockOnBuildFailure pins the deferred lock release: a
// failed buildReporter (invalid sonarr URL, no network I/O) must not leak the
// report lock, or every subsequent report would refuse with ErrReportRunning
// until restart.
func TestRunReportReleasesLockOnBuildFailure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "reports")
	cfg := &config.Config{ReportDir: dir, SonarrURL: "not-a-url", SonarrAPIKey: "k"}

	err := runReport(cfg)
	if err == nil {
		t.Fatal("runReport(invalid sonarr URL) = nil, want a build error")
	}
	if errors.Is(err, cycle.ErrReportRunning) {
		t.Fatalf("err = %v, want a build error, not a lock refusal", err)
	}

	release, err := cycle.TryReportLock(dir)
	if err != nil {
		t.Fatalf("report lock still held after the failed run: %v", err)
	}
	release()
}

// TestBuildIndexer pins the Torznab feed server wiring hermetically: a
// configured feed builds a non-nil server (New is pure assembly, so nothing is
// loaded yet) and cleanup is callable; an unconfigured one builds nothing, which
// is the socket-less posture and what makes the compare cycle's in-process
// handover nil.
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

	unconfigured := buildIndexer(&config.Config{})
	if unconfigured.indexer != nil {
		t.Error("indexer != nil for an unconfigured feed, want none built (the daemon binds no HTTP port)")
	}
	unconfigured.cleanup()
}

// TestStartIndexerUnconfiguredIsNoOp pins the socket-less contract: with no
// Prowlarr Torznab URL configured, startIndexer builds no indexer and starts
// no goroutine (no log record from an indexer Supervise/stop path), and the
// returned stop func returns immediately instead of waiting on a goroutine.
// Serial (capture swaps slog.Default).
func TestStartIndexerUnconfiguredIsNoOp(t *testing.T) {
	rec := capture.Default(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	stop := startIndexer(ctx, buildIndexer(&config.Config{}))
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

// TestStartIndexerLogsRunErrorAndStops pins the configured half of startIndexer:
// the feed goroutine is launched, a Supervise failure is logged as the component=indexer
// ERROR fault line, and the returned stop func waits for the goroutine rather than
// deadlocking or returning before the record is written. The failure used is
// the indexer's fail-closed refusal on an empty feed_api_key, which returns before
// any port bind and before every context check, so the message is stable even if
// stop's cancel wins the race. Serial: capture swaps slog.Default.
func TestStartIndexerLogsRunErrorAndStops(t *testing.T) {
	rec := capture.Default(t)
	ctx := t.Context()

	cfg := &config.Config{IndexerNyaaTorznabURL: "http://prowlarr:9696/22/api"}
	stop := startIndexer(ctx, buildIndexer(cfg))
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

// TestDispatchOutcome pins the two operator-visible contracts main derives from
// a dispatch error: the slog level the cycle-error Loki alert keys on
// (level=ERROR) and the exit code a scheduler reads. The self-heal rule decides
// both - a designed outcome or a routine shutdown is a WARN, and only a refused
// concurrent report (which owes nothing, because the run holding the lock is
// producing the report) exits 0.
func TestDispatchOutcome(t *testing.T) {
	for name, tc := range map[string]struct {
		err       error
		starter   bool
		wantLevel slog.Level
		wantMsg   string
		wantExit  int
	}{
		"starter the environment did not complete": {
			err:       fmt.Errorf("%w in /config/config.yaml: no arr configured", errInvalidConfig),
			starter:   true,
			wantLevel: slog.LevelWarn,
			wantMsg:   "no config found; wrote a starter config, but it cannot start yet: " + starterHint + ", then restart",
			wantExit:  1,
		},
		"operator's invalid file": {
			err:       fmt.Errorf("%w in /config/config.yaml: no arr configured", errInvalidConfig),
			wantLevel: slog.LevelError,
			wantMsg:   "seadex-scout failed",
			wantExit:  1,
		},
		"refused concurrent report": {
			err:       fmt.Errorf("acquire report lock: %w", cycle.ErrReportRunning),
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
			level, msg, exit := dispatchOutcome(tc.err, tc.starter)
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

// TestWatchdogLeaseCoversAColdReconcileAtTheShippedCadence pins the one half of
// the wedge lease that is the ROOT's: run() arms it from cfg.PollInterval, so
// the app's own default cadence has to yield a lease a cold reconcile can finish
// inside. The lease arithmetic and the floor's measured value are pinned beside
// the mechanism, in internal/cycle's watchdog tests.
func TestWatchdogLeaseCoversAColdReconcileAtTheShippedCadence(t *testing.T) {
	// measuredColdReconcile is the observed worst case on a large library (the
	// typical cold pass is ~25 minutes). At the shipped default the lease has to
	// survive it, or the watchdog demands the restart that kills the walk before
	// it can persist the AniList memo - which makes the next boot cold again.
	const measuredColdReconcile = 2 * time.Hour
	if got := cycle.WatchdogLease(config.DefaultPollInterval); got <= measuredColdReconcile {
		t.Errorf("cycle.WatchdogLease(DefaultPollInterval=%v) = %v, want more than the measured %v cold reconcile",
			config.DefaultPollInterval, got, measuredColdReconcile)
	}
}

// TestDetachedWriteError pins the alert-facing exit classification of a
// shutdown-truncated report write: a routine shutdown reads WARN (excluded from
// alerts/logql.yaml's SeadexScoutCycleError rule) while a genuine write fault keeps
// ERROR. It stays in the root because what it pins is the coupling to
// dispatchOutcome, the root's own exit-code/level contract. Three non-obvious
// properties carry it: the multi-%w wrap surviving (fmt drops ALL wrapping on a nil
// %w operand), the guard's asymmetry, and Normalize not wrapping twice.
func TestDetachedWriteError(t *testing.T) {
	cancelled := func() context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}

	t.Run("a shutdown-truncated detached write classifies as a routine shutdown", func(t *testing.T) {
		ctx := cancelled()
		werr := fmt.Errorf("write report pair: %w", context.DeadlineExceeded)
		got := shutdown.DetachedWriteError(ctx, werr)
		if !errors.Is(got, context.Canceled) {
			t.Errorf("errors.Is(err, context.Canceled) = false, want true (the multi-%%w wrap must survive; otherwise the redeploy ERROR alert returns): %v", got)
		}
		if !errors.Is(got, context.DeadlineExceeded) {
			t.Errorf("the original write error was lost: %v", got)
		}
		if level, _, _ := dispatchOutcome(got, false); level != slog.LevelWarn {
			t.Errorf("dispatchOutcome level = %v, want WARN (a shutdown-truncated report must not trip SeadexScoutCycleError)", level)
		}
		// shutdown.Normalize runs deferred over the same ctx and must
		// leave an already-classified error alone rather than wrapping it twice.
		if again := shutdown.Normalize(ctx, got); again != got {
			t.Errorf("Normalize re-wrapped the classified error: %v", again)
		}
	})
	t.Run("a genuine write failure keeps its fault classification", func(t *testing.T) {
		werr := errors.New("write report pair: no space left on device")
		if got := shutdown.DetachedWriteError(cancelled(), werr); got != werr {
			t.Errorf("shutdown.DetachedWriteError(cancelled, non-timeout) = %v, want the error unchanged", got)
		}
	})
	t.Run("a live context never reclassifies", func(t *testing.T) {
		werr := fmt.Errorf("write report pair: %w", context.DeadlineExceeded)
		if got := shutdown.DetachedWriteError(t.Context(), werr); got != werr {
			t.Errorf("shutdown.DetachedWriteError(live, timeout) = %v, want the error unchanged", got)
		}
	})
}
