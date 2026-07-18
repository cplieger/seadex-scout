package state

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// FuzzStoreLoadQuarantine drives Load with arbitrary state-file bytes and pins
// the corruption-recovery invariants: Load never panics; a rejected payload is
// quarantined at path+".corrupt" with its original bytes and the live path is
// renamed away; an accepted payload is never quarantined and stays usable -
// Save re-persists it (stamping SchemaVersion) and Load reads it back, so the
// shared maxStateBytes/envelope contract can never strand a state Load
// accepted. Each call uses a fresh t.TempDir(), so fuzz input never shapes a
// filesystem path.
func FuzzStoreLoadQuarantine(f *testing.F) {
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte("  null\n"))
	f.Add([]byte(`[]`))
	f.Add([]byte(``))
	f.Add([]byte(`{`))
	f.Add([]byte(`{"baselined":true,"version":1}`))
	f.Add([]byte(`{"version":"not-a-number"}`))
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
