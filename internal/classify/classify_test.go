package classify

import (
	"fmt"
	"testing"

	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/tracker"
	"github.com/cplieger/seadex-scout/internal/trackerlink"
)

func TestTorrentBuildsSharedReleaseInput(t *testing.T) {
	entry := &seadex.Entry{Notes: "BD remux noted by SeaDex"}
	torrent := &seadex.Torrent{
		ReleaseGroup: "SubsPlease",
		Tracker:      "Nyaa",
		DualAudio:    true,
		Files: []seadex.File{
			{Name: "[SubsPlease] Frieren - 01 [1080p][HEVC].mkv"},
			{Name: ""},
		},
	}

	got := Torrent(entry, torrent)

	if got.Group != "SubsPlease" {
		t.Errorf("Torrent() group = %q, want SubsPlease", got.Group)
	}
	if got.Tracker != "Nyaa" || got.TrackerType != tracker.Public {
		t.Errorf("Torrent() tracker = %q/%q, want Nyaa/public", got.Tracker, got.TrackerType)
	}
	if got.Resolution != "1080p" {
		t.Errorf("Torrent() resolution = %q, want 1080p", got.Resolution)
	}
	if got.Codec != "x265" {
		t.Errorf("Torrent() codec = %q, want x265", got.Codec)
	}
	// Notes scoping: the SeaDex entry notes say "remux", but the per-file name
	// carries an HEVC encode marker, and per-file evidence wins for the file
	// (entry-wide notes only fill gaps).
	if got.Kind != release.KindEncode {
		t.Errorf("Torrent() kind = %q, want encode (per-file HEVC marker beats the entry-notes remux)", got.Kind)
	}
	if !got.DualAudio {
		t.Error("Torrent() must preserve the SeaDex dual-audio flag")
	}
}

// TestTorrentNotesFillGapWhenFilesCarryNoMarker pins the gap-filling half of
// the notes-scoping contract: when the torrent's file names carry no remux or
// encode marker, the entry-wide SeaDex notes classify the release.
func TestTorrentNotesFillGapWhenFilesCarryNoMarker(t *testing.T) {
	entry := &seadex.Entry{Notes: "BD remux noted by SeaDex"}
	torrent := &seadex.Torrent{
		ReleaseGroup: "PMR",
		Tracker:      "Nyaa",
		Files:        []seadex.File{{Name: "Frieren - 01 (1080p).mkv"}},
	}

	got := Torrent(entry, torrent)

	if got.Kind != release.KindRemux {
		t.Errorf("Torrent() kind = %q, want remux from entry notes when the file names carry no marker", got.Kind)
	}
}

