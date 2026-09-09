// Package storage defines a backend-agnostic content-addressed object store.
// Chunks and xorbs are both stored as opaque blobs keyed by content hash;
// the same interface is satisfied by a local filesystem store
// (storage/fsstore) and an S3-compatible store (storage/s3store), so the CAS
// server can run against either without any caller-side branching.
package storage

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound is returned (via errors.Is) by Get/GetRange when no blob
// exists under the requested key. Every backend must wrap its native
// not-found signal (os.ErrNotExist, an S3 404) in this sentinel so callers
// can branch on it without knowing which backend is in use.
var ErrNotFound = errors.New("storage: blob not found")

// ErrSizeMismatch is returned (via errors.Is) by Put when the number of
// bytes actually read from r does not match the declared size. This is a
// client protocol error (a wrong Content-Length or a truncated body), not
// a storage fault; the caller should treat it as non-retryable without a
// corrected size.
var ErrSizeMismatch = errors.New("storage: reader did not match declared size")

// ErrContentMismatch is returned (via errors.Is) by VerifyingStore.Put
// when a dedup hit's incoming content differs from what's already stored
// under the same key — a hash collision or storage corruption, since two
// different byte sequences should never produce the same content hash.
// Unlike ErrSizeMismatch, this is never a normal client protocol error;
// it always indicates something worth an operator's attention. See
// VerifyingStore's doc comment.
var ErrContentMismatch = errors.New("storage: dedup hit content differs from stored blob")

// Store is a content-addressed blob store: Put is a no-op if the key
// already exists (the basis for dedup), Get/GetRange stream back what was
// stored, and Has checks existence without transferring data.
//
// Put is atomic with respect to failure: if r returns an error, ctx is
// canceled, or the read stops short of size, no partial blob is left
// visible under key — implementations must stage writes (e.g. a temp file
// renamed into place, or an upload that is only finalized on success) so a
// caller can safely retry the same key after a failed attempt without a
// prior partial write corrupting the retry. Get/GetRange never observe a
// partially-written blob as a result.
type Store interface {
	// Put streams exactly size bytes from r into key if not already
	// present. Returns true if the blob was newly written, false if it
	// already existed (deduplicated) — in the deduplicated case, r is
	// drained/ignored without being stored again.
	Put(ctx context.Context, key string, r io.Reader, size int64) (written bool, err error)

	// Get returns a reader for the full blob stored under key. The caller
	// must Close it. Returns an error satisfying errors.Is(err,
	// ErrNotFound) if no blob exists under key.
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// GetRange returns a reader for [offset, offset+length) of the blob
	// stored under key. The caller must Close it. Backends that cannot do
	// a partial read natively may fall back to a full read followed by a
	// bounded copy. Returns an error satisfying errors.Is(err,
	// ErrNotFound) if no blob exists under key.
	GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error)

	// Has reports whether a blob is already stored under key.
	Has(ctx context.Context, key string) (bool, error)
}

// URLPresigner is an optional capability: backends that can serve bytes
// directly to a client (e.g. S3/MinIO) implement it so the CAS server can
// hand out a presigned URL instead of proxying the bytes itself. Backends
// without a native presign mechanism (e.g. the filesystem store) simply
// don't implement this interface; callers type-assert for it.
type URLPresigner interface {
	// PresignGet returns a URL that, when fetched with a plain GET within
	// expirySeconds, returns the blob stored under key.
	PresignGet(ctx context.Context, key string, expirySeconds int) (string, error)
}

// Deleter is an optional capability: backends that support removing a
// previously-stored blob implement it. Not part of the core Store
// interface since not every caller needs delete (the Xet Data API's chunk
// store, for instance, never removes anything) — this exists for callers
// like a storage-budget eviction sweep that do. Deleting an
// already-absent key is not an error (idempotent).
type Deleter interface {
	Delete(ctx context.Context, key string) error
}

// Sizer is an optional capability: backends that can report the total
// bytes currently stored implement it, so a caller (e.g. an eviction
// sweep) can tell whether it's over a size budget without maintaining its
// own running total independently of the backend's actual state. This is
// expected to be called on a slow poll interval (minutes), not a hot
// path — backends are free to implement it by walking/listing everything
// they hold each call.
type Sizer interface {
	TotalBytes(ctx context.Context) (int64, error)
}
