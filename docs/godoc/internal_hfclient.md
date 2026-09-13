# `github.com/guilt/xet-server/internal/hfclient`

```
package hfclient // import "github.com/guilt/xet-server/internal/hfclient"

cas.go: the CAS-side half of hfclient - talks to the real Xet CAS server whose
base URL is discovered per-repo via Client.GetXetToken (see the package doc
comment). Kept in its own type (CASClient, not a method set on Client) since CAS
is architecturally a separate service from the Hub even in the real deployment
this package talks to.

Package hfclient is an HTTP client for the real Hugging Face Hub API
(https://huggingface.co by default) and the real Xet CAS API it points callers
at - the upstream half of a caching pull-through proxy (internal/proxyhub,
internal/proxycas). Every call takes the caller's own bearer token and forwards
it upstream unchanged via auth.CredentialHelper (pure credential passthrough:
this package never holds or uses a secret of its own).

The real CAS base URL is never configured directly; it's discovered per-repo
from the Hub's own xet-{read,write}-token response (GetXetToken below),
exactly how a real hf_xet client discovers it - mirroring xet-core's
own DirectRefreshRouteTokenRefresher::get_cas_jwt (see docs/PROTOCOL.md
section 6 for the exact response contract this package parses). Use
NewCASClient(token.CasURL) to build the CAS-side client once a GetXetToken call
has returned it.

CONSTANTS

const DefaultHubURL = "https://huggingface.co"
    DefaultHubURL is the real Hugging Face Hub's base URL, used when no
    -upstream-hub-url override is given.


TYPES

type CASClient struct {
	BaseURL string
	HTTP    *http.Client
}
    CASClient is an upstream HTTP client for the real Xet CAS API at BaseURL (as
    returned by a Hub xet-token response's CasURL field - see NewCASClient).

func NewCASClient(baseURL string) *CASClient
    NewCASClient returns a CASClient targeting baseURL - normally a Hub
    xet-token response's CasURL, never a value this package invents or defaults
    on its own (see the package doc comment on why there is no DefaultCASURL
    constant paralleling DefaultHubURL).

func (c *CASClient) FetchChunkDedup(ctx context.Context, cred auth.CredentialHelper, prefix, hash string) (*http.Response, error)
    FetchChunkDedup calls GET /v1/chunks/{prefix}/{hash} - the global
    chunk-dedup lookup, returning the raw bytes of whichever shard referenced
    this chunk hash (see internal/casserver/reconstruction.go's handleChunkDedup
    doc comment for the wire contract this mirrors). Same caller-closes-body,
    non-2xx-is-not-an-error contract as FetchXorb.

func (c *CASClient) FetchPresigned(ctx context.Context, url, rangeHeader string) (*http.Response, error)
    FetchPresigned GETs an absolute presigned URL (a self-signed CDN/S3 URL
    from an upstream reconstruction response's fetch_info) with an optional
    Range header, returning the raw response. No credential is attached
    - the signature is in the URL itself. This is how proxycas fetches
    xorb bytes for caching: the real Xet CAS server does not serve xorb
    bodies over its own /v1/xorbs/ endpoint (its allow list is HEAD,POST),
    so the presigned URLs from the reconstruction are the only way to get them.
    Same caller-closes-body, non-2xx-is-not-an-error contract as FetchXorb.

func (c *CASClient) FetchReconstruction(ctx context.Context, cred auth.CredentialHelper, fileID, rangeHeader string, v2 bool) (*http.Response, error)
    FetchReconstruction calls GET /v1/reconstructions/{file_id} (or /v2/...
    if v2 is true), forwarding rangeHeader unchanged. Returns the raw response
    for the caller (internal/proxycas) to relay downstream and cache. IMPORTANT:
    real production Xet's fetch_info/xorbs URLs are presigned S3 URLs pointing
    directly at blob storage, not at this CAS server - a caller that relays
    this response unmodified lets the downstream client fetch bytes straight
    from S3, bypassing the proxy's cache entirely. proxycas handles this not
    by rewriting the decoded response itself, but by feeding its terms into
    its embedded casserver.Server via IngestFileRecon and then delegating:
    casserver's own reconstruction handler always emits fetch URLs pointing at
    itself, so the rewrite happens as a side effect of using the real server
    as the serving engine rather than needing a rewrite step of its own.
    This package intentionally does not do any such rewriting itself -
    it's a proxycas/casserver concern, not an upstream-client one. Same
    caller-closes-body, non-2xx-is-not-an-error contract as FetchXorb.

func (c *CASClient) FetchXorb(ctx context.Context, cred auth.CredentialHelper, prefix, hash, rangeHeader string) (*http.Response, error)
    FetchXorb calls GET /v1/xorbs/{prefix}/{hash}, forwarding rangeHeader
    (the exact Range header value the downstream caller sent, or "" for none)
    unchanged - proxycas is a byte cache, not a re-verifying client, so this
    returns the response body and status exactly as the real CAS server sent
    them (200 or 206) for the caller to stream/store as-is, the same trust model
    casserver's own xorb-fetch path already uses for locally-stored bytes (see
    internal/casserver/xorbs.go).

    The caller MUST close the returned response body, even on a non-2xx status
    (that path is not treated as an error here - see the doc comment on why:
    unlike the Hub-side calls in hfclient.go, a 404/416 here is exactly what
    proxycas needs to relay to its own caller unchanged, not something to
    collapse into a *StatusError).

func (c *CASClient) HeadXorb(ctx context.Context, cred auth.CredentialHelper, prefix, hash string) (*http.Response, error)
    HeadXorb calls HEAD /v1/xorbs/{prefix}/{hash} - a real upstream HEAD,
    not a full GET with the body discarded: proxycas's own -no-cache HEAD path
    uses this to report a xorb's size without transferring its bytes at all,
    matching -no-cache's "no local storage read/write" contract without the
    bandwidth waste a GET-and-discard would cost. Same caller-closes-body,
    non-2xx-is-not-an-error contract as FetchXorb.

func (c *CASClient) UploadShard(ctx context.Context, cred auth.CredentialHelper, body io.Reader) (*http.Response, error)
    UploadShard calls POST /v1/shards.

func (c *CASClient) UploadXorb(ctx context.Context, cred auth.CredentialHelper, prefix, hash string, body io.Reader) (*http.Response, error)
    UploadXorb calls POST /v1/xorbs/{prefix}/{hash} - the write-through path
    proxycas uses to relay a caller's xorb upload to the real CAS server
    immediately (uploads are never served from or held back by the local cache;
    they're also opportunistically written to it on the way through so
    a download right after the caller's own upload is a local hit - see
    internal/proxycas).

type Client struct {
	HubBaseURL string
	HTTP       *http.Client
}
    Client is an upstream HTTP client for the real Hub API. HubBaseURL defaults
    to DefaultHubURL; there is deliberately no CASBaseURL field - see the
    package doc comment for why that's discovered per-call instead.

func New(hubBaseURL string) *Client
    New returns a Client targeting hubBaseURL (DefaultHubURL if empty).

func (c *Client) Commit(ctx context.Context, cred auth.CredentialHelper, repoType, repoID, revision string, files []CommitFile) (*CommitResult, error)
    Commit calls POST /api/{repo_type}s/{repo_id}/commit/{revision} with an
    ndjson body of lfsFile entries - the write-through path proxyhub uses to
    relay a caller's `hf upload` commit to the real Hub immediately, never
    served from cache.

func (c *Client) CreateBranch(ctx context.Context, cred auth.CredentialHelper, repoType, repoID, branch string) error
    CreateBranch calls POST /api/{repo_type}s/{repo_id}/branch/{branch}
    - idempotent (real create_branch(exist_ok=True) semantics; see
    internal/hubserver's handleCreateBranch, the shim's own copy of this same
    contract, and CHANGELOG.md's v0.8.0 entry on why the caller side of this
    call needs X-Error-Code: RevisionNotFound to trigger it in the first place).

func (c *Client) CreateRepo(ctx context.Context, cred auth.CredentialHelper, name, organization, repoType string) error
    CreateRepo calls POST /api/repos/create - idempotent on the real Hub the
    same way it is on this project's own hubserver shim.

func (c *Client) GetXetToken(ctx context.Context, cred auth.CredentialHelper, repoType, repoID, revision string, kind XetTokenKind) (*XetToken, error)
    GetXetToken calls GET
    /api/{repo_type}s/{repo_id}/xet-{read,write}-token/{revision}.
    The returned XetToken.CasURL is the real upstream CAS base URL to use for
    every subsequent CAS call (xorb/chunk fetch, reconstruction) against this
    repo/revision - see NewCASClient.

func (c *Client) ListTree(ctx context.Context, cred auth.CredentialHelper, repoType, repoID, revision string) ([]TreeEntry, error)
    ListTree calls GET /api/{repo_type}s/{repo_id}/tree/{revision},
    recursive=true - enumerates every file in the repo at revision in one call.
    The real endpoint paginates (GitHub-style Link headers); this follows every
    page before returning, matching huggingface_hub's own paginate() helper (see
    internal/hubserver/tree.go's doc comment).

func (c *Client) Preupload(ctx context.Context, cred auth.CredentialHelper, repoType, repoID, revision string, files []PreuploadFile) ([]PreuploadResult, error)
    Preupload calls POST /api/{repo_type}s/{repo_id}/preupload/{revision} -
    negotiates per-file upload mode with the real Hub before a commit.

func (c *Client) RelayRaw(ctx context.Context, cred auth.CredentialHelper, method, path, contentType string, body io.Reader) (*http.Response, error)
    RelayRaw sends method to upstreamHubURL+path with body verbatim (and the
    given Content-Type), returning the raw response for the caller to relay
    unchanged. This is the write-path escape hatch for requests whose exact
    wire shape this client does not model - preupload's per-file `sample` field,
    commit's ndjson LFS-pointer lines - where a decode-and-reencode would
    silently drop fields the real Hub requires (real huggingface_hub rejects
    a preupload body whose files lack `sample`: "expected string, received
    undefined"). The caller must close the returned body.

func (c *Client) RepoInfo(ctx context.Context, cred auth.CredentialHelper, repoType, repoID, revision string) (*RepoInfo, error)
    RepoInfo calls GET /api/{repo_type}s/{repo_id}/revision/{revision} -
    resolves revision (a branch/tag name or already a commit hash) against the
    real Hub. Returns a *StatusError wrapping the real upstream status (404 with
    X-Error-Code: RevisionNotFound for an unknown revision) on failure.

func (c *Client) Resolve(ctx context.Context, cred auth.CredentialHelper, repoType, repoID, revision, filename string) (*ResolveInfo, error)
    Resolve calls HEAD /[{repo_type}s/]{repo_id}/resolve/{revision}/{filename}
    - the real Hub's per-file metadata endpoint; huggingface_hub's HEAD call is
    what triggers the Xet download path (see internal/hubserver/resolve.go's doc
    comment for the exact header contract this mirrors).

    repoType selects the URL prefix the real Hub requires: "model" (or "")
    resolves at the bare /{repo_id}/... path, while "dataset"/"space" resolve
    under /datasets/{repo_id}/... and /spaces/{repo_id}/... respectively (see
    resolveURLPrefix). Omitting the prefix for a dataset makes the real Hub
    answer 404 RepositoryNotFound - it looks for a MODEL of that name - which is
    exactly what broke `hf download bigcode/the-stack-v2 --repo-type dataset`
    through this proxy.

type CommitFile struct {
	Path string `json:"path"`
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}
    CommitFile is one file entry proxyhub relays upstream as part of a commit -
    mirrors internal/hubserver's commitLine.Value shape.

type CommitResult struct {
	CommitOID string `json:"commitOid"`
	CommitURL string `json:"commitUrl"`
}
    CommitResult is POST .../commit/{revision}'s response body - mirrors
    internal/hubserver's commitResponse.

type PreuploadFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}
    PreuploadFile is one file entry in a preupload request/response - mirrors
    internal/hubserver's preuploadRequest/preuploadFileInfo shapes.

type PreuploadResult struct {
	Path         string `json:"path"`
	UploadMode   string `json:"uploadMode"`
	ShouldIgnore bool   `json:"shouldIgnore"`
	OID          string `json:"oid,omitempty"`
}
    PreuploadResult is one file's negotiated upload mode - mirrors
    hubserver.preuploadFileInfo's full shape. ShouldIgnore must round-trip
    even though this project's own hubserver always sets it false:
    real huggingface_hub's _fetch_upload_modes reads file["shouldIgnore"]
    unconditionally with no default, so an upstream response missing it (e.g.
    relayed through a decode step that dropped the field) crashes the real hf
    CLI with a KeyError rather than a clean error from this project's own code.
    OID is read via file.get("oid") (a safe default of None), so it can't cause
    the same crash, but is still included for completeness.

type RepoInfo struct {
	ID       string        `json:"id"`
	SHA      string        `json:"sha"`
	Siblings []RepoSibling `json:"siblings"`
}
    RepoInfo is the subset of the real Hub's ModelInfo/DatasetInfo/SpaceInfo
    JSON shape this package reads - see internal/hubserver's repoInfoResponse
    (the shim's own, deliberately narrower, mirror of this same shape) for why
    sha is the one field that actually matters downstream. Siblings is captured
    too so a caller (proxyhub) can relay the file list back to the client -
    huggingface_hub's snapshot_download treats a repo whose siblings is empty as
    unreliable and falls back to list_repo_tree, whose generator-returning shape
    then breaks tqdm's thread_map (min() on an empty sequence of length_hints),
    so preserving upstream siblings avoids that fallback path entirely.

type RepoSibling struct {
	RFilename string `json:"rfilename"`
}
    RepoSibling mirrors one element of the real Hub's per-file `siblings`
    array (huggingface_hub's RepoSibling): only rfilename is required for
    snapshot_download's fast path.

type ResolveInfo struct {
	XetHash         string
	LinkedSize      int64
	ETag            string
	RepoCommit      string
	XetRefreshRoute string
}
    ResolveInfo is the subset of resolve/HEAD response headers this package
    reads - mirrors internal/hubserver's handleResolve response headers (the
    shim's own copy of this same contract).

type StatusError struct {
	Method    string
	URL       string
	Status    int
	ErrorCode string // X-Error-Code response header, if present
	Body      []byte // response body, capped by the caller reading it
}
    StatusError is returned by any Client/CASClient method when the upstream
    response status is not the one success code that method expects.
    Preserves the status so a caller (internal/proxyhub, internal/proxycas)
    can map it 1:1 onto its own downstream response instead of collapsing every
    upstream failure into a generic 502 - callers should check errors.As(err,
    &StatusError{}) and, on match, mirror Status (and, where applicable,
    the specific X-Error-Code header that made this project's own hubserver
    correctly signal RevisionNotFound to a real huggingface_hub caller -
    see docs/PROTOCOL.md and CHANGELOG.md's v0.8.0 entry on that fix).
    Body is included (truncated) in Error()'s own message, since a real upstream
    5xx's body is often the only clue why - this is diagnostic-by-default,
    not something callers need to read directly.

func (e *StatusError) Error() string

type TreeEntry struct {
	Type    string `json:"type"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	OID     string `json:"oid"`
	XetHash string `json:"xetHash,omitempty"`
}
    TreeEntry mirrors one element of the real Hub's tree-listing response - see
    internal/hubserver/tree.go's treeEntry (the shim's own mirror of this exact
    shape, confirmed against huggingface_hub's RepoFile deserialization).

type XetToken struct {
	CasURL      string `json:"casUrl"`
	Exp         int64  `json:"exp"`
	AccessToken string `json:"accessToken"`
}
    XetToken is the real Hub's xet-{read,write}-token response body - mirrors
    internal/hubserver's xetTokenResponse (the shim's own copy of this same
    shape, confirmed against xet-core's CasJWTInfo - see docs/PROTOCOL.md
    section 6). CasURL is the real upstream CAS base URL for this repo/revision:
    this is the ONE place this package learns it - see the package doc comment.

type XetTokenKind int
    XetTokenKind selects which of the two functionally-identical
    xet-{read,write}-token endpoints to call - see internal/hubserver's
    xetTokenType (the shim's own copy of this same distinction).

const (
	XetTokenRead XetTokenKind = iota
	XetTokenWrite
)
```
