package indexer

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/slogx/capture"
)

// sampleFeed is a representative Prowlarr per-indexer Torznab response (one Nyaa
// item), used to verify the parser handles the namespaced torznab:attr elements
// and the enclosure/comments fields.
const sampleFeed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:atom="http://www.w3.org/2005/Atom" xmlns:torznab="http://torznab.com/schemas/2015/feed">
  <channel>
    <title>Nyaa.si</title>
    <item>
      <title>[Group] Some Anime S01 [1080p]</title>
      <guid>https://nyaa.si/view/1234567</guid>
      <comments>https://nyaa.si/view/1234567</comments>
      <pubDate>Mon, 06 Jul 2026 12:00:00 +0000</pubDate>
      <size>14352012572</size>
      <link>http://prowlarr:9696/1/download?apikey=x&amp;link=abc</link>
      <enclosure url="http://prowlarr:9696/1/download?apikey=x&amp;link=abc" length="14352012572" type="application/x-bittorrent"/>
      <torznab:attr name="category" value="5070"/>
      <torznab:attr name="seeders" value="42"/>
      <torznab:attr name="peers" value="50"/>
      <torznab:attr name="infohash" value="ABCDEF1234567890abcdef1234567890abcdef12"/>
      <torznab:attr name="downloadvolumefactor" value="1"/>
    </item>
  </channel>
</rss>`

func TestParseTorznab(t *testing.T) {
	items, err := parseTorznab([]byte(sampleFeed))
	if err != nil {
		t.Fatalf("parseTorznab: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	it := items[0]

	if it.Title != "[Group] Some Anime S01 [1080p]" {
		t.Errorf("title = %q", it.Title)
	}
	if it.InfoURL != "https://nyaa.si/view/1234567" {
		t.Errorf("infoURL = %q", it.InfoURL)
	}
	// The torznab:attr namespaced elements must be captured.
	if it.InfoHash != "abcdef1234567890abcdef1234567890abcdef12" {
		t.Errorf("infohash = %q (torznab:attr not parsed?)", it.InfoHash)
	}
	if it.Seeders != 42 {
		t.Errorf("seeders = %d, want 42", it.Seeders)
	}
	if it.Leechers != 8 { // peers 50 - seeders 42
		t.Errorf("leechers = %d, want 8", it.Leechers)
	}
	if it.Size != 14352012572 {
		t.Errorf("size = %d", it.Size)
	}
	if len(it.Categories) != 1 || it.Categories[0] != 5070 {
		t.Errorf("categories = %v, want [5070]", it.Categories)
	}
	if it.DownloadURL != "http://prowlarr:9696/1/download?apikey=x&link=abc" {
		t.Errorf("downloadURLForScope = %q", it.DownloadURL)
	}
	if it.PubDate.IsZero() {
		t.Error("pubDate not parsed")
	}
}

// TestParseTorznabClampsNegativeCounts pins the numeric-domain normalization
// of the untrusted Torznab decode (the sibling of totalSize's guard on the
// SeaDex path): negative size/seeders/leechers values clamp to the feed's
// zero-as-unknown representation, and a negative peers value cannot inflate
// the derived leechers count via an unbounded negative seeders subtraction.
func TestParseTorznabClampsNegativeCounts(t *testing.T) {
	const feed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed">
  <channel>
    <item>
      <title>all negative</title>
      <size>-14352012572</size>
      <torznab:attr name="seeders" value="-42"/>
      <torznab:attr name="leechers" value="-3"/>
    </item>
    <item>
      <title>negative seeders with positive peers</title>
      <torznab:attr name="seeders" value="-5"/>
      <torznab:attr name="peers" value="1"/>
    </item>
    <item>
      <title>negative enclosure length</title>
      <enclosure url="http://prowlarr:9696/1/download" length="-5" type="application/x-bittorrent"/>
      <torznab:attr name="peers" value="-9"/>
    </item>
    <item>
      <title>negative enclosure length falls through to size element</title>
      <enclosure url="http://prowlarr:9696/1/download" length="-5" type="application/x-bittorrent"/>
      <size>99</size>
    </item>
  </channel>
</rss>`
	items, err := parseTorznab([]byte(feed))
	if err != nil {
		t.Fatalf("parseTorznab: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("got %d items, want 4", len(items))
	}
	if it := items[0]; it.Size != 0 || it.Seeders != 0 || it.Leechers != 0 {
		t.Errorf("all-negative item = size %d seeders %d leechers %d, want 0/0/0", it.Size, it.Seeders, it.Leechers)
	}
	// Clamped seeders (0) with peers 1 derives leechers 1, never 1-(-5)=6.
	if it := items[1]; it.Seeders != 0 || it.Leechers != 1 {
		t.Errorf("negative-seeders item = seeders %d leechers %d, want 0/1", it.Seeders, it.Leechers)
	}
	if it := items[2]; it.Size != 0 || it.Leechers != 0 {
		t.Errorf("negative-enclosure item = size %d leechers %d, want 0/0", it.Size, it.Leechers)
	}
	// An invalid negative enclosure length is unset-or-invalid, not a
	// distinct value: the chain falls through to the valid <size> element
	// instead of clamping the whole item to 0.
	if it := items[3]; it.Size != 99 {
		t.Errorf("negative-enclosure-with-size item = size %d, want 99 (fall through to the <size> element)", it.Size)
	}
}

func TestExtractID(t *testing.T) {
	tests := []struct {
		url, needle, want string
	}{
		{"https://nyaa.si/view/1234567", "/view/", "1234567"},
		{"https://nyaa.si/view/1234567?x=1", "/view/", "1234567"},
		{"https://nyaa.si/view/1234567#c", "/view/", "1234567"},
		{"/torrents.php?id=70543&torrentid=1143533", "torrentid=", "1143533"},
		{"/torrents.php?id=70543&torrentid=1143533&x=1", "torrentid=", "1143533"},
		{"https://nyaa.si/view/abc", "/view/", ""},    // non-numeric rejected
		{"https://nyaa.si/view/12a45", "/view/", ""},  // non-numeric rejected
		{"https://example.com/other/1", "/view/", ""}, // needle absent
		{"", "/view/", ""}, // empty
	}
	for _, tc := range tests {
		if got := extractID(tc.url, tc.needle); got != tc.want {
			t.Errorf("extractID(%q,%q) = %q, want %q", tc.url, tc.needle, got, tc.want)
		}
	}
}

func TestTrackerKey(t *testing.T) {
	if got := trackerKey("Nyaa", "https://nyaa.si/view/1234567"); got != "nyaa:1234567" {
		t.Errorf("nyaa trackerKey = %q", got)
	}
	if got := trackerKey("AB", "/torrents.php?id=70543&torrentid=1143533"); got != "ab:1143533" {
		t.Errorf("ab trackerKey = %q", got)
	}
	if got := trackerKeyFromURL("https://nyaa.si/view/1234567"); got != "nyaa:1234567" {
		t.Errorf("nyaa trackerKeyFromURL = %q", got)
	}
	if got := trackerKeyFromURL("https://nyaa.si./view/1234567"); got != "nyaa:1234567" {
		t.Errorf("nyaa FQDN trailing-dot trackerKeyFromURL = %q, want nyaa:1234567", got)
	}
	if got := trackerKeyFromURL("https://animebytes.tv/torrents.php?id=70543&torrentid=1143533"); got != "ab:1143533" {
		t.Errorf("ab trackerKeyFromURL = %q", got)
	}
	if got := trackerKeyFromURL("https://example.com/x/1"); got != "" {
		t.Errorf("unknown host trackerKeyFromURL = %q, want empty", got)
	}
	// Misleading hosts that embed a tracker name as a substring must NOT be
	// keyed: a host-substring match would have accepted these and let a
	// tracker-controlled URL bypass the SeaDex curation gate.
	if got := trackerKeyFromURL("https://notnyaa.example/view/1234567"); got != "" {
		t.Errorf("misleading nyaa host trackerKeyFromURL = %q, want empty", got)
	}
	if got := trackerKeyFromURL("https://example.com/torrent/1167293/group?tracker=animebytes"); got != "" {
		t.Errorf("misleading animebytes URL trackerKeyFromURL = %q, want empty", got)
	}
	// Component-aware extraction: a curated id embedded in a query value or
	// fragment of a trusted host's URL identifies nothing - only the path
	// (Nyaa /view, AB permalink) or the torrentid query parameter may key.
	if got := trackerKeyFromURL("https://nyaa.si/?next=/view/1234567"); got != "" {
		t.Errorf("nyaa query-embedded id trackerKeyFromURL = %q, want empty", got)
	}
	if got := trackerKeyFromURL("https://animebytes.tv/?next=/torrent/1167293/group"); got != "" {
		t.Errorf("ab query-embedded id trackerKeyFromURL = %q, want empty", got)
	}
	if got := trackerKeyFromURL("https://nyaa.si/#/view/1234567"); got != "" {
		t.Errorf("nyaa fragment-embedded id trackerKeyFromURL = %q, want empty", got)
	}
	if got := trackerKeyFromURL("https://animebytes.tv/torrent/1167293/group"); got != "ab:1167293" {
		t.Errorf("ab permalink trackerKeyFromURL = %q, want ab:1167293", got)
	}
}

func TestMarkAndDedupe(t *testing.T) {
	set := &curation{
		byHash: map[string]bool{"abcdef1234567890abcdef1234567890abcdef12": true},
		byKey:  map[string]bool{"nyaa:1143533": false},
	}
	raw := []item{
		{Title: "best by hash", InfoHash: "abcdef1234567890abcdef1234567890abcdef12", GUID: "g1"},
		{Title: "alt by key", InfoURL: "https://nyaa.si/view/1143533", GUID: "g2"},
		{Title: "not curated", InfoURL: "https://nyaa.si/view/999", GUID: "g3"},
		{Title: "dup of best", InfoHash: "abcdef1234567890abcdef1234567890abcdef12", GUID: "g1"},
	}
	out, _ := markAndDedupe(raw, set, upstreamNyaa)
	if len(out) != 2 {
		t.Fatalf("got %d items, want 2 (best + alt, dup dropped, uncurated dropped)", len(out))
	}
	if out[0].DownloadVolumeFactor != dvfBest {
		t.Errorf("best marker = %q, want %q", out[0].DownloadVolumeFactor, dvfBest)
	}
	if out[1].DownloadVolumeFactor != dvfAlt {
		t.Errorf("alt marker = %q, want %q", out[1].DownloadVolumeFactor, dvfAlt)
	}
}

