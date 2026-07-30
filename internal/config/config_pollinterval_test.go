package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPollIntervalDefaultAndFloorValues pins the two poll_interval CONSTANTS by
// value, not symbolically.
//
// Every other test in this package derives its expectation from these constants,
// so all of them keep passing if a constant drifts. These are the ones that
// would catch it, and the values are not arbitrary: both follow the CONSUMER.
// Sonarr's RSS Sync Interval is 10-120 minutes with a default of 15, so fetching
// faster than 15m cannot reach the arrs any sooner, and below Sonarr's own
// 10-minute minimum a shorter interval buys freshness no arr can read while
// still costing the upstream a probe every time.
//
// The interval is also no longer the cost knob it used to be: most iterations
// are a cheap tick and every 24h worth of them is one full reconcile, so the
// upstream load is proportional to the change RATE rather than to 1/interval.
// That is what makes a 15m default defensible against a community-run upstream
// at all, and it is why lowering the floor further is a different decision from
// lowering the default.
func TestPollIntervalDefaultAndFloorValues(t *testing.T) {
	t.Parallel()
	if DefaultPollInterval != 15*time.Minute {
		t.Errorf("DefaultPollInterval = %v, want 15m (Sonarr's own RSS Sync Interval default)", DefaultPollInterval)
	}
	if minPollInterval != 15*time.Minute {
		t.Errorf("minPollInterval = %v, want 15m", minPollInterval)
	}
	// The floor must not exceed the default, or every unconfigured deployment
	// would silently be clamped away from the documented default.
	if minPollInterval > DefaultPollInterval {
		t.Errorf("minPollInterval %v exceeds DefaultPollInterval %v", minPollInterval, DefaultPollInterval)
	}
	if maxPollInterval <= minPollInterval {
		t.Errorf("maxPollInterval %v does not exceed minPollInterval %v", maxPollInterval, minPollInterval)
	}
}

// TestLoadPollIntervalClampsAndDefaults pins the two operator-visible outcomes
// end to end through Load, rather than through parseInterval alone: an interval
// under the floor is raised to it (not rejected, and not honoured), and an
// absent key yields the default.
//
// Going through Load matters because the clamp and the default live on different
// code paths - the clamp inside the scheduler parse, the default in the
// defaults baseline the file is decoded onto - and only Load exercises both.
func TestLoadPollIntervalClampsAndDefaults(t *testing.T) {
	t.Parallel()
	const arrs = "sonarr:\n  enabled: true\n  url: \"http://sonarr:8989\"\n  api_key: \"k\"\n"
	tests := map[string]struct {
		pollLine string
		want     time.Duration
	}{
		"under the floor clamps up":  {"poll_interval: \"1m\"\n", minPollInterval},
		"the floor itself is kept":   {"poll_interval: \"15m\"\n", 15 * time.Minute},
		"above the floor is honored": {"poll_interval: \"6h\"\n", 6 * time.Hour},
		"absent yields the default":  {"", DefaultPollInterval},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(arrs+tc.pollLine), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.PollExternal {
				t.Errorf("PollExternal = true, want false (a duration is scheduled mode)")
			}
			if cfg.PollInterval != tc.want {
				t.Errorf("PollInterval = %v, want %v", cfg.PollInterval, tc.want)
			}
		})
	}
}
