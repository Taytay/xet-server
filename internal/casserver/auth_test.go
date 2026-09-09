package casserver

// Auth wiring tests: scope enforcement per endpoint, backward
// compatibility (a Server constructed the old way — no
// SetAuthenticator call — must behave identically to before v0.8.0),
// and adversarial Authorization headers.
//
// Every token string below (e.g. "test-fixture-token-not-a-real-secret")
// is a hardcoded test fixture with no relation to any real credential —
// flagged explicitly so static-analysis secret scanners don't need to
// guess.

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"xet-server/internal/auth"
	"xet-server/internal/storage/fsstore"
)

// testFixtureToken is the shared-secret value every test in this file
// configures StaticTokenAuth with. Not a real credential of any kind —
// just a fixed string two test helpers need to agree on.
const testFixtureToken = "test-fixture-token-not-a-real-secret"

// newAuthTestServer is like newTestServer but with a StaticTokenAuth
// installed via SetAuthenticator, for tests exercising 401/403 behavior.
func newAuthTestServer(t *testing.T, token string) (*httptest.Server, *Server) {
	t.Helper()
	store, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("fsstore.New() error = %v", err)
	}
	casSrv := New(store)
	casSrv.SetAuthenticator(auth.NewStaticTokenAuth(token))
	httpSrv := httptest.NewServer(casSrv)
	t.Cleanup(httpSrv.Close)
	return httpSrv, casSrv
}

// doWithAuth sends method/url with bearerHeader as the raw Authorization
// header value (skipped entirely if empty). Returns ok=false if
// net/http's own client rejected the header before ever sending the
// request (e.g. a value containing a raw control character or CRLF —
// net/http validates this client-side and never puts such a value on the
// wire) — callers should treat that as an acceptable outcome for
// adversarial input, the same as a clean 401 from the server, since
// either way the malformed credential never reached (or succeeded
// against) the server.
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
	// does (New(store), no SetAuthenticator call at all) must accept
	// every request with zero Authorization header, on both read and
	// write endpoints.
	ts, _ := newTestServer(t)

	telemetryResp, ok := doWithAuth(t, http.MethodPost, ts.URL+"/v1/telemetry", "", nil)
	if !ok {
		t.Fatal("request unexpectedly rejected client-side")
	}
	telemetryResp.Body.Close()
	if telemetryResp.StatusCode != http.StatusOK {
		t.Errorf("telemetry status = %d, want 200 with no auth configured", telemetryResp.StatusCode)
	}

	blob, xorbHash, _ := buildXorb(t, [][]byte{[]byte("backward compat content")})
	uploadResp, ok := doWithAuth(t, http.MethodPost, ts.URL+"/v1/xorbs/default/"+xorbHash.Hex(), "", bytes.NewReader(blob))
	if !ok {
		t.Fatal("request unexpectedly rejected client-side")
	}
	defer uploadResp.Body.Close()
	if uploadResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(uploadResp.Body)
		t.Errorf("upload status = %d, want 200 with no auth configured; body = %s", uploadResp.StatusCode, body)
	}

	fetchResp, ok := doWithAuth(t, http.MethodGet, ts.URL+"/v1/xorbs/default/"+xorbHash.Hex(), "", nil)
	if !ok {
		t.Fatal("request unexpectedly rejected client-side")
	}
	fetchResp.Body.Close()
	if fetchResp.StatusCode != http.StatusOK {
		t.Errorf("fetch status = %d, want 200 with no auth configured", fetchResp.StatusCode)
	}
}

func TestAuth_WriteEndpoint_RequiresToken(t *testing.T) {
	ts, _ := newAuthTestServer(t, testFixtureToken)
	blob, xorbHash, _ := buildXorb(t, [][]byte{[]byte("gated content")})

	noTokenResp, ok := doWithAuth(t, http.MethodPost, ts.URL+"/v1/xorbs/default/"+xorbHash.Hex(), "", bytes.NewReader(blob))
	if !ok {
		t.Fatal("request unexpectedly rejected client-side")
	}
	noTokenResp.Body.Close()
	if noTokenResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-token upload status = %d, want 401", noTokenResp.StatusCode)
	}

	// "wrong-fixture-token": deliberately incorrect, not a real credential.
	wrongTokenResp, ok := doWithAuth(t, http.MethodPost, ts.URL+"/v1/xorbs/default/"+xorbHash.Hex(), "Bearer wrong-fixture-token", bytes.NewReader(blob))
	if !ok {
		t.Fatal("request unexpectedly rejected client-side")
	}
	wrongTokenResp.Body.Close()
	if wrongTokenResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong-token upload status = %d, want 401", wrongTokenResp.StatusCode)
	}

	correctResp, ok := doWithAuth(t, http.MethodPost, ts.URL+"/v1/xorbs/default/"+xorbHash.Hex(), "Bearer "+testFixtureToken, bytes.NewReader(blob))
	if !ok {
		t.Fatal("request unexpectedly rejected client-side")
	}
	defer correctResp.Body.Close()
	if correctResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(correctResp.Body)
		t.Errorf("correct-token upload status = %d, want 200; body = %s", correctResp.StatusCode, body)
	}
}

