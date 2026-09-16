package proxyhub

// token_heal_test.go: tests for the two token-hardening behaviors added
// for reliable long-running mirrors -
//   1. the stale-fallback refusing to serve an already-expired cached xet
//      token (token.go's tokenSafetyMargin guard), and
//   2. FreshXetTokenFor mints a fresh replacement for a token the proxy
//      relayed, using the same client credential (the heal behind
//      proxycas's fetchCASWithHeal).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/guilt/xet-server/internal/hfclient"
)

// TestXetToken_OfflineWithExpiredCachedTokenReturnsError pins the stale
// fallback guard: once the cached xet token has expired, a live-refresh
// failure must NOT serve it (the real CAS would reject it with a baffling
// 401); the upstream failure is surfaced instead.
func TestXetToken_OfflineWithExpiredCachedTokenReturnsError(t *testing.T) {
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(hfclient.XetToken{CasURL: "https://real-cas.example.invalid", Exp: time.Now().Add(-time.Hour).Unix(), AccessToken: "already-expired-token"})
	}))

	s := newTestServer(hubTS, "http://localhost:8420")
	ts := httptest.NewServer(s)
	defer ts.Close()

	url := ts.URL + "/api/models/alice/my-model/xet-read-token/main"
	primeResp, err := http.Get(url)
	if err != nil {
		t.Fatalf("priming GET error = %v", err)
	}
	primeResp.Body.Close()

	hubTS.Close()

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET after upstream went offline error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("status = %d, want non-200: an expired cached token must not be served stale", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "already-expired-token") {
		t.Error("response carries the expired cached token - the stale-fallback must refuse it")
	}
}

// TestFreshXetTokenFor_MintsFreshReplacementWithRecordedCredential pins the
// heal's token side: a token the proxy relayed is remembered together with
// the credential that minted it, and FreshXetTokenFor mints a replacement
// using that SAME credential (verified by checking the Authorization header
// the real Hub sees on the refresh call).
func TestFreshXetTokenFor_MintsFreshReplacementWithRecordedCredential(t *testing.T) {
	const (
		clientCredential = "Bearer hf-test-credential"
		firstToken       = "original-upstream-token"
		secondToken      = "freshly-minted-token"
	)
	var (
		firstCredential  string
		secondCredential string
		callCount        int
	)
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		cred := r.Header.Get("Authorization")
		switch callCount {
		case 1:
			firstCredential = cred
			json.NewEncoder(w).Encode(hfclient.XetToken{CasURL: "https://real-cas.example.invalid", Exp: time.Now().Add(time.Hour).Unix(), AccessToken: firstToken})
		case 2:
			secondCredential = cred
			json.NewEncoder(w).Encode(hfclient.XetToken{CasURL: "https://real-cas.example.invalid", Exp: time.Now().Add(time.Hour).Unix(), AccessToken: secondToken})
		default:
			t.Errorf("unexpected extra Hub call #%d", callCount)
		}
	}))
	defer hubTS.Close()

	s := newTestServer(hubTS, "http://localhost:8420")
	ts := httptest.NewServer(s)
	defer ts.Close()

	// Prime the token through the real handler so recordAccessToken records
	// the client's credential against firstToken, exactly as a live client
	// would.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/models/alice/my-model/xet-read-token/main", nil)
	req.Header.Set("Authorization", clientCredential)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("priming GET error = %v", err)
	}
	resp.Body.Close()

	if firstCredential != clientCredential {
		t.Fatalf("first Hub call credential = %q, want the client's forwarded %q", firstCredential, clientCredential)
	}

	fresh, ok := s.FreshXetTokenFor(context.Background(), firstToken)
	if !ok {
		t.Fatal("FreshXetTokenFor() ok = false, want true for a token the proxy relayed")
	}
	if fresh != secondToken {
		t.Errorf("fresh token = %q, want %q", fresh, secondToken)
	}
	if secondCredential != clientCredential {
		t.Errorf("heal refresh credential = %q, want the client's own credential forwarded unchanged", secondCredential)
	}
}

// TestFreshXetTokenFor_UnknownTokenReturnsFalse pins that a token this proxy
// never relayed cannot be healed (the proxy has no idea which repo/ref or
// credential to mint for) - the caller must relay the 401 as-is.
func TestFreshXetTokenFor_UnknownTokenReturnsFalse(t *testing.T) {
	hubTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("an unknown token must not trigger a Hub call")
	}))
	defer hubTS.Close()

	s := newTestServer(hubTS, "http://localhost:8420")
	if fresh, ok := s.FreshXetTokenFor(context.Background(), "token-this-proxy-never-relayed"); ok {
		t.Errorf("FreshXetTokenFor() = (%q, true), want ok=false for an unknown token", fresh)
	}
}
