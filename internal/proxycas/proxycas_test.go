package proxycas

// Tests for the thin proxycas wrapper: does it correctly delegate to
// its embedded *casserver.Server, fetch-and-ingest on a miss, and stay
// fully offline-capable once cached? casserver's own test suite already
// covers byte-range serving, reconstruction-response shape, footer/shard
// indexing, and snapshotting in depth - these tests deliberately don't
// re-verify any of that, only that THIS package's delegation logic is
// correct.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/guilt/xet-server/internal/auth"
	"github.com/guilt/xet-server/internal/hfclient"
	"github.com/guilt/xet-server/internal/merklehash"
	"github.com/guilt/xet-server/internal/reconwire"
	"github.com/guilt/xet-server/internal/storage/fsstore"
	"github.com/guilt/xet-server/internal/xorbformat"
)

func hashFromByte(b byte) merklehash.Hash {
	var buf [32]byte
	for i := range buf {
		buf[i] = b
	}
	h, _ := merklehash.FromRawBytes(buf[:])
	return h
}

// buildFooterlessXorb serializes payloads as consecutive uncompressed
// chunks with no trailing footer - the real upload/download wire format,
// and the shape this package's fetch-and-ingest path must be able to
// hand to casserver.IngestXorb.
func buildFooterlessXorb(t *testing.T, payloads [][]byte) (blob []byte, xorbHash merklehash.Hash) {
	t.Helper()
	var buf bytes.Buffer
	var chunkEntries []merklehash.ChunkEntry
	for _, p := range payloads {
		if err := xorbformat.WriteChunkHeader(&buf, xorbformat.ChunkHeader{
			CompressedLength:   uint32(len(p)),
			CompressionScheme:  xorbformat.CompressionNone,
			UncompressedLength: uint32(len(p)),
		}); err != nil {
			t.Fatalf("WriteChunkHeader() error = %v", err)
		}
		buf.Write(p)
		h := merklehash.ComputeDataHash(p)
		chunkEntries = append(chunkEntries, merklehash.ChunkEntry{Hash: h, Size: uint64(len(p))})
	}
	return buf.Bytes(), merklehash.XorbHash(chunkEntries)
}

func newTestServer(t *testing.T, casTS *httptest.Server) (*httptest.Server, *Server) {
	t.Helper()
	store, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("fsstore.New() error = %v", err)
	}
	proxy := New(store)
	casClient := hfclient.NewCASClient(casTS.URL)

	mux := http.NewServeMux()
	mux.Handle("/", withCASClientMiddleware(proxy, casClient))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, proxy
}

func withCASClientMiddleware(next http.Handler, casClient *hfclient.CASClient) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := WithCASClient(r.Context(), casClient)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// fakeUpstreamCAS serves xorbs from an in-memory map, counting fetches.
type fakeUpstreamCAS struct {
	xorbs      map[string][]byte
	fetchCount map[string]int
}

func newFakeUpstreamCAS() *fakeUpstreamCAS {
	return &fakeUpstreamCAS{xorbs: map[string][]byte{}, fetchCount: map[string]int{}}
}

func (f *fakeUpstreamCAS) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hash := matchXorbPath(r.URL.Path)
		if hash == "" {
			http.NotFound(w, r)
			return
		}
		data, ok := f.xorbs[hash]
		if !ok {
			http.NotFound(w, r)
			return
		}
		f.fetchCount[hash]++
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		w.Write(data)
	}
}

func matchXorbPath(path string) string {
	const prefix = "/v1/xorbs/default/"
	if len(path) > len(prefix) && path[:len(prefix)] == prefix {
		return path[len(prefix):]
	}
	return ""
}