// TestMarkAndDedupeRejectsConflictingIdentity pins lookup's identity
// consistency rule: every structurally valid identity signal an untrusted
// Torznab item carries must resolve to curated entries agreeing on best/alt.
// An item pairing a curated best info hash with the page URL of a different
// torrent (an alt entry, or a structurally valid but uncurated one) must be
// dropped, never admitted on the first matching signal.
func TestMarkAndDedupeRejectsConflictingIdentity(t *testing.T) {
	t.Run("best hash against an alt or uncurated key", func(t *testing.T) {
		set := &curation{
			byHash: map[string]bool{"abcdef1234567890abcdef1234567890abcdef12": true},
			byKey:  map[string]bool{"nyaa:1143533": false},
		}
		raw := []item{
			{
				Title: "best hash + alt key", GUID: "g1",
				InfoHash: "abcdef1234567890abcdef1234567890abcdef12",
				InfoURL:  "https://nyaa.si/view/1143533",
			},
			{
				Title: "best hash + uncurated key", GUID: "g2",
				InfoHash: "abcdef1234567890abcdef1234567890abcdef12",
				InfoURL:  "https://nyaa.si/view/999",
			},
		}
		if out, _ := markAndDedupe(raw, set, upstreamNyaa); len(out) != 0 {
			t.Errorf("got %d items, want 0 (conflicting identity signals must drop the item)", len(out))
		}
	})

	// Two curated keys that AGREE on best/alt but name DIFFERENT releases:
	// healthy Prowlarr emits the same tracker id in comments and guid, so an
	// item whose InfoURL and GUID resolve to distinct curated torrents is an
	// invalid untrusted response and must fail closed - the same-marker
	// coincidence must not admit it.
	t.Run("two curated best ids on one item", func(t *testing.T) {
		bothBest := &curation{
			byHash: map[string]bool{},
			byKey:  map[string]bool{"nyaa:100": true, "nyaa:200": true},
		}
		conflicting := []item{{
			Title:   "two curated best ids",
			InfoURL: "https://nyaa.si/view/100",
			GUID:    "https://nyaa.si/view/200",
		}}
		if out, _ := markAndDedupe(conflicting, bothBest, upstreamNyaa); len(out) != 0 {
			t.Errorf("got %d items, want 0 (distinct tracker identities must drop the item even when both are best)", len(out))
		}
	})
}

// TestMarkAndDedupeRejectsCrossTorrentPair pins lookup's hash/key pair
// relation: an item pairing torrent A's curated info hash with torrent B's
// curated tracker key must be rejected even when both signals carry the same
// best/alt marker, because byPair records only same-torrent hash/key
// combinations. A hash-only Nyaa item still matches without a pair, and a
// legacy snapshot (nil byPair, persisted before the relation existed) fails
// closed for dual-signal items - the relation cannot be proven - while
// single-signal matching keeps working until the next snapshot rebuild.
func TestMarkAndDedupeRejectsCrossTorrentPair(t *testing.T) {
	hashA := "abcdef1234567890abcdef1234567890abcdef12"
	hashB := "0123456789012345678901234567890123456789"
	set := &curation{
		byHash: map[string]bool{hashA: true, hashB: true},
		byKey:  map[string]bool{"nyaa:100": true, "nyaa:200": true},
		byPair: map[string]bool{
			pairKey(hashA, "nyaa:100"): true,
			pairKey(hashB, "nyaa:200"): true,
		},
	}
	// A legacy snapshot (nil byPair, persisted before the relation existed)
	// cannot PROVE any hash/key co-membership, so a dual-signal item fails
	// closed - even a genuinely same-torrent pair - until the next cycle
	// rewrites the snapshot with the relation; single-signal matching keeps
	// working through the upgrade window.
	legacy := &curation{byHash: set.byHash, byKey: set.byKey}

	matching := []item{{
		Title: "hash and key from one torrent", InfoHash: hashA,
		InfoURL: "https://nyaa.si/view/100", GUID: "https://nyaa.si/view/100",
	}}
	crossWired := []item{{
		Title: "torrent A hash + torrent B key", InfoHash: hashA,
		InfoURL: "https://nyaa.si/view/200", GUID: "https://nyaa.si/view/200",
	}}
	hashOnly := []item{{Title: "hash only", InfoHash: hashA, GUID: "g1"}}

	for _, tc := range []struct {
		name  string
		items []item
		set   *curation
		want  int
		why   string
	}{
		{
			"same-torrent pair matches", matching, set, 1,
			"a same-torrent hash/key pair must match",
		},
		{
			"cross-torrent pair rejected", crossWired, set, 0,
			"a cross-torrent hash/key pair must not match even when both are best",
		},
		{
			"hash-only item needs no pair", hashOnly, set, 1,
			"a hash-only Nyaa item needs no pair",
		},
		{
			"legacy snapshot rejects cross-torrent pair", crossWired, legacy, 0,
			"a legacy nil-byPair snapshot must reject an unprovable dual-signal pair",
		},
		{
			"legacy snapshot rejects same-torrent pair", matching, legacy, 0,
			"even a same-torrent pair is unprovable against a nil byPair",
		},
		{
			"legacy snapshot keeps single-signal matching", hashOnly, legacy, 1,
			"single-signal matching survives a legacy nil-byPair snapshot",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if out, _ := markAndDedupe(tc.items, tc.set, upstreamNyaa); len(out) != tc.want {
				t.Errorf("got %d items, want %d (%s)", len(out), tc.want, tc.why)
			}
		})
	}
}

// TestMarkAndDedupeKeyOnlyABNeedsNoPair pins that the pair gate does not
// change AnimeBytes matching: AB exposes no info hash in Torznab, so a
// key-only item matches on its scoped tracker key alone with no pair to
// prove, even against a snapshot whose pair relation is empty.
func TestMarkAndDedupeKeyOnlyABNeedsNoPair(t *testing.T) {
	set := &curation{
		byHash: map[string]bool{},
		byKey:  map[string]bool{"ab:300": true},
		byPair: map[string]bool{},
	}
	raw := []item{{
		Title:   "key only",
		InfoURL: "https://animebytes.tv/torrent/300/group",
		GUID:    "https://animebytes.tv/torrent/300/group",
	}}
	out, _ := markAndDedupe(raw, set, upstreamAB)
	if len(out) != 1 {
		t.Fatalf("got %d items, want 1 (a key-only AB item needs no pair)", len(out))
	}
	if out[0].DownloadVolumeFactor != dvfBest {
		t.Errorf("marker = %q, want %q", out[0].DownloadVolumeFactor, dvfBest)
	}
}

// TestMarkAndDedupeRejectsCrossScopeKey pins lookup's tracker-scope binding: a
// tracker key parsed from an item's page URL must belong to the endpoint being
// served, so a curated Nyaa item is rejected under the /ab scope (a swapped
// upstream or cross-tracker item must not surface under the wrong per-tracker
// indexer). It also pins the AB-specific rule that a scoped tracker key is
// mandatory: AnimeBytes exposes no info hash in Torznab, so a hash-only item
// cannot match under /ab even when its hash is curated.
func TestMarkAndDedupeRejectsCrossScopeKey(t *testing.T) {
	set := &curation{
		byHash: map[string]bool{"abcdef1234567890abcdef1234567890abcdef12": true},
		byKey:  map[string]bool{"nyaa:1143533": false, "ab:1143533": false},
	}
	raw := []item{
		{Title: "nyaa key under ab scope", InfoURL: "https://nyaa.si/view/1143533", GUID: "g1"},
		{Title: "curated hash only under ab scope", InfoHash: "abcdef1234567890abcdef1234567890abcdef12", GUID: "g2"},
	}
	if out, _ := markAndDedupe(raw, set, upstreamAB); len(out) != 0 {
		t.Fatalf("got %d items, want 0 (cross-scope key and hash-only items must not match under /ab)", len(out))
	}
	abOnly := []item{{Title: "ab key under nyaa scope", InfoURL: "https://animebytes.tv/torrents.php?id=1&torrentid=1143533", GUID: "g3"}}
	if out, _ := markAndDedupe(abOnly, set, upstreamNyaa); len(out) != 0 {
		t.Fatalf("got %d items, want 0 (an AnimeBytes key must not match under /nyaa)", len(out))
	}
}

// TestMarkAndDedupeRejectsUncuratedHash pins the miss leg of the curation
// gate's info-hash arm: an item carrying a structurally valid 40-hex info hash
// that is NOT in the SeaDex curation set is no identity signal, so an item
// carrying nothing else must be dropped, never admitted or marked - and the
// drop is an ordinary no-match, not an identity conflict.
func TestMarkAndDedupeRejectsUncuratedHash(t *testing.T) {
	set := &curation{
		byHash: map[string]bool{"abcdef1234567890abcdef1234567890abcdef12": true},
		byKey:  map[string]bool{},
	}
	raw := []item{{Title: "uncurated hash", InfoHash: "0123456789012345678901234567890123456789", GUID: "g1"}}
	out, conflicts := markAndDedupe(raw, set, upstreamNyaa)
	if len(out) != 0 {
		t.Errorf("got %d items, want 0 (a valid but uncurated info hash must not match)", len(out))
	}
	if conflicts != 0 {
		t.Errorf("identity conflicts = %d, want 0 (nothing curated was contradicted)", conflicts)
	}
}

// TestMarkAndDedupeAdmitsUnknownHashBesideCuratedKey pins the hash-miss leg
// lookup deliberately does NOT veto on (l-f30): a SeaDex record with no usable
// info hash registers only its tracker key, while Prowlarr's Nyaa results
// always carry the real hash - so the curated release arrives with a hash the
// set has never seen beside its own curated page URL. Reading that miss as
// "this hash names an uncurated release" made the release invisible to every
// search. A hash the set DOES know still has to prove co-membership, which the
// cross-torrent case below re-checks.
func TestMarkAndDedupeAdmitsUnknownHashBesideCuratedKey(t *testing.T) {
	set := &curation{
		byHash: map[string]bool{},
		byKey:  map[string]bool{"nyaa:1143533": true},
		byPair: map[string]bool{},
	}
	raw := []item{{
		Title:    "curated key, hash SeaDex never recorded",
		InfoHash: "0123456789012345678901234567890123456789",
		InfoURL:  "https://nyaa.si/view/1143533",
		GUID:     "https://nyaa.si/view/1143533",
	}}
	out, conflicts := markAndDedupe(raw, set, upstreamNyaa)
	if len(out) != 1 {
		t.Fatalf("got %d items, want 1 (an unknown hash must not veto a curated tracker key)", len(out))
	}
	if out[0].DownloadVolumeFactor != dvfBest {
		t.Errorf("marker = %q, want %q (the key's own best/alt value)", out[0].DownloadVolumeFactor, dvfBest)
	}
	if conflicts != 0 {
		t.Errorf("identity conflicts = %d, want 0 (an admitted item is no conflict)", conflicts)
	}
}

