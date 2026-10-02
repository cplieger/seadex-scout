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

// TestABEvidenceAdapterReadsRawEvidence pins the third adapter's policy surface at
// its defining site: an AB label or definitive raw-URL host evidence grades
// ABDefinite, a hidden-host host:port form grades ABAmbiguous, an honest or empty
// URL grades ABNone. The adapter must feed the RAW upstream URL to the host
// cross-check: PublishURL(t) drops the schemeless AB form under a public label to
// "", which would grade that case ABNone.
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

// TestPublishRefusalNamesTheCause pins the adapter that carries the publisher's
// refusal reason to its two diagnostic consumers: the audit row marker and the
// SeaDex client's catalogue WARN must be able to tell a tracker this build does not
// carry (remedy: a table entry) from an unvouchable url (remedy: fix the SeaDex
// record), and the link half stays byte-identical to PublishURL.
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

func TestPayloadRevision(t *testing.T) {
	episodes := func(format string, n int) []seadex.File {
		files := make([]seadex.File, 0, n)
		for i := 1; i <= n; i++ {
			files = append(files, seadex.File{Name: fmt.Sprintf(format, i), Length: 1000})
		}
		return files
	}
	tests := []struct {
		desc  string
		files []seadex.File
		want  release.Revision
	}{
		{"every episode v2", episodes("[G] Show - S01E%02dv2 (BD 1080p).mkv", 12), release.Revision{Version: 2, Marker: release.RevisionVersion}},
		{"one reissued episode in an otherwise original pack", mixedPack(), release.Revision{Version: 2, Marker: release.RevisionVersion}},
		{"no episode reissued", episodes("[G] Show - %02d (BD).mkv", 12), release.Revision{Version: 1, Marker: release.RevisionNone}},
		{"creditless extra never votes", append(episodes("[G] Show - %02d (BD).mkv", 2), seadex.File{Name: "[G] Show - NCOP v3.mkv", Length: 1000}), release.Revision{Version: 1, Marker: release.RevisionNone}},
		{"every file repack", episodes("86.Eighty.Six.S01E%02d.REPACK.1080p.Blu-ray.Opus2.0.x265-koala.mkv", 3), release.Revision{Version: 2, Marker: release.RevisionRepack}},
		{"no files", nil, release.Revision{}},
		{"directory-bearing names", []seadex.File{{Name: "Show v3/[G] Show - 01v2.mkv", Length: 1000}}, release.Revision{Version: 2, Marker: release.RevisionVersion}},
		{"reissued episode beside an over-long premiere", []seadex.File{
			{Name: "[G] Show - 01 (BD).mkv", Length: 3000},
			{Name: "[G] Show - 02v2 (BD).mkv", Length: 1000},
			{Name: "[G] Show - 03 (BD).mkv", Length: 1000},
		}, release.Revision{Version: 2, Marker: release.RevisionVersion}},
	}
	for _, tc := range tests {
		if got := PayloadRevision(tc.files); got != tc.want {
			t.Errorf("PayloadRevision(%s) = %+v, want %+v", tc.desc, got, tc.want)
		}
	}
}

// mixedPack is a 12-episode pack whose group reissued one episode: eleven
// untokened files and episode 07 as v2.
func mixedPack() []seadex.File {
	files := make([]seadex.File, 0, 12)
	for i := 1; i <= 12; i++ {
		token := ""
		if i == 7 {
			token = "v2"
		}
		files = append(files, seadex.File{Name: fmt.Sprintf("[G] Show - %02d%s (BD 1080p).mkv", i, token), Length: 1000})
	}
	return files
}

func TestBestRevisions(t *testing.T) {
	v2Files := []seadex.File{{Name: "[UDF] 91 Days - 04v2 (BDRip 1080p).mkv", Length: 1000}}
	plain := []seadex.File{{Name: "[G] Show - 01 (BD).mkv", Length: 1000}}
	originalPack := []seadex.File{{Name: "[G] Show - 01 (BD).mkv", Length: 1000}, {Name: "[G] Show - 02 (BD).mkv", Length: 1000}}
	entry := &seadex.Entry{Torrents: []seadex.Torrent{
		{ReleaseGroup: "UDF", Tracker: "Nyaa", IsBest: true, Files: v2Files},
		{ReleaseGroup: "udf", Tracker: "AB", IsBest: true, Files: v2Files},
		{ReleaseGroup: "G", IsBest: true, Files: originalPack},
		{ReleaseGroup: "G", IsBest: true, Files: []seadex.File{{Name: "[G] Show - 02v2 (BD).mkv", Length: 1000}}},
		{ReleaseGroup: "O", IsBest: true, Files: originalPack},
		{ReleaseGroup: "K", IsBest: true, Files: []seadex.File{{Name: "[K] Show - 01v3 (BD).mkv", Length: 1000}}},
		{ReleaseGroup: "K", IsBest: false, Files: plain},
		{ReleaseGroup: "Z", IsBest: true, Files: v2Files},
		{ReleaseGroup: "Z", IsBest: true},
		{ReleaseGroup: "", IsBest: true, Files: v2Files},
		{ReleaseGroup: "Alt", IsBest: false, Files: v2Files},
	}}
	got := BestRevisions(entry)
	want := map[string]release.Revision{
		"udf":   {Version: 2, Marker: release.RevisionVersion},
		"g":     {Version: 2, Marker: release.RevisionVersion},
		"o":     {Version: 1, Marker: release.RevisionNone},
		"k":     {Version: 3, Marker: release.RevisionVersion},
		"z":     {},
		"nogrp": {Version: 2, Marker: release.RevisionVersion},
	}
	if len(got) != len(want) {
		t.Errorf("BestRevisions() = %+v, want %+v", got, want)
	}
	for group, rev := range want {
		if got[group] != rev {
			t.Errorf("BestRevisions()[%q] = %+v, want %+v", group, got[group], rev)
		}
	}
}

func TestTorrentCarriesFileRevisionNeverNotes(t *testing.T) {
	entry := &seadex.Entry{Notes: "the v3 batch fixes the subtitles; REPACK of episode 4"}
	torrent := &seadex.Torrent{ReleaseGroup: "G", Files: []seadex.File{{Name: "[G] Show - 01v2 (BD 1080p).mkv", Length: 1000}}}
	if got := Torrent(entry, torrent).Revision; got != (release.Revision{Version: 2, Marker: release.RevisionVersion}) {
		t.Errorf("Torrent().Revision = %+v, want v2 from the file name", got)
	}
	torrent.Files = []seadex.File{{Name: "[G] Show - 01 (BD 1080p).mkv", Length: 1000}}
	if got := Torrent(entry, torrent).Revision; got != (release.Revision{Version: 1, Marker: release.RevisionNone}) {
		t.Errorf("Torrent().Revision with unversioned files = %+v, want v1 (notes are not evidence)", got)
	}
}
