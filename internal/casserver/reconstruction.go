package casserver

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"xet-server/internal/merklehash"
	"xet-server/internal/shardformat"
	"xet-server/internal/storage"
	"xet-server/internal/xorbformat"
)

// reconstructionWindow looks up fileID's shard-derived reconstruction
// entries and clips the requested byte range against the file's total
// size, shared by both handleReconstructionV1 and handleReconstructionV2
// since both need the identical entries/range-window before diverging on
// how they shape the fetch-info portion of their response.
//
// found=false means fileID is unknown (caller should 404). found=true
// with a non-nil err means fileID is known but rangeHeader was
// unsatisfiable (caller should 416) — distinguishing these two cases is
// why this doesn't just return a single error.
func (s *Server) reconstructionWindow(fileID merklehash.Hash, rangeHeader string) (entries []shardformat.FileDataSequenceEntry, rangeStart, rangeEnd int64, found bool, err error) {
	s.fileReconMu.RLock()
	entries, found = s.fileRecon[fileID]
	s.fileReconMu.RUnlock()
	if !found {
		return nil, 0, 0, false, nil
	}

	var fileSize int64
	for _, e := range entries {
		fileSize += int64(e.UnpackedSegmentBytes)
	}

	start, end, hasRange, rangeErr := parseByteRange(rangeHeader, fileSize)
	if rangeErr != nil {
		return entries, 0, 0, true, rangeErr
	}
	if !hasRange {
		start, end = 0, fileSize-1
	}
	return entries, start, end, true, nil
}

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

	entries, rangeStart, rangeEnd, found, err := s.reconstructionWindow(fileID, r.Header.Get("Range"))
	if !found {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		httpError(w, err.Error(), http.StatusRequestedRangeNotSatisfiable)
		return
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

		_, physStart, physEnd, ok := s.xorbFooterAndPhysicalRange(e)
		if !ok {
			httpError(w, fmt.Sprintf("reconstruction references unknown xorb %s", e.XorbHash.Hex()), http.StatusInternalServerError)
			return
		}

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

// xorbFooterAndPhysicalRange looks up e.XorbHash's footer, bumps its
// last-access time (see the comment on the call site this replaces for
// why), and computes the physical (compressed+header) byte range e's
// chunk-index range occupies within that xorb — logic shared by both V1
// and V2 reconstruction, since both need the same physical byte range per
// term, just packaged into differently-shaped responses.
func (s *Server) xorbFooterAndPhysicalRange(e shardformat.FileDataSequenceEntry) (footer xorbformat.FooterV1, physStart, physEnd int64, ok bool) {
	s.xorbMu.RLock()
	f, known := s.xorbFooters[e.XorbHash]
	s.xorbMu.RUnlock()
	if !known {
		return xorbformat.FooterV1{}, 0, 0, false
	}
	// A client requesting reconstruction is about to fetch this xorb —
	// either from our own byte-serving endpoint (which also bumps this on
	// the actual fetch) or from a presigned URL, which we'd otherwise
	// never observe at all. Bumping here means eviction sees "about to be
	// needed" even in the presigned-URL case.
	s.xorbMu.Lock()
	s.xorbLastAccess[e.XorbHash] = time.Now()
	s.xorbMu.Unlock()

	physStart = 0
	if e.ChunkIndexStart > 0 {
		physStart = int64(f.ChunkBoundaryOffsets[e.ChunkIndexStart-1])
	}
	physEnd = int64(f.ChunkBoundaryOffsets[e.ChunkIndexEnd-1])
	return f, physStart, physEnd, true
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

// handleReconstructionV2 implements GET /v2/reconstructions/{file_id}: the
// multi-range-optimized reconstruction response — same terms/range-window
// logic as V1 (see reconstructionWindow), but groups every chunk/byte
// range touched for a given xorb under that xorb's single fetch URL,
// rather than V1's one fetchInfoEntry per term (so a file whose
// reconstruction touches the same xorb across several non-contiguous
// terms gets one xorb entry with multiple ranges, not several
// nearly-identical entries differing only by range). Response shape
// verified against xet-core's real QueryReconstructionResponseV2 struct
// and its update_260316_v2_reconstruction_multirange.md changelog — see
// docs/PROTOCOL.md's dedicated section on this endpoint for the story of
// why the "V1 vs V2" difference is purely response shape, not different
// underlying data.
func (s *Server) handleReconstructionV2(w http.ResponseWriter, r *http.Request) {
	fileID, err := hexParam(r, "file_id")
	if err != nil {
		httpError(w, "invalid file_id", http.StatusBadRequest)
		return
	}

	entries, rangeStart, rangeEnd, found, err := s.reconstructionWindow(fileID, r.Header.Get("Range"))
	if !found {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		httpError(w, err.Error(), http.StatusRequestedRangeNotSatisfiable)
		return
	}

	baseURL := baseURLFromRequest(r)

	resp := reconstructionResponseV2{
		Xorbs: make(map[string][]xorbMultiRangeFetch),
	}
	// One fetch URL per xorb hash, reused across every term that touches
	// it — computed lazily on first sight of each hash rather than
	// upfront, since most files only ever touch a handful of xorbs.
	fetchURLByXorb := make(map[string]string)

	var fileOffset int64
	firstTerm := true
	for _, e := range entries {
		termStart := fileOffset
		termEnd := fileOffset + int64(e.UnpackedSegmentBytes) // exclusive
		fileOffset = termEnd

		if termEnd <= rangeStart || termStart > rangeEnd {
			continue
		}
		if firstTerm {
			resp.OffsetIntoFirstRange = rangeStart - termStart
			firstTerm = false
		}

		_, physStart, physEnd, ok := s.xorbFooterAndPhysicalRange(e)
		if !ok {
			httpError(w, fmt.Sprintf("reconstruction references unknown xorb %s", e.XorbHash.Hex()), http.StatusInternalServerError)
			return
		}

		resp.Terms = append(resp.Terms, reconstructionTerm{
			Hash:           e.XorbHash.Hex(),
			Range:          indexRange{Start: e.ChunkIndexStart, End: e.ChunkIndexEnd},
			UnpackedLength: e.UnpackedSegmentBytes,
		})

		xorbHex := e.XorbHash.Hex()
		fetchURL, cached := fetchURLByXorb[xorbHex]
		if !cached {
			fetchURL, err = s.xorbFetchURL(r.Context(), e.XorbHash, baseURL)
			if err != nil {
				httpError(w, "build fetch URL: "+err.Error(), http.StatusInternalServerError)
				return
			}
			fetchURLByXorb[xorbHex] = fetchURL
		}

		descriptor := xorbRangeDescriptor{
			Chunks: indexRange{Start: e.ChunkIndexStart, End: e.ChunkIndexEnd},
			Bytes:  byteRange{Start: physStart, End: physEnd - 1},
		}

		fetches := resp.Xorbs[xorbHex]
		if len(fetches) == 0 || fetches[0].URL != fetchURL {
			// Either the first range seen for this xorb, or (shouldn't
			// happen in practice, since fetchURLByXorb caches one URL per
			// xorb per response) a different URL than what's already
			// there — start a new XorbMultiRangeFetch entry.
			resp.Xorbs[xorbHex] = append(fetches, xorbMultiRangeFetch{URL: fetchURL, Ranges: []xorbRangeDescriptor{descriptor}})
		} else {
			fetches[0].Ranges = append(fetches[0].Ranges, descriptor)
			resp.Xorbs[xorbHex] = fetches
		}
	}

	writeJSON(w, resp)
}

// handleChunkDedup implements GET /v1/chunks/{prefix}/{hash}: the real
// wire contract (per xet-core's openapi/cas.openapi.yaml and
// query_for_global_dedup_shard in xet-core's remote_client.rs) is to
// return the raw bytes of whichever previously-uploaded shard referenced
// this chunk hash — the client parses that shard itself
// (filter_cas_chunks_for_global_dedup) to discover which of its own
// chunks it can skip re-uploading. 404 if no uploaded shard has ever
// referenced this chunk hash.
func (s *Server) handleChunkDedup(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("prefix") != chunkDedupPrefix {
		httpError(w, "unsupported chunk-dedup prefix", http.StatusBadRequest)
		return
	}
	hash, err := hexParam(r, "hash")
	if err != nil {
		httpError(w, "invalid hash", http.StatusBadRequest)
		return
	}

	s.chunkDedupMu.RLock()
	shardBytes, known := s.chunkHashToShard[hash]
	s.chunkDedupMu.RUnlock()
	if !known {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(shardBytes)))
	w.Write(shardBytes)
}

// handleTelemetry is a fire-and-forget ack: the real client never retries
// or inspects the response body.
func (s *Server) handleTelemetry(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}
