package hfclient

// Tests for the CAS-side calls (FetchXorb, FetchChunkDedup,
// FetchReconstruction, UploadXorb, UploadShard), driven against a fake
// upstream httptest.Server. Unlike the Hub-side tests, these confirm the
// caller-closes-body, non-2xx-is-not-an-error contract explicitly - a
// 404/416/206 from CAS is exactly what internal/proxycas needs to relay
// downstream unchanged, not something this package should collapse into
// an error.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/guilt/xet-server/internal/auth"
)

func newTestCASClient(t *testing.T, handler http.HandlerFunc) *CASClient {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return NewCASClient(ts.URL)
}

func TestFetchXorb_SendsExactURLAndForwardsCredential(t *testing.T) {
	c := newTestCASClient(t, func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r, testFixtureToken)
		if r.URL.Path != "/v1/xorbs/default/deadbeef" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Write([]byte("xorb bytes"))
	})

	resp, err := c.FetchXorb(context.Background(), auth.NewBearerCredentialHelper(testFixtureToken), "default", "deadbeef", "")
	if err != nil {
		t.Fatalf("FetchXorb() error = %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "xorb bytes" {
		t.Errorf("body = %q", body)
	}
}

func TestFetchXorb_ForwardsRangeHeader(t *testing.T) {
	var gotRange string
	c := newTestCASClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		w.WriteHeader(http.StatusPartialContent)
	})

	resp, err := c.FetchXorb(context.Background(), nil, "default", "deadbeef", "bytes=0-99")
	if err != nil {
		t.Fatalf("FetchXorb() error = %v", err)
	}
	resp.Body.Close()
	if gotRange != "bytes=0-99" {
		t.Errorf("Range header = %q, want %q", gotRange, "bytes=0-99")
	}
	if resp.StatusCode != http.StatusPartialContent {
		t.Errorf("status = %d, want 206", resp.StatusCode)
	}
}

func TestFetchXorb_NoRangeHeaderWhenEmpty(t *testing.T) {
	c := newTestCASClient(t, func(w http.ResponseWriter, r *http.Request) {
		if _, present := r.Header["Range"]; present {
			t.Error("Range header present, want absent when rangeHeader is empty")
		}
		w.WriteHeader(http.StatusOK)
	})
	resp, err := c.FetchXorb(context.Background(), nil, "default", "deadbeef", "")
	if err != nil {
		t.Fatalf("FetchXorb() error = %v", err)
	}
	resp.Body.Close()
}

func TestFetchXorb_404IsNotAnError(t *testing.T) {
	// A CAS 404 (unknown xorb) is exactly what proxycas needs to relay
	// downstream unchanged, not something this package should turn into
	// a Go error - unlike the Hub-side calls, which do return
	// *StatusError on a non-2xx.
	c := newTestCASClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	resp, err := c.FetchXorb(context.Background(), nil, "default", "unknown", "")
	if err != nil {
		t.Fatalf("FetchXorb() error = %v, want nil (404 is a normal response, not a client error)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestFetchChunkDedup_SendsExactURL(t *testing.T) {
	c := newTestCASClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chunks/default-merkledb/abc123" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Write([]byte("shard bytes"))
	})
	resp, err := c.FetchChunkDedup(context.Background(), nil, "default-merkledb", "abc123")
	if err != nil {
		t.Fatalf("FetchChunkDedup() error = %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "shard bytes" {
		t.Errorf("body = %q", body)
	}
}

func TestFetchReconstruction_V1VsV2SendsDifferentPath(t *testing.T) {
	var gotPath string
	c := newTestCASClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte("{}"))
	})

	resp, err := c.FetchReconstruction(context.Background(), nil, "deadbeef", "", false)
	if err != nil {
		t.Fatalf("FetchReconstruction(v1) error = %v", err)
	}
	resp.Body.Close()
	if gotPath != "/v1/reconstructions/deadbeef" {
		t.Errorf("v1 path = %q", gotPath)
	}

	resp2, err := c.FetchReconstruction(context.Background(), nil, "deadbeef", "", true)
	if err != nil {
		t.Fatalf("FetchReconstruction(v2) error = %v", err)
	}
	resp2.Body.Close()
	if gotPath != "/v2/reconstructions/deadbeef" {
		t.Errorf("v2 path = %q", gotPath)
	}
}

func TestFetchReconstruction_ForwardsRangeHeader(t *testing.T) {
	var gotRange string
	c := newTestCASClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		w.Write([]byte("{}"))
	})
	resp, err := c.FetchReconstruction(context.Background(), nil, "deadbeef", "bytes=100-199", false)
	if err != nil {
		t.Fatalf("FetchReconstruction() error = %v", err)
	}
	resp.Body.Close()
	if gotRange != "bytes=100-199" {
		t.Errorf("Range header = %q", gotRange)
	}
}

func TestUploadXorb_SendsBodyToExactURL(t *testing.T) {
	c := newTestCASClient(t, func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r, testFixtureToken)
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/xorbs/default/deadbeef" {
			t.Errorf("path = %q", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "xorb payload" {
			t.Errorf("body = %q", body)
		}
		w.WriteHeader(http.StatusOK)
	})

	resp, err := c.UploadXorb(context.Background(), auth.NewBearerCredentialHelper(testFixtureToken), "default", "deadbeef", bytes.NewReader([]byte("xorb payload")))
	if err != nil {
		t.Fatalf("UploadXorb() error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestUploadShard_SendsBodyToExactURL(t *testing.T) {
	c := newTestCASClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/shards" {
			t.Errorf("path = %q", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "shard payload" {
			t.Errorf("body = %q", body)
		}
		w.WriteHeader(http.StatusOK)
	})
	resp, err := c.UploadShard(context.Background(), nil, bytes.NewReader([]byte("shard payload")))
	if err != nil {
		t.Fatalf("UploadShard() error = %v", err)
	}
	resp.Body.Close()
}

func TestNewCASClient_UsesGivenBaseURLExactly(t *testing.T) {
	c := NewCASClient("https://cas.example.invalid")
	if c.BaseURL != "https://cas.example.invalid" {
		t.Errorf("BaseURL = %q", c.BaseURL)
	}
}
