// Package casserver implements the wire-compatible CAS (Content Addressable
// Storage) HTTP API real Xet clients (hf_xet/xet-core, and by extension the
// `hf` CLI) speak, per xet-core's own openapi/cas.openapi.yaml:
//
//   - POST /v1/xorbs/{prefix}/{hash}       — upload a serialized xorb
//   - POST /v1/shards                      — upload a serialized shard
//   - GET  /v1/reconstructions/{file_id}   — file → xorb/chunk-range map
//   - GET  /v1/xorbs/{prefix}/{hash}       — fetch raw (compressed) xorb bytes
//     (byte-serving endpoint for the
//     URLs handed out in fetch_info;
//     not part of the public CAS API
//     surface, but needed since this
//     server plays both the CAS
//     metadata role and the
//     byte-transfer role real Xet
//     splits across two services)
//   - GET  /v1/chunks/{prefix}/{hash}      — global chunk dedup lookup
//     (always 404: no global dedup
//     index is maintained)
//   - GET  /v2/reconstructions/{file_id}   — always 404/501, signaling
//     clients to fall back to V1
//   - POST /v1/telemetry                   — no-op ack
//   - GET  /v1/storage-stats               — eviction policy stats
//     (operator-facing; not part of the
//     real Xet CAS API)
//
// This server never decompresses chunk payloads — like real CAS, it stores
// and serves xorb bytes as opaque blobs, and integrity is checked via the
// xorb footer's own hash tree rather than by re-verifying chunk contents.
package casserver

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"xet-server/internal/eviction"
	"xet-server/internal/merklehash"
	"xet-server/internal/ratelimit"
	"xet-server/internal/shardformat"
	"xet-server/internal/storage"
	"xet-server/internal/xorbformat"
)

const xorbPrefix = "default"

// Server implements the CAS HTTP API against a storage.Store backend for
// xorb bytes. File-reconstruction and xorb-footer indexes are held in
// memory: they are metadata derived from uploaded shards/xorbs, cheap to
// rebuild, and small relative to the bulk chunk data in Store.
//
// Each index has its own mutex rather than one shared lock: none of the
// four maps are ever read or written together under one critical section
// (confirmed — no code path needs a consistent snapshot across more than
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

	// evictionStats, if set via SetEvictionStats, backs GET
	// /v1/storage-stats. nil (the default, when no eviction.Sweeper is
	// running) means that endpoint reports eviction as disabled rather
	// than erroring.
	evictionStats func() eviction.Stats

	// uploadLimiter, if set via SetUploadRateLimiter, gates the xorb and
	// shard upload endpoints — the expensive paths (decompression,
	// hashing) a hostile or misbehaving client could otherwise hammer. nil
	// (the default) means uploads are unlimited, matching this server's
	// pre-rate-limiting behavior.
	uploadLimiter *ratelimit.Limiter
}

func New(xorbs storage.Store) *Server {
	s := &Server{
		xorbs:          xorbs,
		mux:            http.NewServeMux(),
		fileRecon:      make(map[merklehash.Hash][]shardformat.FileDataSequenceEntry),
		xorbFooters:    make(map[merklehash.Hash]xorbformat.FooterV1),
		xorbRawLength:  make(map[merklehash.Hash]int64),
		xorbLastAccess: make(map[merklehash.Hash]time.Time),
		xorbInFlight:   make(map[merklehash.Hash]int),
		sha256ToXet:    make(map[string]merklehash.Hash),
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
// had never been uploaded — a client that still needs it must re-upload
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
// serving any traffic — it rebuilds the route table (http.ServeMux
// panics on duplicate pattern registration, so routes are re-registered
// from scratch on a fresh mux rather than layered on top of the
// existing one).
func (s *Server) SetUploadRateLimiter(limiter *ratelimit.Limiter) {
	s.uploadLimiter = limiter
	s.mux = http.NewServeMux()
	s.routes()
}

// storageStatsResponse is GET /v1/storage-stats's body — not part of the
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
	defer s.sha256Mu.RUnlock()
	h, ok := s.sha256ToXet[sha256Hex]
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

func (s *Server) routes() {
	uploadXorb := http.HandlerFunc(s.handleUploadXorb)
	uploadShard := http.HandlerFunc(s.handleUploadShard)
	if s.uploadLimiter != nil {
		s.mux.Handle("POST /v1/xorbs/{prefix}/{hash}", s.uploadLimiter.Middleware(uploadXorb))
		s.mux.Handle("POST /v1/shards", s.uploadLimiter.Middleware(uploadShard))
	} else {
		s.mux.Handle("POST /v1/xorbs/{prefix}/{hash}", uploadXorb)
		s.mux.Handle("POST /v1/shards", uploadShard)
	}
	s.mux.HandleFunc("GET /v1/xorbs/{prefix}/{hash}", s.handleFetchXorb)
	s.mux.HandleFunc("HEAD /v1/xorbs/{prefix}/{hash}", s.handleHeadXorb)
	s.mux.HandleFunc("GET /v1/reconstructions/{file_id}", s.handleReconstructionV1)
	s.mux.HandleFunc("GET /v2/reconstructions/{file_id}", s.handleReconstructionV2)
	s.mux.HandleFunc("GET /v1/chunks/{prefix}/{hash}", s.handleChunkDedup)
	s.mux.HandleFunc("POST /v1/telemetry", s.handleTelemetry)
	s.mux.HandleFunc("GET /v1/storage-stats", s.handleStorageStats)
}

// --- JSON response shapes, matching openapi/cas.openapi.yaml verbatim ---

type uploadXorbResponse struct {
	WasInserted bool `json:"was_inserted"`
}

type uploadShardResponse struct {
	Result int `json:"result"` // 0 = already exists, 1 = SyncPerformed
}

type indexRange struct {
	Start uint32 `json:"start"`
	End   uint32 `json:"end"`
}

type byteRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"` // inclusive
}

type reconstructionTerm struct {
	Hash           string     `json:"hash"`
	Range          indexRange `json:"range"`
	UnpackedLength uint32     `json:"unpacked_length"`
}

type fetchInfoEntry struct {
	URL      string     `json:"url"`
	URLRange byteRange  `json:"url_range"`
	Range    indexRange `json:"range"`
}

type reconstructionResponseV1 struct {
	OffsetIntoFirstRange int64                       `json:"offset_into_first_range"`
	Terms                []reconstructionTerm        `json:"terms"`
	FetchInfo            map[string][]fetchInfoEntry `json:"fetch_info"`
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("casserver: encode response", "error", err)
	}
}

func hexParam(r *http.Request, name string) (merklehash.Hash, error) {
	return merklehash.FromHex(r.PathValue(name))
}
