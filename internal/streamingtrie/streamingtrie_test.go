package streamingtrie

import (
	"bytes"
	"fmt"
	"iter"
	"sort"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

// stackTrieBuilder wraps go-ethereum's trie.StackTrie so it satisfies
// the streamingtrie.HashBuilder contract. StackTrie computes a
// canonical MPT root and is the cross-client reference implementation
// (it's what reth, besu, nethermind all match — see
// client/reth/storage_root.go's "mirrors alloy_trie::root::
// storage_root_unhashed" comment).
type stackTrieBuilder struct {
	t *trie.StackTrie
}

func newStackTrieBuilder() *stackTrieBuilder {
	return &stackTrieBuilder{t: trie.NewStackTrie(nil)}
}

func (s *stackTrieBuilder) AddLeaf(keyHash common.Hash, valueRLP []byte) error {
	return s.t.Update(keyHash[:], valueRLP)
}

func (s *stackTrieBuilder) Root() (common.Hash, error) {
	return s.t.Hash(), nil
}

// stackTrieRootMaterialised computes the storage MPT root via the
// canonical path: materialise everything into a slice, keccak-sort by
// key, RLP-encode values with leading-zero strip, feed StackTrie.
// This is the "ground truth" we measure StorageRoot against.
func stackTrieRootMaterialised(pairs []kv) common.Hash {
	type hashed struct {
		keyHash common.Hash
		value   common.Hash
	}
	h := make([]hashed, 0, len(pairs))
	for _, p := range pairs {
		if p.value == (common.Hash{}) {
			continue // zero-value skip — matches StorageRoot
		}
		h = append(h, hashed{
			keyHash: crypto.Keccak256Hash(p.key[:]),
			value:   p.value,
		})
	}
	sort.Slice(h, func(i, j int) bool {
		return bytes.Compare(h[i].keyHash[:], h[j].keyHash[:]) < 0
	})

	st := trie.NewStackTrie(nil)
	for _, e := range h {
		v := e.value[:]
		for len(v) > 0 && v[0] == 0 {
			v = v[1:]
		}
		valRLP, _ := rlp.EncodeToBytes(v)
		_ = st.Update(e.keyHash[:], valRLP)
	}
	return st.Hash()
}

type kv struct{ key, value common.Hash }

// iterFromPairs builds a re-iterable iter.Seq2 from a fixture slice.
// Each call to the returned function produces the same yields — this
// is the contract templates' iters satisfy in production.
func iterFromPairs(pairs []kv) iter.Seq2[common.Hash, common.Hash] {
	return func(yield func(common.Hash, common.Hash) bool) {
		for _, p := range pairs {
			if !yield(p.key, p.value) {
				return
			}
		}
	}
}

// fixturePairs100 is a deterministic 100-slot fixture used by the
// equivalence and re-iteration tests.
func fixturePairs100() []kv {
	out := make([]kv, 100)
	for i := range out {
		// keys are NOT pre-hashed — StorageRoot hashes internally.
		out[i].key = uint64SlotKey(uint64(i) + 7) // +7 to make slots non-contiguous
		// distinct non-zero values; mix small and large so the RLP
		// leading-zero strip is exercised.
		out[i].value = common.BytesToHash([]byte{byte(i + 1), 0x00, 0x00, 0xff})
	}
	return out
}

// TestStorageRoot_EquivalenceWithStackTrie is the canonical equivalence
// guard: the new streaming helper must produce the same root as
// materialise-into-slice + StackTrie on the same inputs. This catches
// drift between the helper's drain/sort/encode path and the Yellow-
// Paper MPT specification that every per-client builder agrees on.
func TestStorageRoot_EquivalenceWithStackTrie(t *testing.T) {
	pairs := fixturePairs100()
	want := stackTrieRootMaterialised(pairs)

	got, err := StorageRoot(t.TempDir(), iterFromPairs(pairs), newStackTrieBuilder(), nil)
	if err != nil {
		t.Fatalf("StorageRoot: %v", err)
	}
	if got != want {
		t.Errorf("storage root mismatch:\n got  %s\n want %s", got.Hex(), want.Hex())
	}
}

// SortedRoot over the same pairs, pre-sorted by keccak(key), must give the same root.
func TestSortedRoot_EquivalenceWithStackTrie(t *testing.T) {
	pairs := fixturePairs100()
	sort.Slice(pairs, func(i, j int) bool {
		a, b := crypto.Keccak256Hash(pairs[i].key[:]), crypto.Keccak256Hash(pairs[j].key[:])
		return bytes.Compare(a[:], b[:]) < 0
	})
	sorted := func(yield func(keyHash, rawKey, value common.Hash) bool) {
		for _, p := range pairs {
			if !yield(crypto.Keccak256Hash(p.key[:]), p.key, p.value) {
				return
			}
		}
	}
	got, err := SortedRoot(sorted, newStackTrieBuilder(), nil)
	if err != nil {
		t.Fatalf("SortedRoot: %v", err)
	}
	if want := stackTrieRootMaterialised(pairs); got != want {
		t.Errorf("storage root mismatch:\n got  %s\n want %s", got.Hex(), want.Hex())
	}
}

// TestStorageRoot_EmptyInputs is the EmptyRootHash pin. An iter that
// yields no pairs (or only zero-valued pairs) must return the canonical
// empty-MPT root 0x56e81f17… so the storage trie root field on the
// account leaf matches what a real-Ethereum genesis-alloc would
// produce for an account with no storage.
func TestStorageRoot_EmptyInputs(t *testing.T) {
	emptyMPTRoot := common.HexToHash("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421")

	// Case A: completely empty iter.
	gotEmpty, err := StorageRoot(t.TempDir(), iterFromPairs(nil), newStackTrieBuilder(), nil)
	if err != nil {
		t.Fatalf("StorageRoot (empty): %v", err)
	}
	if gotEmpty != emptyMPTRoot {
		t.Errorf("empty iter root:\n got  %s\n want %s", gotEmpty.Hex(), emptyMPTRoot.Hex())
	}

	// Case B: nil iter (the no-storage shorthand).
	gotNil, err := StorageRoot(t.TempDir(), nil, newStackTrieBuilder(), nil)
	if err != nil {
		t.Fatalf("StorageRoot (nil): %v", err)
	}
	if gotNil != emptyMPTRoot {
		t.Errorf("nil iter root:\n got  %s\n want %s", gotNil.Hex(), emptyMPTRoot.Hex())
	}
}

// TestStorageRoot_ZeroValueSkipped pins the zero-value-skip rule. A
// fixture with one explicit (key, 0) pair must produce the same root
// as a fixture with no pairs at all. Drift here desynchronises us from
// alloy-genesis's filter(!value.is_zero()) and would diverge from
// every other client's storage-trie computation.
func TestStorageRoot_ZeroValueSkipped(t *testing.T) {
	pairs := []kv{
		{key: uint64SlotKey(1), value: common.Hash{}},      // zero → must skip
		{key: uint64SlotKey(2), value: common.HexToHash("0x1")}, // non-zero — counted
	}

	got, err := StorageRoot(t.TempDir(), iterFromPairs(pairs), newStackTrieBuilder(), nil)
	if err != nil {
		t.Fatalf("StorageRoot: %v", err)
	}
	want := stackTrieRootMaterialised(pairs) // helper also skips zeros
	if got != want {
		t.Errorf("zero-skipped root mismatch:\n got  %s\n want %s", got.Hex(), want.Hex())
	}

	// Cross-check: a fixture with ONLY the zero pair must equal the
	// empty-storage root.
	zeroOnly := []kv{{key: uint64SlotKey(1), value: common.Hash{}}}
	gotZeroOnly, err := StorageRoot(t.TempDir(), iterFromPairs(zeroOnly), newStackTrieBuilder(), nil)
	if err != nil {
		t.Fatalf("StorageRoot (zero-only): %v", err)
	}
	emptyRoot := common.HexToHash("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421")
	if gotZeroOnly != emptyRoot {
		t.Errorf("zero-only iter:\n got  %s\n want %s (empty MPT)", gotZeroOnly.Hex(), emptyRoot.Hex())
	}
}

// TestStorageRoot_ReIterationDeterminism pins the
// "same iter source, same root across calls" contract. A pure-function
// iter.Seq2 (which templates produce) can be passed to StorageRoot
// multiple times; each invocation must return the same root. This
// guards against an accidental stateful closure inside the helper
// (e.g. lazy-init that mutates state on first call).
func TestStorageRoot_ReIterationDeterminism(t *testing.T) {
	pairs := fixturePairs100()
	src := iterFromPairs(pairs)

	first, err := StorageRoot(t.TempDir(), src, newStackTrieBuilder(), nil)
	if err != nil {
		t.Fatalf("StorageRoot (first): %v", err)
	}
	second, err := StorageRoot(t.TempDir(), src, newStackTrieBuilder(), nil)
	if err != nil {
		t.Fatalf("StorageRoot (second): %v", err)
	}
	if first != second {
		t.Errorf("non-deterministic: first=%s second=%s", first.Hex(), second.Hex())
	}
}

// TestDrainIterateRoot_EquivalenceWithStorageRoot: split-form must hash
// to the same root as the one-shot helper.
func TestDrainIterateRoot_EquivalenceWithStorageRoot(t *testing.T) {
	pairs := fixturePairs100()
	want, err := StorageRoot(t.TempDir(), iterFromPairs(pairs), newStackTrieBuilder(), nil)
	if err != nil {
		t.Fatalf("StorageRoot (oracle): %v", err)
	}

	d, err := Drain(t.TempDir(), iterFromPairs(pairs))
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	defer d.Close()
	got, err := d.IterateRoot(newStackTrieBuilder(), nil)
	if err != nil {
		t.Fatalf("IterateRoot: %v", err)
	}
	if got != want {
		t.Errorf("split-form root differs from one-shot: got %s, want %s", got.Hex(), want.Hex())
	}
}

// TestStorageRoot_SinkReceivesAllPairs verifies the Sink contract: it
// is called exactly once per non-zero (keyHash, rawKey, value) triple,
// in keccak-sorted-by-keyHash order. Pairs the Sink with an
// accumulator and asserts the captured triples match what
// stackTrieRootMaterialised would have processed.
func TestStorageRoot_SinkReceivesAllPairs(t *testing.T) {
	pairs := fixturePairs100()

	type seen struct {
		keyHash, rawKey, value common.Hash
	}
	var captured []seen
	var prevKH common.Hash
	first := true

	sink := func(keyHash, rawKey, value common.Hash) error {
		if !first && bytes.Compare(prevKH[:], keyHash[:]) >= 0 {
			return fmt.Errorf("sink received unsorted keyHash: prev=%s now=%s",
				prevKH.Hex(), keyHash.Hex())
		}
		captured = append(captured, seen{keyHash, rawKey, value})
		prevKH = keyHash
		first = false
		return nil
	}

	if _, err := StorageRoot(t.TempDir(), iterFromPairs(pairs), newStackTrieBuilder(), sink); err != nil {
		t.Fatalf("StorageRoot: %v", err)
	}

	if len(captured) != len(pairs) {
		t.Errorf("sink called %d times, want %d", len(captured), len(pairs))
	}

	// Build the same expected set from the fixture and verify rawKey/value
	// mapping (Sink received the same rawKey we Put in).
	expected := make(map[common.Hash]common.Hash, len(pairs))
	for _, p := range pairs {
		expected[crypto.Keccak256Hash(p.key[:])] = p.value
	}
	for _, c := range captured {
		want, ok := expected[c.keyHash]
		if !ok {
			t.Errorf("sink received unexpected keyHash %s", c.keyHash.Hex())
			continue
		}
		if c.value != want {
			t.Errorf("sink rawKey %s value mismatch: got %s, want %s",
				c.rawKey.Hex(), c.value.Hex(), want.Hex())
		}
	}
}
