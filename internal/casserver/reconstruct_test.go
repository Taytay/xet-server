package casserver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/guilt/xet-server/internal/merklehash"
	"github.com/guilt/xet-server/internal/shardformat"
	"github.com/guilt/xet-server/internal/storage/fsstore"
)

// ingestTestFile builds one xorb from payloads, ingests it, and registers
// a file whose reconstruction is chunks [chunkStart, chunkEnd) of it.
func ingestTestFile(t *testing.T, srv *Server, payloads [][]byte, chunkStart, chunkEnd uint32) (fileHash merklehash.Hash, content []byte) {
	t.Helper()
	blob, xorbHash, _ := buildXorb(t, payloads)
	if _, err := srv.IngestXorb(context.Background(), xorbHash, bytes.NewReader(blob)); err != nil {
		t.Fatalf("IngestXorb() error = %v", err)
	}
	for _, p := range payloads[chunkStart:chunkEnd] {
		content = append(content, p...)
	}
	fileHash = merklehash.ComputeDataHash(content)
	srv.IngestFileRecon(fileHash, []shardformat.FileDataSequenceEntry{{
		XorbHash:             xorbHash,
		UnpackedSegmentBytes: uint32(len(content)),
		ChunkIndexStart:      chunkStart,
		ChunkIndexEnd:        chunkEnd,
	}})
	return fileHash, content
}

func newReconTestServer(t *testing.T) *Server {
	t.Helper()
	store, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("fsstore.New() error = %v", err)
	}
	return New(store)
}

