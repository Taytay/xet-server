package proxyhub

// Coverage for handlers/paths not already exercised by proxyhub_test.go:
// branch creation, preupload, -no-cache for tree/resolve (the
// writeTreeJSON/writeResolveHeaders paths), tree path-prefix filtering,
// and auth-gate delegation.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/guilt/xet-server/internal/auth"
	"github.com/guilt/xet-server/internal/hfclient"
)

const testFixtureToken = "test-fixture-token-not-a-real-secret"

func TestCreateBranch_RelaysAndIngests(t *testing.T) {
	var gotPath string
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer hubTS.Close()

	s := newTestServer(hubTS, "http://localhost:8420")
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/models/alice/my-model/branch/experiment-1", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if want := "/api/models/alice/my-model/branch/experiment-1"; gotPath != want {
		t.Errorf("upstream path = %q, want %q", gotPath, want)
	}
	if !s.Embedded.HasRevision("model", "alice/my-model", "experiment-1") {
		t.Error("embedded server does not have the branch after create+ingest")
	}
}

func TestPreupload_SendsFilesAndReturnsNegotiatedModes(t *testing.T) {
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Files []hfclient.PreuploadFile `json:"files"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if len(req.Files) != 1 || req.Files[0].Path != "model.bin" {
			t.Errorf("upstream received files = %+v, want one entry for model.bin", req.Files)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"files": []hfclient.PreuploadResult{{Path: "model.bin", UploadMode: "lfs"}},
		})
	}))
	defer hubTS.Close()

	s := newTestServer(hubTS, "http://localhost:8420")
	ts := httptest.NewServer(s)
	defer ts.Close()

	body := `{"files":[{"path":"model.bin","size":1024}]}`
	resp, err := http.Post(ts.URL+"/api/models/alice/my-model/preupload/main", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var result struct {
		Files []hfclient.PreuploadResult `json:"files"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if len(result.Files) != 1 || result.Files[0].UploadMode != "lfs" {
		t.Errorf("result = %+v, want one lfs entry", result.Files)
	}
}

func TestListTree_NoCacheRelaysLiveAndFiltersByPath(t *testing.T) {
	fetches := 0
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
		json.NewEncoder(w).Encode([]hfclient.TreeEntry{
			{Type: "file", Path: "README.md", Size: 10, OID: "aaa"},
			{Type: "file", Path: "data/train.bin", Size: 20, OID: "bbb"},
		})
	}))
	defer hubTS.Close()

	s := newTestServer(hubTS, "http://localhost:8420")
	s.NoCache = true
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/models/alice/my-model/tree/main/data")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	var entries []hfclient.TreeEntry
	json.NewDecoder(resp.Body).Decode(&entries)
	if len(entries) != 1 || entries[0].Path != "data/train.bin" {
		t.Errorf("entries = %+v, want just data/train.bin (path-filtered)", entries)
	}

	resp2, err := http.Get(ts.URL + "/api/models/alice/my-model/tree/main/data")
	if err != nil {
		t.Fatalf("GET #2 error = %v", err)
	}
	resp2.Body.Close()
	if fetches != 2 {
		t.Errorf("upstream fetched %d times across 2 requests under -no-cache, want 2", fetches)
	}
}

func TestListTree_NoCacheFiltersOutDirectoryEntries(t *testing.T) {
	fetches := 0
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
		json.NewEncoder(w).Encode([]hfclient.TreeEntry{
			{Type: "file", Path: "README.md", Size: 10, OID: "aaa"},
			{Type: "directory", Path: "data", Size: 0, OID: "d1"},
			{Type: "file", Path: "data/train.bin", Size: 20, OID: "bbb"},
			{Type: "directory", Path: "data/nested", Size: 0, OID: "d2"},
		})
	}))
	defer hubTS.Close()

	s := newTestServer(hubTS, "http://localhost:8420")
	s.NoCache = true
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/models/alice/my-model/tree/main")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	var entries []hfclient.TreeEntry
	json.NewDecoder(resp.Body).Decode(&entries)
	if len(entries) != 2 || entries[0].Path != "README.md" || entries[1].Path != "data/train.bin" {
		t.Errorf("entries = %+v, want just README.md and data/train.bin (directory entries filtered)", entries)
	}
}

