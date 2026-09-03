package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestStore_PutAndGet(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	hash := "abcd1234"
	data := []byte("hello chunk")

	written, err := s.Put(hash, data)
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if !written {
		t.Error("Put() written = false, want true for first write")
	}

	got, err := s.Get(hash)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("Get() = %q, want %q", got, data)
	}
}

func TestStore_PutDeduplicatesExistingHash(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	hash := "deadbeef"
	first, err := s.Put(hash, []byte("original data"))
	if err != nil || !first {
		t.Fatalf("first Put() = (%v, %v), want (true, nil)", first, err)
	}

	// Second Put with the same hash but different bytes must be a no-op:
	// callers only ever call Put with data that hashes to `hash`, so this
	// models re-uploading a chunk that's already stored.
	second, err := s.Put(hash, []byte("different data, same key"))
	if err != nil {
		t.Fatalf("second Put() error = %v", err)
	}
	if second {
		t.Error("second Put() written = true, want false (already deduplicated)")
	}

	got, err := s.Get(hash)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if string(got) != "original data" {
		t.Fatalf("Get() = %q, want original data to be preserved after a duplicate Put", got)
	}
}

func TestStore_Has(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if s.Has("nonexistent") {
		t.Error("Has() = true for a chunk that was never stored")
	}

	if _, err := s.Put("cafef00d", []byte("data")); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if !s.Has("cafef00d") {
		t.Error("Has() = false for a chunk that was just stored")
	}
}

func TestStore_GetMissingChunk(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := s.Get("missing"); err == nil {
		t.Error("Get() error = nil, want error for missing chunk")
	}
}

func TestStore_ShardedLayout(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	hash := "0123456789abcdef"
	if _, err := s.Put(hash, []byte("x")); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	want := filepath.Join(root, "01", "23", hash)
	if _, err := os.Stat(want); err != nil {
		t.Errorf("expected chunk at sharded path %s, got error: %v", want, err)
	}
}

func TestStore_NoTempFileLeftBehindOnSuccess(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := s.Put("ff00ff00", []byte("payload")); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	matches, _ := filepath.Glob(filepath.Join(root, "*", "*", "*.tmp-*"))
	if len(matches) != 0 {
		t.Errorf("found leftover temp files after successful Put: %v", matches)
	}
}