// TestMarkAndDedupeCountsIdentityConflicts pins the accounting that keeps the
// fail-closed class visible: an item whose CURATED signal is contradicted by
// another signal (here torrent A's curated hash beside torrent B's curated
// key) is dropped AND counted, so the per-request line distinguishes a
// tampered or misbehaving upstream from a clean no-match. An item that simply
// carries nothing curated is not counted.
func TestMarkAndDedupeCountsIdentityConflicts(t *testing.T) {
	hashA := "abcdef1234567890abcdef1234567890abcdef12"
	set := &curation{
		byHash: map[string]bool{hashA: true},
		byKey:  map[string]bool{"nyaa:100": true, "nyaa:200": true},
		byPair: map[string]bool{pairKey(hashA, "nyaa:100"): true},
	}
	raw := []item{
		{
			Title: "torrent A hash + torrent B key", InfoHash: hashA,
			InfoURL: "https://nyaa.si/view/200", GUID: "https://nyaa.si/view/200",
		},
		{Title: "nothing curated", InfoURL: "https://nyaa.si/view/999", GUID: "g2"},
	}
	out, conflicts := markAndDedupe(raw, set, upstreamNyaa)
	if len(out) != 0 {
		t.Errorf("got %d items, want 0", len(out))
	}
	if conflicts != 1 {
		t.Errorf("identity conflicts = %d, want 1 (only the contradicted item counts)", conflicts)
	}
}

// TestIndexerEndToEnd exercises the writer/server split end to end: the compare
// cycle (FeedWriter) builds + persists the feed snapshot from a SeaDex entry,
// and the server loads it and answers both a real search (proxy Prowlarr ->
// parse -> match against the loaded curation set -> mark) and an empty-q RSS
// check (served from the loaded synthesized feed).
func TestIndexerEndToEnd(t *testing.T) {
	// One SeaDex entry with a best Nyaa torrent matching the sample feed's info
	// hash. A multi-episode season pack (two episode files), so the synthesized
	// RSS feed collapses its title to the season.
	entries := []seadex.Entry{{
		AniListID: 123,
		Torrents: []seadex.Torrent{{
			Tracker:      "Nyaa",
			URL:          "https://nyaa.si/view/1234567",
			InfoHash:     "ABCDEF1234567890abcdef1234567890abcdef12",
			IsBest:       true,
			ReleaseGroup: "PMR",
			Files: []seadex.File{
				{Length: 100, Name: "Some Anime - S01E01 (BD Remux 1080p) [PMR].mkv"},
				{Length: 100, Name: "Some Anime - S01E02 (BD Remux 1080p) [PMR].mkv"},
			},
		}},
	}}

	// The compare cycle builds + persists the feed snapshot; the server reads it.
	// The ledger is seeded empty so the pack journals (a fresh install would
	// baseline and serve an empty journal).
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	if err := newTestWriter(path, "", false).Rebuild(t.Context(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	// Mock Prowlarr Torznab: returns the sample feed regardless of query.
	var gotAPIKey string
	torznabSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/rss+xml")
		// Rewrite the fixture's download link onto this mock endpoint's own
		// origin: search now drops items whose download URL is not on the
		// configured Prowlarr origin, and a real Prowlarr hands out proxy
		// links on its own host.
		_, _ = io.WriteString(w, strings.ReplaceAll(sampleFeed, "http://prowlarr:9696", "http://"+r.Host))
	}))
	defer torznabSrv.Close()

	ix := warmedIndexer(&Config{
		SnapshotPath:   path,
		NyaaTorznabURL: torznabSrv.URL,
		ProwlarrAPIKey: "prowlarr-key",
	}, nil, torznabSrv.Client())

	// A real search (non-empty q) filters to the curation set loaded from the
	// snapshot: the sample item matches by info hash, gets the best marker, and
	// its real seeders pass through.
	items, stats, _ := ix.query(t.Context(), url.Values{"t": {"tvsearch"}, "q": {"Some Anime"}}, "nyaa")
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if !stats.answered || stats.feed || stats.upstream != 1 || stats.curated != 1 {
		t.Errorf("stats = %+v, want answered, not feed, upstream 1, curated 1", stats)
	}
	if items[0].DownloadVolumeFactor != dvfBest {
		t.Errorf("marker = %q, want %q (best)", items[0].DownloadVolumeFactor, dvfBest)
	}
	if items[0].Seeders != 42 {
		t.Errorf("seeders passed through = %d, want 42", items[0].Seeders)
	}
	if gotAPIKey != "prowlarr-key" {
		t.Errorf("upstream X-Api-Key = %q, want prowlarr-key", gotAPIKey)
	}

	// A Movies-category search is NOT re-filtered against the tracker's own
	// categories: both proxied trackers are anime trackers, so a film arrives
	// categorized Anime 5070 (as the fixture item is), and `cat` was already
	// forwarded to Prowlarr. Re-applying the local filter here emptied every
	// Movies search after a successful fetch and a successful curation match.
	movieSearch := url.Values{"t": {"search"}, "q": {"Some Anime 2011"}, "cat": {"2000"}}
	if got, st, _ := ix.query(t.Context(), movieSearch, "nyaa"); len(got) != 1 || st.feed {
		t.Errorf("movie-category search returned %d items (feed=%v), want the 1 curated proxied item", len(got), st.feed)
	}

	// Per-tracker scoping (real search): the ab scope has no configured
	// upstream, so it serves nothing (the nyaa scope is exercised above).
	if got, _, _ := ix.query(t.Context(), url.Values{"t": {"tvsearch"}, "q": {"Some Anime"}}, "ab"); len(got) != 0 {
		t.Errorf("ab scope returned %d items, want 0 (no ab upstream)", len(got))
	}

	// The synthesized RSS feed is served from the loaded snapshot, independent of
	// the live search path: an empty-q request (an RSS "latest" fetch, or
	// Prowlarr's save test) returns the curated Nyaa release, its title collapsed
	// to the season, a directly-built .torrent link, and the best marker.
	got, st, _ := ix.query(t.Context(), url.Values{"t": {"search"}}, "nyaa")
	if len(got) != 1 || !st.feed {
		t.Fatalf("empty-q feed returned %d items (feed=%v), want 1 synthesized item", len(got), st.feed)
	}
	if got[0].Title != "Some Anime - S01 (BD Remux 1080p) [PMR]" {
		t.Errorf("synthesized title = %q, want the season-collapsed title", got[0].Title)
	}
	if got[0].DownloadURL != "https://nyaa.si/download/1234567.torrent" {
		t.Errorf("synthesized download URL = %q, want the public Nyaa .torrent link", got[0].DownloadURL)
	}
	if got[0].DownloadVolumeFactor != dvfBest {
		t.Errorf("synthesized marker = %q, want %q (best)", got[0].DownloadVolumeFactor, dvfBest)
	}
	// (Dropping an uncurated Prowlarr result is covered directly by
	// TestMarkAndDedupe; the mock here returns the curated item for any query.)
}

// TestWiredUpstreamDoesNotForwardAPIKeyAcrossHost pins the redirect policy of
// the client the composition root supplies to the constructors (build.go passes
// httpx.NewClient, so this test wires the production pairing): the Prowlarr API
// key rides an X-Api-Key header, which net/http forwards across redirects, so a
// cross-host hop must be refused before the credential can leave the configured
// origin. Driving the real request path (rather than invoking CheckRedirect
// directly) keeps the test on the contract instead of the mechanism, so an
// equivalent policy moved into a RoundTripper still passes.
func TestWiredUpstreamDoesNotForwardAPIKeyAcrossHost(t *testing.T) {
	leaked := make(chan string, 1)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case leaked <- r.Header.Get("X-Api-Key"):
		default:
		}
		_, _ = io.WriteString(w, `<rss><channel></channel></rss>`)
	}))
	defer sink.Close()

	redirectTarget := strings.Replace(sink.URL, "127.0.0.1", "localhost", 1)
	redirected := make(chan string, 1)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case redirected <- r.Header.Get("X-Api-Key"):
		default:
		}
		http.Redirect(w, r, redirectTarget, http.StatusFound)
	}))
	defer redirector.Close()

	ups := wireUpstreams(httpx.NewClient(upstreamAttemptTimeout), nil, UpstreamConfig{
		NyaaTorznabURL: redirector.URL,
		ProwlarrAPIKey: "prowlarr-key",
	})
	if len(ups) != 1 {
		t.Fatalf("wired %d upstreams, want 1", len(ups))
	}
	_, err := ups[0].fetchAndParse(t.Context(), redirector.URL)
	if err == nil {
		t.Fatal("cross-host redirect returned nil, want the wired client to refuse it")
	}
	select {
	case key := <-redirected:
		if key != "prowlarr-key" {
			t.Errorf("original Prowlarr request X-Api-Key = %q, want %q", key, "prowlarr-key")
		}
	default:
		t.Fatal("Prowlarr endpoint was not requested; the test did not exercise redirect handling")
	}
	select {
	case key := <-leaked:
		t.Fatalf("redirect target received X-Api-Key %q, want no request to cross hosts", key)
	default:
	}
}

// TestNilClientProxiesNothing pins the no-client contract that replaced the old
// nil-HTTP fallback: with no client the server wires no upstream and constructs
// no client of its own, so an enabled tracker still serves its persisted RSS
// feed while a search makes NO outbound request at all (there is no credential
// to forward and no default client to pick a redirect policy for it).
func TestNilClientProxiesNothing(t *testing.T) {
	cfg := UpstreamConfig{NyaaTorznabURL: "http://prowlarr.invalid/1/api", ProwlarrAPIKey: "prowlarr-key"}
	if ups := wireUpstreams(nil, nil, cfg); len(ups) != 0 {
		t.Errorf("wireUpstreams with a nil client wired %d upstreams, want 0", len(ups))
	}
	ix := New(&Config{UpstreamConfig: cfg}, nil, nil)
	if len(ix.upstreams) != 0 {
		t.Errorf("a nil client wired %d upstreams, want 0", len(ix.upstreams))
	}
	items, fetched, failed := ix.fetchRaw(t.Context(), url.Values{"q": {"anything"}}, upstreamNyaa)
	if items != nil || fetched != 0 || failed {
		t.Errorf("fetchRaw with no wired upstream = (%v, %d, %v), want (nil, 0, false)", items, fetched, failed)
	}
}

