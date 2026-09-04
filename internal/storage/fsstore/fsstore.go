// Package fsstore implements storage.Store as a content-addressed
// filesystem directory. Blobs are keyed by their content hash and laid out
// as <root>/<hash[:2]>/<hash[2:4]>/<hash> to avoid huge flat directories.
package fsstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"xet-server/internal/storage"
)

var _ storage.Store = (*Store)(nil)

type Store struct {
	Root string
}

func New(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Store{Root: root}, nil
}

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

func (s *Store) Put(_ context.Context, key string, data []byte) (written bool, err error) {
	p := s.path(key)
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

func (s *Store) Get(_ context.Context, key string) ([]byte, error) {
	return os.ReadFile(s.path(key))
}

func (s *Store) GetRange(_ context.Context, key string, offset, length int64) ([]byte, error) {
	f, err := os.Open(s.path(key))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	buf := make([]byte, length)
	n, err := f.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:n], nil
}
