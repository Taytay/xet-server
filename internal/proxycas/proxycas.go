// Package proxycas implements the CAS-facing side of a caching
// pull-through proxy for the real Xet CAS backend, by EMBEDDING a real
// internal/casserver.Server as its serving engine rather than
// reimplementing byte-range serving, reconstruction-response building,
// footer/shard indexing, or snapshotting independently - every one of
// those already exists, is already tested, and this package's whole
// job is to keep it filled with data fetched from upstream, not to grow
// a second copy of the same logic that has to be kept in sync by hand.
//
// For each request, this package's job is: check whether the embedded
// Server already has what's needed (via its Has*/Ingest* API - see
// casserver.Server's own doc comments on IngestXorb/IngestShard/
// IngestFileRecon/HasXorbFooter/HasXorbBytes/HasFileRecon); if so,
// delegate straight to embedded.ServeHTTP with no upstream call at all -
// this is what makes anything already cached fully offline-capable. On
// a miss, fetch the missing piece from upstream (via internal/hfclient's
// CASClient, discovered per-repo through internal/proxyhub's own
// xet-token handling - see WithCASClient), feed it into the embedded
// Server through Ingest*, then delegate.
//
// Reconstruction is the one place this needs real care: real production
// Xet's fetch_info/xorbs URLs are presigned URLs pointing directly at
// blob storage, not at the CAS server itself - casserver's own
// xorbFetchURL always returns its own byte-serving endpoint (it has no
// notion of a presigned upstream URL), so simply feeding an upstream
// reconstruction's raw xorb hashes into IngestFileRecon and then
// delegating to embedded.ServeHTTP is already correct: the embedded
// server's own reconstruction handler emits URLs pointing at ITSELF,
// which is exactly the rewriting proxycas needs, for free. The one
// upstream Range-header subtlety still applies here (see
// ensureFileReconCached's doc comment): a ranged upstream response only
// contains that window's terms, so the FIRST fetch for a file must
// always be for the whole thing, regardless of what the downstream
// caller actually asked for - IngestFileRecon assumes a complete list.
//
// -no-cache disables all of the above: every request is relayed live to
// upstream via internal/hfclient directly, embedded is never touched.
package proxycas

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"

	"github.com/guilt/xet-server/internal/auth"
	"github.com/guilt/xet-server/internal/casserver"
	"github.com/guilt/xet-server/internal/hfclient"
	"github.com/guilt/xet-server/internal/merklehash"
	"github.com/guilt/xet-server/internal/ratelimit"
	"github.com/guilt/xet-server/internal/reconwire"
	"github.com/guilt/xet-server/internal/shardformat"
	"github.com/guilt/xet-server/internal/storage"
)

// xorbPrefix/chunkDedupPrefix match casserver's own (unexported)
// constants of the same name/value - duplicated here only because this
// package needs them to validate a path segment before delegating,
// same as casserver's own handlers do.
const (
	xorbPrefix       = "default"
	chunkDedupPrefix = "default-merkledb"
)

// Server wraps an embedded *casserver.Server, filling it from upstream
// on demand. See the package doc comment for the overall design.
type Server struct {
	// Embedded is the real serving engine - exported so a caller (e.g.
	// cmd/xet-proxyd) can call its own Snapshot/LoadSnapshot directly;
	// this package never needs its own snapshot format, since it has no
	// state of its own beyond what Embedded already tracks.
	Embedded *casserver.Server

	mux *http.ServeMux

	// NoCache disables all caching (the -no-cache flag's effect): every
	// request is relayed live to upstream via internal/hfclient, and
	// Embedded is never read or written. Off by default.
	NoCache bool

	// authenticator mirrors whatever was last passed to SetAuthenticator
	// (which also forwards it to Embedded, so casserver's own handlers
	// stay correctly gated once this package delegates to them). This
	// package's OWN handlers need their own copy checked up front,
	// before doing any upstream fetch/ingest - an unauthenticated
	// request must never trigger an upstream call or write to Embedded's
	// storage as a side effect of getting all the way to Embedded's own
	// (equally strict) check at the point of delegation.
	authenticator auth.Authenticator

	// rateLimiter, if set via SetRateLimiter, gates every route in this
	// package by source IP - nil (the default) means unlimited. Unlike
	// casserver's own upload-only rate limiter (uploads are the
	// expensive local operation there: chunk decompression + hashing),
	// EVERY route here can trigger a real outbound call to upstream on
	// a cache miss, not just writes - a read-heavy client hammering
	// this proxy costs it (and the real huggingface.co behind it) just
	// as much as a write-heavy one. See SetRateLimiter's doc comment.
	rateLimiter *ratelimit.Limiter
}

