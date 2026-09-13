package proxyhub

// Regression tests for the write-path transparency fixes made so the real
// hf CLI can upload THROUGH this proxy to the real Hub:
//
//   - writeUpstreamError relays the real upstream error body verbatim
//     (huggingface_hub's create_repo reads `url` off a 409 response).
//   - preupload and commit forward the caller's exact request body (the
//     per-file `sample` field / the ndjson LFS-pointer lines), which a
//     decode-and-reencode used to drop.
//   - Git LFS traffic (/{repo}.git/info/lfs/... and /{repo}.git/objects/...)
//     is relayed live to the real Hub instead of 404ing.
//
// All new tests - nothing here rewrites an existing assertion.

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCreateRepo_409RelaysUpstreamBodyVerbatim pins that a real
// huggingface_hub client tolerating exist_ok=True reads the `url` field
// off the 409 response body - so the proxy must relay the upstream body
// rather than its own {"error": ...} shape.
func TestCreateRepo_409RelaysUpstreamBodyVerbatim(t *testing.T) {
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"You already created this model repo: guilt/test","url":"https://huggingface.co/guilt/test"}`))
	}))
	defer hubTS.Close()

	s := newTestServer(hubTS, "http://localhost:8420")
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/repos/create", "application/json", strings.NewReader(`{"name":"test","organization":"guilt","type":"model"}`))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"url":"https://huggingface.co/guilt/test"`) {
		t.Errorf("relayed body %q does not contain the upstream `url` field", body)
	}
}

// TestPreupload_RelaysRawBodyWithSampleField pins that the preupload
// request body is forwarded verbatim - real huggingface_hub includes a
// per-file `sample` field the real Hub requires, which a
// decode-and-reencode would drop ("expected string, received undefined").
func TestPreupload_RelaysRawBodyWithSampleField(t *testing.T) {
	got := ""
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"files":[{"path":"model.bin","uploadMode":"xet","shouldIgnore":false}]}`))
	}))
	defer hubTS.Close()

	s := newTestServer(hubTS, "http://localhost:8420")
	ts := httptest.NewServer(s)
	defer ts.Close()

	body := `{"files":[{"path":"model.bin","size":1024,"sample":"c2FtcGxlLXNhbXBsZQ=="}]}`
	resp, err := http.Post(ts.URL+"/api/models/alice/my-model/preupload/main", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got != body {
		t.Errorf("upstream received body %q, want the caller's exact body %q", got, body)
	}
}

// TestCommit_RelaysRawNDJSONBody pins that the commit ndjson body is
// forwarded verbatim (its lfsFile lines carry fields beyond path/oid/size).
func TestCommit_RelaysRawNDJSONBody(t *testing.T) {
	got := ""
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"commitOid":"abc123","commitUrl":"https://huggingface.co/guilt/test/commit/abc123"}`))
	}))
	defer hubTS.Close()

	s := newTestServer(hubTS, "http://localhost:8420")
	ts := httptest.NewServer(s)
	defer ts.Close()

	body := `{"key":"lfsFile","value":{"path":"model.bin","oid":"sha256:abc","size":1024,"lfs":{"size":1024,"sha256":"abc","pointerSize":130}}}\n`
	resp, err := http.Post(ts.URL+"/api/models/alice/my-model/commit/main", "application/x-ndjson", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got != body {
		t.Errorf("upstream received body %q, want the caller's exact ndjson %q", got, body)
	}
}

// TestLFSPaths_RelayedLiveToUpstream pins that Git LFS traffic - which a
// small/non-Xet file's hf upload/download generates - is relayed live to
// the real Hub rather than 404'd by the resolve catch-all.
func TestLFSPaths_RelayedLiveToUpstream(t *testing.T) {
	hitBatch := false
	hitObject := false
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/info/lfs/objects/batch"):
			hitBatch = true
			w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
			w.Write([]byte(`{"objects":[{"oid":"abc","actions":{"upload":{"href":"https://example.invalid/upload"}}}]}`))
		case strings.Contains(r.URL.Path, "/objects/"):
			hitObject = true
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("lfs-object-bytes"))
		default:
			t.Errorf("unexpected upstream path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer hubTS.Close()

	s := newTestServer(hubTS, "http://localhost:8420")
	ts := httptest.NewServer(s)
	defer ts.Close()

	// Batch upload negotiation.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/alice/my-model.git/info/lfs/objects/batch", strings.NewReader(`{"operation":"upload"}`))
	req.Header.Set("Content-Type", "application/vnd.git-lfs+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST batch error = %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !hitBatch {
		t.Error("LFS batch path was not relayed to upstream")
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"actions"`) {
		t.Errorf("batch response status=%d body=%q, want the upstream's verbatim 200 with actions", resp.StatusCode, body)
	}

	// Object upload PUT.
	putReq, _ := http.NewRequest(http.MethodPut, ts.URL+"/alice/my-model.git/objects/abc", strings.NewReader("bytes"))
	resp2, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatalf("PUT object error = %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if !hitObject {
		t.Error("LFS object path was not relayed to upstream")
	}
	if resp2.StatusCode != http.StatusOK || string(body2) != "lfs-object-bytes" {
		t.Errorf("object response status=%d body=%q, want upstream's verbatim body", resp2.StatusCode, body2)
	}
}

// TestPreupload_OversizeBodyReturns413 pins the defense-in-depth cap on
// the write-path relay: an unbounded body read here would be a
// memory-exhaustion vector (the commit handler previously used a
// 10MB-bounded scanner; the LFS relay is new). All three write handlers
// share readHubWriteBody, so exercising it through preupload covers them.
func TestPreupload_OversizeBodyReturns413(t *testing.T) {
	upstreamHit := false
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer hubTS.Close()

	s := newTestServer(hubTS, "http://localhost:8420")
	ts := httptest.NewServer(s)
	defer ts.Close()

	oversize := bytes.Repeat([]byte("x"), maxHubWriteBodyBytes+1)
	resp, err := http.Post(ts.URL+"/api/models/alice/my-model/preupload/main", "application/json", bytes.NewReader(oversize))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
	if upstreamHit {
		t.Error("oversize body was relayed upstream; it must be rejected before any upstream call")
	}
}
