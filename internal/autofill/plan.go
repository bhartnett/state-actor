package autofill

import (
	"fmt"
	mrand "math/rand"

	"github.com/ethereum/state-actor/internal/entitygen"
	"github.com/ethereum/state-actor/internal/sizecal"
)

// Plan is the deterministic recipe for filling target_size − spec_cost
// bytes of synthetic state in mainnet-shaped proportions. PlanForBudget
// constructs it once from the top-up budget; DrawEOA / DrawContract are
// then called per-entity by the writer until NumEOAs / NumContracts are
// exhausted (or the on-disk SizeTracker stops the loop early).
type Plan struct {
	NumEOAs        int
	NumContracts   int
	EOAFlavors     EOAFlavors
	StorageSampler Sampler
	CodeSampler    Sampler
	// DistinctBytecodes is the code-pool size: each contract draws one of
	// these shared blobs (see poolCode).
	DistinctBytecodes int
	// StorageModel selects how DrawContract sizes and fills a contract's storage.
	StorageModel StorageModel
	// AccountModel shapes DrawEOA's storage-less accounts and every storage account's nonce and
	// balance (account_model.go); nil is the normal model.
	AccountModel *AccountModel
	// Giants is the StorageMainnet contract count at or above GiantMinSlots (see Giant), not
	// counted in NumContracts; GiantSlots is their slot total.
	Giants     int
	GiantSlots uint64

	// SkipDerivedHashes elides the RNG-neutral derived-hash work on the
	// single-threaded draw goroutine (the AddrHash keccak and the
	// delegation CodeHash keccak — neither consumes RNG draws, so the drawn
	// byte-sequence and cross-client roots are identical either way). Safe
	// ONLY for writers that never read Account.AddrHash/CodeHash and re-derive
	// what they need off the draw goroutine (erigon: keys by plain address,
	// re-hashes code on its encode workers). geth/reth/besu/nethermind/ethrex
	// consume AddrHash and MUST leave this false (the default).
	SkipDerivedHashes bool
}

// Profile selects the account/code/storage byte-split AutoFill targets.
type Profile int

const (
	// ProfileMainnet is the default 20/10/70 account/code/storage split.
	ProfileMainnet Profile = iota
	// ProfileAccounts biases the entire budget to EOAs (account-trie only:
	// no contracts, no storage). It generates account-dominated tries that
	// stress the account-domain index + commitment paths at scale while
	// staying on the bounded streaming DrawEOA path — e.g. the 100 GB
	// account-heavy low-memory bench. DrawEOA's RNG draw sequence is
	// unchanged, so cross-client state-root invariance holds as long as
	// every client uses the same profile + seed.
	ProfileAccounts
)

// ratios returns the (account, code, storage) byte-budget fractions for p.
func (p Profile) ratios() (account, code, storage float64) {
	if p == ProfileAccounts {
		return 1.0, 0.0, 0.0
	}
	return sizecal.RatioAccount, sizecal.RatioCode, sizecal.RatioStorage
}

// ParseProfile maps a CLI string ("" / "mainnet" / "accounts") to a Profile.
func ParseProfile(s string) (Profile, error) {
	switch s {
	case "", "mainnet":
		return ProfileMainnet, nil
	case "accounts":
		return ProfileAccounts, nil
	default:
		return ProfileMainnet, fmt.Errorf("autofill: unknown profile %q (want 'mainnet' or 'accounts')", s)
	}
}

// PlanForBudget builds a mainnet-shaped (20/10/70) Plan with the default storage model. Thin wrapper
// over PlanForBudgetProfile(topUp, ProfileMainnet, StorageNormal).
func PlanForBudget(topUp uint64) (*Plan, error) {
	return PlanForBudgetProfile(topUp, ProfileMainnet, StorageNormal)
}

