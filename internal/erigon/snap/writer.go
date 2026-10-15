package snap

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/ethereum/state-actor/internal/erigon"
)

// Writer lays out an Erigon datadir's snapshot directory (salt,
// erigondb.toml, domain dir) and hands each domain to Settings.Files.
// Construct via NewWriter; emit a domain via WriteDomain; finalize via
// Close.
//
// Re-creating a Writer over an existing datadir is supported: NewWriter
// verifies salt/erigondb invariants match what's already on disk and
// returns an error on mismatch (so we never silently emit a file set
// against a different salt than the existence filters were built with).
type Writer struct {
	datadir  string
	settings Settings
	closed   bool
}

// NewWriter validates the on-disk salt + erigondb.toml against
// Settings, creates them if absent, ensures the snapshot directory
// layout exists, and returns a Writer ready to emit domains.
//
// Defaults applied to s if zero:
//   - StepSize:          erigon.StepSize (390_625)
//   - StepsInFrozenFile: erigon.StepsInFrozenFile (256)
//   - Salt:              DeriveSaltFromSeed(s.Seed) if both Salt==0
//     and no salt-state.txt exists yet on disk
func NewWriter(datadir string, s Settings) (*Writer, error) {
	if datadir == "" {
		return nil, errors.New("snap.NewWriter: datadir is required")
	}
	if s.Files == nil {
		return nil, errors.New("snap.NewWriter: Settings.Files is required (internal/erigon/erigonfiles, cgo_erigon_commitment build)")
	}
	if s.StepSize == 0 {
		s.StepSize = erigon.StepSize
	}
	if s.StepsInFrozenFile == 0 {
		s.StepsInFrozenFile = erigon.StepsInFrozenFile
	}

	if err := EnsureSnapshotLayout(datadir); err != nil {
		return nil, err
	}

	// Salt: if on-disk salt-state.txt exists, it wins (idempotency).
	// Otherwise derive from seed (or use the caller-provided salt) and
	// persist.
	if existingSalt, err := ReadSalt(datadir); err == nil {
		if s.Salt != 0 && s.Salt != existingSalt {
			return nil, fmt.Errorf("snap.NewWriter: salt mismatch: settings=%d, on-disk=%d",
				s.Salt, existingSalt)
		}
		s.Salt = existingSalt
	} else if errors.Is(err, os.ErrNotExist) {
		if s.Salt == 0 {
			s.Salt = DeriveSaltFromSeed(s.Seed)
		}
		if err := WriteSalt(datadir, s.Salt); err != nil {
			return nil, err
		}
	} else {
		return nil, fmt.Errorf("snap.NewWriter: read salt-state.txt: %w", err)
	}

	// erigondb.toml: same idempotency contract — error on mismatch,
	// write if absent.
	if err := ensureErigonDBSettings(datadir, s.StepSize, s.StepsInFrozenFile); err != nil {
		return nil, err
	}

	return &Writer{datadir: datadir, settings: s}, nil
}

func ensureErigonDBSettings(datadir string, want, wantFrozen uint64) error {
	path := ErigonDBSettingsPath(datadir)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return WriteErigonDBSettings(datadir, want, wantFrozen)
	}
	if err != nil {
		return fmt.Errorf("snap.NewWriter: read erigondb.toml: %w", err)
	}
	// Cheap presence check — Erigon's parser is lenient on whitespace,
	// so we look for the literal "step_size = <want>" and
	// "steps_in_frozen_file = <wantFrozen>" substrings. A real
	// mismatch returns an error rather than silently overwriting.
	wantStep := fmt.Sprintf("step_size = %d", want)
	wantSteps := fmt.Sprintf("steps_in_frozen_file = %d", wantFrozen)
	body := string(raw)
	if !contains(body, wantStep) || !contains(body, wantSteps) {
		return fmt.Errorf("snap.NewWriter: erigondb.toml mismatch: want %q + %q, got %q",
			wantStep, wantSteps, body)
	}
	return nil
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Salt returns the snapshot salt that's persisted at
// <datadir>/snapshots/salt-state.txt.
func (w *Writer) Salt() uint32 { return w.settings.Salt }

// WriteDomain emits the per-domain file set for (d, r) under
// <datadir>/snapshots/domain/ through Settings.Files. entries MUST yield
// ascending keys; keyCount is how many it yields.
func (w *Writer) WriteDomain(ctx context.Context, d Domain, r StepRange, keyCount uint64, entries func(yield func(DomainEntry) bool)) error {
	if w.closed {
		return errors.New("snap.Writer: Closed")
	}
	if r.From >= r.To {
		return fmt.Errorf("snap.WriteDomain: invalid StepRange [%d, %d)", r.From, r.To)
	}
	domainDir := DomainDir(w.datadir)
	// The compressor and accessor builders write their temp files under the domain dir, on the
	// same filesystem as the output.
	if err := w.settings.Files.WriteDomain(ctx, domainDir, domainDir, d, r, keyCount, w.settings.Salt, entries); err != nil {
		return fmt.Errorf("snap.WriteDomain: %w", err)
	}
	return nil
}

// Close marks the Writer as no-longer-usable. Snapshot files are
// already fsynced at WriteDomain return; Close is reserved for future
// metadata-flush hooks.
func (w *Writer) Close() error {
	w.closed = true
	return nil
}
