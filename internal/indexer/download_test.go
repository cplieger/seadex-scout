package indexer

import "testing"

func TestDownloadURLEscapesAnimeBytesPasskey(t *testing.T) {
	got, ok := downloadURL("AB", "/torrents.php?id=1&torrentid=1167293", "a/b?c#d %")
	if !ok {
		t.Fatal("downloadURL returned ok=false, want a grabbable AnimeBytes URL")
	}
	const want = "https://animebytes.tv/torrent/1167293/download/a%2Fb%3Fc%23d%20%25"
	if got != want {
		t.Errorf("downloadURL with reserved passkey bytes = %q, want %q", got, want)
	}
}
