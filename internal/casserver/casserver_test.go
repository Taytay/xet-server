package casserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"xet-server/internal/merklehash"
	"xet-server/internal/shardformat"
	"xet-server/internal/storage/fsstore"
	"xet-server/internal/xorbformat"
)

// buildXorb chunks payloads into an uncompressed xorb blob with NO footer
// (mirroring a real client's actual wire upload — see the comment on
// handleUploadXorb: xet-core sends xorbs without a footer and expects the
// server to reconstruct it from chunk headers) and returns the blob, its
// hash, and each chunk's hash.
func buildXorb(t *testing.T, payloads [][]byte) (blob []byte, xorbHash merklehash.Hash, chunkHashes []merklehash.Hash) {
	t.Helper()
	var buf bytes.Buffer

	var chunkEntries []merklehash.ChunkEntry

	for _, p := range payloads {
		header := xorbformat.ChunkHeader{
			CompressedLength:   uint32(len(p)),
			CompressionScheme:  xorbformat.CompressionNone,
			UncompressedLength: uint32(len(p)),
		}
		if err := xorbformat.WriteChunkHeader(&buf, header); err != nil {
			t.Fatalf("WriteChunkHeader() error = %v", err)
		}
		buf.Write(p)

		h := merklehash.ComputeDataHash(p)
		chunkHashes = append(chunkHashes, h)
		chunkEntries = append(chunkEntries, merklehash.ChunkEntry{Hash: h, Size: uint64(len(p))})
	}

	xorbHash = merklehash.XorbHash(chunkEntries)
	return buf.Bytes(), xorbHash, chunkHashes
}

func newTestServer(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	store, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("fsstore.New() error = %v", err)
	}
	casSrv := New(store)
	httpSrv := httptest.NewServer(casSrv)
	t.Cleanup(httpSrv.Close)
	return httpSrv, casSrv
}

func TestUploadXorb_Roundtrip(t *testing.T) {
	ts, _ := newTestServer(t)

	payloads := [][]byte{[]byte("chunk one"), []byte("chunk two, a bit longer")}
	blob, xorbHash, _ := buildXorb(t, payloads)

	resp, err := http.Post(ts.URL+"/v1/xorbs/default/"+xorbHash.Hex(), "application/octet-stream", bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("POST xorb error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST xorb status = %d, body = %s", resp.StatusCode, body)
	}
	var uploadResp uploadXorbResponse
	if err := json.NewDecoder(resp.Body).Decode(&uploadResp); err != nil {
		t.Fatalf("decode upload response error = %v", err)
	}
	if !uploadResp.WasInserted {
		t.Error("WasInserted = false on first upload, want true")
	}

	// Second upload of the identical xorb should report already-present.
	resp2, err := http.Post(ts.URL+"/v1/xorbs/default/"+xorbHash.Hex(), "application/octet-stream", bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("second POST xorb error = %v", err)
	}
	defer resp2.Body.Close()
	var uploadResp2 uploadXorbResponse
	json.NewDecoder(resp2.Body).Decode(&uploadResp2)
	if uploadResp2.WasInserted {
		t.Error("second upload WasInserted = true, want false (dedup)")
	}

	// Fetch the raw xorb bytes back and confirm they match exactly.
	getResp, err := http.Get(ts.URL + "/v1/xorbs/default/" + xorbHash.Hex())
	if err != nil {
		t.Fatalf("GET xorb error = %v", err)
	}
	defer getResp.Body.Close()
	got, _ := io.ReadAll(getResp.Body)
	if !bytes.Equal(got, blob) {
		t.Errorf("GET xorb returned %d bytes, want %d bytes matching upload", len(got), len(blob))
	}
}

func TestUploadXorb_RejectsHashMismatch(t *testing.T) {
	ts, _ := newTestServer(t)
	blob, _, _ := buildXorb(t, [][]byte{[]byte("some content")})

	wrongHash := merklehash.ComputeDataHash([]byte("not the real hash"))
	resp, err := http.Post(ts.URL+"/v1/xorbs/default/"+wrongHash.Hex(), "application/octet-stream", bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("POST xorb error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for hash mismatch", resp.StatusCode)
	}
}

