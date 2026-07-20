package release

import (
	"strings"
	"testing"
)

// FuzzClassifyRawURL fuzzes the raw-URL structural classifier over untrusted
// SeaDex link strings with bounded-output and metamorphic invariants (never a
// reimplementation of the class rules): every input lands in exactly one enum
// class; the private parsed object is nil exactly for the two no-facts
// classes (Empty, Malformed) and the exported semantic facts (Scheme, Port,
// HasUserInfo) are zero whenever it is; Host carries no ASCII uppercase (the
// fold is ASCII-only by design, so non-ASCII homograph bytes survive to the
// fail-closed IsASCIIHost gates downstream) and is empty
// for every class that carries
// no extractable host evidence (Empty, Malformed, HiddenHost, Relative) while
// an absolute form always carries its host; HostUnrecoverable marks only
// schemeless-host forms; Trimmed never carries surrounding whitespace; and
// re-classifying the already-trimmed string reproduces the same facts
// (whitespace trimming is the only pre-parse rewrite).
func FuzzClassifyRawURL(f *testing.F) {
	f.Add("   ")
	f.Add("https://nyaa.si/view/1")
	f.Add("https://nyaa.si/\x7f")
	f.Add("https:/animebytes.tv/x")
	f.Add("animebytes.tv:443/x")
	f.Add("https://:443/x")
	f.Add("javascript:alert(1)")
	f.Add("//animebytes.tv/x")
	f.Add("///animebytes.tv/x")
	f.Add(`\\animebytes.tv/x`)
	f.Add(`/\animebytes.tv/x`)
	f.Add("animebytes.tv/torrents.php?id=1")
	f.Add("ANIMEBYTES.TV/torrents.php?id=1")
	f.Add("https://an\u0130mebytes.tv/x")
	f.Add("?x:y")
	f.Add("foo bar@animebytes.tv/x")
	f.Add("/torrents.php?id=1")
	f.Add("1a:b")
	f.Fuzz(func(t *testing.T, raw string) {
		fm := ClassifyRawURL(raw)

		switch fm.Class {
		case URLFormEmpty, URLFormMalformed, URLFormAbsolute, URLFormHiddenHost,
			URLFormProtocolRelative, URLFormSchemelessHost, URLFormRelative:
		default:
			t.Errorf("Class = %v outside the enum for %q", fm.Class, raw)
		}
		if fm.Trimmed != strings.TrimSpace(fm.Trimmed) {
			t.Errorf("Trimmed = %q still carries surrounding whitespace", fm.Trimmed)
		}
		if gotNil := fm.parsed == nil; gotNil != (fm.Class == URLFormEmpty || fm.Class == URLFormMalformed) {
			t.Errorf("parsed nil = %v for class %v, want nil exactly for Empty/Malformed", gotNil, fm.Class)
		}
		if fm.parsed == nil && (fm.Scheme != "" || fm.Port != "" || fm.HasUserInfo) {
			t.Errorf("Scheme=%q Port=%q HasUserInfo=%v without a parse for %q, want zero facts", fm.Scheme, fm.Port, fm.HasUserInfo, raw)
		}
		if fm.Host != asciiLower(fm.Host) {
			t.Errorf("Host = %q carries ASCII uppercase; the ASCII-only fold must apply", fm.Host)
		}
		switch fm.Class {
		case URLFormEmpty, URLFormMalformed, URLFormHiddenHost, URLFormRelative:
			if fm.Host != "" {
				t.Errorf("Host = %q for class %v, want empty (the class carries no extractable host evidence)", fm.Host, fm.Class)
			}
		case URLFormAbsolute:
			if fm.Host == "" {
				t.Errorf("Host empty for URLFormAbsolute input %q", raw)
			}
		}
		if fm.HostUnrecoverable && fm.Class != URLFormSchemelessHost {
			t.Errorf("HostUnrecoverable set on class %v, want only URLFormSchemelessHost", fm.Class)
		}

		again := ClassifyRawURL(fm.Trimmed)
		if again.Class != fm.Class || again.Host != fm.Host || again.Trimmed != fm.Trimmed ||
			again.Scheme != fm.Scheme || again.Port != fm.Port || again.HasUserInfo != fm.HasUserInfo ||
			again.HasBackslash != fm.HasBackslash || again.HostUnrecoverable != fm.HostUnrecoverable {
			t.Errorf("ClassifyRawURL(%q) is not stable on its own Trimmed form %q", raw, fm.Trimmed)
		}
	})
}
