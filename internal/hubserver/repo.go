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
