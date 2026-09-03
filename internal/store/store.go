// Package store implements a content-addressed filesystem store for chunks,
// analogous to Xet's CAS (content-addressed storage) layer. Chunks are keyed
// by their SHA-256 hash and laid out as <root>/<hash[:2]>/<hash[2:4]>/<hash>
// to avoid huge flat directories.
package store

import (
	"fmt"
	"os"
	"path/filepath"
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

func (s *Store) path(hash string) string {
	if len(hash) < 4 {
		return filepath.Join(s.Root, hash)
	}
	return filepath.Join(s.Root, hash[:2], hash[2:4], hash)
}

// Has reports whether a chunk with this hash is already stored — the basis
// for dedup: callers skip writing chunks that already exist.
func (s *Store) Has(hash string) bool {
	_, err := os.Stat(s.path(hash))
	return err == nil
}

// Put writes data under hash if not already present. Returns true if the
// chunk was newly written, false if it was already deduplicated.
func (s *Store) Put(hash string, data []byte) (written bool, err error) {
	p := s.path(hash)
	if _, err := os.Stat(p); err == nil {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return false, err
	}
	tmp := p + fmt.Sprintf(".tmp-%d", os.Getpid())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		os.Remove(tmp)
		return false, err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return false, err
	}
	return true, nil
}

func (s *Store) Get(hash string) ([]byte, error) {
	return os.ReadFile(s.path(hash))
}
