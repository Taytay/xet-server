package shardformat

import (
	"encoding/binary"
	"io"

	"xet-server/internal/merklehash"
)

// FileDataSequenceHeader mirrors file_structs.rs's FileDataSequenceHeader:
// starts one file's entry in the file-info section.
type FileDataSequenceHeader struct {
	FileHash   merklehash.Hash
	FileFlags  uint32
	NumEntries uint32
	// _unused u64 omitted; always zero on write, ignored on read.
}

func (h FileDataSequenceHeader) ContainsVerification() bool {
	return h.FileFlags&fileFlagVerification != 0
}

func (h FileDataSequenceHeader) ContainsMetadataExt() bool {
	return h.FileFlags&fileFlagMetadataExt != 0
}

// IsBookend reports whether h is the all-1s sentinel header xet-core writes
// to terminate the file-info / xorb-info sections for sequential reading.
func (h FileDataSequenceHeader) IsBookend() bool {
	return h.FileHash == bookendHash
}

var bookendHash = mustAllOnesHash()

func mustAllOnesHash() merklehash.Hash {
	var b [32]byte
	for i := range b {
		b[i] = 0xFF
	}
	h, err := merklehash.FromRawBytes(b[:])
	if err != nil {
		panic(err)
	}
	return h
}

// BookendFileHeader returns the sentinel FileDataSequenceHeader used to
// terminate the file-info section.
func BookendFileHeader() FileDataSequenceHeader {
	return FileDataSequenceHeader{FileHash: bookendHash}
}

func WriteFileDataSequenceHeader(w io.Writer, h FileDataSequenceHeader) error {
	if _, err := w.Write(h.FileHash.Bytes()); err != nil {
		return err
	}
	if err := writeU32(w, h.FileFlags); err != nil {
		return err
	}
	if err := writeU32(w, h.NumEntries); err != nil {
		return err
	}
	return writeU64(w, 0) // _unused
}

func ReadFileDataSequenceHeader(r io.Reader) (FileDataSequenceHeader, error) {
	var h FileDataSequenceHeader
	b, err := readN(r, 32)
	if err != nil {
		return h, err
	}
	if h.FileHash, err = merklehash.FromRawBytes(b); err != nil {
		return h, err
	}
	if h.FileFlags, err = readU32(r); err != nil {
		return h, err
	}
	if h.NumEntries, err = readU32(r); err != nil {
		return h, err
	}
	if _, err = readU64(r); err != nil { // _unused
		return h, err
	}
	return h, nil
}

// FileDataSequenceEntry mirrors file_structs.rs's FileDataSequenceEntry:
// one xorb chunk-range reference within a file's reconstruction sequence.
type FileDataSequenceEntry struct {
	XorbHash             merklehash.Hash
	XorbFlags            uint32
	UnpackedSegmentBytes uint32
	ChunkIndexStart      uint32
	ChunkIndexEnd        uint32
}

func WriteFileDataSequenceEntry(w io.Writer, e FileDataSequenceEntry) error {
	if _, err := w.Write(e.XorbHash.Bytes()); err != nil {
		return err
	}
	for _, v := range []uint32{e.XorbFlags, e.UnpackedSegmentBytes, e.ChunkIndexStart, e.ChunkIndexEnd} {
		if err := writeU32(w, v); err != nil {
			return err
		}
	}
	return nil
}

func ReadFileDataSequenceEntry(r io.Reader) (FileDataSequenceEntry, error) {
	var e FileDataSequenceEntry
	b, err := readN(r, 32)
	if err != nil {
		return e, err
	}
	if e.XorbHash, err = merklehash.FromRawBytes(b); err != nil {
		return e, err
	}
	fields := []*uint32{&e.XorbFlags, &e.UnpackedSegmentBytes, &e.ChunkIndexStart, &e.ChunkIndexEnd}
	for _, p := range fields {
		v, err := readU32(r)
		if err != nil {
			return e, err
		}
		*p = v
	}
	return e, nil
}

// XorbChunkSequenceHeader mirrors xorb_structs.rs's XorbChunkSequenceHeader:
// starts one xorb's entry in the xorb-info section.
type XorbChunkSequenceHeader struct {
	XorbHash       merklehash.Hash
	XorbFlags      uint32
	NumEntries     uint32
	NumBytesInXorb uint32
	NumBytesOnDisk uint32
}

func (h XorbChunkSequenceHeader) IsBookend() bool { return h.XorbHash == bookendHash }

// BookendXorbHeader returns the sentinel XorbChunkSequenceHeader used to
// terminate the xorb-info section.
func BookendXorbHeader() XorbChunkSequenceHeader {
	return XorbChunkSequenceHeader{XorbHash: bookendHash}
}

func WriteXorbChunkSequenceHeader(w io.Writer, h XorbChunkSequenceHeader) error {
	if _, err := w.Write(h.XorbHash.Bytes()); err != nil {
		return err
	}
	for _, v := range []uint32{h.XorbFlags, h.NumEntries, h.NumBytesInXorb, h.NumBytesOnDisk} {
		if err := writeU32(w, v); err != nil {
			return err
		}
	}
	return nil
}

func ReadXorbChunkSequenceHeader(r io.Reader) (XorbChunkSequenceHeader, error) {
	var h XorbChunkSequenceHeader
	b, err := readN(r, 32)
	if err != nil {
		return h, err
	}
	if h.XorbHash, err = merklehash.FromRawBytes(b); err != nil {
		return h, err
	}
	fields := []*uint32{&h.XorbFlags, &h.NumEntries, &h.NumBytesInXorb, &h.NumBytesOnDisk}
	for _, p := range fields {
		v, err := readU32(r)
		if err != nil {
			return h, err
		}
		*p = v
	}
	return h, nil
}

// XorbChunkSequenceEntry mirrors xorb_structs.rs's XorbChunkSequenceEntry:
// one chunk's hash, physical position, and uncompressed length within its
// xorb.
type XorbChunkSequenceEntry struct {
	ChunkHash            merklehash.Hash
	ChunkByteRangeStart  uint32
	UnpackedSegmentBytes uint32
	Flags                uint32
	// _unused u32 omitted; always zero on write, ignored on read.
}

func WriteXorbChunkSequenceEntry(w io.Writer, e XorbChunkSequenceEntry) error {
	if _, err := w.Write(e.ChunkHash.Bytes()); err != nil {
		return err
	}
	for _, v := range []uint32{e.ChunkByteRangeStart, e.UnpackedSegmentBytes, e.Flags, 0} {
		if err := writeU32(w, v); err != nil {
			return err
		}
	}
	return nil
}

func ReadXorbChunkSequenceEntry(r io.Reader) (XorbChunkSequenceEntry, error) {
	var e XorbChunkSequenceEntry
	b, err := readN(r, 32)
	if err != nil {
		return e, err
	}
	if e.ChunkHash, err = merklehash.FromRawBytes(b); err != nil {
		return e, err
	}
	fields := []*uint32{&e.ChunkByteRangeStart, &e.UnpackedSegmentBytes, &e.Flags}
	for _, p := range fields {
		v, err := readU32(r)
		if err != nil {
			return e, err
		}
		*p = v
	}
	if _, err := readU32(r); err != nil { // _unused
		return e, err
	}
	return e, nil
}

func writeU32(w io.Writer, v uint32) error {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	_, err := w.Write(b[:])
	return err
}

func readU32(r io.Reader) (uint32, error) {
	var b [4]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b[:]), nil
}
