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

func (s *Store) Delete(_ context.Context, key string) error
    Delete removes the blob stored under key. Deleting an already-absent key
    is not an error, matching the interface's idempotent-delete contract (an
    eviction sweep racing a concurrent Delete of the same key, or retrying after
    a partial failure, shouldn't need to distinguish "already gone" from "just
    removed").

func (s *Store) Get(_ context.Context, key string) (io.ReadCloser, error)

func (s *Store) GetRange(_ context.Context, key string, offset, length int64) (io.ReadCloser, error)

func (s *Store) Has(_ context.Context, key string) (bool, error)

func (s *Store) Put(ctx context.Context, key string, r io.Reader, size int64) (written bool, err error)
    Put stages the write to a per-attempt temp file and only renames it into
    place once size bytes have been fully copied from r. If r errs, ctx is
    canceled, or the copy stops short of size, the temp file is removed and no
    partial blob is ever visible under key — a caller can retry Put with a fresh
    reader afterward with no cleanup of its own required.

func (s *Store) TotalBytes(_ context.Context) (int64, error)
    TotalBytes walks the store's root and sums the size of every stored blob.
    This is an O(number of blobs) directory walk, not a cached counter —
    fine for a slow poll (an eviction sweep runs every few minutes at most),
    but callers should not call this on any request hot path.
```