func TestFetchXorb_MissFetchesFromUpstreamAndIngests(t *testing.T) {
	blob, xorbHash := buildFooterlessXorb(t, [][]byte{[]byte("chunk payload")})
	upstream := newFakeUpstreamCAS()
	upstream.xorbs[xorbHash.Hex()] = blob
	casTS := httptest.NewServer(upstream.handler())
	defer casTS.Close()

	ts, proxy := newTestServer(t, casTS)

	resp, err := http.Get(ts.URL + "/v1/xorbs/default/" + xorbHash.Hex())
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if upstream.fetchCount[xorbHash.Hex()] != 1 {
		t.Errorf("upstream fetched %d times, want 1", upstream.fetchCount[xorbHash.Hex()])
	}
	if !proxy.Embedded.HasXorbFooter(xorbHash) {
		t.Error("embedded server does not have the xorb footer after a fetch-and-ingest")
	}
}

func TestFetchXorb_SecondRequestIsLocalHitNoUpstreamCall(t *testing.T) {
	blob, xorbHash := buildFooterlessXorb(t, [][]byte{[]byte("chunk payload")})
	upstream := newFakeUpstreamCAS()
	upstream.xorbs[xorbHash.Hex()] = blob
	casTS := httptest.NewServer(upstream.handler())
	defer casTS.Close()

	ts, _ := newTestServer(t, casTS)

	for i := 0; i < 2; i++ {
		resp, err := http.Get(ts.URL + "/v1/xorbs/default/" + xorbHash.Hex())
		if err != nil {
			t.Fatalf("GET #%d error = %v", i, err)
		}
		resp.Body.Close()
	}
	if upstream.fetchCount[xorbHash.Hex()] != 1 {
		t.Errorf("upstream fetched %d times across 2 requests, want 1 (second must be a local hit)", upstream.fetchCount[xorbHash.Hex()])
	}
}

func TestFetchXorb_OfflineAfterCacheStillServes(t *testing.T) {
	// The property this whole package exists for: once cached (via the
	// embedded server), a xorb keeps being servable even after upstream
	// becomes completely unreachable.
	blob, xorbHash := buildFooterlessXorb(t, [][]byte{[]byte("chunk payload")})
	upstream := newFakeUpstreamCAS()
	upstream.xorbs[xorbHash.Hex()] = blob
	casTS := httptest.NewServer(upstream.handler())

	ts, _ := newTestServer(t, casTS)

	primeResp, err := http.Get(ts.URL + "/v1/xorbs/default/" + xorbHash.Hex())
	if err != nil {
		t.Fatalf("priming GET error = %v", err)
	}
	primeResp.Body.Close()

	casTS.Close()

	resp, err := http.Get(ts.URL + "/v1/xorbs/default/" + xorbHash.Hex())
	if err != nil {
		t.Fatalf("GET after upstream went offline error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (must serve from the embedded server with upstream offline)", resp.StatusCode)
	}
}

func TestFetchXorb_NoCacheNeverIngests(t *testing.T) {
	blob, xorbHash := buildFooterlessXorb(t, [][]byte{[]byte("chunk payload")})
	upstream := newFakeUpstreamCAS()
	upstream.xorbs[xorbHash.Hex()] = blob
	casTS := httptest.NewServer(upstream.handler())
	defer casTS.Close()

	ts, proxy := newTestServer(t, casTS)
	proxy.NoCache = true

	for i := 0; i < 2; i++ {
		resp, err := http.Get(ts.URL + "/v1/xorbs/default/" + xorbHash.Hex())
		if err != nil {
			t.Fatalf("GET #%d error = %v", i, err)
		}
		resp.Body.Close()
	}
	if upstream.fetchCount[xorbHash.Hex()] != 2 {
		t.Errorf("upstream fetched %d times across 2 requests under -no-cache, want 2", upstream.fetchCount[xorbHash.Hex()])
	}
	if proxy.Embedded.HasXorbFooter(xorbHash) {
		t.Error("embedded server was populated despite -no-cache")
	}
}