func TestAuth_ReadEndpoint_RequiresToken(t *testing.T) {
	ts, _ := newAuthTestServer(t, testFixtureToken)

	// Upload with the correct token first so there's something to fetch.
	blob, xorbHash, _ := buildXorb(t, [][]byte{[]byte("read-gated content")})
	uploadResp, ok := doWithAuth(t, http.MethodPost, ts.URL+"/v1/xorbs/default/"+xorbHash.Hex(), "Bearer "+testFixtureToken, bytes.NewReader(blob))
	if !ok {
		t.Fatal("request unexpectedly rejected client-side")
	}
	uploadResp.Body.Close()

	noTokenResp, ok := doWithAuth(t, http.MethodGet, ts.URL+"/v1/xorbs/default/"+xorbHash.Hex(), "", nil)
	if !ok {
		t.Fatal("request unexpectedly rejected client-side")
	}
	noTokenResp.Body.Close()
	if noTokenResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-token fetch status = %d, want 401", noTokenResp.StatusCode)
	}

	correctResp, ok := doWithAuth(t, http.MethodGet, ts.URL+"/v1/xorbs/default/"+xorbHash.Hex(), "Bearer "+testFixtureToken, nil)
	if !ok {
		t.Fatal("request unexpectedly rejected client-side")
	}
	defer correctResp.Body.Close()
	if correctResp.StatusCode != http.StatusOK {
		t.Errorf("correct-token fetch status = %d, want 200", correctResp.StatusCode)
	}
}

func TestAuth_TelemetryAndStorageStats_NeverGated(t *testing.T) {
	// Telemetry and storage-stats are deliberately excluded from scope
	// enforcement (see casserver.routes' comment) regardless of whether
	// an Authenticator is configured — telemetry has nothing sensitive to
	// protect, and storage-stats is this project's own operator endpoint,
	// not part of the real Xet CAS API.
	ts, _ := newAuthTestServer(t, testFixtureToken)

	telemetryResp, ok := doWithAuth(t, http.MethodPost, ts.URL+"/v1/telemetry", "", nil)
	if !ok {
		t.Fatal("request unexpectedly rejected client-side")
	}
	telemetryResp.Body.Close()
	if telemetryResp.StatusCode != http.StatusOK {
		t.Errorf("telemetry (no token) status = %d, want 200", telemetryResp.StatusCode)
	}

	statsResp, ok := doWithAuth(t, http.MethodGet, ts.URL+"/v1/storage-stats", "", nil)
	if !ok {
		t.Fatal("request unexpectedly rejected client-side")
	}
	statsResp.Body.Close()
	if statsResp.StatusCode != http.StatusOK {
		t.Errorf("storage-stats (no token) status = %d, want 200", statsResp.StatusCode)
	}
}

func TestAuth_AdversarialAuthorizationHeaders(t *testing.T) {
	ts, _ := newAuthTestServer(t, testFixtureToken)
	someHash := "0000000000000000000000000000000000000000000000000000000000000000"[:64]

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
			resp, ok := doWithAuth(t, http.MethodGet, ts.URL+"/v1/xorbs/default/"+someHash, tc.header, nil)
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
	healthResp, ok := doWithAuth(t, http.MethodPost, ts.URL+"/v1/telemetry", "", nil)
	if !ok {
		t.Fatal("request unexpectedly rejected client-side")
	}
	healthResp.Body.Close()
	if healthResp.StatusCode != http.StatusOK {
		t.Errorf("server unhealthy after adversarial auth headers: telemetry status = %d", healthResp.StatusCode)
	}
}

func TestAuth_V2ReconstructionAndChunkDedup_RequireReadScope(t *testing.T) {
	ts, _ := newAuthTestServer(t, testFixtureToken)
	someHash := "0000000000000000000000000000000000000000000000000000000000000000"[:64]

	v2Resp, ok := doWithAuth(t, http.MethodGet, ts.URL+"/v2/reconstructions/"+someHash, "", nil)
	if !ok {
		t.Fatal("request unexpectedly rejected client-side")
	}
	v2Resp.Body.Close()
	if v2Resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("V2 reconstruction (no token) status = %d, want 401", v2Resp.StatusCode)
	}

	dedupResp, ok := doWithAuth(t, http.MethodGet, ts.URL+"/v1/chunks/default-merkledb/"+someHash, "", nil)
	if !ok {
		t.Fatal("request unexpectedly rejected client-side")
	}
	dedupResp.Body.Close()
	if dedupResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("chunk-dedup (no token) status = %d, want 401", dedupResp.StatusCode)
	}
}
