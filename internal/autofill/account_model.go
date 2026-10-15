package autofill

import (
	"bytes"
	"encoding/binary"
	"fmt"
	mrand "math/rand"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/ethereum/state-actor/internal/entitygen"
)

// AccountModel draws storage-less accounts (EOA draws) and storage accounts' nonce/balance from
// mainnet_account_table.go, for --account-model=mainnet. nil is the normal model:
// entitygen.GenerateEOAFlavored's shape, unconditionally.
//
// Every distribution is an exact integer CDF (rng.Int63n over a total, then sort.Search), so every
// platform draws the same accounts.
//
// ponytail: storage-bearing EOAs and delegations (~2% of storage accounts) are not modelled
// separately — every storage account draws contract_storage's nonce/balance. Upgrade path: a class
// weight for storage accounts, like the EOA draw's.
type AccountModel struct {
	// eoaClassCum[i] = accounts across classes 0..i of mainnetAccountClasses (eoa, delegation,
	// contract).
	eoaClassCum [3]uint64

	nonceLen   [3]lenDist
	balanceLen [3]lenDist

	storageNonceLen   lenDist
	storageBalanceLen lenDist

	codes *codeModel
}

// lenDist is an integer CDF over a byte-length histogram: cum[i] counts accounts with length <= i.
type lenDist struct {
	cum   []uint64
	total uint64
}

func newLenDist(counts []uint64) lenDist {
	d := lenDist{cum: make([]uint64, len(counts))}
	for i, n := range counts {
		d.total += n
		d.cum[i] = d.total
	}
	return d
}

// draw returns a length, one rng.Int63n draw over the exact CDF.
func (d *lenDist) draw(rng *mrand.Rand) int {
	r := uint64(rng.Int63n(int64(d.total)))
	return sort.Search(len(d.cum), func(i int) bool { return d.cum[i] > r })
}

// drawSignificantUint64 draws a uint64 with exactly n (0..8) significant bytes: n random bytes with
// a non-zero leading byte. n=0 is zero (no draw).
func drawSignificantUint64(rng *mrand.Rand, n int) uint64 {
	if n == 0 {
		return 0
	}
	var buf [8]byte
	rng.Read(buf[8-n:])
	if buf[8-n] == 0 {
		buf[8-n] = 1
	}
	return binary.BigEndian.Uint64(buf[:])
}

// drawSignificantUint256 draws a *uint256.Int with exactly n (0..32) significant bytes.
func drawSignificantUint256(rng *mrand.Rand, n int) *uint256.Int {
	if n == 0 {
		return uint256.NewInt(0)
	}
	var buf [32]byte
	rng.Read(buf[32-n:])
	if buf[32-n] == 0 {
		buf[32-n] = 1
	}
	return new(uint256.Int).SetBytes(buf[:])
}

// ParseAccountModel maps a CLI string ("" / "normal" / "mainnet") to a model; nil is normal. Each
// call builds a fresh model: its code cache is not safe for concurrent plans.
func ParseAccountModel(s string) (*AccountModel, error) {
	switch s {
	case "", "normal":
		return nil, nil
	case "mainnet":
		return newMainnetAccountModel(), nil
	default:
		return nil, fmt.Errorf("autofill: unknown account model %q (want 'normal' or 'mainnet')", s)
	}
}

func newMainnetAccountModel() *AccountModel {
	am := &AccountModel{}
	var cum uint64
	for i, cls := range mainnetAccountClasses[:3] {
		cum += cls.count
		am.eoaClassCum[i] = cum
		am.nonceLen[i] = newLenDist(cls.nonceLen)
		am.balanceLen[i] = newLenDist(cls.balanceLen)
	}
	storage := mainnetAccountClasses[3]
	am.storageNonceLen = newLenDist(storage.nonceLen)
	am.storageBalanceLen = newLenDist(storage.balanceLen)
	am.codes = newMainnetCodeModel()
	return am
}

