package xorbformat

import (
	"bytes"
	"testing"

	"github.com/guilt/xet-server/internal/merklehash"
)

// buildTestXorb constructs a minimal but structurally valid xorb blob: N
// chunks (uncompressed, arbitrary payload) followed by a matching V1
// footer, mirroring what a real client's serialize path produces. Returns
// the full blob plus the per-chunk uncompressed payloads for comparison.
func buildTestXorb(t *testing.T, payloads [][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer

	var chunkHashes []merklehash.Hash
	var boundaryOffsets []uint32
	var unpackedOffsets []uint32
	var physicalPos uint32
	var logicalPos uint32

	for _, p := range payloads {
		header := ChunkHeader{
			Version:            0,
			CompressedLength:   uint32(len(p)),
			CompressionScheme:  CompressionNone,
			UncompressedLength: uint32(len(p)),
		}
		if err := WriteChunkHeader(&buf, header); err != nil {
			t.Fatalf("WriteChunkHeader() error = %v", err)
		}
		buf.Write(p)

		physicalPos += ChunkHeaderSize + uint32(len(p))
		logicalPos += uint32(len(p))
		boundaryOffsets = append(boundaryOffsets, physicalPos)
		unpackedOffsets = append(unpackedOffsets, logicalPos)
		chunkHashes = append(chunkHashes, merklehash.ComputeDataHash(p))
	}

	var chunkEntries []merklehash.ChunkEntry
	for i, h := range chunkHashes {
		chunkEntries = append(chunkEntries, merklehash.ChunkEntry{Hash: h, Size: uint64(len(payloads[i]))})
	}

	footer := FooterV1{
		XorbHash:             merklehash.XorbHash(chunkEntries),
		ChunkHashes:          chunkHashes,
		ChunkBoundaryOffsets: boundaryOffsets,
		UnpackedChunkOffsets: unpackedOffsets,
		NumChunks:            uint32(len(payloads)),
	}
	if err := WriteFooterV1(&buf, footer); err != nil {
		t.Fatalf("WriteFooterV1() error = %v", err)
	}

	return buf.Bytes()
}

func TestScanChunks_FindsAllChunksBeforeFooter(t *testing.T) {
	payloads := [][]byte{
		[]byte("first chunk payload"),
		[]byte("second, a bit longer chunk payload here"),
		[]byte("third"),
	}
	blob := buildTestXorb(t, payloads)

	entries, err := ScanChunks(bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("ScanChunks() error = %v", err)
	}
	if len(entries) != len(payloads) {
		t.Fatalf("ScanChunks() found %d chunks, want %d", len(entries), len(payloads))
	}
	for i, e := range entries {
		if int(e.Header.UncompressedLength) != len(payloads[i]) {
			t.Errorf("chunk %d UncompressedLength = %d, want %d", i, e.Header.UncompressedLength, len(payloads[i]))
		}
		got := blob[e.DataOffset : e.DataOffset+int64(e.Header.CompressedLength)]
		if !bytes.Equal(got, payloads[i]) {
			t.Errorf("chunk %d payload = %q, want %q", i, got, payloads[i])
		}
	}
}

func TestParseFooterV1_RoundTrip(t *testing.T) {
	payloads := [][]byte{
		[]byte("alpha"),
		[]byte("beta-payload-content"),
	}
	blob := buildTestXorb(t, payloads)

	entries, err := ScanChunks(bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("ScanChunks() error = %v", err)
	}
	lastEntry := entries[len(entries)-1]
	footerStart := lastEntry.DataOffset + int64(lastEntry.Header.CompressedLength)

	footer, err := ParseFooterV1(bytes.NewReader(blob[footerStart:]))
	if err != nil {
		t.Fatalf("ParseFooterV1() error = %v", err)
	}

	if footer.NumChunks != uint32(len(payloads)) {
		t.Errorf("NumChunks = %d, want %d", footer.NumChunks, len(payloads))
	}
	for i, p := range payloads {
		want := merklehash.ComputeDataHash(p)
		if footer.ChunkHashes[i] != want {
			t.Errorf("ChunkHashes[%d] = %s, want %s", i, footer.ChunkHashes[i].Hex(), want.Hex())
		}
	}

	var chunkEntries []merklehash.ChunkEntry
	for i, h := range footer.ChunkHashes {
		chunkEntries = append(chunkEntries, merklehash.ChunkEntry{Hash: h, Size: uint64(len(payloads[i]))})
	}
	wantXorbHash := merklehash.XorbHash(chunkEntries)
	if footer.XorbHash != wantXorbHash {
		t.Errorf("XorbHash = %s, want %s", footer.XorbHash.Hex(), wantXorbHash.Hex())
	}
}

func TestParseFooterV1_RejectsBadIdent(t *testing.T) {
	bad := bytes.Repeat([]byte{0xFF}, 64)
	if _, err := ParseFooterV1(bytes.NewReader(bad)); err == nil {
		t.Error("ParseFooterV1() error = nil for garbage input, want error")
	}
}

func TestChunkHeader_RoundTrip(t *testing.T) {
	h := ChunkHeader{
		Version:            0,
		CompressedLength:   12345,
		CompressionScheme:  CompressionLZ4,
		UncompressedLength: 65536,
	}
	var buf bytes.Buffer
	if err := WriteChunkHeader(&buf, h); err != nil {
		t.Fatalf("WriteChunkHeader() error = %v", err)
	}
	if buf.Len() != ChunkHeaderSize {
		t.Fatalf("serialized header length = %d, want %d", buf.Len(), ChunkHeaderSize)
	}

	got, err := ReadChunkHeader(&buf)
	if err != nil {
		t.Fatalf("ReadChunkHeader() error = %v", err)
	}
	if got != h {
		t.Errorf("ReadChunkHeader() = %+v, want %+v", got, h)
	}
}

func TestChunkHeader_RejectsInvalidCompressionScheme(t *testing.T) {
	var buf [ChunkHeaderSize]byte
	buf[4] = 42 // invalid scheme
	if _, err := parseChunkHeader(buf); err == nil {
		t.Error("parseChunkHeader() error = nil for invalid compression scheme, want error")
	}
}
