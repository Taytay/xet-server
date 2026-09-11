// Package hfclient is an HTTP client for the real Hugging Face Hub API
// (https://huggingface.co by default) and the real Xet CAS API it points
// callers at — the upstream half of a caching pull-through proxy
// (internal/proxyhub, internal/proxycas). Every call takes the caller's
// own bearer token and forwards it upstream unchanged via
// auth.CredentialHelper (pure credential passthrough: this package never
// holds or uses a secret of its own).
//
// The real CAS base URL is never configured directly; it's discovered
// per-repo from the Hub's own xet-{read,write}-token response (GetXetToken
// below), exactly how a real hf_xet client discovers it — mirroring xet-core's
// own DirectRefreshRouteTokenRefresher::get_cas_jwt (see docs/PROTOCOL.md
// section 6 for the exact response contract this package parses). Use
// NewCASClient(token.CasURL) to build the CAS-side client once a
// GetXetToken call has returned it.
package hfclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"xet-server/internal/auth"
)

// defaultHTTPClient is used by New/NewCASClient when the caller doesn't
// supply its own *http.Client. Its Transport bounds connect/TLS-handshake/
// response-header latency (10s each — generous for a reachable host, but
// short enough that a hung or unresponsive real huggingface.co/CAS
// endpoint fails fast instead of holding a goroutine — and, upstream of
// that, an internal/proxycas or internal/proxyhub request handler, and
// the downstream client's own connection — open indefinitely under
// sustained load). Deliberately NOT an overall request Timeout: a xorb
// transfer can legitimately take longer than any of these per-phase
// budgets on a slow connection, and a blanket timeout would abort an
// otherwise-healthy large transfer along with a genuinely hung one.
// Per-call deadlines for the metadata endpoints (which are always small
// and should never legitimately take long) are applied by the caller via
// context — see internal/proxyhub.Server.callContext, wrapped around its
// single-request calls (RepoInfo, Resolve, GetXetToken). ListTree is
// deliberately excluded: it loops internally over every page of a large
// repo's tree within one context, so a fixed per-call bound would abort
// a legitimately large (but healthy) listing partway through — its
// per-page requests are still bounded by this Transport's own
// ResponseHeaderTimeout.
var defaultHTTPClient = &http.Client{
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 10 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
}

// DefaultHubURL is the real Hugging Face Hub's base URL, used when no
// -upstream-hub-url override is given.
const DefaultHubURL = "https://huggingface.co"

// Client is an upstream HTTP client for the real Hub API. HubBaseURL
// defaults to DefaultHubURL; there is deliberately no CASBaseURL field —
// see the package doc comment for why that's discovered per-call instead.
type Client struct {
	HubBaseURL string
	HTTP       *http.Client
}

// New returns a Client targeting hubBaseURL (DefaultHubURL if empty).
func New(hubBaseURL string) *Client {
	if hubBaseURL == "" {
		hubBaseURL = DefaultHubURL
	}
	return &Client{HubBaseURL: hubBaseURL, HTTP: defaultHTTPClient}
}

// do builds and sends an HTTP request, filling cred's credential (pure
// passthrough — cred is normally auth.NewBearerCredentialHelper wrapping
// whatever token the downstream caller sent this proxy) before sending.
// Every exported method on Client and CASClient below goes through this
// (CASClient has its own copy, casDo, since it's a distinct base URL/
// http.Client pair) so a credential is never silently skipped on one
// call path but not another.
func (c *Client) do(ctx context.Context, cred auth.CredentialHelper, method, url string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if cred != nil {
		if err := cred.FillCredential(req); err != nil {
			return nil, fmt.Errorf("fill credential: %w", err)
		}
	}
	return c.httpClient().Do(req)
}

// httpClient returns c.HTTP, or defaultHTTPClient if unset — lets a
// zero-value Client still work (matching this project's other client
// packages), while still getting bounded connect/handshake/response
// -header timeouts rather than http.DefaultClient's totally unbounded
// behavior.
func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return defaultHTTPClient
}

