//go:build cgo_reth

package reth

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/erigontech/mdbx-go/mdbx"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/internal/entitygen"
	iReth "github.com/ethereum/state-actor/internal/reth"
)

// WriteContractStorage writes per-slot rows for a single contract across
// the v2 storage tables. accounts must be a contract account (StateAccount
// + Storage slots populated). blockNum is the block at which storage was
// newly written (0 for genesis). All slots are written within ONE MDBX
// transaction (the caller's).
//
// Per-slot writes:
//   - HashedStorages (DupSort): keccak(Address) → StorageEntry{keccak(slot_key), slot_value} — canonical v2 state, always.
//   - StorageChangeSets (DupSort): BlockNumberAddress → StorageEntry{slot_key, prev=0} — archive only.
//   - StoragesHistory: StorageShardedKey{addr, slot_key, u64::MAX} → IntegerList([block]) — archive only; routed to the RocksDB CF via envs.HistorySink().
//
// For genesis (blockNum=0) the "before" value in StorageChangeSets is 0 —
// the slot was newly set.
//
// Returns the sum of HashedStorages entry sizes (compact encoding) across
// all slots, for the caller's stats accumulator. Auxiliary tables and
// their keys are not counted. Caller transfers the count to
// *generator.Stats only after the enclosing MDBX txn commits.
func WriteContractStorage(
	envs *Envs,
	txn *mdbx.Txn,
	contract *entitygen.Account,
	blockNum uint64,
	archive bool,
) (uint64, error) {
	blockKey := iReth.BlockNumberAddress{BlockNumber: blockNum, Address: contract.Address}
	var blockKeyBuf bytes.Buffer
	blockKey.EncodeKey(&blockKeyBuf)
	blockKeyBytes := blockKeyBuf.Bytes()

	// Hash every slot key up front and process in ascending hashed-key
	// order, so HashedStorages (DupSort, sub-sorted by this same key)
	// fills pages densely instead of the RNG order contract.Storage
	// arrives in. Archive tables are keyed by the raw slot key, not the
	// hash, so this only reorders their writes — content is unchanged.
	type hashedSlot struct {
		slot    entitygen.StorageSlot
		keyHash common.Hash
	}
	hashed := make([]hashedSlot, len(contract.Storage))
	for i, slot := range contract.Storage {
		hashed[i] = hashedSlot{slot: slot, keyHash: crypto.Keccak256Hash(slot.Key[:])}
	}
	sort.Slice(hashed, func(i, j int) bool {
		return bytes.Compare(hashed[i].keyHash[:], hashed[j].keyHash[:]) < 0
	})

	var storageBytes uint64
	for _, hs := range hashed {
		slot := hs.slot
		slotValueU256 := uint256.NewInt(0).SetBytes(slot.Value[:])

		// HashedStorages: keccak(Address) → StorageEntry{keccak(slot_key), slot_value}.
		// Canonical v2 storage table.
		hashedEntry := iReth.StorageEntry{Key: hs.keyHash, Value: slotValueU256}
		var hashedBuf bytes.Buffer
		hashedEntry.EncodeCompact(&hashedBuf)
		hashedEntryBytes := hashedBuf.Bytes()
		if err := txn.Put(envs.MdbxDBIs["HashedStorages"], contract.AddrHash[:], hashedEntryBytes, 0); err != nil {
			return 0, fmt.Errorf("HashedStorages %s slot %s: %w",
				contract.AddrHash.Hex(), slot.Key.Hex(), err)
		}

		// Stats: bank HashedStorages compact-encoded entry bytes.
		storageBytes += uint64(len(hashedEntryBytes))
	}

	// Archive tables are keyed by the raw slot key; contract.Storage arrives pre-sorted by it.
	if archive {
		if err := envs.useFill("StorageChangeSets"); err != nil {
			return 0, fmt.Errorf("WriteContractStorage: useFill(StorageChangeSets): %w", err)
		}
		for _, slot := range contract.Storage {
			// StorageChangeSets: BlockNumberAddress → StorageEntry{slot_key, prev_value=0}
			// For genesis (block 0), the "before" value is 0 — slot was newly set.
			changeEntry := iReth.StorageEntry{Key: slot.Key, Value: uint256.NewInt(0)}
			var changeBuf bytes.Buffer
			changeEntry.EncodeCompact(&changeBuf)
			if err := txn.Put(envs.MdbxDBIs["StorageChangeSets"], blockKeyBytes, changeBuf.Bytes(), 0); err != nil {
				return 0, fmt.Errorf("StorageChangeSets %s slot %s: %w",
					contract.Address.Hex(), slot.Key.Hex(), err)
			}

			// StoragesHistory → RocksDB CF (v2 routing per
			// EitherReader::new_storages_history). Wire format:
			// StorageShardedKey(addr, slot_key, u64::MAX) → EncodeIntegerList([blockNum]).
			if err := envs.HistorySink().PutStorageHistory(contract.Address, slot.Key, blockNum); err != nil {
				return 0, fmt.Errorf("StoragesHistory %s slot %s: %w",
					contract.Address.Hex(), slot.Key.Hex(), err)
			}
		}
	}
	return storageBytes, nil
}
