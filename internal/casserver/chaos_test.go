package casserver

// Chaos/reliability tests: sustained concurrent load mixing valid and
// invalid traffic, interrupted uploads followed by clean retries, and
// upload/fetch/eviction interleaving - checked against actual data
// integrity (byte-identical round-trips), not just "the server didn't
// crash." These run against a real httptest.Server wrapping the genuine
// casserver.Server + fsstore.Store, so timing-sensitive races have a
// chance to actually manifest, unlike a single-goroutine unit test.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/guilt/xet-server/internal/eviction"
	"github.com/guilt/xet-server/internal/storage/fsstore"
)

// TestChaos_InterruptedUploadThenCleanRetry simulates a client whose
// connection drops mid-upload (server sees a short/canceled body read),
// then confirms a subsequent clean upload of the identical xorb succeeds
// and produces byte-correct stored content - proving fsstore.Put's
// atomic-rename staging (see internal/storage/fsstore/fsstore.go) leaves
// no corruption behind for a retry to inherit.
func TestChaos_InterruptedUploadThenCleanRetry(t *testing.T) {
	ts, _ := newTestServer(t)
	client := ts.Client()

	payload := bytes.Repeat([]byte("chaos test payload content "), 10000) // large enough for a mid-stream cut to matter
	blob, xorbHash, _ := buildXorb(t, [][]byte{payload})

	// Simulate an interrupted upload: a reader that errors partway
	// through, mimicking a dropped connection.
	interrupted := &interruptingReader{data: blob, cutAt: len(blob) / 2}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/xorbs/default/"+xorbHash.Hex(), interrupted)
	if err != nil {
		t.Fatalf("NewRequest error = %v", err)
	}
	req.ContentLength = int64(len(blob))
	resp, err := client.Do(req)
	if err == nil {
		// Some transports may still complete the request; if so it must
		// have failed server-side (the body was short of what
		// Content-Length promised).
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		if resp.StatusCode == http.StatusOK {
			t.Fatal("interrupted upload reported 200 OK, want a failure - the body was deliberately truncated")
		}
	}

	// Clean retry with the full, uninterrupted blob.
	retryResp, err := client.Post(ts.URL+"/v1/xorbs/default/"+xorbHash.Hex(), "application/octet-stream", bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("retry POST error = %v", err)
	}
	defer retryResp.Body.Close()
	retryBody, _ := io.ReadAll(retryResp.Body)
	if retryResp.StatusCode != http.StatusOK {
		t.Fatalf("retry status = %d, body = %s", retryResp.StatusCode, retryBody)
	}

	// The retry must show up as newly-written (the interrupted attempt
	// left no partial blob behind to be mistaken for a completed one).
	var uploadResp uploadXorbResponse
	if err := json.Unmarshal(retryBody, &uploadResp); err != nil {
		t.Fatalf("decode retry response error = %v", err)
	}
	if !uploadResp.WasInserted {
		t.Error("retry WasInserted = false, want true - the interrupted attempt should not have left a blob in place")
	}

	// Fetch it back and confirm byte-for-byte correctness.
	getResp, err := client.Get(ts.URL + "/v1/xorbs/default/" + xorbHash.Hex())
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer getResp.Body.Close()
	got, _ := io.ReadAll(getResp.Body)
	if !bytes.Equal(got, blob) {
		t.Errorf("fetched xorb differs from the clean retry's upload (%d bytes vs %d) - possible corruption from the interrupted attempt", len(got), len(blob))
	}
}

// interruptingReader reads normally up to cutAt bytes, then returns an
// error, simulating a client connection dropping mid-body.
type interruptingReader struct {
	data  []byte
	pos   int
	cutAt int
}

func (r *interruptingReader) Read(p []byte) (int, error) {
	if r.pos >= r.cutAt {
		return 0, fmt.Errorf("simulated connection drop")
	}
	remaining := r.cutAt - r.pos
	n := len(p)
	if n > remaining {
		n = remaining
	}
	copy(p, r.data[r.pos:r.pos+n])
	r.pos += n
	return n, nil
}

