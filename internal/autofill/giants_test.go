package autofill

import (
	"bytes"
	"slices"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ethereum/state-actor/internal/sizecal"
)

// At jochemnet's own giant count the power law must land near its measured giant slots.
func TestGiantSlotSumMatchesMainnet(t *testing.T) {
	const measured = 550_578_056
	sum := giantSlotSum(mainnetGiantAccounts)
	if d := float64(sum) - measured; d > 0.01*measured || d < -0.01*measured {
		t.Errorf("giantSlotSum(%d) = %d, more than 1%% from jochemnet's %d", mainnetGiantAccounts, sum, measured)
	}
}

// Drawn contracts at the table mean plus the giants must fill the storage budget.
func TestMainnetPlanFillsStorageBudget(t *testing.T) {
	for _, topUp := range []uint64{1 << 30, 3 << 30, 100 << 30, 1 << 40} {
		plan, err := PlanForBudgetProfile(topUp, ProfileMainnet, StorageMainnet)
		if err != nil {
			t.Fatal(err)
		}
		budget := uint64(float64(topUp)*sizecal.RatioStorage) / sizecal.BytesPerSlot("")
		got := uint64(plan.NumContracts)*mainnetMeanSlotsMilli/1000 + plan.GiantSlots
		if d := float64(got) - float64(budget); d > 0.01*float64(budget) || d < -0.01*float64(budget) {
			t.Errorf("%d B: %d drawn + %d giants give %d slots, budget %d", topUp, plan.NumContracts, plan.Giants, got, budget)
		}
		if plan.Giants == 0 {
			t.Errorf("%d B: no giants", topUp)
		}
	}
}

// Sorted must yield exactly Slot(0..Slots-1), keyed by keccak(key), strictly ascending, whether the
// slots fit one range or span many.
func TestGiantSortedYieldsAllSlotsInKeccakOrder(t *testing.T) {
	plan, err := PlanForBudgetProfile(3<<30, ProfileMainnet, StorageMainnet)
	if err != nil {
		t.Fatal(err)
	}
	g := plan.Giant(7, 0)
	g.Slots = 10_000
	want := make([]common.Hash, 0, g.Slots)
	for j := range g.Slots {
		key, value := g.Slot(j)
		if value == (common.Hash{}) {
			t.Fatalf("slot %d has a zero value", j)
		}
		want = append(want, crypto.Keccak256Hash(key[:]))
	}
	slices.SortFunc(want, func(a, b common.Hash) int { return bytes.Compare(a[:], b[:]) })

	for _, rangeSlots := range []uint64{g.Slots, 999, 37} { // 1, 11 and 256 (capped) ranges
		var got []common.Hash
		g.sorted(rangeSlots, func(keyHash, key, _ common.Hash) bool {
			if crypto.Keccak256Hash(key[:]) != keyHash {
				t.Fatalf("rangeSlots %d: keyHash does not match key", rangeSlots)
			}
			got = append(got, keyHash)
			return true
		})
		if !slices.Equal(got, want) {
			t.Fatalf("rangeSlots %d: yielded %d slots, not the %d slots in keccak order", rangeSlots, len(got), len(want))
		}
	}

	n := 0
	g.sorted(37, func(_, _, _ common.Hash) bool { n++; return n < 5 })
	if n != 5 {
		t.Fatalf("early stop yielded %d slots, want 5", n)
	}
}

// Giants come largest first, with distinct addresses, and are a pure function of (seed, k).
func TestGiantHeaders(t *testing.T) {
	plan, err := PlanForBudgetProfile(100<<30, ProfileMainnet, StorageMainnet)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[common.Address]bool{}
	prev := ^uint64(0)
	for k := range plan.Giants {
		g := plan.Giant(1, k)
		if g.Slots > prev || g.Slots < GiantMinSlots {
			t.Errorf("giant %d: %d slots after %d", k, g.Slots, prev)
		}
		prev = g.Slots
		if seen[g.Address] {
			t.Errorf("giant %d: duplicate address %s", k, g.Address)
		}
		seen[g.Address] = true
		if again := plan.Giant(1, k); again.Address != g.Address || again.Balance.Cmp(g.Balance) != 0 || again.CodeHash != g.CodeHash {
			t.Errorf("giant %d: header differs between calls", k)
		}
	}
}