func TestUploadXorb_RejectsMalformedBody(t *testing.T) {
	ts, _ := newTestServer(t)
	someHash := merklehash.ComputeDataHash([]byte("x"))
	resp, err := http.Post(ts.URL+"/v1/xorbs/default/"+someHash.Hex(), "application/octet-stream", bytes.NewReader([]byte("garbage not a xorb")))
	if err != nil {
		t.Fatalf("POST xorb error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for malformed body", resp.StatusCode)
	}
}

// TestFullFileRoundtrip drives the complete real-client-shaped flow:
// chunk → xorb → shard → upload both → GET reconstruction → GET xorb bytes
// per term → reassemble → compare to the original file content. This is
// the same sequence hf_xet performs, just constructed by hand here instead
// of by the real client.
func TestFullFileRoundtrip(t *testing.T) {
	ts, _ := newTestServer(t)

	payloads := [][]byte{
		[]byte("the quick brown fox "),
		[]byte("jumps over the lazy dog "),
		[]byte("and keeps running"),
	}
	fileContent := bytes.Join(payloads, nil)

	xorbBlob, xorbHash, chunkHashes := buildXorb(t, payloads)

	resp, err := http.Post(ts.URL+"/v1/xorbs/default/"+xorbHash.Hex(), "application/octet-stream", bytes.NewReader(xorbBlob))
	if err != nil {
		t.Fatalf("upload xorb error = %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload xorb status = %d", resp.StatusCode)
	}

	var chunkEntries []merklehash.ChunkEntry
	for i, h := range chunkHashes {
		chunkEntries = append(chunkEntries, merklehash.ChunkEntry{Hash: h, Size: uint64(len(payloads[i]))})
	}
	fileHash := merklehash.FileHash(chunkEntries)

	xorbEntry := shardformat.XorbEntry{
		Header: shardformat.XorbChunkSequenceHeader{
			XorbHash:       xorbHash,
			NumEntries:     uint32(len(payloads)),
			NumBytesInXorb: uint32(len(fileContent)),
		},
	}
	var rangeStart uint32
	for i, h := range chunkHashes {
		xorbEntry.Chunks = append(xorbEntry.Chunks, shardformat.XorbChunkSequenceEntry{
			ChunkHash:            h,
			ChunkByteRangeStart:  rangeStart,
			UnpackedSegmentBytes: uint32(len(payloads[i])),
		})
		rangeStart += uint32(len(payloads[i]))
	}

	fileEntry := shardformat.FileEntry{
		Header: shardformat.FileDataSequenceHeader{FileHash: fileHash, NumEntries: 1},
		Entries: []shardformat.FileDataSequenceEntry{
			{
				XorbHash:             xorbHash,
				UnpackedSegmentBytes: uint32(len(fileContent)),
				ChunkIndexStart:      0,
				ChunkIndexEnd:        uint32(len(payloads)),
			},
		},
	}

	var shardBuf bytes.Buffer
	if _, err := shardformat.WriteShard(&shardBuf, []shardformat.FileEntry{fileEntry}, []shardformat.XorbEntry{xorbEntry}); err != nil {
		t.Fatalf("WriteShard() error = %v", err)
	}

	shardResp, err := http.Post(ts.URL+"/v1/shards", "application/octet-stream", &shardBuf)
	if err != nil {
		t.Fatalf("upload shard error = %v", err)
	}
	defer shardResp.Body.Close()
	if shardResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(shardResp.Body)
		t.Fatalf("upload shard status = %d, body = %s", shardResp.StatusCode, body)
	}

	reconResp, err := http.Get(ts.URL + "/v1/reconstructions/" + fileHash.Hex())
	if err != nil {
		t.Fatalf("GET reconstruction error = %v", err)
	}
	defer reconResp.Body.Close()
	if reconResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(reconResp.Body)
		t.Fatalf("GET reconstruction status = %d, body = %s", reconResp.StatusCode, body)
	}
	var recon reconstructionResponseV1
	if err := json.NewDecoder(reconResp.Body).Decode(&recon); err != nil {
		t.Fatalf("decode reconstruction error = %v", err)
	}
	if len(recon.Terms) != 1 {
		t.Fatalf("got %d terms, want 1", len(recon.Terms))
	}

	// Reassemble the file by following each term's fetch_info URL + range,
	// exactly as a real client would.
	var reassembled bytes.Buffer
	for _, term := range recon.Terms {
		fetchEntries := recon.FetchInfo[term.Hash]
		if len(fetchEntries) == 0 {
			t.Fatalf("no fetch_info for xorb %s", term.Hash)
		}
		fi := fetchEntries[0]

		req, _ := http.NewRequest(http.MethodGet, fi.URL, nil)
		req.Header.Set("Range", "bytes="+strconv.FormatInt(fi.URLRange.Start, 10)+"-"+strconv.FormatInt(fi.URLRange.End, 10))
		rangeResp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("fetch xorb range error = %v", err)
		}
		rawChunkBytes, err := io.ReadAll(rangeResp.Body)
		rangeResp.Body.Close()
		if err != nil {
			t.Fatalf("read xorb range body error = %v", err)
		}

		// The fetched range covers full chunk records (header+payload); since
		// this test's chunks are uncompressed, decode them back to raw bytes.
		decodedData, _, err := decodeUncompressedChunks(rawChunkBytes)
		if err != nil {
			t.Fatalf("decode chunk range error = %v", err)
		}
		reassembled.Write(decodedData)
	}

	if !bytes.Equal(reassembled.Bytes(), fileContent) {
		t.Errorf("reassembled file = %q, want %q", reassembled.Bytes(), fileContent)
	}
}

