// Package manifest describes how to reconstruct a file from an ordered list
// of content-defined chunks, analogous to Xet's file reconstruction metadata.
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
)

// ChunkRef locates one chunk within a reconstructed file.
type ChunkRef struct {
	Hash   string `json:"hash"`
	Offset int64  `json:"offset"`
	Length int    `json:"length"`
}

// Manifest is everything needed to reconstruct one file's bytes from chunks
// held in the store, plus enough metadata to verify the result.
type Manifest struct {
	FileID string     `json:"file_id"`
	Size   int64      `json:"size"`
	SHA256 string     `json:"sha256"`
	Chunks []ChunkRef `json:"chunks"`
}

// FileID derives a stable content-based ID for a manifest: the hash of its
// full-file digest. Two uploads of identical file content get the same ID.
func FileID(fullSHA256 string) string {
	sum := sha256.Sum256([]byte(fullSHA256))
	return hex.EncodeToString(sum[:])[:32]
}

func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *Manifest) Save(path string) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
