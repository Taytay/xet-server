package hubserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"xet-server/internal/merklehash"
)

// fakeCAS is a minimal casInfo implementation for testing hubserver in
// isolation, without a real casserver.Server.
type fakeCAS struct {
	sha256ToXet map[string]merklehash.Hash
	sizes       map[merklehash.Hash]int64
}

func newFakeCAS() *fakeCAS {
	return &fakeCAS{sha256ToXet: map[string]merklehash.Hash{}, sizes: map[merklehash.Hash]int64{}}
}

func (f *fakeCAS) XetHashForSHA256(sha256Hex string) (merklehash.Hash, bool) {
	h, ok := f.sha256ToXet[sha256Hex]
	return h, ok
}

func (f *fakeCAS) FileSize(fileHash merklehash.Hash) (int64, bool) {
	s, ok := f.sizes[fileHash]
	return s, ok
}

func newTestServer(t *testing.T) (*httptest.Server, *fakeCAS) {
	t.Helper()
	cas := newFakeCAS()
	hubSrv := New("http://localhost:9999", cas)
	ts := httptest.NewServer(hubSrv)
	t.Cleanup(ts.Close)
	return ts, cas
}

func TestCreateRepo(t *testing.T) {
	ts, _ := newTestServer(t)

	body := `{"name":"my-model","organization":"alice","type":"model"}`
	resp, err := http.Post(ts.URL+"/api/repos/create", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var got createRepoResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode error = %v", err)
	}
	if got.URL != "alice/my-model" {
		t.Errorf("URL = %q, want %q", got.URL, "alice/my-model")
	}
}

func TestXetReadToken_SetsHeaders(t *testing.T) {
	ts, _ := newTestServer(t)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/models/alice/my-model/xet-read-token/main", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if got := resp.Header.Get("X-Xet-Cas-Url"); got != "http://localhost:9999" {
		t.Errorf("X-Xet-Cas-Url = %q, want %q", got, "http://localhost:9999")
	}
	if resp.Header.Get("X-Xet-Access-Token") == "" {
		t.Error("X-Xet-Access-Token header missing")
	}
	if resp.Header.Get("X-Xet-Token-Expiration") == "" {
		t.Error("X-Xet-Token-Expiration header missing")
	}
}

func TestXetWriteToken_SetsHeaders(t *testing.T) {
	ts, _ := newTestServer(t)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/datasets/bob/my-dataset/xet-write-token/main", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("X-Xet-Access-Token") == "" {
		t.Error("X-Xet-Access-Token header missing")
	}

	// hf_xet's Rust client (DirectRefreshRouteTokenRefresher::get_cas_jwt in
	// xet-core) decodes this response as JSON, not from headers — an empty
	// body here makes it retry indefinitely instead of failing fast. See
	// xet_client/src/hub_client/types.rs's CasJWTInfo for the wire format.
	var body xetTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode JSON body: %v", err)
	}
	if body.CasURL != "http://localhost:9999" {
		t.Errorf("casUrl = %q, want %q", body.CasURL, "http://localhost:9999")
	}
	if body.AccessToken == "" {
		t.Error("accessToken missing from JSON body")
	}
	if body.Exp == 0 {
		t.Error("exp missing from JSON body")
	}
}

func TestCommit_ParsesLfsFileEntries(t *testing.T) {
	ts, _ := newTestServer(t)

	ndjson := `{"key":"header","value":{"summary":"test commit"}}
{"key":"lfsFile","value":{"path":"model.bin","algo":"sha256","oid":"abc123","size":500000}}
`
	resp, err := http.Post(ts.URL+"/api/models/alice/my-model/commit/main", "application/x-ndjson", strings.NewReader(ndjson))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var got commitResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode error = %v", err)
	}
	if got.CommitOID == "" {
		t.Error("CommitOID is empty")
	}
}

func TestResolve_HeadReturnsXetHeaders(t *testing.T) {
	ts, _ := newTestServer(t)

	// Simulate a prior commit registering the file, and the CAS layer
	// having indexed its SHA-256 -> Xet hash mapping (as would happen once
	// the paired shard upload completes).
	ndjson := `{"key":"lfsFile","value":{"path":"model.bin","algo":"sha256","oid":"deadbeef","size":12345}}
`
	commitResp, err := http.Post(ts.URL+"/api/models/alice/my-model/commit/main", "application/x-ndjson", strings.NewReader(ndjson))
	if err != nil {
		t.Fatalf("commit POST error = %v", err)
	}
	commitResp.Body.Close()

	xetHash := merklehash.ComputeDataHash([]byte("model.bin content"))
	cas := newFakeCAS()
	cas.sha256ToXet["deadbeef"] = xetHash
	cas.sizes[xetHash] = 12345

	hubSrv := New("http://localhost:9999", cas)
	ts2 := httptest.NewServer(hubSrv)
	defer ts2.Close()

	commitResp2, err := http.Post(ts2.URL+"/api/models/alice/my-model/commit/main", "application/x-ndjson", strings.NewReader(ndjson))
	if err != nil {
		t.Fatalf("commit POST error = %v", err)
	}
	commitResp2.Body.Close()

	req, _ := http.NewRequest(http.MethodHead, ts2.URL+"/alice/my-model/resolve/main/model.bin", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if got := resp.Header.Get("X-Xet-Hash"); got != xetHash.Hex() {
		t.Errorf("X-Xet-Hash = %q, want %q", got, xetHash.Hex())
	}
	if got := resp.Header.Get("X-Linked-Size"); got != "12345" {
		t.Errorf("X-Linked-Size = %q, want %q", got, "12345")
	}
	if resp.Header.Get("X-Xet-Refresh-Route") == "" {
		t.Error("X-Xet-Refresh-Route header missing")
	}
}

func TestResolve_UnknownFileReturns404(t *testing.T) {
	ts, _ := newTestServer(t)

	req, _ := http.NewRequest(http.MethodHead, ts.URL+"/alice/my-model/resolve/main/never-uploaded.bin", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}
