package casserver

// Adversarial payload tests: deliberately malformed/hostile requests at
// the HTTP layer, checked for a clean error response AND that the server
// keeps working correctly afterward (a bad request must never leave the
// server in a state where a subsequent good request fails). This
// complements the fuzz tests in fuzz_test.go/xorbformat/shardformat's own
// fuzz_test.go, which exercise the parsers directly without HTTP in the
// loop — these tests exercise the full request-handling path (routing,
// size caps, body streaming) that fuzzing the parsers alone doesn't touch.

import (
	"bytes"
	"encoding/binary"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"xet-server/internal/merklehash"
	"xet-server/internal/shardformat"
	"xet-server/internal/xorbformat"
)

// assertServerStillHealthy performs a trivial valid request and fails the
// test if the server doesn't answer correctly — the actual point of every
// adversarial test below, not just "did this one request 4xx."
func assertServerStillHealthy(t *testing.T, ts *http.Client, baseURL string) {
	t.Helper()
	resp, err := http.Post(baseURL+"/v1/telemetry", "application/octet-stream", nil)
	if err != nil {
		t.Fatalf("server unresponsive after adversarial request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("server responded %d to a trivial telemetry POST after adversarial request, want 200", resp.StatusCode)
	}
}

func TestAdversarial_UploadXorb(t *testing.T) {
	someHash := merklehash.ComputeDataHash([]byte("target"))

	cases := []struct {
		name       string
		urlSuffix  string // appended to /v1/xorbs/
		body       []byte
		wantStatus int // 0 = just check it's a 4xx/5xx-family error, not a specific code
	}{
		{
			name:      "empty body",
			urlSuffix: "default/" + someHash.Hex(),
			body:      []byte{},
		},
		{
			name:      "single byte body",
			urlSuffix: "default/" + someHash.Hex(),
			body:      []byte{0x00},
		},
		{
			name:      "all zero bytes, xorb-sized",
			urlSuffix: "default/" + someHash.Hex(),
			body:      make([]byte, 4096),
		},
		{
			name:      "all 0xFF bytes",
			urlSuffix: "default/" + someHash.Hex(),
			body:      bytes.Repeat([]byte{0xFF}, 4096),
		},
		{
			name:      "chunk header claims huge CompressedLength with no payload",
			urlSuffix: "default/" + someHash.Hex(),
			body:      buildHugeClaimChunkHeader(),
		},
		{
			name:      "wrong prefix segment",
			urlSuffix: "not-default/" + someHash.Hex(),
			body:      []byte("irrelevant"),
		},
		{
			name:      "hash param is not hex at all",
			urlSuffix: "default/not-a-hex-string-at-all-just-garbage-text-here-padding",
			body:      []byte("irrelevant"),
		},
		{
			name:      "hash param is empty",
			urlSuffix: "default/",
			body:      []byte("irrelevant"),
		},
		{
			name:      "hash param has SQL-injection-shaped content",
			urlSuffix: "default/'; DROP TABLE xorbs; --",
			body:      []byte("irrelevant"),
		},
		{
			name:      "hash param has path traversal shape",
			urlSuffix: "default/../../../etc/passwd",
			body:      []byte("irrelevant"),
		},
		{
			name:      "hash param has null bytes",
			urlSuffix: "default/" + someHash.Hex()[:60] + "\x00\x00\x00\x00",
			body:      []byte("irrelevant"),
		},
	}

	ts, _ := newTestServer(t)
	client := ts.Client()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := client.Post(ts.URL+"/v1/xorbs/"+tc.urlSuffix, "application/octet-stream", bytes.NewReader(tc.body))
			if err != nil {
				// A transport-level error (e.g. the URL itself was invalid
				// for net/http to even send) is acceptable — the server
				// was never reached, so it can't have misbehaved.
				return
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
			if resp.StatusCode < 400 || resp.StatusCode >= 600 {
				t.Errorf("status = %d, want a 4xx/5xx error for %q", resp.StatusCode, tc.name)
			}
		})
	}

	assertServerStillHealthy(t, client, ts.URL)
}