// drawEOA draws one storage-less account. RNG draw order (fixed; an independent branch from
// generateEOAFlavored's sequence, used only when AccountModel != nil):
//  1. rng.Read(addr[20])
//  2. class: rng.Int63n over the 3 storage-less classes (eoa/delegation/contract)
//  3. nonce length: rng.Int63n over the class's nonce CDF, then rng.Read(the length's bytes)
//  4. balance length: rng.Int63n over the class's balance CDF, then rng.Read(the length's bytes)
//  5. class-specific: eoa draws nothing further; delegation draws rng.Intn(DelegationTargetPoolSize)
//     (the normal model's fixed target pool); contract draws one rng.Int63n code rank (see
//     codeModel.draw).
func (am *AccountModel) drawEOA(rng *mrand.Rand, skipDerivedHashes bool) *entitygen.Account {
	var addr common.Address
	rng.Read(addr[:])

	r := uint64(rng.Int63n(int64(am.eoaClassCum[2])))
	class := sort.Search(3, func(i int) bool { return am.eoaClassCum[i] > r })

	nonce := drawSignificantUint64(rng, am.nonceLen[class].draw(rng))
	balance := drawSignificantUint256(rng, am.balanceLen[class].draw(rng))

	acc := &entitygen.Account{
		Address: addr,
		StateAccount: &types.StateAccount{
			Nonce:    nonce,
			Balance:  balance,
			Root:     types.EmptyRootHash,
			CodeHash: types.EmptyCodeHash.Bytes(),
		},
	}

	switch class {
	case 1: // delegation
		target := delegationTargets[rng.Intn(len(delegationTargets))]
		code := make([]byte, 0, 23)
		code = append(code, delegationPrefix...)
		code = append(code, target[:]...)
		acc.Code = code
		if !skipDerivedHashes {
			h := crypto.Keccak256Hash(code)
			acc.CodeHash = h
			acc.StateAccount.CodeHash = h.Bytes()
		}
	case 2: // contract
		rank := am.codes.draw(rng)
		acc.Code = am.codes.codeBytes(rank)
		if !skipDerivedHashes {
			h := am.codes.codeHash(rank)
			acc.CodeHash = h
			acc.StateAccount.CodeHash = h.Bytes()
		}
	}

	if !skipDerivedHashes {
		acc.AddrHash = crypto.Keccak256Hash(addr[:])
	}
	return acc
}

// DrawStorageNonceBalance draws a storage account's nonce and balance from contract_storage, for
// DrawContract's contracts and the giants' headers.
//
// RNG draw order: nonce length, nonce bytes, balance length, balance bytes.
func (am *AccountModel) DrawStorageNonceBalance(rng *mrand.Rand) (uint64, *uint256.Int) {
	nonce := drawSignificantUint64(rng, am.storageNonceLen.draw(rng))
	balance := drawSignificantUint256(rng, am.storageBalanceLen.draw(rng))
	return nonce, balance
}

// drawContract builds a storage-bearing contract: address, nonce and balance from contract_storage,
// then numSlots key/value pairs. Its own draw order, independent of entitygen.finishContract's
// (which stays pinned by the normal model's goldens).
//
// RNG draw order: 1. rng.Read(addr) 2. DrawStorageNonceBalance(rng) 3. for each of numSlots:
// rng.Read(key), then rng.Read(value), or valueLen(rng) and that many value bytes when valueLen is
// non-nil.
func (am *AccountModel) drawContract(rng *mrand.Rand, code []byte, codeHash common.Hash, numSlots int, valueLen func(*mrand.Rand) int) *entitygen.Account {
	var addr common.Address
	rng.Read(addr[:])

	nonce, balance := am.DrawStorageNonceBalance(rng)

	storage := make([]entitygen.StorageSlot, 0, numSlots)
	for range numSlots {
		var key, value common.Hash
		rng.Read(key[:])
		if valueLen == nil {
			rng.Read(value[:])
			if value == (common.Hash{}) {
				value[31] = 1
			}
		} else {
			// n significant bytes, non-zero leading byte (entitygen.finishContract's mainnet shape).
			n := valueLen(rng)
			rng.Read(value[32-n:])
			if value[32-n] == 0 {
				value[32-n] = 1
			}
		}
		storage = append(storage, entitygen.StorageSlot{Key: key, Value: value})
	}
	sort.Slice(storage, func(i, j int) bool { return bytes.Compare(storage[i].Key[:], storage[j].Key[:]) < 0 })

	return &entitygen.Account{
		Address:  addr,
		AddrHash: crypto.Keccak256Hash(addr[:]),
		StateAccount: &types.StateAccount{
			Nonce:    nonce,
			Balance:  balance,
			Root:     types.EmptyRootHash,
			CodeHash: codeHash.Bytes(),
		},
		Code:     code,
		CodeHash: codeHash,
		Storage:  storage,
	}
}

// codeModel draws a storage-less contract's code: deterministic per rank (codeForRank), weighted
// by mainnetTopCodes exactly, then by mainnetRestCodeGroups. Rest ranks get lengths through a fixed
// bijection onto mainnetRestCodeLen's knots.
type codeModel struct {
	topCum    []uint64 // topCum[i] = accounts across top codes 0..i (rank == i)
	topWeight uint64

	restGroupCum    []uint64 // cumulative accounts (accounts per code * codes) across groups 0..i
	restGroupOffset []uint64 // codes before group i (rank base within rest)
	restTotalCodes  uint64
	restLenPermA    uint64 // multiplier coprime with restTotalCodes: rank -> length bijection

	totalWeight uint64

	cache map[int]codeEntry // rank -> generated code (+ lazily-derived hash); single-threaded
}

type codeEntry struct {
	code      []byte
	hash      common.Hash
	hashReady bool
}

