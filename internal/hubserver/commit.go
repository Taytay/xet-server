package hubserver

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
)

// commitLine mirrors one line of the ndjson commit payload
// huggingface_hub sends: either {"key":"header",...} or
// {"key":"lfsFile","value":{"path":...,"oid":...,"size":...}} for a
// Xet/LFS-backed file (this server never sees "file" — small inline
// content — since the test client only ever uploads via Xet).
type commitLine struct {
	Key   string `json:"key"`
	Value struct {
		Path string `json:"path"`
		OID  string `json:"oid"`
		Size int64  `json:"size"`
	} `json:"value"`
}

type commitResponse struct {
	CommitOID      string `json:"commitOid"`
	CommitURL      string `json:"commitUrl"`
	PullRequestURL string `json:"pullRequestUrl,omitempty"`
}

// handleCommit implements POST /api/{repo_type}s/{repo_id}/commit/{revision}:
// parses the ndjson commit payload and records each lfsFile entry's path
// and declared SHA-256 (oid). The Xet/Merkle file hash needed to actually
// serve the file on download is backfilled lazily on first resolve
// request, by asking the paired CAS server (see resolve.go) — the commit
// payload only ever carries the plain SHA-256, never the Xet hash.
func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request, repoType, repoID, revision string) {
	_ = revision // single implicit "main" revision; not tracked separately
	rs := s.getOrCreateRepo(repoType, repoID)

	scanner := bufio.NewScanner(r.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var fileCount int
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
		s.mu.Lock()
		rs.files[cl.Value.Path] = &fileRef{
			Path:      cl.Value.Path,
			SHA256Hex: cl.Value.OID,
			Size:      cl.Value.Size,
		}
		s.mu.Unlock()
		fileCount++
	}
	if err := scanner.Err(); err != nil {
		httpErrorJSON(w, "read commit body: "+err.Error(), http.StatusBadRequest)
		return
	}

	oid, err := randomCommitOID()
	if err != nil {
		httpErrorJSON(w, "generate commit oid: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	rs.commitOID = oid
	rs.commitSeen = true
	s.mu.Unlock()

	writeJSON(w, commitResponse{
		CommitOID: oid,
		CommitURL: "/" + repoID + "/commit/" + oid,
	})
}

func randomCommitOID() (string, error) {
	b := make([]byte, 20) // matches a git SHA-1 commit OID's byte length
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

type preuploadRequest struct {
	Files []struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
	} `json:"files"`
}

type preuploadFileInfo struct {
	Path         string `json:"path"`
	UploadMode   string `json:"uploadMode"`
	ShouldIgnore bool   `json:"shouldIgnore"`
	OID          string `json:"oid,omitempty"`
}

type preuploadResponse struct {
	Files []preuploadFileInfo `json:"files"`
}

// handlePreupload implements POST /api/{repo_type}s/{repo_id}/preupload/{revision}:
// tells huggingface_hub which upload path to use per file. Every non-empty
// file is routed through "lfs" (which is what triggers the Xet upload path
// once hf_xet is installed) — this server only ever expects Xet uploads,
// so there is no separate small-file/regular-blob path to model.
func (s *Server) handlePreupload(w http.ResponseWriter, r *http.Request) {
	var req preuploadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpErrorJSON(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	resp := preuploadResponse{}
	for _, f := range req.Files {
		mode := "lfs"
		if f.Size == 0 {
			mode = "regular"
		}
		resp.Files = append(resp.Files, preuploadFileInfo{
			Path:       f.Path,
			UploadMode: mode,
		})
	}
	writeJSON(w, resp)
}