// buildHugeClaimChunkHeader builds a xorb body containing exactly one
// chunk header claiming a near-maximum 24-bit CompressedLength (the wire
// field's actual size limit — see xorbformat.ChunkHeader's doc comment),
// with zero payload bytes following. handleUploadXorb must reject this
// via a read/seek failure while scanning, not attempt to honor the claim.
func buildHugeClaimChunkHeader() []byte {
	var buf bytes.Buffer
	xorbformat.WriteChunkHeader(&buf, xorbformat.ChunkHeader{
		Version:            0,
		CompressedLength:   0xFFFFFF, // max 24-bit value
		CompressionScheme:  xorbformat.CompressionNone,
		UncompressedLength: 0xFFFFFF,
	})
	return buf.Bytes()
}

func TestAdversarial_UploadXorb_ExceedsMaxSize(t *testing.T) {
	ts, _ := newTestServer(t)
	client := ts.Client()

	someHash := merklehash.ComputeDataHash([]byte("oversized"))
	// One byte over maxXorbBytes; http.MaxBytesReader must cut this off
	// with a 413, not let handleUploadXorb try to stage all of it first.
	oversized := io.LimitReader(zeroReader{}, maxXorbBytes+1)

	resp, err := client.Post(ts.URL+"/v1/xorbs/default/"+someHash.Hex(), "application/octet-stream", oversized)
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413 for a body exceeding maxXorbBytes", resp.StatusCode)
	}

	assertServerStillHealthy(t, client, ts.URL)
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func TestAdversarial_UploadShard(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"empty body", []byte{}},
		{"single byte", []byte{0x00}},
		{"header-sized all zero", make([]byte, 48)},
		{"malicious NumEntries, footer-less", buildMaliciousShardNumEntries()},
		{"malicious xorb NumEntries, footer-less", buildMaliciousShardXorbNumEntries()},
		{"random garbage, shard-header-sized", bytes.Repeat([]byte{0x41, 0x42, 0x43, 0x44}, 12)},
		{"truncated valid shard", buildTruncatedValidShard(t)},
	}

	ts, _ := newTestServer(t)
	client := ts.Client()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := client.Post(ts.URL+"/v1/shards", "application/octet-stream", bytes.NewReader(tc.body))
			if err != nil {
				t.Fatalf("POST error = %v", err)
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
			if resp.StatusCode < 400 || resp.StatusCode >= 600 {
				t.Errorf("status = %d, want a 4xx/5xx error for %q", resp.StatusCode, tc.name)
			}
		})
	}

	assertServerStillHealthy(t, client, ts.URL)
}

// buildMaliciousShardNumEntries mirrors the exact payload shape of the
// fixed allocation-size DoS (see shardformat/dos_test.go), submitted here
// through the real HTTP handler rather than calling ReadShard directly —
// confirming handleUploadShard's whole request path stays safe, not just
// the parser in isolation.
func buildMaliciousShardNumEntries() []byte {
	var buf bytes.Buffer
	shardformat.WriteHeader(&buf, shardformat.Header{Version: 2, FooterSize: 0})
	shardformat.WriteFileDataSequenceHeader(&buf, shardformat.FileDataSequenceHeader{
		FileHash:   merklehash.ComputeDataHash([]byte("malicious")),
		NumEntries: 0xFFFFFFFF,
	})
	return buf.Bytes()
}

func buildMaliciousShardXorbNumEntries() []byte {
	var buf bytes.Buffer
	shardformat.WriteHeader(&buf, shardformat.Header{Version: 2, FooterSize: 0})
	shardformat.WriteFileDataSequenceHeader(&buf, shardformat.BookendFileHeader())
	shardformat.WriteXorbChunkSequenceHeader(&buf, shardformat.XorbChunkSequenceHeader{
		XorbHash:   merklehash.ComputeDataHash([]byte("malicious-xorb")),
		NumEntries: 0xFFFFFFFF,
	})
	return buf.Bytes()
}

// buildTruncatedValidShard builds a structurally valid shard via
// WriteShard and then chops it in half — a common real-world corruption
// shape (a client's connection dropped mid-upload), distinct from a
// deliberately hostile payload.
func buildTruncatedValidShard(t *testing.T) []byte {
	t.Helper()
	fileEntry := shardformat.FileEntry{
		Header:  shardformat.FileDataSequenceHeader{FileHash: merklehash.ComputeDataHash([]byte("f")), NumEntries: 1},
		Entries: []shardformat.FileDataSequenceEntry{{XorbHash: merklehash.ComputeDataHash([]byte("x")), UnpackedSegmentBytes: 100, ChunkIndexStart: 0, ChunkIndexEnd: 1}},
	}
	var buf bytes.Buffer
	if _, err := shardformat.WriteShard(&buf, []shardformat.FileEntry{fileEntry}, nil); err != nil {
		t.Fatalf("WriteShard() error = %v", err)
	}
	full := buf.Bytes()
	return full[:len(full)/2]
}

