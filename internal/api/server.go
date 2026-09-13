// Package api implements the Xet Data API (mounted at /v1 by cmd/xetd,
// sharing that namespace with - but never overlapping the specific
// literal paths of - internal/casserver's own /v1,/v2 CAS
// reimplementation; see xetDataV1 below for the exact paths this package
// owns): a local stand-in for Xet's CAS/reconstruction service, not
// wire-compatible with the real protocol (see internal/casserver for
// that). Clients upload files, which are chunked and deduplicated against
// the store; clients download files by reconstructing them from a
// manifest's chunk list.
//
// Every route except /v1/stats (this project's own operator endpoint, not
// part of any real protocol) is gated by auth.Authenticator per the scope
// real Xet/HF convention implies: write for upload, read for
// download/manifest. Defaults to auth.NoAuth{} - this server's pre-v0.8.0
// behavior, unconditionally allowing every request - until
// SetAuthenticator is called with something else. See
// internal/casserver's identical pattern, which this mirrors.
package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/guilt/xet-server/internal/auth"
	"github.com/guilt/xet-server/internal/chunk"
	"github.com/guilt/xet-server/internal/manifest"
	"github.com/guilt/xet-server/internal/routing"
	"github.com/guilt/xet-server/internal/storage"
	"github.com/guilt/xet-server/internal/storage/fsstore"
)

type Server struct {
	chunks       storage.Store
	manifestsDir string
	chunker      *chunk.Chunker
	mux          *http.ServeMux

	statsMu     sync.Mutex
	uniqueBytes int64
	uniqueCount int64
	filesStored int64

	// authenticator gates every route except /stats per its required
	// scope (see requireScope and routes below). Defaults to
	// auth.NoAuth{} until SetAuthenticator is called with something else.
	authenticator auth.Authenticator
}

// New creates a Server backed by a local filesystem chunk store rooted at
// <dataRoot>/chunks. Use NewWithStore to supply a different storage.Store
// backend (e.g. S3/MinIO).
func New(dataRoot string) (*Server, error) {
	chunksDir := filepath.Join(dataRoot, "chunks")
	cs, err := fsstore.New(chunksDir)
	if err != nil {
		return nil, err
	}
	return NewWithStore(dataRoot, cs)
}

// NewWithStore creates a Server whose chunk bytes live in the given
// storage.Store backend. Manifests always live on the local filesystem
// under <dataRoot>/manifests, since they're small CAS metadata rather than
// the bulk data the storage backend abstraction is for.
func NewWithStore(dataRoot string, chunks storage.Store) (*Server, error) {
	manifestsDir := filepath.Join(dataRoot, "manifests")
	if err := os.MkdirAll(manifestsDir, 0o755); err != nil {
		return nil, err
	}
	s := &Server{
		chunks:        chunks,
		manifestsDir:  manifestsDir,
		chunker:       chunk.NewChunker(chunk.DefaultMinSize, chunk.DefaultAvgSize, chunk.DefaultMaxSize),
		mux:           http.NewServeMux(),
		authenticator: auth.NoAuth{},
	}
	s.routes()
	return s, nil
}

// SetAuthenticator replaces this server's Authenticator (default
// auth.NoAuth{}, i.e. no enforcement - this server's pre-v0.8.0 behavior)
// and rebuilds the route table so the new scope checks take effect
// immediately, matching internal/casserver.Server.SetAuthenticator.
func (s *Server) SetAuthenticator(a auth.Authenticator) {
	s.authenticator = a
	s.mux = http.NewServeMux()
	s.routes()
}

