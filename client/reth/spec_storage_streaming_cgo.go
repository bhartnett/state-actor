//go:build cgo_reth

package reth

import (
	"bytes"
	"context"
	"fmt"
	"runtime"
	"sync"

	"github.com/erigontech/mdbx-go/mdbx"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/generator"
	"github.com/ethereum/state-actor/internal/autofill"
	"github.com/ethereum/state-actor/internal/progress"
	iReth "github.com/ethereum/state-actor/internal/reth"
	"github.com/ethereum/state-actor/internal/streamingtrie"
	"github.com/ethereum/state-actor/internal/streamsort"
)

// maxStreamSpecStorageWorkers caps drain-and-compute parallelism; each worker
// owns a streamsort.Store so peak RAM scales linearly.
const maxStreamSpecStorageWorkers = 8

// chunkSlots is the slot-batch size workers emit to the consumer; bounds
// in-flight memory per entity.
const chunkSlots = 1024

// chunkChanCap = 4 lets a worker run ~4 chunks ahead of the consumer to hide
// per-txn commit latency without unbounded RAM growth.
const chunkChanCap = 4

// slotPrepared holds the per-slot byte buffers + raw key a single slot
// contributes. Produced on the worker goroutine, consumed on the main
// goroutine inside Mdbx.Update — no encoding work in the hot write path.
// rawKey lets the consumer route an archive-mode StoragesHistory put
// into the RocksDB CF (envs.HistorySink builds the StorageShardedKey
// internally).
type slotPrepared struct {
	rawKey      common.Hash // slot key (untouched); StoragesHistory sink arg
	hashedEntry []byte      // HashedStorages value (encoded keyHash || value)
	changeEntry []byte      // StorageChangeSets value (encoded rawKey || 0); nil in full mode
}

// preparedEntity carries everything the consumer needs to commit one
// entity's storage. The worker fills chunkCh in keccak-ascending order,
// then sends the computed root via rootCh and closes chunkCh. errCh
// carries any IterateRoot failure surfacing on the worker side.
// StoragesTrie rows are spilled straight into the shared trieRows sorter
// (see storagesTrieEmitter) as the worker's HashBuilder emits them, not
// buffered here; drainStoragesTrie writes them after RunCgo's last writer.
type preparedEntity struct {
	addr     common.Address
	addrHash common.Hash
	blockKey []byte // BlockNumberAddress-encoded
	chunkCh  chan []slotPrepared
	rootCh   chan common.Hash
	errCh    chan error
}

// streamedEntity is one Phase-0 account: cfg.PreAlloc[idx], or auto-fill giant g when non-nil.
type streamedEntity struct {
	idx int
	g   *autofill.Giant
}