// StatusError is returned by any Client/CASClient method when the
// upstream response status is not the one success code that method
// expects. Preserves the status so a caller (internal/proxyhub,
// internal/proxycas) can map it 1:1 onto its own downstream response
// instead of collapsing every upstream failure into a generic 502 —
// callers should check errors.As(err, &StatusError{}) and, on match,
// mirror Status (and, where applicable, the specific X-Error-Code header
// that made this project's own hubserver correctly signal
// RevisionNotFound to a real huggingface_hub caller — see
// docs/PROTOCOL.md and CHANGELOG.md's v0.8.0 entry on that fix). Body is
// included (truncated) in Error()'s own message, since a real upstream
// 5xx's body is often the only clue why — this is diagnostic-by-default,
// not something callers need to read directly.
type StatusError struct {
	Method    string
	URL       string
	Status    int
	ErrorCode string // X-Error-Code response header, if present
	Body      []byte // response body, capped by the caller reading it
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("%s %s: upstream status %d", e.Method, e.URL, e.Status)
	if e.ErrorCode != "" {
		msg += fmt.Sprintf(" (X-Error-Code: %s)", e.ErrorCode)
	}
	if len(e.Body) > 0 {
		body := e.Body
		const maxBodyInMessage = 512
		if len(body) > maxBodyInMessage {
			body = body[:maxBodyInMessage]
		}
		msg += fmt.Sprintf(": %s", body)
	}
	return msg
}

// newStatusError reads (and closes) resp.Body into a StatusError. Callers
// must not use resp.Body after calling this.
func newStatusError(method, url string, resp *http.Response) *StatusError {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	return &StatusError{
		Method:    method,
		URL:       url,
		Status:    resp.StatusCode,
		ErrorCode: resp.Header.Get("X-Error-Code"),
		Body:      body,
	}
}

// repoPath builds the "{repoType}s/{repoID}" URL segment real Hub URLs
// use (e.g. "models/alice/my-model") — repoID itself contains a "/"
// (namespace/name), so this is exactly two path segments once expanded,
// matching hubserver.splitRepoPath's inverse.
func repoPath(repoType, repoID string) string {
	return repoType + "s/" + repoID
}

// RepoInfo is the subset of the real Hub's ModelInfo/DatasetInfo/SpaceInfo
// JSON shape this package reads — see internal/hubserver's
// repoInfoResponse (the shim's own, deliberately narrower, mirror of this
// same shape) for why sha is the one field that actually matters
// downstream.
type RepoInfo struct {
	ID  string `json:"id"`
	SHA string `json:"sha"`
}

// RepoInfo calls GET /api/{repo_type}s/{repo_id}/revision/{revision} —
// resolves revision (a branch/tag name or already a commit hash) against
// the real Hub. Returns a *StatusError wrapping the real upstream status
// (404 with X-Error-Code: RevisionNotFound for an unknown revision) on
// failure.
func (c *Client) RepoInfo(ctx context.Context, cred auth.CredentialHelper, repoType, repoID, revision string) (*RepoInfo, error) {
	reqURL := c.HubBaseURL + "/api/" + repoPath(repoType, repoID) + "/revision/" + url.PathEscape(revision)
	resp, err := c.do(ctx, cred, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, newStatusError(http.MethodGet, reqURL, resp)
	}
	defer resp.Body.Close()
	var info RepoInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("decode repo info: %w", err)
	}
	return &info, nil
}

// TreeEntry mirrors one element of the real Hub's tree-listing response —
// see internal/hubserver/tree.go's treeEntry (the shim's own mirror of
// this exact shape, confirmed against huggingface_hub's RepoFile
// deserialization).
type TreeEntry struct {
	Type    string `json:"type"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	OID     string `json:"oid"`
	XetHash string `json:"xetHash,omitempty"`
}

// ListTree calls GET /api/{repo_type}s/{repo_id}/tree/{revision},
// recursive=true — enumerates every file in the repo at revision in one
// call. The real endpoint paginates (GitHub-style Link headers); this
// follows every page before returning, matching huggingface_hub's own
// paginate() helper (see internal/hubserver/tree.go's doc comment).
func (c *Client) ListTree(ctx context.Context, cred auth.CredentialHelper, repoType, repoID, revision string) ([]TreeEntry, error) {
	reqURL := c.HubBaseURL + "/api/" + repoPath(repoType, repoID) + "/tree/" + url.PathEscape(revision) + "?recursive=true"
	var entries []TreeEntry
	for reqURL != "" {
		resp, err := c.do(ctx, cred, http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, newStatusError(http.MethodGet, reqURL, resp)
		}
		var page []TreeEntry
		decodeErr := json.NewDecoder(resp.Body).Decode(&page)
		next := nextPageURL(resp)
		resp.Body.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("decode tree page: %w", decodeErr)
		}
		entries = append(entries, page...)
		reqURL = next
	}
	return entries, nil
}

// nextPageURL extracts the "next" relation from a GitHub-style Link
// response header (RFC 8288), or "" if there is no next page. Mirrors
// huggingface_hub's own _get_next_page (utils/_pagination.py).
func nextPageURL(resp *http.Response) string {
	for _, part := range strings.Split(resp.Header.Get("Link"), ",") {
		m := linkHeaderRE.FindStringSubmatch(part)
		if m != nil && m[2] == "next" {
			return m[1]
		}
	}
	return ""
}

// linkHeaderRE matches one RFC 8288 Link header entry of the form
// `<url>; rel="name"` (whitespace-tolerant) — the only shape this
// project needs to parse, since it only ever looks for the "next"
// relation.
var linkHeaderRE = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="([^"]+)"`)

