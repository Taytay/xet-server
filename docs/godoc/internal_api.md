# `xet-server/internal/api`

```
package api // import "xet-server/internal/api"

Package api implements the Xet Data API (mounted at /v1 by cmd/xetd,
sharing that namespace with — but never overlapping the specific literal paths
of — internal/casserver's own /v1,/v2 CAS reimplementation; see xetDataV1
below for the exact paths this package owns): a local stand-in for Xet's
CAS/reconstruction service, not wire-compatible with the real protocol (see
internal/casserver for that). Clients upload files, which are chunked and
deduplicated against the store; clients download files by reconstructing them
from a manifest's chunk list.

Every route except /v1/stats (this project's own operator endpoint,
not part of any real protocol) is gated by auth.Authenticator per the scope
real Xet/HF convention implies: write for upload, read for download/manifest.
Defaults to auth.NoAuth{} — this server's pre-v0.8.0 behavior, unconditionally
allowing every request — until SetAuthenticator is called with something else.
See internal/casserver's identical pattern, which this mirrors.

CONSTANTS

const (
	V1          = "/v1"
	UploadPath  = V1 + "/upload"
	FilesPrefix = V1 + "/files/"
	StatsPath   = V1 + "/stats"
)
    V1 is this API's URL version prefix, exported so cmd/xetd can reference it
    directly when wiring routes onto its own top-level mux, instead of re-typing
    "/v1" as a raw literal at the call site. It shares the /v1 namespace with
    internal/casserver's own CAS protocol rather than living under a separate
    top-level path — grouped there because cmd/xetd mounts both on the same
    server, and this project's own "Xet Data" surface is versioned exactly like
    casserver's /v1,/v2 rather than left bare.

    UploadPath, FilesPrefix, and StatsPath are the specific literal sub-paths
    (relative to V1) this package registers, also exported so cmd/xetd's own
    mux.Handle calls reference the same constants this package's own routes()
    uses — one definition per path, not duplicated as a string literal at each
    call site. They never collide with casserver's own /v1 paths (xorbs, shards,
    reconstructions, chunks, telemetry, storage-stats): cmd/xetd registers
    these literal patterns on its shared top-level mux ahead of casserver's
    "/v1/" wildcard, and Go's http.ServeMux always prefers the more specific
    match regardless of registration order. A future incompatible change
    to this API's wire shape can land at /v2/upload,... (its own routes()
    using routing.MountWithVersion("/v2", ...) instead) without touching v1's
    registrations below.


TYPES

type Server struct {
	// Has unexported fields.
}

func New(dataRoot string) (*Server, error)
    New creates a Server backed by a local filesystem chunk store rooted at
    <dataRoot>/chunks. Use NewWithStore to supply a different storage.Store
    backend (e.g. S3/MinIO).

func NewWithStore(dataRoot string, chunks storage.Store) (*Server, error)
    NewWithStore creates a Server whose chunk bytes live in the given
    storage.Store backend. Manifests always live on the local filesystem under
    <dataRoot>/manifests, since they're small CAS metadata rather than the bulk
    data the storage backend abstraction is for.

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request)

func (s *Server) SetAuthenticator(a auth.Authenticator)
    SetAuthenticator replaces this server's Authenticator (default
    auth.NoAuth{}, i.e. no enforcement — this server's pre-v0.8.0 behavior) and
    rebuilds the route table so the new scope checks take effect immediately,
    matching internal/casserver.Server.SetAuthenticator.

type UploadResult struct {
	FileID       string  `json:"file_id"`
	Name         string  `json:"name,omitempty"`
	Size         int64   `json:"size"`
	SHA256       string  `json:"sha256"`
	ChunksTotal  int     `json:"chunks_total"`
	ChunksNew    int     `json:"chunks_new"`
	BytesStored  int64   `json:"bytes_stored"`
	BytesOnWire  int64   `json:"bytes_on_wire"`
	DedupPercent float64 `json:"dedup_percent"`
}
```