func TestResolve_NoCacheRelaysLiveHeaders(t *testing.T) {
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Xet-Hash", "deadbeef")
		w.Header().Set("X-Linked-Size", "42")
		w.WriteHeader(http.StatusOK)
	}))
	defer hubTS.Close()

	s := newTestServer(hubTS, "http://localhost:8420")
	s.NoCache = true
	ts := httptest.NewServer(s)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodHead, ts.URL+"/alice/my-model/resolve/main/model.bin", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Xet-Hash"); got != "deadbeef" {
		t.Errorf("X-Xet-Hash = %q, want %q", got, "deadbeef")
	}
	if got := resp.Header.Get("X-Xet-Refresh-Route"); got == "" {
		t.Error("X-Xet-Refresh-Route header missing")
	}
}

func TestResolve_NoCachePlainFileRelayedLive(t *testing.T) {
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("ETag", `"etag-plain"`)
			w.Header().Set("X-Repo-Commit", "commitoid")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("PLAIN-BYTES"))
	}))
	defer hubTS.Close()

	s := newTestServer(hubTS, "http://localhost:8420")
	s.NoCache = true
	ts := httptest.NewServer(s)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodHead, ts.URL+"/alice/my-model/resolve/main/config.json", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD error = %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("ETag"); got != `"etag-plain"` {
		t.Errorf("ETag = %q, want %q (relayed live from upstream under -no-cache)", got, `"etag-plain"`)
	}
}

func TestAuth_BackwardCompat_DefaultServerRequiresNoToken(t *testing.T) {
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(hfclient.RepoInfo{ID: "alice/my-model", SHA: "main"})
	}))
	defer hubTS.Close()

	s := newTestServer(hubTS, "http://localhost:8420")
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/models/alice/my-model/revision/main")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 with no auth configured", resp.StatusCode)
	}
}

func TestAuth_ReadEndpoint_RequiresToken(t *testing.T) {
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(hfclient.RepoInfo{ID: "alice/my-model", SHA: "main"})
	}))
	defer hubTS.Close()

	s := newTestServer(hubTS, "http://localhost:8420")
	s.SetAuthenticator(auth.NewStaticTokenAuth(testFixtureToken))
	ts := httptest.NewServer(s)
	defer ts.Close()

	noTokenResp, err := http.Get(ts.URL + "/api/models/alice/my-model/revision/main")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	noTokenResp.Body.Close()
	if noTokenResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-token status = %d, want 401", noTokenResp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/models/alice/my-model/revision/main", nil)
	req.Header.Set("Authorization", "Bearer "+testFixtureToken)
	correctResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer correctResp.Body.Close()
	if correctResp.StatusCode != http.StatusOK {
		t.Errorf("correct-token status = %d, want 200", correctResp.StatusCode)
	}
}

func TestAuth_CredentialPassthrough_ForwardsCallersOwnToken(t *testing.T) {
	var gotAuth string
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(hfclient.RepoInfo{ID: "alice/my-model", SHA: "main"})
	}))
	defer hubTS.Close()

	s := newTestServer(hubTS, "http://localhost:8420")
	s.SetAuthenticator(auth.NewStaticTokenAuth(testFixtureToken))
	ts := httptest.NewServer(s)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/models/alice/my-model/revision/main", nil)
	req.Header.Set("Authorization", "Bearer "+testFixtureToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if gotAuth != "Bearer "+testFixtureToken {
		t.Errorf("upstream Authorization = %q, want the caller's own token forwarded unchanged", gotAuth)
	}
}
