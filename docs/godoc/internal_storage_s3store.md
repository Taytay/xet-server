# `xet-server/internal/storage/s3store`

```
package s3store // import "xet-server/internal/storage/s3store"

Package s3store implements storage.Store against any S3-compatible
HTTP API (AWS S3, MinIO, etc.) using hand-rolled SigV4 request signing
(internal/sigv4) instead of a third-party SDK. Uses path-style addressing
(http://endpoint/bucket/key), which both MinIO and AWS S3 support.

Currently library-only: neither cmd/xetd nor cmd/xet-proxyd exposes a flag to
select this backend over internal/storage/fsstore (both binaries construct
an fsstore.Store directly) — a caller wanting S3 storage today has to build
their own main package around this package. See its own tests (this package's
live-MinIO test) for a working usage example.

TYPES

type Store struct {
	Endpoint string // e.g. "http://localhost:9000", no trailing slash
	Bucket   string
	Prefix   string // optional key prefix, e.g. "chunks/"
	Signer   *sigv4.Signer
	HTTP     *http.Client
	Now      func() time.Time // overridable for tests; defaults to time.Now
}

func New(endpoint, bucket, prefix, accessKey, secretKey, region string) *Store

func (s *Store) Delete(ctx context.Context, key string) error
    Delete removes the object stored under key. Deleting an already-absent
    key is not an error — S3's DELETE already behaves this way natively (204
    whether or not the key existed), matching the interface's idempotent-delete
    contract.

func (s *Store) EnsureBucket(ctx context.Context) error
    EnsureBucket creates the bucket if it doesn't already exist. MinIO and S3
    both accept an empty-body PUT to the bucket root for this.

func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, error)

func (s *Store) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error)

func (s *Store) Has(ctx context.Context, key string) (bool, error)

func (s *Store) PresignGet(_ context.Context, key string, expirySeconds int) (string, error)
    PresignGet implements storage.URLPresigner: returns a SigV4
    query-authenticated URL a client can GET directly against the S3/MinIO
    endpoint, bypassing our own server for the bytes themselves.

func (s *Store) Put(ctx context.Context, key string, r io.Reader, size int64) (written bool, err error)
    Put uploads size bytes from r under key if not already present. S3 has
    no native "create if absent" semantic, so this does a HEAD-then-PUT;
    a benign race (two callers uploading the identical bytes for the same
    content-addressed key concurrently) just means both write the same content
    and both report "written" — the CAS dedup logic that matters for cost/perf
    still works because the vast majority of calls hit an existing key on Has
    and skip the PUT entirely.

    The upload signs with sigv4.UnsignedPayload rather than a SHA-256 content
    hash: computing that hash would require buffering the full body before
    the request even starts, which defeats streaming a multi-GB xorb. S3 and
    MinIO both accept UNSIGNED-PAYLOAD for PUT; the object's own ETag/hash
    verification on the read side (this project's own chunk/xorb hashing) still
    catches corruption in transit.

    A failed or canceled PUT is not retried or cleaned up here — S3 has no
    partial-object visibility (a PUT either lands in full or the object doesn't
    exist), so unlike fsstore there is no staging file to remove; the caller can
    simply retry Put with a fresh reader.

func (s *Store) TotalBytes(ctx context.Context) (int64, error)
    TotalBytes sums the size of every object under this store's prefix via
    paginated ListObjectsV2 calls. Like fsstore's TotalBytes, this is meant
    for a slow poll (an eviction sweep), not a request hot path — each call is
    O(number of objects / 1000) round trips to the S3-compatible endpoint.
```