// TestConsumerWarningsStayIndependent pins why each constructor wires its own
// upstreams: the per-upstream WARN-onset latches are per-instance, so a server
// and a feed writer built from the SAME client and config must still hold their
// own latch state. Sharing them would let the server's first filter warning arm
// the writer's latch, silently demoting the writer's independently actionable
// onset WARN to Debug.
func TestConsumerWarningsStayIndependent(t *testing.T) {
	const (
		droppedMsg = "upstream items dropped: download URL not on the Prowlarr endpoint origin"
		blankedMsg = "upstream display URLs blanked: not the tracker's own canonical http(s) page URL"
	)
	log, rec := capture.New()
	cfg := UpstreamConfig{NyaaTorznabURL: "http://prowlarr:9696/1/api"}
	client := &http.Client{}
	ix := New(&Config{UpstreamConfig: cfg}, log, client)
	writer := NewFeedWriter(&FeedWriterConfig{UpstreamConfig: cfg}, log, client)
	if len(ix.upstreams) != 1 || len(writer.harvest.upstreams) != 1 {
		t.Fatalf("consumer upstream counts = (%d, %d), want (1, 1)", len(ix.upstreams), len(writer.harvest.upstreams))
	}
	if ix.upstreams[0] == writer.harvest.upstreams[0] {
		t.Fatal("the two consumers share one upstream instance, so they share its WARN-onset latches")
	}

	for _, u := range []*upstream{ix.upstreams[0], writer.harvest.upstreams[0]} {
		u.filterDownloadURLs([]item{
			{Title: "foreign download", DownloadURL: "https://evil.example/steal"},
			{
				Title: "foreign display", DownloadURL: "http://prowlarr:9696/1/download?link=ok",
				InfoURL: "https://evil.example/phish",
			},
		}, mustFeedURL(t, u))
	}

	warnCounts := map[string]int{droppedMsg: 0, blankedMsg: 0}
	for _, record := range rec.Records() {
		if record.Level == slog.LevelWarn {
			warnCounts[record.Message]++
		}
	}
	for message, got := range warnCounts {
		if got != 2 {
			t.Errorf("WARN count for %q = %d, want 2 (one onset per consumer)", message, got)
		}
	}
}

