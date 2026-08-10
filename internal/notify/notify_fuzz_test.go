package notify

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cplieger/runesafe"
	"github.com/cplieger/seadex-scout/internal/compare"
)

// FuzzCapAttrBoundedAndSanitized fuzzes the emit path's per-attribute
// boundary over arbitrary untrusted text: every finding-line attribute
// derived from SeaDex, tracker, or library data (title, groups, tracker,
// classification_reason, URLs, info hash) rides through capAttr, and SeaDex
// admits multi-MB values. The invariants are the three properties the emitted
// Loki record depends on and that no table test can cover exhaustively:
// bounded volume (at most the budget plus the "..." marker, so one hostile
// record can never exceed a downstream log-pipeline line limit and suppress
// the warn line the better-release alert keys on), valid UTF-8 with no unsafe
// rune (no C1 terminal-escape introducer, bidi override, or partial rune
// re-minted by a naive re-cap), and honest passthrough - a value that needs
// neither the cap nor the growth re-cap must emit byte-identical to
// runesafe.Sanitize, never gaining a spurious truncation marker.
func FuzzCapAttrBoundedAndSanitized(f *testing.F) {
	f.Add("")
	f.Add("Frieren")
	f.Add("a\u009bb\u202ec\x1bd")
	f.Add("\xff\xfe\xfd")
	f.Add("https://nyaa.si/view/1?q=a\u2028b")
	f.Add(strings.Repeat("\xff", 12<<10))
	f.Add(strings.Repeat("g", maxAttrBytes))
	f.Add(strings.Repeat("\u202e", 4<<10))
	f.Fuzz(func(t *testing.T, raw string) {
		got := capAttr(raw)

		if len(got) > maxAttrBytes+len("...") {
			t.Errorf("capAttr(%d bytes) = %d bytes, want <= %d", len(raw), len(got), maxAttrBytes+len("..."))
		}
		if !utf8.ValidString(got) {
			t.Errorf("capAttr(%q) = %q, want valid UTF-8", raw, got)
		}
		for _, r := range got {
			if runesafe.IsUnsafe(r, true) {
				t.Errorf("capAttr(%q) emits unsafe rune %U", raw, r)
			}
		}
		clean := runesafe.Sanitize(raw)
		if len(raw) <= maxAttrBytes && len(clean) <= maxAttrBytes && got != clean {
			t.Errorf("capAttr(%q) = %q, want the byte-identical sanitized form %q", raw, got, clean)
		}
	})
}

// FuzzJoinLinksAttrBounded fuzzes the aggregate attribute boundary: a finding
// carries up to 512 obtainable sources, each tracker label and URL untrusted,
// so release_urls is the attribute most exposed to volume amplification. The
// invariants mirror the single-value target - bounded output and no unsafe
// rune - across an arbitrary source COUNT, which is the axis the table tests
// fix at 512.
func FuzzJoinLinksAttrBounded(f *testing.F) {
	f.Add("Nyaa", "https://nyaa.si/view/1", 2)
	f.Add("AB\u009b", "https://animebytes.tv/t/1\u202e", 7)
	f.Add("\xff", "\xff\xff", 512)
	f.Add("", "", 0)
	f.Fuzz(func(t *testing.T, tracker, url string, n int) {
		count := n % 600
		if count < 0 {
			count = -count
		}
		links := make([]compare.ReleaseLink, count)
		for i := range links {
			links[i] = compare.ReleaseLink{Tracker: tracker, URL: url}
		}

		got := joinLinksAttr(links)

		if len(got) > maxAttrBytes+len("...") {
			t.Errorf("joinLinksAttr(%d links) = %d bytes, want <= %d", count, len(got), maxAttrBytes+len("..."))
		}
		if !utf8.ValidString(got) {
			t.Errorf("joinLinksAttr(%d links) = %q, want valid UTF-8", count, got)
		}
		for _, r := range got {
			if runesafe.IsUnsafe(r, true) {
				t.Errorf("joinLinksAttr(%d links) emits unsafe rune %U", count, r)
			}
		}
	})
}