// TestXetHashForSHA256_UsesHexEncoding is a regression test for a bug
// found by driving a real `hf upload`/`hf download` round-trip through
// this server: real hf_xet clients write FileMetadataExt.SHA256 through
// the same byte-order transform as a genuine Merkle hash's Hex() (word
// reversal per 8-byte little-endian group), not as the plain SHA-256's
// raw bytes. Indexing sha256ToXet with a raw hex encode
// (hex.EncodeToString(SHA256.Bytes())) therefore produced a key that
// never matched the plain SHA-256 huggingface_hub sends as the commit
// payload's lfsFile.oid, so XetHashForSHA256 always missed and every
// `hf download` 404'd. Confirmed against a real captured upload: Hex()
// of the raw MetadataExt.SHA256 bytes equals the file's actual SHA-256
// exactly; a raw hex encode does not.
func TestXetHashForSHA256_UsesHexEncoding(t *testing.T) {
	ts, srv := newTestServer(t)

	payloads := [][]byte{[]byte("metadata ext regression test content")}
	xorbBlob, xorbHash, chunkHashes := buildXorb(t, payloads)

	resp, err := http.Post(ts.URL+"/v1/xorbs/default/"+xorbHash.Hex(), "application/octet-stream", bytes.NewReader(xorbBlob))
	if err != nil {
		t.Fatalf("upload xorb error = %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload xorb status = %d", resp.StatusCode)
	}

	var chunkEntries []merklehash.ChunkEntry
	for i, h := range chunkHashes {
		chunkEntries = append(chunkEntries, merklehash.ChunkEntry{Hash: h, Size: uint64(len(payloads[i]))})
	}
	fileHash := merklehash.FileHash(chunkEntries)

	// A real SHA-256 (of arbitrary content, unrelated to the xorb/chunk
	// hashes above — this field is deliberately a different hash space).
	// Real hf_xet clients write this field's wire bytes such that Hex()
	// of those bytes equals the plain SHA-256's normal hex string — build
	// it the same way via FromHex, rather than a raw byte copy, to mirror
	// what actually appears on the wire.
	plainSHA256 := sha256.Sum256([]byte("plain sha256 of the uploaded file"))
	plainSHA256Hex := hex.EncodeToString(plainSHA256[:])
	metadataExtHash, err := merklehash.FromHex(plainSHA256Hex)
	if err != nil {
		t.Fatalf("FromHex() error = %v", err)
	}

	fileEntry := shardformat.FileEntry{
		Header: shardformat.FileDataSequenceHeader{FileHash: fileHash, NumEntries: 1},
		Entries: []shardformat.FileDataSequenceEntry{
			{
				XorbHash:             xorbHash,
				UnpackedSegmentBytes: uint32(len(payloads[0])),
				ChunkIndexStart:      0,
				ChunkIndexEnd:        uint32(len(payloads)),
			},
		},
		MetadataExt: &shardformat.FileMetadataExt{SHA256: metadataExtHash},
	}

	var shardBuf bytes.Buffer
	if _, err := shardformat.WriteShard(&shardBuf, []shardformat.FileEntry{fileEntry}, nil); err != nil {
		t.Fatalf("WriteShard() error = %v", err)
	}

	shardResp, err := http.Post(ts.URL+"/v1/shards", "application/octet-stream", &shardBuf)
	if err != nil {
		t.Fatalf("upload shard error = %v", err)
	}
	defer shardResp.Body.Close()
	if shardResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(shardResp.Body)
		t.Fatalf("upload shard status = %d, body = %s", shardResp.StatusCode, body)
	}

	// The lookup key a real Hub API shim uses: the plain SHA-256's normal
	// lowercase hex (what huggingface_hub sends as lfsFile.oid), not the
	// raw wire bytes' hex encode.
	got, ok := srv.XetHashForSHA256(plainSHA256Hex)
	if !ok {
		t.Fatalf("XetHashForSHA256(%s) not found; sha256ToXet was indexed under the wrong key", plainSHA256Hex)
	}
	if got != fileHash {
		t.Errorf("XetHashForSHA256(%s) = %s, want %s", plainSHA256Hex, got.Hex(), fileHash.Hex())
	}
}

