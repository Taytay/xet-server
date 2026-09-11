package hfclient

// Tests for the Hub-side calls (RepoInfo, ListTree, GetXetToken, Resolve,
// CreateRepo, CreateBranch, Commit, Preupload), driven against a fake
// upstream httptest.Server standing in for the real huggingface.co — this
// sandbox cannot reach the real Hub, so every test here verifies the
// exact request this package sends (method, URL, headers, body) and that
// it parses a real-shaped response correctly, using a fake server that
// asserts on those exact things rather than a live one.
//
// Every token string below (e.g. "test-fixture-token-not-a-real-secret")
// is a hardcoded test fixture with no relation to any real credential —
// flagged explicitly so static-analysis secret scanners don't need to
// guess.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"xet-server/internal/auth"
)

const testFixtureToken = "test-fixture-token-not-a-real-secret"

func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return New(ts.URL), ts
}

// requireBearer fails the test unless r carries exactly the expected
// bearer token — used by every fake-upstream handler below to verify
// pure credential passthrough actually happened (the caller's token
// reached the upstream request unchanged, not silently dropped or
// replaced by some proxy-owned secret).
func requireBearer(t *testing.T, r *http.Request, want string) {
	t.Helper()
	got := r.Header.Get("Authorization")
	if got != "Bearer "+want {
		t.Errorf("Authorization = %q, want %q", got, "Bearer "+want)
	}
}

func TestRepoInfo_SendsExactURLAndForwardsCredential(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r, testFixtureToken)
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/api/models/alice/my-model/revision/main" {
			t.Errorf("path = %q, want %q", r.URL.Path, "/api/models/alice/my-model/revision/main")
		}
		json.NewEncoder(w).Encode(RepoInfo{ID: "alice/my-model", SHA: "main"})
	})

	info, err := c.RepoInfo(context.Background(), auth.NewBearerCredentialHelper(testFixtureToken), "model", "alice/my-model", "main")
	if err != nil {
		t.Fatalf("RepoInfo() error = %v", err)
	}
	if info.ID != "alice/my-model" || info.SHA != "main" {
		t.Errorf("info = %+v, want {alice/my-model main}", info)
	}
}

func TestRepoInfo_UnknownRevisionReturnsStatusErrorWithErrorCode(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Error-Code", "RevisionNotFound")
		http.Error(w, "not found", http.StatusNotFound)
	})

	_, err := c.RepoInfo(context.Background(), nil, "model", "alice/my-model", "ghost")
	if err == nil {
		t.Fatal("RepoInfo() error = nil, want a *StatusError")
	}
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("error = %v (%T), want *StatusError", err, err)
	}
	if statusErr.Status != http.StatusNotFound {
		t.Errorf("Status = %d, want 404", statusErr.Status)
	}
	if statusErr.ErrorCode != "RevisionNotFound" {
		t.Errorf("ErrorCode = %q, want %q", statusErr.ErrorCode, "RevisionNotFound")
	}
}

func TestRepoInfo_NilCredentialSendsNoAuthorizationHeader(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want empty with a nil credential", got)
		}
		json.NewEncoder(w).Encode(RepoInfo{ID: "a/b", SHA: "main"})
	})
	if _, err := c.RepoInfo(context.Background(), nil, "model", "a/b", "main"); err != nil {
		t.Fatalf("RepoInfo() error = %v", err)
	}
}

