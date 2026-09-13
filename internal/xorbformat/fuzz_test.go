package xorbformat

// Fuzz targets for xorb wire-format parsing. ScanChunks is the hottest
// path here: it's called on every uploaded xorb's full body
// (casserver.handleUploadXorb) with completely attacker-controlled bytes
// before any hash verification happens. ReadChunkHeader and ParseFooterV1
// are lower-level pieces ScanChunks/tests build on.

import (
	"bytes"
	"testing"
)

func FuzzReadChunkHeader(f *testing.F) {
	var buf bytes.Buffer
	WriteChunkHeader(&buf, ChunkHeader{Version: 0, CompressedLength: 1234, CompressionScheme: CompressionLZ4, UncompressedLength: 5678})
	f.Add(buf.Bytes())
	f.Add([]byte{})
	f.Add(make([]byte, ChunkHeaderSize-1))
	f.Add(make([]byte, ChunkHeaderSize))
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}) // all-1s: max lengths, invalid scheme byte

	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("ReadChunkHeader panicked on %d bytes: %v", len(data), r)
			}
		}()
		_, _ = ReadChunkHeader(bytes.NewReader(data))
	})
}

func FuzzScanChunks(f *testing.F) {
	// A structurally valid xorb: two chunks + a matching footer (built the
	// same way the existing unit tests do), so the fuzzer starts from
	// something ScanChunks actually walks successfully before mutating.
	seedXorb := buildTestXorbForFuzz()
	f.Add(seedXorb)
	f.Add([]byte{})
	f.Add([]byte("XETBLOB")) // just the footer ident, no chunks at all
	f.Add(make([]byte, 4))   // shorter than one chunk header
	// A chunk header claiming a huge CompressedLength with no payload
	// bytes following - ScanChunks must fail on the resulting Seek/read,
	// not hang or allocate based on the claim (it only seeks, never
	// allocates CompressedLength bytes itself, but worth pinning as a
	// regression target since the whole point of this fuzz pass is
	// catching exactly this class of bug).
	var hugeClaim bytes.Buffer
	WriteChunkHeader(&hugeClaim, ChunkHeader{CompressedLength: 0xFFFFFF, CompressionScheme: CompressionNone, UncompressedLength: 0xFFFFFF})
	f.Add(hugeClaim.Bytes())

	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("ScanChunks panicked on %d bytes: %v", len(data), r)
			}
		}()
		_, _ = ScanChunks(bytes.NewReader(data))
	})
}

// buildTestXorbForFuzz builds a minimal valid xorb+footer blob without
// needing a *testing.T (f.Add seeds are registered outside any subtest),
// mirroring buildTestXorb in xorbformat_test.go but self-contained.
func buildTestXorbForFuzz() []byte {
	var buf bytes.Buffer
	payload := []byte("fuzz seed chunk content")
	header := ChunkHeader{CompressedLength: uint32(len(payload)), CompressionScheme: CompressionNone, UncompressedLength: uint32(len(payload))}
	if err := WriteChunkHeader(&buf, header); err != nil {
		return nil
	}
	buf.Write(payload)
	return buf.Bytes()
}

func FuzzParseFooterV1(f *testing.F) {
	var buf bytes.Buffer
	footer := FooterV1{
		ChunkHashes:          nil,
		ChunkBoundaryOffsets: nil,
		UnpackedChunkOffsets: nil,
		NumChunks:            0,
	}
	if err := WriteFooterV1(&buf, footer); err == nil {
		f.Add(buf.Bytes())
	}
	f.Add([]byte{})
	f.Add([]byte("XETBLOB"))
	f.Add([]byte("NOTAVALIDIDENT!"))
	// A footer claiming a huge NumChunks with no chunk hash bytes
	// following - the regression target for the allocation-size DoS fixed
	// in ParseFooterV1 (see maxFooterEntryPreallocate).
	var hugeCount bytes.Buffer
	hugeCount.WriteString("XETBLOB")
	hugeCount.WriteByte(1)            // footerVersionV1
	hugeCount.Write(make([]byte, 32)) // xorb hash placeholder
	hugeCount.WriteString("XBLBHSH")
	hugeCount.WriteByte(0)                          // hashSectionVersion
	hugeCount.Write([]byte{0xFF, 0xFF, 0xFF, 0xFF}) // numChunks = max uint32, little-endian
	f.Add(hugeCount.Bytes())

	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("ParseFooterV1 panicked on %d bytes: %v", len(data), r)
			}
		}()
		_, _ = ParseFooterV1(bytes.NewReader(data))
	})
}
