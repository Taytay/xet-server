package lz4

import (
	"bytes"
	"os"
	"testing"
)

// TestDecompressBlock_LiteralsOnly covers the simplest valid block: a
// single sequence consisting only of literals (permitted as the final/only
// sequence in a block per the spec's parsing restrictions).
func TestDecompressBlock_LiteralsOnly(t *testing.T) {
	// Token: high nibble = literal length (5), low nibble = 0 (unused, no
	// match follows since this is the last/only sequence).
	src := []byte{0x50, 'h', 'e', 'l', 'l', 'o'}
	got, err := DecompressBlock(src, 5)
	if err != nil {
		t.Fatalf("DecompressBlock() error = %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("DecompressBlock() = %q, want %q", got, "hello")
	}
}

// TestDecompressBlock_WithMatch covers literals followed by a back-reference
// match: "ab" + copy 2 bytes from offset 2 back ("ab") = "abab".
func TestDecompressBlock_WithMatch(t *testing.T) {
	// Token: litLen=2 (high nibble), matchLen-4=0 (low nibble, i.e. matchLen=4...
	// but we only want matchLen=2, so use minimum matchLen=4 and adjust literals).
	// Sequence: literals "ab", offset=2, matchLen=4 (0+4) -> copies "abab" from
	// position 0, giving total output "ab" + "abab" = "ababab" (6 bytes).
	src := []byte{0x20, 'a', 'b', 0x02, 0x00}
	got, err := DecompressBlock(src, 6)
	if err != nil {
		t.Fatalf("DecompressBlock() error = %v", err)
	}
	if string(got) != "ababab" {
		t.Errorf("DecompressBlock() = %q, want %q", got, "ababab")
	}
}

func TestDecompressBlock_ExtendedLiteralLength(t *testing.T) {
	// litLen nibble = 15 (signals extension), extension byte = 10 -> total
	// literal length = 15 + 10 = 25.
	literal := bytes.Repeat([]byte{'x'}, 25)
	src := append([]byte{0xF0, 10}, literal...)
	got, err := DecompressBlock(src, 25)
	if err != nil {
		t.Fatalf("DecompressBlock() error = %v", err)
	}
	if !bytes.Equal(got, literal) {
		t.Errorf("DecompressBlock() = %q, want %q", got, literal)
	}
}

func TestDecompressBlock_RejectsZeroOffset(t *testing.T) {
	src := []byte{0x10, 'a', 0x00, 0x00} // litLen=1, offset=0 (invalid)
	if _, err := DecompressBlock(src, 5); err == nil {
		t.Error("DecompressBlock() error = nil for zero offset, want error")
	}
}

func TestDecompressBlock_RejectsOffsetBeyondDecodedData(t *testing.T) {
	src := []byte{0x10, 'a', 0x05, 0x00} // litLen=1, offset=5 but only 1 byte decoded so far
	if _, err := DecompressBlock(src, 5); err == nil {
		t.Error("DecompressBlock() error = nil for offset exceeding decoded length, want error")
	}
}

func TestDecompressFrame_RejectsBadMagic(t *testing.T) {
	garbage := []byte{0, 1, 2, 3, 4, 5, 6, 7}
	if _, err := DecompressFrame(garbage); err == nil {
		t.Error("DecompressFrame() error = nil for bad magic, want error")
	}
}

// TestDecompressFrame_RealHFXetCapture decompresses an actual LZ4 frame
// captured from a real hf_xet upload (produced by the Rust lz4_flex crate,
// which xet-core uses for its wire format), extracted from a live xetd
// session at internal/lz4/testdata/real_xorb_chunk.bin: an 8-byte xorb
// chunk header followed by the LZ4-compressed payload of highly repetitive
// text ("The quick brown fox jumps over the lazy dog. " repeated). This is
// the strongest test in this package - it proves the decoder works against
// genuine third-party-produced frames, not just self-authored fixtures.
func TestDecompressFrame_RealHFXetCapture(t *testing.T) {
	raw, err := os.ReadFile("testdata/real_xorb_chunk.bin")
	if err != nil {
		t.Fatalf("read testdata error = %v", err)
	}
	if len(raw) < 8 {
		t.Fatalf("testdata file too short: %d bytes", len(raw))
	}

	// Skip the 8-byte xorb chunk header (version, 3-byte compressed length,
	// compression scheme, 3-byte uncompressed length) to get to the LZ4
	// frame magic bytes.
	frame := raw[8:]

	decoded, err := DecompressFrame(frame)
	if err != nil {
		t.Fatalf("DecompressFrame() error = %v", err)
	}
	if len(decoded) == 0 {
		t.Fatal("DecompressFrame() returned no data")
	}

	const needle = "The quick brown fox jumps over the lazy dog. "
	if !bytes.Contains(decoded, []byte(needle)) {
		t.Errorf("decoded output does not contain expected text; first 80 bytes: %q", decoded[:min(80, len(decoded))])
	}
}
