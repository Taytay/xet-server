package casserver

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"

	"xet-server/internal/bg4"
	"xet-server/internal/lz4"
	"xet-server/internal/merklehash"
	"xet-server/internal/xorbformat"
)

func httpError(w http.ResponseWriter, msg string, code int) {
	log.Printf("casserver: %d %s", code, msg)
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

// handleUploadXorb implements POST /v1/xorbs/{prefix}/{hash}: store the
// serialized xorb bytes as-is, then independently reconstruct the xorb's
// chunk hash list and footer by scanning chunk headers and decompressing
// each payload — real hf_xet clients upload xorbs *without* a footer
// ("XORBs are sent without footer - the server/client reconstructs it from
// chunk data", per xet-core's file_upload_session.rs) — and verify the
// claimed hash against the resulting Merkle aggregation.
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

	body, err := io.ReadAll(r.Body)
	if err != nil {
		httpError(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	entries, err := xorbformat.ScanChunks(bytes.NewReader(body))
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
		payload := body[e.DataOffset : e.DataOffset+int64(e.Header.CompressedLength)]
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

	written, err := s.xorbs.Put(r.Context(), claimedHash.Hex(), body)
	if err != nil {
		httpError(w, "store xorb: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	s.xorbFooters[claimedHash] = xorbformat.FooterV1{
		XorbHash:             computedHash,
		ChunkHashes:          chunkHashes,
		ChunkBoundaryOffsets: boundaryOffsets,
		UnpackedChunkOffsets: unpackedOffsets,
		NumChunks:            uint32(len(entries)),
	}
	s.xorbRawLength[claimedHash] = int64(len(body))
	s.mu.Unlock()

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

	s.mu.RLock()
	total, known := s.xorbRawLength[hash]
	s.mu.RUnlock()
	if !known {
		http.NotFound(w, r)
		return
	}

	start, end, hasRange, err := parseByteRange(r.Header.Get("Range"), total)
	if err != nil {
		httpError(w, err.Error(), http.StatusRequestedRangeNotSatisfiable)
		return
	}

	var data []byte
	if hasRange {
		data, err = s.xorbs.GetRange(r.Context(), hash.Hex(), start, end-start+1)
	} else {
		data, err = s.xorbs.Get(r.Context(), hash.Hex())
	}
	if err != nil {
		httpError(w, "fetch xorb: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if hasRange {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.WriteHeader(http.StatusPartialContent)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(int64(len(data)), 10))
	w.Write(data)
}

func (s *Server) handleHeadXorb(w http.ResponseWriter, r *http.Request) {
	hash, err := hexParam(r, "hash")
	if err != nil {
		httpError(w, "invalid hash", http.StatusBadRequest)
		return
	}
	s.mu.RLock()
	total, known := s.xorbRawLength[hash]
	s.mu.RUnlock()
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
