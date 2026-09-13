// Package lz4 implements LZ4 block and frame decompression from the public
// LZ4 format specifications:
//   - Block format: https://github.com/lz4/lz4/blob/dev/doc/lz4_Block_format.md
//   - Frame format: https://github.com/lz4/lz4/blob/dev/doc/lz4_Frame_format.md
//
// Written from these specifications rather than ported from any existing
// implementation. xet-core (via the Rust lz4_flex crate's "frame" module)
// always uses the LZ4 *frame* format on the wire, never raw blocks - so a
// CAS server that wants to independently verify chunk hashes for
// LZ4-compressed chunks needs a frame-aware decoder, not just a block
// decoder. Only decompression is implemented; this server never needs to
// produce LZ4 output itself.
package lz4

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ErrOutputTooSmall is returned by decompressBlockInto when dst is not
// large enough to hold the decoded block. Callers that don't know the
// decoded size in advance (the LZ4 frame format doesn't store it) can
// retry with a larger buffer.
var ErrOutputTooSmall = errors.New("lz4: output buffer too small")

// DecompressBlock decompresses a single LZ4 block (no frame header) per the
// block format spec, into a buffer of exactly dstLen bytes.
func DecompressBlock(src []byte, dstLen int) ([]byte, error) {
	dst := make([]byte, dstLen)
	n, err := decompressBlockInto(src, dst)
	if err != nil {
		return nil, err
	}
	if n != dstLen {
		return nil, fmt.Errorf("lz4: decompressed %d bytes, want %d", n, dstLen)
	}
	return dst, nil
}

// decompressBlockInto decodes src (a sequence of LZ4 "sequences": a token
// byte, optional extended literal-length bytes, literal bytes, a 2-byte
// little-endian offset, and optional extended match-length bytes) into dst,
// returning the number of bytes written.
func decompressBlockInto(src []byte, dst []byte) (int, error) {
	var si, di int

	for si < len(src) {
		if si >= len(src) {
			return di, fmt.Errorf("lz4: truncated block: expected token byte")
		}
		token := src[si]
		si++

		litLen := int(token >> 4)
		if litLen == 15 {
			extra, n, err := readExtendedLength(src[si:])
			if err != nil {
				return di, err
			}
			si += n
			litLen += extra
		}

		if si+litLen > len(src) {
			return di, fmt.Errorf("lz4: truncated block: literal run overruns input")
		}
		if di+litLen > len(dst) {
			return di, ErrOutputTooSmall
		}
		copy(dst[di:di+litLen], src[si:si+litLen])
		si += litLen
		di += litLen

		// The last sequence in a block is literals-only (spec section
		// "Parsing restrictions"): if we've consumed all input, stop here
		// rather than trying to read a nonexistent offset/match-length.
		if si == len(src) {
			break
		}

		if si+2 > len(src) {
			return di, fmt.Errorf("lz4: truncated block: expected 2-byte offset")
		}
		offset := int(binary.LittleEndian.Uint16(src[si : si+2]))
		si += 2
		if offset == 0 {
			return di, fmt.Errorf("lz4: invalid zero offset")
		}
		if offset > di {
			return di, fmt.Errorf("lz4: offset %d exceeds decoded length %d", offset, di)
		}

		matchLen := int(token & 0x0F)
		if matchLen == 15 {
			extra, n, err := readExtendedLength(src[si:])
			if err != nil {
				return di, err
			}
			si += n
			matchLen += extra
		}
		matchLen += 4 // minimum match length per spec

		if di+matchLen > len(dst) {
			return di, ErrOutputTooSmall
		}
		copyStart := di - offset
		for i := 0; i < matchLen; i++ {
			dst[di+i] = dst[copyStart+i]
		}
		di += matchLen
	}

	return di, nil
}

// readExtendedLength reads the variable-length extension used when a
// token's 4-bit length nibble is 15: each subsequent byte adds up to 255 to
// the length, and a byte less than 255 terminates the sequence.
func readExtendedLength(src []byte) (value int, consumed int, err error) {
	for {
		if consumed >= len(src) {
			return 0, 0, fmt.Errorf("lz4: truncated extended length")
		}
		b := src[consumed]
		consumed++
		value += int(b)
		if b != 255 {
			return value, consumed, nil
		}
	}
}
