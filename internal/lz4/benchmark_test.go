package lz4

// Benchmarks for LZ4 decompression throughput: DecompressFrame is called
// on every LZ4-compressed chunk's payload during a xorb upload (see
// casserver.decompressChunkPayload), so its speed directly bounds upload
// throughput for compressible content.

import (
	"encoding/binary"
	"os"
	"testing"
)

// buildUncompressedFrame constructs a minimal valid LZ4 frame containing
// size bytes of data as a single "uncompressed" block (the frame format's
// own escape hatch for incompressible data — bit 31 of the block-size
// field). This package has no encoder (decompression-only, per its
// package doc), so this is the only way to get larger-than-the-one-real-
// fixture LZ4 frame input for benchmarking without vendoring a real LZ4
// encoder just for test data.
func buildUncompressedFrame(data []byte) []byte {
	var buf []byte
	var tmp [4]byte

	binary.LittleEndian.PutUint32(tmp[:], frameMagic)
	buf = append(buf, tmp[:]...)

	buf = append(buf, 0x40) // FLG: version=01, no optional fields
	buf = append(buf, 0x70) // BD: block max size code 7 (4MB), informational only
	buf = append(buf, 0x00) // header checksum byte, unchecked by this decoder

	blockSizeField := uint32(len(data)) | (1 << 31) // uncompressed flag
	binary.LittleEndian.PutUint32(tmp[:], blockSizeField)
	buf = append(buf, tmp[:]...)
	buf = append(buf, data...)

	binary.LittleEndian.PutUint32(tmp[:], 0) // EndMark
	buf = append(buf, tmp[:]...)

	return buf
}

func benchmarkDecompressFrame(b *testing.B, frame []byte, decodedSize int) {
	b.SetBytes(int64(decodedSize))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := DecompressFrame(frame); err != nil {
			b.Fatalf("DecompressFrame() error = %v", err)
		}
	}
}

func BenchmarkDecompressFrame_RealCapture(b *testing.B) {
	raw, err := os.ReadFile("testdata/real_xorb_chunk.bin")
	if err != nil {
		b.Skipf("testdata/real_xorb_chunk.bin not available: %v", err)
	}
	if len(raw) < 8 {
		b.Fatalf("testdata file too short: %d bytes", len(raw))
	}
	frame := raw[8:] // skip the 8-byte xorb chunk header; see lz4_test.go's TestDecompressFrame_RealHFXetCapture
	decoded, err := DecompressFrame(frame)
	if err != nil {
		b.Fatalf("DecompressFrame() on real fixture error = %v", err)
	}
	benchmarkDecompressFrame(b, frame, len(decoded))
}

func benchmarkUncompressedFrame(b *testing.B, size int) {
	data := repeatedContent(size)
	frame := buildUncompressedFrame(data)
	benchmarkDecompressFrame(b, frame, size)
}

func BenchmarkDecompressFrame_Uncompressed_64KB(b *testing.B) { benchmarkUncompressedFrame(b, 64*1024) }
func BenchmarkDecompressFrame_Uncompressed_1MB(b *testing.B)  { benchmarkUncompressedFrame(b, 1<<20) }

func repeatedContent(size int) []byte {
	pattern := []byte("The quick brown fox jumps over the lazy dog. ")
	out := make([]byte, size)
	for i := 0; i < size; i += len(pattern) {
		copy(out[i:], pattern)
	}
	return out
}