// New creates a Server backed by xorbs for the embedded casserver.Server's
// local storage.
func New(xorbs storage.Store) *Server {
	s := &Server{
		Embedded:      casserver.New(xorbs),
		mux:           http.NewServeMux(),
		authenticator: auth.NoAuth{},
	}
	s.routes()
	return s
}

// SetAuthenticator gates this package's own handlers (see
// authenticator's doc comment on why this package needs its own copy)
// AND forwards to the embedded casserver.Server, so casserver's own
// scope checks - the ones that actually run once a request is
// delegated - stay in sync.
func (s *Server) SetAuthenticator(a auth.Authenticator) {
	s.authenticator = a
	s.Embedded.SetAuthenticator(a)
}

// SetRateLimiter installs a per-source-IP rate limiter in front of
// EVERY route this package serves (see rateLimiter's doc comment on why
// that's broader than casserver.Server.SetUploadRateLimiter's
// upload-only scope). Unlike casserver's own SetUploadRateLimiter, this
// never rebuilds the mux - gate reads s.rateLimiter fresh on every
// request, so a later call takes effect immediately, matching this
// package's own SetAuthenticator/requireScope contract exactly. Does
// NOT forward to Embedded: casserver's own upload rate limiter is a
// distinct, narrower concern (guarding its own local CPU cost) that a
// caller wanting both should configure separately via
// s.Embedded.SetUploadRateLimiter.
func (s *Server) SetRateLimiter(l *ratelimit.Limiter) {
	s.rateLimiter = l
}

// requireScope authenticates r against s.authenticator and checks for
// scope, writing a 401/403 and returning false if the request should
// not proceed to any upstream fetch/ingest. Every handler in this
// package calls this before doing anything else - see authenticator's
// doc comment for why the check has to happen here too, not only once
// (and later) at the point of delegating to Embedded.
func (s *Server) requireScope(w http.ResponseWriter, r *http.Request, scope auth.Scope) bool {
	principal, err := s.authenticator.Authenticate(r)
	if err != nil {
		if errors.Is(err, auth.ErrUnauthenticated) {
			httpError(w, "unauthenticated: "+err.Error(), http.StatusUnauthorized)
		} else {
			httpError(w, "authentication failed: "+err.Error(), http.StatusForbidden)
		}
		return false
	}
	if !principal.HasScope(scope) {
		httpError(w, "principal lacks required scope: "+string(scope), http.StatusForbidden)
		return false
	}
	return true
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// xorbsPath/etc. are proxycas's own copies of casserver's path
// constants, used only to build this package's own route table below -
// unexported since no caller outside this package references them.
const (
	xorbsPath             = casserver.XorbsPath
	shardsPath            = casserver.ShardsPath
	reconstructionsPath   = casserver.ReconstructionsPath
	reconstructionsPathV2 = casserver.ReconstructionsPathV2
	chunksPath            = casserver.ChunksPath
)

func (s *Server) routes() {
	mux := http.NewServeMux()
	mux.Handle("GET "+xorbsPath, s.gate(auth.ScopeRead, s.handleFetchXorb))
	mux.Handle("HEAD "+xorbsPath, s.gate(auth.ScopeRead, s.handleHeadXorb))
	mux.Handle("POST "+xorbsPath, s.gate(auth.ScopeWrite, s.handleUploadXorb))
	mux.Handle("POST "+shardsPath, s.gate(auth.ScopeWrite, s.handleUploadShard))
	mux.Handle("GET "+reconstructionsPath, s.gate(auth.ScopeRead, s.handleReconstruction))
	mux.Handle("GET "+reconstructionsPathV2, s.gate(auth.ScopeRead, s.handleReconstructionV2))
	mux.Handle("GET "+chunksPath, s.gate(auth.ScopeRead, s.handleChunkDedup))
	s.mux = mux
}

// gate wraps next so it only runs once requireScope passes and (if a
// rate limiter is configured) the per-source-IP rate limit allows it -
// same order as casserver.Server.routes' own upload-limiter wrapping
// (auth first, then rate limiting), so an unauthenticated flood never
// gets to consume another client's rate-limit budget by sharing its
// source IP. Reads s.authenticator/s.rateLimiter fresh on every request
// (not captured at routes() time), so a later SetAuthenticator/
// SetRateLimiter call takes effect immediately with no mux rebuild
// needed.
func (s *Server) gate(scope auth.Scope, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.requireScope(w, r, scope) {
			return
		}
		if s.rateLimiter != nil {
			s.rateLimiter.Middleware(http.HandlerFunc(next)).ServeHTTP(w, r)
			return
		}
		next(w, r)
	}
}