// TestTorrentDualAudioStructuredFieldOnly pins the dual-audio sourcing at the
// adapter: the structured per-torrent SeaDex field is the only evidence — a
// flagged torrent classifies dual-audio whatever the text says, and an
// unflagged torrent never picks it up from the entry-wide notes or a file
// name, because notes describe every release in the entry and can even negate
// the marker ("lacks dual audio").
func TestTorrentDualAudioStructuredFieldOnly(t *testing.T) {
	tests := []struct {
		name    string
		notes   string
		file    string
		flagged bool
		want    bool
	}{
		{name: "flagged torrent with no text marker", notes: "", file: "Show - 01 [1080p].mkv", flagged: true, want: true},
		{name: "flagged torrent with negating notes", notes: "lacks dual audio", file: "Show - 01 [1080p].mkv", flagged: true, want: true},
		{name: "unflagged torrent with dual audio notes", notes: "this release is dual audio", file: "Show - 01 [1080p].mkv", flagged: false, want: false},
		{name: "unflagged torrent with negating notes", notes: "lacks dual audio", file: "Show - 01 [1080p].mkv", flagged: false, want: false},
		{name: "unflagged torrent with dual audio file name", notes: "", file: "Show - 01 [1080p][Dual Audio].mkv", flagged: false, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := &seadex.Entry{Notes: tt.notes}
			torrent := &seadex.Torrent{
				ReleaseGroup: "PMR",
				Tracker:      "Nyaa",
				DualAudio:    tt.flagged,
				Files:        []seadex.File{{Name: tt.file}},
			}
			if got := Torrent(entry, torrent).DualAudio; got != tt.want {
				t.Errorf("Torrent() DualAudio = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestTorrentPrimaryPayloadIgnoresSmallExtraMarker pins the primary-payload
// selection: a best BD encode whose payload is twelve similarly-sized HEVC
// episodes plus one small BDRemux-named NCED extra must classify from the
// episodes (KindEncode), not let the tiny extra's remux marker override the
// whole recommendation (which would wrongly drop it under exclude_remux).
func TestTorrentPrimaryPayloadIgnoresSmallExtraMarker(t *testing.T) {
	files := make([]seadex.File, 0, 13)
	for i := 1; i <= 12; i++ {
		files = append(files, seadex.File{
			Name:   fmt.Sprintf("Show - %02d [1080p][HEVC].mkv", i),
			Length: 1_400_000_000 + int64(i)*1_000_000,
		})
	}
	files = append(files, seadex.File{Name: "Show - NCED01 [BDRemux].mkv", Length: 90_000_000})
	torrent := &seadex.Torrent{ReleaseGroup: "cappybara", Tracker: "Nyaa", Files: files}

	got := Torrent(&seadex.Entry{}, torrent)

	if got.Kind != release.KindEncode {
		t.Errorf("Torrent() kind = %q, want encode (a small NCED extra's BDRemux marker must not override the episode payload)", got.Kind)
	}
	if got.Resolution != "1080p" {
		t.Errorf("Torrent() resolution = %q, want 1080p from the primary payload", got.Resolution)
	}
}

// TestTorrentLargeUnicodeCreditlessExtraStaysEncode pins the classification
// consequence of the İ fold: a CREDİTLESS extra large enough to pass the
// size refinement must still be excluded by the type gate, so its BDRemux
// marker cannot flip an x265 episode payload to remux (and invert an
// operator's exclude_remux filter).
func TestTorrentLargeUnicodeCreditlessExtraStaysEncode(t *testing.T) {
	files := make([]seadex.File, 0, 13)
	for i := 1; i <= 12; i++ {
		files = append(files, seadex.File{
			Name:   fmt.Sprintf("Show - %02d [1080p][x265].mkv", i),
			Length: 1_400_000_000 + int64(i)*1_000_000,
		})
	}
	files = append(files, seadex.File{Name: "Show - CRED\u0130TLESS01v2 [BDRemux].mkv", Length: 1_400_000_000})
	torrent := &seadex.Torrent{ReleaseGroup: "cappybara", Tracker: "Nyaa", Files: files}

	got := Torrent(&seadex.Entry{}, torrent)

	if got.Kind != release.KindEncode {
		t.Errorf("Torrent() kind = %q, want encode (a payload-sized CREDİTLESS extra's BDRemux marker must not override the episode payload)", got.Kind)
	}
}

// TestTorrentUnderscoreDelimitedCreditlessExtraDoesNotVote pins the boundary
// semantics of the creditless type gate: underscore is a scene delimiter for
// the rest of the classification stack, so an underscore-delimited NCED extra
// must be excluded like any other creditless file — its remux marker cannot
// outrank the episode payload's encode marker.
func TestTorrentUnderscoreDelimitedCreditlessExtraDoesNotVote(t *testing.T) {
	torrent := &seadex.Torrent{Files: []seadex.File{
		{Name: "Show_01_[1080p][x265].mkv", Length: 1000},
		{Name: "Show_NCED_01_[BDRemux].mkv", Length: 900},
	}}
	got := Torrent(&seadex.Entry{}, torrent)
	if got.Kind != release.KindEncode {
		t.Errorf("Torrent() kind = %q, want encode (an underscore-delimited NCED extra must not vote)", got.Kind)
	}
}

// TestFallbackPrecedence pins the shared empty-recommendation precedence at
// its defining site: theoretical beats incomplete - the one order compare's
// emptyResult and audit's rowQualifier both map their vocabulary from.
func TestFallbackPrecedence(t *testing.T) {
	tests := []struct {
		name  string
		entry seadex.Entry
		want  EntryFallback
	}{
		{"theoretical only", seadex.Entry{TheoreticalBest: "remux"}, FallbackTheoretical},
		{"theoretical wins over incomplete", seadex.Entry{TheoreticalBest: "remux", Incomplete: true}, FallbackTheoretical},
		{"incomplete only", seadex.Entry{Incomplete: true}, FallbackIncomplete},
		{"neither flag", seadex.Entry{}, FallbackNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Fallback(&tt.entry); got != tt.want {
				t.Errorf("Fallback(%+v) = %v, want %v", tt.entry, got, tt.want)
			}
		})
	}
}

// TestABVisibleAdapterGatesOnRawEvidence pins the adapter's policy surface:
// the operator toggle admits everything; with the toggle off an AB label or
// an AB host in the RAW upstream URL hides the torrent, an absolute public
// URL stays visible, an empty URL carries no host evidence to cross-check
// (visible), and a hidden-host form hides conservatively.
func TestABVisibleAdapterGatesOnRawEvidence(t *testing.T) {
	tests := []struct {
		name    string
		torrent seadex.Torrent
		include bool
		want    bool
	}{
		{"AB label hidden when off", seadex.Torrent{Tracker: "AB", URL: "/torrents.php?id=1&torrentid=2"}, false, false},
		{"AB label visible when on", seadex.Torrent{Tracker: "AB", URL: "/torrents.php?id=1&torrentid=2"}, true, true},
		{"mislabeled AB URL hidden when off", seadex.Torrent{Tracker: "Nyaa", URL: "https://animebytes.tv/torrents.php?id=1"}, false, false},
		{"public tracker with absolute URL visible when off", seadex.Torrent{Tracker: "Nyaa", URL: "https://nyaa.si/view/1"}, false, true},
		{"empty URL carries no host evidence and stays visible", seadex.Torrent{Tracker: "Nyaa", URL: ""}, false, true},
		{"hidden-host URL form hidden conservatively when off", seadex.Torrent{Tracker: "Nyaa", URL: "animebytes.tv:443/torrents.php?id=1"}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ABVisible(&tt.torrent, tt.include); got != tt.want {
				t.Errorf("ABVisible(%q, %q, %v) = %v, want %v", tt.torrent.Tracker, tt.torrent.URL, tt.include, got, tt.want)
			}
		})
	}
}

// TestObtainableAdapterPreservesRawURLForCrossCheck pins the adapter's wiring
// invariant that filter.Obtainable's own tests cannot cover: the RAW upstream
// URL must feed the AnimeBytes host cross-check (so a mislabeled schemeless AB
// URL is caught) while PublishURL supplies the actionable link. Passing the
// canonical URL to both arguments returns true here.
func TestObtainableAdapterPreservesRawURLForCrossCheck(t *testing.T) {
	torrent := &seadex.Torrent{
		Tracker: "Nyaa",
		URL:     "animebytes.tv/torrents.php?id=1&torrentid=2",
	}
	rel := &release.Release{Tracker: "Nyaa", TrackerType: tracker.Public}

	if got := Obtainable(rel, torrent, false); got {
		t.Error("Obtainable() = true, want false for a mislabeled schemeless AnimeBytes URL when AnimeBytes is disabled")
	}
}

// TestABEvidenceAdapterReadsRawEvidence pins the third adapter's policy surface
// at its defining site, mirroring the ABVisible and Obtainable adapter tests:
// an AB tracker label or definitively extracted raw-URL host evidence (absolute
// or schemeless animebytes.tv) grades ABDefinite, a hidden-host host:port form
// grades ABAmbiguous (evidence that settles nothing), and an honest public URL
// or an empty URL grades ABNone. The adapter must feed the RAW upstream URL
// (t.URL) to the host cross-check: passing PublishURL(t) instead (which drops
// the schemeless AB form under a public label to "") would grade that case
// ABNone and fail this test.
func TestABEvidenceAdapterReadsRawEvidence(t *testing.T) {
	tests := []struct {
		name    string
		torrent seadex.Torrent
		want    tracker.ABEvidence
	}{
		{"AB label is definitive", seadex.Torrent{Tracker: "AB", URL: "/torrents.php?id=1&torrentid=2"}, tracker.ABDefinite},
		{"absolute AB URL under a public label is definitive", seadex.Torrent{Tracker: "Nyaa", URL: "https://animebytes.tv/torrents.php?id=1"}, tracker.ABDefinite},
		{"schemeless AB URL under a public label is definitive", seadex.Torrent{Tracker: "Nyaa", URL: "animebytes.tv/torrents.php?id=1"}, tracker.ABDefinite},
		{"hidden-host form settles nothing", seadex.Torrent{Tracker: "Nyaa", URL: "animebytes.tv:443/torrents.php?id=1"}, tracker.ABAmbiguous},
		{"public tracker with public URL is not AB", seadex.Torrent{Tracker: "Nyaa", URL: "https://nyaa.si/view/1"}, tracker.ABNone},
		{"empty URL carries no host evidence", seadex.Torrent{Tracker: "Nyaa", URL: ""}, tracker.ABNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ABEvidence(&tt.torrent); got != tt.want {
				t.Errorf("ABEvidence(%q, %q) = %d, want %d", tt.torrent.Tracker, tt.torrent.URL, got, tt.want)
			}
		})
	}
}

// TestDivergedIncomplete pins the shared diverged-downgrade rule at its
// defining site, mirroring TestFallbackPrecedence: an incomplete entry
// downgrades a diverged comparison to the incomplete vocabulary, a complete
// entry does not. Cross-package callers (compare, audit.rowQualifier) pin the
// mapped vocabularies, but the one shared rule both map from had no test in
// its own package.
func TestDivergedIncomplete(t *testing.T) {
	if !DivergedIncomplete(&seadex.Entry{Incomplete: true}) {
		t.Error("DivergedIncomplete(incomplete entry) = false, want true")
	}
	if DivergedIncomplete(&seadex.Entry{}) {
		t.Error("DivergedIncomplete(complete entry) = true, want false")
	}
}

// TestPublishRefusalNamesTheCause pins the adapter that carries the publisher's
// refusal reason to the two diagnostic consumers (l-f127): the audit row marker
// and the SeaDex client's catalogue WARN must be able to tell a tracker this
// build does not carry (remedy: a seadex-scout table entry) from an unvouchable
// url (remedy: fix the SeaDex record), and the link half stays byte-identical to
// PublishURL so a consumer reading only the link is unaffected.
func TestPublishRefusalNamesTheCause(t *testing.T) {
	tests := []struct {
		name    string
		torrent seadex.Torrent
		want    trackerlink.Refusal
	}{
		{name: "published", torrent: seadex.Torrent{Tracker: "Nyaa", URL: "https://nyaa.si/view/1"}, want: trackerlink.RefusalNone},
		{name: "no url at all", torrent: seadex.Torrent{Tracker: "Nyaa"}, want: trackerlink.RefusalNoURL},
		{name: "tracker this build does not carry", torrent: seadex.Torrent{Tracker: "beyondhd", URL: "https://beyondhd.co/t/1"}, want: trackerlink.RefusalUnknownTracker},
		{name: "foreign host under a trusted label", torrent: seadex.Torrent{Tracker: "Nyaa", URL: "https://evil.example/view/1"}, want: trackerlink.RefusalUnvouchableURL},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			link, refusal := PublishRefusal(&tc.torrent)
			if refusal != tc.want {
				t.Errorf("PublishRefusal(%+v) refusal = %d, want %d", tc.torrent, refusal, tc.want)
			}
			if got := PublishURL(&tc.torrent); got != link {
				t.Errorf("PublishRefusal link = %q but PublishURL = %q; the two adapters must read one policy", link, got)
			}
		})
	}
}
