package hfclient

// Regression tests for the CAS upload Content-Type header. The real CAS
// server's openapi (xet-core/openapi/cas.openapi.yaml) declares
// application/octet-stream for POST /v1/xorbs/{prefix}/{hash} and
// POST /v1/shards; without it a FastAPI-backed upstream rejects the body.

import (
	"bytes"
	"context"
	"net/http"
	"testing"
)

func TestUploadXorb_SendsOctetStreamContentType(t *testing.T) {
	c := newTestCASClient(t, func(w http.ResponseWriter, r *http.Request) {
		requireContentType(t, r, "application/octet-stream")
		w.WriteHeader(http.StatusOK)
	})
	resp, err := c.UploadXorb(context.Background(), nil, "default", "deadbeef", bytes.NewReader([]byte("xorb")))
	if err != nil {
		t.Fatalf("UploadXorb() error = %v", err)
	}
	defer resp.Body.Close()
}

func TestUploadShard_SendsOctetStreamContentType(t *testing.T) {
	c := newTestCASClient(t, func(w http.ResponseWriter, r *http.Request) {
		requireContentType(t, r, "application/octet-stream")
		w.WriteHeader(http.StatusOK)
	})
	resp, err := c.UploadShard(context.Background(), nil, bytes.NewReader([]byte("shard")))
	if err != nil {
		t.Fatalf("UploadShard() error = %v", err)
	}
	defer resp.Body.Close()
}

// TestFetchPresigned_ForwardsURLAndRange pins that FetchPresigned GETs the
// exact presigned URL it is given with the exact Range header and attaches
// no credential (the signature is in the URL itself).
func TestFetchPresigned_ForwardsURLAndRange(t *testing.T) {
	wantPath := "/presigned/xorb"
	c := newTestCASClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wantPath {
			t.Errorf("path = %q, want %q", r.URL.Path, wantPath)
		}
		if got := r.Header.Get("Range"); got != "bytes=0-65" {
			t.Errorf("Range = %q, want bytes=0-65", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want empty (presigned URL carries its own signature)", got)
		}
		w.Write([]byte("bytes"))
	})
	resp, err := c.FetchPresigned(context.Background(), c.BaseURL+wantPath, "bytes=0-65")
	if err != nil {
		t.Fatalf("FetchPresigned() error = %v", err)
	}
	defer resp.Body.Close()
}
