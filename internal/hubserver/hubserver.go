// Package hubserver implements just enough of huggingface.co's Hub REST
// API (distinct from the CAS API in internal/casserver) to let the real
// `hf upload` / `hf download` CLI commands (via huggingface_hub) work
// end-to-end against a local server, with HF_ENDPOINT pointed at it:
//
//   - POST /api/repos/create                                  — repo creation (idempotent)
//   - GET  /api/{repo_type}s/{repo_id}/xet-{read,write}-token/{revision}
//     — issues a CAS endpoint + bearer token via response headers
//   - POST /api/{repo_type}s/{repo_id}/commit/{revision}       — ndjson commit payload
//   - HEAD/GET /{repo_id}/resolve/{revision}/{filename}        — file metadata + content
//
// No git refs, branches, PRs, or real auth are modeled: every repo has a
// single implicit "main" revision, and any bearer token is accepted.
// State is held in memory alongside the paired casserver.Server, since a
// commit's file entries need to reference the Xet file hash the CAS layer
// already knows how to reconstruct.
package hubserver

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"xet-server/internal/merklehash"
)

// casInfo is the subset of casserver.Server's read-side API this package
// needs, kept as a small local interface (rather than importing
// casserver.Server directly) so hubserver stays a thin, independently
// testable layer over whatever CAS backend it's paired with.
type casInfo interface {
	XetHashForSHA256(sha256Hex string) (merklehash.Hash, bool)
	FileSize(fileHash merklehash.Hash) (int64, bool)
}

// Server implements the Hub API shim. CASBaseURL is the base URL of the
// paired casserver.Server instance (e.g. "http://localhost:8420"), handed
// out via the xet-token routes' X-Xet-Cas-Url header. CAS provides the
// bridge from a committed file's plain SHA-256 to its Xet/Merkle hash.
type Server struct {
	CASBaseURL string
	CAS        casInfo
	mux        *http.ServeMux

	// mu guards only the repos map itself (adding a new repoKey); each
	// repoState has its own mutex for its files, so a commit/resolve on
	// one repo never blocks on unrelated activity in another.
	mu    sync.RWMutex
	repos map[repoKey]*repoState
}

type repoKey struct {
	repoType string
	repoID   string
}

// fileRef is one committed file's identity: its path in the repo, plain
// SHA-256 (as declared in the commit's lfsFile.oid), and the Xet/Merkle
// file hash the CAS layer indexes it under (backfilled once known).
type fileRef struct {
	Path      string
	SHA256Hex string
	XetHash   merklehash.Hash
	Size      int64
}

type repoState struct {
	mu         sync.RWMutex
	files      map[string]*fileRef // keyed by path in repo
	commitOID  string
	commitSeen bool
}

func New(casBaseURL string, cas casInfo) *Server {
	s := &Server{
		CASBaseURL: casBaseURL,
		CAS:        cas,
		mux:        http.NewServeMux(),
		repos:      make(map[repoKey]*repoState),
	}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) routes() {
	s.mux.HandleFunc("POST /api/repos/create", s.handleCreateRepo)
	// repoID in real Hub URLs is "namespace/name" (contains a slash), so
	// these routes take the whole remainder as one wildcard and parse the
	// repo type / repo ID / trailing segment out of it manually.
	s.mux.HandleFunc("GET /api/{rest...}", s.handleAPIGet)
	s.mux.HandleFunc("POST /api/{rest...}", s.handleAPIPost)
	// A single handler (registered for both methods on the same pattern,
	// rather than separate HEAD/GET registrations) avoids Go's ServeMux
	// treating "HEAD /{rest...}" as ambiguous against "GET /api/{rest...}"
	// (HEAD implicitly falls back to a GET pattern's handler otherwise).
	s.mux.HandleFunc("/{rest...}", s.handleResolveDispatch)
}

// splitRepoPath splits a path of the form
// "{repoType}s/{namespace}/{name}/{tail...}" into repoType, "{namespace}/{name}",
// and the remaining tail segments. repoType is singularized ("models" ->
// "model") to match huggingface_hub's repo_type values.
func splitRepoPath(rest string) (repoType, repoID string, tail []string, ok bool) {
	parts := strings.Split(rest, "/")
	if len(parts) < 3 {
		return "", "", nil, false
	}
	repoTypePlural := parts[0]
	repoType = strings.TrimSuffix(repoTypePlural, "s")
	repoID = parts[1] + "/" + parts[2]
	return repoType, repoID, parts[3:], true
}

// handleAPIGet dispatches GET /api/... requests: xet-{read,write}-token
// routes, keyed by repo type/ID and matched by the tail segments'
// "xet-{read,write}-token" prefix followed by a revision.
func (s *Server) handleAPIGet(w http.ResponseWriter, r *http.Request) {
	repoType, repoID, tail, ok := splitRepoPath(r.PathValue("rest"))
	if !ok || len(tail) < 2 {
		http.NotFound(w, r)
		return
	}
	switch tail[0] {
	case "xet-read-token":
		s.handleXetToken(w, r, repoType, repoID, readToken)
	case "xet-write-token":
		s.handleXetToken(w, r, repoType, repoID, writeToken)
	default:
		http.NotFound(w, r)
	}
}

// handleAPIPost dispatches POST /api/... requests: repo creation, preupload
// mode negotiation, and commit — keyed the same way as handleAPIGet.
func (s *Server) handleAPIPost(w http.ResponseWriter, r *http.Request) {
	rest := r.PathValue("rest")
	if rest == "repos/create" {
		s.handleCreateRepo(w, r)
		return
	}
	repoType, repoID, tail, ok := splitRepoPath(rest)
	if !ok || len(tail) < 2 {
		http.NotFound(w, r)
		return
	}
	revision := tail[1]
	switch tail[0] {
	case "commit":
		s.handleCommit(w, r, repoType, repoID, revision)
	case "preupload":
		s.handlePreupload(w, r)
	default:
		http.NotFound(w, r)
	}
}

// handleResolveDispatch dispatches GET/HEAD /{repoID}/resolve/{revision}/{filename}
// requests. Unlike the /api/ routes, resolve URLs have no repoType prefix
// segment: the path is "{namespace}/{name}/resolve/{revision}/{filename...}".
func (s *Server) handleResolveDispatch(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(r.PathValue("rest"), "/", 5)
	if len(parts) < 5 || parts[2] != "resolve" {
		http.NotFound(w, r)
		return
	}
	repoID := parts[0] + "/" + parts[1]
	revision := parts[3]
	filename := parts[4]
	s.handleResolve(w, r, repoID, revision, filename)
}

func (s *Server) getOrCreateRepo(repoType, repoID string) *repoState {
	key := repoKey{repoType: repoType, repoID: repoID}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, ok := s.repos[key]
	if !ok {
		rs = &repoState{files: make(map[string]*fileRef)}
		s.repos[key] = rs
	}
	return rs
}

// httpErrorJSON writes a JSON error response and logs it at a level
// matching its cause — see casserver.httpError's comment for why 5xx and
// 4xx are split between Warn and Debug.
func httpErrorJSON(w http.ResponseWriter, msg string, code int) {
	if code >= 500 {
		slog.Warn("hubserver: request failed", "status", code, "error", msg)
	} else {
		slog.Debug("hubserver: request rejected", "status", code, "error", msg)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

type xetTokenType int

const (
	readToken xetTokenType = iota
	writeToken
)
