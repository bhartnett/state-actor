// Package erigon writes a bootable Erigon v3 chaindata directory by
// driving the pinned `erigon init` CLI against a synthetic genesis.json
// and then writing bulk state (accounts, storage slots, contract code,
// commitment) into Erigon's snapshot flat-file tier via the streaming
// snapshot orchestrator.
//
// The MDBX touchpoint shrinks to ONE write: block 0's header.stateRoot
// is patched via the inline-mdbx `patchGenesisHeaderStateRoot` helper
// (commitment_cgo.go) so the daemon's first-FCU root validation sees a
// header consistent with the snapshot-tier bloat state.
//
// Build: requires the cgo_erigon tag, which pulls in
// github.com/erigontech/mdbx-go for the header-patch MDBX write +
// cgo_erigon_commitment for the HexPatriciaHashed commitment vendor.
// Without the tag, run_stub.go returns a clear error directing the user
// at Dockerfile.erigon.
//
//	docker build -f Dockerfile.erigon -t state-actor-erigon .
//	docker run --rm -v $PWD/_artifacts:/data state-actor-erigon \
//	  --client=erigon --db=/data --target-size=500MB --seed=42
//
// Pinned Erigon commit: see Dockerfile.erigon.
package erigon