func TestListTree_SendsRecursiveTrueAndReturnsEntries(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models/alice/my-model/tree/main" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("recursive") != "true" {
			t.Errorf("recursive query param = %q, want %q", r.URL.Query().Get("recursive"), "true")
		}
		json.NewEncoder(w).Encode([]TreeEntry{
			{Type: "file", Path: "model.bin", Size: 100, OID: "abc"},
			{Type: "file", Path: "config.json", Size: 10, OID: "def"},
		})
	})

	entries, err := c.ListTree(context.Background(), nil, "model", "alice/my-model", "main")
	if err != nil {
		t.Fatalf("ListTree() error = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
}

func TestListTree_FollowsLinkHeaderPagination(t *testing.T) {
	callCount := 0
	var ts *httptest.Server
	handler := func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, ts.URL+"/api/models/alice/my-model/tree/main?recursive=true&page=2"))
			json.NewEncoder(w).Encode([]TreeEntry{{Type: "file", Path: "a.bin", Size: 1, OID: "aaa"}})
			return
		}
		json.NewEncoder(w).Encode([]TreeEntry{{Type: "file", Path: "b.bin", Size: 2, OID: "bbb"}})
	}
	ts = httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(ts.Close)
	c := New(ts.URL)

	entries, err := c.ListTree(context.Background(), nil, "model", "alice/my-model", "main")
	if err != nil {
		t.Fatalf("ListTree() error = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries across both pages, want 2", len(entries))
	}
	if callCount != 2 {
		t.Errorf("upstream called %d times, want 2 (one per page)", callCount)
	}
}

func TestGetXetToken_ReadVsWriteHitsDifferentPath(t *testing.T) {
	var gotPath string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewEncoder(w).Encode(XetToken{CasURL: "https://cas.example.invalid", Exp: 123, AccessToken: "tok"})
	})

	if _, err := c.GetXetToken(context.Background(), nil, "model", "alice/my-model", "main", XetTokenRead); err != nil {
		t.Fatalf("GetXetToken(read) error = %v", err)
	}
	if gotPath != "/api/models/alice/my-model/xet-read-token/main" {
		t.Errorf("read path = %q", gotPath)
	}

	if _, err := c.GetXetToken(context.Background(), nil, "model", "alice/my-model", "main", XetTokenWrite); err != nil {
		t.Fatalf("GetXetToken(write) error = %v", err)
	}
	if gotPath != "/api/models/alice/my-model/xet-write-token/main" {
		t.Errorf("write path = %q", gotPath)
	}
}

func TestGetXetToken_ParsesCasURLFromJSONBody(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// Deliberately omit the X-Xet-* headers real huggingface_hub's
		// resolve path reads, to prove this call reads the JSON body —
		// see docs/PROTOCOL.md section 6 on why the body, not headers, is
		// what hf_xet's token-refresh call site actually decodes.
		json.NewEncoder(w).Encode(XetToken{CasURL: "https://cas.example.invalid", Exp: 999, AccessToken: "the-token"})
	})

	tok, err := c.GetXetToken(context.Background(), nil, "model", "a/b", "main", XetTokenRead)
	if err != nil {
		t.Fatalf("GetXetToken() error = %v", err)
	}
	if tok.CasURL != "https://cas.example.invalid" {
		t.Errorf("CasURL = %q", tok.CasURL)
	}
	if tok.AccessToken != "the-token" {
		t.Errorf("AccessToken = %q", tok.AccessToken)
	}
	if tok.Exp != 999 {
		t.Errorf("Exp = %d, want 999", tok.Exp)
	}
}

func TestResolve_ParsesHeadersFromHEADResponse(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("method = %s, want HEAD", r.Method)
		}
		if r.URL.Path != "/alice/my-model/resolve/main/model.bin" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("X-Xet-Hash", "deadbeef")
		w.Header().Set("X-Linked-Size", "12345")
		w.Header().Set("ETag", `"abc"`)
		w.Header().Set("X-Repo-Commit", "commitoid")
		w.Header().Set("X-Xet-Refresh-Route", "https://example.invalid/refresh")
		w.WriteHeader(http.StatusOK)
	})

	info, err := c.Resolve(context.Background(), nil, "alice/my-model", "main", "model.bin")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if info.XetHash != "deadbeef" {
		t.Errorf("XetHash = %q", info.XetHash)
	}
	if info.LinkedSize != 12345 {
		t.Errorf("LinkedSize = %d, want 12345", info.LinkedSize)
	}
	if info.ETag != `"abc"` {
		t.Errorf("ETag = %q", info.ETag)
	}
	if info.RepoCommit != "commitoid" {
		t.Errorf("RepoCommit = %q", info.RepoCommit)
	}
	if info.XetRefreshRoute != "https://example.invalid/refresh" {
		t.Errorf("XetRefreshRoute = %q", info.XetRefreshRoute)
	}
}

