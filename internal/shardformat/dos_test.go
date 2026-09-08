package shardformat

import (
	"bytes"
	"runtime"
	"testing"
	"time"
)

// TestReadShard_MaliciousNumEntriesDoesNotOOM is a regression test for a
// fixed allocation-size DoS: a shard's FileDataSequenceHeader/
// XorbChunkSequenceHeader carry a NumEntries field read directly off the
// wire with no bound against how many bytes are actually left to read.
// Before the fix, readFileInfoSection did make([]FileDataSequenceEntry,
// fh.NumEntries) — a ~50-byte malicious shard claiming NumEntries near
// uint32's max forced an attempted allocation of tens of gigabytes before
// a single one of the claimed entries had been validated to exist on the
// wire. POST /v1/shards accepts up to 16 MiB (see casserver.maxShardBytes)
// and requires no auth, so this was reachable from any client.
//
// The fix (append-based incremental growth capped at a small
// preallocation ceiling) must still correctly parse honest shards — see
// TestShard_RoundTrip and TestReadShard_RealHFXetCapture for that — this
// test only asserts the malicious case fails fast (an EOF-shaped parse
// error) rather than attempting a huge allocation or hanging.
func TestReadShard_MaliciousNumEntriesDoesNotOOM(t *testing.T) {
	var buf bytes.Buffer
	// Footer-less header (real-client-shaped): FooterSize = 0 triggers
	// readShardWithoutFooter, which reads sequentially to EOF.
	if err := WriteHeader(&buf, Header{Version: headerVersion, FooterSize: 0}); err != nil {
		t.Fatalf("WriteHeader() error = %v", err)
	}

	// One FileDataSequenceHeader claiming the maximum possible NumEntries,
	// immediately followed by EOF — no actual entry bytes.
	maliciousHeader := FileDataSequenceHeader{
		FileHash:   hashFromByte(0xAB),
		FileFlags:  0,
		NumEntries: 0xFFFFFFFF, // ~4.29 billion; at 48 bytes/entry, ~206 GB if naively pre-allocated
	}
	if err := WriteFileDataSequenceHeader(&buf, maliciousHeader); err != nil {
		t.Fatalf("WriteFileDataSequenceHeader() error = %v", err)
	}
	// Deliberately no entry bytes, no bookend, no xorb section: the reader
	// must hit EOF while trying to read the first claimed entry.

	malicious := buf.Bytes()
	t.Logf("malicious shard body is %d bytes, claims %d entries", len(malicious), maliciousHeader.NumEntries)

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	done := make(chan struct{})
	var parseErr error
	go func() {
		_, parseErr = ReadShard(bytes.NewReader(malicious))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ReadShard() did not return within 5s on a malicious NumEntries payload — looks hung, not just slow")
	}

	if parseErr == nil {
		t.Fatal("ReadShard() error = nil, want a parse error (EOF reading claimed entries) for a truncated malicious payload")
	}

	runtime.ReadMemStats(&after)
	// A generous ceiling: legitimate parsing of a 50-byte input should
	// never need more than a few MB of heap growth. The old, unfixed code
	// would have attempted a single allocation request of ~206 GB, which
	// fails immediately with a fatal OOM (not a recoverable panic) on any
	// real machine — so on a fixed build we expect this to stay small; on
	// a reintroduced regression, the goroutine above would crash the whole
	// test binary rather than this assertion ever running.
	const maxReasonableGrowth = 64 * 1024 * 1024
	grew := after.TotalAlloc - before.TotalAlloc
	if grew > maxReasonableGrowth {
		t.Errorf("heap grew by %d bytes parsing a 50-byte malicious payload, want < %d — looks like unbounded preallocation regressed", grew, maxReasonableGrowth)
	}
}

// TestReadShard_MaliciousXorbNumEntriesDoesNotOOM is the xorb-info-section
// analog of the file-info-section test above: XorbChunkSequenceHeader's
// NumEntries field is equally attacker-controlled and equally unbounded
// before the fix.
func TestReadShard_MaliciousXorbNumEntriesDoesNotOOM(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteHeader(&buf, Header{Version: headerVersion, FooterSize: 0}); err != nil {
		t.Fatalf("WriteHeader() error = %v", err)
	}
	// Empty, valid file-info section: just the bookend.
	if err := WriteFileDataSequenceHeader(&buf, BookendFileHeader()); err != nil {
		t.Fatalf("WriteFileDataSequenceHeader(bookend) error = %v", err)
	}

	maliciousXorbHeader := XorbChunkSequenceHeader{
		XorbHash:   hashFromByte(0xCD),
		NumEntries: 0xFFFFFFFF,
	}
	if err := WriteXorbChunkSequenceHeader(&buf, maliciousXorbHeader); err != nil {
		t.Fatalf("WriteXorbChunkSequenceHeader() error = %v", err)
	}

	done := make(chan struct{})
	var parseErr error
	go func() {
		_, parseErr = ReadShard(bytes.NewReader(buf.Bytes()))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ReadShard() did not return within 5s on a malicious xorb NumEntries payload")
	}

	if parseErr == nil {
		t.Fatal("ReadShard() error = nil, want a parse error for a truncated malicious xorb-info section")
	}
}
