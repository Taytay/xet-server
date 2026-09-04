package shardformat

import (
	"io"

	"xet-server/internal/merklehash"
)

// FileEntry is one file's reconstruction sequence: a header plus its
// ordered xorb-chunk-range references, mirroring xet-core's MDBFileInfo
// (verification entries and metadata_ext are not modeled — this server
// never emits or requires them).
type FileEntry struct {
	Header  FileDataSequenceHeader
	Entries []FileDataSequenceEntry
}

// XorbEntry is one xorb's chunk list within the xorb-info section,
// mirroring xet-core's MDBXorbInfo.
type XorbEntry struct {
	Header XorbChunkSequenceHeader
	Chunks []XorbChunkSequenceEntry
}

// Shard is the fully decoded contents of a shard file: header, footer, and
// both content sections. Lookup tables are derived (not stored) here since
// a CAS server holds shards in memory and can just linear/binary-search
// the content it already parsed, rather than re-deriving offsets on disk.
type Shard struct {
	Header Header
	Footer Footer
	Files  []FileEntry
	Xorbs  []XorbEntry
}

// WriteShard serializes files/xorbs into a complete shard file, mirroring
// MDBShardInfo::serialize_from's section order: header, file-info section
// (+ bookend), xorb-info section (+ bookend), file lookup table, xorb
// lookup table, chunk lookup table, footer.
func WriteShard(w io.Writer, files []FileEntry, xorbs []XorbEntry) (Footer, error) {
	var buf countingWriter
	cw := io.MultiWriter(w, &buf)

	if err := WriteHeader(cw, DefaultHeader()); err != nil {
		return Footer{}, err
	}

	footer := Footer{}
	footer.FileInfoOffset = buf.n

	var fileLookup []FileLookupEntry
	var fileIndex uint32
	for _, f := range files {
		fileLookup = append(fileLookup, FileLookupEntry{Key: f.Header.FileHash.TruncateHash(), Index: fileIndex})
		if err := WriteFileDataSequenceHeader(cw, f.Header); err != nil {
			return Footer{}, err
		}
		for _, e := range f.Entries {
			if err := WriteFileDataSequenceEntry(cw, e); err != nil {
				return Footer{}, err
			}
		}
		fileIndex += 1 + uint32(len(f.Entries))
	}
	if err := WriteFileDataSequenceHeader(cw, BookendFileHeader()); err != nil {
		return Footer{}, err
	}

	footer.XorbInfoOffset = buf.n

	var xorbLookup []XorbLookupEntry
	var chunkLookup []ChunkLookupEntry
	var xorbIndex uint32
	for _, x := range xorbs {
		xorbLookup = append(xorbLookup, XorbLookupEntry{Key: x.Header.XorbHash.TruncateHash(), Index: xorbIndex})
		if err := WriteXorbChunkSequenceHeader(cw, x.Header); err != nil {
			return Footer{}, err
		}
		for i, c := range x.Chunks {
			if err := WriteXorbChunkSequenceEntry(cw, c); err != nil {
				return Footer{}, err
			}
			chunkLookup = append(chunkLookup, ChunkLookupEntry{
				Key: c.ChunkHash.TruncateHash(), XorbIndex: xorbIndex, ChunkIndex: uint32(i),
			})
		}
		xorbIndex += 1 + uint32(len(x.Chunks))
	}
	if err := WriteXorbChunkSequenceHeader(cw, BookendXorbHeader()); err != nil {
		return Footer{}, err
	}

	footer.FileLookupOffset = buf.n
	footer.FileLookupNumEntry = uint64(len(fileLookup))
	if err := WriteFileLookupTable(cw, fileLookup); err != nil {
		return Footer{}, err
	}

	footer.XorbLookupOffset = buf.n
	footer.XorbLookupNumEntry = uint64(len(xorbLookup))
	if err := WriteXorbLookupTable(cw, xorbLookup); err != nil {
		return Footer{}, err
	}

	SortChunkLookupEntries(chunkLookup)
	footer.ChunkLookupOffset = buf.n
	footer.ChunkLookupNumEntry = uint64(len(chunkLookup))
	if err := WriteChunkLookupTable(cw, chunkLookup); err != nil {
		return Footer{}, err
	}

	footer.FooterOffset = buf.n
	footer.ShardKeyExpiry = ^uint64(0)
	if err := WriteFooter(cw, footer); err != nil {
		return Footer{}, err
	}

	return footer, nil
}

// ReadShard parses a complete shard file from r, which must support
// seeking (the footer is read from the end first, then used to locate
// everything else).
func ReadShard(r io.ReadSeeker) (*Shard, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	header, err := ReadHeader(r)
	if err != nil {
		return nil, err
	}

	if _, err := r.Seek(-int64(footerSize), io.SeekEnd); err != nil {
		return nil, err
	}
	footer, err := ReadFooter(r)
	if err != nil {
		return nil, err
	}

	s := &Shard{Header: header, Footer: footer}

	if _, err := r.Seek(int64(footer.FileInfoOffset), io.SeekStart); err != nil {
		return nil, err
	}
	for {
		fh, err := ReadFileDataSequenceHeader(r)
		if err != nil {
			return nil, err
		}
		if fh.IsBookend() {
			break
		}
		entries := make([]FileDataSequenceEntry, fh.NumEntries)
		for i := range entries {
			e, err := ReadFileDataSequenceEntry(r)
			if err != nil {
				return nil, err
			}
			entries[i] = e
		}
		s.Files = append(s.Files, FileEntry{Header: fh, Entries: entries})
	}

	if _, err := r.Seek(int64(footer.XorbInfoOffset), io.SeekStart); err != nil {
		return nil, err
	}
	for {
		xh, err := ReadXorbChunkSequenceHeader(r)
		if err != nil {
			return nil, err
		}
		if xh.IsBookend() {
			break
		}
		chunks := make([]XorbChunkSequenceEntry, xh.NumEntries)
		for i := range chunks {
			c, err := ReadXorbChunkSequenceEntry(r)
			if err != nil {
				return nil, err
			}
			chunks[i] = c
		}
		s.Xorbs = append(s.Xorbs, XorbEntry{Header: xh, Chunks: chunks})
	}

	return s, nil
}

// FindFile returns the FileEntry for fileHash, or false if not present.
// Linear scan is fine here: a CAS server holds one Shard in memory per
// upload and looks files up by the handful of files in that upload, not
// across a large persisted shard index.
func (s *Shard) FindFile(fileHash merklehash.Hash) (FileEntry, bool) {
	for _, f := range s.Files {
		if f.Header.FileHash == fileHash {
			return f, true
		}
	}
	return FileEntry{}, false
}

// FindXorb returns the XorbEntry for xorbHash, or false if not present.
func (s *Shard) FindXorb(xorbHash merklehash.Hash) (XorbEntry, bool) {
	for _, x := range s.Xorbs {
		if x.Header.XorbHash == xorbHash {
			return x, true
		}
	}
	return XorbEntry{}, false
}

type countingWriter struct{ n uint64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += uint64(len(p))
	return len(p), nil
}
