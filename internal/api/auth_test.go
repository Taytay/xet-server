package api

// Auth wiring tests: scope enforcement per endpoint, backward compatibility
// (a Server constructed the old way — no SetAuthenticator call — must
// behave identically to before v0.8.0), and adversarial Authorization
// headers. Mirrors internal/casserver/auth_test.go's structure and
// conventions.
//
// Every token string below (e.g. "test-fixture-token-not-a-real-secret") is
// a hardcoded test fixture with no relation to any real credential —
// flagged explicitly so static-analysis secret scanners don't need to
// guess.

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"encoding/json"

	"xet-server/internal/auth"
)

// testFixtureToken is the shared-secret value every test in this file
// configures StaticTokenAuth with. Not a real credential of any kind.
const testFixtureToken = "test-fixture-token-not-a-real-secret"

// newAuthTestServer is like newTestServer but with a StaticTokenAuth
// installed via SetAuthenticator, for tests exercising 401/403 behavior.
func newAuthTestServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	srv, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	srv.SetAuthenticator(auth.NewStaticTokenAuth(token))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

// doWithAuth sends method/url with bearerHeader as the raw Authorization
// header value (skipped entirely if empty). Returns ok=false if net/http's
// own client rejected the header before ever sending the request (e.g. a
// value containing a raw control character or CRLF) — callers should treat
// that as an acceptable outcome for adversarial input, the same as a clean
// 401 from the server, since either way the malformed credential never
// reached (or succeeded against) the server.
func doWithAuth(t *testing.T, method, url, bearerHeader string, body io.Reader) (resp *http.Response, ok bool) {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	if bearerHeader != "" {
		req.Header.Set("Authorization", bearerHeader)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		if strings.Contains(err.Error(), "invalid header field value") {
			return nil, false
		}
		t.Fatalf("%s %s error = %v", method, url, err)
	}
	return resp, true
}

func TestAuth_BackwardCompat_DefaultServerNeverConstructedWithSetAuthenticator(t *testing.T) {
	// The regression guard for "keep current implementation working as
	// is": a Server built exactly the way every pre-v0.8.0 caller already
	// does (New(dataRoot), no SetAuthenticator call at all) must accept
	// every request with zero Authorization header, on both read and
	// write endpoints.
	ts := newTestServer(t)

	uploadResp, ok := doWithAuth(t, http.MethodPost, ts.URL+"/v1/upload", "", bytes.NewReader([]byte("backward compat content")))
	if !ok {
		t.Fatal("request unexpectedly rejected client-side")
	}
	defer uploadResp.Body.Close()
	if uploadResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(uploadResp.Body)
		t.Fatalf("upload status = %d, want 200 with no auth configured; body = %s", uploadResp.StatusCode, body)
	}
	var res UploadResult
	if err := json.NewDecoder(uploadResp.Body).Decode(&res); err != nil {
		t.Fatalf("decode upload response error = %v", err)
	}

	fetchResp, ok := doWithAuth(t, http.MethodGet, ts.URL+"/v1/files/"+res.FileID, "", nil)
	if !ok {
		t.Fatal("request unexpectedly rejected client-side")
	}
	fetchResp.Body.Close()
	if fetchResp.StatusCode != http.StatusOK {
		t.Errorf("fetch status = %d, want 200 with no auth configured", fetchResp.StatusCode)
	}

	manifestResp, ok := doWithAuth(t, http.MethodGet, ts.URL+"/v1/files/"+res.FileID+"/manifest", "", nil)
	if !ok {
		t.Fatal("request unexpectedly rejected client-side")
	}
	manifestResp.Body.Close()
	if manifestResp.StatusCode != http.StatusOK {
		t.Errorf("manifest status = %d, want 200 with no auth configured", manifestResp.StatusCode)
	}

	statsResp, ok := doWithAuth(t, http.MethodGet, ts.URL+"/v1/stats", "", nil)
	if !ok {
		t.Fatal("request unexpectedly rejected client-side")
	}
	statsResp.Body.Close()
	if statsResp.StatusCode != http.StatusOK {
		t.Errorf("stats status = %d, want 200 with no auth configured", statsResp.StatusCode)
	}
}

