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
