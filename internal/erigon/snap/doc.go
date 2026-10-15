// Package snap lays out an Erigon datadir's frozen state snapshots:
// the snapshots/ directory tree, salt-state.txt, erigondb.toml, and the
// commitment domain's state-row splice. It hands each domain's sorted
// entries to a FileSet, which writes the files themselves.
//
// The file format (encoding, accessors, names, versions) is Erigon's
// and changes between Erigon releases, so it lives behind FileSet:
// internal/erigon/erigonfiles writes every file with the pinned Erigon's
// own code. This package does NOT import github.com/erigontech/erigon,
// so builds without the cgo_erigon_commitment tag still compile.
package snap
