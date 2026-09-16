// Package casserver implements the wire-compatible CAS (Content Addressable
// Storage) HTTP API real Xet clients (hf_xet/xet-core, and by extension the
// `hf` CLI) speak, per xet-core's own openapi/cas.openapi.yaml:
//
//   - POST /v1/xorbs/{prefix}/{hash}       - upload a serialized xorb
//   - POST /v1/shards                      - upload a serialized shard
//   - GET  /v1/reconstructions/{file_id}   - file -> xorb/chunk-range map
//   - GET  /v1/xorbs/{prefix}/{hash}       - fetch raw (compressed) xorb bytes
//     (byte-serving endpoint for the
//     URLs handed out in fetch_info;
//     not part of the public CAS API
//     surface, but needed since this
//     server plays both the CAS
//     metadata role and the
//     byte-transfer role real Xet
//     splits across two services)
//   - GET  /v1/chunks/{prefix}/{hash}      - global chunk dedup lookup:
//     returns the raw bytes of whichever
//     uploaded shard referenced this chunk
//     hash (the real wire contract per
//     xet-core's openapi spec - a client
//     parses the returned shard itself to
//     find dedup-eligible chunks), 404 if
//     no uploaded shard ever referenced it
//   - GET  /v2/reconstructions/{file_id}   - multi-range-optimized
//     file -> xorb/chunk-range map (same
//     underlying data as V1, grouped by
//     xorb instead of one entry per term)
//   - POST /v1/telemetry                   - no-op ack
//   - GET  /v1/storage-stats               - eviction policy stats
//     (operator-facing; not part of the
//     real Xet CAS API)
//
// This server never decompresses chunk payloads - like real CAS, it stores
// and serves xorb bytes as opaque blobs, and integrity is checked via the
// xorb footer's own hash tree rather than by re-verifying chunk contents.
//
// Every route above except telemetry and storage-stats requires the
// scope real xet-core's own OpenAPI spec documents for it (read for every
// GET, write for the two uploads), enforced via auth.Authenticator - see
// SetAuthenticator. The default (auth.NoAuth{}) enforces nothing, this
// server's behavior prior to v0.8.0.
package casserver

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/guilt/xet-server/internal/auth"
	"github.com/guilt/xet-server/internal/eviction"
	"github.com/guilt/xet-server/internal/merklehash"
	"github.com/guilt/xet-server/internal/ratelimit"
	"github.com/guilt/xet-server/internal/reconwire"
	"github.com/guilt/xet-server/internal/routing"
	"github.com/guilt/xet-server/internal/shardformat"
	"github.com/guilt/xet-server/internal/storage"
	"github.com/guilt/xet-server/internal/xorbformat"
)

const xorbPrefix = "default"

