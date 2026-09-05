package casserver

import (
	"context"
	"fmt"
	"net/http"

	"xet-server/internal/merklehash"
	"xet-server/internal/storage"
)

// handleReconstructionV1 implements GET /v1/reconstructions/{file_id}: looks
// up the file's shard-derived reconstruction entries, clips them to the
// requested byte range (via the standard HTTP Range header, inclusive end
// — real clients page through large files by re-requesting successively
// higher windows), and for each term resolves the referenced xorb's
// chunk-index range into a byte range (using that xorb's footer, indexed
// at upload time), then emits a fetch_info URL for each distinct xorb
// touched.
//
// Per the client's documented contract, a Range header whose start is at or
// past the file's end must get a 416 (mapped by hf_xet to "no more data,
// stop paging"), not an empty 200 — an empty-but-200 response would leave
// the client's sequential writer waiting for a term it will never receive.
func (s *Server) handleReconstructionV1(w http.ResponseWriter, r *http.Request) {
	fileID, err := hexParam(r, "file_id")
	if err != nil {
		httpError(w, "invalid file_id", http.StatusBadRequest)
		return
	}

	s.fileReconMu.RLock()
	entries, ok := s.fileRecon[fileID]
	s.fileReconMu.RUnlock()
	if !ok {
		http.NotFound(w, r)
		return
	}

	var fileSize int64
	for _, e := range entries {
		fileSize += int64(e.UnpackedSegmentBytes)
	}

	rangeStart, rangeEnd, hasRange, err := parseByteRange(r.Header.Get("Range"), fileSize)
	if err != nil {
		httpError(w, err.Error(), http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if !hasRange {
		rangeStart, rangeEnd = 0, fileSize-1
	}

	baseURL := baseURLFromRequest(r)

	resp := reconstructionResponseV1{
		FetchInfo: make(map[string][]fetchInfoEntry),
	}

	var fileOffset int64
	firstTerm := true
	for _, e := range entries {
		termStart := fileOffset
		termEnd := fileOffset + int64(e.UnpackedSegmentBytes) // exclusive
		fileOffset = termEnd

		// Skip terms entirely outside the requested window.
		if termEnd <= rangeStart || termStart > rangeEnd {
			continue
		}
		if firstTerm {
			resp.OffsetIntoFirstRange = rangeStart - termStart
			firstTerm = false
		}

		s.xorbMu.RLock()
		footer, known := s.xorbFooters[e.XorbHash]
		s.xorbMu.RUnlock()
		if !known {
			httpError(w, fmt.Sprintf("reconstruction references unknown xorb %s", e.XorbHash.Hex()), http.StatusInternalServerError)
			return
		}

		physStart := int64(0)
		if e.ChunkIndexStart > 0 {
			physStart = int64(footer.ChunkBoundaryOffsets[e.ChunkIndexStart-1])
		}
		physEnd := int64(footer.ChunkBoundaryOffsets[e.ChunkIndexEnd-1])

		resp.Terms = append(resp.Terms, reconstructionTerm{
			Hash:           e.XorbHash.Hex(),
			Range:          indexRange{Start: e.ChunkIndexStart, End: e.ChunkIndexEnd},
			UnpackedLength: e.UnpackedSegmentBytes,
		})

		fetchURL, err := s.xorbFetchURL(r.Context(), e.XorbHash, baseURL)
		if err != nil {
			httpError(w, "build fetch URL: "+err.Error(), http.StatusInternalServerError)
			return
		}

		resp.FetchInfo[e.XorbHash.Hex()] = append(resp.FetchInfo[e.XorbHash.Hex()], fetchInfoEntry{
			URL:      fetchURL,
			URLRange: byteRange{Start: physStart, End: physEnd - 1},
			Range:    indexRange{Start: e.ChunkIndexStart, End: e.ChunkIndexEnd},
		})
	}

	writeJSON(w, resp)
}

// xorbFetchURL returns a presigned URL if the storage backend supports it
// (storage.URLPresigner — e.g. S3/MinIO), otherwise a URL pointing back at
// this server's own byte-serving endpoint.
func (s *Server) xorbFetchURL(ctx context.Context, xorbHash merklehash.Hash, baseURL string) (string, error) {
	if presigner, ok := s.xorbs.(storage.URLPresigner); ok {
		return presigner.PresignGet(ctx, xorbHash.Hex(), 3600)
	}
	return fmt.Sprintf("%s/v1/xorbs/%s/%s", baseURL, xorbPrefix, xorbHash.Hex()), nil
}

func baseURLFromRequest(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s", scheme, r.Host)
}

// handleReconstructionV2 always signals "not supported, fall back to V1"
// per the OpenAPI spec's documented client contract, rather than
// implementing the multi-range-optimized V2 response shape.
func (s *Server) handleReconstructionV2(w http.ResponseWriter, r *http.Request) {
	httpError(w, "V2 reconstruction not implemented; fall back to V1", http.StatusNotImplemented)
}

// handleChunkDedup always reports the chunk as untracked: this server
// maintains no global chunk-deduplication index.
func (s *Server) handleChunkDedup(w http.ResponseWriter, r *http.Request) {
	http.NotFound(w, r)
}

// handleTelemetry is a fire-and-forget ack: the real client never retries
// or inspects the response body.
func (s *Server) handleTelemetry(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}
