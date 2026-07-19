package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// newerSchemaState reports whether data is what Load classifies as valid
// newer-schema state: a JSON object envelope whose persisted "version" member
// decodes to an int beyond SchemaVersion. It reads the wire shape directly
// (a map of json.RawMessage) instead of decoding into State, so the oracle
// stays independent of production: a regression to State.Version's JSON tag
// or decoding shape changes Load's classification without silently changing
// this helper with it, and the newer-schema seeds fail instead of staying
// green.
func newerSchemaState(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return false
	}
	rawVersion, ok := envelope["version"]
	if !ok {
		return false
	}
	var version int
	if err := json.Unmarshal(rawVersion, &version); err != nil {
		return false
	}
	return version > SchemaVersion
}

// FuzzStoreLoadQuarantine drives Load with arbitrary state-file bytes and pins
// the corruption-recovery invariants: Load never panics; a rejected payload is
// quarantined at path+".corrupt" with its original bytes and the live path is
// renamed away - EXCEPT a decoded object whose Version is newer than
// SchemaVersion, which is valid newer-schema state (an image rollback), stays
// preserved at the live path with no .corrupt copy, and blocks Save so this
// binary cannot overwrite it; an accepted payload is never quarantined and
// stays usable - Save re-persists it (stamping SchemaVersion) and Load reads
// it back, so the shared maxStateBytes/envelope contract can never strand a
// state Load accepted. Each call uses a fresh t.TempDir(), so fuzz input never
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
	f.Add([]byte(`{"version":-1}`))
	f.Add([]byte(`{"version":99,"baselined":true}`))
	f.Add([]byte(`{"findings":"moved-member-shape","version":99}`))
	f.Add([]byte(`{"findings":{"k":{}},"shrunk_walks":3}`))
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