// decodeUncompressedChunks walks a run of xorb chunk records (header +
// uncompressed payload, no footer) and concatenates their payloads. Only
// handles CompressionNone, matching what this test file's fixtures write.
func decodeUncompressedChunks(data []byte) ([]byte, int, error) {
	var out bytes.Buffer
	pos := 0
	for pos < len(data) {
		header, err := xorbformat.ReadChunkHeader(bytes.NewReader(data[pos : pos+xorbformat.ChunkHeaderSize]))
		if err != nil {
			return nil, pos, err
		}
		pos += xorbformat.ChunkHeaderSize
		out.Write(data[pos : pos+int(header.CompressedLength)])
		pos += int(header.CompressedLength)
	}
	return out.Bytes(), pos, nil
}

func TestReconstruction_UnknownFileID(t *testing.T) {
	ts, _ := newTestServer(t)
	unknown := merklehash.ComputeDataHash([]byte("never uploaded"))
	resp, err := http.Get(ts.URL + "/v1/reconstructions/" + unknown.Hex())
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestReconstructionV2_SignalsFallbackToV1(t *testing.T) {
	ts, _ := newTestServer(t)
	someHash := merklehash.ComputeDataHash([]byte("x"))
	resp, err := http.Get(ts.URL + "/v2/reconstructions/" + someHash.Hex())
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 (fall back to V1)", resp.StatusCode)
	}
}

func TestChunkDedup_AlwaysNotFound(t *testing.T) {
	ts, _ := newTestServer(t)
	someHash := merklehash.ComputeDataHash([]byte("x"))
	resp, err := http.Get(ts.URL + "/v1/chunks/default-merkledb/" + someHash.Hex())
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (no global dedup index)", resp.StatusCode)
	}
}

