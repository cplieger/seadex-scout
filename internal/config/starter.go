package config

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// feedKeyBytes is the entropy behind a generated feed_api_key: 16 bytes renders as
// 32 hex characters, the shape the config comment recommends (`openssl rand -hex 16`).
const feedKeyBytes = 16

// SeedStarter returns the starter config with a freshly generated feed_api_key
// substituted for the example's empty one.
//
// The key gates the Torznab feed, whose /ab responses embed the operator's AnimeBytes
// passkey in every download link, and it is the ONE credential here the operator
// invents rather than copies - so generating it removes the weak-key possibility at
// the only moment the app authors this file. Deliberately scoped to the starter write:
// an empty key on a configured feed stays the hard validation error it is, and the
// committed config.example.yaml keeps its empty value so no key is ever published.
func SeedStarter(example []byte) ([]byte, error) {
	buf := make([]byte, feedKeyBytes)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("generate feed_api_key for the starter config: %w", err)
	}
	// Anchored on the example's exact empty-value spelling and applied once, so a
	// future edit to that line fails the starter test rather than writing an unkeyed config.
	const placeholder = `feed_api_key: ""`
	replacement := fmt.Sprintf("feed_api_key: %q", hex.EncodeToString(buf))
	seeded := bytes.Replace(example, []byte(placeholder), []byte(replacement), 1)
	if bytes.Equal(seeded, example) {
		return nil, fmt.Errorf("starter config no longer contains %s; cannot seed a feed_api_key", placeholder)
	}
	return seeded, nil
}
