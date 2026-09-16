package storage

import (
	"context"
	"time"
)

// Enumerator is an optional capability: backends that can list every
// blob they hold implement it, so a caller that owns the store's
// lifecycle (casserver's garbage collector) can find blobs no index
// references any more - a xorb whose files were all dropped, or one
// uploaded just before a crash that lost the snapshot naming it. Like
// Sizer this is a walk over everything stored, meant for an occasional
// sweep, never a request hot path.
//
// fn is called once per completed blob with its key, size and last
// modification time; an in-flight upload's staging file is not a blob
// and is skipped. Returning an error from fn stops the walk and returns
// that error.
type Enumerator interface {
	Enumerate(ctx context.Context, fn func(key string, size int64, modTime time.Time) error) error
}

// Unwrapper is implemented by a Store that decorates another one
// (VerifyingStore) so a caller needing a capability the decorator does
// not forward can reach the backend underneath.
type Unwrapper interface {
	Unwrap() Store
}
