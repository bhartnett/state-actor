//go:build cgo_neth

package nethermind

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/linxGnu/grocksdb"

	"github.com/ethereum/state-actor/internal/neth/flat"
)

// TestCloseZeroesBottommostSeqnos covers the two shapes Close has to get
// right — a plain single-CF DB (state) and a column-family DB (flat) — since
// they are separate call sites and either could lose the KForce option.
//
// Without it, CompactRange trivially MOVES the flushed L0 files into the empty
// bottom level: flat tree, but every file keeps largest_seqno != 0, so
// Nethermind rewrites the whole store once it releases its first snapshot.
func TestCloseZeroesBottommostSeqnos(t *testing.T) {
	dataDir := t.TempDir()
	dbs, err := openNethDBs(dataDir)
	if err != nil {
		t.Fatalf("openNethDBs: %v", err)
	}

	wo := grocksdb.NewDefaultWriteOptions()
	defer wo.Destroy()
	fo := grocksdb.NewDefaultFlushOptions()
	defer fo.Destroy()
	// Three disjoint flushes each land one trivially-movable L0 file.
	for flush := range 3 {
		for i := range 2000 {
			key := make([]byte, 32)
			key[0], key[1], key[2] = byte(flush), byte(i), byte(i>>8)
			if err := dbs.state.Put(wo, key, make([]byte, 256)); err != nil {
				t.Fatalf("state put: %v", err)
			}
			if err := dbs.flat.PutCF(wo, dbs.flatCFs[flat.ColStateNodes], key, make([]byte, 256)); err != nil {
				t.Fatalf("flat put: %v", err)
			}
		}
		if err := dbs.state.Flush(fo); err != nil {
			t.Fatalf("state flush: %v", err)
		}
		if err := dbs.flat.FlushCF(dbs.flatCFs[flat.ColStateNodes], fo); err != nil {
			t.Fatalf("flat flush: %v", err)
		}
	}
	dbs.Close()

	// Reopen with auto-compactions off: observe what Close left, not what a
	// reopen would quietly repair.
	opts := grocksdb.NewDefaultOptions()
	defer opts.Destroy()
	opts.SetDisableAutoCompactions(true)

	state, err := grocksdb.OpenDb(opts, filepath.Join(dataDir, dbNameState))
	if err != nil {
		t.Fatalf("reopen state: %v", err)
	}
	defer state.Close()
	assertSeqnosZeroed(t, dbNameState, state.GetProperty("rocksdb.sstables"))

	cfOpts := make([]*grocksdb.Options, len(flat.ColumnNames))
	for i := range cfOpts {
		cfOpts[i] = opts
	}
	flatDB, handles, err := grocksdb.OpenDbColumnFamilies(opts, filepath.Join(dataDir, dbNameFlat), flat.ColumnNames, cfOpts)
	if err != nil {
		t.Fatalf("reopen flat: %v", err)
	}
	defer flatDB.Close()
	for _, h := range handles {
		defer h.Destroy()
	}
	assertSeqnosZeroed(t, dbNameFlat, flatDB.GetPropertyCF("rocksdb.sstables", handles[flat.ColStateNodes]))
}

// assertSeqnosZeroed fails if any file in the rocksdb.sstables dump still
// carries a non-zero sequence number: that file was moved, not rewritten.
func assertSeqnosZeroed(t *testing.T, db, sstables string) {
	t.Helper()
	for _, line := range strings.Split(sstables, "\n") {
		i := strings.Index(line, "seq:")
		if i < 0 {
			continue
		}
		seq := line[i+len("seq:"):]
		if j := strings.IndexAny(seq, ", "); j > 0 {
			seq = seq[:j]
		}
		if seq != "0" {
			t.Fatalf("%s: bottommost file left at seq:%s — Nethermind will rewrite the store on its first "+
				"snapshot release; CompactRange ran without bottommost_level_compaction=KForce", db, seq)
		}
	}
}
