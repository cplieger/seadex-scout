package filter

import (
	"testing"

	"pgregory.net/rapid"
)

func TestABVisibleGeneratedHostBoundaryProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		label := rapid.StringMatching(`[A-Za-z0-9]{1,32}`).Draw(t, "label")

		if abVisible("Nyaa", "https://"+label+".animebytes.tv/x", false) {
			t.Errorf("abVisible surfaced generated AnimeBytes subdomain %q with the toggle off", label+".animebytes.tv")
		}
		if abVisible("Nyaa", "https://"+label+".ANIMEBYTES.TV./x", false) {
			t.Errorf("abVisible surfaced generated mixed-case AnimeBytes FQDN %q with the toggle off", label+".ANIMEBYTES.TV.")
		}
		if !abVisible("Nyaa", "https://"+label+"animebytes.tv.example/x", false) {
			t.Errorf("abVisible hid generated lookalike host %q as AnimeBytes", label+"animebytes.tv.example")
		}
	})
}
