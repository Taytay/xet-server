// Package api implements the xetd HTTP server: a local stand-in for Xet's
// CAS/reconstruction service. Clients upload files, which are chunked and
// deduplicated against the store; clients download files by reconstructing
// them from a manifest's chunk list.
package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"xet-lite/internal/chunk"
	"xet-lite/internal/manifest"
	"xet-lite/internal/store"
)

type Server struct {
	chunks       *store.Store
	manifestsDir string
	chunker      *chunk.Chunker
	mux          *http.ServeMux
}

func New(dataRoot string) (*Server, error) {
	chunksDir := filepath.Join(dataRoot, "chunks")
	manifestsDir := filepath.Join(dataRoot, "manifests")
	if err := os.MkdirAll(manifestsDir, 0o755); err != nil {
		return nil, err
	}
	cs, err := store.New(chunksDir)
	if err != nil {
		return nil, err
	}
	s := &Server{
		chunks:       cs,
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

	fullHash := sha256.New()
	var chunks []manifest.ChunkRef
	var size int64
	var newChunks int
	var bytesStored int64

	tee := io.TeeReader(r.Body, fullHash)
	err := s.chunker.Split(tee, func(c chunk.Chunk) error {
		written, err := s.chunks.Put(c.Hash, c.Data)
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
	log.Printf("upload %s: %d chunks (%d new), %d bytes -> %d bytes stored (%.1f%% dedup)",
		fileID, res.ChunksTotal, res.ChunksNew, res.Size, res.BytesStored, res.DedupPercent)

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
	for _, c := range m.Chunks {
		data, err := s.chunks.Get(c.Hash)
		if err != nil {
			http.Error(w, "missing chunk "+c.Hash, http.StatusInternalServerError)
			return
		}
		if _, err := w.Write(data); err != nil {
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
	var chunkCount int
	var chunkBytes int64
	filepath.Walk(s.chunks.Root, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && !strings.Contains(info.Name(), ".tmp-") {
			chunkCount++
			chunkBytes += info.Size()
		}
		return nil
	})
	manifestFiles, _ := filepath.Glob(filepath.Join(s.manifestsDir, "*.json"))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"unique_chunks":      chunkCount,
		"unique_chunk_bytes": chunkBytes,
		"files_stored":       len(manifestFiles),
	})
}