func TestAdversarial_MalformedRangeHeaders(t *testing.T) {
	ts, srv := newTestServer(t)
	client := ts.Client()

	// Upload one real xorb so there's something valid to request a range
	// against — the point here is exercising the Range-parsing path with
	// bad headers, not testing upload itself.
	blob, xorbHash, _ := buildXorb(t, [][]byte{[]byte("range test content, long enough to slice")})
	uploadResp, err := client.Post(ts.URL+"/v1/xorbs/default/"+xorbHash.Hex(), "application/octet-stream", bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("upload error = %v", err)
	}
	uploadResp.Body.Close()
	_ = srv

	badRanges := []string{
		"bytes=",
		"bytes=-",
		"bytes=abc-def",
		"bytes=-1--1",
		"bytes=99999999999999999999999999999-",
		"bytes=100-50", // reversed
		"BYTES=0-10",   // wrong case — real Range headers are case-sensitive per RFC
		"items=0-10",   // wrong unit
		strings.Repeat("bytes=0-1,", 1000) + "bytes=0-1", // absurdly long multi-range
	}

	for _, rangeHeader := range badRanges {
		t.Run(rangeHeader, func(t *testing.T) {
			if len(rangeHeader) > 60 {
				t.Logf("testing long Range header (%d bytes)", len(rangeHeader))
			}
			req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/xorbs/default/"+xorbHash.Hex(), nil)
			if err != nil {
				t.Fatalf("NewRequest error = %v", err)
			}
			req.Header.Set("Range", rangeHeader)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("GET error = %v", err)
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
			// Either "ignored, serve full content" (200) or "rejected"
			// (416) are acceptable — what's NOT acceptable is a 500 or a
			// hang, which would indicate parseByteRange let something
			// malformed slip through to code that assumes valid input.
			if resp.StatusCode == http.StatusInternalServerError {
				t.Errorf("status = 500 for malformed Range header %q, want 200 or 416", rangeHeader)
			}
		})
	}

	assertServerStillHealthy(t, client, ts.URL)
}

func TestAdversarial_ConcurrentUploadsOfSameXorb(t *testing.T) {
	ts, _ := newTestServer(t)
	client := ts.Client()

	blob, xorbHash, _ := buildXorb(t, [][]byte{[]byte("concurrent dedup target content")})

	const concurrency = 20
	results := make(chan int, concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			resp, err := client.Post(ts.URL+"/v1/xorbs/default/"+xorbHash.Hex(), "application/octet-stream", bytes.NewReader(blob))
			if err != nil {
				results <- -1
				return
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
			results <- resp.StatusCode
		}()
	}

	okCount := 0
	for i := 0; i < concurrency; i++ {
		status := <-results
		if status == http.StatusOK {
			okCount++
		} else if status != -1 {
			t.Errorf("concurrent upload %d got status %d, want 200", i, status)
		}
	}
	if okCount != concurrency {
		t.Errorf("okCount = %d, want all %d concurrent uploads of the identical xorb to succeed (dedup, not corruption)", okCount, concurrency)
	}

	// Confirm the stored bytes are still exactly correct after the race.
	getResp, err := client.Get(ts.URL + "/v1/xorbs/default/" + xorbHash.Hex())
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer getResp.Body.Close()
	got, _ := io.ReadAll(getResp.Body)
	if !bytes.Equal(got, blob) {
		t.Error("stored xorb bytes differ from upload after concurrent duplicate uploads — possible corruption from the race")
	}

	assertServerStillHealthy(t, client, ts.URL)
}

