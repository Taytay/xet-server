# `xet-server/internal/storage/s3store`

```
package s3store // import "xet-server/internal/storage/s3store"

Package s3store implements storage.Store against any S3-compatible
HTTP API (AWS S3, MinIO, etc.) using hand-rolled SigV4 request signing
(internal/sigv4) instead of a third-party SDK. Uses path-style addressing
(http://endpoint/bucket/key), which both MinIO and AWS S3 support.

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

func (s *Store) EnsureBucket(ctx context.Context) error
    EnsureBucket creates the bucket if it doesn't already exist. MinIO and S3
    both accept an empty-body PUT to the bucket root for this.

func (s *Store) Get(ctx context.Context, key string) ([]byte, error)

func (s *Store) GetRange(ctx context.Context, key string, offset, length int64) ([]byte, error)

func (s *Store) Has(ctx context.Context, key string) (bool, error)

func (s *Store) PresignGet(_ context.Context, key string, expirySeconds int) (string, error)
    PresignGet implements storage.URLPresigner: returns a SigV4
    query-authenticated URL a client can GET directly against the S3/MinIO
    endpoint, bypassing our own server for the bytes themselves.

func (s *Store) Put(ctx context.Context, key string, data []byte) (written bool, err error)
    Put uploads data under key if not already present. S3 has no native
    "create if absent" semantic, so this does a HEAD-then-PUT; a benign race
    (two callers uploading the identical bytes for the same content-addressed
    key concurrently) just means both write the same content and both report
    "written" — the CAS dedup logic that matters for cost/perf still works
    because the vast majority of calls hit an existing key on Has and skip the
    PUT entirely.
```
