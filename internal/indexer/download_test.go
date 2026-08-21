package indexer

import "testing"

func TestDownloadURLEscapesAnimeBytesPasskey(t *testing.T) {
	got, ok := downloadURLForScope(trackerScope("AB"), "/torrents.php?id=1&torrentid=1167293", "a/b?c#d %")
	if !ok {
		t.Fatal("downloadURLForScope returned ok=false, want a grabbable AnimeBytes URL")
	}
	const want = "https://animebytes.tv/torrent/1167293/download/a%2Fb%3Fc%23d%20%25"
	if got != want {
		t.Errorf("downloadURLForScope with reserved passkey bytes = %q, want %q", got, want)
	}
}

// TestDownloadURLBuildsFromTheVouchedForm pins the classify-once contract on the
// download path (h-f8): the ownership gate vouches the BROWSER's reading of a
// raw SeaDex URL, so the id must be extracted from that same reading rather than
// from the original spelling. Before the fix an edge-padded URL passed ownership
// and then lost its id to a net/url re-parse of the padded string, so the
// release silently carried no download link.
//
// The refusal half is equally load-bearing: an embedded tab or newline, a
// backslash and a hidden-host form are refused by the gate BEFORE any id is
// extracted, so cleaning the vouched form does not widen what can be grabbed
// beyond edge padding.
func TestDownloadURLBuildsFromTheVouchedForm(t *testing.T) {
	const passkey = "pk"
	tests := map[string]struct {
		tracker, sourceURL, want string
	}{
		"nyaa leading tab":            {"Nyaa", "\thttps://nyaa.si/view/123", "https://nyaa.si/download/123.torrent"},
		"nyaa leading nul":            {"Nyaa", "\x00https://nyaa.si/view/123", "https://nyaa.si/download/123.torrent"},
		"nyaa leading space":          {"Nyaa", " https://nyaa.si/view/123", "https://nyaa.si/download/123.torrent"},
		"nyaa trailing cr":            {"Nyaa", "https://nyaa.si/view/123\r", "https://nyaa.si/download/123.torrent"},
		"nyaa trailing nul":           {"Nyaa", "https://nyaa.si/view/123\x00", "https://nyaa.si/download/123.torrent"},
		"nyaa trailing space":         {"Nyaa", "https://nyaa.si/view/123 ", "https://nyaa.si/download/123.torrent"},
		"ab relative leading space":   {"AB", " /torrents.php?id=1&torrentid=456", "https://animebytes.tv/torrent/456/download/pk"},
		"ab relative trailing nul":    {"AB", "/torrents.php?id=1&torrentid=456\x00", "https://animebytes.tv/torrent/456/download/pk"},
		"ab absolute leading nul":     {"AB", "\x00https://animebytes.tv/torrents.php?id=1&torrentid=456", "https://animebytes.tv/torrent/456/download/pk"},
		"ab permalink trailing lf":    {"AB", "https://animebytes.tv/torrent/456/group\n", "https://animebytes.tv/torrent/456/download/pk"},
		"embedded tab still refused":  {"Nyaa", "https://nyaa\t.si/view/123", ""},
		"embedded lf still refused":   {"Nyaa", "https://nyaa.si/view\n/123", ""},
		"backslash still refused":     {"Nyaa", "https:/\\nyaa.si/view/123", ""},
		"hidden host still refused":   {"Nyaa", "https:nyaa.si/view/123", ""},
		"unparseable still refused":   {"Nyaa", "http://[::1", ""},
		"foreign host still refused":  {"Nyaa", "https://evil.example/view/123", ""},
		"padding does not mint an id": {"Nyaa", "https://nyaa.si/view/ 123", ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, ok := downloadURLForScope(trackerScope(tc.tracker), tc.sourceURL, passkey)
			if tc.want == "" {
				if ok {
					t.Errorf("downloadURLForScope(%q, %q) = %q, ok=true, want refused", tc.tracker, tc.sourceURL, got)
				}
				return
			}
			if !ok {
				t.Fatalf("downloadURLForScope(%q, %q) ok=false, want %q", tc.tracker, tc.sourceURL, tc.want)
			}
			if got != tc.want {
				t.Errorf("downloadURLForScope(%q, %q) = %q, want %q", tc.tracker, tc.sourceURL, got, tc.want)
			}
		})
	}
}