// casClientContextKey is how internal/proxyhub hands this request's
// per-repo upstream CASClient down to this package's handlers - set via
// WithCASClient before the request reaches this server's mux.
type casClientContextKey struct{}

// WithCASClient returns a copy of ctx carrying casClient, for handlers in
// this package to read via casClientFromContext. internal/proxyhub calls
// this (after resolving the repo's CAS endpoint via a Hub xet-token
// request) before forwarding a request into this server's ServeHTTP.
func WithCASClient(ctx context.Context, casClient *hfclient.CASClient) context.Context {
	return context.WithValue(ctx, casClientContextKey{}, casClient)
}

func casClientFromContext(r *http.Request) (*hfclient.CASClient, error) {
	c, ok := r.Context().Value(casClientContextKey{}).(*hfclient.CASClient)
	if !ok || c == nil {
		return nil, fmt.Errorf("proxycas: no upstream CAS client in request context (caller must set one via WithCASClient)")
	}
	return c, nil
}

// httpError writes a plain-text HTTP error, logging it at a level
// matching its cause - same convention as casserver's own httpError: a
// 5xx (this proxy's own fault, or an upstream failure it's relaying) is
// worth surfacing at Warn by default, while a 4xx (a client protocol/
// input error, expected under normal operation) logs at Debug only, so
// it doesn't spam the default log level.
func httpError(w http.ResponseWriter, msg string, code int) {
	if code >= 500 {
		slog.Warn("proxycas: request failed", "status", code, "error", msg)
	} else {
		slog.Debug("proxycas: request rejected", "status", code, "error", msg)
	}
	http.Error(w, msg, code)
}

// upstreamStatusError wraps a non-2xx status from an upstream CAS fetch
// so writeFetchError can classify it: a 404 means "upstream doesn't have
// it either" (relay as 404); anything else is an upstream/network fault
// (502).
type upstreamStatusError struct {
	Status int
	Body   string
}

func (e *upstreamStatusError) Error() string {
	return fmt.Sprintf("upstream status %d: %s", e.Status, e.Body)
}

// writeFetchError classifies err from an upstream CAS fetch: a
// *upstreamStatusError carries the real upstream status, relayed as-is
// (a 404 means "upstream doesn't have it either"; a 401/403 means the
// caller's own credential was rejected upstream - either way, the real
// status is more useful downstream than a blanket 502, and matches
// proxyhub.writeUpstreamError's identical relay-4xx-as-is policy for the
// same class of failure). Anything else (a network/dial/timeout failure,
// with no real upstream status at all) is a 502.
func writeFetchError(w http.ResponseWriter, r *http.Request, action string, err error) {
	var statusErr *upstreamStatusError
	if errors.As(err, &statusErr) {
		if statusErr.Status == http.StatusNotFound {
			http.NotFound(w, r)
			return
		}
		if statusErr.Status >= 400 && statusErr.Status < 500 {
			httpError(w, action+": "+err.Error(), statusErr.Status)
			return
		}
	}
	httpError(w, action+": "+err.Error(), http.StatusBadGateway)
}

