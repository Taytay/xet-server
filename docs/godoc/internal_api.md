# `xet-server/internal/api`

```
package api // import "xet-server/internal/api"

Package api implements the xetd HTTP server: a local stand-in for Xet's
CAS/reconstruction service. Clients upload files, which are chunked and
deduplicated against the store; clients download files by reconstructing them
from a manifest's chunk list.

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
