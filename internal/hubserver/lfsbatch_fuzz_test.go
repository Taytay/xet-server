package hubserver

// lfsbatch_fuzz_test.go drives arbitrary bodies through the git-lfs batch
// endpoint over a real HTTP server. This endpoint is the entry point to
// the whole Xet upload path (see docs/PROTOCOL.md section 13), it is
// reached with only a write-scope check in front of it, and it ECHOES
// caller-supplied data back in its response - so it is worth asserting
// directly that no input can panic it, make it hang, or turn a small
// request into a large reply.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// FuzzLFSBatchHandler asserts the batch endpoint always answers with a
// sane status and, when it answers 200, a well-formed response that is
// bounded by the caller's own object count (never larger).
func FuzzLFSBatchHandler(f *testing.F) {
	f.Add(`{"operation":"upload","objects":[{"oid":"abc","size":1}]}`)
	f.Add(`{"operation":"upload","objects":[]}`)
	f.Add(`{"operation":"download","objects":[{"oid":"a","size":1}]}`)
	f.Add(`{"operation":"upload"}`)
	f.Add(`{}`)
	f.Add(``)
	f.Add(`null`)
	f.Add(`[]`)
	f.Add(`{"operation":"upload","objects":null}`)
	f.Add(`{"operation":"upload","objects":[{"oid":null,"size":null}]}`)
	f.Add(`{"operation":"upload","objects":[{"oid":"a","size":-9223372036854775808}]}`)
	f.Add(`{"operation":"upload","objects":[{"oid":"` + strings.Repeat("a", 8192) + `","size":1}]}`)
	f.Add(`{"operation":"upload","objects":[` + strings.Repeat(`{"oid":"a","size":1},`, 200) + `{"oid":"z","size":1}]}`)
	f.Add("{\"operation\":\"upload\",\"objects\":[{\"oid\":\"\x00\",\"size\":1}]}")
	f.Add(strings.Repeat(`{"operation":"upload",`, 64))

	srv := New("http://localhost:9999", newFakeCAS())
	ts := httptest.NewServer(srv)
	f.Cleanup(ts.Close)

	f.Fuzz(func(t *testing.T, body string) {
		req, err := http.NewRequest(http.MethodPost,
			ts.URL+"/fuzzns/fuzzrepo.git/info/lfs/objects/batch",
			bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("request failed (handler panicked or hung?): %v", err)
		}
		defer resp.Body.Close()

		var out bytes.Buffer
		if _, err := out.ReadFrom(resp.Body); err != nil {
			t.Fatalf("read response: %v", err)
		}

		switch resp.StatusCode {
		case http.StatusOK:
			// A 200 must be a decodable batch response advertising xet,
			// and must not contain more objects than were asked for -
			// the anti-amplification property.
			var parsed lfsBatchResponse
			if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
				t.Fatalf("200 response is not valid JSON: %v (body %q)", err, out.String())
			}
			if parsed.Transfer != "xet" {
				t.Fatalf("200 response advertised transfer %q, want \"xet\"", parsed.Transfer)
			}
			var req lfsBatchRequest
			if err := json.Unmarshal([]byte(body), &req); err != nil {
				t.Fatalf("handler accepted a body that does not decode: %q", body)
			}
			if len(parsed.Objects) > len(req.Objects) {
				t.Fatalf("response echoed %d objects for a request carrying %d - amplification",
					len(parsed.Objects), len(req.Objects))
			}
			if len(parsed.Objects) > maxLFSBatchObjects {
				t.Fatalf("response carried %d objects, over the %d cap",
					len(parsed.Objects), maxLFSBatchObjects)
			}
		case http.StatusBadRequest, http.StatusRequestEntityTooLarge,
			http.StatusMethodNotAllowed, http.StatusUnauthorized, http.StatusForbidden:
			// Expected rejection paths.
		default:
			t.Fatalf("unexpected status %d for body %q", resp.StatusCode, body)
		}
	})
}

// TestLFSBatch_RejectsOversizeBody pins the request-body bound: a body
// past maxLFSBatchBytes must be refused with 413 rather than buffered.
func TestLFSBatch_RejectsOversizeBody(t *testing.T) {
	srv := New("http://localhost:9999", newFakeCAS())
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Comfortably over the cap, and valid JSON right up to the cut.
	var b bytes.Buffer
	b.WriteString(`{"operation":"upload","objects":[`)
	for b.Len() < maxLFSBatchBytes+(1<<20) {
		b.WriteString(`{"oid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":1},`)
	}
	b.WriteString(`{"oid":"z","size":1}]}`)

	resp, err := ts.Client().Post(
		ts.URL+"/ns/repo.git/info/lfs/objects/batch",
		"application/json", bytes.NewReader(b.Bytes()))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d for a body over the %d-byte cap",
			resp.StatusCode, http.StatusRequestEntityTooLarge, maxLFSBatchBytes)
	}
}

// TestLFSBatch_RejectsTooManyObjects pins the object-count bound. This is
// the amplification guard: the body here is small, but echoing every
// object back would produce a far larger response.
func TestLFSBatch_RejectsTooManyObjects(t *testing.T) {
	srv := New("http://localhost:9999", newFakeCAS())
	ts := httptest.NewServer(srv)
	defer ts.Close()

	var b bytes.Buffer
	b.WriteString(`{"operation":"upload","objects":[`)
	for i := 0; i <= maxLFSBatchObjects; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"oid":"a","size":1}`)
	}
	b.WriteString(`]}`)

	if b.Len() >= maxLFSBatchBytes {
		t.Fatalf("test body (%d bytes) hit the size cap first; it must exercise the COUNT cap", b.Len())
	}

	resp, err := ts.Client().Post(
		ts.URL+"/ns/repo.git/info/lfs/objects/batch",
		"application/json", bytes.NewReader(b.Bytes()))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d for %d objects (cap %d)",
			resp.StatusCode, http.StatusRequestEntityTooLarge,
			maxLFSBatchObjects+1, maxLFSBatchObjects)
	}
}

// TestLFSBatch_RejectsMalformedRepoID pins that a path whose last two
// segments aren't a usable namespace/name is refused, so no repo state is
// created for an identity no real request can address.
func TestLFSBatch_RejectsMalformedRepoID(t *testing.T) {
	srv := New("http://localhost:9999", newFakeCAS())
	ts := httptest.NewServer(srv)
	defer ts.Close()

	for _, path := range []string{
		"//.git/info/lfs/objects/batch",
		"/.git/info/lfs/objects/batch",
	} {
		resp, err := ts.Client().Post(ts.URL+path, "application/json",
			strings.NewReader(`{"operation":"upload","objects":[{"oid":"a","size":1}]}`))
		if err != nil {
			t.Fatalf("request %q: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("path %q returned 200; want a rejection for a malformed repo id", path)
		}
	}
}
