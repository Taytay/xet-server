# `xet-server/internal/chunk`

```
package chunk // import "xet-server/internal/chunk"

Package chunk implements content-defined chunking (CDC) using a gear-hash
rolling hash, in the same spirit as HF Xet's chunker: chunk boundaries are
determined by file content rather than fixed offsets, so inserting or editing
bytes in the middle of a file only changes the chunks around the edit, not the
whole file.

CONSTANTS

const (
	DefaultMinSize = 4 * 1024
	DefaultAvgSize = 64 * 1024
	DefaultMaxSize = 256 * 1024
)

TYPES

type Chunk struct {
	Hash   string
	Offset int64
	Length int
	Data   []byte
}
    Chunk is one content-defined slice of a file.

type Chunker struct {
	Min, Avg, Max int
	// Has unexported fields.
}
    Chunker splits a byte stream into content-defined chunks bounded by [Min,
    Max], targeting an average size of Avg.

func NewChunker(min, avg, max int) *Chunker

func (c *Chunker) Split(r io.Reader, onChunk func(Chunk) error) error
    Split streams r and invokes onChunk for each chunk found, in file order.
    Memory use is bounded by Max, regardless of input size.
```
