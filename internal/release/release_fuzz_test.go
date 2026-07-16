package release

import (
	"strings"
	"testing"
)

// FuzzClassify fuzzes the pure classifier over untrusted SeaDex/arr strings
// (release names, entry notes, group, tracker, MediaInfo codec) and asserts the
// bounded-output and cross-function invariants the compare and audit layers
// rely on: Kind/TrackerType/Codec/Resolution stay inside their enums, Group is
// never empty (the NOGRP fallback), the classified group always intersects its
// own raw group under NormalizeGroup, NormalizeGroup is idempotent, a bounded
// remux token in the release name always classifies remux (per-file evidence
// wins), and a parsed resolution always ranks above 0 in ResolutionRank.
func FuzzClassify(f *testing.F) {
	f.Add("Show 1080p BDRemux [Dual Audio]", "best remux available", "PMR", "Nyaa", "")
	f.Add("Show x265 crf18", "", "", "AB", "HEVC")
	f.Add("", "", "", "", "")
	f.Add("Show.720p.AVC.4500 kbps", "notes", "NOGRP", "SomeTracker", "")
	f.Add("Individual Circumstances 2160p", "", "  ", "animetosho", "h.264")
	f.Fuzz(func(t *testing.T, name, notes, group, tracker, codec string) {
		rel := Classify(&Input{Names: []string{name}, Notes: notes, Group: group, Tracker: tracker, VideoCodec: codec})

		switch rel.Kind {
		case KindRemux, KindEncode, KindUnknown:
		default:
			t.Errorf("Kind = %q outside the enum", rel.Kind)
		}
		switch rel.TrackerType {
		case TrackerPublic, TrackerPrivate, TrackerUnknown:
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

		ng := NormalizeGroup(rel.Group)
		if ng == "" {
			t.Errorf("NormalizeGroup(%q) = empty; a classified group must normalize non-empty", rel.Group)
		}
		if NormalizeGroup(ng) != ng {
			t.Errorf("NormalizeGroup not idempotent: %q -> %q", ng, NormalizeGroup(ng))
		}
		if !GroupsIntersect([]string{rel.Group}, []string{group}) {
			t.Errorf("classified group %q does not intersect its own raw group %q", rel.Group, group)
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

// FuzzIsAnimeBytesHost fuzzes the AB host gate over arbitrary host strings
// with metamorphic and bounded-output invariants (never a reimplementation of
// the dot-boundary rule): gluing an explicit ".animebytes.tv" label boundary
// onto any host always matches; gluing a dotless prefix onto a non-matching
// host never creates a match (the suffix rule cannot be bypassed without a
// label boundary); a single trailing dot never changes the answer; and a
// matching host must at least end in "animebytes.tv" after the root-dot trim.
func FuzzIsAnimeBytesHost(f *testing.F) {
	f.Add("animebytes.tv")
	f.Add("www.animebytes.tv")
	f.Add("animebytes.tv.")
	f.Add("maliciousanimebytes.tv")
	f.Add("animebytes.tv.evil.com")
	f.Add(".animebytes.tv")
	f.Add("")
	f.Fuzz(func(t *testing.T, host string) {
		got := IsAnimeBytesHost(host)

		if !IsAnimeBytesHost(host + ".animebytes.tv") {
			t.Errorf("IsAnimeBytesHost(%q) = false, want true: an explicit .animebytes.tv label boundary always matches", host+".animebytes.tv")
		}
		if !got && !strings.HasPrefix(host, ".") && IsAnimeBytesHost("evil"+host) {
			t.Errorf("IsAnimeBytesHost(%q) = true for a dotless-prefix variant of non-matching host %q: suffix rule bypassed", "evil"+host, host)
		}
		if !strings.HasSuffix(host, ".") {
			if dotted := IsAnimeBytesHost(host + "."); dotted != got {
				t.Errorf("IsAnimeBytesHost(%q) = %v but IsAnimeBytesHost(%q) = %v: DNS-root trailing dot must not change the answer", host, got, host+".", dotted)
			}
		}
		if got && !strings.HasSuffix(strings.TrimSuffix(host, "."), "animebytes.tv") {
			t.Errorf("IsAnimeBytesHost(%q) = true but the host does not even end in animebytes.tv", host)
		}
	})
}
