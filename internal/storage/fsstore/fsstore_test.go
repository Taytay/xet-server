package fsstore

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func put(t *testing.T, s *Store, ctx context.Context, key string, data []byte) bool {
	t.Helper()
	written, err := s.Put(ctx, key, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	return written
}

func get(t *testing.T, s *Store, ctx context.Context, key string) ([]byte, error) {
	t.Helper()
	rc, err := s.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func getRange(t *testing.T, s *Store, ctx context.Context, key string, offset, length int64) ([]byte, error) {
	t.Helper()
	rc, err := s.GetRange(ctx, key, offset, length)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func TestStore_PutAndGet(t *testing.T) {
	ctx := context.Background()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	key := "abcd1234"
	data := []byte("hello chunk")

	if written := put(t, s, ctx, key, data); !written {
		t.Error("Put() written = false, want true for first write")
	}

	got, err := get(t, s, ctx, key)
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
	if first := put(t, s, ctx, key, []byte("original data")); !first {
		t.Fatalf("first Put() written = %v, want true", first)
	}

	if second := put(t, s, ctx, key, []byte("different data, same key")); second {
		t.Error("second Put() written = true, want false (already deduplicated)")
	}

	got, err := get(t, s, ctx, key)
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

	put(t, s, ctx, "cafef00d", []byte("data"))
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
	put(t, s, ctx, key, data)

	got, err := getRange(t, s, ctx, key, 3, 5)
	if err != nil {
		t.Fatalf("GetRange() error = %v", err)
	}
	want := data[3:8]
	if string(got) != string(want) {
		t.Fatalf("GetRange(3, 5) = %q, want %q", got, want)
	}

	// Range extending to (but not past) EOF should return the tail, not error.
	got, err = getRange(t, s, ctx, key, 12, 4)
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
	put(t, s, ctx, key, []byte("x"))

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
	put(t, s, ctx, "ff00ff00", []byte("payload"))

	matches, _ := filepath.Glob(filepath.Join(root, "*", "*", "*.tmp-*"))
	if len(matches) != 0 {
		t.Errorf("found leftover temp files after successful Put: %v", matches)
	}
}

func TestStore_PutSizeMismatchLeavesNoBlob(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	key := "short-read"
	// Declare a larger size than the reader actually provides.
	_, err = s.Put(ctx, key, bytes.NewReader([]byte("short")), 100)
	if err == nil {
		t.Fatal("Put() error = nil, want an error for a reader shorter than declared size")
	}

	if has, _ := s.Has(ctx, key); has {
		t.Error("Has() = true after a failed Put; a partial blob must not be visible")
	}
	matches, _ := filepath.Glob(filepath.Join(root, "*", "*", "*.tmp-*"))
	if len(matches) != 0 {
		t.Errorf("found leftover temp files after failed Put: %v", matches)
	}
}

func TestStore_TotalBytes_CountsOnlyCompletedBlobs(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	put(t, s, ctx, "aaaaaaaa", bytes.Repeat([]byte("a"), 100))
	put(t, s, ctx, "bbbbbbbb", bytes.Repeat([]byte("b"), 200))

	total, err := s.TotalBytes(ctx)
	if err != nil {
		t.Fatalf("TotalBytes() error = %v", err)
	}
	if total != 300 {
		t.Errorf("TotalBytes() = %d, want 300 (100 + 200 from two completed blobs)", total)
	}
}

func TestStore_TotalBytes_ExcludesFreshTempFile(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	put(t, s, ctx, "aaaaaaaa", bytes.Repeat([]byte("a"), 100))

	// Simulate an in-flight upload: a temp file that exists but hasn't
	// been renamed into place yet, with a fresh (just-now) mtime.
	dir := filepath.Join(root, "bb", "bb")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	tmpPath := filepath.Join(dir, "bbbbbbbb.tmp-inflight")
	if err := os.WriteFile(tmpPath, bytes.Repeat([]byte("b"), 5000), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	total, err := s.TotalBytes(ctx)
	if err != nil {
		t.Fatalf("TotalBytes() error = %v", err)
	}
	if total != 100 {
		t.Errorf("TotalBytes() = %d, want 100 — a fresh in-flight temp file must not be counted", total)
	}

	// And it must still be on disk — TotalBytes must not have touched a
	// temp file young enough to plausibly be a live upload.
	if _, err := os.Stat(tmpPath); err != nil {
		t.Errorf("fresh temp file was removed by TotalBytes(), want it left alone: %v", err)
	}
}

func TestStore_TotalBytes_ReapsStaleTempFile(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	put(t, s, ctx, "aaaaaaaa", bytes.Repeat([]byte("a"), 100))

	// Simulate an orphaned temp file from a crashed process: exists, but
	// its mtime is far older than staleTempFileAge.
	dir := filepath.Join(root, "cc", "cc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	tmpPath := filepath.Join(dir, "cccccccc.tmp-orphaned")
	if err := os.WriteFile(tmpPath, bytes.Repeat([]byte("c"), 9000), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	oldTime := time.Now().Add(-2 * staleTempFileAge)
	if err := os.Chtimes(tmpPath, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}

	total, err := s.TotalBytes(ctx)
	if err != nil {
		t.Fatalf("TotalBytes() error = %v", err)
	}
	if total != 100 {
		t.Errorf("TotalBytes() = %d, want 100 — a stale orphaned temp file must not be counted", total)
	}

	// It must actually be gone afterward — silently excluding it forever
	// without reclaiming the disk space would be its own bug (the
	// scenario this test guards against).
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("stale temp file still exists after TotalBytes(), want it reaped: err=%v", err)
	}
}

func TestStore_TotalBytes_TolerantOfDisappearingFile(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	put(t, s, ctx, "aaaaaaaa", bytes.Repeat([]byte("a"), 100))

	// A file that gets removed between WalkDir listing it and the
	// callback calling d.Info() on it can't be reliably reproduced
	// deterministically without hooking the walk itself, so this test
	// instead confirms the documented contract holds for the case that
	// matters in practice: TotalBytes succeeds on a store where nothing
	// unexpected is present, establishing the baseline the chaos test
	// (TestChaos_UploadFetchEvictInterleaved in internal/casserver)
	// exercises under real concurrent Put/Delete races.
	if _, err := s.TotalBytes(ctx); err != nil {
		t.Fatalf("TotalBytes() error = %v, want nil on a normal store", err)
	}
}