// PlanForBudgetProfile builds a Plan that, if fully consumed, would emit
// roughly topUp bytes of synthetic state split per the profile's
// account/code/storage ratios (mainnet = 20/10/70; accounts = 100/0/0).
//
// The contract count is pinned by the bytecode budget so per-contract code
// stays mainnet-realistic (truncated normal in [1 KiB, 24 KiB] centered at
// 5 KiB). Mean storage-per-contract is then derived from the storage
// budget so the math closes; with the canonical ratios it lands at ≈ 35 KiB
// regardless of topUp scale. ProfileAccounts zeroes the code+storage
// budgets, so numContracts == 0 and the whole budget becomes EOAs.
//
// Returns an error when topUp is below the account-trie cost of a single
// EOA (175 B). A non-nil Plan is safe to consume even when NumContracts is
// zero (e.g. when bytecode budget is below the minimum code size).
//
// StorageMainnet instead derives the contract count from the storage budget: drawn contracts at the
// table's mean slot count plus Giants at or above GiantMinSlots (solveMainnetContracts). Contracts
// are drawn out of the account budget, so the total account count is the same under both models.
func PlanForBudgetProfile(topUp uint64, profile Profile, model StorageModel) (*Plan, error) {
	bytesPerAccount := sizecal.BytesPerAccount("")
	if topUp < bytesPerAccount {
		return nil, fmt.Errorf("autofill: target_size %d B below one account (%d B)", topUp, bytesPerAccount)
	}

	rAccount, rCode, rStorage := profile.ratios()
	budgetF := float64(topUp)
	bAcct := uint64(budgetF * rAccount)
	bCode := uint64(budgetF * rCode)
	bStorage := uint64(budgetF * rStorage)

	var numContracts, giants int
	storageSampler := Sampler{
		Min: sizecal.MinContractStorage,
		Max: sizecal.MaxContractStorage,
	}

	bytesPerSlot := sizecal.BytesPerSlot("")
	switch {
	case model == StorageMainnet && bStorage >= bytesPerSlot:
		numContracts, giants = solveMainnetContracts(bStorage / bytesPerSlot)
	case bCode >= sizecal.MinContractCode && bStorage >= bytesPerSlot:
		// Round bCode / MeanContractCode → contract count.
		numContracts = int((bCode + sizecal.MeanContractCode/2) / sizecal.MeanContractCode)
		if numContracts > 0 {
			mean := float64(bStorage) / float64(numContracts)
			mean = max(mean, float64(sizecal.MinContractStorage))
			mean = min(mean, float64(sizecal.MaxContractStorage))
			storageSampler.Mean = mean
			storageSampler.Stddev = mean / 3
		}
	}

	totalContracts := numContracts + giants
	accountUsed := uint64(totalContracts) * bytesPerAccount
	var numEOAs int
	if bAcct > accountUsed {
		numEOAs = int((bAcct - accountUsed) / bytesPerAccount)
	}

	if numEOAs == 0 && totalContracts == 0 {
		return nil, fmt.Errorf("autofill: budget %d B too small for any entity", topUp)
	}

	codeMean := float64(sizecal.MeanContractCode)
	codeSampler := Sampler{
		Mean:   codeMean,
		Stddev: codeMean / 3,
		Min:    sizecal.MinContractCode,
		Max:    sizecal.MaxContractCode,
	}

	return &Plan{
		NumEOAs:           numEOAs,
		NumContracts:      numContracts,
		EOAFlavors:        DefaultEOAFlavors(),
		CodeSampler:       codeSampler,
		StorageSampler:    storageSampler,
		DistinctBytecodes: max(totalContracts/sizecal.MainnetAccountsPerDistinctBytecode, 1),
		StorageModel:      model,
		Giants:            giants,
		GiantSlots:        giantSlotSum(giants),
	}, nil
}

// DrawEOA produces one synthetic EOA. Under the normal model it advances rng by the canonical 3
// entitygen.GenerateEOA draws plus 2 Bernoulli Float64s plus a conditional pool-index draw (when
// the delegation Bernoulli fires); a non-nil AccountModel draws per AccountModel.drawEOA instead.
func (p *Plan) DrawEOA(rng *mrand.Rand) *entitygen.Account {
	if p.AccountModel != nil {
		return p.AccountModel.drawEOA(rng, p.SkipDerivedHashes)
	}
	if p.SkipDerivedHashes {
		return GenerateEOAFlavoredLean(rng, p.EOAFlavors)
	}
	return GenerateEOAFlavored(rng, p.EOAFlavors)
}

// DrawContract produces one synthetic contract. RNG draw order is fixed
// across all client emission sites:
//  1. the slot count: StorageSampler.Draw(rng) bytes / sizecal.BytesPerSlot, or one
//     rng.Float64 under StorageMainnet;
//  2. rng.Intn(DistinctBytecodes) — code-pool index (see poolCode);
//  3. entitygen.GenerateContractWithCode(rng, code, hash, numSlots, valueLen), valueLen
//     non-nil (mainnet value lengths) only under StorageMainnet; AccountModel.drawContract
//     instead under a non-nil AccountModel.
func (p *Plan) DrawContract(rng *mrand.Rand) *entitygen.Account {
	var numSlots int
	var valueLen func(*mrand.Rand) int
	if p.StorageModel == StorageMainnet {
		numSlots, valueLen = drawMainnetSlots(rng), drawMainnetValueLen
	} else {
		numSlots = max(int(p.StorageSampler.Draw(rng)/sizecal.BytesPerSlot("")), 1)
	}
	code, hash := poolCode(rng.Intn(p.DistinctBytecodes), p.CodeSampler)
	if p.AccountModel != nil {
		return p.AccountModel.drawContract(rng, code, hash, numSlots, valueLen)
	}
	return entitygen.GenerateContractWithCode(rng, code, hash, numSlots, valueLen)
}
