//go:build cgo_reth

package reth

import (
	"bytes"
	"context"
	mrand "math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/erigontech/mdbx-go/mdbx"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/generator"
	"github.com/ethereum/state-actor/internal/autofill"
	iReth "github.com/ethereum/state-actor/internal/reth"
)

func rethTestPlan(tb testing.TB, budget uint64) *autofill.Plan {
	tb.Helper()
	p, err := autofill.PlanForBudget(budget)
	if err != nil {
		tb.Fatalf("PlanForBudget(%d): %v", budget, err)
	}
	return p
}

func TestRunCgoMinimalAlloc(t *testing.T) {
	tmp := t.TempDir()
	cfg := generator.Config{
		DBPath:   tmp,
		AutoFill: rethTestPlan(t, 512<<10),
		Seed:     12345,
	}
	stats, err := RunCgo(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("RunCgo: %v", err)
	}
	if stats == nil {
		t.Fatal("RunCgo returned nil stats")
	}
	for _, p := range []string{"db/mdbx.dat", "db/database.version", "chainspec.json", "rocksdb/CURRENT"} {
		if _, err := os.Stat(filepath.Join(tmp, p)); err != nil {
			t.Errorf("expected %s: %v", p, err)
		}
	}
	if (stats.StateRoot == common.Hash{}) {
		t.Errorf("state root is zero hash")
	}
}

func TestRunCgoSyntheticEOAs(t *testing.T) {
	tmp := t.TempDir()
	plan := rethTestPlan(t, 512<<10)
	cfg := generator.Config{
		DBPath:   tmp,
		AutoFill: plan,
		Seed:     12345,
	}
	stats, err := RunCgo(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("RunCgo: %v", err)
	}
	if stats == nil {
		t.Fatal("RunCgo returned nil stats")
	}
	if stats.AccountsCreated != plan.NumEOAs {
		t.Errorf("AccountsCreated = %d, want %d", stats.AccountsCreated, plan.NumEOAs)
	}
	emptyRoot := common.HexToHash("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421")
	if stats.StateRoot == emptyRoot {
		t.Error("state root should differ from empty-MPT hash for non-empty alloc")
	}
	for _, p := range []string{"db/mdbx.dat", "db/database.version", "chainspec.json", "rocksdb/CURRENT"} {
		if _, err := os.Stat(filepath.Join(tmp, p)); err != nil {
			t.Errorf("expected %s: %v", p, err)
		}
	}
}

