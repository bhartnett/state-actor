package reth

import (
	"bytes"
	"errors"
	"math/rand"
	"sort"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/trie"
)

// emptyTrieRootHash is the canonical Merkle-Patricia trie root of the empty trie:
// keccak256(rlp([])) = 0x56e81f17...
var emptyTrieRootHash = common.HexToHash("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421")

func TestHashBuilderEmpty(t *testing.T) {
	emit := func(path StoredNibbles, node BranchNodeCompact) error { return nil }
	hb := NewHashBuilder(emit)
	root := hb.Root()
	if root != emptyTrieRootHash {
		t.Errorf("empty trie root = %s, want %s", root.Hex(), emptyTrieRootHash.Hex())
	}
	// Sanity vs. go-ethereum reference:
	if root != trie.NewStackTrie(nil).Hash() {
		t.Errorf("empty trie root != trie.StackTrie's empty root")
	}
}

func TestHashBuilderSingleLeaf(t *testing.T) {
	emit := func(path StoredNibbles, node BranchNodeCompact) error {
		t.Errorf("unexpected emission for single leaf: path=%+v node=%+v", path, node)
		return nil
	}
	hb := NewHashBuilder(emit)
	key := bytes.Repeat([]byte{0xa0}, 32)
	value := bytes.Repeat([]byte{0x42}, 32)
	// AddLeaf takes nibble-unpacked keys (64 nibbles for 32-byte key)
	nibbles := bytesToNibbles(key)
	if err := hb.AddLeaf(nibbles, value); err != nil {
		t.Fatalf("AddLeaf: %v", err)
	}
	got := hb.Root()
	st := trie.NewStackTrie(nil)
	st.Update(key, value)
	want := st.Hash()
	if got != want {
		t.Errorf("single-leaf root: got=%s want=%s", got.Hex(), want.Hex())
	}
}

func TestHashBuilderTwoLeaves_Shared(t *testing.T) {
	hb := NewHashBuilder(func(StoredNibbles, BranchNodeCompact) error { return nil })
	k1 := bytes.Repeat([]byte{0xa0}, 32)
	k2 := bytes.Repeat([]byte{0xa0}, 32)
	k2[31] = 0xa1
	if err := hb.AddLeaf(bytesToNibbles(k1), []byte{0x01}); err != nil {
		t.Fatal(err)
	}
	if err := hb.AddLeaf(bytesToNibbles(k2), []byte{0x02}); err != nil {
		t.Fatal(err)
	}
	got := hb.Root()
	st := trie.NewStackTrie(nil)
	st.Update(k1, []byte{0x01})
	st.Update(k2, []byte{0x02})
	want := st.Hash()
	if got != want {
		t.Errorf("shared-prefix root: got=%s want=%s", got.Hex(), want.Hex())
	}
}

func TestHashBuilderTwoLeaves_Diverged(t *testing.T) {
	hb := NewHashBuilder(func(StoredNibbles, BranchNodeCompact) error { return nil })
	k1 := make([]byte, 32)
	k1[0] = 0x10
	k2 := make([]byte, 32)
	k2[0] = 0x20
	if err := hb.AddLeaf(bytesToNibbles(k1), []byte{0x01}); err != nil {
		t.Fatal(err)
	}
	if err := hb.AddLeaf(bytesToNibbles(k2), []byte{0x02}); err != nil {
		t.Fatal(err)
	}
	got := hb.Root()
	st := trie.NewStackTrie(nil)
	st.Update(k1, []byte{0x01})
	st.Update(k2, []byte{0x02})
	want := st.Hash()
	if got != want {
		t.Errorf("diverged root: got=%s want=%s", got.Hex(), want.Hex())
	}
}

func TestHashBuilderThreeTopDiverged(t *testing.T) {
	hb := NewHashBuilder(func(StoredNibbles, BranchNodeCompact) error { return nil })
	mk := func(b byte) []byte { k := make([]byte, 32); k[0] = b; return k }
	keys := [][]byte{mk(0x10), mk(0x20), mk(0x30)}
	values := [][]byte{{0x01}, {0x02}, {0x03}}
	for i := range keys {
		if err := hb.AddLeaf(bytesToNibbles(keys[i]), values[i]); err != nil {
			t.Fatal(err)
		}
	}
	got := hb.Root()
	st := trie.NewStackTrie(nil)
	for i := range keys {
		st.Update(keys[i], values[i])
	}
	want := st.Hash()
	if got != want {
		t.Errorf("three-top-diverged root: got=%s want=%s", got.Hex(), want.Hex())
	}
}

