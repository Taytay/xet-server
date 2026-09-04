// WriteFooterV1 writes footer f's wire representation to w, matching
// xet_object_format.rs's XorbObjectInfoV1::serialize. Used by this
// package's own tests to build round-trip fixtures; a real CAS server
// only ever needs ParseFooterV1 for xorbs uploaded by real clients.
package xorbformat

import "io"

// WriteFooterV1 serializes f to w in the same section order and layout
// XorbObjectInfoV1::serialize uses.
func WriteFooterV1(w io.Writer, f FooterV1) error {
	if uint32(len(f.ChunkHashes)) != f.NumChunks ||
		uint32(len(f.ChunkBoundaryOffsets)) != f.NumChunks ||
		uint32(len(f.UnpackedChunkOffsets)) != f.NumChunks {
		return errLengthMismatch
	}

	if _, err := w.Write(xorbIdent[:]); err != nil {
		return err
	}
	if err := writeU8(w, footerVersionV1); err != nil {
		return err
	}
	if _, err := w.Write(f.XorbHash.Bytes()); err != nil {
		return err
	}

	// Hash section.
	if _, err := w.Write(hashIdent[:]); err != nil {
		return err
	}
	if err := writeU8(w, hashSectionVersion); err != nil {
		return err
	}
	if err := writeU32(w, f.NumChunks); err != nil {
		return err
	}
	for _, h := range f.ChunkHashes {
		if _, err := w.Write(h.Bytes()); err != nil {
			return err
		}
	}

	// Boundary section.
	if _, err := w.Write(boundaryIdent[:]); err != nil {
		return err
	}
	if err := writeU8(w, boundarySectionVer); err != nil {
		return err
	}
	if err := writeU32(w, f.NumChunks); err != nil {
		return err
	}
	for _, v := range f.ChunkBoundaryOffsets {
		if err := writeU32(w, v); err != nil {
			return err
		}
	}
	for _, v := range f.UnpackedChunkOffsets {
		if err := writeU32(w, v); err != nil {
			return err
		}
	}

	// Fixed tail.
	if err := writeU32(w, f.NumChunks); err != nil {
		return err
	}
	// hashes_section_offset_from_end: bytes from end-of-footer back to the
	// start of the hash section ident.
	hashesLen := uint32(7+1+4) + f.NumChunks*32
	boundaryLen := uint32(7+1+4) + f.NumChunks*4*2
	tailLen := uint32(4 + 4 + 4 + nonceBufferLen)
	if err := writeU32(w, hashesLen+boundaryLen+tailLen); err != nil {
		return err
	}
	if err := writeU32(w, boundaryLen+tailLen); err != nil {
		return err
	}
	var nonce [nonceBufferLen]byte
	if _, err := w.Write(nonce[:]); err != nil {
		return err
	}
	return nil
}

func writeU8(w io.Writer, v uint8) error {
	_, err := w.Write([]byte{v})
	return err
}

func writeU32(w io.Writer, v uint32) error {
	b := []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}
	_, err := w.Write(b)
	return err
}

var errLengthMismatch = &lengthMismatchError{}

type lengthMismatchError struct{}

func (*lengthMismatchError) Error() string {
	return "xorbformat: ChunkHashes/ChunkBoundaryOffsets/UnpackedChunkOffsets length does not match NumChunks"
}