func newMainnetCodeModel() *codeModel {
	cm := &codeModel{
		topCum:          make([]uint64, len(mainnetTopCodes)),
		restGroupCum:    make([]uint64, len(mainnetRestCodeGroups)),
		restGroupOffset: make([]uint64, len(mainnetRestCodeGroups)),
		cache:           make(map[int]codeEntry),
	}
	for i, t := range mainnetTopCodes {
		cm.topWeight += t[0]
		cm.topCum[i] = cm.topWeight
	}
	var restWeight uint64
	for i, g := range mainnetRestCodeGroups {
		cm.restGroupOffset[i] = cm.restTotalCodes
		cm.restTotalCodes += g[1]
		restWeight += g[0] * g[1]
		cm.restGroupCum[i] = restWeight
	}
	cm.totalWeight = cm.topWeight + restWeight
	cm.restLenPermA = coprimeMultiplier(cm.restTotalCodes)
	return cm
}

// draw returns a code rank, one rng.Int63n draw weighted by account count: top codes exactly; rest
// codes by one draw that picks both the group and, by integer division within the group's weight
// span, a uniform code inside it (every code in a group has the same accounts per code).
func (cm *codeModel) draw(rng *mrand.Rand) int {
	r := uint64(rng.Int63n(int64(cm.totalWeight)))
	if r < cm.topWeight {
		return sort.Search(len(cm.topCum), func(i int) bool { return cm.topCum[i] > r })
	}
	r -= cm.topWeight
	g := sort.Search(len(cm.restGroupCum), func(i int) bool { return cm.restGroupCum[i] > r })
	var base uint64
	if g > 0 {
		base = cm.restGroupCum[g-1]
	}
	within := (r - base) / mainnetRestCodeGroups[g][0]
	return len(cm.topCum) + int(cm.restGroupOffset[g]) + int(within)
}

// codeLen returns rank's code length: exact for a top rank; for a rest rank, the knot interval its
// bijection target falls in, uniform inside the interval.
func (cm *codeModel) codeLen(rank int) int {
	if rank < len(mainnetTopCodes) {
		return int(mainnetTopCodes[rank][1])
	}
	local := uint64(rank-len(mainnetTopCodes)) * cm.restLenPermA % cm.restTotalCodes
	k := mainnetRestCodeLen[:]
	i := sort.Search(len(k), func(i int) bool { return k[i][1] > local })
	var lo, below uint64
	if i > 0 {
		lo, below = k[i-1][0], k[i-1][1]
	}
	return int(lo + 1 + (local-below)*(k[i][0]-lo)/(k[i][1]-below))
}

// codeBytes returns rank's generated code, generating and caching it on first use; never hashes.
func (cm *codeModel) codeBytes(rank int) []byte {
	e, ok := cm.cache[rank]
	if !ok {
		e = codeEntry{code: codeForRank(rank, cm.codeLen(rank))}
		cm.cache[rank] = e
	}
	return e.code
}

// codeHash returns keccak256(codeBytes(rank)), computed at most once per rank. codeBytes(rank)
// must be called first.
func (cm *codeModel) codeHash(rank int) common.Hash {
	e := cm.cache[rank]
	if !e.hashReady {
		e.hash = crypto.Keccak256Hash(e.code)
		e.hashReady = true
		cm.cache[rank] = e
	}
	return e.hash
}

// codeDomain seeds storage-less contract code bytes: fixed (not --seed-derived), like
// delegationTargets, so every client and run draws byte-identical code for a rank.
const codeDomain = "state-actor/account-model/code/v1"

// codeForRank expands a code's bytes from its rank: keccak256(domain || BE u64 rank || BE u32 block)
// per 32-byte block, truncated to length. A leading 0xEF is cleared so a code never parses as an
// EIP-7702 designator or an EOF container.
func codeForRank(rank, length int) []byte {
	if length <= 0 {
		length = 1
	}
	var preimage [len(codeDomain) + 8 + 4]byte
	copy(preimage[:], codeDomain)
	binary.BigEndian.PutUint64(preimage[len(codeDomain):], uint64(rank))

	code := make([]byte, 0, length)
	for block := uint32(0); len(code) < length; block++ {
		binary.BigEndian.PutUint32(preimage[len(codeDomain)+8:], block)
		h := crypto.Keccak256(preimage[:])
		code = append(code, h[:min(length-len(code), len(h))]...)
	}
	if code[0] == 0xEF {
		code[0] = 0x00
	}
	return code
}

// coprimeMultiplier returns a multiplier m with gcd(m, n) == 1, so rank*m mod n is a bijection on
// [0, n). Starts from Knuth's multiplicative-hashing constant so ranks spread across lengths.
func coprimeMultiplier(n uint64) uint64 {
	if n <= 1 {
		return 1
	}
	m := uint64(0x9E3779B97F4A7C15) % n
	if m == 0 {
		m = 1
	}
	for gcd(m, n) != 1 {
		m++
		if m >= n {
			m = 1
		}
	}
	return m
}

func gcd(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
