package tracker

import (
	"strings"
	"testing"

	"github.com/cplieger/urlform"
)

// FuzzLookupByRelativeURL fuzzes the structural relative-URL tracker resolver over
// arbitrary untrusted URL strings (SeaDex-published torrent URLs), never a
// reimplementation of the shape rule: a match is always exactly the canonical AnimeBytes
// table entry, a match implies urlform classified the input as a host-less path form (so
// tracker identity from an absolute, protocol-relative or hidden-host input must come
// from the host gate), and prefixing a scheme+host onto a matching input never matches.
func FuzzLookupByRelativeURL(f *testing.F) {
	f.Add("/torrents.php?id=12345&torrentid=1167293")
	f.Add("/torrents.php?torrentid=1")
	f.Add("/TORRENTS.PHP?torrentid=1")
	f.Add("/torrents.php?id=12345")
	f.Add("/view/1918784")
	f.Add("https://animebytes.tv/torrents.php?torrentid=1")
	f.Add("//animebytes.tv/torrents.php?torrentid=1")
	f.Add("torrents.php?torrentid=1")
	f.Add(`\torrents.php?id=1&torrentid=2`)
	f.Add("")
	f.Add("/torrents.php?%gg=1&torrentid=1")
	f.Fuzz(func(t *testing.T, raw string) {
		got, ok := LookupByRelativeURL(raw)
		if !ok {
			if got.Name != "" {
				t.Errorf("LookupByRelativeURL(%q) = %+v with ok=false, want the zero Tracker", raw, got)
			}
			return
		}
		if got.Name != NameAnimeBytes {
			t.Errorf("LookupByRelativeURL(%q) = %q, want only %q can match", raw, got.Name, NameAnimeBytes)
		}
		canonical, tableOK := Lookup(NameAnimeBytes)
		if !tableOK || got.Name != canonical.Name || got.Type != canonical.Type || got.BaseURL != canonical.BaseURL {
			t.Errorf("LookupByRelativeURL(%q) = %+v, want the canonical table entry %+v", raw, got, canonical)
		}
		if c := urlform.Classify(raw).Class; c != urlform.ClassRelative && c != urlform.ClassSchemelessHost {
			t.Errorf("LookupByRelativeURL(%q) matched but urlform classifies it %v, not a host-less path form", raw, c)
		}
		if abs := "https://evil.example" + strings.TrimSpace(raw); func() bool { _, ok := LookupByRelativeURL(abs); return ok }() {
			t.Errorf("LookupByRelativeURL(%q) = true for the absolutized form of a matching relative URL: shape rule bypassed", abs)
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

// hostGateInvariants asserts the shared metamorphic and bounded-output invariants for a
// tracker host gate over one fuzz input, never a reimplementation of the dot-boundary
// rule; each check's failure message states the rule it holds. The gate resolves through
// LookupByHost, which folds case and trims whitespace, so the normalized comparisons
// here fold the same way.
func hostGateInvariants(t *testing.T, gate func(string) bool, domain, host string) {
	t.Helper()
	got := gate(host)
	if (host == domain || host == domain+".") && !got {
		t.Errorf("gate(%q) = false, want true for the canonical %s host", host, domain)
	}
	if glued := fuzzLabel(host) + "." + domain; !gate(glued) {
		t.Errorf("gate(%q) = false, want true: a valid label on an explicit .%s boundary always matches", glued, domain)
	}
	if bad := host + ".." + domain; gate(bad) {
		t.Errorf("gate(%q) = true, want false: an empty label is never a real subdomain boundary", bad)
	}
	if !got && !strings.HasPrefix(strings.TrimSpace(host), ".") && gate("evil"+host) {
		t.Errorf("gate(%q) = true for a dotless-prefix variant of non-matching host %q: suffix rule bypassed", "evil"+host, host)
	}
	// The gate trims surrounding whitespace itself, so the trailing-dot
	// metamorphic check appends the dot to the trimmed host (a dot after
	// trailing whitespace is not a DNS-root dot).
	if trimmed := strings.TrimSpace(host); !strings.HasSuffix(trimmed, ".") {
		if dotted, base := gate(trimmed+"."), gate(trimmed); dotted != base {
			t.Errorf("gate(%q) = %v but gate(%q) = %v: DNS-root trailing dot must not change the answer", trimmed, base, trimmed+".", dotted)
		}
	}
	if norm := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), "."); got && !strings.HasSuffix(norm, domain) {
		t.Errorf("gate(%q) = true but the host does not even end in %s", host, domain)
	}
}

// FuzzIsAnimeBytesHost fuzzes the AB host gate over arbitrary host strings.
// hostGateInvariants owns the invariants asserted.
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
	f.Fuzz(func(t *testing.T, host string) { hostGateInvariants(t, IsAnimeBytesHost, "animebytes.tv", host) })
}

// FuzzNyaaHostResolution fuzzes the canonical-table resolution of an untrusted URL host
// to the Nyaa tracker - the question the indexer's scopeOfHost asks (LookupByHost, then
// the resolved tracker's name), and the reason the table tolerates hostile input. The
// AnimeBytes predicate beside it survives as an export only because filter's AB evidence
// gate consumes it. hostGateInvariants owns the invariants asserted.
func FuzzNyaaHostResolution(f *testing.F) {
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
		nyaaHost := func(host string) bool {
			trk, ok := LookupByHost(host)
			return ok && trk.Name == NameNyaa
		}
		hostGateInvariants(t, nyaaHost, "nyaa.si", host)
	})
}
