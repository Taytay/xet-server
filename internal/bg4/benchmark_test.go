package bg4

// Benchmark for the ByteGrouping4 reverse transform, called on every
// chunk payload declaring CompressionByteGrouping4LZ4 during a xorb
// upload (after LZ4 decompression), so its speed adds directly to upload
// latency for that compression scheme.

import (
	"crypto/rand"
	"testing"
)

func benchmarkReverse(b *testing.B, size int) {
	data := make([]byte, size)
	rand.Read(data)
	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Reverse(data)
	}
}

func BenchmarkReverse_64KB(b *testing.B) { benchmarkReverse(b, 64*1024) }
func BenchmarkReverse_1MB(b *testing.B)  { benchmarkReverse(b, 1<<20) }
