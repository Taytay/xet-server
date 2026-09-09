package hubserver

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

type createRepoRequest struct {
	Name         string `json:"name"`
	Organization string `json:"organization"`
	Type         string `json:"type"`
}

type createRepoResponse struct {
	URL string `json:"url"`
}

// handleCreateRepo implements POST /api/repos/create: idempotently
// registers a repo (this server has no notion of "already exists" beyond
// creating the in-memory repoState on first touch, so repeat calls with
// the same name are harmless no-ops).
func (s *Server) handleCreateRepo(w http.ResponseWriter, r *http.Request) {
	var req createRepoRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpErrorJSON(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		httpErrorJSON(w, "missing repo name", http.StatusBadRequest)
		return
	}

	repoType := req.Type
	if repoType == "" {
		repoType = "model"
	}
	repoID := req.Name
	if req.Organization != "" {
		repoID = req.Organization + "/" + req.Name
	}

	s.getOrCreateRepo(repoType, repoID)

	writeJSON(w, createRepoResponse{URL: repoID})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// repoInfoResponse is GET /api/{repo_type}s/{repo_id}/revision/{revision}'s
// body — a small subset of the real Hub's ModelInfo/DatasetInfo/SpaceInfo
// JSON shape. huggingface_hub's snapshot_download only ever reads `sha`
// off the deserialized object (asserting it's non-nil before using it as
// the resolved commit hash for every subsequent per-file download), so
// that's the one field that must be present and meaningful; the rest are
// included because the client's dataclass constructors accept (and
// harmlessly ignore) extra fields, and because a real client inspecting
// the response for debugging should see something recognizable.
//
// sha is set to the revision name itself (e.g. "main"), not a real git
// commit hash — this shim has no separate git-commit-hash identity for a
// revision distinct from its name, and nothing in huggingface_hub
// validates sha's format; every downstream call (list_repo_tree, resolve)
// already accepts a revision name directly, so round-tripping the name
// back as "sha" keeps the whole flow internally consistent.
type repoInfoResponse struct {
	ID      string `json:"id"`
	SHA     string `json:"sha"`
	Private bool   `json:"private"`
}

// handleRepoInfo implements GET /api/{repo_type}s/{repo_id}/revision/{revision}:
// the request huggingface_hub's snapshot_download (used by `hf download`
// for a whole-repo download, as opposed to a single named file) issues
// first, to resolve revision to a commit hash before listing/downloading
// files — and the request `hf upload`'s CLI command issues first, to
// check whether the target branch already exists before creating it.
// Matches resolve.go's existing read-path convention: the repo itself is
// implicitly created on first touch (matching how a real Hub repo always
// exists once anything references it), but a nonexistent revision 404s
// with X-Error-Code: RevisionNotFound rather than being silently created
// — a repo-info lookup is a read, not a push, so it must not have the
// side effect of creating a branch nobody has committed to yet. The
// X-Error-Code header is required, not cosmetic: huggingface_hub's
// hf_raise_for_status only raises the specific RevisionNotFoundError
// (which callers like `hf upload`'s branch-creation step specifically
// catch) when this header is present; a plain 404 falls through to a
// generic, uncaught HfHubHTTPError instead.
func (s *Server) handleRepoInfo(w http.ResponseWriter, r *http.Request, repoType, repoID, revision string) {
	rs := s.getOrCreateRepo(repoType, repoID)
	if _, ok := rs.getRevision(revision); !ok {
		w.Header().Set("X-Error-Code", "RevisionNotFound")
		http.NotFound(w, r)
		return
	}
	writeJSON(w, repoInfoResponse{ID: repoID, SHA: revision})
}

// handleCreateBranch implements POST /api/{repo_type}s/{repo_id}/branch/{branch}:
// creates (or, matching real Hub's create_branch(exist_ok=True) contract,
// no-ops on) the named revision. Called by `hf upload`'s CLI command
// after a handleRepoInfo lookup reports the branch doesn't exist yet.
// This shim's revisions already spring into existence implicitly on
// first commit (see getOrCreateRevision) — this handler just does the
// same thing eagerly, in response to an explicit request instead of
// waiting for the first commit, so a caller that checks "does this
// branch exist" immediately afterward (e.g. via handleRepoInfo) sees it.
func (s *Server) handleCreateBranch(w http.ResponseWriter, r *http.Request, repoType, repoID, branch string) {
	rs := s.getOrCreateRepo(repoType, repoID)
	rs.getOrCreateRevision(branch)
	w.WriteHeader(http.StatusOK)
}

// xetTokenResponse is the JSON body real HF Hub returns from
// xet-{read,write}-token, per xet-core's CasJWTInfo (xet_client/src/hub_client/types.rs):
// hf_xet's Rust client decodes this response as JSON (DirectRefreshRouteTokenRefresher::get_cas_jwt),
// not from headers — a header-only response with an empty body fails
// hf_xet's resp.json() decode, which it treats as a transient error and
// retries indefinitely instead of failing fast.
type xetTokenResponse struct {
	CasURL      string `json:"casUrl"`
	Exp         int64  `json:"exp"`
	AccessToken string `json:"accessToken"`
}

// handleXetToken implements GET /api/{repo_type}s/{repo_id}/xet-{read,write}-token/{revision}:
// issues a CAS endpoint + bearer token, both as a JSON body (what hf_xet's
// Rust client actually parses) and via the X-Xet-* response headers
// (huggingface_hub's parse_xet_connection_info_from_headers, used on the
// resolve/download path). Auth is not modeled — any request succeeds and
// gets a fresh random token with a generous expiry.
func (s *Server) handleXetToken(w http.ResponseWriter, r *http.Request, repoType, repoID string, kind xetTokenType) {
	_ = kind // read vs write both get the same unrestricted token; no scope enforcement here
	s.getOrCreateRepo(repoType, repoID)

	token, err := randomToken()
	if err != nil {
		httpErrorJSON(w, "generate token: "+err.Error(), http.StatusInternalServerError)
		return
	}
	expiry := time.Now().Add(1 * time.Hour).Unix()

	w.Header().Set("X-Xet-Cas-Url", s.CASBaseURL)
	w.Header().Set("X-Xet-Access-Token", token)
	w.Header().Set("X-Xet-Token-Expiration", strconv.FormatInt(expiry, 10))
	writeJSON(w, xetTokenResponse{
		CasURL:      s.CASBaseURL,
		Exp:         expiry,
		AccessToken: token,
	})
}

func randomToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
