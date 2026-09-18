package pathredact

import (
	"path/filepath"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestTextPropertyMasksEveryPathForm is the per-PR randomized net under the app's
// one secret-masking primitive. The masked value is secret-capable (config
// expansion places an allowlisted ${SEADEX_SCOUT_*} value in any string field) and
// Text is what keeps it out of Loki, while TestTextGuards covers six hand-picked
// strings, so the ancestor walk holds only for the two forms it names. Two halves:
// the secret-bearing component survives in NO path form an os.PathError from the
// report pipeline can carry, and the marker is always emitted.
func TestTextPropertyMasksEveryPathForm(t *testing.T) {
	// The surrounding text is drawn from a punctuation-only alphabet so it can
	// never coincidentally contain the generated secret component.
	noise := rapid.StringOfN(rapid.RuneFrom([]rune("!?#()<>@%+ \n")), 0, 12, -1)
	rapid.Check(t, func(t *rapid.T) {
		secret := rapid.StringOfN(rapid.RuneFrom([]rune("abcdefKLM0123456789")), 6, 12, -1).Draw(t, "secret")
		dir := "/config/" + secret + "/reports"
		form := rapid.SampledFrom([]string{
			dir,
			dir + "/report-2026-07-11T15-04-05Z.json",
			dir + "/.atomicfile-1.tmp",
			filepath.Dir(dir),
			filepath.Dir(filepath.Dir(dir)),
		}).Draw(t, "form")
		text := noise.Draw(t, "prefix") + form + noise.Draw(t, "suffix")

		got := Text(dir, text)

		if strings.Contains(got, secret) {
			t.Errorf("Text(%q, %q) = %q, leaked the secret-capable dir component", dir, text, got)
		}
		if !strings.Contains(got, ReportDirMarker) {
			t.Errorf("Text(%q, %q) = %q, want the %q marker", dir, text, got, ReportDirMarker)
		}
	})
}
