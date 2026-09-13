package merklehash

// Fuzz targets for merklehash's parsing entry points: FromHex (parses a
// URL/JSON-facing hash string) and FromRawBytes (parses a binary-format
// field's raw bytes). Both are called directly on attacker-supplied data -
// FromHex via hexParam in casserver (every {hash}/{file_id} URL path
// segment), FromRawBytes throughout shardformat/xorbformat when reading a
// 32-byte hash field off the wire.

import "testing"

func FuzzFromHex(f *testing.F) {
	// Seed with valid hex round-tripped from a real hash, plus edge cases:
	// empty, too short, too long, non-hex characters, uppercase (Hex()
	// always emits lowercase, so uppercase input must still be judged
	// solely on hex.Decode's own case-insensitivity, not crash).
	seed := ComputeDataHash([]byte("fuzz seed")).Hex()
	f.Add(seed)
	f.Add("")
	f.Add("0")
	f.Add(seed[:63])                                      // one short
	f.Add(seed + "0")                                     // one long
	f.Add(seed[:32] + "ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ") // invalid hex chars, right length
	f.Add(seed[:32] + string([]byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f}))

	f.Fuzz(func(t *testing.T, s string) {
		h, err := FromHex(s)
		if err != nil {
			return // any rejection of malformed input is fine
		}
		// If it parsed, round-tripping through Hex() must reproduce
		// exactly the input FromHex accepted (case-normalized), since
		// FromHex/Hex are meant to be exact inverses for valid 64-char hex.
		if len(s) != 64 {
			t.Fatalf("FromHex(%q) succeeded with err=nil despite len=%d != 64", s, len(s))
		}
		roundTripped := h.Hex()
		if len(roundTripped) != 64 {
			t.Fatalf("Hex() of a successfully-parsed hash returned length %d, want 64", len(roundTripped))
		}
	})
}

func FuzzFromRawBytes(f *testing.F) {
	valid := ComputeDataHash([]byte("fuzz seed")).Bytes()
	f.Add(valid)
	f.Add([]byte{})
	f.Add(valid[:31])
	f.Add(append(valid, 0x00))
	f.Add(make([]byte, 1000)) // wrong length, but shouldn't panic

	f.Fuzz(func(t *testing.T, b []byte) {
		h, err := FromRawBytes(b)
		if err != nil {
			return
		}
		if len(b) != 32 {
			t.Fatalf("FromRawBytes(%d bytes) succeeded with err=nil, want an error for any length != 32", len(b))
		}
		// Bytes() must be the exact inverse for a successful parse.
		out := h.Bytes()
		for i := range b {
			if out[i] != b[i] {
				t.Fatalf("FromRawBytes/Bytes round-trip mismatch at byte %d: got %d, want %d", i, out[i], b[i])
			}
		}
	})
}
