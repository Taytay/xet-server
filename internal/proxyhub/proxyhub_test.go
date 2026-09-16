package proxyhub

// Tests for the thin proxyhub wrapper: does it correctly delegate to
// its embedded *hubserver.Server, fetch-and-ingest on a miss, and stay
// offline-capable (per -cache-ttl's stale-fallback policy) once cached?
// hubserver's own test suite already covers repo/revision/file state
// management, resolve header shape, and snapshotting in depth - these
// tests deliberately don't re-verify any of that, only that THIS
// package's caching/delegation/xet-token-rewriting logic is correct.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/guilt/xet-server/internal/hfclient"
)

func newTestServer(hubTS *httptest.Server, casBaseURL string) *Server {
	return New(hubTS.URL, casBaseURL)
}

func TestRepoInfo_MissFetchesFromUpstreamAndServesLocally(t *testing.T) {
	fetches := 0
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
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
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if fetches != 1 {
		t.Errorf("upstream fetched %d times, want 1", fetches)
	}
	if !s.Embedded.HasRevision("model", "alice/my-model", "main") {
		t.Error("embedded server does not have the revision after a fetch-and-ingest")
	}
}

func TestRepoInfo_WithinTTLIsLocalHitNoUpstreamCall(t *testing.T) {
	fetches := 0
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
		json.NewEncoder(w).Encode(hfclient.RepoInfo{ID: "alice/my-model", SHA: "main"})
	}))
	defer hubTS.Close()

	s := newTestServer(hubTS, "http://localhost:8420")
	s.CacheTTL = time.Hour
	ts := httptest.NewServer(s)
	defer ts.Close()

	for i := 0; i < 3; i++ {
		resp, err := http.Get(ts.URL + "/api/models/alice/my-model/revision/main")
		if err != nil {
			t.Fatalf("GET #%d error = %v", i, err)
		}
		resp.Body.Close()
	}
	if fetches != 1 {
		t.Errorf("upstream fetched %d times across 3 requests within TTL, want 1", fetches)
	}
}

func TestRepoInfo_OfflineAfterCacheStillServesRegardlessOfTTL(t *testing.T) {
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(hfclient.RepoInfo{ID: "alice/my-model", SHA: "main"})
	}))

	s := newTestServer(hubTS, "http://localhost:8420")
	ts := httptest.NewServer(s)
	defer ts.Close()

	primeResp, err := http.Get(ts.URL + "/api/models/alice/my-model/revision/main")
	if err != nil {
		t.Fatalf("priming GET error = %v", err)
	}
	primeResp.Body.Close()

	hubTS.Close()

	resp, err := http.Get(ts.URL + "/api/models/alice/my-model/revision/main")
	if err != nil {
		t.Fatalf("GET after upstream went offline error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (must serve from the embedded server with upstream offline)", resp.StatusCode)
	}
}

func TestRepoInfo_UnreachableUpstreamWithNothingCachedReturns502(t *testing.T) {
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	hubTS.Close()

	s := newTestServer(hubTS, "http://localhost:8420")
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/models/alice/my-model/revision/main")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

func TestRepoInfo_NoCacheAlwaysRelaysLiveNoFallback(t *testing.T) {
	fetches := 0
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
		json.NewEncoder(w).Encode(hfclient.RepoInfo{ID: "alice/my-model", SHA: "main"})
	}))

	s := newTestServer(hubTS, "http://localhost:8420")
	s.NoCache = true
	ts := httptest.NewServer(s)
	defer ts.Close()

	primeResp, err := http.Get(ts.URL + "/api/models/alice/my-model/revision/main")
	if err != nil {
		t.Fatalf("priming GET error = %v", err)
	}
	primeResp.Body.Close()

	hubTS.Close()

	resp, err := http.Get(ts.URL + "/api/models/alice/my-model/revision/main")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 under -no-cache (no fallback)", resp.StatusCode)
	}
}

