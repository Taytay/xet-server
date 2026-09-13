package hubserver

// Tests for the three endpoints closing the whole-repo-download gap:
// GET .../revision/{revision} (repo info - huggingface_hub's
// snapshot_download resolves this before listing/downloading files), GET
// .../tree/{revision} (file listing), and POST .../branch/{branch}
// (branch creation, called by `hf upload`'s CLI command when pushing to a
// revision that doesn't exist yet). Discovered missing by driving the
// real `hf` CLI end-to-end against this shim - `hf download REPO_ID` (no
// filename) 404s without these; `hf download REPO_ID FILENAME` (a single
// named file) already worked before this fix and still does.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestRepoInfo_ExistingRevisionReturnsSHA(t *testing.T) {
	ts, _ := newTestServer(t)

	ndjson := `{"key":"lfsFile","value":{"path":"model.bin","algo":"sha256","oid":"deadbeef","size":100}}` + "\n"
	commitResp, err := http.Post(ts.URL+"/api/models/alice/my-model/commit/main", "application/x-ndjson", strings.NewReader(ndjson))
	if err != nil {
		t.Fatalf("commit POST error = %v", err)
	}
	commitResp.Body.Close()

	resp, err := http.Get(ts.URL + "/api/models/alice/my-model/revision/main")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got repoInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode error = %v", err)
	}
	if got.ID != "alice/my-model" {
		t.Errorf("ID = %q, want %q", got.ID, "alice/my-model")
	}
	if got.SHA != "main" {
		t.Errorf("SHA = %q, want %q", got.SHA, "main")
	}
}

func TestRepoInfo_MainExistsImplicitlyEvenWithoutACommit(t *testing.T) {
	ts, _ := newTestServer(t)

	// "main" always exists once the repo does (created implicitly), even
	// before any commit - matching how a real repo always has a default
	// branch.
	resp, err := http.Get(ts.URL + "/api/models/alice/never-committed/revision/main")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (main exists implicitly)", resp.StatusCode)
	}
}

func TestRepoInfo_UnknownRevisionReturns404WithRevisionNotFoundErrorCode(t *testing.T) {
	ts, _ := newTestServer(t)

	resp, err := http.Get(ts.URL + "/api/models/alice/my-model/revision/never-existed")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	// huggingface_hub's hf_raise_for_status only raises the specific
	// RevisionNotFoundError (which `hf upload`'s branch-creation step
	// specifically catches) when this header is present on a 404 - a
	// plain 404 falls through to a generic, uncaught HfHubHTTPError
	// instead. This header is the actual fix, not the status code alone.
	if got := resp.Header.Get("X-Error-Code"); got != "RevisionNotFound" {
		t.Errorf("X-Error-Code = %q, want %q", got, "RevisionNotFound")
	}
}

