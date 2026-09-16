package casserver

// folder.go: the parts of the CAS that let its data directory be a
// synced folder (Dropbox, Syncthing, a NAS mount) shared by several
// xetd replicas, each serving its own machine from its own copy.
//
// The rule that makes that safe is the same one git-remote-dfs uses for
// git history on a dumb folder: every authoritative file is write-once
// and named by its content, so two replicas writing "the same" file
// write identical bytes and a sync tool never has to merge anything.
// Xorbs already are (fsstore keys them by hash). This file adds the
// second half: shard bodies persisted the same way under <data>/shards,
// and an index that is DERIVED from those files rather than stored in a
// mutable snapshot. A replica learns what the others uploaded by
// rescanning the shard directory - at startup, on a timer, and whenever
// a lookup misses - and lazily derives a synced xorb's footer from its
// bytes the first time it is needed.
//
// What a replica cannot know is whether the sync tool has delivered
// every xorb a shard references yet; ErrContentUnavailable is how that
// surfaces, so the LFS bridge can tell a client to retry instead of
// serving a truncated file.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/guilt/xet-server/internal/merklehash"
	"github.com/guilt/xet-server/internal/storage"
	"github.com/guilt/xet-server/internal/xorbformat"
)

// ErrContentUnavailable is returned (via errors.Is) by ReconstructFile
// and reported by MissingXorbs when a file's shard is known but at
// least one xorb it references is not in this replica's store yet. On a
// synced folder that is the normal state of a file another machine
// pushed moments ago; the caller should ask the client to retry.
var ErrContentUnavailable = errors.New("casserver: content not yet available on this replica")

// rescanMinInterval throttles miss-driven rescans: a burst of lookups
// for unknown hashes (a client checking dedup for brand-new content)
// costs at most one directory listing per interval.
const rescanMinInterval = time.Second

// folderState is the shard-directory bookkeeping, kept off Server's
// main struct so the fields read as one unit. Zero value: no shard dir.
type folderState struct {
	mu  sync.Mutex
	dir string // "" until SetShardDir
	// lastMissScan is when a lookup miss last triggered a scan. Kept
	// apart from the periodic loop's scans on purpose: a timer that
	// fires every second must not starve the miss path, which is what
	// makes a file visible the moment a client first asks for it.
	lastMissScan time.Time
	// scannedMtime is the shard dir's modification time as observed
	// just before the last scan listed it. A directory's mtime moves
	// whenever an entry is added or renamed into it - which is how every
	// sync tool delivers a file - so an unchanged mtime means a miss can
	// skip the listing, and a changed one means it must not wait.
	scannedMtime time.Time
}

// SetShardDir makes s persist every ingested shard body to dir (named by
// the shard's content hash) and rescan dir for shards written by other
// replicas. Call before the first request. It does not scan by itself;
// call ScanShards once after any snapshot has been loaded.
func (s *Server) SetShardDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("casserver: create shard dir: %w", err)
	}
	s.folder.mu.Lock()
	s.folder.dir = dir
	s.folder.mu.Unlock()
	return nil
}

// ShardDir returns the directory set by SetShardDir, or "".
func (s *Server) ShardDir() string {
	s.folder.mu.Lock()
	defer s.folder.mu.Unlock()
	return s.folder.dir
}

// persistShard writes body to <shardDir>/<hash> if no such file exists,
// staging through a temp file and renaming so a sync tool (or a
// concurrent ScanShards) never sees a partial shard under its final
// name. A no-op without a shard dir.
func (s *Server) persistShard(hash merklehash.Hash, body []byte) error {
	dir := s.ShardDir()
	if dir == "" {
		return nil
	}
	final := filepath.Join(dir, hash.Hex())
	if _, err := os.Stat(final); err == nil {
		return nil
	}
	tmp, err := os.CreateTemp(dir, hash.Hex()+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, final); err != nil {
		os.Remove(tmpName)
		// Another writer (a replica's sync, a concurrent upload of the
		// same shard) may have landed the identical file first.
		if _, statErr := os.Stat(final); statErr == nil {
			return nil
		}
		return err
	}
	return nil
}

// ScanShards indexes every shard file in the shard dir that this server
// has not indexed yet. A file is only trusted if its content hashes to
// its name: a sync tool's half-delivered file (or anything else in the
// directory) fails that check and is skipped until a later scan sees it
// whole. Returns how many shards were newly indexed. Safe to call
// concurrently with requests.
func (s *Server) ScanShards(ctx context.Context) (added int, err error) {
	dir := s.ShardDir()
	if dir == "" {
		return 0, nil
	}
	var mtime time.Time
	if info, err := os.Stat(dir); err == nil {
		mtime = info.ModTime()
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("casserver: list shard dir: %w", err)
	}
	s.gcMu.RLock()
	defer s.gcMu.RUnlock()
	s.folder.mu.Lock()
	s.folder.scannedMtime = mtime
	s.folder.mu.Unlock()
	// Oldest first, so on a cold start the index grows in upload order
	// and "first shard to reference a chunk wins" stays stable across
	// replicas.
	sort.Slice(entries, func(i, j int) bool { return entryModTime(entries[i]).Before(entryModTime(entries[j])) })
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return added, err
		}
		if e.IsDir() {
			continue
		}
		hash, err := merklehash.FromHex(e.Name())
		if err != nil {
			continue // a temp file, a sync tool's marker, an editor's droppings
		}
		s.chunkDedupMu.RLock()
		_, known := s.shardBodies[hash]
		s.chunkDedupMu.RUnlock()
		if known {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			slog.Debug("shard scan: unreadable, will retry", "file", e.Name(), "error", err)
			continue
		}
		if merklehash.ComputeDataHash(body) != hash {
			slog.Debug("shard scan: content does not hash to its name (partial sync?), skipping for now", "file", e.Name(), "bytes", len(body))
			continue
		}
		if err := s.indexShard(body, hash); err != nil {
			slog.Warn("shard scan: file hashes correctly but does not parse; ignoring", "file", e.Name(), "error", err)
			continue
		}
		added++
	}
	if added > 0 {
		slog.Info("shard scan indexed shards written by other replicas", "added", added)
	}
	return added, nil
}

