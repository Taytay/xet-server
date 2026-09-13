package casserver

// snapshot_fuzz_test.go fuzzes the v2 snapshot round-trip. The v2 format
// change (see docs/PROTOCOL.md section 11.1) replaced a
// map[Hash][]byte chunk->shard-body index with an indirection through a
// content-addressed shardBodies table, specifically because the old shape
// re-serialized a multi-MB shard body once per referencing chunk and OOM'd
// the snapshot goroutine. That indirection is exactly the kind of change
// that can silently lose or mis-associate entries, and a snapshot that
// restores wrong is indistinguishable from a healthy server until someone
// tries to download something after a restart - so round-trip fidelity is
// worth asserting against arbitrary inputs, not just hand-picked ones.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/guilt/xet-server/internal/merklehash"
	"github.com/guilt/xet-server/internal/storage/fsstore"
)

// FuzzSnapshotRoundTrip builds a server state from fuzzer-chosen bytes,
// snapshots it, loads it into a fresh server, and asserts the chunk ->
// shard-body association survives exactly. It also pins the anti-blowup
// property the v2 format exists for: on-disk size must scale with the
// number of DISTINCT shard bodies, not with the number of chunks
// referencing them.
func FuzzSnapshotRoundTrip(f *testing.F) {
	f.Add([]byte("a"), []byte("b"), uint8(3))
	f.Add([]byte(""), []byte(""), uint8(0))
	f.Add([]byte("shard-one"), []byte("shard-two"), uint8(255))
	f.Add(make([]byte, 1024), make([]byte, 2048), uint8(64))

	f.Fuzz(func(t *testing.T, bodyA, bodyB []byte, nChunks uint8) {
		dir := t.TempDir()
		store, err := fsstore.New(filepath.Join(dir, "xorbs"))
		if err != nil {
			t.Fatalf("fsstore.New: %v", err)
		}
		srv := New(store)

		// Point many chunk hashes at two shard bodies, mimicking the real
		// shape: one body referenced by a large number of chunks.
		hashA := merklehash.ComputeDataHash(bodyA)
		hashB := merklehash.ComputeDataHash(bodyB)
		srv.shardBodies[hashA] = bodyA
		srv.shardBodies[hashB] = bodyB

		want := map[merklehash.Hash]merklehash.Hash{}
		for i := 0; i < int(nChunks); i++ {
			var ch merklehash.Hash
			ch[0] = byte(i)
			ch[1] = byte(i >> 8)
			target := hashA
			if i%2 == 1 {
				target = hashB
			}
			srv.chunkHashToShard[ch] = target
			want[ch] = target
		}

		path := filepath.Join(dir, "snapshot.json")
		if err := srv.Snapshot(path); err != nil {
			t.Fatalf("Snapshot: %v", err)
		}

		store2, err := fsstore.New(filepath.Join(dir, "xorbs2"))
		if err != nil {
			t.Fatalf("fsstore.New 2: %v", err)
		}
		restored := New(store2)
		if err := restored.LoadSnapshot(path); err != nil {
			t.Fatalf("LoadSnapshot: %v", err)
		}

		if len(restored.chunkHashToShard) != len(want) {
			t.Fatalf("restored %d chunk entries, want %d",
				len(restored.chunkHashToShard), len(want))
		}
		for ch, wantShard := range want {
			gotShard, ok := restored.chunkHashToShard[ch]
			if !ok {
				t.Fatalf("chunk %s missing after restore", ch.Hex())
			}
			if gotShard != wantShard {
				t.Fatalf("chunk %s -> shard %s, want %s",
					ch.Hex(), gotShard.Hex(), wantShard.Hex())
			}
			// The body must come back byte-identical through the
			// indirection, or a chunk-dedup response would serve the
			// wrong shard.
			gotBody, ok := restored.shardBodies[gotShard]
			if !ok {
				t.Fatalf("shard body %s missing after restore", gotShard.Hex())
			}
			var wantBody []byte
			if wantShard == hashA {
				wantBody = bodyA
			} else {
				wantBody = bodyB
			}
			if string(gotBody) != string(wantBody) {
				t.Fatalf("shard body for %s changed across the round trip", gotShard.Hex())
			}
		}

		// Anti-blowup: the snapshot stores each distinct body once, so its
		// size must stay near (bodies + per-chunk hash entries) rather than
		// growing with bodies x chunks. Generous bound - the point is to
		// catch a regression to per-chunk body duplication, which would be
		// orders of magnitude over this, not to pin an exact size.
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat snapshot: %v", err)
		}
		distinctBodies := len(bodyA) + len(bodyB)
		perChunkOverhead := 256 // hex hashes + JSON punctuation, generously
		limit := int64(distinctBodies*2 + len(want)*perChunkOverhead + 8192)
		if info.Size() > limit {
			t.Fatalf("snapshot is %d bytes for %d chunks over %d body bytes (limit %d) - "+
				"suggests shard bodies are being duplicated per chunk again",
				info.Size(), len(want), distinctBodies, limit)
		}
	})
}

// TestSnapshotV2_NoPerChunkBodyDuplication is the concrete, non-fuzz
// statement of the bug v2 fixed: one large shard body referenced by many
// chunks must serialize ONCE. Under the v1 layout this produced a file
// roughly bodySize x chunkCount (a ~30 MB body over ~1.7 M chunks ran
// toward 50 TB and killed the process); here it must stay close to the
// body's own size.
func TestSnapshotV2_NoPerChunkBodyDuplication(t *testing.T) {
	dir := t.TempDir()
	store, err := fsstore.New(filepath.Join(dir, "xorbs"))
	if err != nil {
		t.Fatalf("fsstore.New: %v", err)
	}
	srv := New(store)

	const bodySize = 64 * 1024
	const chunkCount = 4000

	body := make([]byte, bodySize)
	for i := range body {
		body[i] = byte(i)
	}
	shardHash := merklehash.ComputeDataHash(body)
	srv.shardBodies[shardHash] = body
	for i := 0; i < chunkCount; i++ {
		var ch merklehash.Hash
		ch[0], ch[1], ch[2] = byte(i), byte(i>>8), byte(i>>16)
		srv.chunkHashToShard[ch] = shardHash
	}

	path := filepath.Join(dir, "snapshot.json")
	if err := srv.Snapshot(path); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// v1 would have been >= bodySize*chunkCount (~256 MB here). v2 stores
	// the body once (base64 in JSON, ~4/3 expansion) plus ~200 bytes of
	// hex hashes per chunk.
	v1Size := int64(bodySize) * int64(chunkCount)
	v2Limit := int64(bodySize*2 + chunkCount*256 + 8192)
	if info.Size() > v2Limit {
		t.Errorf("snapshot is %d bytes; expected under %d (v1 layout would have been ~%d)",
			info.Size(), v2Limit, v1Size)
	}
	t.Logf("snapshot %d bytes for %d chunks x %d-byte body (v1 layout would be ~%d bytes)",
		info.Size(), chunkCount, bodySize, v1Size)
}