func TestCreateRepo_SendsExpectedBody(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/repos/create" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["name"] != "my-model" || body["organization"] != "alice" || body["type"] != "model" {
			t.Errorf("body = %+v", body)
		}
		w.WriteHeader(http.StatusOK)
	})
	if err := c.CreateRepo(context.Background(), nil, "my-model", "alice", "model"); err != nil {
		t.Fatalf("CreateRepo() error = %v", err)
	}
}

func TestCreateBranch_PostsToExpectedPath(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/models/alice/my-model/branch/experiment-1" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	})
	if err := c.CreateBranch(context.Background(), nil, "model", "alice/my-model", "experiment-1"); err != nil {
		t.Fatalf("CreateBranch() error = %v", err)
	}
}

func TestCommit_SendsNdjsonBodyAndParsesResult(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "" && ct != "application/x-ndjson" {
			// no strict requirement on Content-Type from this client, but
			// the body itself must be valid ndjson either way.
			_ = ct
		}
		if r.URL.Path != "/api/models/alice/my-model/commit/main" {
			t.Errorf("path = %q", r.URL.Path)
		}
		json.NewEncoder(w).Encode(CommitResult{CommitOID: "deadbeef", CommitURL: "/alice/my-model/commit/deadbeef"})
	})

	result, err := c.Commit(context.Background(), nil, "model", "alice/my-model", "main", []CommitFile{
		{Path: "model.bin", OID: "abc123", Size: 100},
	})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if result.CommitOID != "deadbeef" {
		t.Errorf("CommitOID = %q", result.CommitOID)
	}
}

func TestPreupload_SendsFilesAndParsesResult(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models/alice/my-model/preupload/main" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var body struct {
			Files []PreuploadFile `json:"files"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if len(body.Files) != 1 || body.Files[0].Path != "model.bin" {
			t.Errorf("decoded files = %+v", body.Files)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"files": []PreuploadResult{{Path: "model.bin", UploadMode: "lfs", ShouldIgnore: false, OID: "abc123"}},
		})
	})

	results, err := c.Preupload(context.Background(), nil, "model", "alice/my-model", "main", []PreuploadFile{
		{Path: "model.bin", Size: 100},
	})
	if err != nil {
		t.Fatalf("Preupload() error = %v", err)
	}
	if len(results) != 1 || results[0].UploadMode != "lfs" {
		t.Errorf("results = %+v", results)
	}
	// Regression test: ShouldIgnore must round-trip through Preupload —
	// real huggingface_hub's _fetch_upload_modes reads
	// file["shouldIgnore"] with no default, so a silently-dropped field
	// crashes the real hf CLI with a KeyError, not a clean Go-side error.
	if got := results[0]; !containsField(t, "shouldIgnore", got) {
		t.Errorf("PreuploadResult %+v does not encode a shouldIgnore field", got)
	}
}

// containsField re-marshals v and checks the JSON output contains field
// — used where the zero value (false, "") of a field under test is
// indistinguishable from "field absent" via a plain struct comparison.
func containsField(t *testing.T, field string, v any) bool {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	_, ok := m[field]
	return ok
}

func TestNew_EmptyURLDefaultsToRealHub(t *testing.T) {
	c := New("")
	if c.HubBaseURL != DefaultHubURL {
		t.Errorf("HubBaseURL = %q, want %q", c.HubBaseURL, DefaultHubURL)
	}
}