// requireScope wraps next so it only runs once r authenticates against
// s.authenticator and the resulting Principal has scope - writing a 401
// (no/invalid credential) or 403 (valid credential, insufficient scope)
// otherwise. Mirrors casserver.Server.requireScope's semantics exactly.
func (s *Server) requireScope(scope auth.Scope, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, err := s.authenticator.Authenticate(r)
		if err != nil {
			if errors.Is(err, auth.ErrUnauthenticated) {
				http.Error(w, "unauthenticated: "+err.Error(), http.StatusUnauthorized)
			} else {
				http.Error(w, "authentication failed: "+err.Error(), http.StatusForbidden)
			}
			return
		}
		if !principal.HasScope(scope) {
			http.Error(w, "principal lacks required scope: "+string(scope), http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// V1 is this API's URL version prefix, exported so cmd/xetd can reference
// it directly when wiring routes onto its own top-level mux, instead of
// re-typing "/v1" as a raw literal at the call site. It shares the /v1
// namespace with internal/casserver's own CAS protocol rather than living
// under a separate top-level path - grouped there because cmd/xetd mounts
// both on the same server, and this project's own "Xet Data" surface is
// versioned exactly like casserver's /v1,/v2 rather than left bare.
//
// UploadPath, FilesPrefix, and StatsPath are the specific literal
// sub-paths (relative to V1) this package registers, also exported so
// cmd/xetd's own mux.Handle calls reference the same constants this
// package's own routes() uses - one definition per path, not duplicated
// as a string literal at each call site. They never collide with
// casserver's own /v1 paths (xorbs, shards, reconstructions, chunks,
// telemetry, storage-stats): cmd/xetd registers these literal patterns on
// its shared top-level mux ahead of casserver's "/v1/" wildcard, and Go's
// http.ServeMux always prefers the more specific match regardless of
// registration order. A future incompatible change to this API's wire
// shape can land at /v2/upload,... (its own routes() using
// routing.MountWithVersion("/v2", ...) instead) without touching v1's
// registrations below.
const (
	V1          = "/v1"
	UploadPath  = V1 + "/upload"
	FilesPrefix = V1 + "/files/"
	StatsPath   = V1 + "/stats"
)

func (s *Server) routes() {
	routing.Apply(s.mux, []routing.Route{
		routing.Mount("POST", UploadPath, s.requireScope(auth.ScopeWrite, s.handleUpload)),
		routing.Mount("GET", FilesPrefix+"{id}", s.requireScope(auth.ScopeRead, s.handleDownload)),
		routing.Mount("GET", FilesPrefix+"{id}/manifest", s.requireScope(auth.ScopeRead, s.handleManifest)),
		// stats is this project's own operator endpoint, not part of any
		// real protocol, so - like casserver's storage-stats - it is
		// never gated.
		routing.Mount("GET", StatsPath, http.HandlerFunc(s.handleStats)),
	})
}

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

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	ctx := r.Context()

	fullHash := sha256.New()
	var chunks []manifest.ChunkRef
	var size int64
	var newChunks int
	var bytesStored int64

	tee := io.TeeReader(r.Body, fullHash)
	err := s.chunker.Split(tee, func(c chunk.Chunk) error {
		written, err := s.chunks.Put(ctx, c.Hash, bytes.NewReader(c.Data), int64(len(c.Data)))
		if err != nil {
			return err
		}
		if written {
			newChunks++
			bytesStored += int64(c.Length)
		}
		chunks = append(chunks, manifest.ChunkRef{Hash: c.Hash, Offset: c.Offset, Length: c.Length})
		size += int64(c.Length)
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	sha := hex.EncodeToString(fullHash.Sum(nil))
	fileID := manifest.FileID(sha)
	m := &manifest.Manifest{FileID: fileID, Size: size, SHA256: sha, Chunks: chunks}
	if err := m.Save(filepath.Join(s.manifestsDir, fileID+".json")); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.statsMu.Lock()
	s.uniqueCount += int64(newChunks)
	s.uniqueBytes += bytesStored
	s.filesStored++
	s.statsMu.Unlock()

	res := UploadResult{
		FileID:      fileID,
		Name:        name,
		Size:        size,
		SHA256:      sha,
		ChunksTotal: len(chunks),
		ChunksNew:   newChunks,
		BytesStored: bytesStored,
		BytesOnWire: size,
	}
	if size > 0 {
		res.DedupPercent = 100 * (1 - float64(bytesStored)/float64(size))
	}
	slog.Info("upload", "fileID", fileID, "chunksTotal", res.ChunksTotal, "chunksNew", res.ChunksNew,
		"size", res.Size, "bytesStored", res.BytesStored, "dedupPercent", res.DedupPercent)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func (s *Server) loadManifest(id string) (*manifest.Manifest, error) {
	if strings.ContainsAny(id, "/\\") {
		return nil, fmt.Errorf("invalid file id")
	}
	return manifest.Load(filepath.Join(s.manifestsDir, id+".json"))
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m, err := s.loadManifest(id)
	if err != nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", m.Size))
	ctx := r.Context()
	for _, c := range m.Chunks {
		data, err := s.chunks.Get(ctx, c.Hash)
		if err != nil {
			http.Error(w, "missing chunk "+c.Hash, http.StatusInternalServerError)
			return
		}
		_, err = io.Copy(w, data)
		data.Close()
		if err != nil {
			return
		}
	}
}

func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m, err := s.loadManifest(id)
	if err != nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(m)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	s.statsMu.Lock()
	chunkCount, chunkBytes, filesStored := s.uniqueCount, s.uniqueBytes, s.filesStored
	s.statsMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"unique_chunks":      chunkCount,
		"unique_chunk_bytes": chunkBytes,
		"files_stored":       filesStored,
	})
}
