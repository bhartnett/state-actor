package autofill

import (
	"bytes"
	"encoding/binary"
	"math/bits"
	mrand "math/rand"
	"math/rand/v2"
	"runtime"
	"slices"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
)

// giantAddrDomain separates giant preimages from DrawContract's raw-rng addresses and specbuild's
// keccak(seed||name) addresses.
const giantAddrDomain = "state-actor/giant/v1"

// giantRangeSlots is the slot count Giant.Sorted sorts at once (~40 MB of keccak/index pairs).
const giantRangeSlots = 1 << 20

// giantSortsInFlight is how many sorted ranges one Giant.Sorted keeps ahead of its consumer.
const giantSortsInFlight = 2

// solveMainnetContracts splits budgetSlots into drawn contracts at mainnetMeanSlotsMilli and n giants
// with n = round(C*t/T), C the total contract count: a bounded fixed-point search from the drawn-only
// estimate. n drops until the giants fit the budget, so small budgets get none.
func solveMainnetContracts(budgetSlots uint64) (drawn, giants int) {
	c := budgetSlots * 1000 / mainnetMeanSlotsMilli
	var n uint64
	for range 8 {
		n = (2*c*mainnetGiantAccounts + mainnetStorageAccounts) / (2 * mainnetStorageAccounts)
		for n > 0 && giantSlotSum(int(n)) > budgetSlots {
			n--
		}
		next := (budgetSlots-giantSlotSum(int(n)))*1000/mainnetMeanSlotsMilli + n
		if next == c {
			break
		}
		c = next
	}
	return int(c - n), int(n)
}

// giantSlots returns x_i, the i-th (1-based, i=1..n) stratified quantile of an alpha=1 power law
// starting at X = GiantMinSlots: x_i = X * 2n^2 / (2n^2 - (2i-1)(n-1)). x_1 ≈ X, x_n ≈ 2Xn/3.
func giantSlots(n, i int) uint64 {
	nn, ii := uint64(n), uint64(i)
	return GiantMinSlots * 2 * nn * nn / (2*nn*nn - (2*ii-1)*(nn-1))
}

// giantSlotSum returns the total slots of n giants.
func giantSlotSum(n int) uint64 {
	var sum uint64
	for i := 1; i <= n; i++ {
		sum += giantSlots(n, i)
	}
	return sum
}

// Giant is one StorageMainnet contract at or above GiantMinSlots. Its storage is never held whole:
// Slot generates any slot on demand, and Sorted streams them in keccak(key) order.
type Giant struct {
	Address  common.Address
	AddrHash common.Hash
	Nonce    uint64
	Balance  *uint256.Int
	Code     []byte
	CodeHash common.Hash
	Slots    uint64

	slotSeed uint64
}

// Giant returns giant k (0 <= k < p.Giants, largest first) as a pure function of (seed, k). It never
// touches the auto-fill rng, so giants can be generated in any order, concurrently with the draws.
// A non-nil AccountModel draws the nonce and balance (AccountModel.DrawStorageNonceBalance).
func (p *Plan) Giant(seed int64, k int) *Giant {
	i := p.Giants - k // stratum, 1-based
	addrHash := giantHash("addr", seed, i)
	h := giantHash("header", seed, i)
	r := rand.New(rand.NewPCG(binary.BigEndian.Uint64(h[:8]), binary.BigEndian.Uint64(h[8:16])))
	code, codeHash := poolCode(r.IntN(p.DistinctBytecodes), p.CodeSampler)
	g := &Giant{
		Balance:  new(uint256.Int).Mul(uint256.NewInt(uint64(r.IntN(100))), uint256.NewInt(1e18)),
		Nonce:    uint64(r.IntN(1000)),
		Code:     code,
		CodeHash: codeHash,
		Slots:    giantSlots(p.Giants, i),
		slotSeed: binary.BigEndian.Uint64(giantHash("storage", seed, i).Bytes()),
	}
	copy(g.Address[:], addrHash[12:])
	if p.AccountModel != nil {
		g.Nonce, g.Balance = p.AccountModel.DrawStorageNonceBalance(mrand.New(mrand.NewSource(int64(binary.BigEndian.Uint64(h[16:24])))))
	}
	g.AddrHash = crypto.Keccak256Hash(g.Address[:])
	return g
}

