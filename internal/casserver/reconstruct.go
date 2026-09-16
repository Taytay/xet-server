package casserver

// reconstruct.go: server-side file reconstruction. The real CAS never
// does this - it hands clients a term list and lets them fetch and
// decompress xorb ranges themselves (see reconstruction.go). A git-lfs
// client, though, only speaks the "basic" transfer for downloads: one
// GET that must return the whole file's plain bytes. hubserver's LFS
// bridge therefore needs the CAS to assemble a file from its shard
// terms and xorb chunks here, once, and stream the result.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/guilt/xet-server/internal/merklehash"
	"github.com/guilt/xet-server/internal/reconwire"
	"github.com/guilt/xet-server/internal/xorbformat"
)

// ErrUnknownFile is returned (via errors.Is) by ReconstructFile when no
// uploaded shard has described fileHash.
var ErrUnknownFile = errors.New("casserver: unknown file")

// ErrRangeNotSatisfiable is returned (via errors.Is) by ReconstructFile
// when [start, end] falls outside the file.
var ErrRangeNotSatisfiable = errors.New("casserver: range not satisfiable")

// ReconstructFile writes bytes [start, end] (inclusive, like an HTTP
// Range) of the file identified by fileHash to w, decompressing each
// chunk from its xorb on the way out. A zero-length file is served for
// start == 0 and end == -1 only. Every xorb touched is marked in flight
// for the duration so an eviction sweep cannot delete it mid-stream.
//
// The write to w may have started when an error is returned (a xorb
// missing from storage, a corrupt chunk), so a caller streaming an HTTP
// body cannot turn a late error into a clean status code; it should log
// it and drop the connection, and the client's own SHA-256 check
// (git-lfs verifies every download) catches the truncation.
func (s *Server) ReconstructFile(ctx context.Context, fileHash merklehash.Hash, start, end int64, w io.Writer) error {
	s.fileReconMu.RLock()
	entries, ok := s.fileRecon[fileHash]
	s.fileReconMu.RUnlock()
	if !ok {
		return ErrUnknownFile
	}
	size := reconwire.FileSize(entries)
	if size == 0 {
		if start == 0 && end == -1 {
			return nil
		}
		return ErrRangeNotSatisfiable
	}
	if start < 0 || start > end || start >= size {
		return ErrRangeNotSatisfiable
	}
	if end >= size {
		end = size - 1
	}

	var fileOffset int64
	for _, e := range entries {
		termStart := fileOffset
		termEnd := fileOffset + int64(e.UnpackedSegmentBytes) // exclusive
		fileOffset = termEnd
		if termEnd <= start || termStart > end {
			continue
		}
		if err := s.writeTerm(ctx, e.XorbHash, e.ChunkIndexStart, e.ChunkIndexEnd, e.UnpackedSegmentBytes, termStart, start, end, w); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

// writeTerm streams the part of one reconstruction term (chunks
// [chunkStart, chunkEnd) of xorb hash, occupying file bytes from
// termStart) that intersects [start, end].
func (s *Server) writeTerm(ctx context.Context, hash merklehash.Hash, chunkStart, chunkEnd, declaredBytes uint32, termStart, start, end int64, w io.Writer) error {
	footer, known := s.lookupXorbFooter(hash)
	if !known {
		return &reconwire.ErrUnknownXorbFooter{Hash: hash}
	}
	chunkCount := uint32(len(footer.ChunkBoundaryOffsets))
	if chunkStart > chunkEnd || chunkEnd == 0 || chunkEnd > chunkCount || uint32(len(footer.UnpackedChunkOffsets)) != chunkCount {
		return &reconwire.ErrChunkIndexOutOfRange{Hash: hash, ChunkIndexStart: chunkStart, ChunkEnd: chunkEnd, FooterChunkCount: chunkCount}
	}

	// Per-chunk physical (compressed, header included) and logical
	// (uncompressed) extents, from the footer's cumulative end offsets.
	physStart := func(i uint32) int64 {
		if i == 0 {
			return 0
		}
		return int64(footer.ChunkBoundaryOffsets[i-1])
	}
	unpackedLen := func(i uint32) int64 {
		if i == 0 {
			return int64(footer.UnpackedChunkOffsets[0])
		}
		return int64(footer.UnpackedChunkOffsets[i]) - int64(footer.UnpackedChunkOffsets[i-1])
	}

	var termBytes int64
	for i := chunkStart; i < chunkEnd; i++ {
		termBytes += unpackedLen(i)
	}
	if termBytes != int64(declaredBytes) {
		return fmt.Errorf("reconstruction term for xorb %s declares %d bytes but its chunks hold %d", hash.Hex(), declaredBytes, termBytes)
	}

	// Find the sub-run of chunks that intersects [start, end].
	firstNeeded, lastNeeded := chunkEnd, chunkStart // empty until set
	var chunkFileStart = termStart
	chunkOffsets := make([]int64, 0, chunkEnd-chunkStart)
	for i := chunkStart; i < chunkEnd; i++ {
		chunkOffsets = append(chunkOffsets, chunkFileStart)
		chunkFileEnd := chunkFileStart + unpackedLen(i) // exclusive
		if chunkFileEnd > start && chunkFileStart <= end {
			if firstNeeded == chunkEnd {
				firstNeeded = i
			}
			lastNeeded = i
		}
		chunkFileStart = chunkFileEnd
	}
	if firstNeeded == chunkEnd {
		return nil // nothing of this term is inside the window
	}

	rangeStart := physStart(firstNeeded)
	rangeEnd := int64(footer.ChunkBoundaryOffsets[lastNeeded]) // exclusive
	if rangeEnd <= rangeStart {
		return fmt.Errorf("xorb %s footer has a non-increasing chunk boundary at chunk %d", hash.Hex(), lastNeeded)
	}

	s.xorbMu.Lock()
	s.xorbInFlight[hash]++
	s.xorbLastAccess[hash] = time.Now()
	s.xorbMu.Unlock()
	defer func() {
		s.xorbMu.Lock()
		s.xorbInFlight[hash]--
		s.xorbMu.Unlock()
	}()

	data, err := s.xorbs.GetRange(ctx, hash.Hex(), rangeStart, rangeEnd-rangeStart)
	if err != nil {
		return fmt.Errorf("read xorb %s: %w", hash.Hex(), err)
	}
	defer data.Close()

	for i := firstNeeded; i <= lastNeeded; i++ {
		header, err := xorbformat.ReadChunkHeader(data)
		if err != nil {
			return fmt.Errorf("xorb %s chunk %d: read header: %w", hash.Hex(), i, err)
		}
		payload := make([]byte, header.CompressedLength)
		if _, err := io.ReadFull(data, payload); err != nil {
			return fmt.Errorf("xorb %s chunk %d: read payload: %w", hash.Hex(), i, err)
		}
		plain, err := xorbformat.DecompressChunkPayload(header.CompressionScheme, payload, header.UncompressedLength)
		if err != nil {
			return fmt.Errorf("xorb %s chunk %d: %w", hash.Hex(), i, err)
		}
		if int64(len(plain)) != unpackedLen(i) {
			return fmt.Errorf("xorb %s chunk %d: decompressed to %d bytes, footer says %d", hash.Hex(), i, len(plain), unpackedLen(i))
		}

		// Clip this chunk's bytes to the requested window.
		chunkFileStart := chunkOffsets[i-chunkStart]
		lo, hi := int64(0), int64(len(plain)) // [lo, hi) within plain
		if start > chunkFileStart {
			lo = start - chunkFileStart
		}
		if end+1 < chunkFileStart+int64(len(plain)) {
			hi = end + 1 - chunkFileStart
		}
		if lo < hi {
			if _, err := w.Write(plain[lo:hi]); err != nil {
				return err
			}
		}
	}
	return nil
}
