package hubserver

// Tests for the exported ingestion methods (IngestRepoInfo, IngestFile)
// and their read-accessor counterparts (HasRepo, HasRevision, HasFile) -
// the API surface a caller embedding this Server as a caching layer
// (internal/proxyhub) uses instead of rebuilding a repoState/
// revisionState model independently. Driven directly against the
// exported methods and Server's own read methods, not through HTTP -
// hubserver_test.go's existing handler-level tests already cover the
// HTTP boundary these methods sit behind.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/guilt/xet-server/internal/merklehash"
)

func newIngestTestServer(t *testing.T) *Server {
	t.Helper()
	return New("http://localhost:9999", newFakeCAS())
}

func TestIngestRepoInfo_CreatesRepoAndRevision(t *testing.T) {
	s := newIngestTestServer(t)
	if s.HasRepo("model", "alice/my-model") {
		t.Fatal("HasRepo() = true before any ingest")
	}

	s.IngestRepoInfo("model", "alice/my-model", "experiment-1")

	if !s.HasRepo("model", "alice/my-model") {
		t.Error("HasRepo() = false after IngestRepoInfo")
	}
	if !s.HasRevision("model", "alice/my-model", "experiment-1") {
		t.Error("HasRevision() = false for the ingested revision")
	}
	// The implicit default revision ("main") must still exist too - the
	// same guarantee getOrCreateRepo already provides for a real commit.
	if !s.HasRevision("model", "alice/my-model", "main") {
		t.Error("HasRevision() = false for the implicit default revision")
	}
}

func TestIngestFile_RecordsFileMetadata(t *testing.T) {
	s := newIngestTestServer(t)
	xetHash := hashFromByte(0xAB)

	if s.HasFile("model", "alice/my-model", "main", "model.bin") {
		t.Fatal("HasFile() = true before any ingest")
	}

	s.IngestFile("model", "alice/my-model", "main", "model.bin", "deadbeef", 1024, xetHash)

	if !s.HasFile("model", "alice/my-model", "main", "model.bin") {
		t.Fatal("HasFile() = false after IngestFile")
	}
}

func TestIngestFile_WithZeroXetHashStillRecordsFile(t *testing.T) {
	// A caller that only knows a file exists (e.g. from a tree listing
	// entry with no xetHash field yet) must still be able to record it -
	// mirrors a freshly-committed file before resolve.go's lazy CAS
	// backfill runs.
	s := newIngestTestServer(t)
	s.IngestFile("model", "alice/my-model", "main", "model.bin", "deadbeef", 1024, merklehash.Hash{})

	if !s.HasFile("model", "alice/my-model", "main", "model.bin") {
		t.Fatal("HasFile() = false after IngestFile with a zero XetHash")
	}
}

func TestHasRepo_UnknownRepoReturnsFalse(t *testing.T) {
	s := newIngestTestServer(t)
	if s.HasRepo("model", "nobody/nothing") {
		t.Error("HasRepo() = true for a repo never touched")
	}
}

func TestHasRevision_UnknownRevisionReturnsFalse(t *testing.T) {
	s := newIngestTestServer(t)
	s.IngestRepoInfo("model", "alice/my-model", "main")
	if s.HasRevision("model", "alice/my-model", "never-created") {
		t.Error("HasRevision() = true for a revision never created")
	}
}

func TestHasFile_UnknownFileReturnsFalse(t *testing.T) {
	s := newIngestTestServer(t)
	s.IngestRepoInfo("model", "alice/my-model", "main")
	if s.HasFile("model", "alice/my-model", "main", "never-committed.bin") {
		t.Error("HasFile() = true for a file never committed/ingested")
	}
}

func TestIngestCommit_RecordsRealUpstreamOID(t *testing.T) {
	xetHash := hashFromByte(0x11)
	cas := newFakeCAS()
	cas.sizes[xetHash] = 10

	s := New("http://localhost:9999", cas)
	s.IngestCommit("model", "alice/my-model", "main", "realupstreamcommitoid1234567890")
	s.IngestFile("model", "alice/my-model", "main", "model.bin", "deadbeef", 10, xetHash)

	ts := httptest.NewServer(s)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodHead, ts.URL+"/alice/my-model/resolve/main/model.bin", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD error = %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Repo-Commit"); got != "realupstreamcommitoid1234567890" {
		t.Errorf("X-Repo-Commit = %q, want the real upstream commit OID", got)
	}
}

func hashFromByte(b byte) merklehash.Hash {
	var buf [32]byte
	for i := range buf {
		buf[i] = b
	}
	h, _ := merklehash.FromRawBytes(buf[:])
	return h
}
