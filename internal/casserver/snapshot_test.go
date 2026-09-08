package casserver

// Tests for Server's snapshot/restore persistence: a fresh Server
// restored from a snapshot must serve requests identically to the
// original that took the snapshot, and the on-disk snapshot file must be
// safe from a concurrent partial-write observation.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"xet-server/internal/merklehash"
	"xet-server/internal/shardformat"
	"xet-server/internal/storage/fsstore"
)

func TestSnapshot_RestoresFileReconAndXorbState(t *testing.T) {
	dataDir := t.TempDir()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatalf("fsstore.New() error = %v", err)
	}
	srv := New(store)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	payloads := [][]byte{[]byte("chunk one"), []byte("chunk two, a bit longer")}
	xorbBlob, xorbHash, chunkHashes := buildXorb(t, payloads)
	resp, err := http.Post(ts.URL+"/v1/xorbs/default/"+xorbHash.Hex(), "application/octet-stream", bytes.NewReader(xorbBlob))
	if err != nil {
		t.Fatalf("upload xorb error = %v", err)
	}
	resp.Body.Close()

	fileHash := merklehash.ComputeDataHash([]byte("snapshot test file"))
	xorbEntry := shardformat.XorbEntry{
		Header: shardformat.XorbChunkSequenceHeader{XorbHash: xorbHash, NumEntries: uint32(len(chunkHashes))},
	}
	var totalUnpacked int
	for i, ch := range chunkHashes {
		xorbEntry.Chunks = append(xorbEntry.Chunks, shardformat.XorbChunkSequenceEntry{ChunkHash: ch, UnpackedSegmentBytes: uint32(len(payloads[i]))})
		totalUnpacked += len(payloads[i])
	}
	fileEntry := shardformat.FileEntry{
		Header:      shardformat.FileDataSequenceHeader{FileHash: fileHash, NumEntries: 1},
		Entries:     []shardformat.FileDataSequenceEntry{{XorbHash: xorbHash, UnpackedSegmentBytes: uint32(totalUnpacked), ChunkIndexStart: 0, ChunkIndexEnd: uint32(len(chunkHashes))}},
		MetadataExt: &shardformat.FileMetadataExt{SHA256: fileHash}, // reuse fileHash as a stand-in SHA-256 for this test
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

	// Sanity: reconstruction must work on the original server before we
	// even attempt the snapshot round-trip.
	origRecon, err := http.Get(ts.URL + "/v1/reconstructions/" + fileHash.Hex())
	if err != nil {
		t.Fatalf("original reconstruction GET error = %v", err)
	}
	var origResp reconstructionResponseV1
	if err := json.NewDecoder(origRecon.Body).Decode(&origResp); err != nil {
		t.Fatalf("decode original reconstruction error = %v", err)
	}
	origRecon.Body.Close()
	if origRecon.StatusCode != http.StatusOK {
		t.Fatalf("original reconstruction status = %d", origRecon.StatusCode)
	}

	snapPath := filepath.Join(t.TempDir(), "snapshot.json")
	if err := srv.Snapshot(snapPath); err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}

	// A fresh server, same storage backend (the xorb bytes are already on
	// disk via store — only the in-memory indices need restoring).
	restored := New(store)
	if err := restored.LoadSnapshot(snapPath); err != nil {
		t.Fatalf("LoadSnapshot() error = %v", err)
	}
	restoredTS := httptest.NewServer(restored)
	defer restoredTS.Close()

	restoredRecon, err := http.Get(restoredTS.URL + "/v1/reconstructions/" + fileHash.Hex())
	if err != nil {
		t.Fatalf("restored reconstruction GET error = %v", err)
	}
	defer restoredRecon.Body.Close()
	var restoredResp reconstructionResponseV1
	if err := json.NewDecoder(restoredRecon.Body).Decode(&restoredResp); err != nil {
		t.Fatalf("decode restored reconstruction error = %v", err)
	}
	if restoredRecon.StatusCode != http.StatusOK {
		t.Fatalf("restored reconstruction status = %d", restoredRecon.StatusCode)
	}
	// Compare everything except the fetch URL's host:port, which
	// legitimately differs between the two httptest.Server instances —
	// same underlying data either way.
	if restoredResp.OffsetIntoFirstRange != origResp.OffsetIntoFirstRange {
		t.Errorf("restored OffsetIntoFirstRange = %d, want %d", restoredResp.OffsetIntoFirstRange, origResp.OffsetIntoFirstRange)
	}
	if len(restoredResp.Terms) != len(origResp.Terms) {
		t.Fatalf("restored has %d terms, want %d", len(restoredResp.Terms), len(origResp.Terms))
	}
	for i := range origResp.Terms {
		if restoredResp.Terms[i] != origResp.Terms[i] {
			t.Errorf("restored term[%d] = %+v, want %+v", i, restoredResp.Terms[i], origResp.Terms[i])
		}
	}
	for xorbHex, origFetches := range origResp.FetchInfo {
		restoredFetches, ok := restoredResp.FetchInfo[xorbHex]
		if !ok {
			t.Errorf("restored FetchInfo missing xorb %s", xorbHex)
			continue
		}
		if len(restoredFetches) != len(origFetches) {
			t.Errorf("restored FetchInfo[%s] has %d entries, want %d", xorbHex, len(restoredFetches), len(origFetches))
			continue
		}
		for i := range origFetches {
			if restoredFetches[i].URLRange != origFetches[i].URLRange || restoredFetches[i].Range != origFetches[i].Range {
				t.Errorf("restored FetchInfo[%s][%d] = %+v, want %+v (ignoring URL host:port)", xorbHex, i, restoredFetches[i], origFetches[i])
			}
		}
	}

	// The restored server must also be able to fetch the actual xorb
	// bytes (proving the storage backend + restored xorbRawLength/footer
	// state line up correctly).
	fetchResp, err := http.Get(restoredTS.URL + "/v1/xorbs/default/" + xorbHash.Hex())
	if err != nil {
		t.Fatalf("restored xorb fetch error = %v", err)
	}
	defer fetchResp.Body.Close()
	fetched, _ := io.ReadAll(fetchResp.Body)
	if !bytes.Equal(fetched, xorbBlob) {
		t.Error("restored server's xorb fetch returned different bytes than the original upload")
	}

	// SHA256->Xet and chunk-dedup indices must also survive.
	if xetHash, ok := restored.XetHashForSHA256(fileHash.Hex()); !ok || xetHash != fileHash {
		t.Errorf("restored XetHashForSHA256 = (%v, %v), want (%v, true)", xetHash, ok, fileHash)
	}
	dedupResp, err := http.Get(restoredTS.URL + "/v1/chunks/default-merkledb/" + chunkHashes[0].Hex())
	if err != nil {
		t.Fatalf("restored chunk-dedup GET error = %v", err)
	}
	defer dedupResp.Body.Close()
	if dedupResp.StatusCode != http.StatusOK {
		t.Errorf("restored chunk-dedup status = %d, want 200 (index must survive snapshot/restore)", dedupResp.StatusCode)
	}
}

