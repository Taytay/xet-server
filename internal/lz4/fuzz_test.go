package lz4

// Fuzz targets for the from-scratch LZ4 decoder: both are reachable with
// fully attacker-controlled bytes via casserver.decompressChunkPayload,
// since a xorb upload's chunk payload is decompressed to verify its
// claimed hash before this server trusts anything about it.

import (
	"os"
	"testing"
	"time"
)

func FuzzDecompressFrame(f *testing.F) {
	if raw, err := os.ReadFile("testdata/real_xorb_chunk.bin"); err == nil {
		f.Add(raw) // a real captured LZ4 frame from a live hf_xet upload
	}
	f.Add([]byte{})
	f.Add([]byte{0x04, 0x22, 0x4D, 0x18})                   // valid magic, nothing after
	f.Add([]byte{0x04, 0x22, 0x4D, 0x18, 0x40, 0x70, 0x00}) // magic + minimal-ish header, truncated
	// Malformed magic (one byte off a valid frame).
	f.Add([]byte{0x04, 0x22, 0x4D, 0x19, 0x40, 0x70, 0x73})
	// The exact shape of a real, confirmed decompression-amplification
	// DoS (see dos_test.go's TestDecompressFrame_RejectsAmplificationBomb):
	// a match-length extension inflating one block far past its frame
	// descriptor's declared max block size. Kept small here (fuzzing runs
	// this seed on every invocation, unlike the dedicated regression test)
	// - still exercises the same code path the full-size bomb does.
	f.Add(buildAmplificationBombFrame(buildAmplificationBombBlock(1000)))

	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("DecompressFrame panicked on input of length %d: %v", len(data), r)
			}
		}()
		// Any error return is fine - this is untrusted input by design.
		// The failure modes under test are a panic, or taking
		// disproportionately long relative to input size (the
		// amplification-DoS shape fixed in frame.go: decompression must be
		// bounded by the frame's own declared max block size, not merely
		// eventually terminate).
		start := time.Now()
		_, _ = DecompressFrame(data)
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("DecompressFrame took %v on a %d-byte input - possible unbounded decompression amplification", elapsed, len(data))
		}
	})
}

func FuzzDecompressBlock(f *testing.F) {
	f.Add([]byte{}, 0)
	f.Add([]byte{0x00}, 10)                                    // a single zero token byte, dstLen mismatched
	f.Add([]byte{0xF0, 0xFF}, 300)                             // literal-length-15 extension, truncated
	f.Add([]byte{0x40, 0x01, 0x02, 0x03, 0x04, 0x00, 0x00}, 4) // a match with offset 0 pattern nearby

	f.Fuzz(func(t *testing.T, data []byte, dstLenSeed int) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("DecompressBlock panicked on input of length %d, dstLen seed %d: %v", len(data), dstLenSeed, r)
			}
		}()
		// Clamp dstLen to a sane range: an unclamped huge dstLen would
		// itself allocate a giant buffer regardless of the decoder's own
		// logic, which is a property of the API contract (caller must
		// know the real decoded size), not a decoder bug - DecompressFrame
		// above is the fuzz target for the "attacker controls the claimed
		// size" case via decompressBlockUnknownSize's own regrowth logic.
		dstLen := dstLenSeed % (1 << 20)
		if dstLen < 0 {
			dstLen = -dstLen
		}
		_, _ = DecompressBlock(data, dstLen)
	})
}
