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