func TestSnapshot_MissingFileIsNotAnError(t *testing.T) {
	store, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("fsstore.New() error = %v", err)
	}
	srv := New(store)

	err = srv.LoadSnapshot(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Errorf("LoadSnapshot() error = %v, want nil for a missing snapshot file (fresh server, no prior snapshot)", err)
	}
}

func TestSnapshot_RejectsIncompatibleVersion(t *testing.T) {
	store, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("fsstore.New() error = %v", err)
	}
	srv := New(store)

	path := filepath.Join(t.TempDir(), "snapshot.json")
	if err := os.WriteFile(path, []byte(`{"version": 99999}`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if err := srv.LoadSnapshot(path); err == nil {
		t.Error("LoadSnapshot() error = nil, want an error for an incompatible snapshot version")
	}
}

func TestSnapshot_NoTempFileLeftBehindOnSuccess(t *testing.T) {
	store, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("fsstore.New() error = %v", err)
	}
	srv := New(store)

	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")
	if err := srv.Snapshot(path); err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}

	matches, _ := filepath.Glob(filepath.Join(dir, "*.tmp-*"))
	if len(matches) != 0 {
		t.Errorf("found leftover temp files after successful Snapshot(): %v", matches)
	}
}

func TestSnapshot_RestoredEvictionCandidatesHaveBackfilledLastAccess(t *testing.T) {
	store, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("fsstore.New() error = %v", err)
	}
	srv := New(store)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	blob, xorbHash, _ := buildXorb(t, [][]byte{[]byte("eviction candidate test content")})
	resp, err := http.Post(ts.URL+"/v1/xorbs/default/"+xorbHash.Hex(), "application/octet-stream", bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("upload xorb error = %v", err)
	}
	resp.Body.Close()

	path := filepath.Join(t.TempDir(), "snapshot.json")
	if err := srv.Snapshot(path); err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}

	restored := New(store)
	if err := restored.LoadSnapshot(path); err != nil {
		t.Fatalf("LoadSnapshot() error = %v", err)
	}

	candidates := restored.EvictionCandidates()
	found := false
	for _, c := range candidates {
		if c.Key == xorbHash.Hex() {
			found = true
			if !c.LastAccess.IsZero() {
				t.Errorf("restored xorb's LastAccess = %v, want zero-value (backfilled, no real access recorded yet)", c.LastAccess)
			}
		}
	}
	if !found {
		t.Error("restored xorb missing from EvictionCandidates() — xorbRawLength restored without a corresponding xorbLastAccess entry")
	}
}