func TestRunCgoSyntheticEOAsDeterminism(t *testing.T) {
	// Same seed → same state root.
	plan := rethTestPlan(t, 512<<10)
	cfg := generator.Config{AutoFill: plan, Seed: 9999}
	tmp1 := t.TempDir()
	cfg.DBPath = tmp1
	s1, err := RunCgo(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	tmp2 := t.TempDir()
	cfg.DBPath = tmp2
	s2, err := RunCgo(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if s1.StateRoot != s2.StateRoot {
		t.Errorf("non-deterministic state root: %s vs %s", s1.StateRoot.Hex(), s2.StateRoot.Hex())
	}
}

func TestRunCgoSyntheticContracts(t *testing.T) {
	tmp := t.TempDir()
	plan := rethTestPlan(t, 512<<10)
	cfg := generator.Config{
		DBPath:   tmp,
		AutoFill: plan,
		Seed:     42,
	}
	stats, err := RunCgo(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("RunCgo: %v", err)
	}
	if stats == nil {
		t.Fatal("RunCgo returned nil stats")
	}
	if stats.AccountsCreated != plan.NumEOAs {
		t.Errorf("AccountsCreated = %d, want %d", stats.AccountsCreated, plan.NumEOAs)
	}
	if stats.ContractsCreated != plan.NumContracts {
		t.Errorf("ContractsCreated = %d, want %d", stats.ContractsCreated, plan.NumContracts)
	}
	emptyRoot := common.HexToHash("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421")
	if stats.StateRoot == emptyRoot {
		t.Error("state root should differ from empty-MPT for non-empty alloc")
	}

	// Verify static-files segments exist (paths per Task 4's findings).
	for _, p := range []string{
		"static_files",
		"db/mdbx.dat",
		"db/database.version",
		"chainspec.json",
		"rocksdb/CURRENT",
	} {
		if _, err := os.Stat(filepath.Join(tmp, p)); err != nil {
			t.Errorf("expected %s: %v", p, err)
		}
	}
}

func TestRunCgoContractsDeterminism(t *testing.T) {
	plan := rethTestPlan(t, 512<<10)
	cfg := generator.Config{AutoFill: plan, Seed: 1234}
	var roots [2]common.Hash
	for i := range roots {
		tmp := t.TempDir()
		cfg.DBPath = tmp
		s, err := RunCgo(context.Background(), cfg, Options{})
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		roots[i] = s.StateRoot
	}
	if roots[0] != roots[1] {
		t.Errorf("non-deterministic state root: %s vs %s", roots[0].Hex(), roots[1].Hex())
	}
}

// compactRow re-implements the HashedAccounts compact encoding directly
// (independent of hashedAccountValue, so it can catch a regression there):
// BytecodeHash=nil for EOA-shaped entities, &keccak(code) for
// contract-shaped ones (code may be empty, hashing to EmptyCodeHash).
func compactRow(nonce uint64, balance *uint256.Int, isContract bool, code []byte) []byte {
	acc := iReth.Account{Nonce: nonce, Balance: balance}
	if isContract {
		h := crypto.Keccak256Hash(code)
		acc.BytecodeHash = &h
	}
	var buf bytes.Buffer
	acc.EncodeCompact(&buf)
	return buf.Bytes()
}

// TestRunCgoHashedAccountsSortedAppend checks that HashedAccounts rows come
// from RunCgo's phase-2 cursor.Append over the addrHash-sorted stream
// (hashedAccountValue / putAccountRLP in run_cgo.go). Covers an alloc plain
// EOA, an alloc 7702-delegating EOA, an alloc storage-only contract, and an
// AutoFill mix of plain EOAs + 7702 EOAs + contracts.
func TestRunCgoHashedAccountsSortedAppend(t *testing.T) {
	tmp := t.TempDir()

	plan := rethTestPlan(t, 256<<10) // matches e2e_testing.GoldenTestBudget — guarantees NumContracts > 0.

	plainAddr := common.Address{0x01}
	delegateAddr := common.Address{0x02}
	storageAddr := common.Address{0x03}
	delegationTarget := common.Address{0x04}
	delegationCode := append([]byte{0xef, 0x01, 0x00}, delegationTarget[:]...)
	storageKey := common.HexToHash("0x01")
	storageVal := common.HexToHash("0x02")

	cfg := generator.Config{
		DBPath:   tmp,
		AutoFill: plan,
		Seed:     777,
		GenesisAccounts: map[common.Address]*types.StateAccount{
			plainAddr:    {Nonce: 1, Balance: uint256.NewInt(1_000_000), Root: types.EmptyRootHash, CodeHash: types.EmptyCodeHash.Bytes()},
			delegateAddr: {Nonce: 2, Balance: uint256.NewInt(2_000_000), Root: types.EmptyRootHash, CodeHash: types.EmptyCodeHash.Bytes()},
			storageAddr:  {Nonce: 3, Balance: uint256.NewInt(3_000_000), Root: types.EmptyRootHash, CodeHash: types.EmptyCodeHash.Bytes()},
		},
		GenesisCode: map[common.Address][]byte{
			delegateAddr: delegationCode,
		},
		GenesisStorage: map[common.Address]map[common.Hash]common.Hash{
			storageAddr: {storageKey: storageVal},
		},
	}

	stats, err := RunCgo(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatalf("RunCgo: %v", err)
	}

	// Reproduce the exact entity set RunCgo generated: alloc accounts are
	// deterministic from cfg; AutoFill draws are deterministic from the same
	// seed + draw order RunCgo uses internally (NEVER reorder — see
	// entitygen's package doc and plan.go's DrawEOA/DrawContract comments).
	want := make(map[common.Hash][]byte)
	var wantAccountBytes uint64
	addWant := func(addrHash common.Hash, nonce uint64, balance *uint256.Int, isContract bool, code []byte) {
		c := compactRow(nonce, balance, isContract, code)
		want[addrHash] = c
		wantAccountBytes += uint64(len(c))
	}
	for _, acc := range buildAllocAccounts(cfg) {
		isContract := len(acc.Code) != 0 || len(acc.Storage) != 0
		addWant(acc.AddrHash, acc.StateAccount.Nonce, acc.StateAccount.Balance, isContract, acc.Code)
	}
	rng := mrand.New(mrand.NewSource(cfg.Seed))
	for range plan.NumEOAs {
		acc := plan.DrawEOA(rng)
		addWant(acc.AddrHash, acc.StateAccount.Nonce, acc.StateAccount.Balance, len(acc.Code) != 0, acc.Code)
	}
	for range plan.NumContracts {
		acc := plan.DrawContract(rng)
		addWant(acc.AddrHash, acc.StateAccount.Nonce, acc.StateAccount.Balance, true, acc.Code)
	}
	if got, wantN := len(want), stats.AccountsCreated+stats.ContractsCreated; got != wantN {
		t.Fatalf("reconstructed %d expected entities, want %d (stats.AccountsCreated+ContractsCreated)", got, wantN)
	}

	// (d) AccountBytes accounting unchanged vs the old per-writer scheme.
	if stats.AccountBytes != wantAccountBytes {
		t.Errorf("stats.AccountBytes = %d, want %d (independently summed compact-encoding lengths)", stats.AccountBytes, wantAccountBytes)
	}

	// Open the produced MDBX env read-only and walk HashedAccounts.
	env, err := mdbx.NewEnv(mdbx.Label("chaindata"))
	if err != nil {
		t.Fatalf("mdbx.NewEnv: %v", err)
	}
	defer env.Close()
	if err := env.SetOption(mdbx.OptMaxDB, uint64(len(iReth.Tables))); err != nil {
		t.Fatalf("SetOption: %v", err)
	}
	if err := env.Open(filepath.Join(tmp, "db"), mdbx.Readonly, 0o644); err != nil {
		t.Fatalf("env.Open: %v", err)
	}

	got := make(map[common.Hash][]byte, len(want))
	var rowCount int
	if err := env.View(func(txn *mdbx.Txn) error {
		dbi, err := txn.OpenDBISimple("HashedAccounts", 0)
		if err != nil {
			return err
		}
		cur, err := txn.OpenCursor(dbi)
		if err != nil {
			return err
		}
		defer cur.Close()
		for k, v, err := cur.Get(nil, nil, mdbx.First); err == nil; k, v, err = cur.Get(nil, nil, mdbx.Next) {
			got[common.BytesToHash(k)] = append([]byte(nil), v...)
			rowCount++
		}
		return nil
	}); err != nil {
		t.Fatalf("read HashedAccounts: %v", err)
	}

	// (b) exact row count.
	if rowCount != len(want) {
		t.Errorf("HashedAccounts has %d rows, want %d", rowCount, len(want))
	}

	// (a) full k,v content equals the independently-reconstructed set.
	for addrHash, wantCompact := range want {
		gotCompact, ok := got[addrHash]
		if !ok {
			t.Errorf("HashedAccounts missing row for %s", addrHash.Hex())
			continue
		}
		if !bytes.Equal(gotCompact, wantCompact) {
			t.Errorf("HashedAccounts[%s] = %x, want %x", addrHash.Hex(), gotCompact, wantCompact)
		}
	}
	for addrHash := range got {
		if _, ok := want[addrHash]; !ok {
			t.Errorf("HashedAccounts has unexpected row for %s", addrHash.Hex())
		}
	}
}
