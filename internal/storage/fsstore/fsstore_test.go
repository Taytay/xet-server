package fsstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestStore_PutAndGet(t *testing.T) {
	ctx := context.Background()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	key := "abcd1234"
	data := []byte("hello chunk")

	written, err := s.Put(ctx, key, data)
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if !written {
		t.Error("Put() written = false, want true for first write")
	}

	got, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("Get() = %q, want %q", got, data)
	}
}

func TestStore_PutDeduplicatesExistingKey(t *testing.T) {
	ctx := context.Background()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	key := "deadbeef"
	first, err := s.Put(ctx, key, []byte("original data"))
	if err != nil || !first {
		t.Fatalf("first Put() = (%v, %v), want (true, nil)", first, err)
	}

	second, err := s.Put(ctx, key, []byte("different data, same key"))
	if err != nil {
		t.Fatalf("second Put() error = %v", err)
	}
	if second {
		t.Error("second Put() written = true, want false (already deduplicated)")
	}

	got, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if string(got) != "original data" {
		t.Fatalf("Get() = %q, want original data to be preserved after a duplicate Put", got)
	}
}

func TestStore_Has(t *testing.T) {
	ctx := context.Background()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	has, err := s.Has(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("Has() error = %v", err)
	}
	if has {
		t.Error("Has() = true for a key that was never stored")
	}

	if _, err := s.Put(ctx, "cafef00d", []byte("data")); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	has, err = s.Has(ctx, "cafef00d")
	if err != nil {
		t.Fatalf("Has() error = %v", err)
	}
	if !has {
		t.Error("Has() = false for a key that was just stored")
	}
}

func TestStore_GetMissingKey(t *testing.T) {
	ctx := context.Background()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := s.Get(ctx, "missing"); err == nil {
		t.Error("Get() error = nil, want error for missing key")
	}
}

func TestStore_GetRange(t *testing.T) {
	ctx := context.Background()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	key := "range-test"
	data := []byte("0123456789abcdef")
	if _, err := s.Put(ctx, key, data); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	got, err := s.GetRange(ctx, key, 3, 5)
	if err != nil {
		t.Fatalf("GetRange() error = %v", err)
	}
	want := data[3:8]
	if string(got) != string(want) {
		t.Fatalf("GetRange(3, 5) = %q, want %q", got, want)
	}

	// Range extending to (but not past) EOF should return the tail, not error.
	got, err = s.GetRange(ctx, key, 12, 4)
	if err != nil {
		t.Fatalf("GetRange() at EOF boundary error = %v", err)
	}
	if string(got) != string(data[12:16]) {
		t.Fatalf("GetRange(12, 4) = %q, want %q", got, data[12:16])
	}
}

func TestStore_ShardedLayout(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	key := "0123456789abcdef"
	if _, err := s.Put(ctx, key, []byte("x")); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	want := filepath.Join(root, "01", "23", key)
	if _, err := os.Stat(want); err != nil {
		t.Errorf("expected blob at sharded path %s, got error: %v", want, err)
	}
}

func TestStore_NoTempFileLeftBehindOnSuccess(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := s.Put(ctx, "ff00ff00", []byte("payload")); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	matches, _ := filepath.Glob(filepath.Join(root, "*", "*", "*.tmp-*"))
	if len(matches) != 0 {
		t.Errorf("found leftover temp files after successful Put: %v", matches)
	}
}
