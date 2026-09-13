package hubserver

// Tests for Server's snapshot/restore persistence: a fresh Server
// restored from a snapshot must serve requests identically to the
// original, across multiple repos and revisions.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guilt/xet-server/internal/merklehash"
)

func TestSnapshot_RestoresMultiRepoMultiRevisionState(t *testing.T) {
	xetHashMain := merklehash.ComputeDataHash([]byte("main content"))
	xetHashDev := merklehash.ComputeDataHash([]byte("dev content"))
	cas := newFakeCAS()
	cas.sha256ToXet["main-oid"] = xetHashMain
	cas.sha256ToXet["dev-oid"] = xetHashDev
	cas.sizes[xetHashMain] = 100
	cas.sizes[xetHashDev] = 200

	srv := New("http://localhost:9999", cas)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	mainNdjson := `{"key":"lfsFile","value":{"path":"model.bin","algo":"sha256","oid":"main-oid","size":100}}` + "\n"
	mainResp, err := http.Post(ts.URL+"/api/models/alice/my-model/commit/main", "application/x-ndjson", strings.NewReader(mainNdjson))
	if err != nil {
		t.Fatalf("main commit error = %v", err)
	}
	mainResp.Body.Close()

	devNdjson := `{"key":"lfsFile","value":{"path":"model.bin","algo":"sha256","oid":"dev-oid","size":200}}` + "\n"
	devResp, err := http.Post(ts.URL+"/api/models/alice/my-model/commit/dev", "application/x-ndjson", strings.NewReader(devNdjson))
	if err != nil {
		t.Fatalf("dev commit error = %v", err)
	}
	devResp.Body.Close()

	// A second, independent repo, to confirm the snapshot handles more
	// than one repoKey.
	secondNdjson := `{"key":"lfsFile","value":{"path":"other.bin","algo":"sha256","oid":"main-oid","size":100}}` + "\n"
	secondResp, err := http.Post(ts.URL+"/api/models/bob/other-model/commit/main", "application/x-ndjson", strings.NewReader(secondNdjson))
	if err != nil {
		t.Fatalf("second repo commit error = %v", err)
	}
	secondResp.Body.Close()

	path := filepath.Join(t.TempDir(), "snapshot.json")
	if err := srv.Snapshot(path); err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}

	restored := New("http://localhost:9999", cas)
	if err := restored.LoadSnapshot(path); err != nil {
		t.Fatalf("LoadSnapshot() error = %v", err)
	}
	restoredTS := httptest.NewServer(restored)
	defer restoredTS.Close()

	cases := []struct {
		name     string
		url      string
		wantHash merklehash.Hash
		wantSize string
	}{
		{"alice/my-model main", "/alice/my-model/resolve/main/model.bin", xetHashMain, "100"},
		{"alice/my-model dev", "/alice/my-model/resolve/dev/model.bin", xetHashDev, "200"},
		{"bob/other-model main", "/bob/other-model/resolve/main/other.bin", xetHashMain, "100"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodHead, restoredTS.URL+tc.url, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("HEAD error = %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if got := resp.Header.Get("X-Xet-Hash"); got != tc.wantHash.Hex() {
				t.Errorf("X-Xet-Hash = %q, want %q", got, tc.wantHash.Hex())
			}
			if got := resp.Header.Get("X-Linked-Size"); got != tc.wantSize {
				t.Errorf("X-Linked-Size = %q, want %q", got, tc.wantSize)
			}
		})
	}

	// A revision that was never committed to must still 404 on the
	// restored server, not spring into existence.
	req, _ := http.NewRequest(http.MethodHead, restoredTS.URL+"/alice/my-model/resolve/never-existed/model.bin", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a revision that was never committed to", resp.StatusCode)
	}
}

func TestSnapshot_MissingFileIsNotAnError(t *testing.T) {
	srv := New("http://localhost:9999", newFakeCAS())
	err := srv.LoadSnapshot(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Errorf("LoadSnapshot() error = %v, want nil for a missing snapshot file", err)
	}
}

func TestSnapshot_RejectsIncompatibleVersion(t *testing.T) {
	srv := New("http://localhost:9999", newFakeCAS())
	path := filepath.Join(t.TempDir(), "snapshot.json")
	if err := os.WriteFile(path, []byte(`{"version": 99999}`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := srv.LoadSnapshot(path); err == nil {
		t.Error("LoadSnapshot() error = nil, want an error for an incompatible snapshot version")
	}
}

func TestSnapshot_NoTempFileLeftBehindOnSuccess(t *testing.T) {
	srv := New("http://localhost:9999", newFakeCAS())
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")
	if err := srv.Snapshot(path); err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*.tmp-*"))
	if len(matches) != 0 {
		t.Errorf("found leftover temp files after successful Snapshot(): %v", matches)
	}
}

func TestSnapshot_EmptyServerRoundTrips(t *testing.T) {
	srv := New("http://localhost:9999", newFakeCAS())
	path := filepath.Join(t.TempDir(), "snapshot.json")
	if err := srv.Snapshot(path); err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}

	restored := New("http://localhost:9999", newFakeCAS())
	if err := restored.LoadSnapshot(path); err != nil {
		t.Fatalf("LoadSnapshot() on an empty snapshot error = %v", err)
	}
}
