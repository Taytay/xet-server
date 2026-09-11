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
// Each repo supports real, independent revisions (a "main" branch is
// created implicitly on first touch, matching how a real repo always has
// a default branch; any other revision name is created on first commit to
// it) — commit/resolve/preupload all operate against the named revision's
// own file set and commit history, not a single shared implicit state.
// No git refs/PRs are modeled. Every route except telemetry-equivalents
// (there are none in this shim) requires the scope real xet-core/Hub
// convention implies (write for token/commit/preupload/repo-create,
// read for resolve), enforced via auth.Authenticator — see
// SetAuthenticator. The default (auth.NoAuth{}) enforces nothing, this
// server's behavior prior to v0.8.0: any bearer token (or none) is
// accepted. State is held in memory alongside the paired
// casserver.Server, since a commit's file entries need to reference the
// Xet file hash the CAS layer already knows how to reconstruct — and
// periodically checkpointed to disk (see Server.Snapshot/LoadSnapshot in
// snapshot.go) so it survives a restart.
package hubserver

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"xet-server/internal/auth"
	"xet-server/internal/merklehash"
	"xet-server/internal/routing"
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
	// repoState has its own mutex for its revisions map, and each
	// revisionState has its own mutex for its files — a commit/resolve on
	// one repo, or one revision within a repo, never blocks on unrelated
	// activity elsewhere.
	mu    sync.RWMutex
	repos map[repoKey]*repoState

	// authenticator gates every route per its required scope (see
	// requireScope and each dispatch handler below). Defaults to
	// auth.NoAuth{} — this server's pre-v0.8.0 behavior, unconditionally
	// allowing every request — until SetAuthenticator is called with
	// something else.
	authenticator auth.Authenticator
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

// defaultRevision is created implicitly for every repo on first touch,
// matching how a real repo always has a default branch even before any
// commit — the same way real HF Hub's repo creation immediately gives you
// a "main" you can push to.
const defaultRevision = "main"

// repoState holds one repo's independent revisions. mu guards only the
// revisions map itself; each revisionState guards its own files/commit
// metadata.
type repoState struct {
	mu        sync.RWMutex
	revisions map[string]*revisionState
}

// revisionState is one revision's (branch's) committed file set and
// commit history within a repo — what used to be repoState's fields
// directly, before repos gained more than one revision.
type revisionState struct {
	mu         sync.RWMutex
	files      map[string]*fileRef // keyed by path in repo
	commitOID  string
	commitSeen bool
}

func New(casBaseURL string, cas casInfo) *Server {
	s := &Server{
		CASBaseURL:    casBaseURL,
		CAS:           cas,
		mux:           http.NewServeMux(),
		repos:         make(map[repoKey]*repoState),
		authenticator: auth.NoAuth{},
	}
	s.routes()
	return s
}

// SetAuthenticator replaces this server's Authenticator (default
// auth.NoAuth{}, i.e. no enforcement — this server's pre-v0.8.0
// behavior). Safe to call at any time — unlike casserver's
// SetAuthenticator, hubserver's routes dispatch by parsing the path
// inside each handler rather than registering one mux pattern per
// logical endpoint, so there's no route table to rebuild.
func (s *Server) SetAuthenticator(a auth.Authenticator) {
	s.authenticator = a
}

