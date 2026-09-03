package chunk

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"math/rand"
	"testing"
)

func reassemble(chunks []Chunk) []byte {
	var buf bytes.Buffer
	for _, c := range chunks {
		buf.Write(c.Data)
	}
	return buf.Bytes()
}

func splitAll(t *testing.T, c *Chunker, data []byte) []Chunk {
	t.Helper()
	var chunks []Chunk
	err := c.Split(bytes.NewReader(data), func(ch Chunk) error {
		chunks = append(chunks, ch)
		return nil
	})
	if err != nil {
		t.Fatalf("Split() error = %v", err)
	}
	return chunks
}

func TestChunker_Reconstruction(t *testing.T) {
	c := NewChunker(DefaultMinSize, DefaultAvgSize, DefaultMaxSize)
	data := randomBytes(1_500_000, 1)

	chunks := splitAll(t, c, data)
	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk")
	}
	if got := reassemble(chunks); !bytes.Equal(got, data) {
		t.Fatalf("reassembled data does not match input: got %d bytes, want %d bytes", len(got), len(data))
	}
}

func TestChunker_EmptyInput(t *testing.T) {
	c := NewChunker(DefaultMinSize, DefaultAvgSize, DefaultMaxSize)
	chunks := splitAll(t, c, []byte{})
	if len(chunks) != 0 {
		t.Fatalf("expected no chunks for empty input, got %d", len(chunks))
	}
}

func TestChunker_SmallerThanMin(t *testing.T) {
	c := NewChunker(DefaultMinSize, DefaultAvgSize, DefaultMaxSize)
	data := randomBytes(100, 2)
	chunks := splitAll(t, c, data)
	if len(chunks) != 1 {
		t.Fatalf("expected exactly 1 chunk for input smaller than Min, got %d", len(chunks))
	}
	if !bytes.Equal(chunks[0].Data, data) {
		t.Fatal("single chunk does not match input data")
	}
}

func TestChunker_RespectsMaxSize(t *testing.T) {
	// Highly compressible/degenerate input (all zero bytes) is the case most
	// likely to blow past Avg without hitting a boundary; Max must still cap it.
	c := NewChunker(4*1024, 8*1024, 16*1024)
	data := make([]byte, 200_000)

	chunks := splitAll(t, c, data)
	for i, ch := range chunks {
		if ch.Length > c.Max {
			t.Fatalf("chunk %d length %d exceeds Max %d", i, ch.Length, c.Max)
		}
	}
	if got := reassemble(chunks); !bytes.Equal(got, data) {
		t.Fatal("reassembled data does not match input")
	}
}

func TestChunker_HashMatchesContent(t *testing.T) {
	c := NewChunker(DefaultMinSize, DefaultAvgSize, DefaultMaxSize)
	data := randomBytes(500_000, 3)
	chunks := splitAll(t, c, data)

	for i, ch := range chunks {
		sum := sha256.Sum256(ch.Data)
		want := hex.EncodeToString(sum[:])
		if ch.Hash != want {
			t.Errorf("chunk %d: Hash = %s, want %s (sha256 of Data)", i, ch.Hash, want)
		}
	}
}

func TestChunker_OffsetsAreContiguous(t *testing.T) {
	c := NewChunker(DefaultMinSize, DefaultAvgSize, DefaultMaxSize)
	data := randomBytes(500_000, 4)
	chunks := splitAll(t, c, data)

	var want int64
	for i, ch := range chunks {
		if ch.Offset != want {
			t.Fatalf("chunk %d: Offset = %d, want %d", i, ch.Offset, want)
		}
		want += int64(ch.Length)
	}
	if want != int64(len(data)) {
		t.Fatalf("total chunked length = %d, want %d", want, len(data))
	}
}

// TestChunker_LocalEditIsolation is the core CDC property that makes dedup
// useful: editing a small region in the middle of a file should only change
// the chunk(s) touching that region, not chunks far away from it.
func TestChunker_LocalEditIsolation(t *testing.T) {
	c := NewChunker(DefaultMinSize, DefaultAvgSize, DefaultMaxSize)
	original := randomBytes(2_000_000, 5)

	edited := make([]byte, len(original))
	copy(edited, original)
	mid := len(edited) / 2
	copy(edited[mid:mid+256], randomBytes(256, 6))

	chunksA := splitAll(t, c, original)
	chunksB := splitAll(t, c, edited)

	hashesA := map[string]bool{}
	for _, ch := range chunksA {
		hashesA[ch.Hash] = true
	}

	shared := 0
	for _, ch := range chunksB {
		if hashesA[ch.Hash] {
			shared++
		}
	}

	// With ~64KB average chunks over a 2MB file (~30 chunks) and a single
	// 256-byte edit, all but a couple of chunks should be untouched.
	unaffected := len(chunksB) - shared
	if unaffected > 4 {
		t.Errorf("local edit affected %d chunks out of %d total; want isolation to a handful of chunks near the edit", unaffected, len(chunksB))
	}
	if shared == 0 {
		t.Error("expected at least some chunks to be shared between original and edited content")
	}
}

func TestChunker_DeterministicAcrossRuns(t *testing.T) {
	c1 := NewChunker(DefaultMinSize, DefaultAvgSize, DefaultMaxSize)
	c2 := NewChunker(DefaultMinSize, DefaultAvgSize, DefaultMaxSize)
	data := randomBytes(800_000, 7)

	chunksA := splitAll(t, c1, data)
	chunksB := splitAll(t, c2, data)

	if len(chunksA) != len(chunksB) {
		t.Fatalf("chunk count differs across runs: %d vs %d", len(chunksA), len(chunksB))
	}
	for i := range chunksA {
		if chunksA[i].Hash != chunksB[i].Hash {
			t.Fatalf("chunk %d hash differs across runs: %s vs %s", i, chunksA[i].Hash, chunksB[i].Hash)
		}
	}
}

func randomBytes(n int, seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, n)
	r.Read(b)
	return b
}
