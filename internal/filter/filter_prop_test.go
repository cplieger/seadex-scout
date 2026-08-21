package filter

import (
	"testing"

	"pgregory.net/rapid"
)

func TestABVisibleGeneratedHostBoundaryProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		label := rapid.StringMatching(`[A-Za-z0-9]{1,32}`).Draw(t, "label")

		if ABVisible("Nyaa", "https://"+label+".animebytes.tv/x", false) {
			t.Errorf("ABVisible surfaced generated AnimeBytes subdomain %q with the toggle off", label+".animebytes.tv")
		}
		if ABVisible("Nyaa", "https://"+label+".ANIMEBYTES.TV./x", false) {
			t.Errorf("ABVisible surfaced generated mixed-case AnimeBytes FQDN %q with the toggle off", label+".ANIMEBYTES.TV.")
		}
		if !ABVisible("Nyaa", "https://"+label+"animebytes.tv.example/x", false) {
			t.Errorf("ABVisible hid generated lookalike host %q as AnimeBytes", label+"animebytes.tv.example")
		}
	})
}
