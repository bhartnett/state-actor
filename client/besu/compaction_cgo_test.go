//go:build cgo_besu

package besu

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/linxGnu/grocksdb"

	"github.com/ethereum/state-actor/internal/besu/keys"
)

// TestCloseZeroesBottommostSeqnos pins why Close forces bottommost
// compaction. At RocksDB's default (kIfHaveCompactionFilter, and no filter is
// configured) CompactRange TRIVIALLY MOVES the flushed L0 files into the empty
// bottom level: the tree looks flat, but the files are never rewritten, so
// every one keeps largest_seqno != 0. RocksDB then rewrites exactly those
// files the first time the opening client releases a snapshot
// (ComputeBottommostFilesMarkedForCompaction) — a full rewrite of the store,
// repeated after every restart that does not finish it.
//
// Measured here at 3 flushes: without KForce the bottom level holds 3
// moved files carrying seqnos; with it, 1 rewritten file, all seqnos 0.
func TestCloseZeroesBottommostSeqnos(t *testing.T) {
	datadir := t.TempDir()
	db, err := openBesuDB(datadir)
	if err != nil {
		t.Fatalf("openBesuDB: %v", err)
	}

	// Three disjoint flushes: each lands one L0 file that is individually
	// eligible for a trivial move into the empty bottom level.
	wo := grocksdb.NewDefaultWriteOptions()
	defer wo.Destroy()
	fo := grocksdb.NewDefaultFlushOptions()
	defer fo.Destroy()
	cf := db.cfs[cfIdxAccountInfoState]
	for flush := range 3 {
		for i := range 2000 {
			key := common.BigToHash(common.Big1).Bytes()
			key[0], key[1], key[2] = byte(flush), byte(i), byte(i>>8)
			if err := db.db.PutCF(wo, cf, key, make([]byte, 256)); err != nil {
				t.Fatalf("put: %v", err)
			}
		}
		if err := db.db.FlushCF(cf, fo); err != nil {
			t.Fatalf("flush: %v", err)
		}
	}
	db.Close()

	// Reopen with auto-compactions off so the reader observes what Close
	// left behind, not what a reopen would fix.
	opts := grocksdb.NewDefaultOptions()
	defer opts.Destroy()
	opts.SetDisableAutoCompactions(true)
	names := keys.BonsaiCFNames()
	cfOpts := make([]*grocksdb.Options, len(names))
	for i := range cfOpts {
		cfOpts[i] = opts
	}
	reopened, handles, err := grocksdb.OpenDbColumnFamilies(opts, filepath.Join(datadir, "database"), names, cfOpts)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	for _, h := range handles {
		defer h.Destroy()
	}

	// rocksdb.sstables prints each file's bounding internal keys, which carry
	// their sequence numbers; a rewritten bottommost file reports seq:0.
	for _, line := range strings.Split(reopened.GetPropertyCF("rocksdb.sstables", handles[cfIdxAccountInfoState]), "\n") {
		i := strings.Index(line, "seq:")
		if i < 0 {
			continue
		}
		seq := line[i+len("seq:"):]
		if j := strings.IndexAny(seq, ", "); j > 0 {
			seq = seq[:j]
		}
		if seq != "0" {
			t.Fatalf("bottommost file left at seq:%s — Besu will rewrite the store on its first snapshot release; "+
				"CompactRange ran without bottommost_level_compaction=KForce", seq)
		}
	}
}
