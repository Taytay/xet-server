package hubserver

// Tests for handleResolve's XetHash-known fast path (see resolve.go):
// when fileRef.XetHash is already known - e.g. via IngestFile, for a
// file learned from an upstream response rather than a real shard
// upload - resolve must serve using CAS.FileSize(xetHash) directly,
// with no CAS.XetHashForSHA256 bridge entry required at all. The size
// returned must always come from the CAS layer's own verified data
// (FileSize), never fileRef's own client-declared Size - a miss there
// stays a clean 404, never a fabricated fallback value.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/guilt/xet-server/internal/merklehash"
)

func TestResolve_KnownXetHashServesViaFileSizeWithNoBridgeEntry(t *testing.T) {
	xetHash := merklehash.ComputeDataHash([]byte("real file content"))
	cas := newFakeCAS()
	cas.sizes[xetHash] = 42 // no cas.sha256ToXet entry at all - the bridge is never consulted

	hubSrv := New("http://localhost:9999", cas)
	hubSrv.IngestFile("model", "alice/my-model", "main", "model.bin", "deadbeef", 999, xetHash)

	ts := httptest.NewServer(hubSrv)
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
	if got := resp.Header.Get("X-Xet-Hash"); got != xetHash.Hex() {
		t.Errorf("X-Xet-Hash = %q, want %q", got, xetHash.Hex())
	}
	// 42 (from CAS.FileSize, the verified value), not 999 (the
	// unverified Size IngestFile was called with) - proves the size
	// returned is never taken from fileRef's own client-declared field.
	if got := resp.Header.Get("X-Linked-Size"); got != "42" {
		t.Errorf("X-Linked-Size = %q, want %q (the CAS-verified size, not the declared 999)", got, "42")
	}
}

func TestResolve_KnownXetHashButFileSizeMissingReturns404(t *testing.T) {
	// The "shard missing" case: a file was ingested with a XetHash, but
	// the CAS layer has no reconstruction for it (e.g. never actually
	// fetched/verified) - must 404 cleanly, not fabricate a size from
	// fileRef.Size.
	xetHash := merklehash.ComputeDataHash([]byte("never actually verified"))
	cas := newFakeCAS() // no cas.sizes entry for xetHash at all

	hubSrv := New("http://localhost:9999", cas)
	hubSrv.IngestFile("model", "alice/my-model", "main", "model.bin", "deadbeef", 999, xetHash)

	ts := httptest.NewServer(hubSrv)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodHead, ts.URL+"/alice/my-model/resolve/main/model.bin", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (FileSize could not verify this file; must not fabricate a size)", resp.StatusCode)
	}
}

func TestResolve_ZeroXetHashFallsBackToCASBridge(t *testing.T) {
	// A file ingested with no XetHash yet (the zero value - e.g. a tree
	// listing entry with no xetHash field) must still fall back to the
	// existing CAS.XetHashForSHA256 bridge lookup, matching this
	// server's pre-existing behavior exactly.
	xetHash := merklehash.ComputeDataHash([]byte("bridged content"))
	cas := newFakeCAS()
	cas.sha256ToXet["deadbeef"] = xetHash
	cas.sizes[xetHash] = 7

	hubSrv := New("http://localhost:9999", cas)
	hubSrv.IngestFile("model", "alice/my-model", "main", "model.bin", "deadbeef", 999, merklehash.Hash{})

	ts := httptest.NewServer(hubSrv)
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
	if got := resp.Header.Get("X-Xet-Hash"); got != xetHash.Hex() {
		t.Errorf("X-Xet-Hash = %q, want %q (via the CAS bridge)", got, xetHash.Hex())
	}
}