// TestUnresolvedFirstLoadFaultsInsteadOfServingEmpty pins the startup window of
// the cache's reload clock: until its first load resolves, nothing is installed
// and the empty in-memory snapshot is indistinguishable from a fresh install, so
// every request must answer the snapshot-unavailable Torznab fault rather than a
// successful empty feed the arr would record as a clean no-match. It must answer
// IMMEDIATELY - the request path performs no load and waits on nothing, which is
// what a wedged /config mount used to be able to break - and serve normally once
// the load resolves.
//
// The unresolved state is set directly rather than simulated with a wedged
// filesystem: the loader owns the load, so there is nothing on the request path
// left to block on, and this state machine is exactly what the fault reads.
func TestUnresolvedFirstLoadFaultsInsteadOfServingEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	if err := newTestWriter(path, "", false).Rebuild(t.Context(), nyaaTestEntries(1), nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	ix := New(&Config{SnapshotPath: path, NyaaTorznabURL: "http://prowlarr/1/api", ProwlarrAPIKey: "k"}, nil, nil)
	// A loader is running and has not resolved its first load: exactly the state
	// start leaves behind when the wait expires before the load returns.
	ix.cache.watchStarted.Store(true)

	served := make(chan *torznabFault, 1)
	go func() {
		_, _, fault := ix.query(t.Context(), url.Values{"t": {"search"}}, "nyaa")
		served <- fault
	}()
	select {
	case fault := <-served:
		if fault == nil || fault.summary != "feed snapshot unavailable" {
			t.Errorf("fault while the first load is unresolved = %+v, want the snapshot-unavailable fault", fault)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request waited on the unresolved first load; want an immediate snapshot-unavailable fault")
	}

	// The loader resolves: requests serve the loaded snapshot.
	ix.cache.loader.refresh(t.Context())
	close(ix.cache.firstLoad)
	items, _, fault := ix.query(t.Context(), url.Values{"t": {"search"}}, "nyaa")
	if fault != nil {
		t.Errorf("post-load fault = %+v, want none", fault)
	}
	if len(items) != 1 {
		t.Errorf("post-load feed = %d items, want 1", len(items))
	}
}

// TestFirstLoadWaitIsBounded pins the startup bound on the initial snapshot load:
// a slow or wedged /config mount cannot be interrupted mid-syscall, so
// awaitFirstLoad must stop WAITING at warmLoadTimeout rather than hold the whole
// daemon's startup down behind it. The WARN is the only signal that startup
// stopped waiting and began serving without the persisted snapshot; unasserted,
// it can be deleted or dropped to Debug with the whole suite still green.
func TestFirstLoadWaitIsBounded(t *testing.T) {
	prev := warmLoadTimeout
	warmLoadTimeout = 50 * time.Millisecond
	t.Cleanup(func() { warmLoadTimeout = prev })

	log, rec := capture.New()
	// firstLoad is never closed: the load is still running, which is the state
	// the bound exists for.
	c := newSnapshotCache(filepath.Join(t.TempDir(), "feed.json"), "", log)

	done := make(chan struct{})
	go func() { defer close(done); c.awaitFirstLoad(t.Context()) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("awaitFirstLoad still waiting on an unresolved load; want the wait bounded by warmLoadTimeout")
	}
	if got := rec.CountLevel(slog.LevelWarn, "feed snapshot warm load still running"); got != 1 {
		t.Errorf("warm-load timeout WARN count = %d, want 1; log output:\n%s", got, strings.Join(rec.Messages(), "\n"))
	}
}

// TestStartServesPublishedSnapshotWithoutRequestLoad pins the reload clock's own
// cadence: start loads the persisted snapshot once, and a snapshot written
// afterwards by ANOTHER process (the `poll` subcommand) is picked up by the
// cache's own tick - never by a request, which does one lock-free read of
// whatever is current.
func TestStartServesPublishedSnapshotWithoutRequestLoad(t *testing.T) {
	prev := snapshotWatchInterval
	snapshotWatchInterval = 10 * time.Millisecond
	t.Cleanup(func() { snapshotWatchInterval = prev })

	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyFeed(t, path)
	writer := newTestWriter(path, "", false)
	if err := writer.Rebuild(t.Context(), nyaaTestEntries(1), nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ix := New(&Config{SnapshotPath: path, NyaaTorznabURL: "http://prowlarr/1/api", ProwlarrAPIKey: "k"}, nil, nil)
	ix.cache.start(ctx)
	if got := ix.feedFor(upstreamNyaa); len(got) != 1 {
		t.Fatalf("feed after start = %d items, want the persisted snapshot loaded (1)", len(got))
	}

	// A second process rewrote the snapshot: no request triggers the load, so the
	// tick is what must pick it up.
	if err := writer.Rebuild(t.Context(), nyaaTestEntries(3), nil); err != nil {
		t.Fatalf("second Rebuild: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := ix.feedFor(upstreamNyaa); len(got) == 3 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("feed = %d items after the out-of-process rewrite, want 3 within a few ticks",
				len(ix.feedFor(upstreamNyaa)))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestFeedWriterReload verifies the server picks up a newer snapshot a writer in
// ANOTHER process persists after the server started (the cross-process poll ->
// resident daemon path, the one case that still goes through the file): an
// initially-absent snapshot serves an empty feed, and once the file is written
// the cache's own reload clock installs it. The request path deliberately does
// not - a request is a lock-free read of whatever is published - so the tick is
// what makes the new feed servable.
func TestFeedWriterReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	ix := warmedIndexer(&Config{SnapshotPath: path, NyaaTorznabURL: "http://prowlarr/1/api", ProwlarrAPIKey: "k"}, nil, nil)

	// No snapshot yet: the empty-q feed serves nothing.
	if got, _, _ := ix.query(t.Context(), url.Values{"t": {"search"}}, "nyaa"); len(got) != 0 {
		t.Fatalf("pre-write feed = %d items, want 0", len(got))
	}

	// A cycle in another process (here, a writer with no in-process server)
	// persists a snapshot; the cache's next tick installs it. The pre-write
	// empty-feed assertion above doubles as the fresh-install journal shape, so
	// the ledger is seeded empty for the rebuild to journal.
	seedEmptyFeed(t, path)
	entries := []seadex.Entry{{
		AniListID: 7,
		Torrents: []seadex.Torrent{{
			Tracker: "Nyaa", URL: "https://nyaa.si/view/42", InfoHash: "aa" + strings.Repeat("b", 38),
			IsBest: true, ReleaseGroup: "GRP",
			Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [GRP].mkv"}},
		}},
	}}
	if err := newTestWriter(path, "", false).Rebuild(t.Context(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if got, _, _ := ix.query(t.Context(), url.Values{"t": {"search"}}, "nyaa"); len(got) != 0 {
		t.Fatalf("feed before the reload tick = %d items, want 0: a request must not load the snapshot itself", len(got))
	}
	tick(ix)
	got, st, _ := ix.query(t.Context(), url.Values{"t": {"search"}}, "nyaa")
	if len(got) != 1 || !st.feed {
		t.Fatalf("post-write feed = %d items (feed=%v), want 1 reloaded item", len(got), st.feed)
	}
	if got[0].DownloadURL != "https://nyaa.si/download/42.torrent" {
		t.Errorf("reloaded item download = %q", got[0].DownloadURL)
	}
}

// TestAnimeBytesMatching covers the real AB URL forms: SeaDex stores
// `...torrentid={id}` while Prowlarr's Torznab item uses `/torrent/{id}/group`.
// Both must key to the same "ab:{id}" (validated live: SeaDex torrentid 1167293
// == Prowlarr /torrent/1167293/group).
func TestAnimeBytesMatching(t *testing.T) {
	seadexURL := "/torrents.php?id=86576&torrentid=1167293"
	prowlarrComments := "https://animebytes.tv/torrent/1167293/group"
	prowlarrGUID := "https://animebytes.tv/torrent/1167293/group?nh=709E38EC"

	if got := trackerKey("AB", seadexURL); got != "ab:1167293" {
		t.Errorf("SeaDex AB key = %q, want ab:1167293", got)
	}
	if got := trackerKeyFromURL(prowlarrComments); got != "ab:1167293" {
		t.Errorf("Prowlarr AB comments key = %q, want ab:1167293", got)
	}
	if got := trackerKeyFromURL(prowlarrGUID); got != "ab:1167293" {
		t.Errorf("Prowlarr AB guid key = %q, want ab:1167293", got)
	}

	// End to end: an AB item (no info hash) matches the SeaDex set by tracker key.
	set := &curation{byHash: map[string]bool{}, byKey: map[string]bool{"ab:1167293": true}}
	raw := []item{{Title: "[Momonoki] Frieren S01", InfoURL: prowlarrComments, GUID: prowlarrGUID}}
	out, _ := markAndDedupe(raw, set, upstreamAB)
	if len(out) != 1 || out[0].DownloadVolumeFactor != dvfBest {
		t.Fatalf("AB item did not match/mark best: %+v", out)
	}
}

func TestServesQuery(t *testing.T) {
	serves := map[string]url.Values{
		"movie":                          {"t": {"movie"}, "q": {"Totoro"}},
		"movie search in the Movies cat": {"t": {"search"}, "q": {"From Up on Poppy Hill 2011"}, "cat": {"2000"}},
		"season pack search":             {"t": {"tvsearch"}, "q": {"Frieren"}, "season": {"1"}},
		"bare tvsearch (RSS)":            {"t": {"tvsearch"}},
		"bare search (RSS, empty q)":     {"t": {"search"}},
		"generic series search":          {"t": {"search"}, "q": {"Frieren"}},
		"special":                        {"t": {"search"}, "q": {"Frieren OVA"}},
		// query() is not called for caps, but caps still classifies as a serve.
		"caps":                              {"t": {"caps"}},
		"top of the Movies range is a film": {"t": {"search"}, "q": {"Some Film 2011"}, "cat": {"2999"}},
		// A single release, so it is always answered.
		"season-0 special search": {"t": {"tvsearch"}, "q": {"Frieren"}, "season": {"0"}, "ep": {"1"}},
	}
	for name, q := range serves {
		if !servesQuery(q) {
			t.Errorf("servesQuery(%s: %v) = false, want true", name, q)
		}
	}

	skips := map[string]url.Values{
		"per-episode (season+ep)":  {"t": {"tvsearch"}, "q": {"Frieren"}, "season": {"1"}, "ep": {"1"}},
		"anime absolute episode":   {"t": {"search"}, "q": {"Frieren 01"}},
		"4-digit absolute episode": {"t": {"search"}, "q": {"One Piece 1085"}},
		// cat 3000 is past the Movies range, so the episode skip still applies.
		"absolute episode outside the Movies range": {"t": {"search"}, "q": {"Frieren 01"}, "cat": {"3000"}},
	}
	for name, q := range skips {
		if servesQuery(q) {
			t.Errorf("servesQuery(%s: %v) = true, want false (per-episode query)", name, q)
		}
	}
}

func TestScopeFromPath(t *testing.T) {
	tests := []struct{ path, want string }{
		{"/", ""},         // no tracker segment -> 404
		{"/api", ""},      // bare API path, no tracker -> 404
		{"", ""},          // empty
		{"/nyaa", "nyaa"}, // per-tracker base path
		{"/nyaa/api", "nyaa"},
		{"/NYAA", "nyaa"}, // case-insensitive
		{"/ab", "ab"},
		{"/ab/api", "ab"},
		{"/about", ""}, // not the "ab" segment
	}
	for _, tc := range tests {
		if got := scopeFromPath(tc.path); got != tc.want {
			t.Errorf("scopeFromPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestUpstreamForScope(t *testing.T) {
	nyaa := &upstream{name: "nyaa"}
	ab := &upstream{name: "ab"}
	all := []*upstream{nyaa, ab}

	if got := upstreamForScope(all, ""); got != nil {
		t.Errorf("empty scope: got %v, want nil (no combined feed)", got)
	}
	if got := upstreamForScope(all, "nyaa"); got != nyaa {
		t.Errorf("scope nyaa: got %v, want nyaa", got)
	}
	if got := upstreamForScope(all, "ab"); got != ab {
		t.Errorf("scope ab: got %v, want ab", got)
	}
}

// TestScopeFromHost pins the Host-fallback routing table. Since the gate reads
// webhttp.CanonicalHost (the shared strict authority parser) rather than
// splitting the raw Host on its first dot (l-f25), a bare tracker host carrying
// a port routes correctly and malformed authorities no longer route at all.
func TestScopeFromHost(t *testing.T) {
	tests := []struct{ host, want string }{
		{"nyaa.cplieger.com", "nyaa"},
		{"nyaa.cplieger.com:443", "nyaa"}, // port ignored
		{"AB.example.com", "ab"},          // case-insensitive
		{"ab.example.com", "ab"},
		{"seadex.cplieger.com", ""}, // non-tracker subdomain -> 404
		{"seadex-scout:9118", ""},   // internal docker name + port
		{"seadex-scout", ""},        // internal docker name
		{"", ""},
		// A bare tracker host with a port: the raw first-dot split left the port
		// inside the label ("ab:9118"), so this failed to select any scope.
		{"ab:9118", "ab"},
		{"nyaa:9118", "nyaa"},
		{"nyaa.example.com.", "nyaa"}, // one trailing FQDN dot is legal
		// Malformed authorities the raw split accepted as a tracker.
		{"ab..example", ""},
		{"ab.example.com:", ""},
		{"ab.example.com:notaport", ""},
		{"nyaa.example.com..", ""},
		{"[ab.example.com]", ""}, // bracketed non-IPv6
	}
	for _, tc := range tests {
		if got := scopeFromHost(tc.host); got != tc.want {
			t.Errorf("scopeFromHost(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}

func TestScopeFor(t *testing.T) {
	tests := []struct{ host, path, want string }{
		{"seadex-scout:9118", "/nyaa/api", "nyaa"},   // path (internal direct use)
		{"seadex-scout:9118", "/ab", "ab"},           // path
		{"seadex-scout:9118", "/api", ""},            // neither names a tracker -> 404
		{"nyaa.cplieger.com", "/api", "nyaa"},        // host fallback (proxy subdomain)
		{"ab.cplieger.com", "/api", "ab"},            // host fallback
		{"seadex.cplieger.com", "/nyaa/api", "nyaa"}, // path over aggregate host
		{"nyaa.cplieger.com", "/ab/api", "ab"},       // explicit path wins over host
	}
	for _, tc := range tests {
		if got := scopeFor(tc.host, tc.path); got != tc.want {
			t.Errorf("scopeFor(%q,%q) = %q, want %q", tc.host, tc.path, got, tc.want)
		}
	}
}

// TestDerivedTitle exercises the file-name-derived title synthesis (the permanent
// last resort when no show title is known; see TestSynthesizeTitle for the
// assembled-title path).
func TestDerivedTitle(t *testing.T) {
	tests := []struct {
		name  string
		group string
		want  string
		files []seadex.File
	}{
		{
			name: "season pack (multi-file) collapses SxxExx to the season",
			files: []seadex.File{
				{Name: "Frieren Beyond Journey's End - S01E07 (BD Remux 1080p AVC FLAC AAC) [Dual Audio] [PMR].mkv"},
				{Name: "Frieren Beyond Journey's End - S01E08 (BD Remux 1080p AVC FLAC AAC) [Dual Audio] [PMR].mkv"},
			},
			want: "Frieren Beyond Journey's End - S01 (BD Remux 1080p AVC FLAC AAC) [Dual Audio] [PMR]",
		},
		{
			name: "torrent directory is not part of the release title",
			files: []seadex.File{
				{Name: "Season 1/Taboo Tattoo S01E01 Tattoo [Bluray-1080p Remux-h264]-LazyRemux.mkv"},
				{Name: "Season 1/Taboo Tattoo S01E02 Surprise Attack [Bluray-1080p Remux-h264]-LazyRemux.mkv"},
			},
			want: "Taboo Tattoo S01 Tattoo [Bluray-1080p Remux-h264]-LazyRemux",
		},
		{
			name:  "single-episode torrent keeps its SxxExx (complete-but-unpacked season)",
			files: []seadex.File{{Name: "Scum.of.the.Brave.S01E05.A.Brave.Sensei.1080p.CR.WEB-DL.AAC2.0.H.264-VARYG.mkv"}},
			want:  "Scum.of.the.Brave.S01E05.A.Brave.Sensei.1080p.CR.WEB-DL.AAC2.0.H.264-VARYG",
		},
		{
			name: "versioned episode in a pack still collapses to the season",
			files: []seadex.File{
				{Name: "[LostYears] Frieren Beyond Journey's End - S01E15v2 (WEB 1080p x265 10-bit AAC Opus) [3564C0AD].mkv"},
				{Name: "[LostYears] Frieren Beyond Journey's End - S01E16 (WEB 1080p x265 10-bit AAC Opus) [06E8039D].mkv"},
			},
			want: "[LostYears] Frieren Beyond Journey's End - S01 (WEB 1080p x265 10-bit AAC Opus) [3564C0AD]",
		},
		{
			name: "a v2 revision of the same episode is one episode, not a pack",
			files: []seadex.File{
				{Name: "Show - S01E01 (1080p) [G].mkv"},
				{Name: "Show - S01E01v2 (1080p) [G].mkv"},
			},
			want: "Show - S01E01 (1080p) [G]",
		},
		{
			name: "creditless extras skipped; a lone episode keeps its SxxExx",
			files: []seadex.File{
				{Name: "NCED 01 (BD Remux 1080p AVC FLAC) [PMR].mkv"},
				{Name: "Show Title - S02E01 (BD 1080p) [Grp].mkv"},
			},
			want: "Show Title - S02E01 (BD 1080p) [Grp]",
		},
		{
			name:  "single movie file used verbatim",
			files: []seadex.File{{Name: "A Silent Voice (2016) (BD 1080p x264 FLAC) [Group].mkv"}},
			want:  "A Silent Voice (2016) (BD 1080p x264 FLAC) [Group]",
		},
		{
			name: "absolute-numbered pack drops the episode number",
			files: []seadex.File{
				{Name: "[Grp] Some Show - 07 (1080p).mkv"},
				{Name: "[Grp] Some Show - 08 (1080p).mkv"},
			},
			want: "[Grp] Some Show (1080p)",
		},
		{
			name:  "single absolute-numbered episode keeps its number",
			files: []seadex.File{{Name: "[Grp] Some Show - 07 (1080p).mkv"}},
			want:  "[Grp] Some Show - 07 (1080p)",
		},
		{
			name:  "no files falls back to release group",
			files: nil,
			group: "PMR",
			want:  "PMR",
		},
		{
			name:  "episode-range token in a single file is one release, kept verbatim",
			files: []seadex.File{{Name: "Show - S01E01-E13 (1080p) [G].mkv"}},
			want:  "Show - S01E01-E13 (1080p) [G]",
		},
		{
			name: "pack of episode-range files collapses to the season",
			files: []seadex.File{
				{Name: "Show - S01E01-E02 (1080p) [G].mkv"},
				{Name: "Show - S01E03-E04 (1080p) [G].mkv"},
			},
			want: "Show - S01 (1080p) [G]",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := derivedTitle(&seadex.Torrent{Files: tc.files, ReleaseGroup: tc.group}, EntryInfo{})
			if got != tc.want {
				t.Errorf("derivedTitle = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestABFeedRequiresPasskey verifies a CONFIGURED /ab feed (ab_torznab_url
// set) rejects an empty-q request (Prowlarr's save-test or an RSS check) with
// a Torznab <error> when no passkey is set, so the AnimeBytes indexer cannot
// be saved without one; the /nyaa feed and an AB request once a passkey is set
// are unaffected. An UNCONFIGURED AB tracker is a different contract - the
// off-switch shape pinned by TestServeUnconfiguredABServesNoPasskeyItems.
func TestABFeedRequiresPasskey(t *testing.T) {
	serve := func(ix *Indexer, target string) string {
		rec := httptest.NewRecorder()
		ix.serve(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return rec.Body.String()
	}

	noKey := New(&Config{APIKey: "k", ABTorznabURL: "http://prowlarr/2/api"}, nil, nil)
	if body := serve(noKey, "/ab?t=search&apikey=k"); !strings.Contains(body, "<error") || !strings.Contains(body, "passkey") {
		t.Errorf("ab empty-q without passkey: body = %q, want a Torznab <error> mentioning the passkey", body)
	}
	if body := serve(noKey, "/nyaa?t=search&apikey=k"); strings.Contains(body, "<error") {
		t.Errorf("nyaa empty-q must not error: %q", body)
	}

	withKey := New(&Config{APIKey: "k", ABTorznabURL: "http://prowlarr/2/api", ABPasskey: "PASSKEY"}, nil, nil)
	if body := serve(withKey, "/ab?t=search&apikey=k"); strings.Contains(body, "<error") {
		t.Errorf("ab empty-q with passkey must not error: %q", body)
	}
}

// TestServeUnconfiguredABServesNoPasskeyItems pins the README's per-tracker
// off switch on the serve path: with ab_torznab_url EMPTY and ab_passkey still
// set, an /ab empty-q request (the periodic RSS check) must serve NO
// passkey-bearing items - even against a stale on-disk snapshot persisted
// while AnimeBytes was still configured - and must answer with the same empty
// feed shape as a tracker with no data, never the missing-passkey nudge (that
// nudge is for a CONFIGURED tracker). The configured sibling subtest proves
// the same snapshot serves normally once ab_torznab_url is set, so the gate
// cannot dark-launch an always-off AB feed.
func TestServeUnconfiguredABServesNoPasskeyItems(t *testing.T) {
	// A stale snapshot written before the operator blanked ab_torznab_url: its
	// AB feed carries a credential-bearing download link.
	stale := `{"version":2,"owners":{},"published":{},"nyaa_feed":[],"ab_feed":[{"FirstSeen":"2026-07-01T00:00:00Z","Key":"ab:1167293","Title":"Frieren - S01 (BD Remux 1080p) [PMR]","GUID":"https://animebytes.tv/torrents.php?id=86576&torrentid=1167293","DownloadURL":"https://animebytes.tv/torrent/1167293/download/SECRETPASSKEY"}]}`
	path := filepath.Join(t.TempDir(), "feed.json")
	if err := os.WriteFile(path, []byte(stale), 0o600); err != nil {
		t.Fatalf("write stale snapshot: %v", err)
	}
	serve := func(ix *Indexer) string {
		rec := httptest.NewRecorder()
		ix.serve(rec, httptest.NewRequest(http.MethodGet, "/ab?t=search&apikey=k", nil))
		return rec.Body.String()
	}

	t.Run("unconfigured AB serves the empty-feed shape", func(t *testing.T) {
		off := warmedIndexer(&Config{APIKey: "k", SnapshotPath: path, ABPasskey: "SECRETPASSKEY"}, nil, nil)
		body := serve(off)
		if strings.Contains(body, "SECRETPASSKEY") {
			t.Errorf("unconfigured AB response leaks the passkey: %q", body)
		}
		if strings.Contains(body, "<item>") {
			t.Errorf("unconfigured AB served feed items: %q", body)
		}
		if strings.Contains(body, "<error") {
			t.Errorf("unconfigured AB answered with a Torznab <error>, want the plain empty feed: %q", body)
		}
	})

	t.Run("configured AB serves the same snapshot", func(t *testing.T) {
		on := warmedIndexer(&Config{APIKey: "k", SnapshotPath: path, ABTorznabURL: "http://prowlarr/2/api", ABPasskey: "SECRETPASSKEY"}, nil, nil)
		body := serve(on)
		if !strings.Contains(body, "<item>") || !strings.Contains(body, "Frieren - S01 (BD Remux 1080p) [PMR]") {
			t.Errorf("configured AB did not serve the snapshot item: %q", body)
		}
	})
}

// TestRenderSynthesizedItem checks a synthesized RSS item renders in the live
// AnimeBytes Torznab item shape: an enclosure with the direct .torrent link, the
// anime category, the SeaDex freeleech marker (downloadvolumefactor 0.75 +
// uploadvolumefactor 1), a floored seeders count, the SeaDex entry as comments,
// and the info hash.
func TestRenderSynthesizedItem(t *testing.T) {
	out, _ := renderFeed([]item{{
		Title:                "Frieren Beyond Journey's End - S01 (BD Remux 1080p AVC FLAC AAC) [Dual Audio] [PMR]",
		GUID:                 "https://nyaa.si/view/1961373",
		InfoURL:              "https://releases.moe/154587",
		DownloadURL:          "https://nyaa.si/download/1961373.torrent",
		InfoHash:             "143ed15e5e3df072ae91adaeb149973a887590dd",
		DownloadVolumeFactor: dvfBest,
		Categories:           []int{catAnime},
		Size:                 22497965274,
	}})

	want := []string{
		"<title>Frieren Beyond Journey&#39;s End - S01 (BD Remux 1080p AVC FLAC AAC) [Dual Audio] [PMR]</title>",
		`<enclosure url="https://nyaa.si/download/1961373.torrent" length="22497965274" type="application/x-bittorrent"/>`,
		`<comments>https://releases.moe/154587</comments>`,
		`<torznab:attr name="category" value="5070"/>`,
		`<torznab:attr name="infohash" value="143ed15e5e3df072ae91adaeb149973a887590dd"/>`,
		`<torznab:attr name="downloadvolumefactor" value="0.75"/>`,
		`<torznab:attr name="uploadvolumefactor" value="1"/>`,
		`<torznab:attr name="seeders" value="1"/>`,
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("rendered feed missing %q\nfull output:\n%s", w, out)
		}
	}
}

// TestServeRequiresAPIKeyBeforeServingCaps verifies the API-key gate rejects a
// missing or wrong apikey before any capabilities document is served, and that a
// correct key yields the exact caps shape the arrs expect.
func TestServeRequiresAPIKeyBeforeServingCaps(t *testing.T) {
	ix := New(&Config{APIKey: "secret"}, nil, nil)

	bad := httptest.NewRecorder()
	ix.serve(bad, httptest.NewRequest(http.MethodGet, "/nyaa?t=caps&apikey=wrong", nil))
	if bad.Code != http.StatusUnauthorized {
		t.Errorf("bad apikey status = %d, want %d", bad.Code, http.StatusUnauthorized)
	}
	if strings.Contains(bad.Body.String(), "<caps>") {
		t.Errorf("bad apikey body contains caps response: %q", bad.Body.String())
	}

	missing := httptest.NewRecorder()
	ix.serve(missing, httptest.NewRequest(http.MethodGet, "/nyaa?t=caps", nil))
	if missing.Code != http.StatusUnauthorized {
		t.Errorf("missing apikey status = %d, want %d", missing.Code, http.StatusUnauthorized)
	}

	good := httptest.NewRecorder()
	ix.serve(good, httptest.NewRequest(http.MethodGet, "/nyaa?t=caps&apikey=secret", nil))
	if good.Code != http.StatusOK {
		t.Errorf("good apikey status = %d, want %d; body=%q", good.Code, http.StatusOK, good.Body.String())
	}
	if ct := good.Header().Get("Content-Type"); ct != "application/xml; charset=utf-8" {
		t.Errorf("caps content type = %q, want application/xml; charset=utf-8", ct)
	}
	body := good.Body.String()
	for _, want := range []string{
		"<caps>",
		`<search available="yes" supportedParams="q"/>`,
		`<tv-search available="yes" supportedParams="q,season,ep"/>`,
		`<movie-search available="yes" supportedParams="q"/>`,
		`<category id="5000" name="TV"><subcat id="5070" name="Anime"/></category>`,
		`<category id="2000" name="Movies"/>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("caps response missing %q\nfull body:\n%s", want, body)
		}
	}
}

// TestParseCatsRejectsNonPositiveCategoryIDs pins the domain invariant the
// parent leg of the category match rests on. categoryMatch derives an item's
// parent as c-c%1000, which is 0 for every id below 1000, so a requested set
// that ever contained 0 would match those items on the parent leg alone and
// widen the feed the arr auto-grabs from. The cat list is client text, so a
// stray 0 or a negative id has to drop out of the set rather than be trusted.
func TestParseCatsRejectsNonPositiveCategoryIDs(t *testing.T) {
	got := parseCats("0,-5,5070, 2000 ,abc,")
	if len(got) != 2 || !got[catAnime] || !got[catMovies] {
		t.Fatalf("parseCats = %v, want only {%d, %d}", got, catMovies, catAnime)
	}

	// The consequence: a requested 0 alongside a real category must not carry a
	// sub-1000 item in on the parent leg.
	items := []item{{Title: "sub-1000 tracker category", Categories: []int{42}}}
	if kept := filterByCats(items, parseCats("0,5070")); len(kept) != 0 {
		t.Errorf("filterByCats with a requested category 0 kept %#v, want no items", kept)
	}
}

// TestFilterByCatsAppliesTorznabCategorySemantics pins the Torznab category
// filter contract: an Anime item satisfies a TV-parent request, Movies excludes
// Anime, and an uncategorized item always passes through (Prowlarr already
// applied the upstream category filter).
func TestFilterByCatsAppliesTorznabCategorySemantics(t *testing.T) {
	items := []item{
		{Title: "anime", Categories: []int{catAnime}},
		{Title: "movie", Categories: []int{catMovies}},
		{Title: "uncategorized"},
	}

	if got := filterByCats(items, nil); len(got) != 3 {
		t.Errorf("empty category filter returned %d items, want 3", len(got))
	}

	anime := filterByCats(items, map[int]bool{catAnime: true})
	if len(anime) != 2 || anime[0].Title != "anime" || anime[1].Title != "uncategorized" {
		t.Errorf("anime filter returned %#v, want anime plus uncategorized passthrough", anime)
	}

	tv := filterByCats(items, map[int]bool{catTV: true})
	if len(tv) != 2 || tv[0].Title != "anime" || tv[1].Title != "uncategorized" {
		t.Errorf("TV parent filter returned %#v, want anime subcategory plus uncategorized passthrough", tv)
	}

	movies := filterByCats(items, map[int]bool{catMovies: true})
	if len(movies) != 2 || movies[0].Title != "movie" || movies[1].Title != "uncategorized" {
		t.Fatalf("movies filter returned %#v, want movie plus uncategorized passthrough", movies)
	}
}

// TestFilterByCatsMatchesAnyTorznabParent pins the GENERALIZED parent-category
// rule beyond the old Anime-under-TV special case: a Movies/HD 2040
// subcategory item satisfies its 2000 Movies parent while staying excluded
// from the unrelated TV parent, so a regression back to a hard-coded
// anime-to-TV mapping fails here.
func TestFilterByCatsMatchesAnyTorznabParent(t *testing.T) {
	items := []item{{Title: "movie subcategory", Categories: []int{2040}}}
	got := filterByCats(items, map[int]bool{catMovies: true})
	if len(got) != 1 || got[0].Title != "movie subcategory" {
		t.Errorf("Movies parent filter returned %#v, want the 2040 subcategory item", got)
	}
	if got := filterByCats(items, map[int]bool{catTV: true}); len(got) != 0 {
		t.Errorf("TV parent filter returned %#v, want no movie-subcategory items", got)
	}
}

// nyaaTestEntries builds n distinct single-torrent Nyaa SeaDex entries, the
// minimal input for a synthesized feed of n items in reload tests.
func nyaaTestEntries(n int) []seadex.Entry {
	entries := make([]seadex.Entry, 0, n)
	for i := range n {
		entries = append(entries, seadex.Entry{
			AniListID: 7 + i,
			Torrents: []seadex.Torrent{{
				Tracker: "Nyaa", URL: "https://nyaa.si/view/" + strconv.Itoa(42+i), IsBest: true,
				Files: []seadex.File{{Length: 1, Name: "Show " + strconv.Itoa(i) + " - S01E01 (1080p) [G].mkv"}},
			}},
		})
	}
	return entries
}

// seedRebuild writes a journaled snapshot of entries at path: an empty-ledger
// seed followed by one Rebuild, so every entry lands in the feed (the reload
// tests need populated snapshots, not the first-run baseline).
func seedRebuild(path string, entries []seadex.Entry) error {
	if err := os.WriteFile(path, []byte(emptyFeedJSON), 0o600); err != nil {
		return err
	}
	return newTestWriter(path, "", false).Rebuild(context.Background(), entries, nil)
}

// setMtime sets path's mtime to when, the trigger reload's freshness check reads.
func setMtime(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

// bumpMtime pushes path's mtime an hour into the future so an in-place rewrite
// (same inode, sub-granularity timestamp) is seen as changed by reload.
func bumpMtime(t *testing.T, path string) {
	t.Helper()
	setMtime(t, path, time.Now().Add(time.Hour))
}

// TestDownloadURL pins the download-link builder that produces the AnimeBytes secret link:
// Nyaa builds a public .torrent, AB embeds the operator passkey, and every un-grabbable
// case (unknown tracker, missing id, AB without a passkey) is rejected with ok=false so no
// bogus or link-less item is emitted.
func TestDownloadURL(t *testing.T) {
	tests := []struct {
		name, tracker, src, passkey, wantURL string
		wantOK                               bool
	}{
		{"nyaa builds public torrent link", "Nyaa", "https://nyaa.si/view/1961373", "", "https://nyaa.si/download/1961373.torrent", true},
		{"nyaa missing id rejected", "Nyaa", "https://nyaa.si/view/abc", "", "", false},
		{"ab embeds passkey", "AB", "/torrents.php?id=1&torrentid=1167293", "PK", "https://animebytes.tv/torrent/1167293/download/PK", true},
		{"ab without passkey rejected", "AB", "/torrents.php?id=1&torrentid=1167293", "", "", false},
		{"ab missing id rejected", "AB", "/torrents.php?id=1", "PK", "", false},
		{"unknown tracker rejected", "AnimeTosho", "https://animetosho.org/view/1", "PK", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotURL, gotOK := downloadURLForScope(trackerScope(tc.tracker), tc.src, tc.passkey)
			if gotURL != tc.wantURL || gotOK != tc.wantOK {
				t.Errorf("downloadURLForScope(%q, %q, passkey) = (%q, %v), want (%q, %v)", tc.tracker, tc.src, gotURL, gotOK, tc.wantURL, tc.wantOK)
			}
		})
	}
}

// TestValidInfoHash pins the info-hash gate that keeps a bogus value out of the feed's
// infohash attr: a real 40-char SHA-1 hex is lowercased and trimmed, and anything else -
// SeaDex's literal "<redacted>" for private trackers, a wrong length, or a 40-char string
// with a non-hex byte - is dropped.
func TestValidInfoHash(t *testing.T) {
	const valid = "143ed15e5e3df072ae91adaeb149973a887590dd"
	tests := []struct{ name, in, want string }{
		{"valid lowercase kept", valid, valid},
		{"uppercase normalized", "143ED15E5E3DF072AE91ADAEB149973A887590DD", valid},
		{"whitespace trimmed", "  " + valid + "  ", valid},
		{"redacted dropped", "<redacted>", ""},
		{"wrong length dropped", "abc", ""},
		{"forty chars with a non-hex byte dropped", "g43ed15e5e3df072ae91adaeb149973a887590dd", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := validInfoHash(tc.in); got != tc.want {
				t.Errorf("validInfoHash(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestApplyPaging pins the synthesized feed's Torznab paging contract (t=caps
// advertises limit/offset with default=defaultCapsLimit): limit trims the
// window, offset advances it, an offset past the end yields an empty page, a
// missing or invalid limit falls back to the advertised default (which leaves
// a feed smaller than the default untouched and trims a larger one), and the
// offset is applied before the limit. The substitution is silent to the
// client, so the Debug line is the only signal a misconfigured limit was
// ignored: each case also pins that it fires for a present-but-unusable value
// and stays quiet for an absent one.
func TestApplyPaging(t *testing.T) {
	feed := []item{{GUID: "a"}, {GUID: "b"}, {GUID: "c"}}
	big := make([]item, defaultCapsLimit+3)
	for i := range big {
		big[i] = item{GUID: strconv.Itoa(i)}
	}
	tests := []struct {
		name                    string
		feed                    []item
		query                   string
		want                    []string
		wantUnusableLimitLogged bool
	}{
		{"no params leave a feed below the default untouched", feed, "", []string{"a", "b", "c"}, false},
		{"limit trims the window", feed, "limit=2", []string{"a", "b"}, false},
		{"offset advances the window", feed, "offset=2", []string{"c"}, false},
		{"offset+limit page", feed, "offset=1&limit=1", []string{"b"}, false},
		{"offset past the end is an empty page", feed, "offset=10", nil, false},
		{"invalid params fall back to the default window", feed, "offset=x&limit=-1", []string{"a", "b", "c"}, true},
		{"zero limit falls back to the default window", feed, "limit=0", []string{"a", "b", "c"}, true},
		{"zero offset leaves the window anchored", feed, "offset=0", []string{"a", "b", "c"}, false},
		{"no limit applies the advertised default to a larger feed", big, "", func() []string {
			want := make([]string, defaultCapsLimit)
			for i := range want {
				want[i] = strconv.Itoa(i)
			}
			return want
		}(), false},
		{"explicit limit beyond the default wins", big, "limit=" + strconv.Itoa(defaultCapsLimit+3), func() []string {
			want := make([]string, defaultCapsLimit+3)
			for i := range want {
				want[i] = strconv.Itoa(i)
			}
			return want
		}(), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatalf("ParseQuery(%q): %v", tc.query, err)
			}
			// Per-case recorder so the diagnostic assertion is per-case.
			pagingLog, pagingRec := capture.New()
			got := applyPaging(pagingLog, tc.feed, q)
			if logged := pagingRec.Contains("unusable Torznab limit param; using the advertised default"); logged != tc.wantUnusableLimitLogged {
				t.Errorf("unusable-limit diagnostic logged = %v, want %v; records: %v",
					logged, tc.wantUnusableLimitLogged, pagingRec.Messages())
			}
			if len(got) != len(tc.want) {
				t.Fatalf("applyPaging(%q) returned %d items, want %d", tc.query, len(got), len(tc.want))
			}
			for i := range got {
				if got[i].GUID != tc.want[i] {
					t.Errorf("applyPaging(%q)[%d].GUID = %q, want %q", tc.query, i, got[i].GUID, tc.want[i])
				}
			}
		})
	}
}

// TestParsePubDate pins the Torznab <pubDate> parser on the untrusted upstream
// date string: each supported layout parses to the same instant, and any empty,
// whitespace-only, or unparseable value yields the zero time (the failure signal
// writeItem keys on to substitute the epoch for the pubDate element).
func TestParsePubDate(t *testing.T) {
	want := time.Date(2026, time.July, 6, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ name, in string }{
		{"RFC1123Z", "Mon, 06 Jul 2026 12:00:00 +0000"},
		{"RFC1123", "Mon, 06 Jul 2026 12:00:00 GMT"},
		{"RFC822Z", "06 Jul 26 12:00 +0000"},
		{"RFC822", "06 Jul 26 12:00 GMT"},
		{"RFC3339", "2026-07-06T12:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parsePubDate(tc.in); !got.Equal(want) {
				t.Errorf("parsePubDate(%q) = %v, want %v", tc.in, got, want)
			}
		})
	}
	for _, tc := range []struct{ name, in string }{
		{"empty", ""},
		{"whitespace only", "   "},
		{"unparseable", "not a date"},
		{"wrong shape", "2026/07/06 12:00"},
	} {
		t.Run("zero on "+tc.name, func(t *testing.T) {
			if got := parsePubDate(tc.in); !got.IsZero() {
				t.Errorf("parsePubDate(%q) = %v, want the zero time", tc.in, got)
			}
		})
	}
}

// TestServeFailsClosedWithoutConfiguredAPIKey pins serve's independent
// fail-closed guard for an unconfigured feed_api_key: Run refuses to bind in
// that state, but any other construction path reaching serve must get a 503,
// never a served feed - an absent apikey param also hashes to sha256(""), so
// skipping straight to the constant-time compare would OPEN the gate and serve
// the passkey-bearing feed unauthenticated.
func TestServeFailsClosedWithoutConfiguredAPIKey(t *testing.T) {
	ix := New(&Config{}, nil, nil)
	for _, target := range []string{
		"/nyaa?t=caps",
		"/nyaa?t=caps&apikey=",
		"/ab?t=search&apikey=x",
	} {
		rec := httptest.NewRecorder()
		ix.serve(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("serve(%q) with unconfigured feed_api_key = %d, want 503", target, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "<caps>") {
			t.Errorf("serve(%q) leaked a caps response despite unconfigured feed_api_key", target)
		}
	}
}

// TestSearchUsesConfiguredABUpstream is the AB-side behavioral mirror of
// TestIndexerEndToEnd's Nyaa search: an AB-only config must actually wire the
// AnimeBytes upstream in New, so an /ab search proxies Prowlarr, matches the
// curated AB torrent by tracker key (AB exposes no info hash in Torznab), and
// marks it best - while the unconfigured nyaa scope serves nothing without an
// upstream failure. Without the AB wiring in New, a valid snapshot and
// Prowlarr response would still produce no curated search items.
func TestSearchUsesConfiguredABUpstream(t *testing.T) {
	// The compare cycle rebuilds the curation set from a SeaDex AB entry
	// (torrentid 1167293, best). No passkey is needed: a search matches by
	// tracker key and rides Prowlarr's own download link.
	entries := []seadex.Entry{{
		AniListID: 154587,
		Torrents: []seadex.Torrent{{
			Tracker: "AB", URL: "/torrents.php?id=86576&torrentid=1167293", InfoHash: "<redacted>",
			IsBest: true, ReleaseGroup: "PMR",
			Files: []seadex.File{{Length: 1, Name: "Frieren - S01E01 (BD Remux 1080p) [PMR].mkv"}},
		}},
	}}
	path := filepath.Join(t.TempDir(), "feed.json")
	if err := seedRebuild(path, entries); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	// Mock Prowlarr AB Torznab: one item whose guid/comments carry the
	// /torrent/1167293/group permalink (the live AB shape), no info hash.
	const abFeed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed">
  <channel>
    <title>AnimeBytes</title>
    <item>
      <title>[PMR] Frieren S01 [BD Remux 1080p]</title>
      <guid>https://animebytes.tv/torrent/1167293/group?nh=709E38EC</guid>
      <comments>https://animebytes.tv/torrent/1167293/group</comments>
      <size>22497965274</size>
      <link>http://prowlarr:9696/2/download?apikey=x&amp;link=abc</link>
      <enclosure url="http://prowlarr:9696/2/download?apikey=x&amp;link=abc" length="22497965274" type="application/x-bittorrent"/>
      <torznab:attr name="category" value="5070"/>
      <torznab:attr name="seeders" value="7"/>
    </item>
  </channel>
</rss>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		// Rewrite the fixture's download link onto this mock endpoint's own
		// origin: search drops items whose download URL is off the configured
		// Prowlarr origin, and a real Prowlarr hands out its own proxy links.
		_, _ = io.WriteString(w, strings.ReplaceAll(abFeed, "http://prowlarr:9696", "http://"+r.Host))
	}))
	defer srv.Close()

	ix := warmedIndexer(&Config{SnapshotPath: path, ABTorznabURL: srv.URL, ProwlarrAPIKey: "k"}, nil, srv.Client())

	items, stats, fault := ix.query(t.Context(), url.Values{"t": {"tvsearch"}, "q": {"Frieren"}}, "ab")
	if len(items) != 1 {
		t.Fatalf("ab search returned %d items, want 1 (the AB upstream must be wired)", len(items))
	}
	if items[0].DownloadVolumeFactor != dvfBest {
		t.Errorf("marker = %q, want %q (best)", items[0].DownloadVolumeFactor, dvfBest)
	}
	if !stats.answered || fault != nil || stats.upstream != 1 || stats.curated != 1 {
		t.Errorf("ab stats = %+v (fault=%+v), want answered, no fault, upstream 1, curated 1", stats, fault)
	}

	// The nyaa scope has no configured upstream: an empty result (a standing
	// misconfiguration), never reported as an upstream failure.
	nyaaItems, _, nyaaFault := ix.query(t.Context(), url.Values{"t": {"tvsearch"}, "q": {"Frieren"}}, "nyaa")
	if len(nyaaItems) != 0 || nyaaFault != nil {
		t.Errorf("nyaa scope = %d items (fault=%+v), want 0 items and no fault", len(nyaaItems), nyaaFault)
	}
}

// TestFeedForUnknownScopeServesNothing pins feedFor's default arm: a scope
// that names no tracker serves no feed even when both configured trackers
// hold items, so a routing bug can never leak one tracker's feed (or the
// in-memory credential-bearing AB items) under an unrecognized scope.
func TestFeedForUnknownScopeServesNothing(t *testing.T) {
	ix := New(&Config{
		NyaaTorznabURL: "http://prowlarr/1/api",
		ABTorznabURL:   "http://prowlarr/2/api",
		ABPasskey:      "PK",
	}, nil, nil)
	ix.cache.mu.Lock()
	ix.cache.snap.NyaaFeed = []journalItem{
		{Title: "n"},
	}
	ix.cache.snap.ABFeed = []journalItem{
		{Title: "a"},
	}
	ix.cache.mu.Unlock()
	if got := ix.feedFor("other"); got != nil {
		t.Errorf("feedFor(unknown scope) = %+v, want nil", got)
	}
	if got := ix.feedFor(""); got != nil {
		t.Errorf("feedFor(empty scope) = %+v, want nil", got)
	}
}

// TestNewCopiesConfig pins New's defensive Config snapshot, the invariant the
// unlocked per-request config reads rest on (server.go's feed_api_key /
// ab_torznab_url gates, query.go's per-scope upstream checks, reload.go's AB
// passkey rebuild all read the narrowed ix.apiKey / ix.enablement values with
// no lock, safe only because they are by-value copies taken once in New). A caller that reuses or clears its Config
// after construction must therefore change nothing the server serves: the
// construction-time feed key still authorizes, and a CONFIGURED AnimeBytes
// tracker still answers the missing-passkey nudge rather than the
// unconfigured-tracker empty feed.
func TestNewCopiesConfig(t *testing.T) {
	cfg := &Config{APIKey: "k", ABTorznabURL: "http://prowlarr/2/api"}
	ix := New(cfg, nil, nil)

	// The caller reuses (or clears) its Config after construction.
	cfg.APIKey = ""
	cfg.ABTorznabURL = ""

	rec := httptest.NewRecorder()
	ix.serve(rec, httptest.NewRequest(http.MethodGet, "/nyaa?t=caps&apikey=k", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("caps after the caller blanked its APIKey = %d, want 200 (New must snapshot the config values by value)", rec.Code)
	}

	rec = httptest.NewRecorder()
	ix.serve(rec, httptest.NewRequest(http.MethodGet, "/ab?t=search&apikey=k", nil))
	if body := rec.Body.String(); !strings.Contains(body, "<error") || !strings.Contains(body, "passkey") {
		t.Errorf("ab empty-q body after the caller blanked ab_torznab_url = %q, want the configured-tracker missing-passkey <error>", body)
	}
}

// TestRejectionLinesNameTheClientIP pins the Loki-visible contract of the two
// request-rejection lines: the caller is identified by the fleet-standard
// `client_ip` attribute (every webhttp consumer's spelling, so one shared query
// over the fleet's security lines includes this app), resolved through
// webhttp.ClientIP - which strips the port, so the value is an address the
// operator can match against a firewall or DHCP lease rather than an
// ephemeral-port socket string.
func TestRejectionLinesNameTheClientIP(t *testing.T) {
	log, rec := capture.New()
	ix := New(&Config{
		APIKey:         "secret",
		NyaaTorznabURL: "http://prowlarr/1/api",
	}, log, nil)

	badKey := httptest.NewRequest(http.MethodGet, "/nyaa?t=caps&apikey=wrong", nil)
	badKey.RemoteAddr = "192.0.2.10:54321"
	ix.serve(httptest.NewRecorder(), badKey)

	noScope := httptest.NewRequest(http.MethodGet, "/other?t=caps&apikey=secret", nil)
	noScope.RemoteAddr = "192.0.2.11:12345"
	ix.serve(httptest.NewRecorder(), noScope)

	// Both rejections share one message, so read the attrs record by record
	// rather than through the first-match accessors.
	var gotIPs []string
	for _, r := range rec.Records() {
		if r.Message != "indexer request rejected" {
			continue
		}
		ip := ""
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "client_ip":
				ip = a.Value.String()
			case "remote":
				t.Errorf("rejection line still carries the pre-adoption `remote` attr: %v", a.Value)
			}
			return true
		})
		gotIPs = append(gotIPs, ip)
	}
	// Host only, no port: webhttp.ClientIP strips it, so the value matches a
	// firewall rule or a DHCP lease instead of an ephemeral socket string.
	want := []string{"192.0.2.10", "192.0.2.11"}
	if !slices.Equal(gotIPs, want) {
		t.Errorf("client_ip values on the rejection lines = %q, want %q (messages: %v)", gotIPs, want, rec.Messages())
	}
}

// TestDisabledTrackerFeedIsNotGatedBySnapshotState pins the off switch against
// the snapshot state machine: an empty per-tracker Torznab URL is that tracker's
// documented off switch, and its RSS answer is the plain empty feed - so it must
// hold even while nothing has ever loaded (here a malformed first snapshot, the
// startup-fault state). Answering the snapshot-unavailable Torznab error there
// would fail an operator's Prowlarr save-test for a tracker they deliberately
// turned off, on a fault that has nothing to do with it. An ENABLED tracker still
// gets the fault, which is the assertion that keeps this from reading as "the
// gate was simply removed".
func TestDisabledTrackerFeedIsNotGatedBySnapshotState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write malformed snapshot: %v", err)
	}
	// Nyaa on, AnimeBytes off.
	ix := warmedIndexer(&Config{SnapshotPath: path, NyaaTorznabURL: "http://prowlarr/1/api"}, nil, nil)

	rss := url.Values{}
	items, stats, fault := ix.query(t.Context(), rss, upstreamAB)
	if fault != nil {
		t.Errorf("disabled tracker RSS fault = %+v, want none (the off switch is config, not snapshot state)", fault)
	}
	if len(items) != 0 {
		t.Errorf("disabled tracker RSS items = %d, want 0", len(items))
	}
	if !stats.answered || !stats.feed {
		t.Errorf("disabled tracker RSS stats = %+v, want an answered feed request", stats)
	}
	if _, _, fault := ix.query(t.Context(), rss, upstreamNyaa); fault == nil {
		t.Error("configured tracker RSS fault = nil while nothing has loaded, want the snapshot-unavailable fault")
	}
}
