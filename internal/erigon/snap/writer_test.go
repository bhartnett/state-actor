package snap

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// memFiles is a FileSet that keeps each domain's entries in memory, for tests of snap's own logic.
// The on-disk format is Erigon's and is tested in internal/erigon/erigonfiles.
type memFiles map[Domain][][2][]byte

func (m memFiles) WriteDomain(_ context.Context, _, _ string, d Domain, _ StepRange, _ uint64, _ uint32,
	entries func(yield func(DomainEntry) bool)) error {
	entries(func(e DomainEntry) bool {
		m[d] = append(m[d], [2][]byte{append([]byte(nil), e.Key...), append([]byte(nil), e.Value...)})
		return true
	})
	return nil
}

// TestNewWriterSaltAndSettings checks NewWriter lays down salt-state.txt and erigondb.toml, reuses
// the on-disk salt on re-open, and refuses a different explicit salt.
func TestNewWriterSaltAndSettings(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, Settings{Seed: 42, Files: memFiles{}})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()
	for _, want := range []string{
		filepath.Join(SnapshotsDir(dir), "salt-state.txt"),
		filepath.Join(SnapshotsDir(dir), "erigondb.toml"),
	} {
		if _, err := os.Stat(want); err != nil {
			t.Fatalf("expected file %s: %v", want, err)
		}
	}

	w2, err := NewWriter(dir, Settings{Seed: 7, Files: memFiles{}})
	if err != nil {
		t.Fatalf("NewWriter (re-open): %v", err)
	}
	defer w2.Close()
	if w2.Salt() != w.Salt() {
		t.Errorf("re-open with another seed: salt %d, on-disk %d", w2.Salt(), w.Salt())
	}

	if _, err := NewWriter(dir, Settings{Seed: 42, Salt: w.Salt() + 1, Files: memFiles{}}); err == nil {
		t.Errorf("NewWriter with mismatched salt: expected error, got nil")
	}
}

func TestDeriveSaltFromSeedDeterministic(t *testing.T) {
	if DeriveSaltFromSeed(42) != DeriveSaltFromSeed(42) {
		t.Error("DeriveSaltFromSeed not deterministic")
	}
	if DeriveSaltFromSeed(42) == DeriveSaltFromSeed(43) {
		t.Error("DeriveSaltFromSeed returns same salt for different seeds")
	}
}
