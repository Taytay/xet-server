package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/guilt/xet-server/internal/merklehash"
)

// fakeCAS is a minimal casInfo implementation for testing hubserver in
// isolation, without a real casserver.Server.
type fakeCAS struct {
	sha256ToXet map[string]merklehash.Hash
	sizes       map[merklehash.Hash]int64
	// content backs ReconstructFile for the LFS download tests; a file in
	// sizes but not here fails reconstruction like an evicted xorb would.
	content map[merklehash.Hash][]byte
	missing map[merklehash.Hash][]merklehash.Hash
}

func newFakeCAS() *fakeCAS {
	return &fakeCAS{sha256ToXet: map[string]merklehash.Hash{}, sizes: map[merklehash.Hash]int64{}, content: map[merklehash.Hash][]byte{}, missing: map[merklehash.Hash][]merklehash.Hash{}}
}

func (f *fakeCAS) ReconstructFile(_ context.Context, fileHash merklehash.Hash, start, end int64, w io.Writer) error {
	data, ok := f.content[fileHash]
	if !ok {
		return errors.New("fakeCAS: no content for file")
	}
	if start < 0 || start > end || end >= int64(len(data)) {
		return errors.New("fakeCAS: range not satisfiable")
	}
	_, err := w.Write(data[start : end+1])
	return err
}

// missing, if set, makes MissingXorbs report those hashes for the file:
// the "another replica's xorbs have not synced yet" state.
func (f *fakeCAS) MissingXorbs(_ context.Context, fileHash merklehash.Hash) ([]merklehash.Hash, error) {
	if _, ok := f.sizes[fileHash]; !ok {
		return nil, errors.New("fakeCAS: unknown file")
	}
	return f.missing[fileHash], nil
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
	// xet-core) decodes this response as JSON, not from headers - an empty
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

func TestRevisions_MainCreatedImplicitlyOnRepoCreate(t *testing.T) {
	ts, _ := newTestServer(t)

	resp, err := http.Post(ts.URL+"/api/repos/create", "application/json", strings.NewReader(`{"name":"my-model","organization":"alice","type":"model"}`))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	resp.Body.Close()

	// A resolve against the implicit "main" revision on a freshly-created,
	// never-committed-to repo must 404 (file not found), not error out as
	// if the revision itself doesn't exist - main always exists once the
	// repo does.
	req, _ := http.NewRequest(http.MethodHead, ts.URL+"/alice/my-model/resolve/main/anything.bin", nil)
	resolveResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD error = %v", err)
	}
	defer resolveResp.Body.Close()
	if resolveResp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (file not found on an existing revision, not a revision-not-found error)", resolveResp.StatusCode)
	}
}

func TestRevisions_UnknownRevisionReturns404NotImplicitlyCreated(t *testing.T) {
	ts, _ := newTestServer(t)

	// Commit only to "main"; a resolve against a revision that was never
	// committed to must 404 as a revision lookup failure, and must not
	// have silently created that revision as a side effect of the lookup.
	ndjson := `{"key":"lfsFile","value":{"path":"model.bin","algo":"sha256","oid":"deadbeef","size":100}}` + "\n"
	commitResp, err := http.Post(ts.URL+"/api/models/alice/my-model/commit/main", "application/x-ndjson", strings.NewReader(ndjson))
	if err != nil {
		t.Fatalf("commit POST error = %v", err)
	}
	commitResp.Body.Close()

	req, _ := http.NewRequest(http.MethodHead, ts.URL+"/alice/my-model/resolve/never-committed-branch/model.bin", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a revision that was never committed to", resp.StatusCode)
	}
}

