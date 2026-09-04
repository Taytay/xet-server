package merklehash

import "fmt"

// meanTreeBranchingFactor mirrors xet-core's
// AGGREGATED_HASHES_MEAN_TREE_BRANCHING_FACTOR.
const meanTreeBranchingFactor = 4

// hashSize is a (hash, size) pair: one chunk or one aggregated node plus
// its total byte length, mirroring xet-core's `(MerkleHash, u64)` pairs.
type hashSize struct {
	Hash Hash
	Size uint64
}

// isNaturalCut mirrors is_natural_cut: a hash triggers a Merkle-tree
// boundary when its low 64 bits are divisible by the branching factor.
func isNaturalCut(h Hash) bool {
	return h.Mod64(meanTreeBranchingFactor) == 0
}

// nextMergeCut finds the next cut point in hashes, mirroring
// next_merge_cut exactly: scan indices [2, min(2*BF+1, len)), cut right
// after the first natural-cut hash found, else cut at that upper bound.
func nextMergeCut(hashes []hashSize) int {
	if len(hashes) <= 2 {
		return len(hashes)
	}
	end := 2*meanTreeBranchingFactor + 1
	if end > len(hashes) {
		end = len(hashes)
	}
	for i := 2; i < end; i++ {
		if isNaturalCut(hashes[i].Hash) {
			return i + 1
		}
	}
	return end
}

// mergedHashOfSequence mirrors merged_hash_of_sequence: format each entry
// as "{hash_hex} : {size}\n", concatenate, and hash the result as an
// internal Merkle node. The exact text format matters — it's part of what
// makes this hash byte-compatible with xet-core.
func mergedHashOfSequence(group []hashSize) hashSize {
	var buf []byte
	var totalLen uint64
	for _, hs := range group {
		buf = append(buf, []byte(fmt.Sprintf("%s : %d\n", hs.Hash.Hex(), hs.Size))...)
		totalLen += hs.Size
	}
	return hashSize{Hash: ComputeInternalNodeHash(buf), Size: totalLen}
}

// aggregatedNodeHash mirrors aggregated_node_hash: iteratively collapse the
// list of (hash, size) pairs using nextMergeCut until only one remains.
func aggregatedNodeHash(chunks []hashSize) Hash {
	if len(chunks) == 0 {
		return Hash{}
	}

	hv := make([]hashSize, len(chunks))
	copy(hv, chunks)

	for len(hv) > 1 {
		writeIdx := 0
		readIdx := 0
		for readIdx != len(hv) {
			nextCut := readIdx + nextMergeCut(hv[readIdx:])
			hv[writeIdx] = mergedHashOfSequence(hv[readIdx:nextCut])
			writeIdx++
			readIdx = nextCut
		}
		hv = hv[:writeIdx]
	}
	return hv[0].Hash
}

// ChunkEntry is one chunk's hash and uncompressed byte length, the input
// unit for XorbHash/FileHash/FileHashWithSalt.
type ChunkEntry struct {
	Hash Hash
	Size uint64
}

func toHashSize(chunks []ChunkEntry) []hashSize {
	hv := make([]hashSize, len(chunks))
	for i, c := range chunks {
		hv[i] = hashSize{Hash: c.Hash, Size: c.Size}
	}
	return hv
}

// XorbHash computes a xorb's content hash from its ordered chunk list,
// mirroring xet-core's xorb_hash: the Merkle aggregation of chunk hashes
// with no salt.
func XorbHash(chunks []ChunkEntry) Hash {
	if len(chunks) == 0 {
		return Hash{}
	}
	return aggregatedNodeHash(toHashSize(chunks))
}

// FileHashWithSalt computes a file's content hash from its ordered chunk
// list, salted with an HMAC key, mirroring xet-core's file_hash_with_salt.
func FileHashWithSalt(chunks []ChunkEntry, salt Hash) Hash {
	if len(chunks) == 0 {
		return Hash{}
	}
	return aggregatedNodeHash(toHashSize(chunks)).HMAC(salt)
}

// FileHash computes a file's content hash with no salt (all-zero salt),
// mirroring xet-core's file_hash.
func FileHash(chunks []ChunkEntry) Hash {
	return FileHashWithSalt(chunks, Hash{})
}
