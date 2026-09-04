package merklehash

import "testing"

// TestHex_ByteOrderReferenceVector reproduces xet-core's own
// test_hash_hex_string_endianness (merklehash/data_hash.rs): a fixed 32-byte
// array and its expected hex string, chosen specifically because it is NOT
// symmetric under a naive whole-buffer hex encode — it only matches once
// each 8-byte group is reversed into little-endian u64 words first. This is
// the exact bug this package's Hex()/FromHex() byte-order handling exists
// to get right.
func TestHex_ByteOrderReferenceVector(t *testing.T) {
	raw := [32]byte{
		22, 175, 58, 132, 4, 75, 131, 214, 190, 153, 138, 66, 226, 3, 153, 242,
		204, 86, 80, 234, 249, 153, 80, 99, 159, 80, 65, 138, 236, 231, 149, 78,
	}
	const want = "d6834b04843aaf16f29903e2428a99be635099f9ea5056cc4e95e7ec8a41509f"

	h, err := FromRawBytes(raw[:])
	if err != nil {
		t.Fatalf("FromRawBytes() error = %v", err)
	}
	if got := h.Hex(); got != want {
		t.Errorf("Hex() = %s, want %s", got, want)
	}

	roundTripped, err := FromHex(want)
	if err != nil {
		t.Fatalf("FromHex() error = %v", err)
	}
	if roundTripped != h {
		t.Errorf("FromHex(Hex(h)) != h: got %v, want %v", roundTripped, h)
	}
}

func TestFromHex_RejectsWrongLength(t *testing.T) {
	if _, err := FromHex("abcdef"); err == nil {
		t.Error("FromHex() error = nil for a too-short string, want error")
	}
}

func TestFromRawBytes_RejectsWrongLength(t *testing.T) {
	if _, err := FromRawBytes([]byte{1, 2, 3}); err == nil {
		t.Error("FromRawBytes() error = nil for a too-short slice, want error")
	}
}

func TestBytes_RoundTripsWithFromRawBytes(t *testing.T) {
	h := ComputeDataHash([]byte("some chunk content"))
	got, err := FromRawBytes(h.Bytes())
	if err != nil {
		t.Fatalf("FromRawBytes() error = %v", err)
	}
	if got != h {
		t.Errorf("FromRawBytes(h.Bytes()) != h")
	}
}
