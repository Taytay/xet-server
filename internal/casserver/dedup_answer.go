package casserver

import (
	"bytes"
	"sort"

	"github.com/guilt/xet-server/internal/merklehash"
	"github.com/guilt/xet-server/internal/shardformat"
)

// maxDedupAnswerChunks caps the chunk entries in one global-dedup answer:
// 65536 entries is about 3 MB on the wire and describes about 4 GB of
// content at the default 64 KB chunk. The client caches every answer as
// a shard file, so the bound keeps one query from pulling a whole repo's
// index; the xorb the chunk lives in always fits.
const maxDedupAnswerChunks = 1 << 16

// dedupAnswer builds the body of GET /v1/chunks/{prefix}/{hash} for a
// chunk the server knows: a shard whose xorb-info lists the xorb the
// chunk lives in first, then every other xorb of every file that
// references that xorb, in term order. No file entries: xet-core's own
// reference client (LocalClient::query_for_global_dedup_shard) answers
// with an xorb-only shard too, and the querying client uses only the
// chunk lookup.
//
// Why not just the shard that introduced the chunk, as before: an upload
// shard's xorb-info lists only the xorbs that upload created, so a chunk
// born in a two-chunk edit maps to a two-chunk shard. xet-core queries
// the first chunk of a file and then at most once per 256 chunks, so a
// second machine editing a file whose first chunk had been edited before
// got one answer covering two chunks and re-uploaded everything else,
// with the file's other xorbs on the server the whole time.
//
// The stored shard is returned unchanged if its xorb-info does not list
// the chunk (a shard indexed by an older build), so the answer is never
// worse than before.
func (s *Server) dedupAnswer(chunk merklehash.Hash) ([]byte, bool) {
	s.chunkDedupMu.RLock()
	shardHash, known := s.chunkHashToShard[chunk]
	body := s.shardBodies[shardHash]
	s.chunkDedupMu.RUnlock()
	if !known || body == nil {
		return nil, false
	}
	parsed := map[merklehash.Hash]*shardformat.Shard{}
	home, ok := s.xorbEntryForChunk(shardHash, body, chunk, parsed)
	if !ok {
		return body, true
	}

	xorbs := []shardformat.XorbEntry{home}
	seen := map[merklehash.Hash]bool{home.Header.XorbHash: true}
	total := len(home.Chunks)
	for _, x := range s.xorbsOfFilesReferencing(home.Header.XorbHash) {
		if seen[x] {
			continue
		}
		seen[x] = true
		entry, ok := s.xorbEntry(x, parsed)
		if !ok {
			continue
		}
		if total+len(entry.Chunks) > maxDedupAnswerChunks {
			break
		}
		xorbs = append(xorbs, entry)
		total += len(entry.Chunks)
	}

	var buf bytes.Buffer
	if _, err := shardformat.WriteShard(&buf, nil, xorbs); err != nil {
		return body, true
	}
	return buf.Bytes(), true
}

// xorbsOfFilesReferencing returns, in term order, the xorb hashes of
// every file whose reconstruction references xorb; files are visited in
// hash order so the answer for a chunk is deterministic. Duplicates are
// left to the caller.
func (s *Server) xorbsOfFilesReferencing(xorb merklehash.Hash) []merklehash.Hash {
	s.fileReconMu.RLock()
	defer s.fileReconMu.RUnlock()
	var files []merklehash.Hash
	for fileHash, entries := range s.fileRecon {
		for _, e := range entries {
			if e.XorbHash == xorb {
				files = append(files, fileHash)
				break
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return bytes.Compare(files[i][:], files[j][:]) < 0 })
	var out []merklehash.Hash
	for _, fileHash := range files {
		for _, e := range s.fileRecon[fileHash] {
			out = append(out, e.XorbHash)
		}
	}
	return out
}

// xorbEntry returns xorb's chunk list from the shard whose xorb-info
// introduced it. parsed memoizes shard parses across one answer.
func (s *Server) xorbEntry(xorb merklehash.Hash, parsed map[merklehash.Hash]*shardformat.Shard) (shardformat.XorbEntry, bool) {
	s.chunkDedupMu.RLock()
	shardHash, known := s.xorbToShard[xorb]
	body := s.shardBodies[shardHash]
	s.chunkDedupMu.RUnlock()
	if !known || body == nil {
		return shardformat.XorbEntry{}, false
	}
	shard, ok := parseMemo(shardHash, body, parsed)
	if !ok {
		return shardformat.XorbEntry{}, false
	}
	for _, x := range shard.Xorbs {
		if x.Header.XorbHash == xorb {
			return x, true
		}
	}
	return shardformat.XorbEntry{}, false
}

// xorbEntryForChunk returns the xorb entry of the stored shard (content
// hash shardHash, body) whose chunk list contains chunk.
func (s *Server) xorbEntryForChunk(shardHash merklehash.Hash, body []byte, chunk merklehash.Hash, parsed map[merklehash.Hash]*shardformat.Shard) (shardformat.XorbEntry, bool) {
	shard, ok := parseMemo(shardHash, body, parsed)
	if !ok {
		return shardformat.XorbEntry{}, false
	}
	for _, x := range shard.Xorbs {
		for _, c := range x.Chunks {
			if c.ChunkHash == chunk {
				return x, true
			}
		}
	}
	return shardformat.XorbEntry{}, false
}

func parseMemo(shardHash merklehash.Hash, body []byte, parsed map[merklehash.Hash]*shardformat.Shard) (*shardformat.Shard, bool) {
	if shard, ok := parsed[shardHash]; ok {
		return shard, shard != nil
	}
	shard, err := shardformat.ReadShard(bytes.NewReader(body))
	if err != nil {
		parsed[shardHash] = nil
		return nil, false
	}
	parsed[shardHash] = shard
	return shard, true
}
