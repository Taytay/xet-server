package lz4

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const frameMagic = 0x184D2204

// DecompressFrame decompresses a complete LZ4 frame (magic number, frame
// descriptor, one or more blocks, end mark, optional checksums) per the
// public frame format spec. Checksums (header/block/content) are present
// on the wire but not verified here — this server only needs the decoded
// bytes to re-hash and compare against a client-claimed chunk hash, and a
// checksum mismatch would just mean the re-hash comparison fails anyway.
func DecompressFrame(src []byte) ([]byte, error) {
	if len(src) < 7 {
		return nil, fmt.Errorf("lz4: frame too short")
	}
	magic := binary.LittleEndian.Uint32(src[0:4])
	if magic != frameMagic {
		return nil, fmt.Errorf("lz4: bad frame magic %#x", magic)
	}
	pos := 4

	flg := src[pos]
	bd := src[pos+1]
	pos += 2

	version := flg >> 6
	if version != 1 {
		return nil, fmt.Errorf("lz4: unsupported frame version %d", version)
	}
	blockChecksumFlag := flg&(1<<4) != 0
	contentSizeFlag := flg&(1<<3) != 0
	contentChecksumFlag := flg&(1<<2) != 0
	dictIDFlag := flg&(1<<0) != 0

	blockMaxSizeCode := (bd >> 4) & 0x7

	if contentSizeFlag {
		pos += 8 // content size field, unused: block sizes are self-describing
	}
	if dictIDFlag {
		pos += 4
	}
	pos++ // header checksum byte

	if pos > len(src) {
		return nil, fmt.Errorf("lz4: frame truncated in header")
	}

	var out []byte
	for {
		if pos+4 > len(src) {
			return nil, fmt.Errorf("lz4: frame truncated reading block size")
		}
		blockSizeField := binary.LittleEndian.Uint32(src[pos : pos+4])
		pos += 4
		if blockSizeField == 0 {
			break // EndMark
		}

		uncompressedFlag := blockSizeField&(1<<31) != 0
		blockSize := int(blockSizeField &^ (1 << 31))

		if pos+blockSize > len(src) {
			return nil, fmt.Errorf("lz4: frame truncated in block data")
		}
		blockData := src[pos : pos+blockSize]
		pos += blockSize

		if blockChecksumFlag {
			pos += 4
		}

		if uncompressedFlag {
			out = append(out, blockData...)
			continue
		}

		maxBlockSize := maxBlockSizeForCode(blockMaxSizeCode)
		decoded, err := decompressBlockUnknownSize(blockData, maxBlockSize)
		if err != nil {
			return nil, fmt.Errorf("lz4: decompress block: %w", err)
		}
		out = append(out, decoded...)
	}

	if contentChecksumFlag {
		pos += 4
	}
	_ = pos

	return out, nil
}

// maxBlockSizeForCode maps the frame descriptor's 3-bit block-max-size code
// to a byte count, per the frame format spec (codes 4-7 only; 0-3 are
// reserved).
func maxBlockSizeForCode(code byte) int {
	switch code {
	case 4:
		return 64 * 1024
	case 5:
		return 256 * 1024
	case 6:
		return 1024 * 1024
	case 7:
		return 4 * 1024 * 1024
	default:
		return 4 * 1024 * 1024 // permissive fallback; decompressBlockUnknownSize regrows as needed
	}
}

// decompressBlockUnknownSize decompresses one LZ4 block without knowing its
// exact decoded size in advance (the frame format, unlike some other LZ4
// containers, does not store per-block uncompressed size). Starts from a
// capacity hint and regrows if the block decodes larger.
func decompressBlockUnknownSize(src []byte, sizeHint int) ([]byte, error) {
	dst := make([]byte, sizeHint)
	for {
		n, err := decompressBlockInto(src, dst)
		if err == nil {
			return dst[:n], nil
		}
		if errors.Is(err, ErrOutputTooSmall) {
			dst = make([]byte, len(dst)*2)
			continue
		}
		return nil, err
	}
}
