package config

import (
	"strings"
	"testing"

	"pgregory.net/rapid"
)

func TestSanitizeTypeErrorEntryRedactsGeneratedExcerptsProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		const sentinel = "EXCERPT-SENTINEL-9c2f"
		pre := rapid.String().Draw(t, "pre")
		post := rapid.String().Draw(t, "post")
		entry := "line 4: cannot unmarshal !!str `" + pre + sentinel + post + "` into bool"

		got := sanitizeTypeErrorEntry(entry)

		if strings.Contains(got, sentinel) {
			t.Errorf("sanitizeTypeErrorEntry(%q) leaks the excerpt sentinel: %q", entry, got)
		}
		if want := "line 4: cannot unmarshal !!str <redacted> into bool"; got != want {
			t.Errorf("sanitizeTypeErrorEntry(%q) = %q, want %q", entry, got, want)
		}
	})
}