func TestReconstructFile_WholeFileAcrossChunks(t *testing.T) {
	srv := newReconTestServer(t)
	payloads := [][]byte{bytes.Repeat([]byte("a"), 1000), bytes.Repeat([]byte("b"), 2000), bytes.Repeat([]byte("c"), 3000)}
	fileHash, want := ingestTestFile(t, srv, payloads, 0, 3)

	var got bytes.Buffer
	if err := srv.ReconstructFile(context.Background(), fileHash, 0, int64(len(want))-1, &got); err != nil {
		t.Fatalf("ReconstructFile() error = %v", err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("reconstructed %d bytes, want %d; content differs", got.Len(), len(want))
	}
}

func TestReconstructFile_SubsetOfXorbChunks(t *testing.T) {
	srv := newReconTestServer(t)
	payloads := [][]byte{[]byte("zero-not-in-file"), []byte("one"), []byte("two"), []byte("three-not-in-file")}
	fileHash, want := ingestTestFile(t, srv, payloads, 1, 3)

	var got bytes.Buffer
	if err := srv.ReconstructFile(context.Background(), fileHash, 0, int64(len(want))-1, &got); err != nil {
		t.Fatalf("ReconstructFile() error = %v", err)
	}
	if got.String() != "onetwo" {
		t.Fatalf("got %q, want onetwo", got.String())
	}
}

func TestReconstructFile_RangesClipMidChunk(t *testing.T) {
	srv := newReconTestServer(t)
	payloads := [][]byte{[]byte("0123456789"), []byte("abcdefghij"), []byte("ABCDEFGHIJ")}
	fileHash, want := ingestTestFile(t, srv, payloads, 0, 3)

	cases := []struct{ start, end int64 }{
		{0, 0}, {9, 10}, {5, 24}, {10, 19}, {29, 29}, {3, 3}, {0, 29}, {12, 100},
	}
	for _, c := range cases {
		var got bytes.Buffer
		err := srv.ReconstructFile(context.Background(), fileHash, c.start, c.end, &got)
		if err != nil {
			t.Fatalf("range %d-%d: error = %v", c.start, c.end, err)
		}
		end := c.end
		if end >= int64(len(want)) {
			end = int64(len(want)) - 1
		}
		if !bytes.Equal(got.Bytes(), want[c.start:end+1]) {
			t.Errorf("range %d-%d: got %q, want %q", c.start, c.end, got.String(), want[c.start:end+1])
		}
	}
}

func TestReconstructFile_MultiTermFile(t *testing.T) {
	srv := newReconTestServer(t)
	blobA, hashA, _ := buildXorb(t, [][]byte{[]byte("first-xorb-"), []byte("chunk-two-")})
	blobB, hashB, _ := buildXorb(t, [][]byte{[]byte("second-xorb")})
	for _, x := range []struct {
		h merklehash.Hash
		b []byte
	}{{hashA, blobA}, {hashB, blobB}} {
		if _, err := srv.IngestXorb(context.Background(), x.h, bytes.NewReader(x.b)); err != nil {
			t.Fatalf("IngestXorb() error = %v", err)
		}
	}
	want := []byte("first-xorb-chunk-two-second-xorbchunk-two-")
	fileHash := merklehash.ComputeDataHash(want)
	srv.IngestFileRecon(fileHash, []shardformat.FileDataSequenceEntry{
		{XorbHash: hashA, UnpackedSegmentBytes: 21, ChunkIndexStart: 0, ChunkIndexEnd: 2},
		{XorbHash: hashB, UnpackedSegmentBytes: 11, ChunkIndexStart: 0, ChunkIndexEnd: 1},
		{XorbHash: hashA, UnpackedSegmentBytes: 10, ChunkIndexStart: 1, ChunkIndexEnd: 2},
	})

	var got bytes.Buffer
	if err := srv.ReconstructFile(context.Background(), fileHash, 0, int64(len(want))-1, &got); err != nil {
		t.Fatalf("ReconstructFile() error = %v", err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("got %q, want %q", got.String(), want)
	}

	// A window entirely inside the second term reads only that xorb.
	got.Reset()
	if err := srv.ReconstructFile(context.Background(), fileHash, 21, 31, &got); err != nil {
		t.Fatalf("ReconstructFile(term 2) error = %v", err)
	}
	if got.String() != "second-xorb" {
		t.Fatalf("got %q, want second-xorb", got.String())
	}
}

func TestReconstructFile_Errors(t *testing.T) {
	srv := newReconTestServer(t)
	payloads := [][]byte{[]byte("0123456789")}
	fileHash, _ := ingestTestFile(t, srv, payloads, 0, 1)

	if err := srv.ReconstructFile(context.Background(), merklehash.ComputeDataHash([]byte("nope")), 0, 0, io.Discard); !errors.Is(err, ErrUnknownFile) {
		t.Errorf("unknown file: err = %v, want ErrUnknownFile", err)
	}
	for _, c := range []struct{ start, end int64 }{{10, 10}, {5, 4}, {-1, 3}} {
		if err := srv.ReconstructFile(context.Background(), fileHash, c.start, c.end, io.Discard); !errors.Is(err, ErrRangeNotSatisfiable) {
			t.Errorf("range %d-%d: err = %v, want ErrRangeNotSatisfiable", c.start, c.end, err)
		}
	}

	// A term whose declared byte count disagrees with its chunks is refused.
	_, xorbHash, _ := buildXorb(t, payloads)
	bad := merklehash.ComputeDataHash([]byte("bad"))
	srv.IngestFileRecon(bad, []shardformat.FileDataSequenceEntry{{XorbHash: xorbHash, UnpackedSegmentBytes: 99, ChunkIndexStart: 0, ChunkIndexEnd: 1}})
	if err := srv.ReconstructFile(context.Background(), bad, 0, 5, io.Discard); err == nil {
		t.Error("mismatched term byte count should fail")
	}

	// A term pointing past the xorb's chunk count is refused.
	oob := merklehash.ComputeDataHash([]byte("oob"))
	srv.IngestFileRecon(oob, []shardformat.FileDataSequenceEntry{{XorbHash: xorbHash, UnpackedSegmentBytes: 10, ChunkIndexStart: 0, ChunkIndexEnd: 5}})
	if err := srv.ReconstructFile(context.Background(), oob, 0, 5, io.Discard); err == nil {
		t.Error("out-of-range chunk index should fail")
	}
}

// Newer xet-core clients (git-xet 0.2.1, recent hf_xet) query global
// dedup under the "default" prefix and POST shards to an unversioned
// /shards; both must be served, not rejected (found by driving a real
// `git push` through git-xet).
func TestNewerClientPaths_DefaultDedupPrefixAndUnversionedShards(t *testing.T) {
	ts, _ := newTestServer(t)
	someHash := merklehash.ComputeDataHash([]byte("x"))
	for _, prefix := range []string{"default", "default-merkledb"} {
		resp, err := http.Get(ts.URL + "/v1/chunks/" + prefix + "/" + someHash.Hex())
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("prefix %q: status %d, want 404 (accepted prefix, unknown chunk)", prefix, resp.StatusCode)
		}
	}
	resp, err := http.Post(ts.URL+"/shards", "application/octet-stream", bytes.NewReader([]byte("not a shard")))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /shards: status %d, want 400 (route exists, body malformed)", resp.StatusCode)
	}
}
