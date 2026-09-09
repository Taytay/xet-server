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

Each repo supports real, independent revisions (a "main" branch is created
implicitly on first touch, matching how a real repo always has a default
branch; any other revision name is created on first commit to it) —
commit/resolve/preupload all operate against the named revision's own file
set and commit history, not a single shared implicit state. No git refs/PRs
are modeled. Every route except telemetry-equivalents (there are none in
this shim) requires the scope real xet-core/Hub convention implies (write
for token/commit/preupload/repo-create, read for resolve), enforced via
auth.Authenticator — see SetAuthenticator. The default (auth.NoAuth{}) enforces
nothing, this server's behavior prior to v0.8.0: any bearer token (or none)
is accepted. State is held in memory alongside the paired casserver.Server,
since a commit's file entries need to reference the Xet file hash the CAS layer
already knows how to reconstruct — and periodically checkpointed to disk (see
Server.Snapshot/LoadSnapshot in snapshot.go) so it survives a restart.

Package hubserver's snapshot.go implements Server's persistence, mirroring
casserver's snapshot.go: an atomic (stage-then-rename) JSON dump of every
repo/revision/file this server knows about, and a load of that file on startup.
See casserver/snapshot.go's package doc comment for the full tradeoff writeup
(periodic checkpoint, not a WAL — writes between snapshots are lost on a hard
crash).

Server's live state (repoState/revisionState, each carrying its own
sync.RWMutex) isn't itself JSON-marshalable, so this file defines flat,
mutex-free snapshot structs and converts to/from them.

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

func (s *Server) LoadSnapshot(path string) error
    LoadSnapshot reads a snapshot previously written by Snapshot and restores
    s's repos/revisions/files from it. Intended to be called once, before s
    starts serving any traffic. A missing file at path is not an error (a fresh
    server with no prior snapshot).

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request)

func (s *Server) SetAuthenticator(a auth.Authenticator)
    SetAuthenticator replaces this server's Authenticator (default
    auth.NoAuth{}, i.e. no enforcement — this server's pre-v0.8.0 behavior).
    Safe to call at any time — unlike casserver's SetAuthenticator, hubserver's
    routes dispatch by parsing the path inside each handler rather than
    registering one mux pattern per logical endpoint, so there's no route table
    to rebuild.

func (s *Server) Snapshot(path string) error
    Snapshot serializes s's repos/revisions/files to path via the same
    stage-to-temp-then-rename pattern casserver.Server.Snapshot and
    storage/fsstore.Store.Put both use, for the same reason: a reader must never
    observe a partially-written file.

    As with casserver's Snapshot, this briefly RLocks each repo/revision
    in turn rather than holding one lock across the whole operation — this
    server's own locking design (see the package doc comment) is already
    per-repo/per-revision specifically so unrelated repos don't contend, and a
    snapshot is not a stronger consistency boundary than any other multi-repo
    read already provides.
```