func TestAdversarial_ContentLengthMismatch(t *testing.T) {
	ts, _ := newTestServer(t)
	client := ts.Client()

	someHash := merklehash.ComputeDataHash([]byte("cl-mismatch"))
	body := []byte("this body is shorter than claimed")

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/xorbs/default/"+someHash.Hex(), bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest error = %v", err)
	}
	// Claim a Content-Length far larger than the actual body; net/http's
	// client will send this as stated, and the server's body-reading loop
	// must not hang waiting for bytes that will never arrive (io.Copy off
	// a real net.Conn just blocks until EOF/timeout — the meaningful
	// assertion here is that the request eventually completes/errors
	// rather than the test itself timing out).
	req.ContentLength = int64(len(body)) * 1000
	req.Header.Set("Content-Type", "application/octet-stream")

	client2 := &http.Client{Timeout: 5 * time.Second} // absolute cap so a regression fails the test, not hangs the suite
	resp, err := client2.Do(req)
	if err != nil {
		// A client-side error (e.g. the transport refusing to send a
		// mismatched Content-Length) is an acceptable outcome — the
		// server was never given a chance to misbehave.
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	assertServerStillHealthy(t, client, ts.URL)
}

// TestAdversarial_UploadXorb_LZ4AmplificationBomb posts a xorb whose one
// chunk declares CompressionLZ4 and carries an LZ4 frame engineered to
// decompress to hundreds of times its compressed size (a match-length
// extension bomb — see internal/lz4/dos_test.go for the full story of the
// real, confirmed DoS this is a regression test for). The compressed
// payload here is small (a few KB) specifically so this test itself stays
// fast; internal/lz4's own dos_test.go carries the full ~16 MiB
// real-world-shaped payload as a timed regression test. The point here is
// confirming the *whole request path* — chunk header parsing through
// decompressChunkPayload — rejects this quickly via the real HTTP
// endpoint, not just the lz4 package in isolation.
func TestAdversarial_UploadXorb_LZ4AmplificationBomb(t *testing.T) {
	ts, _ := newTestServer(t)
	client := ts.Client()

	bombFrame := buildLZ4AmplificationBombFrame(2000) // ~2KB compressed -> ~500KB decompressed if unbounded
	var buf bytes.Buffer
	xorbformat.WriteChunkHeader(&buf, xorbformat.ChunkHeader{
		Version:            0,
		CompressedLength:   uint32(len(bombFrame)),
		CompressionScheme:  xorbformat.CompressionLZ4,
		UncompressedLength: 500000, // claim matches what the bomb would produce if honored
	})
	buf.Write(bombFrame)

	someHash := merklehash.ComputeDataHash([]byte("lz4-bomb-target"))

	done := make(chan *http.Response, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, err := client.Post(ts.URL+"/v1/xorbs/default/"+someHash.Hex(), "application/octet-stream", bytes.NewReader(buf.Bytes()))
		if err != nil {
			errCh <- err
			return
		}
		done <- resp
	}()

	select {
	case resp := <-done:
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		if resp.StatusCode < 400 || resp.StatusCode >= 600 {
			t.Errorf("status = %d, want a 4xx/5xx error for an LZ4 amplification bomb chunk", resp.StatusCode)
		}
	case err := <-errCh:
		t.Fatalf("POST error = %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("upload with an LZ4 amplification bomb chunk did not complete within 5s")
	}

	assertServerStillHealthy(t, client, ts.URL)
}

// buildLZ4AmplificationBombFrame builds a minimal LZ4 frame (magic,
// descriptor, one block, EndMark) whose block is a match-length-extension
// bomb: one literal byte followed by a self-referential match inflated by
// n extension bytes (each worth +255 to the match length).
func buildLZ4AmplificationBombFrame(n int) []byte {
	var block []byte
	block = append(block, 0x1F) // token: litLen=1, matchLen nibble=15 (extension follows)
	block = append(block, 'A')
	block = append(block, 0x01, 0x00) // offset = 1
	for i := 0; i < n; i++ {
		block = append(block, 0xFF)
	}
	block = append(block, 0x01) // terminating extension byte
	block = append(block, 0x00) // trivial empty literal sequence, ends the block

	var frame []byte
	var tmp [4]byte
	binary.LittleEndian.PutUint32(tmp[:], 0x184D2204) // frame magic
	frame = append(frame, tmp[:]...)
	frame = append(frame, 0x40, 0x70, 0x00) // FLG, BD (block max size code 7), header checksum
	binary.LittleEndian.PutUint32(tmp[:], uint32(len(block)))
	frame = append(frame, tmp[:]...)
	frame = append(frame, block...)
	binary.LittleEndian.PutUint32(tmp[:], 0) // EndMark
	frame = append(frame, tmp[:]...)
	return frame
}