func entryModTime(e os.DirEntry) time.Time {
	info, err := e.Info()
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// rescanOnMiss is what a lookup calls when it finds nothing: if a shard
// dir is configured, scan now so a file another replica just synced in
// becomes visible without waiting for the timer. The scan is skipped
// only when the directory's mtime has not moved since the last scan AND
// a miss-driven scan ran within rescanMinInterval, so a change is seen
// at once while a stream of misses on a quiet directory costs one stat
// each. Returns true if the scan added anything, meaning the caller
// should retry its lookup.
func (s *Server) rescanOnMiss() bool {
	s.folder.mu.Lock()
	due := s.folder.dir != ""
	if due {
		changed := true
		if info, err := os.Stat(s.folder.dir); err == nil {
			changed = !info.ModTime().Equal(s.folder.scannedMtime)
		}
		due = changed || time.Since(s.folder.lastMissScan) >= rescanMinInterval
	}
	if due {
		// Claim the slot before scanning so concurrent misses do not
		// all list the directory.
		s.folder.lastMissScan = time.Now()
	}
	s.folder.mu.Unlock()
	if !due {
		return false
	}
	added, err := s.ScanShards(context.Background())
	if err != nil {
		slog.Warn("rescan on miss failed", "error", err)
	}
	return added > 0
}

// RunRescanLoop calls ScanShards every interval until ctx is done. For a
// replica on a synced folder this is how it notices other machines'
// uploads before any client asks for them (so their chunks take part in
// dedup on the next push here).
func (s *Server) RunRescanLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.ScanShards(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("periodic shard scan failed", "error", err)
			}
		}
	}
}

// xorbMeta returns hash's footer and raw length, deriving both from the
// stored bytes if the index does not have them (a xorb that arrived by
// sync, or one uploaded before a crash lost the snapshot). ok is false
// only if the xorb is not in the store either. The derived footer is
// cached, so the full read happens once per xorb per process.
func (s *Server) xorbMeta(ctx context.Context, hash merklehash.Hash) (footer xorbformat.FooterV1, rawLen int64, ok bool) {
	s.xorbMu.RLock()
	footer, ok = s.xorbFooters[hash]
	rawLen = s.xorbRawLength[hash]
	s.xorbMu.RUnlock()
	if ok {
		return footer, rawLen, true
	}

	rc, err := s.xorbs.Get(ctx, hash.Hex())
	if err != nil {
		if !errors.Is(err, storage.ErrNotFound) {
			slog.Warn("open stored xorb to derive its footer", "xorb", hash.Hex(), "error", err)
		}
		return xorbformat.FooterV1{}, 0, false
	}
	defer rc.Close()
	// DeriveFooter needs a seekable ReaderAt; stage to a temp file like
	// IngestXorb does rather than hold a whole xorb in memory.
	tmp, err := os.CreateTemp("", "xet-xorb-footer-*")
	if err != nil {
		slog.Warn("stage xorb for footer derivation", "xorb", hash.Hex(), "error", err)
		return xorbformat.FooterV1{}, 0, false
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	size, err := io.Copy(tmp, rc)
	if err != nil {
		slog.Warn("read stored xorb to derive its footer", "xorb", hash.Hex(), "error", err)
		return xorbformat.FooterV1{}, 0, false
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return xorbformat.FooterV1{}, 0, false
	}
	footer, computed, err := xorbformat.DeriveFooter(tmp)
	if err != nil || computed != hash {
		// A half-synced xorb hashes wrong or does not parse; treat it
		// as absent until the sync completes.
		slog.Debug("stored xorb does not verify (partial sync?)", "xorb", hash.Hex(), "error", err, "bytes", size)
		return xorbformat.FooterV1{}, 0, false
	}

	s.xorbMu.Lock()
	if existing, raced := s.xorbFooters[hash]; raced {
		footer = existing
		size = s.xorbRawLength[hash]
	} else {
		s.xorbFooters[hash] = footer
		s.xorbRawLength[hash] = size
		s.xorbLastAccess[hash] = time.Now()
	}
	s.xorbMu.Unlock()
	return footer, size, true
}

// MissingXorbs reports which xorbs the file identified by fileHash
// needs that this replica does not have (or has only partially, from a
// sync in progress). ErrUnknownFile if no shard describes the file.
// An empty result means ReconstructFile can serve the whole file now.
func (s *Server) MissingXorbs(ctx context.Context, fileHash merklehash.Hash) ([]merklehash.Hash, error) {
	s.fileReconMu.RLock()
	entries, ok := s.fileRecon[fileHash]
	s.fileReconMu.RUnlock()
	if !ok {
		return nil, ErrUnknownFile
	}
	seen := make(map[merklehash.Hash]bool)
	var missing []merklehash.Hash
	for _, e := range entries {
		if seen[e.XorbHash] {
			continue
		}
		seen[e.XorbHash] = true
		if _, _, ok := s.xorbMeta(ctx, e.XorbHash); !ok {
			missing = append(missing, e.XorbHash)
		}
	}
	return missing, nil
}
