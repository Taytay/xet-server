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
//
// This server never decompresses chunk payloads — like real CAS, it stores
// and serves xorb bytes as opaque blobs, and integrity is checked via the
// xorb footer's own hash tree rather than by re-verifying chunk contents.
package casserver

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"xet-server/internal/merklehash"
	"xet-server/internal/shardformat"
	"xet-server/internal/storage"
	"xet-server/internal/xorbformat"
)

const xorbPrefix = "default"

// Server implements the CAS HTTP API against a storage.Store backend for
// xorb bytes. File-reconstruction and xorb-footer indexes are held in
// memory: they are metadata derived from uploaded shards/xorbs, cheap to
// rebuild, and small relative to the bulk chunk data in Store.
type Server struct {
	xorbs storage.Store
	mux   *http.ServeMux

	mu            sync.RWMutex
	fileRecon     map[merklehash.Hash][]shardformat.FileDataSequenceEntry
	xorbFooters   map[merklehash.Hash]xorbformat.FooterV1
	xorbRawLength map[merklehash.Hash]int64
	sha256ToXet   map[string]merklehash.Hash // hex SHA-256 -> Xet/Merkle file hash
}

func New(xorbs storage.Store) *Server {
	s := &Server{
		xorbs:         xorbs,
		mux:           http.NewServeMux(),
		fileRecon:     make(map[merklehash.Hash][]shardformat.FileDataSequenceEntry),
		xorbFooters:   make(map[merklehash.Hash]xorbformat.FooterV1),
		xorbRawLength: make(map[merklehash.Hash]int64),
		sha256ToXet:   make(map[string]merklehash.Hash),
	}
	s.routes()
	return s
}

// XetHashForSHA256 returns the Xet/Merkle file hash for a file previously
// uploaded via a shard whose FileMetadataExt declared this SHA-256, or
// false if no such file is known. Used by hubserver to bridge the Hub
// commit API's plain-SHA-256 file identity to the Xet hash the CAS layer
// indexes reconstructions under.
func (s *Server) XetHashForSHA256(sha256Hex string) (merklehash.Hash, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, ok := s.sha256ToXet[sha256Hex]
	return h, ok
}

// FileSize returns the total unpacked size of a file known to this
// server's reconstruction index, or false if fileHash is unknown.
func (s *Server) FileSize(fileHash merklehash.Hash) (int64, bool) {
	s.mu.RLock()
	entries, ok := s.fileRecon[fileHash]
	s.mu.RUnlock()
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
	s.mux.HandleFunc("POST /v1/xorbs/{prefix}/{hash}", s.handleUploadXorb)
	s.mux.HandleFunc("GET /v1/xorbs/{prefix}/{hash}", s.handleFetchXorb)
	s.mux.HandleFunc("HEAD /v1/xorbs/{prefix}/{hash}", s.handleHeadXorb)
	s.mux.HandleFunc("POST /v1/shards", s.handleUploadShard)
	s.mux.HandleFunc("GET /v1/reconstructions/{file_id}", s.handleReconstructionV1)
	s.mux.HandleFunc("GET /v2/reconstructions/{file_id}", s.handleReconstructionV2)
	s.mux.HandleFunc("GET /v1/chunks/{prefix}/{hash}", s.handleChunkDedup)
	s.mux.HandleFunc("POST /v1/telemetry", s.handleTelemetry)
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
		log.Printf("casserver: encode response: %v", err)
	}
}

func hexParam(r *http.Request, name string) (merklehash.Hash, error) {
	return merklehash.FromHex(r.PathValue(name))
}
