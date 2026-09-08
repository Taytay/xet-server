// Package fsstore implements storage.Store as a content-addressed
// filesystem directory. Blobs are keyed by their content hash and laid out
// as <root>/<hash[:2]>/<hash[2:4]>/<hash> to avoid huge flat directories.
package fsstore

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"xet-server/internal/storage"
)

var (
	_ storage.Store   = (*Store)(nil)
	_ storage.Deleter = (*Store)(nil)
	_ storage.Sizer   = (*Store)(nil)
)

type Store struct {
	Root string
}

func New(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Store{Root: root}, nil
}

// tempFileMarker is the substring os.CreateTemp's pattern
// (filepath.Base(p)+".tmp-*") always produces in a staging file's name —
// used by TotalBytes to distinguish a completed blob from an in-flight
// upload's staging file (see TotalBytes's doc comment).
const tempFileMarker = ".tmp-"

// staleTempFileAge is how old a ".tmp-*" staging file must be before
// TotalBytes treats it as orphaned (a crashed/killed process that never
// reached its own os.Remove(tmp) cleanup path — see Put's error-handling
// branches, none of which run if the process dies mid-copy) rather than a
// legitimately in-flight upload. Chosen well above any realistic single
// xorb upload duration (even a slow multi-GB transfer over a bad link),
// so a false positive here — reaping a temp file that's actually still
// being written — should not happen in practice; a true in-flight upload
// this old almost certainly belongs to a client that's gone anyway.
const staleTempFileAge = 30 * time.Minute

func (s *Store) path(key string) string {
	if len(key) < 4 {
		return filepath.Join(s.Root, key)
	}
	return filepath.Join(s.Root, key[:2], key[2:4], key)
}

func (s *Store) Has(_ context.Context, key string) (bool, error) {
	_, err := os.Stat(s.path(key))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// Put stages the write to a per-attempt temp file and only renames it into
// place once size bytes have been fully copied from r. If r errs, ctx is
// canceled, or the copy stops short of size, the temp file is removed and
// no partial blob is ever visible under key — a caller can retry Put with a
// fresh reader afterward with no cleanup of its own required.
//
// The temp file is created via os.CreateTemp (not a fixed PID-based name):
// concurrent Put calls for the *same* key within one process are a normal,
// expected race (e.g. several clients uploading an identical xorb at once
// — see TestAdversarial_ConcurrentUploadsOfSameXorb), and each needs its
// own independent staging file rather than colliding on one shared path.
func (s *Store) Put(ctx context.Context, key string, r io.Reader, size int64) (written bool, err error) {
	p := s.path(key)
	if _, err := os.Stat(p); err == nil {
		io.Copy(io.Discard, io.LimitReader(r, size))
		return false, nil
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	f, err := os.CreateTemp(dir, filepath.Base(p)+".tmp-*")
	if err != nil {
		return false, err
	}
	tmp := f.Name()
	n, copyErr := io.Copy(f, io.LimitReader(r, size))
	closeErr := f.Close()
	if copyErr == nil && closeErr != nil {
		copyErr = closeErr
	}
	if copyErr == nil && n != size {
		copyErr = storage.ErrSizeMismatch
	}
	if copyErr != nil {
		os.Remove(tmp)
		return false, copyErr
	}
	if ctx.Err() != nil {
		os.Remove(tmp)
		return false, ctx.Err()
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		// Another concurrent Put for the same key may have already
		// renamed its own temp file into place first — that's a
		// successful dedup, not a failure, from this caller's
		// perspective, as long as p now actually exists.
		if _, statErr := os.Stat(p); statErr == nil {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *Store) Get(_ context.Context, key string) (io.ReadCloser, error) {
	f, err := os.Open(s.path(key))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, storage.ErrNotFound
		}
		return nil, err
	}
	return f, nil
}

func (s *Store) GetRange(_ context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	f, err := os.Open(s.path(key))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, storage.ErrNotFound
		}
		return nil, err
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return rangeReadCloser{Reader: io.LimitReader(f, length), f: f}, nil
}

// rangeReadCloser pairs a bounded Reader over an open *os.File with that
// file's Close, so GetRange's caller can Close the returned io.ReadCloser
// without needing to know a file underlies it.
type rangeReadCloser struct {
	io.Reader
	f *os.File
}

func (r rangeReadCloser) Close() error { return r.f.Close() }

// Delete removes the blob stored under key. Deleting an already-absent
// key is not an error, matching the interface's idempotent-delete
// contract (an eviction sweep racing a concurrent Delete of the same key,
// or retrying after a partial failure, shouldn't need to distinguish
// "already gone" from "just removed").
func (s *Store) Delete(_ context.Context, key string) error {
	if err := os.Remove(s.path(key)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// TotalBytes walks the store's root and sums the size of every *completed*
// stored blob. This is an O(number of blobs) directory walk, not a cached
// counter — fine for a slow poll (an eviction sweep runs every few
// minutes at most), but callers should not call this on any request hot
// path.
//
// In-flight upload staging files (os.CreateTemp's ".tmp-*" names — see
// Put) are excluded from the total: counting them would inflate the
// measured size by uploads that haven't committed yet and may never
// complete, skewing an eviction budget check upward for no real storage
// that will persist. But a temp file left behind by a process that
// crashed or was killed mid-upload (Put's own os.Remove(tmp) cleanup
// never got to run) is a real, permanent disk-space leak if silently
// excluded forever — so a temp file older than staleTempFileAge is
// treated as orphaned: it's reaped (removed) here rather than skipped, so
// disk space is actually reclaimed instead of just hidden from the count.
func (s *Store) TotalBytes(_ context.Context) (int64, error) {
	var total int64
	now := time.Now()
	err := filepath.WalkDir(s.Root, func(path string, d fs.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if strings.Contains(d.Name(), tempFileMarker) {
			if now.Sub(info.ModTime()) > staleTempFileAge {
				os.Remove(path) // best-effort; a failed reap just gets retried next sweep
			}
			return nil
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}