func TestHashBuilderFullBranch16(t *testing.T) {
	hb := NewHashBuilder(func(StoredNibbles, BranchNodeCompact) error { return nil })
	mk := func(b byte) []byte { k := make([]byte, 32); k[0] = b; return k }
	st := trie.NewStackTrie(nil)
	for i := byte(0); i < 16; i++ {
		k := mk(i << 4)
		v := []byte{i + 1}
		if err := hb.AddLeaf(bytesToNibbles(k), v); err != nil {
			t.Fatal(err)
		}
		st.Update(k, v)
	}
	got := hb.Root()
	want := st.Hash()
	if got != want {
		t.Errorf("full-branch-16 root: got=%s want=%s", got.Hex(), want.Hex())
	}
}

// TestHashBuilderInlinedChild tests that small leaves (< 32-byte RLP) get
// inlined into the parent branch's RLP rather than hashed and stored separately.
// Asserts root correctness via cross-check with go-ethereum's StackTrie.
func TestHashBuilderInlinedChild(t *testing.T) {
	hb := NewHashBuilder(func(StoredNibbles, BranchNodeCompact) error { return nil })
	// Two leaves with TINY values; their leaf RLP is < 32 bytes → leaves get
	// inlined into the parent branch.
	k1 := []byte{0x10, 0x20}
	k2 := []byte{0x20, 0x30}
	if err := hb.AddLeaf(bytesToNibbles(k1), []byte{0xab}); err != nil {
		t.Fatal(err)
	}
	if err := hb.AddLeaf(bytesToNibbles(k2), []byte{0xcd}); err != nil {
		t.Fatal(err)
	}
	got := hb.Root()

	st := trie.NewStackTrie(nil)
	st.Update(k1, []byte{0xab})
	st.Update(k2, []byte{0xcd})
	if got != st.Hash() {
		t.Errorf("inlined-children root: got=%s want=%s", got.Hex(), st.Hash().Hex())
	}
}

// TestHashBuilderHashedChild stresses the >= 32-byte boundary: large values
// produce leaf RLPs that exceed the inlining threshold so children become
// 32-byte hashes referenced from the parent branch.
func TestHashBuilderHashedChild(t *testing.T) {
	hb := NewHashBuilder(func(StoredNibbles, BranchNodeCompact) error { return nil })
	k1 := []byte{0x10, 0x20}
	k2 := []byte{0x20, 0x30}
	v := bytes.Repeat([]byte{0xff}, 64) // 64-byte value → leaf RLP > 32 bytes
	if err := hb.AddLeaf(bytesToNibbles(k1), v); err != nil {
		t.Fatal(err)
	}
	if err := hb.AddLeaf(bytesToNibbles(k2), v); err != nil {
		t.Fatal(err)
	}
	got := hb.Root()

	st := trie.NewStackTrie(nil)
	st.Update(k1, v)
	st.Update(k2, v)
	if got != st.Hash() {
		t.Errorf("hashed-children root: got=%s want=%s", got.Hex(), st.Hash().Hex())
	}
}

// TestHashBuilderRootMatchesStackTrieProperty fuzz-tests root correctness
// over many random sorted-leaf inputs. Fixed seed so failures reproduce.
func TestHashBuilderRootMatchesStackTrieProperty(t *testing.T) {
	const seed = 0xc0ffee
	rng := rand.New(rand.NewSource(seed))

	for trial := 0; trial < 50; trial++ {
		n := 1 + rng.Intn(200)
		keys := make([][]byte, n)
		for i := range keys {
			k := make([]byte, 32)
			rng.Read(k)
			keys[i] = k
		}
		sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i], keys[j]) < 0 })
		// Deduplicate (sorted, so adjacent duplicates).
		dedup := keys[:0]
		for i, k := range keys {
			if i == 0 || !bytes.Equal(k, keys[i-1]) {
				dedup = append(dedup, k)
			}
		}
		keys = dedup

		values := make([][]byte, len(keys))
		for i := range values {
			vlen := 1 + rng.Intn(40) // 1..40 bytes — straddles the 32-byte boundary
			values[i] = make([]byte, vlen)
			rng.Read(values[i])
		}

		// Every emitted BranchNodeCompact must satisfy `tree_mask ⊆
		// state_mask` + `hash_mask ⊆ state_mask`. This is the broad-coverage
		// fuzz that complements the deterministic fixtures in
		// hash_builder_emissions_test.go — random keys + value-length
		// straddling 32 bytes exercise the same extension-wrapping branches
		// that triggered the 215K-entity bloatnet panic at scale.
		hb := NewHashBuilder(func(_ StoredNibbles, n BranchNodeCompact) error {
			if leftover := n.TreeMask & ^n.StateMask; leftover != 0 {
				t.Errorf("trial %d (n=%d, seed=%#x): tree_mask has bits not in state_mask: state=0x%04x tree=0x%04x leftover=0x%04x",
					trial, len(keys), seed, n.StateMask, n.TreeMask, leftover)
			}
			if leftover := n.HashMask & ^n.StateMask; leftover != 0 {
				t.Errorf("trial %d (n=%d, seed=%#x): hash_mask has bits not in state_mask: state=0x%04x hash=0x%04x leftover=0x%04x",
					trial, len(keys), seed, n.StateMask, n.HashMask, leftover)
			}
			return nil
		})
		for i := range keys {
			if err := hb.AddLeaf(bytesToNibbles(keys[i]), values[i]); err != nil {
				t.Fatalf("trial %d AddLeaf: %v", trial, err)
			}
		}
		got := hb.Root()

		st := trie.NewStackTrie(nil)
		for i := range keys {
			st.Update(keys[i], values[i])
		}
		if got != st.Hash() {
			t.Errorf("trial %d (n=%d, seed=%#x):\n  got=%s\n  want=%s",
				trial, len(keys), seed, got.Hex(), st.Hash().Hex())
		}
	}
}

