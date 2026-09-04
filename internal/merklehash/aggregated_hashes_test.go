package merklehash

import "testing"

// TestXorbHash_ReferenceVectors and TestFileHash_ReferenceVectors check this
// package's port against reference vectors published verbatim in xet-core's
// own test suite (xet_core_structures/src/merklehash/aggregated_hashes.rs,
// test_correctness), which the xet-core authors state are "intended to be
// used as a reference to ensure that other implementations or ports of
// these functions produce the correct hashes." Any mismatch here means this
// port would silently disagree with a real xet-core/hf_xet client.
func TestXorbHash_ReferenceVectors(t *testing.T) {
	tests := []struct {
		name   string
		chunks []ChunkEntry
		want   string
	}{
		{
			name:   "empty",
			chunks: nil,
			want:   "0000000000000000000000000000000000000000000000000000000000000000",
		},
		{
			name:   "single zero hash",
			chunks: []ChunkEntry{{Hash: mustHex(t, "0000000000000000000000000000000000000000000000000000000000000000"), Size: 0}},
			want:   "0000000000000000000000000000000000000000000000000000000000000000",
		},
		{
			name:   "single non-zero hash",
			chunks: []ChunkEntry{{Hash: mustHex(t, "cfc5d07f6f03c29bbf424132963fe08d19a37d5757aaf520bf08119f05cd56d6"), Size: 100}},
			want:   "cfc5d07f6f03c29bbf424132963fe08d19a37d5757aaf520bf08119f05cd56d6",
		},
		{
			name: "three distinct chunks",
			chunks: []ChunkEntry{
				{Hash: mustHex(t, "cfc5d07f6f03c29bbf424132963fe08d19a37d5757aaf520bf08119f05cd56d6"), Size: 100},
				{Hash: mustHex(t, "c3e67584b5c4fc2a89837ec39e40f2c8a6bb0b2987ac94cd4b31e5fbdd210a72"), Size: 200},
				{Hash: mustHex(t, "0d2beb91b9196929a5ddec9f6e306924ddf4a24268e3e59fd8464738d525af37"), Size: 300},
			},
			want: "71ec1275fca074724e2dd666921b3277c7cee603e4d025bcab2d4050015be2bc",
		},
		{
			name: "four identical chunks",
			chunks: []ChunkEntry{
				{Hash: mustHex(t, "cfc5d07f6f03c29bbf424132963fe08d19a37d5757aaf520bf08119f05cd56d6"), Size: 100},
				{Hash: mustHex(t, "cfc5d07f6f03c29bbf424132963fe08d19a37d5757aaf520bf08119f05cd56d6"), Size: 100},
				{Hash: mustHex(t, "cfc5d07f6f03c29bbf424132963fe08d19a37d5757aaf520bf08119f05cd56d6"), Size: 100},
				{Hash: mustHex(t, "cfc5d07f6f03c29bbf424132963fe08d19a37d5757aaf520bf08119f05cd56d6"), Size: 100},
			},
			want: "89f2ada89ff8c96763c6b25010e6dd76a4c05b1466207633ea559acf2093211b",
		},
		{
			name: "eight chunks (two distinct hashes, four zeros interleaved)",
			chunks: []ChunkEntry{
				{Hash: mustHex(t, "cfc5d07f6f03c29bbf424132963fe08d19a37d5757aaf520bf08119f05cd56d6"), Size: 100},
				{Hash: mustHex(t, "c3e67584b5c4fc2a89837ec39e40f2c8a6bb0b2987ac94cd4b31e5fbdd210a72"), Size: 200},
				{Hash: mustHex(t, "cfc5d07f6f03c29bbf424132963fe08d19a37d5757aaf520bf08119f05cd56d6"), Size: 100},
				{Hash: mustHex(t, "c3e67584b5c4fc2a89837ec39e40f2c8a6bb0b2987ac94cd4b31e5fbdd210a72"), Size: 200},
				{Hash: mustHex(t, "0d2beb91b9196929a5ddec9f6e306924ddf4a24268e3e59fd8464738d525af37"), Size: 300},
				{Hash: mustHex(t, "adf8773496a9b7319b2e50dc98093f344053b17d8ad37100b9c07d9805988784"), Size: 400},
			},
			want: "52c826f99507aa05d0b45e9837fa1709e0485425cfbcb1e80db3905cf98b3ee9",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := XorbHash(tt.chunks).Hex()
			if got != tt.want {
				t.Errorf("XorbHash() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestFileHash_ReferenceVectors(t *testing.T) {
	chunks := []ChunkEntry{
		{Hash: mustHex(t, "cfc5d07f6f03c29bbf424132963fe08d19a37d5757aaf520bf08119f05cd56d6"), Size: 100},
		{Hash: mustHex(t, "c3e67584b5c4fc2a89837ec39e40f2c8a6bb0b2987ac94cd4b31e5fbdd210a72"), Size: 200},
		{Hash: mustHex(t, "0d2beb91b9196929a5ddec9f6e306924ddf4a24268e3e59fd8464738d525af37"), Size: 300},
	}

	tests := []struct {
		salt string
		want string
	}{
		{
			salt: "0000000000000000000000000000000000000000000000000000000000000000",
			want: "54e55dccc6653c612bdb5576c5d3cb34bb31bc4e100248abccf4c908b3ef7715",
		},
		{
			salt: "b286606709ef32a1cfede1e603a39f83fef56772d8234c0e35d8920071d80b69",
			want: "337f1f046e4cfc1057f0f3edbd87cf977fdfaf3c2b0c0031ca2d1d2a34aa2270",
		},
		{
			salt: "20f7357cdb2f6d3727375656e4eaf464fa195094ff501edc08aaccfb1419a07b",
			want: "82c5359ad947e91e4da6a00034f94a9159bd3bf24d005ef167648b9d3ba77f8c",
		},
	}

	for _, tt := range tests {
		t.Run(tt.salt, func(t *testing.T) {
			got := FileHashWithSalt(chunks, mustHex(t, tt.salt)).Hex()
			if got != tt.want {
				t.Errorf("FileHashWithSalt(salt=%s) = %s, want %s", tt.salt, got, tt.want)
			}
		})
	}
}

func mustHex(t *testing.T, s string) Hash {
	t.Helper()
	h, err := FromHex(s)
	if err != nil {
		t.Fatalf("FromHex(%q) error = %v", s, err)
	}
	return h
}
