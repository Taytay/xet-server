package chunk

// Benchmarks for content-defined chunking throughput at realistic file
// sizes, over both highly-compressible (repeated pattern) and effectively
// incompressible (random) content, since the gear-hash rolling hash's
// performance shouldn't depend on data shape but is worth confirming.

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func benchRepeatedPattern(size int) []byte {
	pattern := []byte("The quick brown fox jumps over the lazy dog. ")
	out := make([]byte, size)
	for i := 0; i < size; i += len(pattern) {
		n := copy(out[i:], pattern)
		_ = n
	}
	return out
}

func benchRandomBytes(size int) []byte {
	b := make([]byte, size)
	rand.Read(b)
	return b
}

func benchmarkSplit(b *testing.B, data []byte) {
	c := NewChunker(DefaultMinSize, DefaultAvgSize, DefaultMaxSize)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.Split(bytes.NewReader(data), func(Chunk) error { return nil }); err != nil {
			b.Fatalf("Split() error = %v", err)
		}
	}
}

func BenchmarkSplit_1MB_Repeated(b *testing.B)  { benchmarkSplit(b, benchRepeatedPattern(1<<20)) }
func BenchmarkSplit_16MB_Repeated(b *testing.B) { benchmarkSplit(b, benchRepeatedPattern(16<<20)) }
func BenchmarkSplit_1MB_Random(b *testing.B)    { benchmarkSplit(b, benchRandomBytes(1<<20)) }
func BenchmarkSplit_16MB_Random(b *testing.B)   { benchmarkSplit(b, benchRandomBytes(16<<20)) }
