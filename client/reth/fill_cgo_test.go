//go:build cgo_reth

package reth

import (
	"testing"

	"github.com/erigontech/mdbx-go/mdbx"
)

func TestOpenEnvs_PreferWAFDisabled(t *testing.T) {
	tmp := t.TempDir()
	envs, err := OpenEnvs(tmp, true)
	if err != nil {
		t.Fatalf("OpenEnvs: %v", err)
	}
	defer envs.Close()

	got, err := envs.Mdbx.GetOption(mdbx.OptPreferWafInsteadofBalance)
	if err != nil {
		t.Fatalf("GetOption(OptPreferWafInsteadofBalance): %v", err)
	}
	if got != 0 {
		t.Errorf("prefer_waf_insteadof_balance = %d, want 0", got)
	}
}

// TestFillController_SwitchesReserveWithinTxn checks the one guarantee the
// writer depends on: split_reserve can be changed between Put calls inside a
// single write txn, on the goroutine holding it, and each change is visible
// via GetOption immediately — no txn boundary needed.
func TestFillController_SwitchesReserveWithinTxn(t *testing.T) {
	tmp := t.TempDir()
	envs, err := OpenEnvs(tmp, true)
	if err != nil {
		t.Fatalf("OpenEnvs: %v", err)
	}
	defer envs.Close()

	targets := map[string]float64{"HashedAccounts": 0.6, "AccountsTrie": 0.9}
	fc, err := newFillController(envs.Mdbx, targets)
	if err != nil {
		t.Fatalf("newFillController: %v", err)
	}
	wantA, _ := reserveFor(0.6)
	wantB, _ := reserveFor(0.9)
	if wantA == wantB {
		t.Fatal("test fixture picked equal reserves for the two tables; adjust the fills")
	}

	check := func(table string, want uint64) {
		t.Helper()
		if err := fc.use(table); err != nil {
			t.Fatalf("use(%s): %v", table, err)
		}
		got, err := envs.Mdbx.GetOption(mdbxOptSplitReserve)
		if err != nil {
			t.Fatalf("GetOption after use(%s): %v", table, err)
		}
		if got != want {
			t.Errorf("after use(%s): split_reserve=%d, want %d", table, got, want)
		}
	}

	err = envs.Mdbx.Update(func(txn *mdbx.Txn) error {
		check("HashedAccounts", wantA)
		check("AccountsTrie", wantB)
		check("HashedAccounts", wantA) // switching back works too
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func TestFillController_UnlistedTableStaysDense(t *testing.T) {
	tmp := t.TempDir()
	envs, err := OpenEnvs(tmp, true)
	if err != nil {
		t.Fatalf("OpenEnvs: %v", err)
	}
	defer envs.Close()

	fc, err := newFillController(envs.Mdbx, map[string]float64{"HashedAccounts": 0.6})
	if err != nil {
		t.Fatalf("newFillController: %v", err)
	}
	err = envs.Mdbx.Update(func(txn *mdbx.Txn) error {
		if err := fc.use("Bytecodes"); err != nil { // not in targets
			return err
		}
		got, gerr := envs.Mdbx.GetOption(mdbxOptSplitReserve)
		if gerr != nil {
			return gerr
		}
		if got != 0 {
			t.Errorf("split_reserve for unlisted table = %d, want 0", got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
}