// Server implements the CAS HTTP API against a storage.Store backend for
// xorb bytes. File-reconstruction and xorb-footer indexes are held in
// memory: they are metadata derived from uploaded shards/xorbs, cheap to
// rebuild, and small relative to the bulk chunk data in Store.
//
// Each index has its own mutex rather than one shared lock: none of the
// four maps are ever read or written together under one critical section
// (confirmed - no code path needs a consistent snapshot across more than
// one of them), so a single global lock only serialized unrelated
// concurrent uploads/downloads without buying any actual consistency
// guarantee. Splitting them lets a large xorb upload (which only touches
// xorbFooters/xorbRawLength) proceed concurrently with an unrelated
// reconstruction lookup (which only touches fileRecon).
//
// xorbLastAccess/xorbInFlight exist purely to support eviction.Sweeper
// (see EvictionCandidates/ForgetKey below): the last time each xorb was
// uploaded or fetched, and how many fetches are in progress right now, so
// a sweep never deletes a blob a client might be mid-download of.
type Server struct {
	xorbs storage.Store
	mux   *http.ServeMux

	fileReconMu sync.RWMutex
	fileRecon   map[merklehash.Hash][]shardformat.FileDataSequenceEntry

	xorbMu         sync.RWMutex
	xorbFooters    map[merklehash.Hash]xorbformat.FooterV1
	xorbRawLength  map[merklehash.Hash]int64
	xorbLastAccess map[merklehash.Hash]time.Time
	xorbInFlight   map[merklehash.Hash]int

	sha256Mu    sync.RWMutex
	sha256ToXet map[string]merklehash.Hash // hex SHA-256 -> Xet/Merkle file hash

	// chunkDedupMu guards chunkHashToShard AND shardBodies: an index from
	// every chunk hash referenced by any uploaded shard's xorb-info
	// section to that shard's OWN content hash, plus a shared table of
	// shard bodies keyed by that content hash - backing GET
	// /v1/chunks/{prefix}/{hash} (see handleChunkDedup). This
	// content-address-dedup layout replaces an earlier one where every
	// chunk mapped to its OWN copy of the (raw) shard bytes, which made
	// json.Marshal of the snapshot re-emit the same multi-MB shard body
	// once per chunk that referenced it - a 30 MB shard × 1.7 M chunks
	// blew the marshal buffer past 50 TB in practice and OOM'd the
	// snapshot goroutine every minute. Storing each shard body once and
	// pointing every chunk at its 32-byte content hash keeps the on-disk
	// snapshot proportional to (unique shards + chunks), not (shards ×
	// chunks). Deliberately a separate lock from fileReconMu/xorbMu/
	// sha256Mu even though it's populated at the same time as sha256ToXet
	// during shard upload - chunk-dedup lookups are a distinct read
	// pattern (by chunk hash, not file hash) that shouldn't contend with
	// file-hash lookups on the same shard-upload critical section.
	chunkDedupMu     sync.RWMutex
	chunkHashToShard map[merklehash.Hash]merklehash.Hash
	shardBodies      map[merklehash.Hash][]byte
	// xorbToShard maps each xorb hash to the content hash of the shard
	// whose xorb-info introduced it (guarded by chunkDedupMu too). It is
	// derived, never snapshotted: LoadSnapshot rebuilds it from
	// ShardBodies. dedupAnswer uses it to describe a file's other xorbs.
	xorbToShard map[merklehash.Hash]merklehash.Hash

	// evictionStats, if set via SetEvictionStats, backs GET
	// /v1/storage-stats. nil (the default, when no eviction.Sweeper is
	// running) means that endpoint reports eviction as disabled rather
	// than erroring.
	evictionStats func() eviction.Stats

	// uploadLimiter, if set via SetUploadRateLimiter, gates the xorb and
	// shard upload endpoints - the expensive paths (decompression,
	// hashing) a hostile or misbehaving client could otherwise hammer. nil
	// (the default) means uploads are unlimited, matching this server's
	// pre-rate-limiting behavior.
	uploadLimiter *ratelimit.Limiter

	// authenticator gates every route per its required scope (see
	// requireScope/routes). Defaults to auth.NoAuth{} - this server's
	// pre-v0.8.0 behavior, unconditionally allowing every request - until
	// SetAuthenticator is called with something else.
	authenticator auth.Authenticator

	// folder is the synced-folder support (shard dir, rescans); see
	// folder.go. Zero value means shards live only in memory/snapshot.
	folder folderState
}

func New(xorbs storage.Store) *Server {
	s := &Server{
		xorbs:            xorbs,
		mux:              http.NewServeMux(),
		fileRecon:        make(map[merklehash.Hash][]shardformat.FileDataSequenceEntry),
		xorbFooters:      make(map[merklehash.Hash]xorbformat.FooterV1),
		xorbRawLength:    make(map[merklehash.Hash]int64),
		xorbLastAccess:   make(map[merklehash.Hash]time.Time),
		xorbInFlight:     make(map[merklehash.Hash]int),
		sha256ToXet:      make(map[string]merklehash.Hash),
		chunkHashToShard: make(map[merklehash.Hash]merklehash.Hash),
		shardBodies:      make(map[merklehash.Hash][]byte),
		xorbToShard:      make(map[merklehash.Hash]merklehash.Hash),
		authenticator:    auth.NoAuth{},
	}
	s.routes()
	return s
}

