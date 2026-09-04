package casserver

import (
	"bytes"
	"io"
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
	}
	s.mu.Unlock()

	writeJSON(w, uploadShardResponse{Result: 1})
}
