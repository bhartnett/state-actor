package snap

import "context"

// Domain identifies one of Erigon's E3 state domains.
type Domain int

const (
	DomainAccounts Domain = iota
	DomainStorage
	DomainCode
	DomainCommitment
)

// StepRange identifies the [From, To) STEP range covered by a snapshot
// file. Erigon's step size is fixed by config3.DefaultStepSize
// (390_625 txnums); a frozen file holds StepsInFrozenFile (256) of
// them ⇒ ~100M txnums ⇒ ~20k blocks at 5k tx/block.
type StepRange struct {
	From uint64
	To   uint64
}

// DomainEntry is one (key, value) pair to write into a domain. Keys
// MUST be sorted ascending before calling WriteDomain: Erigon's file
// format and its accessors both depend on monotonic key order.
type DomainEntry struct {
	Key   []byte
	Value []byte
}

// FileSet writes one domain's file set (.kv and its accessors) under
// domainDir. Erigon decides the encoding, the accessors and the format
// versions per domain, and changes them between releases, so the only
// implementation is internal/erigon/erigonfiles, which hands every file
// to the pinned Erigon's own writers (cgo_erigon_commitment build). This
// package stays free of the erigontech/erigon import.
type FileSet interface {
	// WriteDomain writes entries (ascending keys, iterated once) as the
	// domain's step range r. keyCount is how many entries there are; a
	// different count is an error. salt is the snapshot salt the
	// accessors are built with (salt-state.txt).
	WriteDomain(ctx context.Context, domainDir, tmpDir string, d Domain, r StepRange, keyCount uint64, salt uint32,
		entries func(yield func(DomainEntry) bool)) error
}

// Settings governs SnapshotWriter behaviour: step boundaries, the salt
// policy and the FileSet that writes each domain.
type Settings struct {
	// StepSize matches Erigon's config3.DefaultStepSize (390_625).
	// Override only for tests; production must match.
	StepSize uint64

	// StepsInFrozenFile matches Erigon's config3.DefaultStepsInFrozenFile
	// (256). One frozen file covers StepSize × StepsInFrozenFile txnums.
	StepsInFrozenFile uint64

	// Salt is the 4-byte snapshot salt written to salt-state.txt.
	// If 0, NewWriter derives it deterministically from Seed via
	// DeriveSaltFromSeed (so two runs with the same seed produce
	// byte-identical snapshot files including the salt-dependent
	// existence filters).
	Salt uint32

	// Seed is state-actor's run-wide seed; threads into Salt if Salt==0.
	Seed int64

	// Files writes each domain's file set. Required.
	Files FileSet
}