// EvictionCandidates implements eviction.Registry: every xorb this server
// knows about, excluding any with a fetch currently in progress. Called
// by eviction.Sweeper on its own poll interval, not a request hot path.
func (s *Server) EvictionCandidates() []eviction.Candidate {
	s.xorbMu.RLock()
	defer s.xorbMu.RUnlock()
	candidates := make([]eviction.Candidate, 0, len(s.xorbRawLength))
	for hash, size := range s.xorbRawLength {
		if s.xorbInFlight[hash] > 0 {
			continue
		}
		candidates = append(candidates, eviction.Candidate{
			Key:        hash.Hex(),
			LastAccess: s.xorbLastAccess[hash],
			Size:       size,
		})
	}
	return candidates
}

// ForgetKey implements eviction.Registry: drops key from every in-memory
// index once eviction.Sweeper has already deleted the underlying blob
// from storage. A subsequent fetch of this xorb 404s, exactly as if it
// had never been uploaded - a client that still needs it must re-upload
// (real Xet clients already handle a missing xorb by re-deriving it from
// the source file, since CAS storage is explicitly not guaranteed
// permanent).
func (s *Server) ForgetKey(key string) {
	hash, err := merklehash.FromHex(key)
	if err != nil {
		slog.Warn("eviction: ForgetKey given an unparseable key", "key", key, "error", err)
		return
	}
	s.xorbMu.Lock()
	delete(s.xorbFooters, hash)
	delete(s.xorbRawLength, hash)
	delete(s.xorbLastAccess, hash)
	delete(s.xorbInFlight, hash)
	s.xorbMu.Unlock()
}

// SetEvictionStats wires an eviction.Sweeper's Stats method into GET
// /v1/storage-stats, so the eviction policy's effect is observable via
// the running server rather than only inferable from logs. Call once at
// startup if an eviction.Sweeper was created for this server's store.
func (s *Server) SetEvictionStats(statsFunc func() eviction.Stats) {
	s.evictionStats = statsFunc
}

// SetUploadRateLimiter wires a per-source-IP token-bucket limiter in
// front of the xorb and shard upload endpoints. Must be called before
// serving any traffic - it rebuilds the route table (http.ServeMux
// panics on duplicate pattern registration, so routes are re-registered
// from scratch on a fresh mux rather than layered on top of the
// existing one).
func (s *Server) SetUploadRateLimiter(limiter *ratelimit.Limiter) {
	s.uploadLimiter = limiter
	s.mux = http.NewServeMux()
	s.routes()
}

// SetAuthenticator replaces this server's Authenticator (default
// auth.NoAuth{}, i.e. no enforcement - this server's pre-v0.8.0
// behavior). Must be called before serving any traffic, for the same
// route-rebuild reason as SetUploadRateLimiter. See auth.Authenticator's
// doc comment for how to implement a custom one.
func (s *Server) SetAuthenticator(a auth.Authenticator) {
	s.authenticator = a
	s.mux = http.NewServeMux()
	s.routes()
}

