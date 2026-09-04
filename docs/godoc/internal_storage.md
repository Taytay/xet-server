# `xet-server/internal/storage`

```
package storage // import "xet-server/internal/storage"

Package storage defines a backend-agnostic content-addressed object store.
Chunks and xorbs are both stored as opaque blobs keyed by content hash; the
same interface is satisfied by a local filesystem store (storage/fsstore) and an
S3-compatible store (storage/s3store), so the CAS server can run against either
without any caller-side branching.

TYPES

type Store interface {
	// Put writes data under key if not already present. Returns true if the
	// blob was newly written, false if it already existed (deduplicated).
	Put(ctx context.Context, key string, data []byte) (written bool, err error)

	// Get reads back the full blob stored under key.
	Get(ctx context.Context, key string) ([]byte, error)

	// GetRange reads back [offset, offset+length) of the blob stored under
	// key. Backends that cannot do a partial read efficiently may fall back
	// to a full Get followed by a slice.
	GetRange(ctx context.Context, key string, offset, length int64) ([]byte, error)

	// Has reports whether a blob is already stored under key.
	Has(ctx context.Context, key string) (bool, error)
}
    Store is a content-addressed blob store: Put is a no-op if the key already
    exists (the basis for dedup), Get/GetRange read back what was stored,
    and Has checks existence without transferring data.

type URLPresigner interface {
	// PresignGet returns a URL that, when fetched with a plain GET within
	// expirySeconds, returns the blob stored under key.
	PresignGet(ctx context.Context, key string, expirySeconds int) (string, error)
}
    URLPresigner is an optional capability: backends that can serve bytes
    directly to a client (e.g. S3/MinIO) implement it so the CAS server can hand
    out a presigned URL instead of proxying the bytes itself. Backends without a
    native presign mechanism (e.g. the filesystem store) simply don't implement
    this interface; callers type-assert for it.
```
