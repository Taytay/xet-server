package casserver

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"

	"xet-server/internal/bg4"
	"xet-server/internal/lz4"
	"xet-server/internal/merklehash"
	"xet-server/internal/xorbformat"
)

// httpError writes an HTTP error response and logs it at a level matching
// its cause: a 5xx reflects a server-side fault worth surfacing at Warn by
// default, while a 4xx is a client protocol/input error — expected to
// happen under normal operation (a bad hash, a truncated upload) and only
// useful for debugging, so it logs at Debug to avoid every malformed
// request from a client spamming the default log level.
func httpError(w http.ResponseWriter, msg string, code int) {
	if code >= 500 {
		slog.Warn("casserver: request failed", "status", code, "error", msg)
	} else {
		slog.Debug("casserver: request rejected", "status", code, "error", msg)
	}
	http.Error(w, msg, code)
}

// decompressChunkPayload returns the uncompressed bytes of one chunk's
// payload per its declared compression scheme. Real hf_xet clients upload
// xorbs without a footer (chunk metadata is reconstructed by the server —
// see the comment on handleUploadXorb), so this is the only way to obtain
// a chunk's true content and independently verify its claimed hash.
func decompressChunkPayload(scheme xorbformat.CompressionScheme, payload []byte, uncompressedLen uint32) ([]byte, error) {
	switch scheme {
	case xorbformat.CompressionNone:
		if uint32(len(payload)) != uncompressedLen {
			return nil, fmt.Errorf("uncompressed chunk length %d does not match header's uncompressed_length %d", len(payload), uncompressedLen)
		}
		return payload, nil
	case xorbformat.CompressionLZ4:
		decoded, err := lz4.DecompressFrame(payload)
		if err != nil {
			return nil, err
		}
		if uint32(len(decoded)) != uncompressedLen {
			return nil, fmt.Errorf("LZ4-decoded chunk length %d does not match header's uncompressed_length %d", len(decoded), uncompressedLen)
		}
		return decoded, nil
	case xorbformat.CompressionByteGrouping4LZ4:
		decoded, err := lz4.DecompressFrame(payload)
		if err != nil {
			return nil, err
		}
		ungrouped := bg4.Reverse(decoded)
		if uint32(len(ungrouped)) != uncompressedLen {
			return nil, fmt.Errorf("BG4+LZ4-decoded chunk length %d does not match header's uncompressed_length %d", len(ungrouped), uncompressedLen)
		}
		return ungrouped, nil
	default:
		return nil, fmt.Errorf("unsupported compression scheme %d", scheme)
	}
}

// maxXorbBytes caps a single xorb upload's body size. Real xet-core targets
// ~64 MiB per xorb before cutting a new one (MAX_XORB_BYTES in
// xet-core's constants), so this is generous headroom above what a
// well-behaved client ever sends in one xorb — its purpose here is purely
// to bound how large a temp file a single hostile/misbehaving request can
// force the server to stage, not to constrain normal traffic.
const maxXorbBytes = 128 * 1024 * 1024

