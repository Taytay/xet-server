package shardformat

// Fuzz target for shard wire-format parsing: ReadShard is called on every
// uploaded shard's full body (casserver.handleUploadShard) with completely
// attacker-controlled bytes. This is the package where a real,
// confirmed allocation-size DoS was found and fixed (see dos_test.go) -
// this fuzzer exists to catch anything else in the same family, plus
// guard against a regression of the fixed bug.

import (
	"bytes"
	"os"
	"testing"
)

func FuzzReadShard(f *testing.F) {
	if raw, err := os.ReadFile("testdata/real_upload_shard.bin"); err == nil {
		f.Add(raw) // a real captured shard from a live hf_xet upload
	}
	f.Add([]byte{})
	f.Add(make([]byte, headerSize-1)) // shorter than just the header
	f.Add(make([]byte, headerSize))   // exactly header-sized, all zero

	// A minimal footer-less shard with one FileDataSequenceHeader claiming
	// the maximum NumEntries and nothing following - the exact shape of
	// the fixed DoS, kept as a fuzz seed so mutation explores nearby
	// malformed variants too.
	var malicious bytes.Buffer
	WriteHeader(&malicious, Header{Version: headerVersion, FooterSize: 0})
	WriteFileDataSequenceHeader(&malicious, FileDataSequenceHeader{
		FileHash:   hashFromByte(0xAB),
		NumEntries: 0xFFFFFFFF,
	})
	f.Add(malicious.Bytes())

	// Same shape for the xorb-info section.
	var maliciousXorb bytes.Buffer
	WriteHeader(&maliciousXorb, Header{Version: headerVersion, FooterSize: 0})
	WriteFileDataSequenceHeader(&maliciousXorb, BookendFileHeader())
	WriteXorbChunkSequenceHeader(&maliciousXorb, XorbChunkSequenceHeader{
		XorbHash:   hashFromByte(0xCD),
		NumEntries: 0xFFFFFFFF,
	})
	f.Add(maliciousXorb.Bytes())

	// A footer-carrying shard (FooterSize != 0) with a huge lookup-table
	// entry count claimed in the footer, no matching bytes.
	var maliciousFooter bytes.Buffer
	WriteHeader(&maliciousFooter, DefaultHeader())
	WriteFileDataSequenceHeader(&maliciousFooter, BookendFileHeader())
	WriteXorbChunkSequenceHeader(&maliciousFooter, BookendXorbHeader())
	WriteFooter(&maliciousFooter, Footer{
		FileLookupNumEntry:  0xFFFFFFFFFFFFFFFF,
		XorbLookupNumEntry:  0xFFFFFFFFFFFFFFFF,
		ChunkLookupNumEntry: 0xFFFFFFFFFFFFFFFF,
		ShardKeyExpiry:      ^uint64(0),
	})
	f.Add(maliciousFooter.Bytes())

	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("ReadShard panicked on %d bytes: %v", len(data), r)
			}
		}()
		_, _ = ReadShard(bytes.NewReader(data))
	})
}
