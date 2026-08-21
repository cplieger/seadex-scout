package config

import (
	"bytes"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
)

// feedKeyLine matches the seeded feed_api_key assignment for the tests that
// need to read or blank the generated value.
var feedKeyLine = regexp.MustCompile(`feed_api_key: "[^"]*"`)

// starterExample is a stand-in for the composition root's embedded
// config.example.yaml: the seeding contract is anchored on the empty-value
// spelling, so the anchor line plus enough surrounding structure to prove
// nothing else is rewritten is the whole fixture this package needs.
const starterExample = "mode: daemon\nindexer:\n  feed_api_key: \"\"\n  ab_passkey: \"\"\n"

// TestSeedStarterSeedsAStrongFeedKey pins the reason the starter seeds a key at
// all: feed_api_key is the one credential in this config the operator invents
// rather than copies from another service, so a fresh install must not be able
// to start life with a weak or empty one. It asserts the properties that matter
// (present, feedKeyBytes rendered as hex, DIFFERENT on every call, and quoted so
// the file the operator is told to edit still parses) rather than any particular
// value, plus that nothing else in the example was rewritten.
func TestSeedStarterSeedsAStrongFeedKey(t *testing.T) {
	keyOf := func(t *testing.T) string {
		t.Helper()
		seeded, err := SeedStarter([]byte(starterExample))
		if err != nil {
			t.Fatalf("SeedStarter() = %v, want nil", err)
		}
		m := feedKeyLine.FindSubmatch(seeded)
		if m == nil {
			t.Fatalf("seeded starter has no feed_api_key line:\n%s", seeded)
		}
		// The only difference from the input is the seeded key, so re-blanking
		// it must reproduce the example exactly. That pins BOTH halves: nothing
		// else was rewritten, and the key really was substituted.
		if blanked := feedKeyLine.ReplaceAll(seeded, []byte(`feed_api_key: ""`)); !bytes.Equal(blanked, []byte(starterExample)) {
			t.Errorf("seeded starter differs from the example beyond feed_api_key:\n%s", blanked)
		}
		return strings.Trim(strings.TrimPrefix(string(m[0]), "feed_api_key: "), `"`)
	}

	first := keyOf(t)
	second := keyOf(t)

	if want := feedKeyBytes * 2; len(first) != want {
		t.Errorf("generated key length = %d, want %d hex characters", len(first), want)
	}
	if _, err := hex.DecodeString(first); err != nil {
		t.Errorf("generated key is not hex: %v", err)
	}
	if first == second {
		t.Error("two starters share a feed_api_key; the key must be generated per call")
	}
	// The generated key must satisfy the validator, including the placeholder
	// refusal and the strength warning's 16-character floor.
	if strings.Contains(first, "${") {
		t.Errorf("generated key looks like an env reference: %q", first)
	}
	if len(first) < 16 {
		t.Errorf("generated key is %d chars, below the strength floor the config warns at", len(first))
	}
}

// TestSeedStarterFailsWhenTheExampleChanges pins the anchor: SeedStarter
// substitutes one exact spelling, so an edit to config.example.yaml that renames
// or re-quotes that line must fail loudly here rather than silently shipping a
// starter with no key.
func TestSeedStarterFailsWhenTheExampleChanges(t *testing.T) {
	if _, err := SeedStarter([]byte("indexer:\n  feed_api_key: ''\n")); err == nil {
		t.Error("SeedStarter() = nil error for an example missing the anchored line, want an error")
	}
}
