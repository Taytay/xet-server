# `github.com/guilt/xet-server/internal/manifest`

```
package manifest // import "github.com/guilt/xet-server/internal/manifest"

Package manifest describes how to reconstruct a file from an ordered list of
content-defined chunks, analogous to Xet's file reconstruction metadata.

FUNCTIONS

func FileID(fullSHA256 string) string
    FileID derives a stable content-based ID for a manifest: the hash of its
    full-file digest. Two uploads of identical file content get the same ID.


TYPES

type ChunkRef struct {
	Hash   string `json:"hash"`
	Offset int64  `json:"offset"`
	Length int    `json:"length"`
}
    ChunkRef locates one chunk within a reconstructed file.

type Manifest struct {
	FileID string     `json:"file_id"`
	Size   int64      `json:"size"`
	SHA256 string     `json:"sha256"`
	Chunks []ChunkRef `json:"chunks"`
}
    Manifest is everything needed to reconstruct one file's bytes from chunks
    held in the store, plus enough metadata to verify the result.

func Load(path string) (*Manifest, error)

func (m *Manifest) Save(path string) error
```
