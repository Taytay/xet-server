# `xet-server/internal/merklehash`

```
package merklehash // import "xet-server/internal/merklehash"

Package merklehash ports xet-core's DataHash type and Merkle-aggregation
algorithm (xet_core_structures/src/merklehash/*.rs) to Go, byte-for-byte, so
hashes computed here are identical to hashes computed by real xet-core / hf_xet
for the same input. This is required for wire compatibility: every chunk hash,
xorb hash, and file hash exchanged over the CAS API must match what a real
client independently computes.

TYPES

type ChunkEntry struct {
	Hash Hash
	Size uint64
}
    ChunkEntry is one chunk's hash and uncompressed byte length, the input unit
    for XorbHash/FileHash/FileHashWithSalt.

type Hash [32]byte
    Hash is xet-core's DataHash: a 256-bit value stored as 4 little-endian
    u64 words (mirrored here as a plain 32-byte array in wire order — see
    Hex/FromHex for the non-obvious byte-order translation).

func ComputeDataHash(slice []byte) Hash
    ComputeDataHash hashes slice as a Merkle leaf (a chunk's content), using the
    same keyed BLAKE3 hash as xet-core's compute_data_hash.

func ComputeInternalNodeHash(slice []byte) Hash
    ComputeInternalNodeHash hashes slice as a Merkle interior node, using the
    same keyed BLAKE3 hash as xet-core's compute_internal_node_hash.

func FileHash(chunks []ChunkEntry) Hash
    FileHash computes a file's content hash with no salt (all-zero salt),
    mirroring xet-core's file_hash.

func FileHashWithSalt(chunks []ChunkEntry, salt Hash) Hash
    FileHashWithSalt computes a file's content hash from its ordered chunk list,
    salted with an HMAC key, mirroring xet-core's file_hash_with_salt.

func FromHex(s string) (Hash, error)
    FromHex parses a hash from its Hex() representation (64 lowercase hex
    chars), inverting the byte-order translation described on Hex.

func FromRawBytes(b []byte) (Hash, error)
    FromRawBytes constructs a Hash directly from its 32 raw wire-order bytes (as
    read from a binary format field), with no byte-order translation.

func XorbHash(chunks []ChunkEntry) Hash
    XorbHash computes a xorb's content hash from its ordered chunk list,
    mirroring xet-core's xorb_hash: the Merkle aggregation of chunk hashes with
    no salt.

func (h Hash) Bytes() []byte
    Bytes returns the 32 raw wire-order bytes, with no byte-order translation —
    the inverse of FromRawBytes, for writing into a binary format field.

func (h Hash) HMAC(key Hash) Hash
    HMAC computes a BLAKE3 keyed hash of h using key, matching DataHash::hmac in
    xet-core (used to salt file hashes).

func (h Hash) Hex() string
    Hex renders the hash the same way xet-core's DataHash::hex() does: the 32
    bytes are reinterpreted as four little-endian u64 words, and each word is
    printed as 16 lowercase hex digits in *big-endian* (normal) digit order.
    Because a little-endian u64's most-significant byte is its last byte in
    memory, this means each 8-byte group's byte order is reversed relative to
    a naive hex-encode of the raw bytes — verified against xet-core's own test
    vector in the package tests.

func (h Hash) IsZero() bool
    IsZero reports whether h is the all-zero hash (xet-core's default/empty
    Merkle hash).

func (h Hash) Mod64(m uint64) uint64
    Mod64 returns h's low 64-bit word (interpreted little-endian, matching
    DataHash's `Rem<u64>` impl) modulo m. Used by the aggregation algorithm's
    natural-cut rule.

func (h Hash) String() string

func (h Hash) TruncateHash() uint64
    TruncateHash returns h's first 64-bit word, matching xet-core's
    truncate_hash (metadata_shard/utils.rs: `hash.deref()[0]`). Used as the
    sort/lookup key in a shard's file/xorb/chunk lookup tables — a different
    word than Mod64 uses, so the two must not be confused.
```
