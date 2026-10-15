package account

import (
	"encoding/binary"
	"fmt"
	"math/bits"
)

// SerialiseV3 encodes a for the Erigon E3 snapshot Accounts domain
// (`<version>-accounts.<from>-<to>.kv` value bytes). Mirrors upstream
// `execution/types/accounts/account.go::SerialiseV3` — the E3 snapshot
// encoder (distinct from Erigon's older MDBX PlainState codec, which
// uses a fieldset bitmask instead of fixed length-prefixed fields).
//
// On-disk layout: ALWAYS four length-prefixed fields in fixed order
// (nonce, balance, codeHash, incarnation). Each field is one length
// byte followed by that many big-endian bytes; length byte = 0 means
// "absent / zero / empty" and no data bytes follow.
//
// Field semantics:
//
//	nonce       len = bitLenToByteLen(bits.Len64(a.Nonce)); 0 if Nonce==0
//	balance     len = a.Balance.ByteLen();                  0 if Balance.IsZero()
//	codeHash    len = 32 if !IsEmptyCodeHash() else 0       (no other values)
//	incarnation len = bitLenToByteLen(bits.Len64(a.Inc));   0 if Incarnation==0
//
// Minimum length = 4 bytes (all-zero account).
// Maximum length = 4 + 8 + 32 + 32 + 8 = 84 bytes.
//
// The MSB-first variable-length loop for nonce/incarnation is kept
// inline here (rather than via a helper) to avoid an allocation in the
// hot path.
func SerialiseV3(a Account) []byte {
	// Pre-compute total length.
	nonceLen := 0
	if a.Nonce > 0 {
		nonceLen = bitLenToByteLen(bits.Len64(a.Nonce))
	}
	balanceLen := 0
	if !a.Balance.IsZero() {
		balanceLen = a.Balance.ByteLen()
	}
	codeHashLen := 0
	if !a.IsEmptyCodeHash() {
		codeHashLen = 32
	}
	incLen := 0
	if a.Incarnation > 0 {
		incLen = bitLenToByteLen(bits.Len64(a.Incarnation))
	}
	totalLen := 4 + nonceLen + balanceLen + codeHashLen + incLen
	out := make([]byte, totalLen)

	pos := 0

	// Nonce.
	out[pos] = byte(nonceLen)
	pos++
	if nonceLen > 0 {
		n := a.Nonce
		for i := nonceLen; i > 0; i-- {
			out[pos+i-1] = byte(n)
			n >>= 8
		}
		pos += nonceLen
	}

	// Balance.
	out[pos] = byte(balanceLen)
	pos++
	if balanceLen > 0 {
		a.Balance.WriteToSlice(out[pos : pos+balanceLen])
		pos += balanceLen
	}

	// CodeHash.
	out[pos] = byte(codeHashLen)
	pos++
	if codeHashLen > 0 {
		copy(out[pos:pos+codeHashLen], a.CodeHash[:])
		pos += codeHashLen
	}

	// Incarnation.
	out[pos] = byte(incLen)
	pos++
	if incLen > 0 {
		inc := a.Incarnation
		for i := incLen; i > 0; i-- {
			out[pos+i-1] = byte(inc)
			inc >>= 8
		}
	}

	return out
}

// DeserialiseV3 parses the SerialiseV3 encoding back into an Account.
// The Account's CodeHash field is set to EmptyCodeHash when the
// codeHash length byte is 0 (matching how SerialiseV3 round-trips
// "no code" accounts).
//
// Returns an error if any length prefix points past the end of buf.
func DeserialiseV3(buf []byte) (Account, error) {
	a := Account{CodeHash: EmptyCodeHash}
	pos := 0

	// Nonce.
	if pos >= len(buf) {
		return a, fmt.Errorf("account: nonce length byte out of range")
	}
	n := int(buf[pos])
	pos++
	if n > 8 {
		return a, fmt.Errorf("account: nonce length %d exceeds u64 capacity", n)
	}
	if len(buf) < pos+n {
		return a, fmt.Errorf("account: nonce field truncated (need %d, have %d)", n, len(buf)-pos)
	}
	if n > 0 {
		a.Nonce = bytesToUint64BE(buf[pos : pos+n])
		pos += n
	}

	// Balance.
	if pos >= len(buf) {
		return a, fmt.Errorf("account: balance length byte out of range")
	}
	n = int(buf[pos])
	pos++
	if n > 32 {
		return a, fmt.Errorf("account: balance length %d exceeds u256 capacity", n)
	}
	if len(buf) < pos+n {
		return a, fmt.Errorf("account: balance field truncated (need %d, have %d)", n, len(buf)-pos)
	}
	if n > 0 {
		a.Balance.SetBytes(buf[pos : pos+n])
		pos += n
	}

	// CodeHash.
	if pos >= len(buf) {
		return a, fmt.Errorf("account: codeHash length byte out of range")
	}
	n = int(buf[pos])
	pos++
	if n != 0 && n != 32 {
		return a, fmt.Errorf("account: codeHash length must be 0 or 32, got %d", n)
	}
	if len(buf) < pos+n {
		return a, fmt.Errorf("account: codeHash field truncated (need %d, have %d)", n, len(buf)-pos)
	}
	if n == 32 {
		copy(a.CodeHash[:], buf[pos:pos+n])
		pos += n
	}

	// Incarnation.
	if pos >= len(buf) {
		return a, fmt.Errorf("account: incarnation length byte out of range")
	}
	n = int(buf[pos])
	pos++
	if n > 8 {
		return a, fmt.Errorf("account: incarnation length %d exceeds u64 capacity", n)
	}
	if len(buf) < pos+n {
		return a, fmt.Errorf("account: incarnation field truncated (need %d, have %d)", n, len(buf)-pos)
	}
	if n > 0 {
		a.Incarnation = bytesToUint64BE(buf[pos : pos+n])
	}

	return a, nil
}

// bytesToUint64BE parses up to 8 big-endian bytes (any length 0..8)
// into a uint64.
func bytesToUint64BE(b []byte) uint64 {
	if len(b) == 0 {
		return 0
	}
	if len(b) >= 8 {
		return binary.BigEndian.Uint64(b[len(b)-8:])
	}
	var tmp [8]byte
	copy(tmp[8-len(b):], b)
	return binary.BigEndian.Uint64(tmp[:])
}