func TestFetchXorb_UnknownHashReturns404(t *testing.T) {
	upstream := newFakeUpstreamCAS()
	casTS := httptest.NewServer(upstream.handler())
	defer casTS.Close()

	ts, _ := newTestServer(t, casTS)
	unknown := hashFromByte(0xFF)
	resp, err := http.Get(ts.URL + "/v1/xorbs/default/" + unknown.Hex())
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestFetchXorb_UpstreamForbiddenRelaysRealStatusNot502(t *testing.T) {
	// Regression test: a 401/403 from the real upstream (the caller's own
	// credential rejected) must relay as-is, matching
	// proxyhub.writeUpstreamError's identical policy for the same class
	// of failure - collapsing it to a blanket 502 would tell the caller
	// "the proxy is broken" when the real problem is "your token is bad."
	casTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer casTS.Close()

	ts, _ := newTestServer(t, casTS)
	unknown := hashFromByte(0xEE)
	resp, err := http.Get(ts.URL + "/v1/xorbs/default/" + unknown.Hex())
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 relayed as-is, not collapsed to 502", resp.StatusCode)
	}
}

func TestHeadXorb_MissFetchesAndReportsSize(t *testing.T) {
	blob, xorbHash := buildFooterlessXorb(t, [][]byte{[]byte("twelve bytes")})
	upstream := newFakeUpstreamCAS()
	upstream.xorbs[xorbHash.Hex()] = blob
	casTS := httptest.NewServer(upstream.handler())
	defer casTS.Close()

	ts, _ := newTestServer(t, casTS)
	resp, err := http.Head(ts.URL + "/v1/xorbs/default/" + xorbHash.Hex())
	if err != nil {
		t.Fatalf("HEAD error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Length"); got != fmt.Sprintf("%d", len(blob)) {
		t.Errorf("Content-Length = %q, want %q", got, fmt.Sprintf("%d", len(blob)))
	}
}

func TestHeadXorb_UnknownHashReturns404(t *testing.T) {
	upstream := newFakeUpstreamCAS()
	casTS := httptest.NewServer(upstream.handler())
	defer casTS.Close()

	ts, _ := newTestServer(t, casTS)
	unknown := hashFromByte(0xFF)
	resp, err := http.Head(ts.URL + "/v1/xorbs/default/" + unknown.Hex())
	if err != nil {
		t.Fatalf("HEAD error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHeadXorb_WrongPrefixReturns400(t *testing.T) {
	// Regression test: handleHeadXorb must validate the prefix path
	// segment same as handleFetchXorb does - a mismatched prefix must
	// never even reach ensureXorbCached (which hardcodes xorbPrefix
	// itself), let alone trigger a real upstream fetch+ingest.
	upstream := newFakeUpstreamCAS()
	casTS := httptest.NewServer(upstream.handler())
	defer casTS.Close()

	ts, _ := newTestServer(t, casTS)
	resp, err := http.Head(ts.URL + "/v1/xorbs/bogus-prefix/" + hashFromByte(0x22).Hex())
	if err != nil {
		t.Fatalf("HEAD error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHeadXorb_NoCacheRelaysLiveHeadNeverIngests(t *testing.T) {
	// Regression test: handleHeadXorb must honor -no-cache exactly like
	// handleFetchXorb does - never writing fetched bytes into the
	// embedded server's local storage, and never issuing a full GET to
	// upstream just to report a size (a real upstream HEAD instead).
	blob, xorbHash := buildFooterlessXorb(t, [][]byte{[]byte("twelve bytes")})
	getCount, headCount := 0, 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			headCount++
		} else {
			getCount++
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(blob)))
		w.Write(blob)
	}))
	defer upstream.Close()

	ts, proxy := newTestServer(t, upstream)
	proxy.NoCache = true

	resp, err := http.Head(ts.URL + "/v1/xorbs/default/" + xorbHash.Hex())
	if err != nil {
		t.Fatalf("HEAD error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if headCount != 1 {
		t.Errorf("upstream received %d HEAD requests, want 1 (a real HEAD relay, not a GET-and-discard)", headCount)
	}
	if getCount != 0 {
		t.Errorf("upstream received %d GET requests, want 0 under -no-cache HEAD", getCount)
	}
	if proxy.Embedded.HasXorbFooter(xorbHash) {
		t.Error("embedded server was populated by a HEAD request despite -no-cache")
	}
}

func TestChunkDedup_AlwaysRelaysLive(t *testing.T) {
	fetches := 0
	casTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chunks/default-merkledb/somehash" {
			http.NotFound(w, r)
			return
		}
		fetches++
		w.Write([]byte("shard bytes"))
	}))
	defer casTS.Close()

	ts, _ := newTestServer(t, casTS)
	for i := 0; i < 2; i++ {
		resp, err := http.Get(ts.URL + "/v1/chunks/default-merkledb/somehash")
		if err != nil {
			t.Fatalf("GET #%d error = %v", i, err)
		}
		resp.Body.Close()
	}
	if fetches != 2 {
		t.Errorf("upstream fetched %d times across 2 requests, want 2 (chunk-dedup is never cached)", fetches)
	}
}

