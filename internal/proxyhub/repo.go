package proxyhub

// repo.go: POST /api/repos/create (uncached write-through), GET
// /api/{repo_type}s/{repo_id}/revision/{revision} (cached read,
// delegating to Embedded), and POST
// /api/{repo_type}s/{repo_id}/branch/{branch} (uncached write-through).

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"xet-server/internal/auth"
)

// createRepoRequest is proxyhub's own copy of hubserver.
// createRepoRequest — duplicated (not imported) for the same reason as
// splitRepoPath (see its note in proxyhub.go): decodes the real Hub's
// wire request shape, which this package relays through unmodified
// rather than delegating to Embedded (only the real Hub can accept a
// repo creation).
type createRepoRequest struct {
	Name         string `json:"name"`
	Organization string `json:"organization"`
	Type         string `json:"type"`
}

// handleCreateRepo relays POST /api/repos/create to the real Hub
// unconditionally, then mirrors the effect into Embedded (non-fatal on
// failure — the caller's actual request already succeeded upstream).
func (s *Server) handleCreateRepo(w http.ResponseWriter, r *http.Request) {
	var req createRepoRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpErrorJSON(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	repoType := req.Type
	if repoType == "" {
		repoType = "model"
	}
	if err := s.Hub.CreateRepo(r.Context(), auth.CredentialFromRequest(r), req.Name, req.Organization, repoType); err != nil {
		writeUpstreamError(w, err)
		return
	}
	repoID := req.Name
	if req.Organization != "" {
		repoID = req.Organization + "/" + req.Name
	}
	if !s.NoCache {
		s.Embedded.IngestRepoInfo(repoType, repoID, "main")
	}
	writeJSON(w, map[string]string{"url": repoID})
}

// writeJSON is proxyhub's own copy of hubserver.writeJSON — duplicated
// (not imported) for the same reason as splitRepoPath (see its note in
// proxyhub.go).
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("proxyhub: encode response", "error", err)
	}
}

// handleRepoInfo implements GET /api/{repo_type}s/{repo_id}/revision/{revision}:
// serve from Embedded if repoType/repoID/revision is already known and
// fresh; otherwise fetch from upstream, ingest, and delegate. Unlike
// writeTreeJSON/writeResolveHeaders, the -no-cache path below writes
// hfclient.RepoInfo directly rather than a shape matching hubserver's
// own repoInfoResponse ({id, sha} vs {id, sha, private}): hfclient.
// RepoInfo never parses a "private" field off the real Hub's response
// in the first place, so there is no real value to round-trip here —
// inventing one would fabricate data this project explicitly avoids
// (see hubserver.resolve.go's anti-fabrication rationale). A caller
// relying on "private" from this endpoint only gets it via the cached
// path (served from Embedded, whose repoInfoResponse always includes
// it, currently always false — see hubserver.getOrCreateRepo).
func (s *Server) handleRepoInfo(w http.ResponseWriter, r *http.Request, repoType, repoID, revision string) {
	key := cacheKey(repoType, repoID, revision)

	if s.NoCache {
		info, err := s.Hub.RepoInfo(r.Context(), auth.CredentialFromRequest(r), repoType, repoID, revision)
		if err != nil {
			writeUpstreamError(w, err)
			return
		}
		writeJSON(w, info)
		return
	}

	if s.repoInfoFreshness.Fresh(key, s.CacheTTL) && s.Embedded.HasRevision(repoType, repoID, revision) {
		s.Embedded.ServeHTTP(w, r)
		return
	}

	callCtx, cancel := s.callContext(r)
	info, err := s.Hub.RepoInfo(callCtx, auth.CredentialFromRequest(r), repoType, repoID, revision)
	cancel()
	if err != nil {
		if s.Embedded.HasRevision(repoType, repoID, revision) {
			// Stale-fallback: upstream failed, but Embedded already has
			// this revision from a previous successful fetch — serve it
			// regardless of age, per the package doc comment's whole
			// reason to exist.
			s.Embedded.ServeHTTP(w, r)
			return
		}
		writeUpstreamError(w, err)
		return
	}

	s.Embedded.IngestRepoInfo(repoType, repoID, info.SHA)
	// info.SHA is also the revision name in this shim's model (see
	// internal/hubserver's repoInfoResponse doc comment) — but the
	// CALLER asked about `revision`, which might be a name upstream
	// resolved differently; ingest under both to be safe, then serve
	// from Embedded so the response shape is byte-identical to what
	// hubserver's own handleRepoInfo would produce for a real commit.
	if revision != info.SHA {
		s.Embedded.IngestRepoInfo(repoType, repoID, revision)
	}
	s.repoInfoFreshness.MarkFresh(key)
	s.Embedded.ServeHTTP(w, r)
}

// handleCreateBranch relays POST /api/{repo_type}s/{repo_id}/branch/{branch}
// to the real Hub unconditionally, then mirrors the effect into Embedded.
func (s *Server) handleCreateBranch(w http.ResponseWriter, r *http.Request, repoType, repoID, branch string) {
	if err := s.Hub.CreateBranch(r.Context(), auth.CredentialFromRequest(r), repoType, repoID, branch); err != nil {
		writeUpstreamError(w, err)
		return
	}
	if !s.NoCache {
		s.Embedded.IngestRepoInfo(repoType, repoID, branch)
	}
	w.WriteHeader(http.StatusOK)
}
