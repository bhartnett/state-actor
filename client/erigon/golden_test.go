//go:build cgo_erigon

package erigon_test

import (
	"context"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/state-actor/client/erigon"
	"github.com/ethereum/state-actor/generator"
	stategenesis "github.com/ethereum/state-actor/genesis"
	"github.com/ethereum/state-actor/internal/e2e_testing"
)

// TestErigonGoldenStateRoot pins erigon to entitygen.CanonicalOsakaMPTRoot
// for the canonical auto-fill config. This is the cross-client invariant
// that erigon's snapshot-tier HPH commitment root equals the MPT state
// root every other client produces for the same entities. Any drift in
// the erigon writer that shifts the state root fails here and requires a
// coordinated update of CanonicalOsakaMPTRoot + all client goldens.
//
// Runs only with -tags cgo_erigon: erigon.Run execs /usr/local/bin/erigon
// and the commitment root needs the cgo vendor, so this is Docker-only.
// The cgo-suite CI job runs it alongside TestE2ESuite.
func TestErigonGoldenStateRoot(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "erigon-golden")
	if err := os.MkdirAll(dbPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// erigon.Run requires a non-nil Genesis (it writes genesis.json + the
	// block-0 header). The header does not affect the state root, which is
	// computed over the auto-fill + canonical system-contract accounts.
	g, err := stategenesis.BuildSynthetic("osaka", big.NewInt(1337), 60_000_000,
		1_700_000_000, []byte{0xde, 0xad, 0xbe, 0xef})
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}
	cfg := e2e_testing.GoldenStateRootCfg(dbPath)
	cfg.Genesis = g
	e2e_testing.AssertGoldenStateRoot(t, "erigon", cfg,
		func(ctx context.Context, cfg generator.Config) (*generator.Stats, error) {
			return erigon.Run(ctx, cfg, erigon.Options{})
		})
}

func TestErigonGoldenStateRootGiants(t *testing.T) {
	g, err := stategenesis.BuildSynthetic("osaka", big.NewInt(1337), 60_000_000,
		1_700_000_000, []byte{0xde, 0xad, 0xbe, 0xef})
	if err != nil {
		t.Fatalf("BuildSynthetic: %v", err)
	}
	cfg := e2e_testing.GoldenGiantRootCfg(t.TempDir())
	cfg.Genesis = g
	e2e_testing.AssertGoldenGiantRoot(t, "erigon", cfg,
		func(ctx context.Context, cfg generator.Config) (*generator.Stats, error) {
			return erigon.Run(ctx, cfg, erigon.Options{})
		})
}