// FuzzCapAlertTextAttrBoundedAndInertMarkup fuzzes the alert-annotation text
// encoder. alerts.yaml interpolates alert_title / alert_recommended_group into
// a Discord annotation BODY, so an untrusted SeaDex title must never render as
// a link or a code span (CWE-116). The escaper emits every dangerous byte as a
// two-byte backslash escape, so walking escape pairs is an exact oracle rather
// than a second copy of the replacer - and it covers the growth re-cap
// boundary, where a cut can land inside an escape pair, which the value-level
// table cannot reach.
//
// '<', '>' and '@' are deliberately NOT in the live-markup set. The angle
// brackets are ordinary text for Discord (the Slack-mrkdwn entity half was
// dropped in l-f84), and mention delivery is controlled by the sender's
// allowed_mentions policy, not by a backslash inserted into annotation text.
func FuzzCapAlertTextAttrBoundedAndInertMarkup(f *testing.F) {
	f.Add("")
	f.Add("Frieren")
	f.Add("[security update](https://attacker.example)")
	f.Add("@everyone <@U123> & co")
	f.Add("Tiger & Bunny <script> a > b")
	f.Add(`a\b*c`)
	f.Add("a\u009bb\u202ec\x1bd")
	f.Add(strings.Repeat("*", 4<<10))
	f.Add(strings.Repeat("\xff", 12<<10))
	f.Fuzz(func(t *testing.T, raw string) {
		got := capAlertTextAttr(raw)

		if len(got) > maxAttrBytes {
			t.Errorf("capAlertTextAttr(%d bytes) = %d bytes, want <= %d", len(raw), len(got), maxAttrBytes)
		}
		if !utf8.ValidString(got) {
			t.Errorf("capAlertTextAttr(%d bytes) is not valid UTF-8", len(raw))
		}
		for i := 0; i < len(got); i++ {
			if got[i] == '\\' {
				i++ // whatever this escape covers is inert
				continue
			}
			if strings.IndexByte("`*_[]()~|", got[i]) >= 0 {
				t.Errorf("capAlertTextAttr(%d bytes) leaves live markup byte %q at offset %d", len(raw), got[i], i)
			}
		}
		for _, r := range got {
			if runesafe.IsUnsafe(r, true) {
				t.Errorf("capAlertTextAttr(%d bytes) emits unsafe rune %U", len(raw), r)
			}
		}
	})
}

// FuzzCapURLAttrBoundedAndInertDestination fuzzes the link-destination output
// encoder over arbitrary untrusted text. alerts.yaml renders arr_url /
// release_url / nyaa_url / public_url / ab_url as `[label](<attr>)`, so any
// surviving destination-breaking byte closes the destination early and the rest
// of an untrusted SeaDex URL renders as attacker-authored markdown (CWE-116).
// The invariants are the three properties that must hold for every input, which
// no per-character table can cover: bounded volume (the re-cap keeps the "..."
// marker INSIDE the budget, so the ceiling is maxAttrBytes exactly), valid UTF-8
// with no unsafe rune, and not one live destination-breaking byte left.
func FuzzCapURLAttrBoundedAndInertDestination(f *testing.F) {
	f.Add("")
	f.Add("https://nyaa.si/view/1")
	f.Add("https://evil.example/a)](javascript:alert(1))")
	f.Add("http://[fd00::1]:8989/series/frieren")
	f.Add("a\u009bb\u202ec\x1bd")
	f.Add(strings.Repeat("(", 4<<10))
	f.Add(strings.Repeat("\xff", 12<<10))
	f.Fuzz(func(t *testing.T, raw string) {
		got := capURLAttr(raw)

		if len(got) > maxAttrBytes {
			t.Errorf("capURLAttr(%d bytes) = %d bytes, want <= %d", len(raw), len(got), maxAttrBytes)
		}
		if !utf8.ValidString(got) {
			t.Errorf("capURLAttr(%d bytes) is not valid UTF-8", len(raw))
		}
		if i := strings.IndexAny(got, " \t\n\r\v\f\\`\"'()<>|"); i >= 0 {
			t.Errorf("capURLAttr(%d bytes) leaves destination-breaking byte %q at offset %d", len(raw), got[i], i)
		}
		for _, r := range got {
			if runesafe.IsUnsafe(r, true) {
				t.Errorf("capURLAttr(%d bytes) emits unsafe rune %U", len(raw), r)
			}
		}
	})
}
