package proxyhub

// Tests for callContext: does MetadataCallTimeout actually bound a
// slow-drip upstream response (one that starts responding within
// hfclient's own Transport-level ResponseHeaderTimeout but then
// trickles bytes slowly), triggering the stale-fallback path sooner
// rather than blocking for as long as the downstream client is willing
// to wait? This class of failure was never exercised by this package's
// other tests (all driven against an httptest.Server that responds
// instantly) — see internal/hfclient/timeout_test.go for the
// equivalent coverage at the Transport-header-timeout layer.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"xet-server/internal/hfclient"
)

// slowDripHandler writes the response headers immediately (so
// ResponseHeaderTimeout never fires) but then sleeps before writing any
// body — simulating a connected-but-stalled upstream, distinct from
// the fully-hung-before-headers case internal/hfclient/timeout_test.go
// already covers.
func slowDripHandler(delay time.Duration, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(delay)
		w.Write(body)
	}
}

func TestRepoInfo_SlowDripUpstreamFallsBackToCacheSoonerThanItWouldHang(t *testing.T) {
	fastResp, _ := json.Marshal(hfclient.RepoInfo{ID: "alice/my-model", SHA: "main"})
	slowHubTS := httptest.NewServer(slowDripHandler(1*time.Second, fastResp))
	defer slowHubTS.Close()

	s := newTestServer(slowHubTS, "http://localhost:8420")
	s.MetadataCallTimeout = 100 * time.Millisecond // short bound for a fast test

	// Simulates the real offline-fallback scenario directly: Embedded
	// already has this revision (as if a previous, successful fetch had
	// ingested it), and upstream has since become slow/unhealthy.
	s.Embedded.IngestRepoInfo("model", "alice/my-model", "main")

	ts := httptest.NewServer(s)
	defer ts.Close()

	start := time.Now()
	resp, err := http.Get(ts.URL + "/api/models/alice/my-model/revision/main")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (must fall back to the already-ingested revision)", resp.StatusCode)
	}
	if elapsed > 2*time.Second {
		t.Errorf("request took %v, want well under 2s (MetadataCallTimeout must cut the slow-drip upstream call short)", elapsed)
	}
}

func TestCallContext_ZeroTimeoutUsesRequestContextUnmodified(t *testing.T) {
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(hfclient.RepoInfo{ID: "alice/my-model", SHA: "main"})
	}))
	defer hubTS.Close()

	s := newTestServer(hubTS, "http://localhost:8420")
	s.MetadataCallTimeout = 0

	req := httptest.NewRequest(http.MethodGet, "/api/models/alice/my-model/revision/main", nil)
	ctx, cancel := s.callContext(req)
	defer cancel()
	if ctx != req.Context() {
		t.Error("callContext() with MetadataCallTimeout=0 did not return the request's own context unmodified")
	}
}
