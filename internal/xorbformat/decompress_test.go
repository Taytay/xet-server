package xorbformat

// Tests for DecompressChunkPayload — moved here from internal/casserver
// (originally an unexported decompressChunkPayload) since
// internal/proxycas needs the identical logic to derive a matching xorb
// footer for bytes it only ever saw as an opaque download/relay, not an
// upload it independently reconstructed chunk-by-chunk. Only
// CompressionNone is round-tripped end-to-end here — LZ4/BG4 have their
// own dedicated package-level tests (internal/lz4, internal/bg4) for the
// actual codec logic; this only needs to confirm DecompressChunkPayload
// dispatches to the right one and validates the resulting length.

import "testing"

func TestDecompressChunkPayload_NoneReturnsPayloadUnchanged(t *testing.T) {
	payload := []byte("uncompressed chunk content")
	got, err := DecompressChunkPayload(CompressionNone, payload, uint32(len(payload)))
	if err != nil {
		t.Fatalf("DecompressChunkPayload() error = %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("got = %q, want %q", got, payload)
	}
}

func TestDecompressChunkPayload_NoneRejectsLengthMismatch(t *testing.T) {
	payload := []byte("some content")
	_, err := DecompressChunkPayload(CompressionNone, payload, uint32(len(payload)+1))
	if err == nil {
		t.Error("expected an error for a length mismatch, got nil")
	}
}

func TestDecompressChunkPayload_UnsupportedSchemeReturnsError(t *testing.T) {
	_, err := DecompressChunkPayload(CompressionScheme(99), []byte("x"), 1)
	if err == nil {
		t.Error("expected an error for an unsupported compression scheme, got nil")
	}
}

func TestDecompressChunkPayload_LZ4RejectsMalformedFrame(t *testing.T) {
	_, err := DecompressChunkPayload(CompressionLZ4, []byte("not a valid lz4 frame"), 100)
	if err == nil {
		t.Error("expected an error for a malformed LZ4 frame, got nil")
	}
}

func TestDecompressChunkPayload_ByteGrouping4LZ4RejectsMalformedFrame(t *testing.T) {
	_, err := DecompressChunkPayload(CompressionByteGrouping4LZ4, []byte("not a valid lz4 frame"), 100)
	if err == nil {
		t.Error("expected an error for a malformed BG4+LZ4 frame, got nil")
	}
}
