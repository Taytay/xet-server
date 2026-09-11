# `xet-server/internal/client`

```
package client // import "xet-server/internal/client"

Package client is a thin HTTP client for talking to a xetd server.

TYPES

type Client struct {
	BaseURL string
	HTTP    *http.Client
	Cred    auth.CredentialHelper
}
    Client is a thin HTTP client for the Xet Data API (internal/api), mounted
    at api.V1 (sharing that namespace with, but never overlapping the specific
    paths of, the real CAS protocol). Cred, if set, is applied to every outgoing
    request via its FillCredential method (see auth.CredentialHelper) — nil is
    equivalent to auth.NoopCredentialHelper{}, this client's pre-v0.8.0 behavior
    of attaching no credential at all.

func New(baseURL string) *Client

func (c *Client) Pull(fileID, localPath string) error
    Pull downloads fileID and writes it to localPath.

func (c *Client) Push(localPath string) (*UploadResult, error)
    Push uploads the file at localPath and returns the server's summary,
    including how much of it deduplicated against chunks already stored.

func (c *Client) Stats() (map[string]any, error)
    Stats fetches store-wide dedup stats from the server.

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
