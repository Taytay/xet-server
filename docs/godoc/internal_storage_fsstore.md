# `xet-server/internal/storage/fsstore`

```
package fsstore // import "xet-server/internal/storage/fsstore"

Package fsstore implements storage.Store as a content-addressed filesystem
directory. Blobs are keyed by their content hash and laid out as
<root>/<hash[:2]>/<hash[2:4]>/<hash> to avoid huge flat directories.

TYPES

type Store struct {
	Root string
}

func New(root string) (*Store, error)

func (s *Store) Get(_ context.Context, key string) (io.ReadCloser, error)

func (s *Store) GetRange(_ context.Context, key string, offset, length int64) (io.ReadCloser, error)

func (s *Store) Has(_ context.Context, key string) (bool, error)

func (s *Store) Put(ctx context.Context, key string, r io.Reader, size int64) (written bool, err error)
    Put stages the write to a per-attempt temp file and only renames it into
    place once size bytes have been fully copied from r. If r errs, ctx is
    canceled, or the copy stops short of size, the temp file is removed and no
    partial blob is ever visible under key — a caller can retry Put with a fresh
    reader afterward with no cleanup of its own required.
```