func TestXetToken_RewritesCasURLButPassesAccessTokenThrough(t *testing.T) {
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(hfclient.XetToken{CasURL: "https://real-cas.example.invalid", Exp: 123, AccessToken: "real-upstream-token"})
	}))
	defer hubTS.Close()

	s := newTestServer(hubTS, "http://localhost:8420")
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/models/alice/my-model/xet-read-token/main")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	var tok hfclient.XetToken
	json.NewDecoder(resp.Body).Decode(&tok)
	if tok.CasURL != "http://localhost:8420" {
		t.Errorf("CasURL = %q, want it rewritten", tok.CasURL)
	}
	if tok.AccessToken != "real-upstream-token" {
		t.Errorf("AccessToken = %q, want the real upstream token unchanged", tok.AccessToken)
	}

	casURL, ok := s.UpstreamCASBaseURL()
	if !ok || casURL != "https://real-cas.example.invalid" {
		t.Errorf("UpstreamCASBaseURL() = (%q, %v), want the real upstream CAS URL", casURL, ok)
	}
}

func TestXetToken_OfflineAfterCacheStillServesRewrittenToken(t *testing.T) {
	// The cached token's exp must be comfortably in the future: the
	// stale-fallback guard (token.go's tokenSafetyMargin) refuses to serve an
	// expired cached token, so this offline test only stays valid while the
	// cached token is still usable.
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(hfclient.XetToken{CasURL: "https://real-cas.example.invalid", Exp: time.Now().Add(time.Hour).Unix(), AccessToken: "real-upstream-token"})
	}))

	s := newTestServer(hubTS, "http://localhost:8420")
	ts := httptest.NewServer(s)
	defer ts.Close()

	primeResp, err := http.Get(ts.URL + "/api/models/alice/my-model/xet-read-token/main")
	if err != nil {
		t.Fatalf("priming GET error = %v", err)
	}
	primeResp.Body.Close()

	hubTS.Close()

	resp, err := http.Get(ts.URL + "/api/models/alice/my-model/xet-read-token/main")
	if err != nil {
		t.Fatalf("GET after upstream went offline error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (stale token still servable offline)", resp.StatusCode)
	}
}

func TestXetToken_ExposesTokenExpirationHeader(t *testing.T) {
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(hfclient.XetToken{CasURL: "https://real-cas.example.invalid", Exp: 999, AccessToken: "real-upstream-token"})
	}))
	defer hubTS.Close()

	s := newTestServer(hubTS, "http://localhost:8420")
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/models/alice/my-model/xet-read-token/main")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Xet-Token-Expiration"); got != "999" {
		t.Errorf("X-Xet-Token-Expiration = %q, want %q (huggingface_hub's parse_xet_connection_info_from_headers reads this)", got, "999")
	}
}

func TestXetToken_NoCacheStillRecordsUpstreamCASBaseURL(t *testing.T) {
	// Regression test: the real upstream CAS URL is routing state (which
	// address the CAS-facing proxy relays to), not cached user data -
	// withholding it under -no-cache used to leave UpstreamCASBaseURL
	// permanently unanswered, 503ing every CAS request forever even
	// though every Hub call was still being relayed live.
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(hfclient.XetToken{CasURL: "https://real-cas.example.invalid", Exp: 123, AccessToken: "real-upstream-token"})
	}))
	defer hubTS.Close()

	s := newTestServer(hubTS, "http://localhost:8420")
	s.NoCache = true
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/models/alice/my-model/xet-read-token/main")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	resp.Body.Close()

	casURL, ok := s.UpstreamCASBaseURL()
	if !ok || casURL != "https://real-cas.example.invalid" {
		t.Errorf("UpstreamCASBaseURL() = (%q, %v), want the real upstream CAS URL even under -no-cache", casURL, ok)
	}
}

func TestListTree_MissIngestsFilesAndServesLocally(t *testing.T) {
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
	s.CacheTTL = time.Hour
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/models/alice/my-model/tree/main")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	var entries []hfclient.TreeEntry
	json.NewDecoder(resp.Body).Decode(&entries)
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if !s.Embedded.HasFile("model", "alice/my-model", "main", "data/train.bin") {
		t.Error("embedded server does not have the tree-listed file after ingest")
	}

	resp2, err := http.Get(ts.URL + "/api/models/alice/my-model/tree/main")
	if err != nil {
		t.Fatalf("GET #2 error = %v", err)
	}
	resp2.Body.Close()
	if fetches != 1 {
		t.Errorf("upstream fetched %d times across 2 requests within TTL, want 1", fetches)
	}
}

