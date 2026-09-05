package casserver

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"

	"xet-server/internal/shardformat"
)

// handleUploadShard implements POST /v1/shards: parse the serialized
// shard and merge its file-reconstruction entries into the server's
// in-memory index, keyed by file hash. xet-core reports 0 (already
// exists) vs 1 (SyncPerformed); this server has no separate shard dedup
// store, so it always reports 1 once the shard parses successfully.
func (s *Server) handleUploadShard(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		httpError(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	shard, err := shardformat.ReadShard(bytes.NewReader(body))
	if err != nil {
		httpError(w, "malformed shard: "+err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
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
			s.sha256ToXet[sha256Hex] = f.Header.FileHash
		}
		slog.Debug("shard file indexed", "fileHash", f.Header.FileHash.Hex(), "hasMetadataExt", hasExt, "sha256Hex", sha256Hex)
	}
	s.mu.Unlock()

	writeJSON(w, uploadShardResponse{Result: 1})
}
