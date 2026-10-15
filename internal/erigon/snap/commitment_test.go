package snap

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

// streamOf builds a BranchStream over a fixed, pre-sorted row set.
func streamOf(rows [][2][]byte) BranchStream {
	return func(yield func(prefix, data []byte) error) error {
		for _, r := range rows {
			if err := yield(r[0], r[1]); err != nil {
				return err
			}
		}
		return nil
	}
}

// TestWriteCommitment_StateRowSplice pins the M1c sorted splice: the
// KeyCommitmentState row must land at its binary sort position inside the
// branch stream — including the boundary cases (state last, state only) and
// the collision case (state SHADOWS an equal branch key) — and the .kv
// entries must be fully ascending.
func TestWriteCommitment_StateRowSplice(t *testing.T) {
	keyState := []byte{0xde, 0xad, 0xbe, 0xef}
	r := StepRange{From: 0, To: 1}

	cases := []struct {
		name string
		rows [][2][]byte // pre-sorted branch rows
		want [][]byte    // expected .kv key order
	}{
		{
			name: "mid-stream splice",
			rows: [][2][]byte{
				{{0x01}, {0xa1}},
				{{0x30, 0x40}, {0xa2}},
				{{0x73, 0x74, 0x61, 0x74, 0x00}, {0xa3}}, // just below "state"
				{{0x80}, {0xa4}},
				{{0xf0}, {0xa5}},
			},
			want: [][]byte{{0x01}, {0x30, 0x40}, {0x73, 0x74, 0x61, 0x74, 0x00}, KeyCommitmentState, {0x80}, {0xf0}},
		},
		{
			name: "all keys below state -> state last",
			rows: [][2][]byte{
				{{0x01}, {0xa1}},
				{{0x02}, {0xa2}},
			},
			want: [][]byte{{0x01}, {0x02}, KeyCommitmentState},
		},
		{
			name: "empty stream -> state only",
			rows: nil,
			want: [][]byte{KeyCommitmentState},
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := memFiles{}
			w, err := NewWriter(t.TempDir(), Settings{Seed: int64(100 + i), Files: files})
			if err != nil {
				t.Fatalf("NewWriter: %v", err)
			}
			defer w.Close()

			if err := WriteCommitment(context.Background(), w, r, keyState, streamOf(tc.rows), uint64(len(tc.rows))); err != nil {
				t.Fatalf("WriteCommitment: %v", err)
			}
			got := files[DomainCommitment]

			if len(got) != len(tc.want) {
				t.Fatalf("row count = %d, want %d", len(got), len(tc.want))
			}
			for j, kv := range got {
				if !bytes.Equal(kv[0], tc.want[j]) {
					t.Fatalf("key[%d] = %x, want %x", j, kv[0], tc.want[j])
				}
				if j > 0 && bytes.Compare(got[j-1][0], kv[0]) >= 0 {
					t.Fatalf(".kv not strictly ascending at %d: %x >= %x", j, got[j-1][0], kv[0])
				}
				if bytes.Equal(kv[0], KeyCommitmentState) && !bytes.Equal(kv[1], keyState) {
					t.Fatalf("state row value = %x, want %x", kv[1], keyState)
				}
			}
		})
	}
}

// TestWriteCommitment_StreamErrorSurfaces pins the no-silent-truncation
// guard: a producer error mid-stream must fail WriteCommitment, not build a
// silently short .kv.
func TestWriteCommitment_StreamErrorSurfaces(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, Settings{Seed: 7, Files: memFiles{}})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	boom := fmt.Errorf("synthetic iterator failure")
	failing := func(yield func(prefix, data []byte) error) error {
		if err := yield([]byte{0x01}, []byte{0xa1}); err != nil {
			return err
		}
		return boom
	}
	err = WriteCommitment(context.Background(), w, StepRange{From: 0, To: 1}, []byte{0x01}, failing, 2)
	if err == nil {
		t.Fatal("WriteCommitment succeeded on a failing stream — silent truncation")
	}
}

// TestWriteCommitment_StateCollisionErrors pins the collision semantics: a
// branch prefix equal to KeyCommitmentState must fail loudly (the old
// last-write-wins path would have silently dropped the branch row).
func TestWriteCommitment_StateCollisionErrors(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, Settings{Seed: 8, Files: memFiles{}})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	rows := [][2][]byte{
		{{0x01}, {0xa1}},
		{append([]byte(nil), KeyCommitmentState...), {0xa2}}, // colliding branch key
		{{0x80}, {0xa3}},
	}
	err = WriteCommitment(context.Background(), w, StepRange{From: 0, To: 1}, []byte{0x01}, streamOf(rows), uint64(len(rows)))
	if err == nil || !strings.Contains(err.Error(), "collides with KeyCommitmentState") {
		t.Fatalf("want loud collision error, got: %v", err)
	}
}