func giantHash(domain string, seed int64, i int) common.Hash {
	var buf [16]byte
	binary.BigEndian.PutUint64(buf[:8], uint64(seed))
	binary.BigEndian.PutUint64(buf[8:], uint64(i))
	return crypto.Keccak256Hash([]byte(giantAddrDomain+"/"+domain), buf[:])
}

// Slot returns slot j (0 <= j < Slots): a random key and a value with mainnet's significant-byte
// distribution and a non-zero leading byte. Each slot has its own PCG stream, so any slot range can
// be generated independently.
func (g *Giant) Slot(j uint64) (key, value common.Hash) {
	r := rand.NewPCG(g.slotSeed, j)
	for o := 0; o < 32; o += 8 {
		binary.BigEndian.PutUint64(key[o:], r.Uint64())
	}
	n := mainnetValueLen(float64(r.Uint64()>>11) / (1 << 53))
	for o := 0; o < 32; o += 8 {
		binary.BigEndian.PutUint64(value[o:], r.Uint64())
	}
	clear(value[:32-n])
	if value[32-n] == 0 {
		value[32-n] = 1
	}
	return key, value
}

// Sorted yields every slot as (keccak(key), key, value) in ascending keccak(key) order, without
// touching disk: one byte per slot assigns each to one of R keccak ranges, and ranges are sorted
// giantSortsInFlight at a time, in parallel, then replayed. Each key is hashed twice.
func (g *Giant) Sorted(yield func(keyHash, key, value common.Hash) bool) {
	g.sorted(giantRangeSlots, yield)
}

type hashedSlot struct {
	hash common.Hash
	j    uint64
}

func (g *Giant) sorted(rangeSlots uint64, yield func(keyHash, key, value common.Hash) bool) {
	numRanges := int(min(256, (g.Slots+rangeSlots-1)/rangeSlots))
	if numRanges == 0 {
		return
	}
	var ranges []byte
	if numRanges > 1 {
		ranges = g.assignRanges(uint64(numRanges))
	}

	out := make([]chan []hashedSlot, numRanges)
	for r := range out {
		out[r] = make(chan []hashedSlot, 1)
	}
	sem := make(chan struct{}, giantSortsInFlight)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for r := range numRanges {
			select {
			case sem <- struct{}{}:
			case <-stop:
				return
			}
			go func() { out[r] <- g.sortRange(ranges, byte(r)) }()
		}
	}()
	for r := range numRanges {
		for _, s := range <-out[r] {
			key, value := g.Slot(s.j)
			if !yield(s.hash, key, value) {
				return
			}
		}
		<-sem
	}
}

// assignRanges returns range[j] = the keccak range of slot j, splitting the 64-bit keccak prefix
// space into numRanges equal, ordered parts. Hashes in parallel across GOMAXPROCS.
func (g *Giant) assignRanges(numRanges uint64) []byte {
	ranges := make([]byte, g.Slots)
	chunk := g.Slots/uint64(runtime.GOMAXPROCS(0)) + 1
	var wg sync.WaitGroup
	for lo := uint64(0); lo < g.Slots; lo += chunk {
		wg.Go(func() {
			kh := crypto.NewKeccakState()
			for j := lo; j < min(lo+chunk, g.Slots); j++ {
				key, _ := g.Slot(j)
				h := crypto.HashData(kh, key[:])
				r, _ := bits.Mul64(binary.BigEndian.Uint64(h[:8]), numRanges)
				ranges[j] = byte(r)
			}
		})
	}
	wg.Wait()
	return ranges
}

// sortRange returns range r's slots sorted by keccak(key); nil ranges means every slot.
func (g *Giant) sortRange(ranges []byte, r byte) []hashedSlot {
	kh := crypto.NewKeccakState()
	var out []hashedSlot
	add := func(j uint64) {
		key, _ := g.Slot(j)
		out = append(out, hashedSlot{crypto.HashData(kh, key[:]), j})
	}
	if ranges == nil {
		out = make([]hashedSlot, 0, g.Slots)
		for j := range g.Slots {
			add(j)
		}
	} else {
		for off := 0; ; {
			k := bytes.IndexByte(ranges[off:], r)
			if k < 0 {
				break
			}
			add(uint64(off + k))
			off += k + 1
		}
	}
	slices.SortFunc(out, func(a, b hashedSlot) int { return bytes.Compare(a.hash[:], b.hash[:]) })
	return out
}
