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
	f.Add("Show S01 PREMUX 1080p", "", "PMR", "Nyaa", "")
	f.Add("Show 1080p x265", "grab the remux", "LostYears", "AB", "")
	f.Add("Show 480p", "crf 18 encode", "no_group", "RuTracker", "avc")
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

// fuzzLabel shapes arbitrary fuzz input into a guaranteed-valid single DNS
// label (letters and digits only, never empty), so the glue invariants below
// can assert the always-matches direction without reimplementing the
// label-chain rules the gate itself enforces.
func fuzzLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "sub"
	}
	return b.String()
}

// FuzzIsAnimeBytesHost fuzzes the AB host gate over arbitrary host strings
// with metamorphic and bounded-output invariants (never a reimplementation of
// the dot-boundary rule): gluing a valid label onto an explicit
// ".animebytes.tv" boundary always matches; gluing an EMPTY label
// ("..animebytes.tv") never matches, whatever precedes it (no resolvable DNS
// name has an empty label); gluing a dotless prefix onto a non-matching host
// never creates a match (the suffix rule cannot be bypassed without a label
// boundary); a single trailing dot never changes the answer; and a matching
// host must at least end in "animebytes.tv" after the gate's own
// case/whitespace fold and root-dot trim (the gate resolves through
// LookupTrackerByHost, which folds case and trims whitespace).
func FuzzIsAnimeBytesHost(f *testing.F) {
	f.Add("animebytes.tv")
	f.Add("www.animebytes.tv")
	f.Add("animebytes.tv.")
	f.Add("maliciousanimebytes.tv")
	f.Add("animebytes.tv.evil.com")
	f.Add(".animebytes.tv")
	f.Add("a..animebytes.tv")
	f.Add("x\u00e9.animebytes.tv")
	f.Add("ANIMEBYTES.TV")
	f.Add("animebytes.tv ")
	f.Add("")
	f.Fuzz(func(t *testing.T, host string) {
		got := IsAnimeBytesHost(host)

		if glued := fuzzLabel(host) + ".animebytes.tv"; !IsAnimeBytesHost(glued) {
			t.Errorf("IsAnimeBytesHost(%q) = false, want true: a valid label on an explicit .animebytes.tv boundary always matches", glued)
		}
		if IsAnimeBytesHost(host + "..animebytes.tv") {
			t.Errorf("IsAnimeBytesHost(%q) = true, want false: an empty label is never a real subdomain boundary", host+"..animebytes.tv")
		}
		if !got && !strings.HasPrefix(strings.TrimSpace(host), ".") && IsAnimeBytesHost("evil"+host) {
			t.Errorf("IsAnimeBytesHost(%q) = true for a dotless-prefix variant of non-matching host %q: suffix rule bypassed", "evil"+host, host)
		}
		// The gate trims surrounding whitespace itself, so the trailing-dot
		// metamorphic check appends the dot to the trimmed host (a dot after
		// trailing whitespace is not a DNS-root dot).
		if trimmed := strings.TrimSpace(host); !strings.HasSuffix(trimmed, ".") {
			if dotted := IsAnimeBytesHost(trimmed + "."); dotted != got {
				t.Errorf("IsAnimeBytesHost(%q) = %v but IsAnimeBytesHost(%q) = %v: DNS-root trailing dot must not change the answer", host, got, trimmed+".", dotted)
			}
		}
		norm := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
		if got && !strings.HasSuffix(norm, "animebytes.tv") {
			t.Errorf("IsAnimeBytesHost(%q) = true but the host does not even end in animebytes.tv", host)
		}
	})
}

// FuzzIsNyaaHost fuzzes the Nyaa host classifier (an untrusted-URL-host gate
// used by indexer routing) with the same metamorphic and bounded-output
// invariants as its AnimeBytes twin: the canonical host itself (with or
// without the DNS-root dot) always matches; a valid label on an explicit
// ".nyaa.si" boundary always matches while an empty label never does; a
// dotless prefix never bypasses the suffix rule; a DNS-root trailing dot
// never changes the answer; and a matching host at least ends in "nyaa.si"
// after the gate's own case/whitespace fold and root-dot trim (the gate
// resolves through LookupTrackerByHost, which folds case and trims
// whitespace).
func FuzzIsNyaaHost(f *testing.F) {
	f.Add("nyaa.si")
	f.Add("www.nyaa.si")
	f.Add("nyaa.si.")
	f.Add("maliciousnyaa.si")
	f.Add("nyaa.si.evil.com")
	f.Add(".nyaa.si")
	f.Add("a..nyaa.si")
	f.Add("x\u00e9.nyaa.si")
	f.Add("NYAA.SI")
	f.Add("nyaa.si ")
	f.Add("")
	f.Fuzz(func(t *testing.T, host string) {
		got := IsNyaaHost(host)

		if (host == "nyaa.si" || host == "nyaa.si.") && !got {
			t.Errorf("IsNyaaHost(%q) = false, want true for the canonical Nyaa host", host)
		}
		if glued := fuzzLabel(host) + ".nyaa.si"; !IsNyaaHost(glued) {
			t.Errorf("IsNyaaHost(%q) = false, want true: a valid label on an explicit .nyaa.si boundary always matches", glued)
		}
		if IsNyaaHost(host + "..nyaa.si") {
			t.Errorf("IsNyaaHost(%q) = true, want false: an empty label is never a real subdomain boundary", host+"..nyaa.si")
		}
		if !got && !strings.HasPrefix(strings.TrimSpace(host), ".") && IsNyaaHost("evil"+host) {
			t.Errorf("IsNyaaHost(%q) = true for a dotless-prefix variant of non-matching host %q: suffix rule bypassed", "evil"+host, host)
		}
		// The gate trims surrounding whitespace itself, so the trailing-dot
		// metamorphic check appends the dot to the trimmed host (a dot after
		// trailing whitespace is not a DNS-root dot).
		if trimmed := strings.TrimSpace(host); !strings.HasSuffix(trimmed, ".") {
			if dotted := IsNyaaHost(trimmed + "."); dotted != got {
				t.Errorf("IsNyaaHost(%q) = %v but IsNyaaHost(%q) = %v: DNS-root trailing dot must not change the answer", host, got, trimmed+".", dotted)
			}
		}
		norm := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
		if got && !strings.HasSuffix(norm, "nyaa.si") {
			t.Errorf("IsNyaaHost(%q) = true but the host does not even end in nyaa.si", host)
		}
	})
}
