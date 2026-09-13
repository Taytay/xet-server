package proxyhub

// Tests for SetRateLimiter: does gate actually enforce it (burst
// allowed, then 429 with Retry-After) across the dispatch functions
// (handleAPIGet/handleAPIPost/handleResolveDispatch each call gate
// per-case rather than through a single wrapper - see gate's doc
// comment), and does a configured rate limiter leave the
// offline-fallback path (see TestRepoInfo_OfflineAfterCacheStillServes
// RegardlessOfTTL) working for requests still within budget?

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/guilt/xet-server/internal/hfclient"
	"github.com/guilt/xet-server/internal/ratelimit"
)

func TestRateLimit_BurstAllowedThenTooManyRequests(t *testing.T) {
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(hfclient.RepoInfo{ID: "alice/my-model", SHA: "main"})
	}))
	defer hubTS.Close()

	s := newTestServer(hubTS, "http://localhost:8420")
	s.SetRateLimiter(ratelimit.New(2, 0)) // burst=2, no refill
	ts := httptest.NewServer(s)
	defer ts.Close()

	url := ts.URL + "/api/models/alice/my-model/revision/main"
	for i := 0; i < 2; i++ {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatalf("GET #%d error = %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("GET #%d got 429 within burst allowance", i)
		}
	}

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET (over budget) error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 once burst is exhausted", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Error("Retry-After header missing on a 429 response")
	}
}

func TestRateLimit_NilLimiterNeverRejects(t *testing.T) {
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(hfclient.RepoInfo{ID: "alice/my-model", SHA: "main"})
	}))
	defer hubTS.Close()

	s := newTestServer(hubTS, "http://localhost:8420") // no SetRateLimiter call: nil is the default
	ts := httptest.NewServer(s)
	defer ts.Close()

	for i := 0; i < 5; i++ {
		resp, err := http.Get(ts.URL + "/api/models/alice/my-model/revision/main")
		if err != nil {
			t.Fatalf("GET #%d error = %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("GET #%d got 429 with no rate limiter configured", i)
		}
	}
}

func TestRateLimit_OfflineFallbackStillServedWithinBudget(t *testing.T) {
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(hfclient.RepoInfo{ID: "alice/my-model", SHA: "main"})
	}))

	s := newTestServer(hubTS, "http://localhost:8420")
	s.SetRateLimiter(ratelimit.New(5, 100))
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
		t.Fatalf("status = %d, want 200 (rate limiting must not block a within-budget fallback-to-cache request)", resp.StatusCode)
	}
}

func TestRateLimit_AppliesAcrossAllThreeDispatchFunctions(t *testing.T) {
	// gate is called inline per-case in handleAPIGet, handleAPIPost, and
	// handleResolveDispatch (no single shared wrapper) - verify the
	// limit is enforced via all three, not just handleAPIGet.
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/repos/create":
			json.NewEncoder(w).Encode(map[string]string{})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer hubTS.Close()

	s := newTestServer(hubTS, "http://localhost:8420")
	s.SetRateLimiter(ratelimit.New(1, 0)) // burst=1, no refill: the second request on ANY route must 429
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/repos/create", "application/json", nil)
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		t.Fatal("first request across the whole server already got 429")
	}

	resp2, err := http.Head(ts.URL + "/alice/my-model/resolve/main/file.bin")
	if err != nil {
		t.Fatalf("HEAD error = %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (burst of 1 already spent by the earlier POST, shared across dispatch functions)", resp2.StatusCode)
	}
}
