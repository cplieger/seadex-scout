package release

import "testing"

// TestClassifyRawURL pins one example per structural class plus the
// backslash-canonicalization and host-extraction facts the two consumers'
// fail directions branch on (seadex publish-or-drop, filter
// extract-evidence-or-hide).
func TestClassifyRawURL(t *testing.T) {
	tests := []struct {
		name              string
		raw               string
		wantClass         URLFormClass
		wantHost          string
		wantBackslash     bool
		wantUnrecoverable bool
	}{
		{name: "empty after trimming", raw: "   ", wantClass: URLFormEmpty},
		{name: "unparseable control character", raw: "https://nyaa.si/\x7f", wantClass: URLFormMalformed},
		{name: "digit-led first segment with colon is malformed", raw: "1a:b", wantClass: URLFormMalformed},
		{name: "absolute with host", raw: " https://NYAA.si/view/1 ", wantClass: URLFormAbsolute, wantHost: "nyaa.si"},
		{name: "non-http scheme still classifies absolute", raw: "ftp://animebytes.tv/x", wantClass: URLFormAbsolute, wantHost: "animebytes.tv"},
		{name: "scheme-relative path hides its host", raw: "https:/animebytes.tv/x", wantClass: URLFormHiddenHost},
		{name: "opaque host-as-scheme hides its host", raw: "animebytes.tv:443/x", wantClass: URLFormHiddenHost},
		{name: "port-only authority hides its host", raw: "https://:443/x", wantClass: URLFormHiddenHost},
		{name: "javascript scheme is hidden-host, not absolute", raw: "javascript:alert(1)", wantClass: URLFormHiddenHost},
		{name: "protocol-relative with host", raw: "//animebytes.tv/x", wantClass: URLFormProtocolRelative, wantHost: "animebytes.tv"},
		{name: "three slashes are ambiguous protocol-relative without host", raw: "///animebytes.tv/x", wantClass: URLFormProtocolRelative},
		{name: "backslash authority canonicalizes to protocol-relative", raw: `\\animebytes.tv/x`, wantClass: URLFormProtocolRelative, wantHost: "animebytes.tv", wantBackslash: true},
		{name: "slash-backslash canonicalizes to protocol-relative", raw: `/\animebytes.tv/x`, wantClass: URLFormProtocolRelative, wantHost: "animebytes.tv", wantBackslash: true},
		{name: "schemeless host recovers the authority", raw: "animebytes.tv/torrents.php?id=1", wantClass: URLFormSchemelessHost, wantHost: "animebytes.tv"},
		{name: "query-only form is schemeless without evidence", raw: "?x:y", wantClass: URLFormSchemelessHost},
		{name: "space before @ makes the authority reparse fail", raw: "foo bar@animebytes.tv/x", wantClass: URLFormSchemelessHost, wantUnrecoverable: true},
		{name: "rooted relative path", raw: "/torrents.php?id=1", wantClass: URLFormRelative},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := ClassifyRawURL(tt.raw)
			if f.Class != tt.wantClass {
				t.Errorf("Class = %v, want %v", f.Class, tt.wantClass)
			}
			if f.Host != tt.wantHost {
				t.Errorf("Host = %q, want %q", f.Host, tt.wantHost)
			}
			if f.HasBackslash != tt.wantBackslash {
				t.Errorf("HasBackslash = %v, want %v", f.HasBackslash, tt.wantBackslash)
			}
			if f.HostUnrecoverable != tt.wantUnrecoverable {
				t.Errorf("HostUnrecoverable = %v, want %v", f.HostUnrecoverable, tt.wantUnrecoverable)
			}
		})
	}
}
