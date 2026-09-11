package casserver

// Regression tests for two bugs found during a manual code-quality
// review: a ranged xorb fetch silently dropping its own Content-Length
// header, and HEAD /v1/xorbs/{prefix}/{hash} skipping the prefix
// validation GET already enforces.

import (
	"bytes"
	"net/http"
	"strconv"
	"testing"
)

func TestFetchXorb_RangedResponseHasContentLength(t *testing.T) {
	ts, _ := newTestServer(t)

	payloads := [][]byte{[]byte("chunk one"), []byte("chunk two, a bit longer")}
	blob, xorbHash, _ := buildXorb(t, payloads)

	resp, err := http.Post(ts.URL+"/v1/xorbs/default/"+xorbHash.Hex(), "application/octet-stream", bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("POST xorb error = %v", err)
	}
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/xorbs/default/"+xorbHash.Hex(), nil)
	req.Header.Set("Range", "bytes=0-4")
	rangeResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("ranged GET error = %v", err)
	}
	defer rangeResp.Body.Close()

	if rangeResp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rangeResp.StatusCode)
	}
	got := rangeResp.Header.Get("Content-Length")
	if got == "" {
		t.Fatal("Content-Length missing on a 206 response — WriteHeader must be called after, not before, setting it")
	}
	if want := strconv.Itoa(5); got != want {
		t.Errorf("Content-Length = %q, want %q", got, want)
	}
}

func TestHeadXorb_WrongPrefixReturns400(t *testing.T) {
	ts, _ := newTestServer(t)

	payloads := [][]byte{[]byte("chunk one")}
	blob, xorbHash, _ := buildXorb(t, payloads)
	resp, err := http.Post(ts.URL+"/v1/xorbs/default/"+xorbHash.Hex(), "application/octet-stream", bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("POST xorb error = %v", err)
	}
	resp.Body.Close()

	headResp, err := http.Head(ts.URL + "/v1/xorbs/bogus-prefix/" + xorbHash.Hex())
	if err != nil {
		t.Fatalf("HEAD error = %v", err)
	}
	defer headResp.Body.Close()
	if headResp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (same prefix validation GET already enforces)", headResp.StatusCode)
	}
}