// TestHashBuilderStorageTriePattern demonstrates the per-contract usage:
// build a fresh HashBuilder over the contract's sorted (slot_hash, value_rlp)
// pairs, get the storage root, and feed the storage root into the parent
// account's leaf RLP. This is exactly the pattern Slice E's trie_writer_cgo.go
// will use.
func TestHashBuilderStorageTriePattern(t *testing.T) {
	type slot struct{ key, value []byte }
	storageSlots := []slot{
		{key: bytes.Repeat([]byte{0x01}, 32), value: []byte{0xa}},
		{key: bytes.Repeat([]byte{0x02}, 32), value: []byte{0xb}},
		{key: bytes.Repeat([]byte{0x03}, 32), value: []byte{0xc}},
	}
	sort.Slice(storageSlots, func(i, j int) bool {
		return bytes.Compare(storageSlots[i].key, storageSlots[j].key) < 0
	})

	storageHB := NewHashBuilder(func(StoredNibbles, BranchNodeCompact) error { return nil })
	for _, s := range storageSlots {
		if err := storageHB.AddLeaf(bytesToNibbles(s.key), s.value); err != nil {
			t.Fatal(err)
		}
	}
	storageRoot := storageHB.Root()

	st := trie.NewStackTrie(nil)
	for _, s := range storageSlots {
		st.Update(s.key, s.value)
	}
	if storageRoot != st.Hash() {
		t.Errorf("storage root: got=%s want=%s", storageRoot.Hex(), st.Hash().Hex())
	}
}

// TestHashBuilderErrKeysOutOfOrder verifies the monotonicity guard.
func TestHashBuilderErrKeysOutOfOrder(t *testing.T) {
	cases := []struct {
		name              string
		keys              [][]byte
		wantErr           bool
		wantSentinelOnIdx int // which AddLeaf call should fail (-1 = none)
	}{
		{
			name:              "ascending OK",
			keys:              [][]byte{{0x01}, {0x02}, {0x03}},
			wantErr:           false,
			wantSentinelOnIdx: -1,
		},
		{
			name:              "equal keys fail",
			keys:              [][]byte{{0x01}, {0x01}},
			wantErr:           true,
			wantSentinelOnIdx: 1,
		},
		{
			name:              "reversed fails",
			keys:              [][]byte{{0x02}, {0x01}},
			wantErr:           true,
			wantSentinelOnIdx: 1,
		},
		{
			name:              "empty after non-empty fails",
			keys:              [][]byte{{0x01}, {}},
			wantErr:           true,
			wantSentinelOnIdx: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hb := NewHashBuilder(func(StoredNibbles, BranchNodeCompact) error { return nil })
			var lastErr error
			var lastErrIdx = -1
			for i, k := range tc.keys {
				err := hb.AddLeaf(k, []byte{0x42})
				if err != nil {
					lastErr = err
					lastErrIdx = i
					break
				}
			}
			if tc.wantErr {
				if lastErr == nil {
					t.Errorf("expected error, got none")
				} else if !errors.Is(lastErr, ErrKeysOutOfOrder) {
					t.Errorf("expected ErrKeysOutOfOrder, got %v", lastErr)
				}
				if lastErrIdx != tc.wantSentinelOnIdx {
					t.Errorf("expected error on AddLeaf #%d, got #%d", tc.wantSentinelOnIdx, lastErrIdx)
				}
			} else {
				if lastErr != nil {
					t.Errorf("expected no error, got %v at #%d", lastErr, lastErrIdx)
				}
			}
		})
	}
}