func TestTelemetry_AlwaysAccepted(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Post(ts.URL+"/v1/telemetry", "application/json", bytes.NewReader([]byte(`{"event":"test"}`)))
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestUploadXorb_RealHFXetCapture replays an actual xorb upload captured
// from a live hf_xet client session (proxy-captured while diagnosing why
// this server originally rejected every real upload: it wrongly expected a
// client-supplied footer, which real xet-core never sends). The captured
// bytes are LZ4-compressed (highly repetitive input text), so this test
// exercises the real decompress-and-rehash path end-to-end, not just the
// CompressionNone path the other tests use.
func TestUploadXorb_RealHFXetCapture(t *testing.T) {
	ts, _ := newTestServer(t)

	body, err := os.ReadFile("testdata/real_upload_lz4.bin")
	if err != nil {
		t.Fatalf("read testdata error = %v", err)
	}
	// The hash hf_xet declared in its upload URL when this was captured.
	const claimedHash = "93c0d0f39f510203216b20b0ad1c1279e49601d47ea887fd3cd473ce00ed5cdb"

	resp, err := http.Post(ts.URL+"/v1/xorbs/default/"+claimedHash, "application/octet-stream", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST xorb error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST xorb status = %d, body = %s", resp.StatusCode, respBody)
	}

	var uploadResp uploadXorbResponse
	if err := json.NewDecoder(resp.Body).Decode(&uploadResp); err != nil {
		t.Fatalf("decode upload response error = %v", err)
	}
	if !uploadResp.WasInserted {
		t.Error("WasInserted = false, want true for first upload")
	}
}

// TestReconstruction_RangePastEOFReturns416 exercises the exact client
// contract this server originally violated: hf_xet pages through large
// files by re-requesting /v1/reconstructions/{file_id} with successively
// higher Range windows, and treats a Range whose start is >= file size as
// the signal to stop (mapped from 416 Range Not Satisfiable). Returning an
// empty 200 there instead — this server's original bug — left the client's
// sequential writer stuck waiting for a term that would never arrive.
func TestReconstruction_RangePastEOFReturns416(t *testing.T) {
	ts, _ := newTestServer(t)

	payloads := [][]byte{[]byte("small file content")}
	xorbBlob, xorbHash, chunkHashes := buildXorb(t, payloads)

	resp, err := http.Post(ts.URL+"/v1/xorbs/default/"+xorbHash.Hex(), "application/octet-stream", bytes.NewReader(xorbBlob))
	if err != nil {
		t.Fatalf("upload xorb error = %v", err)
	}
	resp.Body.Close()

	var chunkEntries []merklehash.ChunkEntry
	chunkEntries = append(chunkEntries, merklehash.ChunkEntry{Hash: chunkHashes[0], Size: uint64(len(payloads[0]))})
	fileHash := merklehash.FileHash(chunkEntries)

	xorbEntry := shardformat.XorbEntry{
		Header: shardformat.XorbChunkSequenceHeader{XorbHash: xorbHash, NumEntries: 1, NumBytesInXorb: uint32(len(payloads[0]))},
		Chunks: []shardformat.XorbChunkSequenceEntry{{ChunkHash: chunkHashes[0], UnpackedSegmentBytes: uint32(len(payloads[0]))}},
	}
	fileEntry := shardformat.FileEntry{
		Header: shardformat.FileDataSequenceHeader{FileHash: fileHash, NumEntries: 1},
		Entries: []shardformat.FileDataSequenceEntry{
			{XorbHash: xorbHash, UnpackedSegmentBytes: uint32(len(payloads[0])), ChunkIndexStart: 0, ChunkIndexEnd: 1},
		},
	}
	var shardBuf bytes.Buffer
	if _, err := shardformat.WriteShard(&shardBuf, []shardformat.FileEntry{fileEntry}, []shardformat.XorbEntry{xorbEntry}); err != nil {
		t.Fatalf("WriteShard() error = %v", err)
	}
	shardResp, err := http.Post(ts.URL+"/v1/shards", "application/octet-stream", &shardBuf)
	if err != nil {
		t.Fatalf("upload shard error = %v", err)
	}
	shardResp.Body.Close()

	// A Range starting exactly at (or past) the file's total size must 416.
	fileSize := len(payloads[0])
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/reconstructions/"+fileHash.Hex(), nil)
	req.Header.Set("Range", "bytes="+strconv.Itoa(fileSize)+"-"+strconv.Itoa(fileSize+100))
	rangeResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET with Range error = %v", err)
	}
	defer rangeResp.Body.Close()
	if rangeResp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("status = %d, want 416 for a Range starting at/past EOF", rangeResp.StatusCode)
	}

	// A Range within bounds must still succeed normally.
	req2, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/reconstructions/"+fileHash.Hex(), nil)
	req2.Header.Set("Range", "bytes=0-"+strconv.Itoa(fileSize-1))
	inBoundsResp, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("GET with in-bounds Range error = %v", err)
	}
	defer inBoundsResp.Body.Close()
	if inBoundsResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(inBoundsResp.Body)
		t.Errorf("status = %d, want 200 for an in-bounds Range; body = %s", inBoundsResp.StatusCode, body)
	}
}
