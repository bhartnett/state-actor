//go:build cgo_reth

package reth

import (
	"bytes"
	"fmt"

	"github.com/erigontech/mdbx-go/mdbx"

	"github.com/ethereum/state-actor/internal/entitygen"
	iReth "github.com/ethereum/state-actor/internal/reth"
)

// WriteEOAs writes the archive-mode rows for each EOA (AccountChangeSets,
// AccountsHistory) inside one atomic MDBX write transaction. Archive-only:
// callers must gate on cfg.Archive before calling.
//
// Accounts are written in input order (caller controls ordering).
func WriteEOAs(envs *Envs, accounts []*entitygen.Account, blockNum uint64) error {
	return envs.Mdbx.Update(func(txn *mdbx.Txn) error {
		if err := envs.useFill("AccountChangeSets"); err != nil {
			return fmt.Errorf("WriteEOAs: useFill(AccountChangeSets): %w", err)
		}
		blockKey := beU64(blockNum)

		for _, acc := range accounts {
			if acc.StateAccount == nil {
				return fmt.Errorf("WriteEOAs: account %s has nil StateAccount", acc.Address.Hex())
			}

			// AccountChangeSets — DupSort: BE_u64(block) → AccountBeforeTx{addr, nil}
			// Address is the DupSort SubKey (encoded first in AccountBeforeTx.EncodeCompact).
			// Info=nil: account had no prior state (genesis creation).
			abt := iReth.AccountBeforeTx{Address: acc.Address, Info: nil}
			var abtBuf bytes.Buffer
			abt.EncodeCompact(&abtBuf)
			if err := txn.Put(envs.MdbxDBIs["AccountChangeSets"], blockKey[:], abtBuf.Bytes(), 0); err != nil {
				return fmt.Errorf("AccountChangeSets %s: %w", acc.Address.Hex(), err)
			}

			// AccountsHistory → RocksDB CF (v2 routing per
			// EitherReader::new_accounts_history). Wire format:
			// ShardedKeyAddress(addr, u64::MAX) → EncodeIntegerList([blockNum]).
			if err := envs.HistorySink().PutAccountHistory(acc.Address, blockNum); err != nil {
				return fmt.Errorf("AccountsHistory %s: %w", acc.Address.Hex(), err)
			}
		}
		return nil
	})
}