func TestRevisions_IndependentFileSetsPerRevision(t *testing.T) {
	xetHashMain := merklehash.ComputeDataHash([]byte("main branch content"))
	xetHashDev := merklehash.ComputeDataHash([]byte("dev branch content"))
	cas := newFakeCAS()
	cas.sha256ToXet["main-oid"] = xetHashMain
	cas.sha256ToXet["dev-oid"] = xetHashDev
	cas.sizes[xetHashMain] = 111
	cas.sizes[xetHashDev] = 222

	hubSrv := New("http://localhost:9999", cas)
	ts := httptest.NewServer(hubSrv)
	defer ts.Close()

	// Commit a file to "main" with one OID/size...
	mainNdjson := `{"key":"lfsFile","value":{"path":"model.bin","algo":"sha256","oid":"main-oid","size":111}}` + "\n"
	mainResp, err := http.Post(ts.URL+"/api/models/alice/my-model/commit/main", "application/x-ndjson", strings.NewReader(mainNdjson))
	if err != nil {
		t.Fatalf("main commit error = %v", err)
	}
	mainResp.Body.Close()

	// ...and the SAME path to a different revision with a DIFFERENT
	// OID/size - real branches diverge exactly like this.
	devNdjson := `{"key":"lfsFile","value":{"path":"model.bin","algo":"sha256","oid":"dev-oid","size":222}}` + "\n"
	devResp, err := http.Post(ts.URL+"/api/models/alice/my-model/commit/dev", "application/x-ndjson", strings.NewReader(devNdjson))
	if err != nil {
		t.Fatalf("dev commit error = %v", err)
	}
	devResp.Body.Close()

	// Resolving model.bin on "main" must return main's XetHash/size...
	mainReq, _ := http.NewRequest(http.MethodHead, ts.URL+"/alice/my-model/resolve/main/model.bin", nil)
	mainResolve, err := http.DefaultClient.Do(mainReq)
	if err != nil {
		t.Fatalf("main resolve HEAD error = %v", err)
	}
	defer mainResolve.Body.Close()
	if got := mainResolve.Header.Get("X-Xet-Hash"); got != xetHashMain.Hex() {
		t.Errorf("main revision X-Xet-Hash = %q, want %q", got, xetHashMain.Hex())
	}
	if got := mainResolve.Header.Get("X-Linked-Size"); got != "111" {
		t.Errorf("main revision X-Linked-Size = %q, want %q", got, "111")
	}

	// ...and resolving the SAME path on "dev" must return dev's, proving
	// the two revisions' file sets are genuinely independent, not sharing
	// one map keyed by path alone.
	devReq, _ := http.NewRequest(http.MethodHead, ts.URL+"/alice/my-model/resolve/dev/model.bin", nil)
	devResolve, err := http.DefaultClient.Do(devReq)
	if err != nil {
		t.Fatalf("dev resolve HEAD error = %v", err)
	}
	defer devResolve.Body.Close()
	if got := devResolve.Header.Get("X-Xet-Hash"); got != xetHashDev.Hex() {
		t.Errorf("dev revision X-Xet-Hash = %q, want %q", got, xetHashDev.Hex())
	}
	if got := devResolve.Header.Get("X-Linked-Size"); got != "222" {
		t.Errorf("dev revision X-Linked-Size = %q, want %q", got, "222")
	}
}

func TestRevisions_IndependentCommitHistoryPerRevision(t *testing.T) {
	ts, _ := newTestServer(t)

	mainNdjson := `{"key":"lfsFile","value":{"path":"a.bin","algo":"sha256","oid":"a","size":1}}` + "\n"
	mainResp, err := http.Post(ts.URL+"/api/models/alice/my-model/commit/main", "application/x-ndjson", strings.NewReader(mainNdjson))
	if err != nil {
		t.Fatalf("main commit error = %v", err)
	}
	defer mainResp.Body.Close()
	var mainCommit commitResponse
	if err := json.NewDecoder(mainResp.Body).Decode(&mainCommit); err != nil {
		t.Fatalf("decode main commit response error = %v", err)
	}

	devNdjson := `{"key":"lfsFile","value":{"path":"b.bin","algo":"sha256","oid":"b","size":2}}` + "\n"
	devResp, err := http.Post(ts.URL+"/api/models/alice/my-model/commit/dev", "application/x-ndjson", strings.NewReader(devNdjson))
	if err != nil {
		t.Fatalf("dev commit error = %v", err)
	}
	defer devResp.Body.Close()
	var devCommit commitResponse
	if err := json.NewDecoder(devResp.Body).Decode(&devCommit); err != nil {
		t.Fatalf("decode dev commit response error = %v", err)
	}

	if mainCommit.CommitOID == devCommit.CommitOID {
		t.Error("main and dev commit OIDs are identical, want independent commit history per revision")
	}
	if mainCommit.CommitOID == "" || devCommit.CommitOID == "" {
		t.Error("expected non-empty commit OIDs for both revisions")
	}
}
