// Package chunk implements content-defined chunking (CDC) using a gear-hash
// rolling hash, in the same spirit as HF Xet's chunker: chunk boundaries are
// determined by file content rather than fixed offsets, so inserting or
// editing bytes in the middle of a file only changes the chunks around the
// edit, not the whole file.
package chunk

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"io"
)

const (
	DefaultMinSize = 4 * 1024
	DefaultAvgSize = 64 * 1024
	DefaultMaxSize = 256 * 1024
)

// Chunk is one content-defined slice of a file.
type Chunk struct {
	Hash   string
	Offset int64
	Length int
	Data   []byte
}

// Chunker splits a byte stream into content-defined chunks bounded by
// [Min, Max], targeting an average size of Avg.
type Chunker struct {
	Min, Avg, Max int
	mask          uint64
}

func NewChunker(min, avg, max int) *Chunker {
	return &Chunker{Min: min, Avg: avg, Max: max, mask: maskFor(avg)}
}

// maskFor picks a bitmask so that, for content with a uniform gear-hash
// distribution, a boundary triggers on average every `avg` bytes.
func maskFor(avg int) uint64 {
	bits := 0
	for v := avg; v > 1; v >>= 1 {
		bits++
	}
	return (uint64(1) << uint(bits)) - 1
}

var gearTable = buildGearTable()

// buildGearTable deterministically fills the 256-entry gear table using
// splitmix64, so every chunker instance agrees on chunk boundaries.
func buildGearTable() [256]uint64 {
	var t [256]uint64
	seed := uint64(0x9E3779B97F4A7C15)
	for i := range t {
		seed += 0x9E3779B97F4A7C15
		z := seed
		z ^= z >> 30
		z *= 0xBF58476D1CE4E5B9
		z ^= z >> 27
		z *= 0x94D049BB133111EB
		z ^= z >> 31
		t[i] = z
	}
	return t
}

// Split streams r and invokes onChunk for each chunk found, in file order.
// Memory use is bounded by Max, regardless of input size.
func (c *Chunker) Split(r io.Reader, onChunk func(Chunk) error) error {
	br := bufio.NewReaderSize(r, 1<<20)
	buf := make([]byte, 0, c.Max)
	var hash uint64
	var offset int64

	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		sum := sha256.Sum256(buf)
		ch := Chunk{
			Hash:   hex.EncodeToString(sum[:]),
			Offset: offset,
			Length: len(buf),
			Data:   append([]byte(nil), buf...),
		}
		offset += int64(len(buf))
		buf = buf[:0]
		hash = 0
		return onChunk(ch)
	}

	for {
		b, err := br.ReadByte()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		buf = append(buf, b)
		hash = (hash << 1) + gearTable[b]
		size := len(buf)

		if size >= c.Min && hash&c.mask == 0 {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if size >= c.Max {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}
