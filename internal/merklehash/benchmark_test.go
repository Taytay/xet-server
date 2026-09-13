package merklehash

// Benchmarks for hashing throughput: ComputeDataHash (per-chunk leaf
// hash, called once per chunk on every upload) and XorbHash (Merkle
// aggregation across a xorb's chunk hashes, called once per xorb upload).

import (
	"crypto/rand"
	"testing"
)

func benchmarkComputeDataHash(b *testing.B, size int) {
	data := make([]byte, size)
	rand.Read(data)
	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ComputeDataHash(data)
	}
}

func BenchmarkComputeDataHash_4KB(b *testing.B)  { benchmarkComputeDataHash(b, 4*1024) }
func BenchmarkComputeDataHash_64KB(b *testing.B) { benchmarkComputeDataHash(b, 64*1024) }
func BenchmarkComputeDataHash_1MB(b *testing.B)  { benchmarkComputeDataHash(b, 1<<20) }

// benchmarkXorbHash measures Merkle-aggregation throughput across
// numChunks chunk hashes, roughly the number of 64KB-average chunks in a
// single xorb (real xet-core targets ~64MB xorbs - see
// casserver.maxXorbBytes's doc comment - so ~1000 chunks/xorb at the
// default average chunk size is a realistic upper bound).
func benchmarkXorbHash(b *testing.B, numChunks int) {
	chunks := make([]ChunkEntry, numChunks)
	for i := range chunks {
		var seed [32]byte
		rand.Read(seed[:])
		h, _ := FromRawBytes(seed[:])
		chunks[i] = ChunkEntry{Hash: h, Size: 65536}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		XorbHash(chunks)
	}
}

func BenchmarkXorbHash_10Chunks(b *testing.B)   { benchmarkXorbHash(b, 10) }
func BenchmarkXorbHash_100Chunks(b *testing.B)  { benchmarkXorbHash(b, 100) }
func BenchmarkXorbHash_1000Chunks(b *testing.B) { benchmarkXorbHash(b, 1000) }