// handleUploadXorb implements POST /v1/xorbs/{prefix}/{hash}: stream the
// serialized xorb body to a temp file (xorbformat.ScanChunks needs seek,
// which an HTTP request body doesn't support), then independently
// reconstruct the xorb's chunk hash list and footer by scanning chunk
// headers and decompressing each payload — real hf_xet clients upload
// xorbs *without* a footer ("XORBs are sent without footer - the
// server/client reconstructs it from chunk data", per xet-core's
// file_upload_session.rs) — and verify the claimed hash against the
// resulting Merkle aggregation. The temp file is only handed to the
// storage backend (which streams it onward) once the whole body has been
// received and validated, so a client that disconnects mid-upload never
// leaves a partial xorb stored.
func (s *Server) handleUploadXorb(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("prefix") != xorbPrefix {
		httpError(w, "unsupported xorb prefix", http.StatusBadRequest)
		return
	}
	claimedHash, err := hexParam(r, "hash")
	if err != nil {
		httpError(w, "invalid hash", http.StatusBadRequest)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxXorbBytes)

	tmp, err := os.CreateTemp("", "xet-xorb-upload-*")
	if err != nil {
		httpError(w, "stage upload: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	size, err := io.Copy(tmp, r.Body)
	if err != nil {
		httpError(w, "read body (exceeds max xorb size or connection error): "+err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		httpError(w, "malformed xorb: "+err.Error(), http.StatusInternalServerError)
		return
	}

	entries, err := xorbformat.ScanChunks(tmp)
	if err != nil {
		httpError(w, "malformed xorb: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(entries) == 0 {
		httpError(w, "malformed xorb: no chunks found", http.StatusBadRequest)
		return
	}

	var chunkHashes []merklehash.Hash
	var chunkEntries []merklehash.ChunkEntry
	var boundaryOffsets, unpackedOffsets []uint32
	for i, e := range entries {
		payload := make([]byte, e.Header.CompressedLength)
		if _, err := tmp.ReadAt(payload, e.DataOffset); err != nil {
			httpError(w, fmt.Sprintf("chunk %d: read payload: %s", i, err), http.StatusBadRequest)
			return
		}
		decoded, err := decompressChunkPayload(e.Header.CompressionScheme, payload, e.Header.UncompressedLength)
		if err != nil {
			httpError(w, fmt.Sprintf("chunk %d: %s", i, err), http.StatusBadRequest)
			return
		}
		h := merklehash.ComputeDataHash(decoded)
		chunkHashes = append(chunkHashes, h)
		chunkEntries = append(chunkEntries, merklehash.ChunkEntry{Hash: h, Size: uint64(len(decoded))})
		boundaryOffsets = append(boundaryOffsets, uint32(e.DataOffset+int64(e.Header.CompressedLength)))
		var unpacked uint32
		if len(unpackedOffsets) > 0 {
			unpacked = unpackedOffsets[len(unpackedOffsets)-1]
		}
		unpackedOffsets = append(unpackedOffsets, unpacked+e.Header.UncompressedLength)
	}

	computedHash := merklehash.XorbHash(chunkEntries)
	if computedHash != claimedHash {
		httpError(w, "xorb hash in URL does not match hash computed from chunk contents", http.StatusBadRequest)
		return
	}

	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		httpError(w, "store xorb: "+err.Error(), http.StatusInternalServerError)
		return
	}
	written, err := s.xorbs.Put(r.Context(), claimedHash.Hex(), tmp, size)
	if err != nil {
		httpError(w, "store xorb: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.xorbMu.Lock()
	s.xorbFooters[claimedHash] = xorbformat.FooterV1{
		XorbHash:             computedHash,
		ChunkHashes:          chunkHashes,
		ChunkBoundaryOffsets: boundaryOffsets,
		UnpackedChunkOffsets: unpackedOffsets,
		NumChunks:            uint32(len(entries)),
	}
	s.xorbRawLength[claimedHash] = size
	s.xorbMu.Unlock()

	writeJSON(w, uploadXorbResponse{WasInserted: written})
}

// handleFetchXorb implements the byte-serving side of a fetch_info URL:
// GET /v1/xorbs/{prefix}/{hash}, honoring an HTTP Range header the same
// way a presigned S3 URL would.
func (s *Server) handleFetchXorb(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("prefix") != xorbPrefix {
		httpError(w, "unsupported xorb prefix", http.StatusBadRequest)
		return
	}
	hash, err := hexParam(r, "hash")
	if err != nil {
		httpError(w, "invalid hash", http.StatusBadRequest)
		return
	}

	s.xorbMu.RLock()
	total, known := s.xorbRawLength[hash]
	s.xorbMu.RUnlock()
	if !known {
		http.NotFound(w, r)
		return
	}

	start, end, hasRange, err := parseByteRange(r.Header.Get("Range"), total)
	if err != nil {
		httpError(w, err.Error(), http.StatusRequestedRangeNotSatisfiable)
		return
	}

	contentLength := total
	var data io.ReadCloser
	if hasRange {
		contentLength = end - start + 1
		data, err = s.xorbs.GetRange(r.Context(), hash.Hex(), start, contentLength)
	} else {
		data, err = s.xorbs.Get(r.Context(), hash.Hex())
	}
	if err != nil {
		httpError(w, "fetch xorb: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer data.Close()

	if hasRange {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.WriteHeader(http.StatusPartialContent)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(contentLength, 10))
	io.Copy(w, data)
}

func (s *Server) handleHeadXorb(w http.ResponseWriter, r *http.Request) {
	hash, err := hexParam(r, "hash")
	if err != nil {
		httpError(w, "invalid hash", http.StatusBadRequest)
		return
	}
	s.xorbMu.RLock()
	total, known := s.xorbRawLength[hash]
	s.xorbMu.RUnlock()
	if !known {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
	w.WriteHeader(http.StatusOK)
}

// parseByteRange parses a "bytes=start-end" Range header (single range
// only, inclusive end per HTTP semantics). Returns hasRange=false if
// header is empty.
func parseByteRange(header string, total int64) (start, end int64, hasRange bool, err error) {
	if header == "" {
		return 0, 0, false, nil
	}
	const prefix = "bytes="
	if len(header) < len(prefix) || header[:len(prefix)] != prefix {
		return 0, 0, false, fmt.Errorf("malformed Range header")
	}
	spec := header[len(prefix):]
	var s, e int64
	n, err := fmt.Sscanf(spec, "%d-%d", &s, &e)
	if err != nil || n != 2 {
		return 0, 0, false, fmt.Errorf("malformed Range header")
	}
	if s < 0 || e < s || s >= total {
		return 0, 0, false, fmt.Errorf("range not satisfiable")
	}
	if e >= total {
		e = total - 1
	}
	return s, e, true, nil
}