// XetTokenKind selects which of the two functionally-identical
// xet-{read,write}-token endpoints to call — see internal/hubserver's
// xetTokenType (the shim's own copy of this same distinction).
type XetTokenKind int

const (
	XetTokenRead XetTokenKind = iota
	XetTokenWrite
)

func (k XetTokenKind) segment() string {
	if k == XetTokenWrite {
		return "xet-write-token"
	}
	return "xet-read-token"
}

// XetToken is the real Hub's xet-{read,write}-token response body —
// mirrors internal/hubserver's xetTokenResponse (the shim's own copy of
// this same shape, confirmed against xet-core's CasJWTInfo — see
// docs/PROTOCOL.md section 6). CasURL is the real upstream CAS base URL
// for this repo/revision: this is the ONE place this package learns it —
// see the package doc comment.
type XetToken struct {
	CasURL      string `json:"casUrl"`
	Exp         int64  `json:"exp"`
	AccessToken string `json:"accessToken"`
}

// GetXetToken calls GET /api/{repo_type}s/{repo_id}/xet-{read,write}-token/{revision}.
// The returned XetToken.CasURL is the real upstream CAS base URL to use
// for every subsequent CAS call (xorb/chunk fetch, reconstruction) against
// this repo/revision — see NewCASClient.
func (c *Client) GetXetToken(ctx context.Context, cred auth.CredentialHelper, repoType, repoID, revision string, kind XetTokenKind) (*XetToken, error) {
	reqURL := c.HubBaseURL + "/api/" + repoPath(repoType, repoID) + "/" + kind.segment() + "/" + url.PathEscape(revision)
	resp, err := c.do(ctx, cred, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, newStatusError(http.MethodGet, reqURL, resp)
	}
	defer resp.Body.Close()
	var tok XetToken
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return nil, fmt.Errorf("decode xet token: %w", err)
	}
	return &tok, nil
}

// ResolveInfo is the subset of resolve/HEAD response headers this package
// reads — mirrors internal/hubserver's handleResolve response headers
// (the shim's own copy of this same contract).
type ResolveInfo struct {
	XetHash         string
	LinkedSize      int64
	ETag            string
	RepoCommit      string
	XetRefreshRoute string
}

// Resolve calls HEAD /{repo_id}/resolve/{revision}/{filename} — the real
// Hub's per-file metadata endpoint; huggingface_hub's HEAD call is what
// triggers the Xet download path (see internal/hubserver/resolve.go's doc
// comment for the exact header contract this mirrors).
func (c *Client) Resolve(ctx context.Context, cred auth.CredentialHelper, repoID, revision, filename string) (*ResolveInfo, error) {
	reqURL := c.HubBaseURL + "/" + repoID + "/resolve/" + url.PathEscape(revision) + "/" + filename
	resp, err := c.do(ctx, cred, http.MethodHead, reqURL, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, newStatusError(http.MethodHead, reqURL, resp)
	}
	defer resp.Body.Close()
	size, _ := parseInt64(resp.Header.Get("X-Linked-Size"))
	return &ResolveInfo{
		XetHash:         resp.Header.Get("X-Xet-Hash"),
		LinkedSize:      size,
		ETag:            resp.Header.Get("ETag"),
		RepoCommit:      resp.Header.Get("X-Repo-Commit"),
		XetRefreshRoute: resp.Header.Get("X-Xet-Refresh-Route"),
	}, nil
}