// requireScope authenticates r against s.authenticator and checks for
// scope, writing a 401 (no/invalid credential) or 403 (valid credential,
// insufficient scope) and returning false if the request should not
// proceed. Mirrors casserver.Server.requireScope's semantics exactly
// (see its doc comment) — kept as a plain bool-returning helper instead
// of a http.HandlerFunc-wrapping middleware here, since hubserver's
// dispatch handlers (handleAPIGet/handleAPIPost/handleResolveDispatch)
// need to parse the path before they know which scope applies, unlike
// casserver's one-mux-pattern-per-endpoint routing.
func (s *Server) requireScope(w http.ResponseWriter, r *http.Request, scope auth.Scope) bool {
	principal, err := s.authenticator.Authenticate(r)
	if err != nil {
		if errors.Is(err, auth.ErrUnauthenticated) {
			httpErrorJSON(w, "unauthenticated: "+err.Error(), http.StatusUnauthorized)
		} else {
			httpErrorJSON(w, "authentication failed: "+err.Error(), http.StatusForbidden)
		}
		return false
	}
	if !principal.HasScope(scope) {
		httpErrorJSON(w, "principal lacks required scope: "+string(scope), http.StatusForbidden)
		return false
	}
	return true
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) routes() {
	// repos/create is dispatched through handleAPIPost (which checks
	// rest == "repos/create"), NOT registered as its own literal mux
	// pattern here — Go's ServeMux prefers a more specific literal pattern
	// over a wildcard one, so a direct "POST /api/repos/create" route
	// would take priority over "POST /api/{rest...}" below and bypass
	// handleAPIPost's requireScope check on this endpoint entirely.
	//
	// repoID in real Hub URLs is "namespace/name" (contains a slash), so
	// these routes take the whole remainder as one wildcard and parse the
	// repo type / repo ID / trailing segment out of it manually.
	//
	// No routing.MountWithVersion here — huggingface_hub's real REST API
	// is unversioned (no "/v1" segment exists in the actual protocol this
	// mirrors), so routing.Mount (no version prefix) is used instead,
	// still getting the same declarative-route-table shape as
	// casserver/internal/api.
	routing.Apply(s.mux, []routing.Route{
		routing.Mount("GET", "/api/{rest...}", http.HandlerFunc(s.handleAPIGet)),
		routing.Mount("POST", "/api/{rest...}", http.HandlerFunc(s.handleAPIPost)),
		// A single handler (registered for both methods on the same
		// pattern, rather than separate HEAD/GET registrations) avoids
		// Go's ServeMux treating "HEAD /{rest...}" as ambiguous against
		// "GET /api/{rest...}" (HEAD implicitly falls back to a GET
		// pattern's handler otherwise).
		routing.Mount("", "/{rest...}", http.HandlerFunc(s.handleResolveDispatch)),
	})
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

// handleAPIGet dispatches GET /api/... requests: xet-{read,write}-token,
// revision (repo info), and tree (file listing) routes, keyed by repo
// type/ID and matched by the tail segments' first element.
func (s *Server) handleAPIGet(w http.ResponseWriter, r *http.Request) {
	repoType, repoID, tail, ok := splitRepoPath(r.PathValue("rest"))
	if !ok || len(tail) < 2 {
		http.NotFound(w, r)
		return
	}
	switch tail[0] {
	case "xet-read-token":
		if !s.requireScope(w, r, auth.ScopeRead) {
			return
		}
		s.handleXetToken(w, r, repoType, repoID)
	case "xet-write-token":
		if !s.requireScope(w, r, auth.ScopeWrite) {
			return
		}
		s.handleXetToken(w, r, repoType, repoID)
	case "revision":
		if !s.requireScope(w, r, auth.ScopeRead) {
			return
		}
		s.handleRepoInfo(w, r, repoType, repoID, tail[1])
	case "tree":
		if !s.requireScope(w, r, auth.ScopeRead) {
			return
		}
		// tail[2:] is the optional path-in-repo, itself a single
		// (percent-decoded-by-net/http) path segment — see
		// list_repo_tree's encoded_path_in_repo, which quotes it with
		// safe="" so a "/" inside it is percent-encoded rather than
		// splitting into more segments.
		var pathInRepo string
		if len(tail) > 2 {
			pathInRepo = strings.Join(tail[2:], "/")
		}
		s.handleListTree(w, r, repoType, repoID, tail[1], pathInRepo)
	default:
		http.NotFound(w, r)
	}
}

// handleAPIPost dispatches POST /api/... requests: repo creation, preupload
// mode negotiation, and commit — keyed the same way as handleAPIGet.
func (s *Server) handleAPIPost(w http.ResponseWriter, r *http.Request) {
	rest := r.PathValue("rest")
	if rest == "repos/create" {
		if !s.requireScope(w, r, auth.ScopeWrite) {
			return
		}
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
		if !s.requireScope(w, r, auth.ScopeWrite) {
			return
		}
		s.handleCommit(w, r, repoType, repoID, revision)
	case "preupload":
		if !s.requireScope(w, r, auth.ScopeWrite) {
			return
		}
		s.handlePreupload(w, r)
	case "branch":
		if !s.requireScope(w, r, auth.ScopeWrite) {
			return
		}
		// tail[1] here is the branch name being created, not a revision to
		// operate against — handleCreateBranch's own signature names it
		// accordingly.
		s.handleCreateBranch(w, r, repoType, repoID, revision)
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
	if !s.requireScope(w, r, auth.ScopeRead) {
		return
	}
	repoID := parts[0] + "/" + parts[1]
	revision := parts[3]
	filename := parts[4]
	s.handleResolve(w, r, repoID, revision, filename)
}

// getOrCreateRepo returns the repoState for repoType/repoID, creating it
// (with an implicit "main" revision already present) on first touch.
func (s *Server) getOrCreateRepo(repoType, repoID string) *repoState {
	key := repoKey{repoType: repoType, repoID: repoID}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, ok := s.repos[key]
	if !ok {
		rs = &repoState{revisions: map[string]*revisionState{
			defaultRevision: newRevisionState(),
		}}
		s.repos[key] = rs
	}
	return rs
}

func newRevisionState() *revisionState {
	return &revisionState{files: make(map[string]*fileRef)}
}

// getOrCreateRevision returns rs's revisionState for the named revision,
// creating an empty one on first touch — matching how pushing to a new
// branch name on a real repo implicitly creates that branch. An empty
// revision string is treated as defaultRevision, since some call sites
// (e.g. a resolve URL with no revision segment) may not always supply one
// explicitly.
func (rs *repoState) getOrCreateRevision(revision string) *revisionState {
	if revision == "" {
		revision = defaultRevision
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	v, ok := rs.revisions[revision]
	if !ok {
		v = newRevisionState()
		rs.revisions[revision] = v
	}
	return v
}

// getRevision returns rs's revisionState for the named revision without
// creating it, or false if that revision doesn't exist yet — used by
// read paths (resolve) where a nonexistent revision should 404, not
// silently spring into existence.
func (rs *repoState) getRevision(revision string) (*revisionState, bool) {
	if revision == "" {
		revision = defaultRevision
	}
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	v, ok := rs.revisions[revision]
	return v, ok
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