// requireScope wraps next so a request must authenticate (via
// s.authenticator) and hold scope before reaching next. A missing/invalid
// credential (auth.ErrUnauthenticated) maps to 401; a valid credential
// lacking scope maps to 403 - the same 401-vs-403 split real xet-core's
// CAS API documents (see docs/PROTOCOL.md's auth section). Logged at
// Debug via httpError, same as any other 4xx here: a client without a
// token, or with the wrong one, is expected/routine traffic to log
// quietly, not a server-side fault.
func (s *Server) requireScope(scope auth.Scope, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, err := s.authenticator.Authenticate(r)
		if err != nil {
			if errors.Is(err, auth.ErrUnauthenticated) {
				httpError(w, "unauthenticated: "+err.Error(), http.StatusUnauthorized)
			} else {
				httpError(w, "authentication failed: "+err.Error(), http.StatusForbidden)
			}
			return
		}
		if !principal.HasScope(scope) {
			httpError(w, "principal lacks required scope: "+string(scope), http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// storageStatsResponse is GET /v1/storage-stats's body - not part of the
// real Xet CAS API surface (xet-core's clients never call it), purely an
// operator-facing endpoint for this server.
type storageStatsResponse struct {
	EvictionEnabled bool  `json:"eviction_enabled"`
	BudgetBytes     int64 `json:"budget_bytes,omitempty"`
	EvictionsTotal  int64 `json:"evictions_total,omitempty"`
	BytesFreedTotal int64 `json:"bytes_freed_total,omitempty"`
}

func (s *Server) handleStorageStats(w http.ResponseWriter, r *http.Request) {
	if s.evictionStats == nil {
		writeJSON(w, storageStatsResponse{EvictionEnabled: false})
		return
	}
	stats := s.evictionStats()
	writeJSON(w, storageStatsResponse{
		EvictionEnabled: true,
		BudgetBytes:     stats.BudgetBytes,
		EvictionsTotal:  stats.EvictionsTotal,
		BytesFreedTotal: stats.BytesFreedTotal,
	})
}

// XetHashForSHA256 returns the Xet/Merkle file hash for a file previously
// uploaded via a shard whose FileMetadataExt declared this SHA-256, or
// false if no such file is known. Used by hubserver to bridge the Hub
// commit API's plain-SHA-256 file identity to the Xet hash the CAS layer
// indexes reconstructions under.
func (s *Server) XetHashForSHA256(sha256Hex string) (merklehash.Hash, bool) {
	s.sha256Mu.RLock()
	h, ok := s.sha256ToXet[sha256Hex]
	s.sha256Mu.RUnlock()
	if !ok && s.rescanOnMiss() {
		// A shard another replica wrote may have just synced in.
		s.sha256Mu.RLock()
		h, ok = s.sha256ToXet[sha256Hex]
		s.sha256Mu.RUnlock()
	}
	return h, ok
}

// FileSize returns the total unpacked size of a file known to this
// server's reconstruction index, or false if fileHash is unknown.
func (s *Server) FileSize(fileHash merklehash.Hash) (int64, bool) {
	s.fileReconMu.RLock()
	entries, ok := s.fileRecon[fileHash]
	s.fileReconMu.RUnlock()
	if !ok {
		return 0, false
	}
	var size int64
	for _, e := range entries {
		size += int64(e.UnpackedSegmentBytes)
	}
	return size, true
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// V1/V2 are this server's URL version prefixes - exported so cmd/xetd
// can reference them directly when wiring routes onto its own top-level
// mux, instead of re-typing "/v1"/"/v2" as a raw literal at the call
// site. V2 exists solely for the multi-range-optimized reconstruction
// endpoint; every other route here is V1.
//
// XorbsPath, ShardsPath, ReconstructionsPath, ChunksPath, TelemetryPath,
// and StoragestatsPath are the specific literal sub-paths (relative to
// V1) this package registers below, also exported so any other package
// referencing one of these paths (cmd/xetd, internal/landingpage, a
// future client) uses the same named constant instead of a duplicated
// string literal. ReconstructionsPathV2 is the V2 counterpart of
// ReconstructionsPath.
const (
	V1 = "/v1"
	V2 = "/v2"

	XorbsPath  = V1 + "/xorbs/{prefix}/{hash}"
	ShardsPath = V1 + "/shards"
	// ShardsPathUnversioned is where xet-core >= 1.5 (git-xet 0.2, recent
	// hf_xet) POSTs shards: cas_client/src/remote_client.rs builds
	// "{endpoint}/shards" with no version segment, while the openapi
	// spec still documents /v1/shards. Both are served identically;
	// cmd/xetd mounts this one at the root of the CAS mux.
	ShardsPathUnversioned = "/shards"
	// Deliberately NOT served: POST /v2/shards. Current hf_xet tries it
	// first, but v2 is a different protocol - the response is a stream
	// of newline-delimited JSON progress frames with a "type" field, and
	// the v1 body {"result":1} makes the client fail with "failed to
	// parse shard upload progress frame". A 404 there makes it fall back
	// to the v1 path, which is what this server speaks (PROTOCOL.md #16).
	ReconstructionsPath   = V1 + "/reconstructions/{file_id}"
	ReconstructionsPathV2 = V2 + "/reconstructions/{file_id}"
	ChunksPath            = V1 + "/chunks/{prefix}/{hash}"
	TelemetryPath         = V1 + "/telemetry"
	StoragestatsPath      = V1 + "/storage-stats"
)

func (s *Server) routes() {
	uploadXorb := s.requireScope(auth.ScopeWrite, s.handleUploadXorb)
	uploadShard := s.requireScope(auth.ScopeWrite, s.handleUploadShard)
	if s.uploadLimiter != nil {
		uploadXorb = s.uploadLimiter.Middleware(uploadXorb).ServeHTTP
		uploadShard = s.uploadLimiter.Middleware(uploadShard).ServeHTTP
	}
	routing.Apply(s.mux, []routing.Route{
		routing.Mount("POST", XorbsPath, uploadXorb),
		routing.Mount("POST", ShardsPath, uploadShard),
		routing.Mount("POST", ShardsPathUnversioned, uploadShard),
		routing.Mount("GET", XorbsPath, s.requireScope(auth.ScopeRead, s.handleFetchXorb)),
		routing.Mount("HEAD", XorbsPath, s.requireScope(auth.ScopeRead, s.handleHeadXorb)),
		routing.Mount("GET", ReconstructionsPath, s.requireScope(auth.ScopeRead, s.handleReconstructionV1)),
		routing.Mount("GET", ReconstructionsPathV2, s.requireScope(auth.ScopeRead, s.handleReconstructionV2)),
		routing.Mount("GET", ChunksPath, s.requireScope(auth.ScopeRead, s.handleChunkDedup)),
		// Telemetry and storage-stats are unauthenticated regardless of
		// s.authenticator: telemetry is a fire-and-forget client
		// diagnostic with nothing sensitive to protect, and
		// storage-stats is this project's own operator-facing endpoint
		// (not part of the real Xet CAS API at all) - matching this
		// server's pre-v0.8.0 behavior for both.
		routing.Mount("POST", TelemetryPath, http.HandlerFunc(s.handleTelemetry)),
		routing.Mount("GET", StoragestatsPath, http.HandlerFunc(s.handleStorageStats)),
	})
}

// --- JSON response shapes, matching openapi/cas.openapi.yaml verbatim ---

type uploadXorbResponse struct {
	WasInserted bool `json:"was_inserted"`
}

type uploadShardResponse struct {
	Result int `json:"result"` // 0 = already exists, 1 = SyncPerformed
}

// reconstructionResponseV1/V2 alias internal/reconwire's exported wire
// types - the response-building logic itself now lives there (shared
// with internal/proxycas), but this package's own tests decode these
// same shapes to assert on this server's responses, so the aliases stay
// as this package's own names rather than every test importing reconwire
// directly for a type it only ever uses for decoding.
type reconstructionResponseV1 = reconwire.ResponseV1
type reconstructionResponseV2 = reconwire.ResponseV2

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("casserver: encode response", "error", err)
	}
}

func hexParam(r *http.Request, name string) (merklehash.Hash, error) {
	return merklehash.FromHex(r.PathValue(name))
}