func TestReconstructionV2_MissFetchesAndServesLocally(t *testing.T) {
	blob, xorbHash := buildFooterlessXorb(t, [][]byte{[]byte("xorb bytes")})
	fileHash := hashFromByte(0xF2)

	// ensureFileReconCached always fetches upstream's V1-shaped
	// reconstruction internally regardless of which version the
	// downstream caller requested (see its own doc comment) - so the
	// fake upstream only needs a V1 reconstruction path, even though
	// this test drives a V2 request through the proxy.
	upstream := newRecoUpstream(fileHash.Hex())
	upstream.xorbs[xorbHash.Hex()] = blob
	upstream.resp = &reconwire.ResponseV1{
		Terms: []reconwire.Term{
			{Hash: xorbHash.Hex(), Range: reconwire.IndexRange{Start: 0, End: 1}, UnpackedLength: uint32(len(blob))},
		},
	}
	casTS := httptest.NewServer(upstream.handler())
	defer casTS.Close()

	ts, _ := newTestServer(t, casTS)
	resp, err := http.Get(ts.URL + "/v2/reconstructions/" + fileHash.Hex())
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got reconwire.ResponseV2
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode error = %v", err)
	}
	if len(got.Xorbs[xorbHash.Hex()]) != 1 {
		t.Errorf("got %d xorbs entries, want 1", len(got.Xorbs[xorbHash.Hex()]))
	}
}

// recoUpstream is a fake upstream that serves both xorb bytes and a
// fixed whole-file reconstruction response, tracking whether any
// reconstruction request carried a Range header.
type recoUpstream struct {
	*fakeUpstreamCAS
	reconPath      string
	resp           *reconwire.ResponseV1
	reconCount     int
	sawRangeHeader bool
}

func newRecoUpstream(fileIDHex string) *recoUpstream {
	return &recoUpstream{fakeUpstreamCAS: newFakeUpstreamCAS(), reconPath: "/v1/reconstructions/" + fileIDHex}
}

func (u *recoUpstream) handler() http.HandlerFunc {
	xorbHandler := u.fakeUpstreamCAS.handler()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == u.reconPath {
			u.reconCount++
			if r.Header.Get("Range") != "" {
				u.sawRangeHeader = true
			}
			json.NewEncoder(w).Encode(u.resp)
			return
		}
		xorbHandler(w, r)
	}
}

