// Package merklehash ports xet-core's DataHash type and Merkle-aggregation
// algorithm (xet_core_structures/src/merklehash/*.rs) to Go, byte-for-byte,
// so hashes computed here are identical to hashes computed by real xet-core
// / hf_xet for the same input. This is required for wire compatibility:
// every chunk hash, xorb hash, and file hash exchanged over the CAS API
// must match what a real client independently computes.
package merklehash

import (
	"encoding/hex"
	"fmt"

	"github.com/zeebo/blake3"
)

// Hash is xet-core's DataHash: a 256-bit value stored as 4 little-endian
// u64 words (mirrored here as a plain 32-byte array in wire order — see
// Hex/FromHex for the non-obvious byte-order translation).
type Hash [32]byte

// dataKey is the BLAKE3 key used for compute_data_hash (leaf/chunk hashes),
// ported verbatim from xet_core_structures/src/merklehash/data_hash.rs.
var dataKey = [32]byte{
	102, 151, 245, 119, 91, 149, 80, 222, 49, 53, 203, 172, 165, 151, 24, 28,
	157, 228, 33, 16, 155, 235, 43, 88, 180, 208, 176, 75, 147, 173, 242, 41,
}

// internalNodeKey is the BLAKE3 key used for compute_internal_node_hash
// (interior Merkle-tree nodes), ported verbatim from the same file.
var internalNodeKey = [32]byte{
	1, 126, 197, 199, 165, 71, 41, 150, 253, 148, 102, 102, 180, 138, 2, 230,
	93, 221, 83, 111, 55, 199, 109, 210, 248, 99, 82, 230, 74, 83, 113, 63,
}

// ComputeDataHash hashes slice as a Merkle leaf (a chunk's content), using
// the same keyed BLAKE3 hash as xet-core's compute_data_hash.
func ComputeDataHash(slice []byte) Hash {
	sum := newKeyedHasher(dataKey)
	sum.Write(slice)
	var h Hash
	copy(h[:], sum.Sum(nil))
	return h
}

// ComputeInternalNodeHash hashes slice as a Merkle interior node, using the
// same keyed BLAKE3 hash as xet-core's compute_internal_node_hash.
func ComputeInternalNodeHash(slice []byte) Hash {
	sum := newKeyedHasher(internalNodeKey)
	sum.Write(slice)
	var h Hash
	copy(h[:], sum.Sum(nil))
	return h
}

// HMAC computes a BLAKE3 keyed hash of h using key, matching DataHash::hmac
// in xet-core (used to salt file hashes).
func (h Hash) HMAC(key Hash) Hash {
	sum := newKeyedHasher([32]byte(key))
	sum.Write(h[:])
	var out Hash
	copy(out[:], sum.Sum(nil))
	return out
}

// newKeyedHasher wraps blake3.NewKeyed for a fixed-size 32-byte key, which
// can never fail the library's length check — panicking here would only
// ever indicate a bug in this file, not a runtime condition callers need
// to handle.
func newKeyedHasher(key [32]byte) *blake3.Hasher {
	h, err := blake3.NewKeyed(key[:])
	if err != nil {
		panic("merklehash: NewKeyed rejected a 32-byte key: " + err.Error())
	}
	return h
}

// asWords reinterprets the 32 raw bytes as four little-endian u64 words, the
// same layout Rust's `[u64; 4]` has in memory on a little-endian host (which
// is what every relevant platform here is). Word i occupies bytes
// [8*i : 8*i+8).
func (h Hash) asWords() [4]uint64 {
	var w [4]uint64
	for i := 0; i < 4; i++ {
		w[i] = uint64(h[8*i]) | uint64(h[8*i+1])<<8 | uint64(h[8*i+2])<<16 | uint64(h[8*i+3])<<24 |
			uint64(h[8*i+4])<<32 | uint64(h[8*i+5])<<40 | uint64(h[8*i+6])<<48 | uint64(h[8*i+7])<<56
	}
	return w
}

func fromWords(w [4]uint64) Hash {
	var h Hash
	for i := 0; i < 4; i++ {
		v := w[i]
		h[8*i] = byte(v)
		h[8*i+1] = byte(v >> 8)
		h[8*i+2] = byte(v >> 16)
		h[8*i+3] = byte(v >> 24)
		h[8*i+4] = byte(v >> 32)
		h[8*i+5] = byte(v >> 40)
		h[8*i+6] = byte(v >> 48)
		h[8*i+7] = byte(v >> 56)
	}
	return h
}

// Hex renders the hash the same way xet-core's DataHash::hex() does: the 32
// bytes are reinterpreted as four little-endian u64 words, and each word is
// printed as 16 lowercase hex digits in *big-endian* (normal) digit order.
// Because a little-endian u64's most-significant byte is its last byte in
// memory, this means each 8-byte group's byte order is reversed relative to
// a naive hex-encode of the raw bytes — verified against xet-core's own
// test vector in the package tests.
func (h Hash) Hex() string {
	w := h.asWords()
	return fmt.Sprintf("%016x%016x%016x%016x", w[0], w[1], w[2], w[3])
}

func (h Hash) String() string { return h.Hex() }

// FromHex parses a hash from its Hex() representation (64 lowercase hex
// chars), inverting the byte-order translation described on Hex.
func FromHex(s string) (Hash, error) {
	if len(s) != 64 {
		return Hash{}, fmt.Errorf("merklehash: invalid hex length %d, want 64", len(s))
	}
	var w [4]uint64
	for i := 0; i < 4; i++ {
		var b [8]byte
		if _, err := hex.Decode(b[:], []byte(s[16*i:16*i+16])); err != nil {
			return Hash{}, fmt.Errorf("merklehash: invalid hex: %w", err)
		}
		w[i] = uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
			uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
	}
	return fromWords(w), nil
}

// FromRawBytes constructs a Hash directly from its 32 raw wire-order bytes
// (as read from a binary format field), with no byte-order translation.
func FromRawBytes(b []byte) (Hash, error) {
	if len(b) != 32 {
		return Hash{}, fmt.Errorf("merklehash: invalid byte length %d, want 32", len(b))
	}
	var h Hash
	copy(h[:], b)
	return h, nil
}

// Bytes returns the 32 raw wire-order bytes, with no byte-order translation
// — the inverse of FromRawBytes, for writing into a binary format field.
func (h Hash) Bytes() []byte {
	b := make([]byte, 32)
	copy(b, h[:])
	return b
}

// Mod64 returns h's low 64-bit word (interpreted little-endian, matching
// DataHash's `Rem<u64>` impl) modulo m. Used by the aggregation algorithm's
// natural-cut rule.
func (h Hash) Mod64(m uint64) uint64 {
	return h.asWords()[3] % m
}

// TruncateHash returns h's first 64-bit word, matching xet-core's
// truncate_hash (metadata_shard/utils.rs: `hash.deref()[0]`). Used as the
// sort/lookup key in a shard's file/xorb/chunk lookup tables — a different
// word than Mod64 uses, so the two must not be confused.
func (h Hash) TruncateHash() uint64 {
	return h.asWords()[0]
}

// IsZero reports whether h is the all-zero hash (xet-core's default/empty
// Merkle hash).
func (h Hash) IsZero() bool {
	return h == Hash{}
}
