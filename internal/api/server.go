// Package api implements the xetd HTTP server: a local stand-in for Xet's
// CAS/reconstruction service. Clients upload files, which are chunked and
// deduplicated against the store; clients download files by reconstructing
// them from a manifest's chunk list.
package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"xet-server/internal/chunk"
	"xet-server/internal/manifest"
	"xet-server/internal/storage"
	"xet-server/internal/storage/fsstore"
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
		chunks:       chunks,
		manifestsDir: manifestsDir,
		chunker:      chunk.NewChunker(chunk.DefaultMinSize, chunk.DefaultAvgSize, chunk.DefaultMaxSize),
		mux:          http.NewServeMux(),
	}
	s.routes()
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) routes() {
	s.mux.HandleFunc("POST /upload", s.handleUpload)
	s.mux.HandleFunc("GET /files/{id}", s.handleDownload)
	s.mux.HandleFunc("GET /files/{id}/manifest", s.handleManifest)
	s.mux.HandleFunc("GET /stats", s.handleStats)
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
