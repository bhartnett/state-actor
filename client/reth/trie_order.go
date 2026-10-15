package reth

import (
	"bytes"
	"sort"

	iReth "github.com/ethereum/state-actor/internal/reth"
)

type trieRow struct{ key, val []byte }

// trieGroupWriter turns HashBuilder's post-order emission into ascending
// Puts, so libmdbx packs the table densely. Every row under a 2-nibble prefix
// arrives contiguously (sibling subtrees complete in ascending nibble order),
// so a prefix's rows are buffered and flushed sorted when the next prefix
// starts. Rows above that depth (at most 16, emitted after their subtrees)
// cannot be ordered without buffering the whole trie and go straight through
// as single-row batches; that is also why the table takes plain Put, not
// cursor Append. Under the alloy_trie branching rule, a group buffers at
// most ~N/3800 of the N stored rows at a time, not the whole trie.
type trieGroupWriter struct {
	put   func(rows []trieRow) error
	group int
	rows  []trieRow
}

func (w *trieGroupWriter) add(path iReth.StoredNibbles, key, val []byte) error {
	if path.Length < 2 {
		return w.put([]trieRow{{key, val}})
	}
	g := int(path.Nibbles[0])<<4 | int(path.Nibbles[1])
	if len(w.rows) > 0 && g != w.group {
		if err := w.flush(); err != nil {
			return err
		}
	}
	w.group = g
	w.rows = append(w.rows, trieRow{key, val})
	return nil
}

func (w *trieGroupWriter) flush() error {
	if len(w.rows) == 0 {
		return nil
	}
	sort.Slice(w.rows, func(i, j int) bool { return bytes.Compare(w.rows[i].key, w.rows[j].key) < 0 })
	err := w.put(w.rows)
	w.rows = w.rows[:0]
	return err
}
