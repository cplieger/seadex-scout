package degradation

import "testing"

func TestShrunk(t *testing.T) {
	tests := map[string]struct {
		count     int
		prevCount int
		want      bool
	}{
		"no prior population is not shrunk":               {count: 0, prevCount: 0, want: false},
		"empty candidate from populated source is shrunk": {count: 0, prevCount: 8, want: true},
		"below half of an even population is shrunk":      {count: 3, prevCount: 8, want: true},
		"exactly half of an even population is accepted":  {count: 4, prevCount: 8, want: false},
		"below half of an odd population is shrunk":       {count: 3, prevCount: 7, want: true},
		"ceiling half of an odd population is accepted":   {count: 4, prevCount: 7, want: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := Shrunk(tt.count, tt.prevCount); got != tt.want {
				t.Errorf("Shrunk(%d, %d) = %v, want %v", tt.count, tt.prevCount, got, tt.want)
			}
		})
	}
}

// TestAdvanceIsTheTransitionRuleForBothCadences pins the rule the four
// reconcile-cadence streaks and the mapping loader's tick-cadence streak share,
// now that it lives beside the thresholds it is compared against (h-f23).
//
// The reset arm is the half that matters and the half that could drift: a streak
// counts CONSECUTIVE failures, so evidence of success has to zero it. The
// threshold being a PARAMETER is what lets one rule serve both cadences.
func TestAdvanceIsTheTransitionRuleForBothCadences(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		start     int
		degraded  bool
		escalate  int
		wantCount int
		wantFired bool
	}{
		"first failure below threshold":      {start: 0, degraded: true, escalate: 2, wantCount: 1, wantFired: false},
		"reaching the threshold fires":       {start: 1, degraded: true, escalate: 2, wantCount: 2, wantFired: true},
		"past the threshold keeps firing":    {start: 9, degraded: true, escalate: 2, wantCount: 10, wantFired: true},
		"success resets from a long streak":  {start: 47, degraded: false, escalate: 2, wantCount: 0, wantFired: false},
		"success on a zero streak is a noop": {start: 0, degraded: false, escalate: 2, wantCount: 0, wantFired: false},
		"the tick cadence uses the same rule with its own number": {
			start: 7, degraded: true, escalate: TickEscalationThreshold, wantCount: 8, wantFired: true,
		},
		"the tick cadence does not fire at the reconcile number": {
			start: 1, degraded: true, escalate: TickEscalationThreshold, wantCount: 2, wantFired: false,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			counter := tc.start
			fired := Advance(&counter, tc.degraded, tc.escalate)
			if counter != tc.wantCount {
				t.Errorf("counter = %d, want %d", counter, tc.wantCount)
			}
			if fired != tc.wantFired {
				t.Errorf("fired = %v, want %v", fired, tc.wantFired)
			}
		})
	}
}

// TestCadenceThresholdsAreDistinctAndOrdered pins why there are two constants
// rather than one reused number: the count is cadence-relative, so the same
// integer means about 2h on the tick and 8 days on the reconcile. Collapsing
// them is how the fleet's threshold silently became a week once the passes it
// counted stopped running hourly.
func TestCadenceThresholdsAreDistinctAndOrdered(t *testing.T) {
	t.Parallel()
	if TickEscalationThreshold <= ReconcileEscalationThreshold {
		t.Errorf("TickEscalationThreshold (%d) must exceed ReconcileEscalationThreshold (%d): the tick runs far more often, so it needs more consecutive failures to mean the same elapsed time",
			TickEscalationThreshold, ReconcileEscalationThreshold)
	}
	if ShrunkWalkAcceptThreshold <= ReconcileEscalationThreshold {
		t.Errorf("ShrunkWalkAcceptThreshold (%d) must exceed ReconcileEscalationThreshold (%d), or the guard would accept the smaller library before it ever escalated to ERROR",
			ShrunkWalkAcceptThreshold, ReconcileEscalationThreshold)
	}
}

// TestApproachingLimitIsTheOneWarningThresholdEveryByteCapShares pins the
// policy the three persisted-file caps used to each recompute for themselves -
// the indexer feed snapshot, the Fribb download and state.json - and which they
// disagreed about at the exact boundary, one warning at the fraction while two
// stayed silent until one byte past it.
//
// Two properties, and the second is the one a reader cannot infer: the
// comparison is INCLUSIVE, so a payload landing exactly on the fraction warns;
// and the limit is divided before it is multiplied, so the threshold truncates
// DOWN. The expected byte counts are therefore hardcoded rather than recomputed
// from the fraction - a threshold derived from the same expression it is
// checking against cannot see the operation order change.
func TestApproachingLimitIsTheOneWarningThresholdEveryByteCapShares(t *testing.T) {
	t.Parallel()
	// 16 MiB truncates to 13421768 (16777216/10*8), NOT the 13421772 that
	// multiplying first would give; 32 MiB truncates to 26843544, not 26843545.
	const (
		feedLimit      = 16 << 20
		feedThreshold  = 13_421_768
		stateLimit     = 32 << 20
		stateThreshold = 26_843_544
	)
	tests := map[string]struct {
		size  int64
		limit int64
		want  bool
	}{
		"an empty payload is nowhere near its limit":       {size: 0, limit: feedLimit, want: false},
		"one byte below the fraction stays silent":         {size: feedThreshold - 1, limit: feedLimit, want: false},
		"exactly the fraction warns":                       {size: feedThreshold, limit: feedLimit, want: true},
		"one byte above the fraction warns":                {size: feedThreshold + 1, limit: feedLimit, want: true},
		"a payload standing on the limit itself warns":     {size: feedLimit, limit: feedLimit, want: true},
		"a payload past the limit warns":                   {size: feedLimit + 1, limit: feedLimit, want: true},
		"one byte below a larger cap's fraction is silent": {size: stateThreshold - 1, limit: stateLimit, want: false},
		"exactly a larger cap's fraction warns":            {size: stateThreshold, limit: stateLimit, want: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := ApproachingLimit(tc.size, tc.limit); got != tc.want {
				t.Errorf("ApproachingLimit(%d, %d) = %v, want %v", tc.size, tc.limit, got, tc.want)
			}
		})
	}
}
