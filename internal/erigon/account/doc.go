// Package account is a pure-Go port of Erigon's account value codec for
// the E3 Accounts domain — the binary format Erigon uses for value bytes
// in the `<version>-accounts.<from>-<to>.kv` snapshot files.
//
// # Spec source
//
// Mirrors `execution/types/accounts/account.go::SerialiseV3` from Erigon
// v3.4.2 (the exact upstream commit is pinned in Dockerfile.erigon).
//
// # Wire format (SerialiseV3)
//
// Always four length-prefixed fields, in fixed order:
//
//	nonce       1 length byte + N big-endian bytes (0 if Nonce==0)
//	balance     1 length byte + N big-endian bytes (0 if Balance==0)
//	codeHash    1 length byte + 32 bytes           (0 if EmptyCodeHash)
//	incarnation 1 length byte + N big-endian bytes (0 if Incarnation==0)
//
// A length byte of 0 means the field is absent and no data bytes follow.
// Minimum length is 4 bytes (all-zero account); maximum is 84.
//
// # Differences from go-ethereum's StateAccount
//
// NOT an RLP encoding, and NOT the bytes geth/reth use for MPT leaves.
// Erigon stores domain values in this format and recomputes the MPT
// (state) root separately via its HexPatriciaHashed commitment tree
// (see internal/erigon/commitment).
package account