// relayResponse copies resp's status, headers, and body verbatim to w -
// used by every -no-cache code path, and by any handler relaying a
// non-2xx upstream response unchanged.
func relayResponse(w http.ResponseWriter, resp *http.Response) {
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// --- xorb byte serving -------------------------------------------------

// maxXorbBytes matches casserver's own cap.
const maxXorbBytes = 128 * 1024 * 1024

// handleFetchXorb implements GET /v1/xorbs/{prefix}/{hash}: serve from
// the embedded server if it already has hash's bytes; otherwise fetch
// the full xorb from upstream, ingest it (unless NoCache), and delegate.
func (s *Server) handleFetchXorb(w http.ResponseWriter, r *http.Request) {
	prefix := r.PathValue("prefix")
	hexHash := r.PathValue("hash")
	if prefix != xorbPrefix {
		httpError(w, "unsupported xorb prefix", http.StatusBadRequest)
		return
	}
	hash, err := merklehash.FromHex(hexHash)
	if err != nil {
		httpError(w, "invalid hash", http.StatusBadRequest)
		return
	}

	if s.NoCache {
		s.relayXorbLive(w, r, prefix, hexHash)
		return
	}

	if err := s.ensureXorbCached(r, hash); err != nil {
		writeFetchError(w, r, "fetch xorb", err)
		return
	}
	s.Embedded.ServeHTTP(w, r)
}

// handleHeadXorb implements HEAD /v1/xorbs/{prefix}/{hash}: same
// cache-or-fetch as handleFetchXorb; under -no-cache, relays a real
// upstream HEAD instead (see relayHeadXorbLive) rather than
// fetch-and-discarding a full GET or writing to local storage.
func (s *Server) handleHeadXorb(w http.ResponseWriter, r *http.Request) {
	prefix := r.PathValue("prefix")
	hexHash := r.PathValue("hash")
	if prefix != xorbPrefix {
		httpError(w, "unsupported xorb prefix", http.StatusBadRequest)
		return
	}
	hash, err := merklehash.FromHex(hexHash)
	if err != nil {
		httpError(w, "invalid hash", http.StatusBadRequest)
		return
	}

	if s.NoCache {
		s.relayHeadXorbLive(w, r, prefix, hexHash)
		return
	}

	if err := s.ensureXorbCached(r, hash); err != nil {
		writeFetchError(w, r, "head xorb", err)
		return
	}
	s.Embedded.ServeHTTP(w, r)
}

// handleUploadXorb implements POST /v1/xorbs/{prefix}/{hash}: relay the
// upload to upstream write-through, then (unless NoCache) also ingest
// the same bytes into the embedded server so a download immediately
// after this upload is already a local hit.
func (s *Server) handleUploadXorb(w http.ResponseWriter, r *http.Request) {
	prefix := r.PathValue("prefix")
	hexHash := r.PathValue("hash")
	if prefix != xorbPrefix {
		httpError(w, "unsupported xorb prefix", http.StatusBadRequest)
		return
	}
	hash, err := merklehash.FromHex(hexHash)
	if err != nil {
		httpError(w, "invalid hash", http.StatusBadRequest)
		return
	}
	cas, err := casClientFromContext(r)
	if err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cred := auth.CredentialFromRequest(r)

	r.Body = http.MaxBytesReader(w, r.Body, maxXorbBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		httpError(w, "read body (exceeds max xorb size or connection error): "+err.Error(), http.StatusRequestEntityTooLarge)
		return
	}

	resp, err := cas.UploadXorb(r.Context(), cred, prefix, hexHash, bytes.NewReader(body))
	if err != nil {
		httpError(w, "upload to upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusOK && !s.NoCache {
		// A failed ingest is non-fatal here: the upload to upstream
		// already succeeded (the caller's actual request), and a future
		// fetch of this xorb falls back to fetching it from upstream
		// again - the same as any other as-yet-uncached xorb. Still
		// logged at Warn: a silently failing write-through would leave
		// this proxy quietly serving every subsequent read for this xorb
		// from upstream instead of cache, indistinguishable from working
		// correctly until something inspects the logs.
		if _, err := s.Embedded.IngestXorb(r.Context(), hash, bytes.NewReader(body)); err != nil {
			slog.Warn("proxycas: write-through xorb ingest failed (upload to upstream already succeeded)", "hash", hash.Hex(), "error", err)
		}
	}

	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

// handleChunkDedup implements GET /v1/chunks/{prefix}/{hash}: always
// relayed live - shard bytes have no stable reusable cache key this
// package computes, and casserver's own chunk-dedup index is populated
// only as a side effect of a real shard upload/ingest, which this
// package never receives one of independent from a real client's own
// upload (see handleUploadShard).
func (s *Server) handleChunkDedup(w http.ResponseWriter, r *http.Request) {
	prefix := r.PathValue("prefix")
	hexHash := r.PathValue("hash")
	if prefix != chunkDedupPrefix {
		httpError(w, "unsupported chunk-dedup prefix", http.StatusBadRequest)
		return
	}
	cas, err := casClientFromContext(r)
	if err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp, err := cas.FetchChunkDedup(r.Context(), auth.CredentialFromRequest(r), prefix, hexHash)
	if err != nil {
		httpError(w, "fetch chunk-dedup from upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	relayResponse(w, resp)
}

// relayXorbLive fetches hash directly from upstream and streams the
// response straight to w - the -no-cache code path for GET.
func (s *Server) relayXorbLive(w http.ResponseWriter, r *http.Request, prefix, hexHash string) {
	cas, err := casClientFromContext(r)
	if err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp, err := cas.FetchXorb(r.Context(), auth.CredentialFromRequest(r), prefix, hexHash, r.Header.Get("Range"))
	if err != nil {
		httpError(w, "fetch xorb from upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	relayResponse(w, resp)
}

// relayHeadXorbLive is relayXorbLive's HEAD counterpart - see
// hfclient.CASClient.HeadXorb's doc comment for why this issues a real
// upstream HEAD instead of a GET.
func (s *Server) relayHeadXorbLive(w http.ResponseWriter, r *http.Request, prefix, hexHash string) {
	cas, err := casClientFromContext(r)
	if err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp, err := cas.HeadXorb(r.Context(), auth.CredentialFromRequest(r), prefix, hexHash)
	if err != nil {
		httpError(w, "head xorb from upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	relayResponse(w, resp)
}

// ensureXorbCached ensures hash's bytes and footer are present in the
// embedded server, fetching+ingesting from upstream on a miss.
func (s *Server) ensureXorbCached(r *http.Request, hash merklehash.Hash) error {
	if s.Embedded.HasXorbFooter(hash) {
		if has, err := s.Embedded.HasXorbBytes(r.Context(), hash); err == nil && has {
			return nil
		}
	}
	cas, err := casClientFromContext(r)
	if err != nil {
		return err
	}
	return s.fetchAndIngestXorb(r.Context(), cas, auth.CredentialFromRequest(r), hash)
}

func (s *Server) fetchAndIngestXorb(ctx context.Context, cas *hfclient.CASClient, cred auth.CredentialHelper, hash merklehash.Hash) error {
	resp, err := cas.FetchXorb(ctx, cred, xorbPrefix, hash.Hex(), "")
	if err != nil {
		return fmt.Errorf("fetch xorb %s from upstream: %w", hash.Hex(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &upstreamStatusError{Status: resp.StatusCode, Body: string(body)}
	}
	if _, err := s.Embedded.IngestXorb(ctx, hash, resp.Body); err != nil {
		return fmt.Errorf("ingest xorb %s: %w", hash.Hex(), err)
	}
	return nil
}

// errPartialXorbCoverage reports that a reconstruction's fetch_info
// covers only PART of the xorb it references, so the whole xorb can never
// be assembled (let alone hash-verified) from it.
//
// This is a normal, expected condition, not a failure: a xorb packs chunks
// from many files, and a given file's reconstruction only ever cites the
// chunk ranges THAT file needs. A file using chunks 15-21 of a 1000-chunk
// xorb gets exactly one fetch_info entry covering those bytes. The
// presigned URL in that entry is scoped by signature to precisely that
// byte range - requesting the full object returns 403 - so there is no way
// to widen it into the complete xorb the content-addressed store requires.
// Callers treat this as "this xorb isn't cacheable from this particular
// reconstruction" and fall back to relaying upstream's response, letting
// the client fetch those bytes straight from the CDN.
var errPartialXorbCoverage = errors.New("proxycas: reconstruction fetch_info covers only part of the xorb")

// cacheXorbFromFetchInfo ensures hash's bytes and footer are present in
// the embedded server, fetching the xorb's bytes via the presigned URLs an
// upstream reconstruction's fetch_info provides (concatenating the covered
// byte ranges in order) and ingesting on a miss. The real Xet CAS server
// does not serve xorb bodies over GET /v1/xorbs/ (its allow list is
// HEAD,POST) - the presigned URLs are the only way to get the bytes - so
// a fetch_info-bearing reconstruction must be cached this way. An upstream
// without presigned URLs (this project's own casserver, or a test
// stand-in) falls back to the direct xorb-fetch path via ensureXorbCached.
//
// Returns errPartialXorbCoverage when fetch_info doesn't span the xorb from
// chunk 0 - see that error's doc comment for why that's expected rather
// than exceptional. That case is detected BEFORE any network transfer,
// so a partially-cited xorb costs no bandwidth here at all (the previous
// version downloaded the cited ranges, concatenated them, then failed
// IngestXorb's hash check with a misleading "xorb hash in URL does not
// match hash computed from chunk contents", re-paying that transfer on
// every subsequent request for the same file).
func (s *Server) cacheXorbFromFetchInfo(r *http.Request, hash merklehash.Hash, infos []reconwire.FetchInfoEntry) error {
	if s.Embedded.HasXorbFooter(hash) {
		if has, err := s.Embedded.HasXorbBytes(r.Context(), hash); err == nil && has {
			return nil
		}
	}
	if len(infos) == 0 {
		return s.ensureXorbCached(r, hash)
	}
	// A complete xorb must be cited from its very first chunk. Anything
	// else is a mid-xorb slice that can't be completed (see
	// errPartialXorbCoverage). Cheap structural check, no I/O.
	if !citesXorbFromStart(infos) {
		return errPartialXorbCoverage
	}

	cas, err := casClientFromContext(r)
	if err != nil {
		return err
	}

	// Concatenate the covered byte ranges in ascending offset order to
	// recover the full (compressed) xorb before ingesting.
	ordered := append([]reconwire.FetchInfoEntry(nil), infos...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].URLRange.Start < ordered[j].URLRange.Start })

	var buf bytes.Buffer
	for _, info := range ordered {
		rangeHeader := fmt.Sprintf("bytes=%d-%d", info.URLRange.Start, info.URLRange.End) // both ends inclusive
		resp, err := cas.FetchPresigned(r.Context(), info.URL, rangeHeader)
		if err != nil {
			return fmt.Errorf("fetch xorb %s presigned range %s: %w", hash.Hex(), rangeHeader, err)
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			return &upstreamStatusError{Status: resp.StatusCode, Body: string(body)}
		}
		if _, err := io.Copy(&buf, resp.Body); err != nil {
			resp.Body.Close()
			return fmt.Errorf("read xorb %s presigned range %s: %w", hash.Hex(), rangeHeader, err)
		}
		resp.Body.Close()
	}

	if _, err := s.Embedded.IngestXorb(r.Context(), hash, bytes.NewReader(buf.Bytes())); err != nil {
		// A hash mismatch here means the cited ranges started at chunk 0
		// but still stopped short of the xorb's end - a partial slice
		// that merely looked complete to the structural check above.
		// Same expected condition as errPartialXorbCoverage, just only
		// detectable after hashing, so report it as such rather than as
		// a hard ingest failure.
		if errors.Is(err, casserver.ErrXorbHashMismatch) {
			return errPartialXorbCoverage
		}
		return fmt.Errorf("ingest xorb %s: %w", hash.Hex(), err)
	}
	return nil
}

// citesXorbFromStart reports whether infos covers the xorb beginning at
// its first chunk (index 0). A xorb is content-addressed over ALL its
// chunks, so a citation that starts anywhere else can never be assembled
// into the verifiable whole - see errPartialXorbCoverage.
func citesXorbFromStart(infos []reconwire.FetchInfoEntry) bool {
	for _, info := range infos {
		if info.Range.Start == 0 {
			return true
		}
	}
	return false
}

// --- shard upload --------------------------------------------------------

// maxShardBytes matches casserver's own cap.
const maxShardBytes = 512 * 1024 * 1024

// handleUploadShard implements POST /v1/shards: relay write-through to
// upstream, then (unless NoCache) also ingest the same bytes into the
// embedded server. Without this route, hf_xet's real upload flow (which
// always POSTs a shard after xorb uploads) would 404 against this proxy.
func (s *Server) handleUploadShard(w http.ResponseWriter, r *http.Request) {
	cas, err := casClientFromContext(r)
	if err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cred := auth.CredentialFromRequest(r)

	r.Body = http.MaxBytesReader(w, r.Body, maxShardBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		httpError(w, "read body (exceeds max shard size or connection error): "+err.Error(), http.StatusRequestEntityTooLarge)
		return
	}

	resp, err := cas.UploadShard(r.Context(), cred, bytes.NewReader(body))
	if err != nil {
		httpError(w, "upload to upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusOK && !s.NoCache {
		// A failed ingest is non-fatal here for the same reason as
		// handleUploadXorb: the upstream write already succeeded. Still
		// logged at Warn for the same reason.
		if err := s.Embedded.IngestShard(body); err != nil {
			slog.Warn("proxycas: write-through shard ingest failed (upload to upstream already succeeded)", "error", err)
		}
	}

	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

// --- reconstruction --------------------------------------------------------

// handleReconstruction implements GET /v1/reconstructions/{file_id}.
func (s *Server) handleReconstruction(w http.ResponseWriter, r *http.Request) {
	s.handleReconstructionCommon(w, r, false)
}

// handleReconstructionV2 implements GET /v2/reconstructions/{file_id}.
func (s *Server) handleReconstructionV2(w http.ResponseWriter, r *http.Request) {
	s.handleReconstructionCommon(w, r, true)
}

// handleReconstructionCommon serves file_id's reconstruction, at
// whatever byte range the caller's Range header requests, either
// entirely from the embedded server (once this file's complete
// reconstruction and every xorb it references are known) or by
// fetching the WHOLE file's reconstruction from upstream on a miss -
// see the package doc comment for why this is a whole-file fetch, not a
// relay of the caller's own range: a Range-limited upstream response
// only contains that window's terms, and IngestFileRecon assumes a
// complete list. Under NoCache, this is a pure relay of the caller's
// Range header and upstream's response, unmodified (URLs stay as the
// real presigned URLs).
func (s *Server) handleReconstructionCommon(w http.ResponseWriter, r *http.Request, v2 bool) {
	fileIDHex := r.PathValue("file_id")
	fileID, err := merklehash.FromHex(fileIDHex)
	if err != nil {
		httpError(w, "invalid file_id", http.StatusBadRequest)
		return
	}

	if s.NoCache {
		s.relayReconstructionLive(w, r, fileIDHex, v2)
		return
	}

	found, err := s.ensureFileReconCached(r, fileID)
	if err != nil {
		// The client only needs upstream's reconstruction response and the
		// presigned URLs inside it - it fetches xorb bodies straight from
		// the CDN, not through this proxy's CAS - so a caching failure must
		// never fail the download. Relay live and let the client proceed;
		// the local cache simply doesn't advance for this file.
		if errors.Is(err, errPartialXorbCoverage) {
			// Expected and unavoidable: this file uses only part of a
			// shared xorb, whose presigned URL is signature-scoped to
			// that slice. Debug, not Warn - it would otherwise fire for
			// a large fraction of files in any densely-packed repo.
			slog.Debug("proxycas: file not cacheable (partial xorb citation), relaying live",
				"file_id", fileIDHex)
		} else {
			slog.Warn("proxycas: reconstruction cache-ingest failed, falling back to live relay (client still downloads via presigned URLs)",
				"file_id", fileIDHex, "error", err)
		}
		s.relayReconstructionLive(w, r, fileIDHex, v2)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	s.Embedded.ServeHTTP(w, r)
}

func (s *Server) relayReconstructionLive(w http.ResponseWriter, r *http.Request, fileIDHex string, v2 bool) {
	cas, err := casClientFromContext(r)
	if err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp, err := cas.FetchReconstruction(r.Context(), auth.CredentialFromRequest(r), fileIDHex, r.Header.Get("Range"), v2)
	if err != nil {
		httpError(w, "fetch reconstruction from upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	relayResponse(w, resp)
}

// ensureFileReconCached returns found=true if fileID's complete
// reconstruction is now known to the embedded server (already was, or
// was just fetched+ingested), found=false for an upstream 404 (unknown
// file - the one non-error "not found" outcome, distinct from every
// other failure, which is returned as an error).
//
// Deliberately prefetches EVERY xorb the file's reconstruction
// references, not just the ones the caller's own Range header touches:
// once HasFileRecon(fileID) is true, a later request for a DIFFERENT
// range of the same file takes the fast path above with no further
// upstream calls - that only stays correct if every term's footer (and
// bytes) is already cached, since casserver's own reconstruction
// handler has no fallback for a term whose footer isn't found (it
// httpErrors). Fetching only the requested window's xorbs on the first
// touch would make the file only PARTIALLY offline-resilient, and any
// subsequent different-range request would need its own extra
// bookkeeping to detect and backfill the gap. The cost is fetching more
// than one request strictly needs; the benefit is that any file ever
// requested - at any range - becomes wholly and safely offline-capable
// after that first request.
func (s *Server) ensureFileReconCached(r *http.Request, fileID merklehash.Hash) (found bool, err error) {
	if s.Embedded.HasFileRecon(fileID) {
		return true, nil
	}

	cas, err := casClientFromContext(r)
	if err != nil {
		return false, err
	}
	cred := auth.CredentialFromRequest(r)

	fileIDHex := fileID.Hex()
	resp, err := cas.FetchReconstruction(r.Context(), cred, fileIDHex, "", false)
	if err != nil {
		return false, fmt.Errorf("fetch reconstruction for %s from upstream: %w", fileIDHex, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return false, &upstreamStatusError{Status: resp.StatusCode, Body: string(body)}
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, fmt.Errorf("read reconstruction for %s: %w", fileIDHex, err)
	}
	var wire reconwire.ResponseV1
	if err := json.Unmarshal(body, &wire); err != nil {
		return false, fmt.Errorf("decode reconstruction for %s: %w", fileIDHex, err)
	}

	entries := make([]shardformat.FileDataSequenceEntry, 0, len(wire.Terms))
	for _, term := range wire.Terms {
		xorbHash, err := merklehash.FromHex(term.Hash)
		if err != nil {
			return false, fmt.Errorf("reconstruction for %s: invalid xorb hash %q: %w", fileIDHex, term.Hash, err)
		}
		if err := s.cacheXorbFromFetchInfo(r, xorbHash, wire.FetchInfo[term.Hash]); err != nil {
			if errors.Is(err, errPartialXorbCoverage) {
				// Expected for any file that uses only part of a shared
				// xorb (see errPartialXorbCoverage). This file can't be
				// served from cache, so don't record a file_recon that
				// would later point at xorb bytes we don't have -
				// casserver's reconstruction handler has no fallback for
				// a term whose xorb is missing and would hard-error.
				// Reporting notFound=false with no error makes the
				// caller relay upstream live instead.
				slog.Debug("proxycas: file not cacheable, some xorbs only partially cited by its reconstruction",
					"file_id", fileIDHex, "xorb", term.Hash)
				return false, errPartialXorbCoverage
			}
			return false, fmt.Errorf("cache xorb %s referenced by %s: %w", term.Hash, fileIDHex, err)
		}
		entries = append(entries, shardformat.FileDataSequenceEntry{
			XorbHash:             xorbHash,
			UnpackedSegmentBytes: term.UnpackedLength,
			ChunkIndexStart:      term.Range.Start,
			ChunkIndexEnd:        term.Range.End,
		})
	}

	s.Embedded.IngestFileRecon(fileID, entries)
	return true, nil
}
