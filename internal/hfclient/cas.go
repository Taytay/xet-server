// cas.go: the CAS-side half of hfclient — talks to the real Xet CAS
// server whose base URL is discovered per-repo via Client.GetXetToken
// (see the package doc comment). Kept in its own type (CASClient, not a
// method set on Client) since CAS is architecturally a separate service
// from the Hub even in the real deployment this package talks to.
package hfclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"xet-server/internal/auth"
)

// CASClient is an upstream HTTP client for the real Xet CAS API at
// BaseURL (as returned by a Hub xet-token response's CasURL field — see
// NewCASClient).
type CASClient struct {
	BaseURL string
	HTTP    *http.Client
}

// NewCASClient returns a CASClient targeting baseURL — normally a Hub
// xet-token response's CasURL, never a value this package invents or
// defaults on its own (see the package doc comment on why there is no
// DefaultCASURL constant paralleling DefaultHubURL).
func NewCASClient(baseURL string) *CASClient {
	return &CASClient{BaseURL: baseURL, HTTP: defaultHTTPClient}
}

// httpClient returns c.HTTP, or defaultHTTPClient (see hfclient.go) if
// unset — lets a zero-value CASClient still work, and still get the
// same connect/handshake/response-header timeout bounds as a Client
// built via NewCASClient, rather than silently falling back to
// http.DefaultClient's unbounded behavior.
func (c *CASClient) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return defaultHTTPClient
}

func (c *CASClient) casDo(ctx context.Context, cred auth.CredentialHelper, method, url string, headers map[string]string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if cred != nil {
		if err := cred.FillCredential(req); err != nil {
			return nil, fmt.Errorf("fill credential: %w", err)
		}
	}
	return c.httpClient().Do(req)
}

// FetchXorb calls GET /v1/xorbs/{prefix}/{hash}, forwarding rangeHeader
// (the exact Range header value the downstream caller sent, or "" for
// none) unchanged — proxycas is a byte cache, not a re-verifying client,
// so this returns the response body and status exactly as the real CAS
// server sent them (200 or 206) for the caller to stream/store as-is,
// the same trust model casserver's own xorb-fetch path already uses for
// locally-stored bytes (see internal/casserver/xorbs.go).
//
// The caller MUST close the returned response body, even on a non-2xx
// status (that path is not treated as an error here — see the doc
// comment on why: unlike the Hub-side calls in hfclient.go, a 404/416
// here is exactly what proxycas needs to relay to its own caller
// unchanged, not something to collapse into a *StatusError).
func (c *CASClient) FetchXorb(ctx context.Context, cred auth.CredentialHelper, prefix, hash, rangeHeader string) (*http.Response, error) {
	reqURL := c.BaseURL + "/v1/xorbs/" + url.PathEscape(prefix) + "/" + url.PathEscape(hash)
	var headers map[string]string
	if rangeHeader != "" {
		headers = map[string]string{"Range": rangeHeader}
	}
	return c.casDo(ctx, cred, http.MethodGet, reqURL, headers, nil)
}

// HeadXorb calls HEAD /v1/xorbs/{prefix}/{hash} — a real upstream HEAD,
// not a full GET with the body discarded: proxycas's own -no-cache HEAD
// path uses this to report a xorb's size without transferring its bytes
// at all, matching -no-cache's "no local storage read/write" contract
// without the bandwidth waste a GET-and-discard would cost. Same
// caller-closes-body, non-2xx-is-not-an-error contract as FetchXorb.
func (c *CASClient) HeadXorb(ctx context.Context, cred auth.CredentialHelper, prefix, hash string) (*http.Response, error) {
	reqURL := c.BaseURL + "/v1/xorbs/" + url.PathEscape(prefix) + "/" + url.PathEscape(hash)
	return c.casDo(ctx, cred, http.MethodHead, reqURL, nil, nil)
}

// FetchChunkDedup calls GET /v1/chunks/{prefix}/{hash} — the global
// chunk-dedup lookup, returning the raw bytes of whichever shard
// referenced this chunk hash (see internal/casserver/reconstruction.go's
// handleChunkDedup doc comment for the wire contract this mirrors). Same
// caller-closes-body, non-2xx-is-not-an-error contract as FetchXorb.
func (c *CASClient) FetchChunkDedup(ctx context.Context, cred auth.CredentialHelper, prefix, hash string) (*http.Response, error) {
	reqURL := c.BaseURL + "/v1/chunks/" + url.PathEscape(prefix) + "/" + url.PathEscape(hash)
	return c.casDo(ctx, cred, http.MethodGet, reqURL, nil, nil)
}

// FetchReconstruction calls GET /v1/reconstructions/{file_id} (or
// /v2/... if v2 is true), forwarding rangeHeader unchanged. Returns the
// raw response for the caller (internal/proxycas) to relay downstream
// and cache. IMPORTANT: real production Xet's fetch_info/xorbs URLs are
// presigned S3 URLs pointing directly at blob storage, not at this CAS
// server — a caller that relays this response unmodified lets the
// downstream client fetch bytes straight from S3, bypassing the proxy's
// cache entirely. proxycas handles this not by rewriting the decoded
// response itself, but by feeding its terms into its embedded
// casserver.Server via IngestFileRecon and then delegating: casserver's
// own reconstruction handler always emits fetch URLs pointing at
// itself, so the rewrite happens as a side effect of using the real
// server as the serving engine rather than needing a rewrite step of
// its own. This package intentionally does not do any such rewriting
// itself — it's a proxycas/casserver concern, not an upstream-client
// one. Same caller-closes-body, non-2xx-is-not-an-error contract as
// FetchXorb.
func (c *CASClient) FetchReconstruction(ctx context.Context, cred auth.CredentialHelper, fileID, rangeHeader string, v2 bool) (*http.Response, error) {
	version := "v1"
	if v2 {
		version = "v2"
	}
	reqURL := c.BaseURL + "/" + version + "/reconstructions/" + url.PathEscape(fileID)
	var headers map[string]string
	if rangeHeader != "" {
		headers = map[string]string{"Range": rangeHeader}
	}
	return c.casDo(ctx, cred, http.MethodGet, reqURL, headers, nil)
}

// UploadXorb calls POST /v1/xorbs/{prefix}/{hash} — the write-through
// path proxycas uses to relay a caller's xorb upload to the real CAS
// server immediately (uploads are never served from or held back by the
// local cache; they're also opportunistically written to it on the way
// through so a download right after the caller's own upload is a local
// hit — see internal/proxycas).
func (c *CASClient) UploadXorb(ctx context.Context, cred auth.CredentialHelper, prefix, hash string, body io.Reader) (*http.Response, error) {
	reqURL := c.BaseURL + "/v1/xorbs/" + url.PathEscape(prefix) + "/" + url.PathEscape(hash)
	return c.casDo(ctx, cred, http.MethodPost, reqURL, nil, body)
}

// UploadShard calls POST /v1/shards.
func (c *CASClient) UploadShard(ctx context.Context, cred auth.CredentialHelper, body io.Reader) (*http.Response, error) {
	reqURL := c.BaseURL + "/v1/shards"
	return c.casDo(ctx, cred, http.MethodPost, reqURL, nil, body)
}
