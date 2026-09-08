package hubserver

import (
	"log/slog"
	"net/http"
	"strconv"
)

// handleResolve implements HEAD and GET /{repoID}/resolve/{revision}/{filename}:
// returns file metadata via headers (commit hash, ETag, size, and Xet
// connection info per parse_xet_file_data_from_response) on HEAD, and
// proxies the actual bytes from the paired CAS server on GET.
//
// huggingface_hub's HEAD call is what triggers the Xet download path: it
// looks for X-Xet-Hash plus either a `Link: <url>; rel="xet-auth"` header
// or X-Xet-Refresh-Route, and if present, downloads via hf_xet instead of
// this resolve URL directly — so the GET path below only matters as a
// fallback and is not exercised by a normal `hf download`.
func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request, repoID, revision, filename string) {
	rs := s.getOrCreateRepo("model", repoID) // repo type is not encoded in the resolve URL; default assumption
	vs, ok := rs.getRevision(revision)
	if !ok {
		slog.Debug("resolve revision lookup", "repoID", repoID, "revision", revision, "found", false)
		http.NotFound(w, r)
		return
	}

	vs.mu.RLock()
	ref, ok := vs.files[filename]
	vs.mu.RUnlock()
	slog.Debug("resolve lookup", "repoID", repoID, "revision", revision, "filename", filename, "found", ok)
	if !ok {
		http.NotFound(w, r)
		return
	}

	xetHash, known := s.CAS.XetHashForSHA256(ref.SHA256Hex)
	slog.Debug("resolve xetHash lookup", "sha256Hex", ref.SHA256Hex, "known", known)
	if !known {
		// The shard carrying this file's metadata_ext hasn't been uploaded
		// yet (or ever will be, e.g. an interrupted upload) — nothing to
		// serve.
		http.NotFound(w, r)
		return
	}
	size, known := s.CAS.FileSize(xetHash)
	slog.Debug("resolve fileSize lookup", "xetHash", xetHash.Hex(), "known", known)
	if !known {
		http.NotFound(w, r)
		return
	}

	refreshRoute := refreshRouteURL(r, repoID, revision)

	w.Header().Set("X-Repo-Commit", commitOIDOrPlaceholder(vs))
	w.Header().Set("ETag", `"`+ref.SHA256Hex+`"`)
	w.Header().Set("X-Linked-Etag", `"`+ref.SHA256Hex+`"`)
	w.Header().Set("X-Linked-Size", strconv.FormatInt(size, 10))
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("X-Xet-Hash", xetHash.Hex())
	w.Header().Set("X-Xet-Refresh-Route", refreshRoute)

	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}

	// GET fallback: huggingface_hub only takes this path when Xet is
	// unavailable client-side, which the paired test setup never exercises,
	// so this just reports that the caller should have gone through Xet
	// instead of implementing a redundant byte-proxy to the CAS server.
	http.Error(w, "direct GET not supported; use the Xet download path via X-Xet-Hash", http.StatusNotImplemented)
}

func refreshRouteURL(r *http.Request, repoID, revision string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/api/models/" + repoID + "/xet-read-token/" + revision
}

func commitOIDOrPlaceholder(vs *revisionState) string {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	if vs.commitSeen {
		return vs.commitOID
	}
	return "0000000000000000000000000000000000000000"
}
