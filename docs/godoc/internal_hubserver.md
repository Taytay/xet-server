# `xet-server/internal/hubserver`

```
package hubserver // import "xet-server/internal/hubserver"

Package hubserver implements just enough of huggingface.co's Hub REST API
(distinct from the CAS API in internal/casserver) to let the real `hf upload` /
`hf download` CLI commands (via huggingface_hub) work end-to-end against a local
server, with HF_ENDPOINT pointed at it:

  - POST /api/repos/create — repo creation (idempotent)
  - GET /api/{repo_type}s/{repo_id}/xet-{read,write}-token/{revision} — issues a
    CAS endpoint + bearer token via response headers
  - POST /api/{repo_type}s/{repo_id}/commit/{revision} — ndjson commit payload
  - HEAD/GET /{repo_id}/resolve/{revision}/{filename} — file metadata + content

No git refs, branches, PRs, or real auth are modeled: every repo has a single
implicit "main" revision, and any bearer token is accepted. State is held in
memory alongside the paired casserver.Server, since a commit's file entries need
to reference the Xet file hash the CAS layer already knows how to reconstruct.

TYPES

type Server struct {
	CASBaseURL string
	CAS        casInfo

	// Has unexported fields.
}
    Server implements the Hub API shim. CASBaseURL is the base URL of the paired
    casserver.Server instance (e.g. "http://localhost:8420"), handed out via
    the xet-token routes' X-Xet-Cas-Url header. CAS provides the bridge from a
    committed file's plain SHA-256 to its Xet/Merkle hash.

func New(casBaseURL string, cas casInfo) *Server

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request)
```