func TestRepoInfo_UnknownRevisionDoesNotCreateIt(t *testing.T) {
	ts, _ := newTestServer(t)

	resp, err := http.Get(ts.URL + "/api/models/alice/my-model/revision/should-not-exist")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	resp.Body.Close()

	// A read-only info lookup must not have the side effect of creating
	// the revision it just reported as missing.
	resp2, err := http.Get(ts.URL + "/api/models/alice/my-model/revision/should-not-exist")
	if err != nil {
		t.Fatalf("second GET error = %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("second lookup status = %d, want 404 (revision must still not exist)", resp2.StatusCode)
	}
}

func TestListTree_ReturnsCommittedFiles(t *testing.T) {
	ts, _ := newTestServer(t)

	ndjson := `{"key":"lfsFile","value":{"path":"model.bin","algo":"sha256","oid":"aaa111","size":100}}
{"key":"lfsFile","value":{"path":"config.json","algo":"sha256","oid":"bbb222","size":50}}
`
	commitResp, err := http.Post(ts.URL+"/api/models/alice/my-model/commit/main", "application/x-ndjson", strings.NewReader(ndjson))
	if err != nil {
		t.Fatalf("commit POST error = %v", err)
	}
	commitResp.Body.Close()

	resp, err := http.Get(ts.URL + "/api/models/alice/my-model/tree/main")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// list_repo_tree's paginate() helper does `yield from r.json()` - the
	// response must be a bare JSON array, not an object wrapping one.
	var got []treeEntry
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode error = %v (response must be a bare JSON array)", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	byPath := map[string]treeEntry{}
	for _, e := range got {
		byPath[e.Path] = e
	}
	model, ok := byPath["model.bin"]
	if !ok {
		t.Fatal("missing model.bin entry")
	}
	if model.Type != "file" {
		t.Errorf("model.bin Type = %q, want %q", model.Type, "file")
	}
	if model.Size != 100 {
		t.Errorf("model.bin Size = %d, want 100", model.Size)
	}
	if model.OID != "aaa111" {
		t.Errorf("model.bin OID = %q, want %q", model.OID, "aaa111")
	}
	if _, ok := byPath["config.json"]; !ok {
		t.Fatal("missing config.json entry")
	}
}

func TestListTree_EmptyRevisionReturnsEmptyArrayNotNull(t *testing.T) {
	ts, _ := newTestServer(t)

	resp, err := http.Get(ts.URL + "/api/models/alice/my-model/tree/main")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got []treeEntry
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode error = %v", err)
	}
	if got == nil {
		t.Error("decoded to nil, want an empty (but non-null) array - a null response would fail huggingface_hub's iteration over it")
	}
}

func TestListTree_UnknownRevisionReturns404WithRevisionNotFoundErrorCode(t *testing.T) {
	ts, _ := newTestServer(t)

	resp, err := http.Get(ts.URL + "/api/models/alice/my-model/tree/never-existed")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Error-Code"); got != "RevisionNotFound" {
		t.Errorf("X-Error-Code = %q, want %q", got, "RevisionNotFound")
	}
}

func TestListTree_FiltersByPathInRepoPrefix(t *testing.T) {
	ts, _ := newTestServer(t)

	ndjson := `{"key":"lfsFile","value":{"path":"weights/a.bin","algo":"sha256","oid":"aaa","size":10}}
{"key":"lfsFile","value":{"path":"weights/b.bin","algo":"sha256","oid":"bbb","size":20}}
{"key":"lfsFile","value":{"path":"README.md","algo":"sha256","oid":"ccc","size":5}}
`
	commitResp, err := http.Post(ts.URL+"/api/models/alice/my-model/commit/main", "application/x-ndjson", strings.NewReader(ndjson))
	if err != nil {
		t.Fatalf("commit POST error = %v", err)
	}
	commitResp.Body.Close()

	resp, err := http.Get(ts.URL + "/api/models/alice/my-model/tree/main/weights")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	var got []treeEntry
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries under weights/, want 2", len(got))
	}
	for _, e := range got {
		if !strings.HasPrefix(e.Path, "weights/") {
			t.Errorf("entry %q not under weights/ prefix", e.Path)
		}
	}
}

func TestCreateBranch_MakesRevisionImmediatelyVisible(t *testing.T) {
	ts, _ := newTestServer(t)

	resp, err := http.Post(ts.URL+"/api/models/alice/my-model/branch/dev", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// The whole point of create_branch: a repo-info lookup right
	// afterward must see the branch as existing, not 404.
	infoResp, err := http.Get(ts.URL + "/api/models/alice/my-model/revision/dev")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer infoResp.Body.Close()
	if infoResp.StatusCode != http.StatusOK {
		t.Errorf("revision info status after branch creation = %d, want 200", infoResp.StatusCode)
	}
}

func TestCreateBranch_IsIdempotent(t *testing.T) {
	ts, _ := newTestServer(t)

	for i := 0; i < 2; i++ {
		resp, err := http.Post(ts.URL+"/api/models/alice/my-model/branch/dev", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("POST #%d error = %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("POST #%d status = %d, want 200 (create_branch(exist_ok=True) must not fail on repeat calls)", i, resp.StatusCode)
		}
	}
}
