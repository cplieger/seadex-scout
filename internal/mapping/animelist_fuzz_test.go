package mapping

import (
	"errors"
	"maps"
	"os"
	"slices"
	"testing"

	"github.com/cplieger/xmlx"
)

// FuzzParseMappingList drives the mapping-list decoder over hostile bodies: it
// must never panic, it must be deterministic, every bound refusal must surface as
// the library's *xmlx.LimitError (anything else is a decode error), and a
// returned map must hold only non-empty mappings under positive keys.
func FuzzParseMappingList(f *testing.F) {
	if fixture, err := os.ReadFile("testdata/anime-list-fixture.xml"); err == nil {
		f.Add(fixture)
	}
	node := func(defaultSeason, rows string) []byte {
		return []byte(`<anime-list><anime anidbid="7" defaulttvdbseason="` + defaultSeason + `"><name>n</name><mapping-list>` +
			rows + `</mapping-list><supplemental-info><studio>s</studio></supplemental-info></anime></anime-list>`)
	}
	f.Add(node("0", `<mapping anidbseason="1" tvdbseason="0">;1-8;</mapping>`))
	f.Add(node("0", `<mapping anidbseason="1" tvdbseason="0">;1-1;2-1;3-1;</mapping>`))
	f.Add(node("0", `<mapping anidbseason="1" tvdbseason="0">;1-3;2-4;</mapping>`))
	f.Add(node("0", `<mapping anidbseason="1" tvdbseason="2">;1-1+2;2-3+4;</mapping>`))
	f.Add(node("0", `<mapping anidbseason="0" tvdbseason="0">;1-0;</mapping>`))
	f.Add(node("0", `<mapping anidbseason="1" tvdbseason="0" start="2" end="9" offset="9">;1-9;</mapping>`))
	f.Add(node("1", `<mapping anidbseason="1" tvdbseason="0">;10-1;</mapping>`))
	f.Add(node("a", `<mapping anidbseason="1" tmdbseason="1" start="1" end="61" offset="0"/><mapping anidbseason="1" tvdbseason="1" start="1" end="8" offset="0"/><mapping anidbseason="1" tvdbseason="2" start="9" offset="-8"/>`))
	f.Add(node("a", `<mapping anidbseason="1" tvdbseason="2" start="14" end="28" offset="-13"/>`))
	f.Add([]byte(`<anime-list></anime-list>`))
	f.Add([]byte(`<rss/>`))
	f.Add([]byte(`<!DOCTYPE anime-list><anime-list/>`))
	f.Add([]byte(``))

	f.Fuzz(func(t *testing.T, body []byte) {
		got, err := parseMappingList(body)
		again, errAgain := parseMappingList(body)
		if (err == nil) != (errAgain == nil) || !maps.EqualFunc(got, again, sameMapping) {
			t.Fatalf("parseMappingList is not deterministic: (%+v, %v) then (%+v, %v)", got, err, again, errAgain)
		}
		if err != nil {
			if got != nil {
				t.Fatalf("parseMappingList error %v returned a map %+v, want nil", err, got)
			}
			if errors.Is(err, xmlx.ErrLimit) {
				if _, ok := errors.AsType[*xmlx.LimitError](err); !ok {
					t.Fatalf("error %v wraps ErrLimit without an *xmlx.LimitError", err)
				}
			}
			return
		}
		if got == nil {
			// A body with nodes and nothing to read yields an empty, non-nil map.
			t.Fatal("parseMappingList returned a nil map with a nil error")
		}
		for id, m := range got {
			if id <= 0 {
				t.Fatalf("mappings key %d is not positive", id)
			}
			if m.SpecialEpisode < 0 {
				t.Fatalf("mappings[%d].SpecialEpisode = %d, want >= 0", id, m.SpecialEpisode)
			}
			if m.SpecialEpisode == 0 && len(m.Seasons) == 0 {
				t.Fatalf("mappings[%d] is empty, want only non-empty mappings retained", id)
			}
			if !slices.IsSortedFunc(m.Seasons, func(a, b SeasonRange) int { return a.First - b.First }) {
				t.Fatalf("mappings[%d].Seasons = %+v, want sorted by First", id, m.Seasons)
			}
			for _, r := range m.Seasons {
				if r.Season < 1 || r.First < 1 {
					t.Fatalf("mappings[%d] range %+v, want Season >= 1 and First >= 1", id, r)
				}
			}
		}
	})
}

// sameMapping is the equality the fuzz determinism check compares two decodes on.
func sameMapping(a, b Mapping) bool {
	return a.SpecialEpisode == b.SpecialEpisode && slices.Equal(a.Seasons, b.Seasons)
}
