// Package shardformat implements the on-wire binary layout of a Xet shard
// - the file/xorb reconstruction index real clients upload via
// POST /v1/shards - ported from
// xet_core_structures/src/metadata_shard/{shard_format,file_structs,xorb_structs}.rs.
//
// A shard file is: header, file-info section (one FileDataSequenceHeader +
// N FileDataSequenceEntry per file, terminated by a bookend header), xorb-info
// section (one XorbChunkSequenceHeader + N XorbChunkSequenceEntry per xorb,
// terminated by a bookend header), then three sorted lookup tables (file,
// xorb, chunk) mapping a truncated hash to an index into the corresponding
// content section, then the footer (read from the end of the file, since it
// carries the offsets needed to find everything else).
package shardformat

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/guilt/xet-server/internal/merklehash"
)

// headerTag is xet-core's MDB_SHARD_HEADER_TAG: literal bytes "HFRepoMetaData\0"
// followed by 17 fixed constant bytes.
var headerTag = [32]byte{
	'H', 'F', 'R', 'e', 'p', 'o', 'M', 'e', 't', 'a', 'D', 'a', 't', 'a', 0, 85,
	105, 103, 69, 106, 123, 129, 87, 131, 165, 189, 217, 92, 205, 209, 74, 169,
}

const (
	headerVersion        = 2
	footerVersion        = 1
	headerSize           = 32 + 8 + 8 // tag + version + footer_size
	footerSize           = 8*9 + 32 + 8*2 + 8*6 + 8*3 + 8
	fileFlagVerification = 1 << 31
	fileFlagMetadataExt  = 1 << 30
)

// Header mirrors MDBShardFileHeader.
type Header struct {
	Version    uint64
	FooterSize uint64
}

// WriteHeader writes h's 48-byte wire representation to w.
func WriteHeader(w io.Writer, h Header) error {
	if _, err := w.Write(headerTag[:]); err != nil {
		return err
	}
	if err := writeU64(w, h.Version); err != nil {
		return err
	}
	return writeU64(w, h.FooterSize)
}

// ReadHeader reads and validates a shard header from r.
func ReadHeader(r io.Reader) (Header, error) {
	var tag [32]byte
	if _, err := io.ReadFull(r, tag[:]); err != nil {
		return Header{}, err
	}
	if tag != headerTag {
		return Header{}, fmt.Errorf("shardformat: invalid header tag, not a Xet shard file")
	}
	version, err := readU64(r)
	if err != nil {
		return Header{}, err
	}
	footerSz, err := readU64(r)
	if err != nil {
		return Header{}, err
	}
	return Header{Version: version, FooterSize: footerSz}, nil
}

// DefaultHeader returns the header xet-core writes for new shards.
func DefaultHeader() Header {
	return Header{Version: headerVersion, FooterSize: footerSize}
}

// Footer mirrors MDBShardFileFooter.
type Footer struct {
	Version             uint64
	FileInfoOffset      uint64
	XorbInfoOffset      uint64
	FileLookupOffset    uint64
	FileLookupNumEntry  uint64
	XorbLookupOffset    uint64
	XorbLookupNumEntry  uint64
	ChunkLookupOffset   uint64
	ChunkLookupNumEntry uint64
	ChunkHashHMACKey    merklehash.Hash
	ShardCreationTime   uint64
	ShardKeyExpiry      uint64
	StoredBytesOnDisk   uint64
	MaterializedBytes   uint64
	StoredBytes         uint64
	FooterOffset        uint64 // always last; byte offset of the footer's own start
}

// WriteFooter writes f's fixed-size wire representation to w.
func WriteFooter(w io.Writer, f Footer) error {
	fields := []uint64{
		footerVersion,
		f.FileInfoOffset, f.XorbInfoOffset,
		f.FileLookupOffset, f.FileLookupNumEntry,
		f.XorbLookupOffset, f.XorbLookupNumEntry,
		f.ChunkLookupOffset, f.ChunkLookupNumEntry,
	}
	for _, v := range fields {
		if err := writeU64(w, v); err != nil {
			return err
		}
	}
	if _, err := w.Write(f.ChunkHashHMACKey.Bytes()); err != nil {
		return err
	}
	tail := []uint64{f.ShardCreationTime, f.ShardKeyExpiry, 0, 0, 0, 0, 0, 0, f.StoredBytesOnDisk, f.MaterializedBytes, f.StoredBytes, f.FooterOffset}
	for _, v := range tail {
		if err := writeU64(w, v); err != nil {
			return err
		}
	}
	return nil
}

// ReadFooter reads a fixed-size footer from r.
func ReadFooter(r io.Reader) (Footer, error) {
	var f Footer
	version, err := readU64(r)
	if err != nil {
		return f, err
	}
	if version != footerVersion {
		return f, fmt.Errorf("shardformat: unsupported footer version %d, want %d", version, footerVersion)
	}
	f.Version = version

	fields := []*uint64{
		&f.FileInfoOffset, &f.XorbInfoOffset,
		&f.FileLookupOffset, &f.FileLookupNumEntry,
		&f.XorbLookupOffset, &f.XorbLookupNumEntry,
		&f.ChunkLookupOffset, &f.ChunkLookupNumEntry,
	}
	for _, p := range fields {
		v, err := readU64(r)
		if err != nil {
			return f, err
		}
		*p = v
	}

	hmacBytes, err := readN(r, 32)
	if err != nil {
		return f, err
	}
	f.ChunkHashHMACKey, err = merklehash.FromRawBytes(hmacBytes)
	if err != nil {
		return f, err
	}

	if f.ShardCreationTime, err = readU64(r); err != nil {
		return f, err
	}
	if f.ShardKeyExpiry, err = readU64(r); err != nil {
		return f, err
	}
	for i := 0; i < 6; i++ { // _buffer
		if _, err := readU64(r); err != nil {
			return f, err
		}
	}
	if f.StoredBytesOnDisk, err = readU64(r); err != nil {
		return f, err
	}
	if f.MaterializedBytes, err = readU64(r); err != nil {
		return f, err
	}
	if f.StoredBytes, err = readU64(r); err != nil {
		return f, err
	}
	if f.FooterOffset, err = readU64(r); err != nil {
		return f, err
	}
	return f, nil
}

func writeU64(w io.Writer, v uint64) error {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	_, err := w.Write(b[:])
	return err
}

func readU64(r io.Reader) (uint64, error) {
	var b [8]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b[:]), nil
}

func readN(r io.Reader, n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}
