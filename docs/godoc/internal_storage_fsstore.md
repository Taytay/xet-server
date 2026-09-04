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

func (s *Store) Get(_ context.Context, key string) ([]byte, error)

func (s *Store) GetRange(_ context.Context, key string, offset, length int64) ([]byte, error)

func (s *Store) Has(_ context.Context, key string) (bool, error)

func (s *Store) Put(_ context.Context, key string, data []byte) (written bool, err error)
```
