package proxycas

// Tests for SetRateLimiter: does gate actually enforce it (burst
// allowed, then 429 with Retry-After), and - since every route in this
// package can fall back to the embedded server's cached data on an
// upstream failure (see the package doc comment) - does a configured
// rate limiter leave that fallback path itself working for requests
// still within budget, rather than accidentally interfering with it?

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/guilt/xet-server/internal/ratelimit"
)

func TestRateLimit_BurstAllowedThenTooManyRequests(t *testing.T) {
	upstream := newFakeUpstreamCAS()
	casTS := httptest.NewServer(upstream.handler())
	defer casTS.Close()

	ts, proxy := newTestServer(t, casTS)
	proxy.SetRateLimiter(ratelimit.New(2, 0)) // burst=2, no refill

	unknown := hashFromByte(0xAA)
	url := ts.URL + "/v1/xorbs/default/" + unknown.Hex()

	for i := 0; i < 2; i++ {
		resp, err := http.Head(url)
		if err != nil {
			t.Fatalf("HEAD #%d error = %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("HEAD #%d got 429 within burst allowance", i)
		}
	}

	resp, err := http.Head(url)
	if err != nil {
		t.Fatalf("HEAD (over budget) error = %v", err)
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
	upstream := newFakeUpstreamCAS()
	casTS := httptest.NewServer(upstream.handler())
	defer casTS.Close()

	ts, _ := newTestServer(t, casTS) // no SetRateLimiter call: nil is the default

	unknown := hashFromByte(0xAB)
	for i := 0; i < 5; i++ {
		resp, err := http.Head(ts.URL + "/v1/xorbs/default/" + unknown.Hex())
		if err != nil {
			t.Fatalf("HEAD #%d error = %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("HEAD #%d got 429 with no rate limiter configured", i)
		}
	}
}

func TestRateLimit_OfflineFallbackStillServedWithinBudget(t *testing.T) {
	// A rate limiter must gate request volume, not interfere with the
	// stale-cache-fallback path that's this package's whole reason to
	// exist (see TestFetchXorb_OfflineAfterCacheStillServes).
	blob, xorbHash := buildFooterlessXorb(t, [][]byte{[]byte("chunk payload")})
	upstream := newFakeUpstreamCAS()
	upstream.xorbs[xorbHash.Hex()] = blob
	casTS := httptest.NewServer(upstream.handler())

	ts, proxy := newTestServer(t, casTS)
	proxy.SetRateLimiter(ratelimit.New(5, 100))

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
		t.Fatalf("status = %d, want 200 (rate limiting must not block a within-budget fallback-to-cache request)", resp.StatusCode)
	}
}
