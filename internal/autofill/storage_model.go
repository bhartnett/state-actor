package autofill

import (
	"fmt"
	mrand "math/rand"
	"sort"
)

// StorageModel selects how auto-fill contracts' storage is drawn.
type StorageModel int

const (
	// StorageNormal gives every contract a budget-derived ~35 KiB of storage (truncated normal)
	// holding 32 random bytes per value. The default.
	StorageNormal StorageModel = iota
	// StorageMainnet draws per-contract slot counts and value lengths from mainnet
	// (mainnet_storage_table.go): most contracts hold one to three slots, and values are compact.
	// Contracts at or above GiantMinSlots follow a power law (giants.go). The contract count follows
	// from the storage budget.
	StorageMainnet
)

// GiantMinSlots splits StorageMainnet contracts into drawn ones (below it, mainnet_storage_table.go)
// and giants (at or above it, giants.go). Above ~10^4 slots mainnet's tail is a power law with
// exponent ~1, so giants scale with the contract count instead of being capped.
const GiantMinSlots = 1_000_000

// ParseStorageModel maps a CLI string ("" / "normal" / "mainnet") to a StorageModel.
func ParseStorageModel(s string) (StorageModel, error) {
	switch s {
	case "", "normal":
		return StorageNormal, nil
	case "mainnet":
		return StorageMainnet, nil
	default:
		return StorageNormal, fmt.Errorf("autofill: unknown storage model %q (want 'normal' or 'mainnet')", s)
	}
}

// drawMainnetSlots draws one contract's slot count (1..GiantMinSlots-1) from a single rng.Float64. It
// uses only IEEE basic arithmetic, never math.Pow or math.Log, so every platform draws the same count
// and the state root stays identical across clients.
func drawMainnetSlots(rng *mrand.Rand) int {
	u := rng.Float64()
	small := mainnetSmallSlotCDF[:]
	if u < small[len(small)-1] {
		return sort.Search(len(small), func(i int) bool { return small[i] > u }) + 1
	}
	t := mainnetSlotTail
	i := sort.Search(len(t), func(i int) bool { return t[i].p > u })
	lo, hi := t[i-1], t[i]
	n := lo.slots + 1 + uint64((u-lo.p)/(hi.p-lo.p)*float64(hi.slots-lo.slots))
	return int(min(n, hi.slots))
}

// drawMainnetValueLen draws a slot value's significant byte count, 1..32.
func drawMainnetValueLen(rng *mrand.Rand) int { return mainnetValueLen(rng.Float64()) }

// mainnetValueLen maps u in [0, 1) to a significant byte count, 1..32.
func mainnetValueLen(u float64) int {
	cdf := mainnetValueLenCDF[:]
	return min(sort.Search(len(cdf), func(i int) bool { return cdf[i] > u }), len(cdf)-1) + 1
}
