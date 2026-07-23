package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// newerSchemaState reports whether data is what Load classifies as valid
// newer-schema state: a JSON object envelope whose persisted "version" member
// decodes to an int beyond SchemaVersion. It reads the wire shape directly
// (a streaming token decode of the raw bytes) instead of decoding into State, so
// the oracle stays independent of production: a regression to State.Version's
// JSON tag or decoding shape changes Load's classification without silently
// changing this helper with it, and the newer-schema seeds fail instead of
// staying green. Every case-insensitive occurrence of the key must decode to
// an int and the effective (last) value must exceed SchemaVersion, mirroring
// Load's newer-schema contract: a payload with any invalid duplicate
// occurrence - a JSON null, which encoding/json otherwise accepts into a
// plain int as a silent no-op but production rejects via its *int decode, or
// a negative value, which violates the documented non-negative discriminator
// domain - is corruption, never newer-schema state.
func newerSchemaState(data []byte) bool {
	// Load's bounded read rejects an over-cap file before the version
	// discriminator can be inspected, so it is quarantined as foreign/corrupt
	// regardless of a valid newer-schema stamp (see the SchemaVersion doc).
	if len(data) > maxStateBytes {
		return false
	}
	// Load quarantines invalid UTF-8 before the newer-schema check (Save
	// only emits valid UTF-8), so such a payload is never newer-schema
	// state regardless of its version stamp.
	if !utf8.Valid(data) {
		return false
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return false
	}
	version, found := 0, false
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		key, isStr := tok.(string)
		if !isStr {
			return false
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return false
		}
		if !strings.EqualFold(key, "version") {
			continue
		}
		var decoded *int
		if err := json.Unmarshal(raw, &decoded); err != nil || decoded == nil || *decoded < 0 {
			return false
		}
		version, found = *decoded, true
	}
	if _, err := dec.Token(); err != nil {
		return false
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return false
	}
	return found && version > SchemaVersion
}

// FuzzStoreLoadQuarantine drives Load with arbitrary state-file bytes and pins
// the corruption-recovery invariants: Load never panics; a rejected payload is
// quarantined at path+".corrupt" with its original bytes and the live path is
// renamed away - EXCEPT a valid-UTF-8 decoded object whose Version is newer
// than SchemaVersion, which is valid newer-schema state (an image rollback), stays
// preserved at the live path with no .corrupt copy, and blocks Save so this
// binary cannot overwrite it; an accepted payload is never quarantined and
// stays usable - Save re-persists it (stamping SchemaVersion) and Load reads
// it back, unless HTML-escape expansion pushes the re-encoding over the
// shared cap, in which case Save's documented over-cap refusal keeps the
// file intact. Each call uses a fresh t.TempDir(), so fuzz input never
// shapes a filesystem path.
func FuzzStoreLoadQuarantine(f *testing.F) {
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte("  null\n"))
	f.Add([]byte(`[]`))
	f.Add([]byte(``))
	f.Add([]byte(`{`))
	f.Add([]byte(`{"baselined":true,"version":1}`))
	f.Add([]byte(`{"version":"not-a-number"}`))
	f.Add([]byte(`{"version":99,"version":"not-a-number"}`))
	f.Add([]byte(`{"version":99,"version":null}`))
	f.Add([]byte(`{"version":null,"Version":99}`))
	f.Add([]byte(`{"version":"bad","Version":99,"findings":{}}`))
	f.Add([]byte(`{"version":-1}`))
	f.Add([]byte(`{"version":-1,"version":99}`))
	f.Add([]byte(`{"version":99,"baselined":true}`))
	f.Add([]byte(`{"Version":99,"baselined":true}`))
	f.Add([]byte(`{"findings":"moved-member-shape","version":99}`))
	f.Add([]byte(`{"findings":{"k":{}},"shrunk_walks":3}`))
	f.Add([]byte(`{"version":99}x`))
	f.Add([]byte(`{"version":99} {"baselined":true}`))
	f.Add([]byte(`{1:2}`))
	f.Add([]byte(`{"version":99,"k":}`))
	f.Add([]byte("{\"version\":99,\"findings\":{\"bad\xffkey\":{}}}"))
	f.Add([]byte("{\"version\":99,\"k\":\"bad\xffval\"}"))
	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("write fuzz state: %v", err)
		}
		store := NewStore(path, testLogger())
		st, err := store.Load(context.Background())
		if err != nil {
			if newerSchemaState(data) {
				if _, statErr := os.Stat(path + ".corrupt"); !errors.Is(statErr, fs.ErrNotExist) {
					t.Errorf("newer-schema state %q quarantined (stat err = %v), want preserved at the live path", data, statErr)
				}
				if saveErr := store.Save(context.Background(), &State{}); saveErr == nil {
					t.Error("Save after loading newer-schema state returned nil error, want refusal")
				}
				live, readErr := os.ReadFile(path)
				if readErr != nil || string(live) != string(data) {
					t.Errorf("live path after newer-schema load = %q (err %v), want original bytes %q", live, readErr, data)
				}
				return
			}
			got, readErr := os.ReadFile(path + ".corrupt")
			if readErr != nil {
				t.Fatalf("Load(%q) errored (%v) but did not quarantine: %v", data, err, readErr)
			}
			if string(got) != string(data) {
				t.Errorf("quarantined bytes = %q, want original %q", got, data)
			}
			if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("live path still present after quarantine (stat err = %v)", statErr)
			}
			return
		}
		if _, statErr := os.Stat(path + ".corrupt"); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("accepted input was quarantined (stat err = %v)", statErr)
		}
		if saveErr := store.Save(context.Background(), &st); saveErr != nil {
			// json.Encoder HTML-escapes <, >, & (and U+2028/U+2029) into 6-byte
			// \u-sequences, so a foreign near-cap file holding them raw can be
			// Load-accepted yet legitimately re-encode past maxStateBytes. The
			// documented over-cap refusal keeps the previous file on disk; it is
			// a rejection, not a stranded state, so it is not a fuzz failure.
			if strings.Contains(saveErr.Error(), "exceeds the") {
				return
			}
			t.Fatalf("Save of a Load-accepted state failed: %v", saveErr)
		}
		again, loadErr := store.Load(context.Background())
		if loadErr != nil {
			t.Fatalf("re-Load after Save of an accepted state failed: %v", loadErr)
		}
		if again.Version != SchemaVersion {
			t.Errorf("re-loaded Version = %d, want stamped %d", again.Version, SchemaVersion)
		}
	})
}