func TestListTree_DirectoryEntriesNotIngestedOrServedAsFiles(t *testing.T) {
	// The upstream tree interleaves real files with directory entries
	// (type "directory", size 0) - e.g. bigcode/the-stack-v2's top-level
	// "data" folder and every per-language subfolder. A directory ingested
	// as a file is later served as "type": "file", which makes
	// huggingface_hub's snapshot_download try to resolve it and 404. This
	// is the regression test for that bug: directories must be dropped at
	// ingest and never appear in the served listing.
	fetches := 0
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
		json.NewEncoder(w).Encode([]hfclient.TreeEntry{
			{Type: "file", Path: "README.md", Size: 10, OID: "aaa"},
			{Type: "directory", Path: "data", Size: 0, OID: "d1"},
			{Type: "file", Path: "data/train.bin", Size: 20, OID: "bbb"},
			{Type: "directory", Path: "data/nested", Size: 0, OID: "d2"},
			{Type: "file", Path: "empty.txt", Size: 0, OID: "ccc"},
		})
	}))
	defer hubTS.Close()

	s := newTestServer(hubTS, "http://localhost:8420")
	s.CacheTTL = time.Hour
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/models/alice/my-model/tree/main")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	var entries []hfclient.TreeEntry
	json.NewDecoder(resp.Body).Decode(&entries)

	want := map[string]bool{"README.md": true, "data/train.bin": true, "empty.txt": true}
	if len(entries) != len(want) {
		t.Fatalf("got %d entries %+v, want only the %d file entries %v", len(entries), entries, len(want), want)
	}
	for _, e := range entries {
		if e.Type != "file" {
			t.Errorf("entry %q served with type %q, want %q", e.Path, e.Type, "file")
		}
		if !want[e.Path] {
			t.Errorf("unexpected entry %q served", e.Path)
		}
	}

	if s.Embedded.HasFile("model", "alice/my-model", "main", "data") {
		t.Error("embedded server ingested directory entry 'data' as a file")
	}
	if s.Embedded.HasFile("model", "alice/my-model", "main", "data/nested") {
		t.Error("embedded server ingested directory entry 'data/nested' as a file")
	}
	for _, p := range []string{"README.md", "data/train.bin", "empty.txt"} {
		if !s.Embedded.HasFile("model", "alice/my-model", "main", p) {
			t.Errorf("embedded server missing file %q after ingest", p)
		}
	}
}

func TestResolve_MissIngestsFileAndServesLocally(t *testing.T) {
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Xet-Hash", "abababababababababababababababababababababababababababababababab")
		w.Header().Set("X-Linked-Size", "42")
		w.Header().Set("ETag", `"etag123"`)
		w.Header().Set("X-Repo-Commit", "commitoid")
		w.WriteHeader(http.StatusOK)
	}))
	defer hubTS.Close()

	s := newTestServer(hubTS, "http://localhost:8420")
	s.CacheTTL = time.Hour
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
	if got := resp.Header.Get("X-Xet-Hash"); got != "abababababababababababababababababababababababababababababababab" {
		t.Errorf("X-Xet-Hash = %q, want %q", got, "abababababababababababababababababababababababababababababababab")
	}
	if !s.Embedded.HasFile("model", "alice/my-model", "main", "model.bin") {
		t.Error("embedded server does not have the resolved file after ingest")
	}
}

func TestResolve_OfflineAfterCacheStillServes(t *testing.T) {
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Xet-Hash", "abababababababababababababababababababababababababababababababab")
		w.Header().Set("X-Linked-Size", "42")
		w.WriteHeader(http.StatusOK)
	}))

	s := newTestServer(hubTS, "http://localhost:8420")
	ts := httptest.NewServer(s)
	defer ts.Close()

	primeReq, _ := http.NewRequest(http.MethodHead, ts.URL+"/alice/my-model/resolve/main/model.bin", nil)
	primeResp, err := http.DefaultClient.Do(primeReq)
	if err != nil {
		t.Fatalf("priming HEAD error = %v", err)
	}
	primeResp.Body.Close()

	hubTS.Close()

	req, _ := http.NewRequest(http.MethodHead, ts.URL+"/alice/my-model/resolve/main/model.bin", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD after upstream went offline error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (stale resolve metadata still servable offline)", resp.StatusCode)
	}
}

