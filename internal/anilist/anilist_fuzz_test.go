package anilist

import "testing"

// FuzzParseMedia exercises the single-media GraphQL decoder against arbitrary
// bytes (the AniList response is an untrusted network boundary). Beyond
// crash-freedom it asserts the title invariant callers rely on: the returned
// title list is free of empty and duplicate entries (what dedupeTitles
// guarantees), so a downstream normalized-title match never keys on "".
func FuzzParseMedia(f *testing.F) {
	f.Add([]byte(`{"data":{"Media":{"format":"TV","seasonYear":2023,"title":{"romaji":"A","english":"B","native":"C"}}}}`))
	f.Add([]byte(`{"data":{"Media":null}}`))
	f.Add([]byte(`{"data":{"Media":null},"errors":[{"message":"x"}]}`))
	f.Add([]byte(`{"data":{"Media":{"title":{"romaji":"A","english":"A"}}}}`))
	f.Add([]byte(``))
	f.Add([]byte(`{bad`))
	f.Add([]byte(`{"data":{"Media":{"format":"MOVIE","startDate":{"year":2020},"title":{"romaji":"A"}}}}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		m, err := parseMedia(raw)
		if err != nil {
			return
		}
		assertTitlesClean(t, m.Titles, raw)
	})
}

// FuzzParseMediaPage exercises the batched Page(media) decoder against arbitrary
// bytes, asserting the same title invariant across every returned id plus the
// id guard callers rely on: parseMediaPage rejects non-positive media IDs, so
// every key in the returned map must be positive.
func FuzzParseMediaPage(f *testing.F) {
	f.Add([]byte(`{"data":{"Page":{"media":[{"id":1,"title":{"romaji":"A","english":"A"}}]}}}`))
	f.Add([]byte(`{"data":{"Page":{"media":[]}}}`))
	f.Add([]byte(`{"errors":[{"message":"x"}]}`))
	f.Add([]byte(``))
	f.Add([]byte(`{bad`))
	f.Add([]byte(`{"data":{"Page":null}}`))
	f.Add([]byte(`{"data":{"Page":{"media":[{"id":0,"title":{"romaji":"missing"}}]}}}`))
	f.Add([]byte(`{"data":{"Page":{"media":[{"id":-1,"title":{"romaji":"negative"}}]}}}`))
	f.Add([]byte(`{"data":{"Page":{"media":[{"id":2,"format":"MOVIE","startDate":{"year":2019},"title":{"romaji":"B","english":"B"}}]}}}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		out, err := parseMediaPage(raw)
		if err != nil {
			return
		}
		for id, m := range out {
			if id <= 0 {
				t.Errorf("parseMediaPage(%q) returned non-positive id %d", raw, id)
			}
			assertTitlesClean(t, m.Titles, raw)
		}
	})
}

func assertTitlesClean(t *testing.T, titles []string, raw []byte) {
	t.Helper()
	seen := make(map[string]struct{}, len(titles))
	for _, title := range titles {
		if title == "" {
			t.Errorf("empty title from %q", raw)
		}
		if _, dup := seen[title]; dup {
			t.Errorf("duplicate title %q from %q", title, raw)
		}
		seen[title] = struct{}{}
	}
}