// streamSpecStorage writes each PreAlloc entity's Storage and each auto-fill
// giant's slots into reth's four storage tables, computes the storage MPT
// root, and splices it into cfg.GenesisAccounts[addr].Root for the alloc
// handler; giants' roots are returned keyed by addrHash instead. StoragesTrie
// rows are spilled into trieRows (shared with WriteContracts); the caller
// drains it after every writer, including this one, has finished.
//
// Architecture: N parallel workers each (a) drain their spec entity's iter
// into a per-call streamsort.Store (giants are already keccak-sorted, no
// drain), (b) walk the sorted slots driving
// the HashBuilder and pre-encoding all 4 per-slot byte buffers into
// chunks. The single consumer goroutine opens one Mdbx.Update per
// entity and does just the 4 txn.Put calls per slot — the encoding,
// keccak, and HashBuilder work happens on the workers.
//
// Per-entity Mdbx.Update — not one global txn — because a single
// global txn wrapping ~1.5B Puts degrades MDBX's dirty-page tree non-
// linearly. MDBX enforces single-writer-per-env so the commits run
// serially anyway; the win is offloading every cycle of CPU work to
// the workers so the consumer is pure I/O.
func streamSpecStorage(ctx context.Context, envs *Envs, trieRows *streamsort.Store, cfg *generator.Config, stats *generator.Stats, giants []*autofill.Giant) (map[common.Hash]common.Hash, error) {
	items := make([]streamedEntity, 0, len(cfg.PreAlloc)+len(giants))
	for i := range cfg.PreAlloc {
		if cfg.PreAlloc[i].Storage != nil {
			items = append(items, streamedEntity{idx: i})
		}
	}
	for _, g := range giants {
		items = append(items, streamedEntity{g: g})
	}
	if len(items) == 0 {
		return nil, nil
	}

	// Phase 0 streams each spec entity's storage slots and is otherwise silent
	// for hours on bloat specs. The count-only slot heartbeat funnels every
	// worker's per-slot count through one SlotMeter; each worker holds its own
	// SlotWorker so the hot per-slot path stays cheap (one non-atomic add + mask).
	cfg.Progress.Stage("reth: phase 0 — streamed storage")
	slotMeter := cfg.Progress.SlotMeter()

	workers := runtime.NumCPU()
	if workers < 1 {
		workers = 1
	}
	if workers > maxStreamSpecStorageWorkers {
		workers = maxStreamSpecStorageWorkers
	}
	if workers > len(items) {
		workers = len(items)
	}

	drainCtx, cancelDrain := context.WithCancelCause(ctx)
	defer cancelDrain(nil)

	entityCh := make(chan streamedEntity, workers*2)
	// preparedCh buffer of workers*2 absorbs small latency spikes; the
	// consumer applies back-pressure via the per-entity chunkCh inside
	// each preparedEntity.
	preparedCh := make(chan *preparedEntity, workers*2)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slotW := slotMeter.Worker()
			for it := range entityCh {
				if drainCtx.Err() != nil {
					return
				}
				if err := drainAndEncodeEntity(drainCtx, cfg, trieRows, it, preparedCh, slotW); err != nil {
					cancelDrain(err)
					return
				}
			}
		}()
	}

	go func() {
		defer close(entityCh)
		for _, it := range items {
			select {
			case entityCh <- it:
			case <-drainCtx.Done():
				return
			}
		}
	}()

	go func() {
		wg.Wait()
		close(preparedCh)
	}()

	giantRoots := make(map[common.Hash]common.Hash, len(giants))
	var totalStorageBytes uint64
	var writeErr error
	for ent := range preparedCh {
		if writeErr != nil {
			// Drain remaining chunks so the worker doesn't block on send.
			for range ent.chunkCh {
			}
			continue
		}
		bytesWritten, err := consumeEntity(ctx, envs, cfg, ent)
		if err != nil {
			writeErr = err
			cancelDrain(err)
			continue
		}
		// Wait for the worker's computed root. By the time chunkCh
		// closed (consumeEntity returns), exactly one of {rootCh,
		// errCh} has a value.
		select {
		case root := <-ent.rootCh:
			if acc, ok := cfg.GenesisAccounts[ent.addr]; ok && acc != nil {
				acc.Root = root
			} else {
				giantRoots[ent.addrHash] = root
			}
		case err := <-ent.errCh:
			writeErr = err
			cancelDrain(err)
			continue
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		totalStorageBytes += bytesWritten
	}

	if writeErr != nil {
		return nil, writeErr
	}
	if cause := context.Cause(drainCtx); cause != nil {
		return nil, cause
	}

	if stats != nil {
		stats.StorageBytes += totalStorageBytes
	}
	return giantRoots, nil
}

// drainAndEncodeEntity runs on a worker goroutine. It drains a spec
// entity's storage iter into a streamsort (giants stream pre-sorted), then
// walks the sorted slots driving the HashBuilder AND pre-encoding the
// per-slot byte buffers into chunks sent to the consumer via ent.chunkCh.
// HashBuilder's StoragesTrie emissions go straight into trieRows (shared
// across workers, serialized under its own putMu — see storagesTrieEmitter).
func drainAndEncodeEntity(
	drainCtx context.Context,
	cfg *generator.Config,
	trieRows *streamsort.Store,
	it streamedEntity,
	preparedCh chan<- *preparedEntity,
	slotW *progress.SlotWorker,
) error {
	var d *streamingtrie.Drained // nil for giants; Close is nil-safe
	var addr common.Address
	if it.g != nil {
		addr = it.g.Address
	} else {
		pe := &cfg.PreAlloc[it.idx]
		addr = pe.Address
		var err error
		if d, err = streamingtrie.Drain(cfg.DBPath, pe.Storage); err != nil {
			return fmt.Errorf("reth: drain spec storage[%d] %s: %w", it.idx, addr.Hex(), err)
		}
	}
	addrHash := crypto.Keccak256Hash(addr[:])

	blockKey := iReth.BlockNumberAddress{BlockNumber: 0, Address: addr}
	var blockKeyBuf bytes.Buffer
	blockKey.EncodeKey(&blockKeyBuf)

	ent := &preparedEntity{
		addr:     addr,
		addrHash: addrHash,
		blockKey: blockKeyBuf.Bytes(),
		chunkCh:  make(chan []slotPrepared, chunkChanCap),
		rootCh:   make(chan common.Hash, 1),
		errCh:    make(chan error, 1),
	}

	// Hand the preparedEntity to the consumer BEFORE starting iteration
	// so the consumer can begin draining chunks as soon as the first one
	// is ready. The consumer is responsible for closing the Drained
	// indirectly by waiting for chunkCh to close.
	select {
	case preparedCh <- ent:
	case <-drainCtx.Done():
		d.Close()
		return drainCtx.Err()
	}

	archive := cfg.Archive

	hb := newRethStorageHashBuilder(storagesTrieEmitter(trieRows, addrHash))
	pending := make([]slotPrepared, 0, chunkSlots)

	flushPending := func() error {
		if len(pending) == 0 {
			return nil
		}
		select {
		case ent.chunkCh <- pending:
			pending = make([]slotPrepared, 0, chunkSlots)
			return nil
		case <-drainCtx.Done():
			return drainCtx.Err()
		}
	}

	sink := func(keyHash, rawKey, value common.Hash) error {
		slotW.Slot()
		slotValueU256 := uint256.NewInt(0).SetBytes(value[:])

		prep := slotPrepared{rawKey: rawKey}

		hashedEntry := iReth.StorageEntry{Key: keyHash, Value: slotValueU256}
		var hashedBuf bytes.Buffer
		hashedEntry.EncodeCompact(&hashedBuf)
		prep.hashedEntry = hashedBuf.Bytes()

		if archive {
			changeEntry := iReth.StorageEntry{Key: rawKey, Value: uint256.NewInt(0)}
			var changeBuf bytes.Buffer
			changeEntry.EncodeCompact(&changeBuf)
			prep.changeEntry = changeBuf.Bytes()
		}

		pending = append(pending, prep)
		if len(pending) >= chunkSlots {
			return flushPending()
		}
		return nil
	}

	var root common.Hash
	var err error
	if it.g != nil {
		root, err = streamingtrie.SortedRoot(it.g.Sorted, hb, sink)
	} else {
		root, err = d.IterateRoot(hb, sink)
	}
	d.Close()
	if err != nil {
		ent.errCh <- fmt.Errorf("reth: iterate storage %s: %w", addr.Hex(), err)
		close(ent.chunkCh)
		return nil
	}

	// Flush trailing chunk (if any) before closing the channel.
	if err := flushPending(); err != nil {
		ent.errCh <- err
		close(ent.chunkCh)
		return nil
	}
	close(ent.chunkCh)
	ent.rootCh <- root
	return nil
}