func TestReconstruction_MissFetchesWholeFileAndServesLocally(t *testing.T) {
	blob, xorbHash := buildFooterlessXorb(t, [][]byte{[]byte("xorb bytes")})
	fileHash := hashFromByte(0xF1)

	upstream := newRecoUpstream(fileHash.Hex())
	upstream.xorbs[xorbHash.Hex()] = blob
	upstream.resp = &reconwire.ResponseV1{
		Terms: []reconwire.Term{
			{Hash: xorbHash.Hex(), Range: reconwire.IndexRange{Start: 0, End: 1}, UnpackedLength: uint32(len(blob))},
		},
	}
	casTS := httptest.NewServer(upstream.handler())
	defer casTS.Close()

	ts, proxy := newTestServer(t, casTS)

	resp, err := http.Get(ts.URL + "/v1/reconstructions/" + fileHash.Hex())
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if upstream.sawRangeHeader {
		t.Error("upstream saw a Range header - reconstruction fetch must always be whole-file")
	}
	if !proxy.Embedded.HasFileRecon(fileHash) {
		t.Error("embedded server does not have the file's reconstruction after a fetch-and-ingest")
	}

	var got reconwire.ResponseV1
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode error = %v", err)
	}
	entries := got.FetchInfo[xorbHash.Hex()]
	if len(entries) != 1 {
		t.Fatalf("got %d fetch_info entries, want 1", len(entries))
	}
	// The embedded server's own reconstruction handler must have emitted
	// a URL pointing at itself (this proxy), not the real upstream -
	// this is the "for free" rewriting the package doc comment
	// describes: casserver's own xorbFetchURL never knows about a
	// presigned upstream URL at all.
	wantSuffix := "/v1/xorbs/default/" + xorbHash.Hex()
	if len(entries[0].URL) < len(wantSuffix) || entries[0].URL[len(entries[0].URL)-len(wantSuffix):] != wantSuffix {
		t.Errorf("URL = %q, want it to end with %q", entries[0].URL, wantSuffix)
	}
}

func TestReconstruction_SecondRequestServedLocallyNoUpstreamCall(t *testing.T) {
	blob, xorbHash := buildFooterlessXorb(t, [][]byte{[]byte("xorb bytes")})
	fileHash := hashFromByte(0xF1)

	upstream := newRecoUpstream(fileHash.Hex())
	upstream.xorbs[xorbHash.Hex()] = blob
	upstream.resp = &reconwire.ResponseV1{
		Terms: []reconwire.Term{
			{Hash: xorbHash.Hex(), Range: reconwire.IndexRange{Start: 0, End: 1}, UnpackedLength: uint32(len(blob))},
		},
	}
	casTS := httptest.NewServer(upstream.handler())
	defer casTS.Close()

	ts, _ := newTestServer(t, casTS)

	for i := 0; i < 3; i++ {
		resp, err := http.Get(ts.URL + "/v1/reconstructions/" + fileHash.Hex())
		if err != nil {
			t.Fatalf("GET #%d error = %v", i, err)
		}
		resp.Body.Close()
	}
	if upstream.reconCount != 1 {
		t.Errorf("upstream reconstruction fetched %d times across 3 requests, want 1", upstream.reconCount)
	}
}

func TestReconstruction_OfflineAfterCacheStillServes(t *testing.T) {
	blob, xorbHash := buildFooterlessXorb(t, [][]byte{[]byte("xorb bytes")})
	fileHash := hashFromByte(0xF1)

	upstream := newRecoUpstream(fileHash.Hex())
	upstream.xorbs[xorbHash.Hex()] = blob
	upstream.resp = &reconwire.ResponseV1{
		Terms: []reconwire.Term{
			{Hash: xorbHash.Hex(), Range: reconwire.IndexRange{Start: 0, End: 1}, UnpackedLength: uint32(len(blob))},
		},
	}
	casTS := httptest.NewServer(upstream.handler())

	ts, _ := newTestServer(t, casTS)

	primeResp, err := http.Get(ts.URL + "/v1/reconstructions/" + fileHash.Hex())
	if err != nil {
		t.Fatalf("priming GET error = %v", err)
	}
	primeResp.Body.Close()

	casTS.Close()

	resp, err := http.Get(ts.URL + "/v1/reconstructions/" + fileHash.Hex())
	if err != nil {
		t.Fatalf("GET after upstream went offline error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (reconstruction must be servable with upstream offline)", resp.StatusCode)
	}
}

