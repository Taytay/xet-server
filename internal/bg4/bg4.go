// Package bg4 implements the "ByteGrouping4" transform xet-core applies
// before LZ4 compression for the ByteGrouping4LZ4 chunk compression
// scheme: bytes are regrouped by their position mod 4 (all byte-0's of
// each 4-byte group first, then all byte-1's, etc.), which tends to expose
// more redundancy to LZ4 in structured binary data (e.g. tensors of
// same-width floats) than compressing the interleaved bytes directly.
//
// Ported from the public reference implementation's algorithm and
// cross-checked against its own published test vector (github.com/
// jedisct1/zig-xet, src/compression.zig: applyByteGrouping /
// reverseByteGrouping), since xet-core's own Rust source was not read for
// this transform - only its existence and name (compression_scheme.rs) were
// confirmed there.
package bg4

// groupOffsets computes the four group boundaries for a length-n buffer,
// matching zig-xet's ByteGroupOffsets.init: group sizes are as equal as
// possible, with any remainder (n%4) distributed to the earliest groups.
func groupOffsets(n int) (g1, g2, g3 int) {
	split := n / 4
	rem := n % 4

	min1 := func(v int) int {
		if v > 1 {
			return 1
		}
		return v
	}
	g0Size := split + min1(rem)
	g1Size := split
	if rem >= 1 {
		g1Size = split + min1(rem-1)
	}
	g2Size := split
	if rem >= 2 {
		g2Size = split + min1(rem-2)
	}

	g1 = g0Size
	g2 = g0Size + g1Size
	g3 = g0Size + g1Size + g2Size
	return
}

// Apply reorders data into four byte-position groups (forward transform,
// applied by the client before LZ4 compression).
func Apply(data []byte) []byte {
	n := len(data)
	out := make([]byte, n)
	g1, g2, g3 := groupOffsets(n)
	split := n / 4
	rem := n % 4

	for i := 0; i < split; i++ {
		out[i] = data[4*i]
		out[g1+i] = data[4*i+1]
		out[g2+i] = data[4*i+2]
		out[g3+i] = data[4*i+3]
	}
	if rem >= 1 {
		out[split] = data[4*split]
	}
	if rem >= 2 {
		out[g1+split] = data[4*split+1]
	}
	if rem >= 3 {
		out[g2+split] = data[4*split+2]
	}
	return out
}

// Reverse undoes Apply: given byte-grouped data, reconstructs the original
// interleaved byte order. This is the direction a CAS server needs, since
// it only ever receives already-grouped-and-compressed chunk data from a
// real client and must decompress it back to verify chunk hashes.
func Reverse(data []byte) []byte {
	n := len(data)
	out := make([]byte, n)
	g1, g2, g3 := groupOffsets(n)
	split := n / 4
	rem := n % 4

	for i := 0; i < split; i++ {
		out[4*i] = data[i]
		out[4*i+1] = data[g1+i]
		out[4*i+2] = data[g2+i]
		out[4*i+3] = data[g3+i]
	}
	if rem >= 1 {
		out[4*split] = data[split]
	}
	if rem >= 2 {
		out[4*split+1] = data[g1+split]
	}
	if rem >= 3 {
		out[4*split+2] = data[g2+split]
	}
	return out
}
