package manifest

import (
	"path/filepath"
	"testing"
)

func TestFileID_DeterministicForSameHash(t *testing.T) {
	hash := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	a := FileID(hash)
	b := FileID(hash)
	if a != b {
		t.Fatalf("FileID(%q) not deterministic: %q vs %q", hash, a, b)
	}
}

func TestFileID_DiffersForDifferentHashes(t *testing.T) {
	a := FileID("hash-one")
	b := FileID("hash-two")
	if a == b {
		t.Fatalf("FileID produced the same ID for different content hashes: %q", a)
	}
}

func TestManifest_SaveAndLoadRoundTrip(t *testing.T) {
	m := &Manifest{
		FileID: "abc123",
		Size:   42,
		SHA256: "deadbeef",
		Chunks: []ChunkRef{
			{Hash: "chunk1", Offset: 0, Length: 20},
			{Hash: "chunk2", Offset: 20, Length: 22},
		},
	}

	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := m.Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if loaded.FileID != m.FileID || loaded.Size != m.Size || loaded.SHA256 != m.SHA256 {
		t.Fatalf("Load() = %+v, want %+v", loaded, m)
	}
	if len(loaded.Chunks) != len(m.Chunks) {
		t.Fatalf("Load() chunk count = %d, want %d", len(loaded.Chunks), len(m.Chunks))
	}
	for i := range m.Chunks {
		if loaded.Chunks[i] != m.Chunks[i] {
			t.Errorf("chunk %d = %+v, want %+v", i, loaded.Chunks[i], m.Chunks[i])
		}
	}
}

func TestManifest_LoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err == nil {
		t.Error("Load() error = nil, want error for missing manifest file")
	}
}
