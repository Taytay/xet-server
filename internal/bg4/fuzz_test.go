package bg4

// Fuzz target for the ByteGrouping4 reverse transform: reachable with
// fully attacker-controlled bytes via casserver.decompressChunkPayload for
// any chunk claiming CompressionByteGrouping4LZ4 (after LZ4 decoding, so
// the input here is whatever bytes an attacker can get LZ4 to decode to).

import "testing"

func FuzzReverse(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x01})
	f.Add([]byte{0x01, 0x02, 0x03})
	f.Add([]byte{0x01, 0x02, 0x03, 0x04})
	f.Add(make([]byte, 1000))

	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Reverse panicked on input of length %d: %v", len(data), r)
			}
		}()
		out := Reverse(data)
		if len(out) != len(data) {
			t.Fatalf("Reverse(%d bytes) returned %d bytes, want same length", len(data), len(out))
		}
		// Apply(Reverse(x)) must reproduce x exactly - Reverse's own doc
		// comment states it's the exact inverse of Apply.
		roundTripped := Apply(out)
		for i := range data {
			if roundTripped[i] != data[i] {
				t.Fatalf("Apply(Reverse(data)) mismatch at byte %d: got %d, want %d (len=%d)", i, roundTripped[i], data[i], len(data))
			}
		}
	})
}
