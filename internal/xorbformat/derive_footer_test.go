package xorbformat

// Tests for DeriveFooter - independently reconstructing a xorb's V1
// footer and content hash from raw chunk bytes with no footer at all
// (the real upload wire format), shared by casserver (verifying a fresh
// upload) and proxycas (deriving a footer for a xorb it only ever saw as
// a downloaded/relayed byte stream).

import (
	"bytes"
	"testing"

	"github.com/guilt/xet-server/internal/merklehash"
)

func TestDeriveFooter_MatchesFooterWrittenByBuildTestXorb(t *testing.T) {
	payloads := [][]byte{
		[]byte("first chunk payload"),
		[]byte("second, a bit longer chunk payload here"),
		[]byte("third"),
	}
	blob := buildTestXorb(t, payloads)

	footer, hash, err := DeriveFooter(bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("DeriveFooter() error = %v", err)
	}
	if footer.NumChunks != uint32(len(payloads)) {
		t.Errorf("NumChunks = %d, want %d", footer.NumChunks, len(payloads))
	}
	if hash != footer.XorbHash {
		t.Errorf("returned hash %s != footer.XorbHash %s", hash.Hex(), footer.XorbHash.Hex())
	}
	for i, p := range payloads {
		want := merklehash.ComputeDataHash(p)
		if footer.ChunkHashes[i] != want {
			t.Errorf("ChunkHashes[%d] = %s, want %s", i, footer.ChunkHashes[i].Hex(), want.Hex())
		}
	}
}

func TestDeriveFooter_EmptyInputReturnsError(t *testing.T) {
	if _, _, err := DeriveFooter(bytes.NewReader(nil)); err == nil {
		t.Error("expected an error for empty input (no chunks found), got nil")
	}
}

func TestDeriveFooter_TruncatedChunkPayloadReturnsError(t *testing.T) {
	// A chunk header claiming more payload bytes than actually follow.
	var buf bytes.Buffer
	if err := WriteChunkHeader(&buf, ChunkHeader{CompressedLength: 100, CompressionScheme: CompressionNone, UncompressedLength: 100}); err != nil {
		t.Fatalf("WriteChunkHeader() error = %v", err)
	}
	buf.Write([]byte("short"))

	if _, _, err := DeriveFooter(bytes.NewReader(buf.Bytes())); err == nil {
		t.Error("expected an error for a truncated chunk payload, got nil")
	}
}
