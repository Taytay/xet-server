package proxyhub

// commit.go: POST /api/{repo_type}s/{repo_id}/commit/{revision} (ndjson
// body) and POST /api/{repo_type}s/{repo_id}/preupload/{revision} —
// both relayed to the real Hub write-through, uncached — only the real
// Hub can accept a real commit or negotiate a real upload mode. The
// commit's effect is mirrored into Embedded (via IngestFile/
// IngestCommit) so a subsequent read of the same data is already a
// local hit.

import (
	"bufio"
	"encoding/json"
	"net/http"

	"xet-server/internal/auth"
	"xet-server/internal/hfclient"
	"xet-server/internal/merklehash"
)

// commitLine mirrors one line of the ndjson commit payload
// huggingface_hub sends — see internal/hubserver/commit.go's commitLine
// for the identical shape this duplicates.
type commitLine struct {
	Key   string `json:"key"`
	Value struct {
		Path string `json:"path"`
		OID  string `json:"oid"`
		Size int64  `json:"size"`
	} `json:"value"`
}

// handleCommit parses the caller's ndjson commit payload, relays it to
// the real Hub, and — on success — mirrors every lfsFile entry into
// Embedded using the REAL upstream commit OID (never a fabricated one;
// see hubserver.Server.IngestCommit's doc comment on why that matters).
// Files are ingested with a zero XetHash (not yet known — a fresh
// commit's Xet hash is only ever backfilled by a real shard upload,
// which this proxy relays but does not itself parse independent of a
// real client's own upload — see internal/proxycas's handleUploadShard
// for where that actually happens); a subsequent resolve of one of
// these files still works via Embedded's own CAS bridge once that
// shard's Xet hash becomes known, exactly as a real hubserver would.
func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request, repoType, repoID, revision string) {
	var files []hfclient.CommitFile
	var ingestPaths []commitLine
	scanner := bufio.NewScanner(r.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var cl commitLine
		if err := json.Unmarshal(line, &cl); err != nil {
			httpErrorJSON(w, "invalid ndjson line: "+err.Error(), http.StatusBadRequest)
			return
		}
		if cl.Key != "lfsFile" {
			continue
		}
		files = append(files, hfclient.CommitFile{Path: cl.Value.Path, OID: cl.Value.OID, Size: cl.Value.Size})
		ingestPaths = append(ingestPaths, cl)
	}
	if err := scanner.Err(); err != nil {
		httpErrorJSON(w, "read commit body: "+err.Error(), http.StatusBadRequest)
		return
	}

	result, err := s.Hub.Commit(r.Context(), auth.CredentialFromRequest(r), repoType, repoID, revision, files)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}

	if !s.NoCache {
		for _, cl := range ingestPaths {
			s.Embedded.IngestFile(repoType, repoID, revision, cl.Value.Path, cl.Value.OID, cl.Value.Size, merklehash.Hash{})
		}
		if result.CommitOID != "" {
			s.Embedded.IngestCommit(repoType, repoID, revision, result.CommitOID)
		}
	}

	writeJSON(w, result)
}

// preuploadRequest is proxyhub's own copy of hubserver.preuploadRequest
// — duplicated (not imported) for the same reason as splitRepoPath (see
// its note in proxyhub.go): decodes the caller's own preupload request
// body to relay to the real Hub, a distinct concern from hfclient.
// PreuploadFile (the shape THIS package sends upstream).
type preuploadRequest struct {
	Files []struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
	} `json:"files"`
}

// handlePreupload relays the caller's file list to the real Hub's
// preupload negotiation, uncached — the negotiated upload mode can
// depend on upstream-side state this proxy has no way to reason about
// locally.
func (s *Server) handlePreupload(w http.ResponseWriter, r *http.Request, repoType, repoID, revision string) {
	var req preuploadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpErrorJSON(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	files := make([]hfclient.PreuploadFile, len(req.Files))
	for i, f := range req.Files {
		files[i] = hfclient.PreuploadFile{Path: f.Path, Size: f.Size}
	}

	results, err := s.Hub.Preupload(r.Context(), auth.CredentialFromRequest(r), repoType, repoID, revision, files)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, map[string]any{"files": results})
}
