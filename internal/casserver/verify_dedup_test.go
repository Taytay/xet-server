package casserver

// End-to-end regression test for storage.VerifyingStore wired through the
// real HTTP upload path (not just the storage-layer unit tests in
// internal/storage/verify_test.go): simulates undetected storage
// corruption (the practical threat -verify-dedup defends against, since a
// real BLAKE3 collision can't be manufactured in a test) and confirms the
// server refuses to silently accept a re-upload that no longer matches
// what's actually on disk, rather than either auto-healing (masking that
// corruption happened at all) or accepting it (compounding the problem).

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/guilt/xet-server/internal/storage"
	"github.com/guilt/xet-server/internal/storage/fsstore"
)

// newVerifyingTestServer is like newTestServer but wraps its fsstore.Store
// with storage.NewVerifyingStore, and returns the fsstore root directory
// so a test can reach in and corrupt a stored blob's bytes directly on
// disk to simulate undetected storage-layer corruption.
func newVerifyingTestServer(t *testing.T) (ts *httptest.Server, srv *Server, fsRoot string) {
	t.Helper()
	root := t.TempDir()
	fs, err := fsstore.New(root)
	if err != nil {
		t.Fatalf("fsstore.New() error = %v", err)
	}
	casSrv := New(storage.NewVerifyingStore(fs))
	httpSrv := httptest.NewServer(casSrv)
	t.Cleanup(httpSrv.Close)
	return httpSrv, casSrv, root
}

func TestVerifyDedup_UndetectedCorruptionIsRefusedNotHealedOrAccepted(t *testing.T) {
	ts, _, fsRoot := newVerifyingTestServer(t)
	client := ts.Client()

	original := bytes.Repeat([]byte("the correct, originally-uploaded content. "), 5000)
	blob, xorbHash, _ := buildXorb(t, [][]byte{original})

	resp, err := client.Post(ts.URL+"/v1/xorbs/default/"+xorbHash.Hex(), "application/octet-stream", bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("initial upload error = %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initial upload status = %d, body = %s", resp.StatusCode, body)
	}
	var uploadResp uploadXorbResponse
	if err := json.Unmarshal(body, &uploadResp); err != nil {
		t.Fatalf("decode upload response error = %v", err)
	}
	if !uploadResp.WasInserted {
		t.Fatal("initial upload WasInserted = false, want true")
	}

	// Simulate undetected storage-layer corruption: flip bytes directly on
	// disk, same length, so the file size and directory listing look
	// completely normal - this is the "quiet corruption" scenario, not a
	// crash or truncation something else would already catch.
	blobPath := findStoredBlobPath(t, fsRoot, xorbHash.Hex())
	corruptStoredBlob(t, blobPath)

	// Re-upload the ORIGINAL, correct bytes again (a client legitimately
	// re-uploading the same file, with no idea anything is wrong). The
	// server must detect that what's on disk no longer matches what the
	// hash claims to represent, and refuse the write rather than silently
	// treating this as a normal dedup hit.
	resp2, err := client.Post(ts.URL+"/v1/xorbs/default/"+xorbHash.Hex(), "application/octet-stream", bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("second upload error = %v", err)
	}
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode == http.StatusOK {
		t.Fatalf("second upload status = 200, want an error - corruption must be detected, not silently accepted as a dedup hit; body = %s", body2)
	}
	t.Logf("second upload correctly rejected: status=%d body=%s", resp2.StatusCode, body2)

	// The server must NOT have auto-healed the corruption either - a
	// mismatch on Put is refused without touching the existing stored
	// blob (see storage.VerifyingStore's doc comment), so a subsequent
	// fetch still returns the (still-corrupted) bytes. This is a
	// deliberate design choice: silently "fixing" a blob that was
	// supposedly content-addressed and immutable would hide evidence of
	// what actually went wrong, which of the two possible causes
	// (collision vs. corruption) it was, and whether other data might be
	// affected too.
	getResp, err := client.Get(ts.URL + "/v1/xorbs/default/" + xorbHash.Hex())
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer getResp.Body.Close()
	fetched, _ := io.ReadAll(getResp.Body)
	if bytes.Equal(fetched, original) {
		t.Error("fetched content matches the original after corruption+rejected-reupload - expected it to remain corrupted (refuse-not-heal), not silently repaired")
	}

	// The server must still be healthy for unrelated traffic.
	healthResp, err := client.Post(ts.URL+"/v1/telemetry", "application/octet-stream", nil)
	if err != nil {
		t.Fatalf("server unresponsive after corruption test: %v", err)
	}
	healthResp.Body.Close()
	if healthResp.StatusCode != http.StatusOK {
		t.Errorf("server responded %d to telemetry after corruption test, want 200", healthResp.StatusCode)
	}
}

// findStoredBlobPath locates the on-disk file fsstore wrote for hexKey,
// mirroring fsstore's own <root>/<key[:2]>/<key[2:4]>/<key> sharding
// scheme without importing fsstore's unexported path() method.
func findStoredBlobPath(t *testing.T, root, hexKey string) string {
	t.Helper()
	if len(hexKey) < 4 {
		return filepath.Join(root, hexKey)
	}
	p := filepath.Join(root, hexKey[:2], hexKey[2:4], hexKey)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("expected stored blob at %s, stat error = %v", p, err)
	}
	return p
}

// corruptStoredBlob flips a byte roughly in the middle of the file,
// preserving its length - the specific corruption shape that a naive
// truncation/size check would never catch, which is exactly why content
// verification (not just size checking) is the point of this feature.
func corruptStoredBlob(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read stored blob error = %v", err)
	}
	if len(data) == 0 {
		t.Fatal("stored blob is empty, can't corrupt a byte in it")
	}
	mid := len(data) / 2
	data[mid] ^= 0xFF
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write corrupted blob error = %v", err)
	}
}
