package hubserver

// Adversarial payload tests for the Hub API shim: malformed ndjson commit
// bodies, hostile repo/file path segments, and oversized inputs. Mirrors
// casserver's adversarial_test.go in spirit — checked for a clean error
// response and that the server keeps answering correctly afterward.

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

func assertHubServerStillHealthy(t *testing.T, client *http.Client, baseURL string) {
	t.Helper()
	resp, err := client.Post(baseURL+"/api/repos/create", "application/json", strings.NewReader(`{"name":"health-check","type":"model"}`))
	if err != nil {
		t.Fatalf("server unresponsive after adversarial request: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("server responded %d to a trivial repo-create after adversarial request, want 200", resp.StatusCode)
	}
}

func TestAdversarial_Commit(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"single newline", "\n"},
		{"not json at all", "this is not json\nnor is this\n"},
		{"json but wrong shape", `{"key": 12345}` + "\n"},
		{"deeply nested json", strings.Repeat(`{"a":`, 10000) + `1` + strings.Repeat(`}`, 10000)},
		{"lfsFile with null path", `{"key":"lfsFile","value":{"path":null,"oid":"abc","size":10}}` + "\n"},
		{"lfsFile with path traversal", `{"key":"lfsFile","value":{"path":"../../../etc/passwd","oid":"abc","size":10}}` + "\n"},
		{"lfsFile with huge size field", `{"key":"lfsFile","value":{"path":"f","oid":"abc","size":99999999999999999999}}` + "\n"},
		{"lfsFile with negative size", `{"key":"lfsFile","value":{"path":"f","oid":"abc","size":-1}}` + "\n"},
		{"lfsFile with empty oid", `{"key":"lfsFile","value":{"path":"f","oid":"","size":10}}` + "\n"},
		{"lfsFile with binary garbage in oid", `{"key":"lfsFile","value":{"path":"f","oid":"` + string([]byte{0x00, 0x01, 0xff}) + `","size":10}}` + "\n"},
		{"many valid lines", strings.Repeat(`{"key":"lfsFile","value":{"path":"f","oid":"abc","size":1}}`+"\n", 1000)},
		{"null bytes throughout", string(bytes.Repeat([]byte{0x00}, 500))},
		{"unterminated json", `{"key":"lfsFile","value":{"path":"f"`},
	}

	ts, _ := newTestServer(t)
	client := ts.Client()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := client.Post(ts.URL+"/api/models/alice/my-model/commit/main", "application/x-ndjson", strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("POST error = %v", err)
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
			// Either success (some of these are actually valid-enough
			// ndjson) or a clean 4xx is fine; a 500 or a hang is not.
			if resp.StatusCode >= 500 {
				t.Errorf("status = %d for %q, want < 500", resp.StatusCode, tc.name)
			}
		})
	}

	assertHubServerStillHealthy(t, client, ts.URL)
}

func TestAdversarial_CommitOversizedLine(t *testing.T) {
	ts, _ := newTestServer(t)
	client := ts.Client()

	// One ndjson line larger than bufio.Scanner's configured max token
	// size (10 MiB, see handleCommit) — must surface as a clean error via
	// scanner.Err(), not panic or hang.
	oversizedLine := `{"key":"lfsFile","value":{"path":"` + strings.Repeat("x", 11*1024*1024) + `","oid":"abc","size":1}}` + "\n"

	resp, err := client.Post(ts.URL+"/api/models/alice/my-model/commit/main", "application/x-ndjson", strings.NewReader(oversizedLine))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a commit line exceeding the scanner's max token size", resp.StatusCode)
	}

	assertHubServerStillHealthy(t, client, ts.URL)
}

func TestAdversarial_ResolveHostileFilenames(t *testing.T) {
	ts, _ := newTestServer(t)
	client := ts.Client()

	hostileFilenames := []string{
		"",
		"../../../etc/passwd",
		strings.Repeat("a", 100000),
		"file\x00with\x00nulls",
		"%2e%2e%2fpath%2ftraversal",
		"file with spaces and 'quotes\" and <tags>",
		"\n\r\t",
		strings.Repeat("😀", 1000), // multi-byte UTF-8 stress
	}

	for _, filename := range hostileFilenames {
		t.Run("filename", func(t *testing.T) {
			resp, err := client.Get(ts.URL + "/alice/my-model/resolve/main/" + filename)
			if err != nil {
				// A transport-level rejection of a malformed URL is fine.
				return
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
			if resp.StatusCode >= 500 {
				t.Errorf("status = %d for filename %q, want < 500 (expect 404, since it was never committed)", resp.StatusCode, filename)
			}
		})
	}

	assertHubServerStillHealthy(t, client, ts.URL)
}

func TestAdversarial_PreuploadMalformedJSON(t *testing.T) {
	ts, _ := newTestServer(t)
	client := ts.Client()

	cases := []string{
		"",
		"not json",
		`{"files": "not an array"}`,
		`{"files": [{"path": null, "size": "not a number"}]}`,
		strings.Repeat("[", 100000),
	}

	for _, body := range cases {
		resp, err := client.Post(ts.URL+"/api/models/alice/my-model/preupload/main", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST error = %v", err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		if resp.StatusCode >= 500 {
			t.Errorf("status = %d for body %q, want < 500", resp.StatusCode, body)
		}
	}

	assertHubServerStillHealthy(t, client, ts.URL)
}

func TestAdversarial_RepoIDWithHostileSegments(t *testing.T) {
	ts, _ := newTestServer(t)
	client := ts.Client()

	hostilePaths := []string{
		"/api/models/../../etc/passwd/commit/main",
		"/api/models//commit/main",
		"/api/models/alice/commit/main", // missing name segment
		"/api/" + strings.Repeat("x", 10000) + "s/alice/model/commit/main",
	}

	for _, path := range hostilePaths {
		resp, err := client.Post(ts.URL+path, "application/x-ndjson", strings.NewReader(""))
		if err != nil {
			continue
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		if resp.StatusCode >= 500 {
			t.Errorf("status = %d for path %q, want < 500", resp.StatusCode, path)
		}
	}

	assertHubServerStillHealthy(t, client, ts.URL)
}
