package shardformat

import (
	"io"
	"sort"
)

// FileLookupEntry / XorbLookupEntry map a truncated hash (Hash.TruncateHash())
// to the byte-index of the corresponding FileDataSequenceHeader /
// XorbChunkSequenceHeader entry within its content section, mirroring the
// (u64, u32) pairs xet-core writes for the file/xorb lookup tables.
type FileLookupEntry struct {
	Key   uint64
	Index uint32
}

type XorbLookupEntry struct {
	Key   uint64
	Index uint32
}

// ChunkLookupEntry maps a truncated chunk hash to (xorb content-section
// index, chunk index within that xorb), mirroring the chunk lookup table's
// (u64, (u32, u32)) pairs.
type ChunkLookupEntry struct {
	Key        uint64
	XorbIndex  uint32
	ChunkIndex uint32
}

func WriteFileLookupTable(w io.Writer, entries []FileLookupEntry) error {
	for _, e := range entries {
		if err := writeU64(w, e.Key); err != nil {
			return err
		}
		if err := writeU32(w, e.Index); err != nil {
			return err
		}
	}
	return nil
}

// maxLookupEntryPreallocate caps how many entries ReadFileLookupTable /
// ReadXorbLookupTable / ReadChunkLookupTable will pre-allocate capacity
// for upfront, regardless of what numEntries (an attacker-controlled wire
// field) claims. Reading still proceeds via append past this cap for a
// genuinely large, honest table — this only prevents a tiny malicious
// shard from claiming numEntries near uint64's max and forcing a
// multi-gigabyte allocation before a single byte of actual entry data has
// been validated to exist.
const maxLookupEntryPreallocate = 4096

func ReadFileLookupTable(r io.Reader, numEntries uint64) ([]FileLookupEntry, error) {
	entries := make([]FileLookupEntry, 0, min(numEntries, maxLookupEntryPreallocate))
	for i := uint64(0); i < numEntries; i++ {
		key, err := readU64(r)
		if err != nil {
			return nil, err
		}
		idx, err := readU32(r)
		if err != nil {
			return nil, err
		}
		entries = append(entries, FileLookupEntry{Key: key, Index: idx})
	}
	return entries, nil
}

func WriteXorbLookupTable(w io.Writer, entries []XorbLookupEntry) error {
	for _, e := range entries {
		if err := writeU64(w, e.Key); err != nil {
			return err
		}
		if err := writeU32(w, e.Index); err != nil {
			return err
		}
	}
	return nil
}

func ReadXorbLookupTable(r io.Reader, numEntries uint64) ([]XorbLookupEntry, error) {
	entries := make([]XorbLookupEntry, 0, min(numEntries, maxLookupEntryPreallocate))
	for i := uint64(0); i < numEntries; i++ {
		key, err := readU64(r)
		if err != nil {
			return nil, err
		}
		idx, err := readU32(r)
		if err != nil {
			return nil, err
		}
		entries = append(entries, XorbLookupEntry{Key: key, Index: idx})
	}
	return entries, nil
}

func WriteChunkLookupTable(w io.Writer, entries []ChunkLookupEntry) error {
	for _, e := range entries {
		if err := writeU64(w, e.Key); err != nil {
			return err
		}
		if err := writeU32(w, e.XorbIndex); err != nil {
			return err
		}
		if err := writeU32(w, e.ChunkIndex); err != nil {
			return err
		}
	}
	return nil
}

func ReadChunkLookupTable(r io.Reader, numEntries uint64) ([]ChunkLookupEntry, error) {
	entries := make([]ChunkLookupEntry, 0, min(numEntries, maxLookupEntryPreallocate))
	for i := uint64(0); i < numEntries; i++ {
		key, err := readU64(r)
		if err != nil {
			return nil, err
		}
		xorbIdx, err := readU32(r)
		if err != nil {
			return nil, err
		}
		chunkIdx, err := readU32(r)
		if err != nil {
			return nil, err
		}
		entries = append(entries, ChunkLookupEntry{Key: key, XorbIndex: xorbIdx, ChunkIndex: chunkIdx})
	}
	return entries, nil
}

// SortChunkLookupEntries sorts entries by Key, matching xet-core's
// `chunk_lookup_combined.sort_unstable_by_key`. BTreeMap iteration in Rust
// already yields file/xorb lookup keys in sorted order (no separate sort
// step there), but chunk entries are collected across all xorbs first and
// explicitly sorted afterward.
func SortChunkLookupEntries(entries []ChunkLookupEntry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
}

// LookupByKey returns the byte-index values of all entries whose Key
// matches key. The lookup tables allow duplicate keys (truncated-hash
// collisions); callers should compare full hashes against the referenced
// content-section entry rather than trust a single match.
func LookupByKey[E interface{ GetKey() uint64 }](entries []E, key uint64) []E {
	lo := sort.Search(len(entries), func(i int) bool { return entries[i].GetKey() >= key })
	var out []E
	for i := lo; i < len(entries) && entries[i].GetKey() == key; i++ {
		out = append(out, entries[i])
	}
	return out
}

func (e FileLookupEntry) GetKey() uint64  { return e.Key }
func (e XorbLookupEntry) GetKey() uint64  { return e.Key }
func (e ChunkLookupEntry) GetKey() uint64 { return e.Key }