func parseInt64(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

// CreateRepo calls POST /api/repos/create — idempotent on the real Hub
// the same way it is on this project's own hubserver shim.
func (c *Client) CreateRepo(ctx context.Context, cred auth.CredentialHelper, name, organization, repoType string) error {
	body, err := json.Marshal(map[string]string{"name": name, "organization": organization, "type": repoType})
	if err != nil {
		return err
	}
	reqURL := c.HubBaseURL + "/api/repos/create"
	resp, err := c.do(ctx, cred, http.MethodPost, reqURL, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return newStatusError(http.MethodPost, reqURL, resp)
	}
	resp.Body.Close()
	return nil
}

// CreateBranch calls POST /api/{repo_type}s/{repo_id}/branch/{branch} —
// idempotent (real create_branch(exist_ok=True) semantics; see
// internal/hubserver's handleCreateBranch, the shim's own copy of this
// same contract, and CHANGELOG.md's v0.8.0 entry on why the caller side
// of this call needs X-Error-Code: RevisionNotFound to trigger it in the
// first place).
func (c *Client) CreateBranch(ctx context.Context, cred auth.CredentialHelper, repoType, repoID, branch string) error {
	reqURL := c.HubBaseURL + "/api/" + repoPath(repoType, repoID) + "/branch/" + url.PathEscape(branch)
	resp, err := c.do(ctx, cred, http.MethodPost, reqURL, strings.NewReader("{}"))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return newStatusError(http.MethodPost, reqURL, resp)
	}
	resp.Body.Close()
	return nil
}

// CommitFile is one file entry proxyhub relays upstream as part of a
// commit — mirrors internal/hubserver's commitLine.Value shape.
type CommitFile struct {
	Path string `json:"path"`
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

// CommitResult is POST .../commit/{revision}'s response body — mirrors
// internal/hubserver's commitResponse.
type CommitResult struct {
	CommitOID string `json:"commitOid"`
	CommitURL string `json:"commitUrl"`
}

// Commit calls POST /api/{repo_type}s/{repo_id}/commit/{revision} with an
// ndjson body of lfsFile entries — the write-through path proxyhub uses
// to relay a caller's `hf upload` commit to the real Hub immediately,
// never served from cache.
func (c *Client) Commit(ctx context.Context, cred auth.CredentialHelper, repoType, repoID, revision string, files []CommitFile) (*CommitResult, error) {
	var body strings.Builder
	for _, f := range files {
		line, err := json.Marshal(map[string]any{"key": "lfsFile", "value": f})
		if err != nil {
			return nil, err
		}
		body.Write(line)
		body.WriteByte('\n')
	}
	reqURL := c.HubBaseURL + "/api/" + repoPath(repoType, repoID) + "/commit/" + url.PathEscape(revision)
	resp, err := c.do(ctx, cred, http.MethodPost, reqURL, strings.NewReader(body.String()))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, newStatusError(http.MethodPost, reqURL, resp)
	}
	defer resp.Body.Close()
	var result CommitResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode commit result: %w", err)
	}
	return &result, nil
}

// PreuploadFile is one file entry in a preupload request/response —
// mirrors internal/hubserver's preuploadRequest/preuploadFileInfo shapes.
type PreuploadFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// PreuploadResult is one file's negotiated upload mode — mirrors
// hubserver.preuploadFileInfo's full shape. ShouldIgnore must round-trip
// even though this project's own hubserver always sets it false: real
// huggingface_hub's _fetch_upload_modes reads file["shouldIgnore"]
// unconditionally with no default, so an upstream response missing it
// (e.g. relayed through a decode step that dropped the field) crashes
// the real hf CLI with a KeyError rather than a clean error from this
// project's own code. OID is read via file.get("oid") (a safe default
// of None), so it can't cause the same crash, but is still included for
// completeness.
type PreuploadResult struct {
	Path         string `json:"path"`
	UploadMode   string `json:"uploadMode"`
	ShouldIgnore bool   `json:"shouldIgnore"`
	OID          string `json:"oid,omitempty"`
}

// Preupload calls POST /api/{repo_type}s/{repo_id}/preupload/{revision} —
// negotiates per-file upload mode with the real Hub before a commit.
func (c *Client) Preupload(ctx context.Context, cred auth.CredentialHelper, repoType, repoID, revision string, files []PreuploadFile) ([]PreuploadResult, error) {
	body, err := json.Marshal(map[string]any{"files": files})
	if err != nil {
		return nil, err
	}
	reqURL := c.HubBaseURL + "/api/" + repoPath(repoType, repoID) + "/preupload/" + url.PathEscape(revision)
	resp, err := c.do(ctx, cred, http.MethodPost, reqURL, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, newStatusError(http.MethodPost, reqURL, resp)
	}
	defer resp.Body.Close()
	var result struct {
		Files []PreuploadResult `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode preupload result: %w", err)
	}
	return result.Files, nil
}
