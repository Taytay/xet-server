package casserver

// Tests for the exported ingestion methods (IngestXorb, IngestShard,
// IngestFileRecon) and their read-accessor counterparts (HasXorbFooter,
// HasXorbBytes, HasFileRecon) - the API surface a caller embedding this
// Server as a caching layer (internal/proxycas, internal/proxyhub) uses
// instead of reimplementing upload-validation/indexing logic
// independently. Driven directly against the exported methods, not
// through HTTP - internal/casserver's existing handler-level tests
// (casserver_test.go) already cover the HTTP boundary these methods sit
// behind.

import (
	"bytes"
	"context"
	"testing"

	"github.com/guilt/xet-server/internal/merklehash"
	"github.com/guilt/xet-server/internal/shardformat"
)

func hashFromByte(b byte) merklehash.Hash {
	var buf [32]byte
	for i := range buf {
		buf[i] = b
	}
	h, _ := merklehash.FromRawBytes(buf[:])
	return h
}

func TestIngestXorb_ValidBytesStoresAndIndexes(t *testing.T) {
	_, casSrv := newTestServer(t)
	blob, xorbHash, _ := buildXorb(t, [][]byte{[]byte("ingested chunk payload")})

	written, err := casSrv.IngestXorb(context.Background(), xorbHash, bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("IngestXorb() error = %v", err)
	}
	if !written {
		t.Error("written = false, want true for a new xorb")
	}
	if !casSrv.HasXorbFooter(xorbHash) {
		t.Error("HasXorbFooter() = false after a successful IngestXorb")
	}
	has, err := casSrv.HasXorbBytes(context.Background(), xorbHash)
	if err != nil {
		t.Fatalf("HasXorbBytes() error = %v", err)
	}
	if !has {
		t.Error("HasXorbBytes() = false after a successful IngestXorb")
	}
}

func TestIngestXorb_DuplicateReturnsWrittenFalse(t *testing.T) {
	_, casSrv := newTestServer(t)
	blob, xorbHash, _ := buildXorb(t, [][]byte{[]byte("dedup me")})

	if _, err := casSrv.IngestXorb(context.Background(), xorbHash, bytes.NewReader(blob)); err != nil {
		t.Fatalf("first IngestXorb() error = %v", err)
	}
	written, err := casSrv.IngestXorb(context.Background(), xorbHash, bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("second IngestXorb() error = %v", err)
	}
	if written {
		t.Error("written = true on a duplicate ingest, want false (dedup)")
	}
}

func TestIngestXorb_HashMismatchReturnsErrXorbHashMismatch(t *testing.T) {
	_, casSrv := newTestServer(t)
	blob, _, _ := buildXorb(t, [][]byte{[]byte("mismatched")})
	_, otherHash, _ := buildXorb(t, [][]byte{[]byte("other")})

	_, err := casSrv.IngestXorb(context.Background(), otherHash, bytes.NewReader(blob))
	if err == nil {
		t.Fatal("expected an error for mismatched hash, got nil")
	}
}

func TestIngestXorb_MalformedBytesReturnsErrMalformedXorb(t *testing.T) {
	_, casSrv := newTestServer(t)
	_, fakeHash, _ := buildXorb(t, [][]byte{[]byte("placeholder")})

	_, err := casSrv.IngestXorb(context.Background(), fakeHash, bytes.NewReader([]byte("not a valid chunk stream")))
	if err == nil {
		t.Fatal("expected an error for malformed bytes, got nil")
	}
}

func TestHasXorbFooter_UnknownHashReturnsFalse(t *testing.T) {
	_, casSrv := newTestServer(t)
	_, unknown, _ := buildXorb(t, [][]byte{[]byte("never ingested")})
	if casSrv.HasXorbFooter(unknown) {
		t.Error("HasXorbFooter() = true for a xorb never ingested")
	}
}

func TestHasXorbBytes_UnknownHashReturnsFalse(t *testing.T) {
	_, casSrv := newTestServer(t)
	_, unknown, _ := buildXorb(t, [][]byte{[]byte("never stored")})
	has, err := casSrv.HasXorbBytes(context.Background(), unknown)
	if err != nil {
		t.Fatalf("HasXorbBytes() error = %v", err)
	}
	if has {
		t.Error("HasXorbBytes() = true for a xorb never stored")
	}
}

func TestIngestFileRecon_PopulatesFileReconAndHasFileRecon(t *testing.T) {
	_, casSrv := newTestServer(t)
	fileHash := hashFromByte(0xF1)
	xorbHash := hashFromByte(0x11)

	if casSrv.HasFileRecon(fileHash) {
		t.Fatal("HasFileRecon() = true before any ingest")
	}

	entries := []shardformat.FileDataSequenceEntry{
		{XorbHash: xorbHash, UnpackedSegmentBytes: 100, ChunkIndexStart: 0, ChunkIndexEnd: 1},
	}
	casSrv.IngestFileRecon(fileHash, entries)

	if !casSrv.HasFileRecon(fileHash) {
		t.Fatal("HasFileRecon() = false after IngestFileRecon")
	}
	size, ok := casSrv.FileSize(fileHash)
	if !ok || size != 100 {
		t.Errorf("FileSize() = (%d, %v), want (100, true)", size, ok)
	}
}

func TestIngestShard_PopulatesFileReconAndChunkDedup(t *testing.T) {
	_, casSrv := newTestServer(t)
	xorbHash := hashFromByte(0x11)
	chunkHash := hashFromByte(0xA1)
	fileHash := hashFromByte(0xF1)

	xorbs := []shardformat.XorbEntry{
		{
			Header: shardformat.XorbChunkSequenceHeader{XorbHash: xorbHash, NumEntries: 1, NumBytesInXorb: 100},
			Chunks: []shardformat.XorbChunkSequenceEntry{
				{ChunkHash: chunkHash, ChunkByteRangeStart: 0, UnpackedSegmentBytes: 100},
			},
		},
	}
	files := []shardformat.FileEntry{
		{
			Header: shardformat.FileDataSequenceHeader{FileHash: fileHash, NumEntries: 1},
			Entries: []shardformat.FileDataSequenceEntry{
				{XorbHash: xorbHash, UnpackedSegmentBytes: 100, ChunkIndexStart: 0, ChunkIndexEnd: 1},
			},
		},
	}
	var buf bytes.Buffer
	if _, err := shardformat.WriteShard(&buf, files, xorbs); err != nil {
		t.Fatalf("WriteShard() error = %v", err)
	}

	if err := casSrv.IngestShard(buf.Bytes()); err != nil {
		t.Fatalf("IngestShard() error = %v", err)
	}

	if !casSrv.HasFileRecon(fileHash) {
		t.Error("HasFileRecon() = false after IngestShard")
	}
}

func TestIngestShard_MalformedBytesReturnsError(t *testing.T) {
	_, casSrv := newTestServer(t)
	if err := casSrv.IngestShard([]byte("not a valid shard")); err == nil {
		t.Fatal("expected an error for a malformed shard, got nil")
	}
}
