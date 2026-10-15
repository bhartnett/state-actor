package autofill

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	mrand "math/rand"
	"testing"

	"github.com/holiman/uint256"
)

func TestParseStorageModel(t *testing.T) {
	for in, want := range map[string]StorageModel{"": StorageNormal, "normal": StorageNormal, "mainnet": StorageMainnet} {
		got, err := ParseStorageModel(in)
		if err != nil || got != want {
			t.Errorf("ParseStorageModel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseStorageModel("heavy"); err == nil {
		t.Error("ParseStorageModel(heavy) should error")
	}
}

// The account budget alone sets the total account count, so switching the storage model may move
// accounts between EOAs, contracts and giants but never change their sum.
func TestMainnetPlanKeepsAccountTotal(t *testing.T) {
	const budget = 10 << 30
	normal, err := PlanForBudgetProfile(budget, ProfileMainnet, StorageNormal)
	if err != nil {
		t.Fatal(err)
	}
	mainnet, err := PlanForBudgetProfile(budget, ProfileMainnet, StorageMainnet)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := normal.NumEOAs+normal.NumContracts, mainnet.NumEOAs+mainnet.NumContracts+mainnet.Giants; a-b > 1 || b-a > 1 {
		t.Errorf("account total moved: normal %d, mainnet %d", a, b)
	}
	if mainnet.NumContracts <= normal.NumContracts {
		t.Errorf("mainnet has %d contracts, normal %d: small mainnet contracts should need more of them for the same storage budget",
			mainnet.NumContracts, normal.NumContracts)
	}
}

// Draws must reproduce the calibrated shape: over half the contracts hold one slot, about 97% hold
// at most 28, none reaches a giant; values carry exactly their drawn significant bytes, about half
// of them 20 (addresses).
func TestMainnetDrawsMatchCalibration(t *testing.T) {
	rng := mrand.New(mrand.NewSource(1))
	const n = 200_000
	var one, small int
	for range n {
		s := drawMainnetSlots(rng)
		if s < 1 || s >= GiantMinSlots {
			t.Fatalf("slot count %d outside [1, %d)", s, GiantMinSlots)
		}
		if s == 1 {
			one++
		}
		if s <= 28 {
			small++
		}
	}
	if got := float64(one) / n; got < 0.545 || got > 0.555 {
		t.Errorf("share with one slot = %.4f, want ≈0.5497", got)
	}
	if got := float64(small) / n; got < 0.968 || got > 0.974 {
		t.Errorf("share with ≤28 slots = %.4f, want ≈0.9712", got)
	}

	plan, err := PlanForBudgetProfile(1<<30, ProfileMainnet, StorageMainnet)
	if err != nil {
		t.Fatal(err)
	}
	var vals, twenty int
	for range 20_000 {
		for _, s := range plan.DrawContract(rng).Storage {
			v := new(uint256.Int).SetBytes32(s.Value[:])
			l := (v.BitLen() + 7) / 8
			if l < 1 || l > 32 {
				t.Fatalf("value %x has %d significant bytes", s.Value, l)
			}
			vals++
			if l == 20 {
				twenty++
			}
		}
	}
	if got := float64(twenty) / float64(vals); got < 0.47 || got > 0.53 {
		t.Errorf("share of 20-byte values = %.4f over %d values, want ≈0.498", got, vals)
	}
}

// storageMainnetDrawDigest pins the first 1000 StorageMainnet plan.DrawContract outputs for a fixed
// seed: sha256 over each contract's address, slot count and every slot's key+value, in draw order.
// Catches any accidental drift in the mainnet draw stream (table edits, RNG reordering).
//
// To update (when intentional): change t.Errorf to t.Logf below, run the test, copy the printed
// digest here.
const storageMainnetDrawDigest = "bbb7a861ba168ae576cbab68587196e302dde24e4fa519affb92ae4f7f9c87d1"

func TestMainnetDrawStreamGolden(t *testing.T) {
	plan, err := PlanForBudgetProfile(1<<30, ProfileMainnet, StorageMainnet)
	if err != nil {
		t.Fatal(err)
	}
	rng := mrand.New(mrand.NewSource(7))
	h := sha256.New()
	var buf [8]byte
	for range 1000 {
		acc := plan.DrawContract(rng)
		h.Write(acc.Address[:])
		binary.BigEndian.PutUint64(buf[:], uint64(len(acc.Storage)))
		h.Write(buf[:])
		for _, s := range acc.Storage {
			h.Write(s.Key[:])
			h.Write(s.Value[:])
		}
	}
	if got := fmt.Sprintf("%x", h.Sum(nil)); got != storageMainnetDrawDigest {
		t.Errorf("StorageMainnet draw stream digest = %s, want %s (table or draw order changed)", got, storageMainnetDrawDigest)
	}
}
