//go:build cgo_erigon_commitment

package erigonfiles

import (
	"bytes"
	"context"
	"encoding/binary"
	"math/rand"
	"os"
	"testing"

	"github.com/erigontech/erigon/db/datastruct/btindex"
	"github.com/erigontech/erigon/db/recsplit"
	"github.com/erigontech/erigon/db/seg"
	"github.com/erigontech/erigon/db/state/statecfg"

	"github.com/ethereum/state-actor/internal/erigon/snap"
)

// fixtureCode mirrors the benchmark's unique_jumpdest contracts: 24,576 bytes of JUMPDEST around
// the contract's own address, so no two are equal and a pattern dictionary collapses them all.
func fixtureCode(addr []byte) []byte {
	code := bytes.Repeat([]byte{0x5b}, 24576)
	copy(code, []byte{0x61, 0x5f, 0xff, 0x56})
	copy(code[44:], addr)
	return code
}

// TestFilesErigonReadsBack writes all four domains through snap.Writer, then reads every key back
// with Erigon's own readers, the accessor the schema gives each domain (B-tree or hash map) and the
// .kv at the schema's compression, and checks an absent key isn't found. Each domain the schema
// compresses must also carry a pattern dictionary: a file written without one reads fine but costs
// a node more pages per read (study round 14). Whether a node picks the files up by name and
// version is the e2e suite's job: Erigon's aggregator (db/state) doesn't build under gofuzz.
func TestFilesErigonReadsBack(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	w, err := snap.NewWriter(dir, snap.Settings{Seed: 42, Files: Files{Workers: 2}})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	rnd := rand.New(rand.NewSource(1))
	run := bytes.Repeat([]byte{0xab, 0xcd, 0xef, 0x01}, 8) // a 32-byte run every value and slot shares
	const n = 600
	pairs := map[snap.Domain][]snap.DomainEntry{}
	for i := range n {
		addr := make([]byte, 20)
		binary.BigEndian.PutUint64(addr[12:], uint64(i+1))
		var code []byte
		if i%2 == 0 {
			code = fixtureCode(addr)
		} else {
			code = make([]byte, 1000+rnd.Intn(4000))
			rnd.Read(code)
		}
		pairs[snap.DomainCode] = append(pairs[snap.DomainCode], snap.DomainEntry{Key: addr, Value: code})
		acct := append(append([]byte{}, run...), byte(i))
		pairs[snap.DomainAccounts] = append(pairs[snap.DomainAccounts], snap.DomainEntry{Key: addr, Value: acct})
		for s := range 3 {
			key := append(append(append([]byte{}, addr...), run...), byte(s))
			val := append(append([]byte{}, run...), byte(s))
			pairs[snap.DomainStorage] = append(pairs[snap.DomainStorage], snap.DomainEntry{Key: key, Value: val})
		}
		// Branch-like rows: a short nibble prefix and a run-heavy value.
		prefix := binary.BigEndian.AppendUint16([]byte{0x00}, uint16(i))
		pairs[snap.DomainCommitment] = append(pairs[snap.DomainCommitment], snap.DomainEntry{Key: append(prefix, run...), Value: append(append([]byte{}, run...), addr...)})
	}

	r := snap.StepRange{From: 0, To: 1}
	for d, ps := range pairs {
		if err := w.WriteDomain(ctx, d, r, uint64(len(ps)), func(yield func(snap.DomainEntry) bool) {
			for _, p := range ps {
				if !yield(p) {
					return
				}
			}
		}); err != nil {
			t.Fatalf("WriteDomain(%d): %v", d, err)
		}
	}

	for d, ps := range pairs {
		cfg, err := domainCfg(d)
		if err != nil {
			t.Fatal(err)
		}
		paths, err := filePaths(snap.DomainDir(dir), d, r)
		if err != nil {
			t.Fatal(err)
		}
		for ext, p := range paths {
			if _, err := os.Stat(p); err != nil {
				t.Errorf("%s: .%s: %v", cfg.Name, ext, err)
			}
		}
		readBack(t, cfg, paths, ps)
	}
}

func readBack(t *testing.T, cfg statecfg.DomainCfg, paths map[string]string, ps []snap.DomainEntry) {
	t.Helper()
	dec, err := seg.NewDecompressor(paths["kv"])
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	if hasDict, compressed := dec.DictWords() > 0, cfg.Compression != seg.CompressNone; hasDict != compressed {
		t.Errorf("%s: pattern dictionary %v, schema compresses %v", cfg.Name, hasDict, compressed)
	}
	g := seg.NewReader(dec.MakeGetter(), cfg.Compression)

	var get func(key []byte) (k, v []byte, found bool)
	switch {
	case cfg.Accessors.Has(statecfg.AccessorBTree):
		btDec, bt, err := btindex.OpenBtreeIndexAndDataFile(paths["bt"], paths["kv"], cfg.Compression)
		if err != nil {
			t.Fatalf("%s: Erigon can't open the B-tree: %v", cfg.Name, err)
		}
		defer btDec.Close()
		defer bt.Close()
		btg := seg.NewReader(btDec.MakeGetter(), cfg.Compression)
		get = func(key []byte) ([]byte, []byte, bool) {
			k, v, _, found, err := bt.Get(key, nil, btg)
			if err != nil {
				t.Fatalf("%s: B-tree lookup of %x: %v", cfg.Name, key, err)
			}
			return k, v, found
		}
	case cfg.Accessors.Has(statecfg.AccessorHashMap):
		idx, err := recsplit.OpenIndex(paths["kvi"])
		if err != nil {
			t.Fatalf("%s: Erigon can't open the hash map: %v", cfg.Name, err)
		}
		defer idx.Close()
		rd := recsplit.NewIndexReader(idx)
		get = func(key []byte) ([]byte, []byte, bool) {
			off, ok := rd.Lookup(key)
			if !ok {
				return nil, nil, false
			}
			g.Reset(off)
			k, _ := g.Next(nil)
			v, _ := g.Next(nil)
			return k, v, bytes.Equal(k, key)
		}
	default:
		t.Fatalf("%s: schema lists no key accessor", cfg.Name)
	}

	for _, p := range ps {
		k, v, found := get(p.Key)
		if !found || !bytes.Equal(k, p.Key) || !bytes.Equal(v, p.Value) {
			t.Fatalf("%s: Erigon's read of %x: found=%v", cfg.Name, p.Key, found)
		}
	}
	if _, _, found := get(bytes.Repeat([]byte{0xee}, len(ps[0].Key))); found {
		t.Errorf("%s: an absent key was found", cfg.Name)
	}
}