// TestChaos_SustainedMixedTraffic fires a sustained burst of concurrent
// requests where roughly half are valid xorb uploads (each distinct, so
// no dedup short-circuit hides a bug) and half are deliberately malformed
// - mirroring a noisy real-world environment where some clients retry
// broken requests while others succeed normally. Every valid upload's
// hash must be independently fetchable and byte-correct afterward,
// proving the malformed traffic never corrupts or drops unrelated valid
// state.
func TestChaos_SustainedMixedTraffic(t *testing.T) {
	ts, _ := newTestServer(t)
	client := ts.Client()

	const numValid = 30
	const numMalformed = 30

	type validUpload struct {
		hash string
		blob []byte
	}
	valid := make([]validUpload, numValid)
	for i := range valid {
		payload := []byte(fmt.Sprintf("distinct valid payload number %d, padded for size", i))
		blob, xorbHash, _ := buildXorb(t, [][]byte{payload})
		valid[i] = validUpload{hash: xorbHash.Hex(), blob: blob}
	}

	var wg sync.WaitGroup
	var validSucceeded atomic.Int64

	for i := 0; i < numValid; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := client.Post(ts.URL+"/v1/xorbs/default/"+valid[i].hash, "application/octet-stream", bytes.NewReader(valid[i].blob))
			if err != nil {
				return
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
			if resp.StatusCode == http.StatusOK {
				validSucceeded.Add(1)
			}
		}(i)
	}
	for i := 0; i < numMalformed; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			garbage := bytes.Repeat([]byte{byte(i)}, 100+i)
			fakeHash := fmt.Sprintf("%064x", i) // not a real hash, just needs to be 64 hex chars
			resp, err := client.Post(ts.URL+"/v1/xorbs/default/"+fakeHash, "application/octet-stream", bytes.NewReader(garbage))
			if err != nil {
				return
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
		}(i)
	}
	wg.Wait()

	if got := validSucceeded.Load(); got != numValid {
		t.Errorf("validSucceeded = %d, want all %d valid uploads to succeed despite concurrent malformed traffic", got, numValid)
	}

	// Every valid upload must be fetchable and byte-correct.
	for _, v := range valid {
		resp, err := client.Get(ts.URL + "/v1/xorbs/default/" + v.hash)
		if err != nil {
			t.Errorf("GET %s error = %v", v.hash, err)
			continue
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !bytes.Equal(got, v.blob) {
			t.Errorf("xorb %s: fetched content differs from upload (corruption under mixed load)", v.hash)
		}
	}
}

// TestChaos_UploadFetchEvictInterleaved runs uploads, fetches, and an
// eviction sweep concurrently against a tight storage budget, and asserts
// no deadlock (the whole test completes within a bounded time) and no
// data corruption for anything the sweep didn't evict.
func TestChaos_UploadFetchEvictInterleaved(t *testing.T) {
	store, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("fsstore.New() error = %v", err)
	}
	srv := New(store)
	sweeper := eviction.New(store, srv, 2000, 20*time.Millisecond) // tiny budget: forces real evictions
	srv.SetEvictionStats(sweeper.Stats)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sweeper.Run(ctx)

	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	client := ts.Client()

	const numXorbs = 15
	payloads := make([][]byte, numXorbs)
	hashes := make([]string, numXorbs)
	blobs := make([][]byte, numXorbs)
	for i := range payloads {
		payloads[i] = bytes.Repeat([]byte(fmt.Sprintf("xorb-%d-content ", i)), 20)
		blob, xorbHash, _ := buildXorb(t, [][]byte{payloads[i]})
		hashes[i] = xorbHash.Hex()
		blobs[i] = blob
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for round := 0; round < 3; round++ {
			for i := 0; i < numXorbs; i++ {
				wg.Add(2)
				go func(i int) {
					defer wg.Done()
					resp, err := client.Post(ts.URL+"/v1/xorbs/default/"+hashes[i], "application/octet-stream", bytes.NewReader(blobs[i]))
					if err == nil {
						io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
					}
				}(i)
				go func(i int) {
					defer wg.Done()
					resp, err := client.Get(ts.URL + "/v1/xorbs/default/" + hashes[i])
					if err == nil {
						io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
					}
				}(i)
			}
		}
		wg.Wait()
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("upload/fetch/evict interleaving did not complete within 30s - possible deadlock")
	}

	// Whatever survived eviction must still be byte-correct - eviction
	// removing a blob is fine, but a *present* blob must never be
	// corrupted by the concurrent sweep.
	for i, hash := range hashes {
		resp, err := client.Get(ts.URL + "/v1/xorbs/default/" + hash)
		if err != nil {
			t.Errorf("xorb %d: GET error = %v", i, err)
			continue
		}
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			continue // evicted; acceptable under a tiny budget
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && !bytes.Equal(got, blobs[i]) {
			t.Errorf("xorb %d: present but content differs from upload - corruption under concurrent eviction", i)
		}
	}

	stats := sweeper.Stats()
	t.Logf("eviction stats after chaos run: %+v", stats)
}
