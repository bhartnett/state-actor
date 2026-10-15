// Package erigon holds the Erigon step layout state-actor writes into
// erigondb.toml. The pinned Erigon commit lives in Dockerfile.erigon.
package erigon

const (
	// StepSize is the default Erigon step size in txnums (per
	// db/config3/config3.go:29). state-actor writes this exact value
	// into erigondb.toml so step boundaries align with Erigon's
	// expectations.
	StepSize uint64 = 390_625

	// StepsInFrozenFile is the default Erigon steps-per-frozen-file
	// (per db/config3/config3.go:34). 256 steps × 390_625 txnums =
	// 100_000_000 txnums ≈ 20k blocks at ~5k tx/block.
	StepsInFrozenFile uint64 = 256
)