// consumeEntity runs on the main goroutine. Opens one Mdbx.Update per
// entity and drains the worker's chunks into 4 txn.Put calls per slot.
// All bytes are pre-encoded by the worker; this function does pure I/O.
func consumeEntity(
	ctx context.Context,
	envs *Envs,
	cfg *generator.Config,
	ent *preparedEntity,
) (uint64, error) {
	var localBytes uint64
	err := envs.Mdbx.Update(func(txn *mdbx.Txn) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for chunk := range ent.chunkCh {
			if err := envs.useFill("HashedStorages"); err != nil {
				return fmt.Errorf("useFill(HashedStorages): %w", err)
			}
			for _, prep := range chunk {
				if err := txn.Put(envs.MdbxDBIs["HashedStorages"], ent.addrHash[:], prep.hashedEntry, 0); err != nil {
					return fmt.Errorf("HashedStorages %s: %w", ent.addrHash.Hex(), err)
				}
				// Stats: bank HashedStorages entry bytes.
				localBytes += uint64(len(prep.hashedEntry))
			}

			if cfg.Archive {
				// Archive mode: write StorageChangeSets (MDBX) + route
				// StoragesHistory to RocksDB CF (v2).
				if err := envs.useFill("StorageChangeSets"); err != nil {
					return fmt.Errorf("useFill(StorageChangeSets): %w", err)
				}
				for _, prep := range chunk {
					if err := txn.Put(envs.MdbxDBIs["StorageChangeSets"], ent.blockKey, prep.changeEntry, 0); err != nil {
						return fmt.Errorf("StorageChangeSets %s: %w", ent.addr.Hex(), err)
					}
					if err := envs.HistorySink().PutStorageHistory(ent.addr, prep.rawKey, 0); err != nil {
						return fmt.Errorf("StoragesHistory %s: %w", ent.addr.Hex(), err)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return localBytes, nil
}

// rethStorageHashBuilder adapts iReth.HashBuilder to streamingtrie.HashBuilder.
type rethStorageHashBuilder struct {
	hb *iReth.HashBuilder
}

// newRethStorageHashBuilder returns a HashBuilder configured to persist
// storage-trie rows: every branch with tree_mask | hash_mask != 0
// (alloy_trie's store_branch_node rule) triggers emit. state-actor's
// spec-storage workers use this so each per-entity storage trie populates
// StoragesTrie alongside the per-slot data rows.
func newRethStorageHashBuilder(emit iReth.NodeEmitter) *rethStorageHashBuilder {
	return &rethStorageHashBuilder{
		hb: iReth.NewHashBuilder(emit),
	}
}

func (r *rethStorageHashBuilder) AddLeaf(keyHash common.Hash, valueRLP []byte) error {
	return r.hb.AddLeaf(addrHashToNibbles(keyHash[:]), valueRLP)
}

func (r *rethStorageHashBuilder) Root() (common.Hash, error) {
	return r.hb.Root(), nil
}
