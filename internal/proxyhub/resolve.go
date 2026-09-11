package proxyhub

// resolve.go implements the cached read path for HEAD (and, minimally,
// GET) /{repo_id}/resolve/{revision}/{filename}, serving from Embedded
// (a real *hubserver.Server) once the file is known and fresh —
// matching internal/hubserver's own handleResolve byte-for-byte,
// including its X-Xet-Refresh-Route header: Embedded's own
// refreshRouteURL already builds that from the incoming request's own
// Host, which is already this proxy's own address, not the real Hub's —
// so no rewrite step is needed here at all.

import (
	"net/http"
	"strconv"

	"xet-server/internal/auth"
	"xet-server/internal/hfclient"
)

func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request, repoID, revision, filename string) {
	// repoType is not encoded in a resolve URL — hubserver's own
	// handleResolve assumes "model" for the exact same reason (see its
	// doc comment); IngestFile below must agree, or a resolve served
	// from Embedded after a repo-info/tree fetch under some OTHER
	// repoType would look up the wrong revisionState.
	const repoType = "model"
	key := cacheKey(repoID, revision, filename)
	cred := auth.CredentialFromRequest(r)

	if s.NoCache {
		info, err := s.Hub.Resolve(r.Context(), cred, repoID, revision, filename)
		if err != nil {
			writeUpstreamError(w, err)
			return
		}
		writeResolveHeaders(w, r, repoID, revision, info)
		return
	}

	if s.resolveFreshness.Fresh(key, s.CacheTTL) && s.Embedded.HasFile(repoType, repoID, revision, filename) {
		s.Embedded.ServeHTTP(w, r)
		return
	}

	callCtx, cancel := s.callContext(r)
	info, err := s.Hub.Resolve(callCtx, cred, repoID, revision, filename)
	cancel()
	if err != nil {
		if s.Embedded.HasFile(repoType, repoID, revision, filename) {
			s.Embedded.ServeHTTP(w, r)
			return
		}
		writeUpstreamError(w, err)
		return
	}

	xetHash := parseXetHashHex(info.XetHash)
	s.recordFileSize(xetHash, info.LinkedSize)
	s.Embedded.IngestFile(repoType, repoID, revision, filename, trimQuotes(info.ETag), info.LinkedSize, xetHash)
	if info.RepoCommit != "" {
		s.Embedded.IngestCommit(repoType, repoID, revision, info.RepoCommit)
	}
	s.resolveFreshness.MarkFresh(key)
	s.Embedded.ServeHTTP(w, r)
}

// writeResolveHeaders is the -no-cache path's own header writer,
// matching hubserver's exact header set without needing Embedded at
// all — mirrors internal/hubserver/resolve.go's handleResolve.
func writeResolveHeaders(w http.ResponseWriter, r *http.Request, repoID, revision string, info *hfclient.ResolveInfo) {
	refreshRoute := refreshRouteURL(r, repoID, revision)
	w.Header().Set("X-Repo-Commit", info.RepoCommit)
	w.Header().Set("ETag", info.ETag)
	w.Header().Set("X-Linked-Etag", info.ETag)
	w.Header().Set("X-Linked-Size", strconv.FormatInt(info.LinkedSize, 10))
	w.Header().Set("Content-Length", strconv.FormatInt(info.LinkedSize, 10))
	w.Header().Set("X-Xet-Hash", info.XetHash)
	w.Header().Set("X-Xet-Refresh-Route", refreshRoute)

	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Error(w, "direct GET not supported; use the Xet download path via X-Xet-Hash", http.StatusNotImplemented)
}

// refreshRouteURL is proxyhub's own copy of hubserver.refreshRouteURL —
// duplicated (not imported) since it's an unexported helper of a
// package this one only otherwise touches through its exported
// Ingest*/Has*/ServeHTTP surface (see splitRepoPath's identical note in
// proxyhub.go). Encodes the X-Xet-Refresh-Route wire contract, so any
// change here must be mirrored in hubserver's copy.
func refreshRouteURL(r *http.Request, repoID, revision string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/api/models/" + repoID + "/xet-read-token/" + revision
}