func TestResolve_PlainFileHeadRelayedLiveNotIngested(t *testing.T) {
	// A plain (non-Xet) file - e.g. bigcode/the-stack-v2's top-level
	// .gitattributes, README.md, *_stats.csv - has no X-Xet-Hash upstream,
	// so Embedded can never serve it (its resolve handler requires CAS
	// reconstruction data keyed by a Xet hash). The proxy must relay the
	// real Hub's response live instead of serving a CAS-backed 404.
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
	s.CacheTTL = time.Hour
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
		t.Errorf("ETag = %q, want %q (relayed live from upstream)", got, `"etag-plain"`)
	}
	if s.Embedded.HasFile("model", "alice/my-model", "main", "config.json") {
		t.Error("plain (non-Xet) file must NOT be ingested into Embedded - it would be served back as a CAS-backed 404")
	}
}

func TestResolve_PlainFileGetStreamsBytesLive(t *testing.T) {
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
	s.CacheTTL = time.Hour
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/alice/my-model/resolve/main/config.json")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body error = %v", err)
	}
	if string(body) != "PLAIN-BYTES" {
		t.Errorf("body = %q, want %q (streamed live from upstream)", body, "PLAIN-BYTES")
	}
}

func TestCreateRepo_AlwaysRelaysLiveAndIngests(t *testing.T) {
	calls := 0
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"url": "alice/my-model"})
	}))
	defer hubTS.Close()

	s := newTestServer(hubTS, "http://localhost:8420")
	ts := httptest.NewServer(s)
	defer ts.Close()

	body := `{"name":"my-model","organization":"alice","type":"model"}`
	for i := 0; i < 2; i++ {
		resp, err := http.Post(ts.URL+"/api/repos/create", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST #%d error = %v", i, err)
		}
		resp.Body.Close()
	}
	if calls != 2 {
		t.Errorf("upstream called %d times across 2 create-repo requests, want 2 (never cached)", calls)
	}
	if !s.Embedded.HasRepo("model", "alice/my-model") {
		t.Error("embedded server does not have the repo after create+ingest")
	}
}

func TestCommit_RelaysAndIngestsWithRealCommitOID(t *testing.T) {
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(hfclient.CommitResult{CommitOID: "realcommitoid", CommitURL: "/alice/my-model/commit/realcommitoid"})
	}))
	defer hubTS.Close()

	s := newTestServer(hubTS, "http://localhost:8420")
	ts := httptest.NewServer(s)
	defer ts.Close()

	ndjson := `{"key":"lfsFile","value":{"path":"model.bin","oid":"abc123","size":10}}` + "\n"
	resp, err := http.Post(ts.URL+"/api/models/alice/my-model/commit/main", "application/x-ndjson", strings.NewReader(ndjson))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()
	var result hfclient.CommitResult
	json.NewDecoder(resp.Body).Decode(&result)
	if result.CommitOID != "realcommitoid" {
		t.Errorf("CommitOID = %q, want %q", result.CommitOID, "realcommitoid")
	}
	if !s.Embedded.HasFile("model", "alice/my-model", "main", "model.bin") {
		t.Error("embedded server does not have the committed file after ingest")
	}
}

func TestPreupload_RelaysShouldIgnoreFieldToRealHfClient(t *testing.T) {
	// Regression test: the real hf CLI's _fetch_upload_modes reads
	// file["shouldIgnore"] with no default - a proxied preupload
	// response missing this field crashes it with a KeyError instead of
	// a clean error, even though this proxy's own Go code decoded and
	// re-encoded the response successfully. Caught via the real
	// integration test (integration-tests/xet_proxyd_offline_handoff.sh)
	// driving the actual hf CLI through this proxy; this unit test pins
	// the fix at the wire-response-shape level so it can't regress
	// silently.
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"files": []hfclient.PreuploadResult{{Path: "model.bin", UploadMode: "xet", ShouldIgnore: false}},
		})
	}))
	defer hubTS.Close()

	s := newTestServer(hubTS, "http://localhost:8420")
	ts := httptest.NewServer(s)
	defer ts.Close()

	body := `{"files":[{"path":"model.bin","size":10}]}`
	resp, err := http.Post(ts.URL+"/api/models/alice/my-model/preupload/main", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()

	var decoded struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode response error = %v", err)
	}
	if len(decoded.Files) != 1 {
		t.Fatalf("files = %+v, want exactly 1", decoded.Files)
	}
	if _, ok := decoded.Files[0]["shouldIgnore"]; !ok {
		t.Errorf("preupload response file entry %+v is missing \"shouldIgnore\" - the real hf CLI reads this key unconditionally", decoded.Files[0])
	}
}
