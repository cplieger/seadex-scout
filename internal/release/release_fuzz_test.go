package release

import (
	"strings"
	"testing"

	"github.com/cplieger/seadex-scout/internal/tracker"
)

// FuzzClassify fuzzes the pure classifier over untrusted SeaDex/arr strings
// (release names, entry notes, group, tracker, MediaInfo codec) and asserts the
// bounded-output and cross-function invariants the compare and audit layers
// rely on: Kind/TrackerType/Codec/Resolution stay inside their enums, Group is
// never empty (the NOGRP fallback), the classified group is never PROVEN
// divergent from its own raw group under GroupsOverlap (a known group matches
// itself; an unknown-evidence group is indeterminate, never None), NormalizeGroup
// is idempotent, a bounded remux token in the release name always classifies
// remux (per-file evidence wins), a parsed resolution always ranks above 0 in
// ResolutionRank, and no text can ever set DualAudio (the structured input
// flag, unset here, is its only source).
func FuzzClassify(f *testing.F) {
	f.Add("Show 1080p BDRemux [Dual Audio]", "best remux available", "PMR", "Nyaa", "")
	f.Add("Show x265 crf18", "", "", "AB", "HEVC")
	f.Add("", "", "", "", "")
	f.Add("Show.720p.AVC.4500 kbps", "notes", "NOGRP", "SomeTracker", "")
	f.Add("Individual Circumstances 2160p", "", "  ", "animetosho", "h.264")
	f.Add("Show S01 PREMUX 1080p", "", "PMR", "Nyaa", "")
	f.Add("Show 1080p x265", "grab the remux", "LostYears", "AB", "")
	f.Add("Show 480p", "crf 18 encode", "no_group", "RuTracker", "avc")
	f.Fuzz(func(t *testing.T, name, notes, group, trackerName, codec string) {
		rel := Classify(&Input{Names: []string{name}, Notes: notes, Group: group, Tracker: trackerName, VideoCodec: codec})

		switch rel.Kind {
		case KindRemux, KindEncode, KindUnknown:
		default:
			t.Errorf("Kind = %q outside the enum", rel.Kind)
		}
		switch rel.TrackerType {
		case tracker.Public, tracker.Private, tracker.Unknown:
		default:
			t.Errorf("TrackerType = %q outside the enum", rel.TrackerType)
		}
		if rel.Codec != "" && rel.Codec != "x265" && rel.Codec != "x264" {
			t.Errorf("Codec = %q, want one of \"\", x265, x264", rel.Codec)
		}
		switch rel.Resolution {
		case "", "2160p", "1440p", "1080p", "720p", "480p":
		default:
			t.Errorf("Resolution = %q outside the parsed set", rel.Resolution)
		}
		if rel.Resolution != "" && ResolutionRank(rel.Resolution) <= 0 {
			t.Errorf("ResolutionRank(%q) = %d, want > 0 for a parsed resolution", rel.Resolution, ResolutionRank(rel.Resolution))
		}
		if rel.Group == "" {
			t.Errorf("Group is empty for raw group %q; the NOGRP fallback must always apply", group)
		}
		if rel.Reason == "" {
			t.Error("Reason is empty; every classification must record why")
		}
		if rel.DualAudio {
			t.Errorf("DualAudio = true from text alone (name %q, notes %q); the structured input flag is the only source", name, notes)
		}

		ng := NormalizeGroup(rel.Group)
		if ng == "" {
			t.Errorf("NormalizeGroup(%q) = empty; a classified group must normalize non-empty", rel.Group)
		}
		if NormalizeGroup(ng) != ng {
			t.Errorf("NormalizeGroup not idempotent: %q -> %q", ng, NormalizeGroup(ng))
		}
		switch overlap := GroupsOverlap([]string{rel.Group}, []string{group}); {
		case overlap == OverlapNone:
			t.Errorf("classified group %q proven divergent from its own raw group %q", rel.Group, group)
		case overlap == OverlapKnown && ng == noGroupNormalized:
			t.Errorf("unknown-evidence group %q read as a proven match against %q", rel.Group, group)
		}

		// Contract: per-file name evidence wins for the file, so a
		// delimiter-bounded remux token in the NAME must always classify remux
		// (a space-bounded token is a conservative sufficient condition that
		// does not reimplement the production tokenizer).
		if strings.Contains(" "+strings.ToLower(name)+" ", " remux ") && rel.Kind != KindRemux {
			t.Errorf("bounded remux marker in name but Kind = %q", rel.Kind)
		}
	})
}
