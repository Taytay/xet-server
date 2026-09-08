package casserver

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"

	"xet-server/internal/shardformat"
)

// maxShardBytes caps a single shard upload's body size. Shards are
// metadata (file/xorb info entries), bounded by chunk count rather than
// file size — even a shard describing a maximally-chunked maxXorbBytes
// xorb (128 MiB / DefaultMinSize 4 KiB ≈ 32K chunks) stays well under a
// few MB. This cap exists to bound handleUploadShard's io.ReadAll, which
// (unlike the xorb path) buffers the whole body in memory rather than
// streaming to a temp file — shard bodies are always small enough that
// this is fine, but an unbounded ReadAll still lets a hostile client force
// arbitrary memory growth by simply not capping Content-Length.
const maxShardBytes = 16 * 1024 * 1024

// chunkDedupPrefix is the only prefix xet-core's real CAS API accepts for
// GET /v1/chunks/{prefix}/{hash} (see openapi/cas.openapi.yaml's
// PrefixGlobalDedupeParam) — distinct from xorbPrefix ("default"), which
// is for a different endpoint.
const chunkDedupPrefix = "default-merkledb"

// handleUploadShard implements POST /v1/shards: parse the serialized
// shard and merge its file-reconstruction entries into the server's
// in-memory index, keyed by file hash. xet-core reports 0 (already
// exists) vs 1 (SyncPerformed); this server has no separate shard dedup
// store, so it always reports 1 once the shard parses successfully.
//
// Every chunk hash referenced by the shard's xorb-info section is also
// indexed against this shard's raw uploaded bytes, backing the global
// chunk-dedup lookup (GET /v1/chunks/{prefix}/{hash} — see
// handleChunkDedup): the real wire contract for that endpoint is "return
// the shard bytes that reference this chunk," which a real client parses
// itself to discover chunks it can dedup against without re-uploading —
// see docs/PROTOCOL.md's global-dedup section for the full story of how
// this was confirmed against xet-core's own client source.
func (s *Server) handleUploadShard(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxShardBytes)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		httpError(w, "read body (exceeds max shard size or connection error): "+err.Error(), http.StatusRequestEntityTooLarge)
		return
	}

	shard, err := shardformat.ReadShard(bytes.NewReader(body))
	if err != nil {
		httpError(w, "malformed shard: "+err.Error(), http.StatusBadRequest)
		return
	}

	s.fileReconMu.Lock()
	for _, f := range shard.Files {
		s.fileRecon[f.Header.FileHash] = f.Entries
		hasExt := f.MetadataExt != nil
		var sha256Hex string
		if f.MetadataExt != nil {
			// FileMetadataExt.SHA256 reuses the 32-byte merklehash.Hash type
			// for storage, but real hf_xet clients write it through the same
			// byte-order transform as a genuine Merkle hash's Hex() (word
			// reversal per 8-byte little-endian group) — confirmed by
			// capturing a real upload and comparing MetadataExt.SHA256's raw
			// bytes against the plain SHA-256 in the commit payload's
			// lfsFile.oid: Hex() of the former equals the latter exactly.
			// A raw-byte hex encode (hex.EncodeToString(Bytes())) silently
			// produces a different string that never matches oid, so the
			// resolve/download path's sha256->Xet-hash lookup always missed.
			sha256Hex = f.MetadataExt.SHA256.Hex()
			s.sha256Mu.Lock()
			s.sha256ToXet[sha256Hex] = f.Header.FileHash
			s.sha256Mu.Unlock()
		}
		slog.Debug("shard file indexed", "fileHash", f.Header.FileHash.Hex(), "hasMetadataExt", hasExt, "sha256Hex", sha256Hex)
	}
	s.fileReconMu.Unlock()

	var chunkCount int
	s.chunkDedupMu.Lock()
	for _, x := range shard.Xorbs {
		for _, c := range x.Chunks {
			// First shard to reference a given chunk hash wins; later
			// shards referencing the same (already-deduplicated) chunk
			// don't need to replace it — any shard referencing the chunk
			// is equally valid for a client's dedup purposes.
			if _, exists := s.chunkHashToShard[c.ChunkHash]; !exists {
				s.chunkHashToShard[c.ChunkHash] = body
				chunkCount++
			}
		}
	}
	s.chunkDedupMu.Unlock()
	slog.Debug("shard chunk-dedup index updated", "newChunkEntries", chunkCount)

	writeJSON(w, uploadShardResponse{Result: 1})
}
