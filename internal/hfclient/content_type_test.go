package hfclient

// Regression tests for the Content-Type header fixes that make the real
// Hub accept this client's write requests. Real HF is FastAPI-based: a
// JSON/ndjson body without a matching Content-Type is not parsed at all,
// so every field reads as "undefined" (e.g. create-repo fails with
// "expected string, received undefined" on its `name` field), and the CAS
// octet-stream upload endpoints require application/octet-stream.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// requireContentType fails the test unless the fake upstream's request
// carried exactly want.
func requireContentType(t *testing.T, r *http.Request, want string) {
	t.Helper()
	if got := r.Header.Get("Content-Type"); got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
}

func TestCreateRepo_SendsJSONContentType(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requireContentType(t, r, "application/json")
		w.WriteHeader(http.StatusOK)
	})
	if err := c.CreateRepo(context.Background(), nil, "my-model", "alice", "model"); err != nil {
		t.Fatalf("CreateRepo() error = %v", err)
	}
}

func TestCreateBranch_SendsJSONContentType(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requireContentType(t, r, "application/json")
		w.WriteHeader(http.StatusOK)
	})
	if err := c.CreateBranch(context.Background(), nil, "model", "alice/my-model", "dev"); err != nil {
		t.Fatalf("CreateBranch() error = %v", err)
	}
}

func TestCommit_SendsNDJSONContentType(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requireContentType(t, r, "application/x-ndjson")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"commitOid":"oid123","commitUrl":"https://example.invalid/commit"}`))
	})
	if _, err := c.Commit(context.Background(), nil, "model", "alice/my-model", "main", []CommitFile{
		{Path: "model.bin", OID: "oid", Size: 42},
	}); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
}

func TestPreupload_SendsJSONContentType(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requireContentType(t, r, "application/json")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"files":[{"path":"model.bin","uploadMode":"lfs"}]}`))
	})
	if _, err := c.Preupload(context.Background(), nil, "model", "alice/my-model", "main", []PreuploadFile{
		{Path: "model.bin", Size: 42},
	}); err != nil {
		t.Fatalf("Preupload() error = %v", err)
	}
}

// TestRelayRaw_ForwardsBodyAndContentTypeVerbatim pins that RelayRaw sends
// the exact body bytes and Content-Type it is given - the write-path
// escape hatch proxyhub uses for preupload/commit/LFS, where the wire
// shape (per-file `sample`, ndjson LFS pointers) is not modeled here.
func TestRelayRaw_ForwardsBodyAndContentTypeVerbatim(t *testing.T) {
	gotBody := ""
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		requireContentType(t, r, "application/vnd.git-lfs+json")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	})
	payload := `{"operation":"upload","objects":[{"oid":"abc","size":7,"sample":"c2FtcGxl"}]}`
	resp, err := c.RelayRaw(context.Background(), nil, http.MethodPost, "/alice/my-model.git/info/lfs/objects/batch", "application/vnd.git-lfs+json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("RelayRaw() error = %v", err)
	}
	defer resp.Body.Close()
	if gotBody != payload {
		t.Errorf("upstream received body %q, want %q", gotBody, payload)
	}
}
