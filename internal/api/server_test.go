package api

import (
	"bytes"
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"xet-server/internal/manifest"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

func randomBytes(n int, seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, n)
	r.Read(b)
	return b
}

func upload(t *testing.T, ts *httptest.Server, data []byte) UploadResult {
	t.Helper()
	resp, err := http.Post(ts.URL+"/upload", "application/octet-stream", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("upload request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload status = %d, body = %s", resp.StatusCode, body)
	}
	var res UploadResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decode upload response error = %v", err)
	}
	return res
}

func TestUploadAndDownload_RoundTrip(t *testing.T) {
	ts := newTestServer(t)
	data := randomBytes(1_200_000, 1)

	res := upload(t, ts, data)
	if res.Size != int64(len(data)) {
		t.Fatalf("upload Size = %d, want %d", res.Size, len(data))
	}
	if res.ChunksTotal == 0 {
		t.Fatal("upload reported 0 chunks")
	}
	if res.ChunksNew != res.ChunksTotal {
		t.Fatalf("first upload ChunksNew = %d, want all %d chunks new", res.ChunksNew, res.ChunksTotal)
	}

	resp, err := http.Get(ts.URL + "/files/" + res.FileID)
	if err != nil {
		t.Fatalf("download request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d", resp.StatusCode)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read download body error = %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("downloaded content does not match uploaded content: got %d bytes, want %d bytes", len(got), len(data))
	}
}

func TestUpload_DedupsAgainstSharedChunks(t *testing.T) {
	ts := newTestServer(t)
	original := randomBytes(1_000_000, 2)

	res1 := upload(t, ts, original)
	if res1.ChunksNew != res1.ChunksTotal {
		t.Fatalf("first upload should have all-new chunks, got %d/%d new", res1.ChunksNew, res1.ChunksTotal)
	}

	// Second upload: same content plus a small appended tail. Only the
	// chunk(s) touching the new tail should be new.
	edited := append(append([]byte{}, original...), randomBytes(50_000, 3)...)
	res2 := upload(t, ts, edited)

	if res2.ChunksNew >= res2.ChunksTotal {
		t.Fatalf("second upload ChunksNew = %d out of %d total, want dedup against first upload's chunks", res2.ChunksNew, res2.ChunksTotal)
	}
	if res2.DedupPercent <= 0 {
		t.Fatalf("second upload DedupPercent = %f, want > 0", res2.DedupPercent)
	}
}

func TestUpload_IdenticalContentReusesFileID(t *testing.T) {
	ts := newTestServer(t)
	data := randomBytes(300_000, 4)

	res1 := upload(t, ts, data)
	res2 := upload(t, ts, data)

	if res1.FileID != res2.FileID {
		t.Fatalf("uploading identical content twice produced different file IDs: %q vs %q", res1.FileID, res2.FileID)
	}
	if res2.ChunksNew != 0 {
		t.Fatalf("re-uploading identical content should dedup all chunks, got %d new", res2.ChunksNew)
	}
}

func TestUpload_EmptyFile(t *testing.T) {
	ts := newTestServer(t)
	res := upload(t, ts, []byte{})

	if res.Size != 0 {
		t.Fatalf("empty upload Size = %d, want 0", res.Size)
	}
	if res.ChunksTotal != 0 {
		t.Fatalf("empty upload ChunksTotal = %d, want 0", res.ChunksTotal)
	}

	resp, err := http.Get(ts.URL + "/files/" + res.FileID)
	if err != nil {
		t.Fatalf("download request error = %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if len(got) != 0 {
		t.Fatalf("downloaded empty file has %d bytes, want 0", len(got))
	}
}

func TestDownload_UnknownFileID(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/files/does-not-exist")
	if err != nil {
		t.Fatalf("request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestDownload_RejectsPathTraversal(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/files/..%2F..%2Fetc%2Fpasswd")
	if err != nil {
		t.Fatalf("request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("status = 200, want a non-200 status for a path-traversal file id")
	}
}

func TestManifestEndpoint(t *testing.T) {
	ts := newTestServer(t)
	data := randomBytes(400_000, 5)
	res := upload(t, ts, data)

	resp, err := http.Get(ts.URL + "/files/" + res.FileID + "/manifest")
	if err != nil {
		t.Fatalf("request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var m manifest.Manifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode manifest error = %v", err)
	}
	if m.FileID != res.FileID {
		t.Errorf("manifest FileID = %q, want %q", m.FileID, res.FileID)
	}
	if m.Size != res.Size {
		t.Errorf("manifest Size = %d, want %d", m.Size, res.Size)
	}
	if len(m.Chunks) != res.ChunksTotal {
		t.Errorf("manifest has %d chunks, want %d", len(m.Chunks), res.ChunksTotal)
	}
}

func TestManifestEndpoint_UnknownFileID(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/files/does-not-exist/manifest")
	if err != nil {
		t.Fatalf("request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestStatsEndpoint(t *testing.T) {
	ts := newTestServer(t)

	statsBefore := getStats(t, ts)
	if statsBefore["files_stored"].(float64) != 0 {
		t.Fatalf("files_stored before any upload = %v, want 0", statsBefore["files_stored"])
	}

	data := randomBytes(600_000, 6)
	res := upload(t, ts, data)

	statsAfter := getStats(t, ts)
	if int(statsAfter["files_stored"].(float64)) != 1 {
		t.Fatalf("files_stored after 1 upload = %v, want 1", statsAfter["files_stored"])
	}
	if int(statsAfter["unique_chunks"].(float64)) != res.ChunksTotal {
		t.Fatalf("unique_chunks = %v, want %d", statsAfter["unique_chunks"], res.ChunksTotal)
	}
}

func getStats(t *testing.T, ts *httptest.Server) map[string]any {
	t.Helper()
	resp, err := http.Get(ts.URL + "/stats")
	if err != nil {
		t.Fatalf("stats request error = %v", err)
	}
	defer resp.Body.Close()
	var stats map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatalf("decode stats error = %v", err)
	}
	return stats
}

func TestNew_CreatesDataLayout(t *testing.T) {
	root := t.TempDir()
	if _, err := New(root); err != nil {
		t.Fatalf("New() error = %v", err)
	}
	info, err := os.Stat(filepath.Join(root, "manifests"))
	if err != nil {
		t.Fatalf("stat manifests dir error = %v", err)
	}
	if !info.IsDir() {
		t.Fatal("manifests path exists but is not a directory")
	}
}