func TestAuth_UploadEndpoint_RequiresWriteToken(t *testing.T) {
	ts := newAuthTestServer(t, testFixtureToken)

	noTokenResp, ok := doWithAuth(t, http.MethodPost, ts.URL+"/v1/upload", "", bytes.NewReader([]byte("gated content")))
	if !ok {
		t.Fatal("request unexpectedly rejected client-side")
	}
	noTokenResp.Body.Close()
	if noTokenResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-token upload status = %d, want 401", noTokenResp.StatusCode)
	}

	// "wrong-fixture-token": deliberately incorrect, not a real credential.
	wrongTokenResp, ok := doWithAuth(t, http.MethodPost, ts.URL+"/v1/upload", "Bearer wrong-fixture-token", bytes.NewReader([]byte("gated content")))
	if !ok {
		t.Fatal("request unexpectedly rejected client-side")
	}
	wrongTokenResp.Body.Close()
	if wrongTokenResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong-token upload status = %d, want 401", wrongTokenResp.StatusCode)
	}

	correctResp, ok := doWithAuth(t, http.MethodPost, ts.URL+"/v1/upload", "Bearer "+testFixtureToken, bytes.NewReader([]byte("gated content")))
	if !ok {
		t.Fatal("request unexpectedly rejected client-side")
	}
	defer correctResp.Body.Close()
	if correctResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(correctResp.Body)
		t.Errorf("correct-token upload status = %d, want 200; body = %s", correctResp.StatusCode, body)
	}
}

func TestAuth_ReadEndpoints_RequireReadToken(t *testing.T) {
	ts := newAuthTestServer(t, testFixtureToken)

	uploadResp, ok := doWithAuth(t, http.MethodPost, ts.URL+"/v1/upload", "Bearer "+testFixtureToken, bytes.NewReader([]byte("read-gated content")))
	if !ok {
		t.Fatal("request unexpectedly rejected client-side")
	}
	defer uploadResp.Body.Close()
	var res UploadResult
	if err := json.NewDecoder(uploadResp.Body).Decode(&res); err != nil {
		t.Fatalf("decode upload response error = %v", err)
	}

	noTokenResp, ok := doWithAuth(t, http.MethodGet, ts.URL+"/v1/files/"+res.FileID, "", nil)
	if !ok {
		t.Fatal("request unexpectedly rejected client-side")
	}
	noTokenResp.Body.Close()
	if noTokenResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-token fetch status = %d, want 401", noTokenResp.StatusCode)
	}

	correctResp, ok := doWithAuth(t, http.MethodGet, ts.URL+"/v1/files/"+res.FileID, "Bearer "+testFixtureToken, nil)
	if !ok {
		t.Fatal("request unexpectedly rejected client-side")
	}
	defer correctResp.Body.Close()
	if correctResp.StatusCode != http.StatusOK {
		t.Errorf("correct-token fetch status = %d, want 200", correctResp.StatusCode)
	}

	noTokenManifestResp, ok := doWithAuth(t, http.MethodGet, ts.URL+"/v1/files/"+res.FileID+"/manifest", "", nil)
	if !ok {
		t.Fatal("request unexpectedly rejected client-side")
	}
	noTokenManifestResp.Body.Close()
	if noTokenManifestResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-token manifest status = %d, want 401", noTokenManifestResp.StatusCode)
	}
}

func TestAuth_Stats_NeverGated(t *testing.T) {
	// /stats is this project's own operator endpoint, not part of any real
	// protocol, so it is deliberately excluded from scope enforcement
	// regardless of whether an Authenticator is configured.
	ts := newAuthTestServer(t, testFixtureToken)

	statsResp, ok := doWithAuth(t, http.MethodGet, ts.URL+"/v1/stats", "", nil)
	if !ok {
		t.Fatal("request unexpectedly rejected client-side")
	}
	statsResp.Body.Close()
	if statsResp.StatusCode != http.StatusOK {
		t.Errorf("stats (no token) status = %d, want 200", statsResp.StatusCode)
	}
}

func TestAuth_AdversarialAuthorizationHeaders(t *testing.T) {
	ts := newAuthTestServer(t, testFixtureToken)

	cases := []struct {
		name   string
		header string
	}{
		{"empty", ""},
		{"bearer no token", "Bearer"},
		{"bearer empty token", "Bearer "},
		{"wrong scheme", "NotBearer " + testFixtureToken},
		{"absurdly long token", "Bearer " + string(bytes.Repeat([]byte("x"), 100000))},
		{"binary garbage", "Bearer \x00\x01\x02"},
		{"embedded CRLF (header injection attempt)", "Bearer " + testFixtureToken + "\r\nX-Injected: true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, ok := doWithAuth(t, http.MethodGet, ts.URL+"/v1/files/nonexistent-id", tc.header, nil)
			if !ok {
				// net/http's client refused to even send this header (a
				// raw control character or CRLF) — the malformed
				// credential never reached the server, which is just as
				// good as the server rejecting it itself.
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d for header %q, want 401 (none of these are the correct token)", resp.StatusCode, tc.header)
			}
		})
	}

	// Server must still be healthy after all that.
	healthResp, ok := doWithAuth(t, http.MethodGet, ts.URL+"/v1/stats", "", nil)
	if !ok {
		t.Fatal("request unexpectedly rejected client-side")
	}
	healthResp.Body.Close()
	if healthResp.StatusCode != http.StatusOK {
		t.Errorf("server unhealthy after adversarial auth headers: stats status = %d", healthResp.StatusCode)
	}
}