func TestReconstruction_UnknownFileReturns404(t *testing.T) {
	casTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer casTS.Close()

	ts, _ := newTestServer(t, casTS)
	unknown := hashFromByte(0xFF)
	resp, err := http.Get(ts.URL + "/v1/reconstructions/" + unknown.Hex())
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestReconstruction_NoCacheRelaysCallersRangeAndOriginalURLUnchanged(t *testing.T) {
	fileHash := hashFromByte(0xF1)
	xorbHash := hashFromByte(0x11)

	upstream := newRecoUpstream(fileHash.Hex())
	upstream.resp = &reconwire.ResponseV1{
		FetchInfo: map[string][]reconwire.FetchInfoEntry{
			xorbHash.Hex(): {{URL: "https://s3.example.invalid/presigned-xorb1"}},
		},
	}
	casTS := httptest.NewServer(upstream.handler())
	defer casTS.Close()

	ts, proxy := newTestServer(t, casTS)
	proxy.NoCache = true

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/reconstructions/"+fileHash.Hex(), nil)
	req.Header.Set("Range", "bytes=0-99")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()

	if !upstream.sawRangeHeader {
		t.Error("upstream did not see the caller's Range header under -no-cache")
	}
	var got reconwire.ResponseV1
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode error = %v", err)
	}
	if got.FetchInfo[xorbHash.Hex()][0].URL != "https://s3.example.invalid/presigned-xorb1" {
		t.Errorf("URL = %q, want the original presigned URL unchanged under -no-cache", got.FetchInfo[xorbHash.Hex()][0].URL)
	}
}

func TestUploadXorb_RelaysToUpstreamAndIngestsOnSuccess(t *testing.T) {
	blob, xorbHash := buildFooterlessXorb(t, [][]byte{[]byte("uploaded chunk")})
	var receivedBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		buf.ReadFrom(r.Body)
		receivedBody = buf.Bytes()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	ts, proxy := newTestServer(t, upstream)

	resp, err := http.Post(ts.URL+"/v1/xorbs/default/"+xorbHash.Hex(), "application/octet-stream", bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !bytes.Equal(receivedBody, blob) {
		t.Error("upstream did not receive the exact xorb bytes sent")
	}
	if !proxy.Embedded.HasXorbFooter(xorbHash) {
		t.Error("embedded server was not populated after a successful upload")
	}
}

func TestUploadShard_RelaysAndIngests(t *testing.T) {
	shardBytes := []byte("not a real shard, just checking relay")
	var receivedBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		buf.ReadFrom(r.Body)
		receivedBody = buf.Bytes()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	ts, _ := newTestServer(t, upstream)

	resp, err := http.Post(ts.URL+"/v1/shards", "application/octet-stream", bytes.NewReader(shardBytes))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !bytes.Equal(receivedBody, shardBytes) {
		t.Error("upstream did not receive the exact shard bytes sent")
	}
	// A malformed shard's ingest failure is swallowed (non-fatal, per
	// the package's design) - the relay itself succeeding is what
	// matters here.
}

func TestAuth_DelegatesToEmbeddedServer(t *testing.T) {
	// This package installs no auth layer of its own - SetAuthenticator
	// must actually reach the embedded server, which is what enforces
	// every scope check.
	upstream := newFakeUpstreamCAS()
	casTS := httptest.NewServer(upstream.handler())
	defer casTS.Close()

	ts, proxy := newTestServer(t, casTS)
	proxy.SetAuthenticator(auth.NewStaticTokenAuth("test-fixture-token-not-a-real-secret"))

	resp, err := http.Get(ts.URL + "/v1/xorbs/default/" + hashFromByte(0x11).Hex())
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (embedded server's auth must be enforced)", resp.StatusCode)
	}
}
