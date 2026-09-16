package casserver

// gc.go: keep-set garbage collection for a standalone server - one
// whose store is the only copy of the data (a docker volume, a local
// folder, one xetd on a NAS mount). It is the opposite tool from
// internal/eviction: eviction treats the store as a cache and deletes
// the least-recently-used xorb whether or not a file still needs it,
// which is right for xet-proxyd (every xorb is re-fetchable from
// huggingface.co) and wrong here.
//
// The server cannot decide on its own what is still needed: a file
// pushed through git-lfs is "live" if some git ref still reaches its
// pointer, and only git knows that. So the operator says which files
// to keep - the SHA-256 OIDs `git lfs ls-files --all --long` prints for
// every repo the server backs - and Collect does the rest: keep OID ->
// Xet file hash -> reconstruction terms -> xorbs; every other file is
// dropped from every shard, the index is rebuilt from what remains, and
// every xorb no live file references is deleted from the store. Files
// the Hub shim's own registry lists (`hf upload`) have no git repo to
// list them from, so cmd/xetd adds them to the keep set itself.
//
// Why there is a grace period, and why its default is three weeks. A
// xet client keeps the shards of its own uploads in its cache for
// MDB_SHARD_LOCAL_CACHE_EXPIRATION (3 weeks in xet-core) and answers
// its own dedup questions from that cache without asking the server; it
// also caches every global-dedup answer until the expiry the server
// wrote into it. A shard the client uploads later may therefore
// reference a xorb the server has not heard about since then. Delete
// that xorb and the new file is unrecoverable, silently: the shard
// upload succeeds and the first download fails. So a xorb is only ever
// deleted when nothing has uploaded, fetched, or advertised it (see
// touchXorbs) for longer than the grace period, dedup answers are
// written to expire after the same three weeks (dedupAnswerTTL), and
// the default grace adds a day on top. An operator who knows every
// client's cache is empty may pass a shorter one; a client whose cache
// outlives it will produce a file that does not download.
//
// Not for a synced folder: another replica may be advertising, indexing
// or serving what this one deletes, and one that has not synced the
// rewritten shards yet re-indexes the old ones. cmd/xetd refuses GC
// with -sync-folder; BACKLOG has the epoch design that would lift that.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/guilt/xet-server/internal/auth"
	"github.com/guilt/xet-server/internal/merklehash"
	"github.com/guilt/xet-server/internal/shardformat"
	"github.com/guilt/xet-server/internal/storage"
)

// ClientShardCacheLifetime is how long a xet client keeps the shards of
// its own uploads in its cache and dedups against them without asking
// the server: MDB_SHARD_LOCAL_CACHE_EXPIRATION in xet-core's
// xet_data/src/processing/constants.rs (3 weeks, as of xet-core 1.6).
const ClientShardCacheLifetime = 21 * 24 * time.Hour

// dedupAnswerTTL is how long a global-dedup answer stays loadable in a
// client's cache (its footer's ShardKeyExpiry). The same three weeks as
// the client's own shards, so one number bounds every way a client can
// hold a reference the server has stopped tracking.
const dedupAnswerTTL = ClientShardCacheLifetime

// DefaultGCGrace is the grace period Collect applies when the request
// does not name one: the client cache lifetime plus a day of margin for
// clock differences and a push that started just before the cutoff.
const DefaultGCGrace = ClientShardCacheLifetime + 24*time.Hour

// ErrGCUnsupported is returned by Collect when the storage backend
// cannot enumerate and delete its blobs (s3store today).
var ErrGCUnsupported = errors.New("casserver: storage backend cannot enumerate and delete blobs")

// GCOptions is one collection's input.
type GCOptions struct {
	// Keep lists the hex SHA-256 of every file to keep, as git-lfs names
	// them (the OID in the pointer). Case-insensitive. An OID no shard
	// describes is counted in the report, not an error: it may be a
	// pointer whose object was never pushed, or one still in transit.
	Keep []string
	// Grace protects any xorb uploaded, fetched or advertised more
	// recently than this, and any xorb file that recently changed on
	// disk, from deletion in this run. Zero means no grace; see the
	// file comment before passing less than DefaultGCGrace.
	Grace time.Duration
	// DryRun computes and reports everything and changes nothing.
	DryRun bool
}

// GCReport is what one collection did, or (DryRun) would do.
type GCReport struct {
	DryRun       bool  `json:"dry_run"`
	GraceSeconds int64 `json:"grace_seconds"`

	// KeepOIDs is how many distinct OIDs the keep set had; KeepUnknown
	// how many of them no shard describes (sample in KeepUnknownSample).
	KeepOIDs          int      `json:"keep_oids"`
	KeepUnknown       int      `json:"keep_unknown"`
	KeepUnknownSample []string `json:"keep_unknown_sample,omitempty"`

	// FilesKept counts every file that stays; FilesRecent is the part of
	// it kept only because its shard arrived inside the grace period (a
	// push whose pointer the keep list could not name yet).
	FilesKept    int `json:"files_kept"`
	FilesRecent  int `json:"files_recent"`
	FilesDropped int `json:"files_dropped"`

	ShardsKept      int `json:"shards_kept"`
	ShardsRewritten int `json:"shards_rewritten"`
	ShardsDeleted   int `json:"shards_deleted"`

	// XorbsKept are referenced by a kept file. XorbsDeleted were not and
	// are gone (or, in a dry run, would be). XorbsDeferred were not
	// referenced either but are inside the grace period or being
	// fetched right now; a later run reconsiders them.
	XorbsKept          int   `json:"xorbs_kept"`
	XorbsKeptBytes     int64 `json:"xorbs_kept_bytes"`
	XorbsDeleted       int   `json:"xorbs_deleted"`
	XorbsDeletedBytes  int64 `json:"xorbs_deleted_bytes"`
	XorbsDeferred      int   `json:"xorbs_deferred"`
	XorbsDeferredBytes int64 `json:"xorbs_deferred_bytes"`

	// Errors are per-item failures (a xorb that could not be deleted, a
	// shard that could not be parsed) that did not stop the run.
	Errors []string `json:"errors,omitempty"`
}

const keepUnknownSampleSize = 10

// gcStore returns the Deleter+Enumerator behind s.xorbs, looking through
// a VerifyingStore, or ok=false.
func (s *Server) gcStore() (storage.Deleter, storage.Enumerator, bool) {
	store := s.xorbs
	for {
		d, hasDelete := store.(storage.Deleter)
		e, hasEnumerate := store.(storage.Enumerator)
		if hasDelete && hasEnumerate {
			return d, e, true
		}
		u, ok := store.(storage.Unwrapper)
		if !ok {
			return nil, nil, false
		}
		store = u.Unwrap()
	}
}

// Collect runs one garbage collection: see the file comment for what it
// keeps and deletes and why. Safe to call on a serving server; uploads
// and shard scans wait while the index is rebuilt (a few seconds for a
// large index), downloads do not. The caller should snapshot right
// after a run that changed anything (GCHandlerConfig.AfterCollect), so
// a crash cannot restore an index that still advertises deleted xorbs.
func (s *Server) Collect(ctx context.Context, opts GCOptions) (GCReport, error) {
	report := GCReport{DryRun: opts.DryRun, GraceSeconds: int64(opts.Grace / time.Second)}
	deleter, enumerator, ok := s.gcStore()
	if !ok {
		return report, ErrGCUnsupported
	}
	if opts.Grace < 0 {
		return report, errors.New("casserver: gc grace must not be negative")
	}

	s.gcMu.Lock()
	// --- mark: keep OIDs -> live files -> live xorbs -------------------
	keep := make(map[string]bool, len(opts.Keep))
	for _, oid := range opts.Keep {
		keep[strings.ToLower(strings.TrimSpace(oid))] = true
	}
	delete(keep, "")
	report.KeepOIDs = len(keep)
	live := make(map[merklehash.Hash]bool)
	s.sha256Mu.RLock()
	for oid := range keep {
		if h, ok := s.sha256ToXet[oid]; ok {
			live[h] = true
		} else {
			report.KeepUnknown++
			if len(report.KeepUnknownSample) < keepUnknownSampleSize {
				report.KeepUnknownSample = append(report.KeepUnknownSample, oid)
			}
		}
	}
	s.sha256Mu.RUnlock()
	sort.Strings(report.KeepUnknownSample)

	// A shard younger than the grace is a push that may still be in
	// progress (xorbs and shard uploaded, git ref not yet moved, so no
	// keep list can name its OID yet): every file it describes is live,
	// and the shard itself is left alone.
	s.chunkDedupMu.RLock()
	hashes := make([]merklehash.Hash, 0, len(s.shardBodies))
	bodies := make(map[merklehash.Hash][]byte, len(s.shardBodies))
	recent := make(map[merklehash.Hash]bool)
	now := time.Now()
	for h, b := range s.shardBodies {
		hashes = append(hashes, h)
		bodies[h] = b
		if now.Sub(s.shardAge(h)) < opts.Grace {
			recent[h] = true
		}
	}
	s.chunkDedupMu.RUnlock()
	sort.Slice(hashes, func(i, j int) bool { return bytes.Compare(hashes[i][:], hashes[j][:]) < 0 })
	for h := range recent {
		shard, err := shardformat.ReadShard(bytes.NewReader(bodies[h]))
		if err != nil {
			continue
		}
		for _, f := range shard.Files {
			if !live[f.Header.FileHash] {
				live[f.Header.FileHash] = true
				report.FilesRecent++
			}
		}
	}

	liveXorbs := make(map[merklehash.Hash]bool)
	keptRecon := make(map[merklehash.Hash][]shardformat.FileDataSequenceEntry)
	s.fileReconMu.RLock()
	for f, entries := range s.fileRecon {
		if !live[f] {
			report.FilesDropped++
			continue
		}
		report.FilesKept++
		keptRecon[f] = entries
		for _, e := range entries {
			liveXorbs[e.XorbHash] = true
		}
	}
	s.fileReconMu.RUnlock()

	// --- sweep the shards: drop dead files and dead xorb-infos ---------
	type survivor struct {
		hash merklehash.Hash
		body []byte
	}
	var survivors []survivor
	var obsolete []merklehash.Hash // shard files to remove once the new index is in place
	for _, h := range hashes {
		body := bodies[h]
		if recent[h] {
			survivors = append(survivors, survivor{h, body})
			report.ShardsKept++
			continue
		}
		shard, err := shardformat.ReadShard(bytes.NewReader(body))
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("shard %s: does not parse, kept as is: %v", h.Hex(), err))
			survivors = append(survivors, survivor{h, body})
			report.ShardsKept++
			continue
		}
		var files []shardformat.FileEntry
		for _, f := range shard.Files {
			if live[f.Header.FileHash] {
				files = append(files, f)
			}
		}
		var xorbs []shardformat.XorbEntry
		for _, x := range shard.Xorbs {
			if liveXorbs[x.Header.XorbHash] {
				xorbs = append(xorbs, x)
			}
		}
		switch {
		case len(files) == len(shard.Files) && len(xorbs) == len(shard.Xorbs):
			survivors = append(survivors, survivor{h, body})
			report.ShardsKept++
		case len(files) == 0 && len(xorbs) == 0:
			obsolete = append(obsolete, h)
			report.ShardsDeleted++
		default:
			var buf bytes.Buffer
			if _, err := shardformat.WriteShard(&buf, files, xorbs); err != nil {
				s.gcMu.Unlock()
				return report, fmt.Errorf("casserver: gc: rewrite shard %s: %w", h.Hex(), err)
			}
			newBody := buf.Bytes()
			newHash := merklehash.ComputeDataHash(newBody)
			if !opts.DryRun {
				// Persisted before the index changes, so a failure here
				// aborts with nothing lost, and a crash after it leaves
				// a file the next startup indexes as one more shard.
				if err := s.persistShard(newHash, newBody); err != nil {
					s.gcMu.Unlock()
					return report, fmt.Errorf("casserver: gc: persist rewritten shard: %w", err)
				}
			}
			survivors = append(survivors, survivor{newHash, newBody})
			obsolete = append(obsolete, h)
			report.ShardsRewritten++
		}
	}

	if !opts.DryRun {
		// --- rebuild the index from the surviving shards -------------
		s.fileReconMu.Lock()
		s.sha256Mu.Lock()
		s.chunkDedupMu.Lock()
		s.fileRecon = make(map[merklehash.Hash][]shardformat.FileDataSequenceEntry, len(keptRecon))
		s.sha256ToXet = make(map[string]merklehash.Hash)
		s.chunkHashToShard = make(map[merklehash.Hash]merklehash.Hash)
		s.shardBodies = make(map[merklehash.Hash][]byte, len(survivors))
		s.xorbToShard = make(map[merklehash.Hash]merklehash.Hash)
		for _, h := range obsolete {
			delete(s.shardIndexedAt, h)
		}
		s.chunkDedupMu.Unlock()
		s.sha256Mu.Unlock()
		s.fileReconMu.Unlock()
		for _, sv := range survivors {
			if err := s.indexShard(sv.body, sv.hash); err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("shard %s: re-index: %v", sv.hash.Hex(), err))
			}
		}
		// A live file no shard describes (one fed through
		// IngestFileRecon) keeps its entries.
		s.fileReconMu.Lock()
		for f, entries := range keptRecon {
			if _, ok := s.fileRecon[f]; !ok {
				s.fileRecon[f] = entries
			}
		}
		s.fileReconMu.Unlock()
		if dir := s.ShardDir(); dir != "" {
			for _, h := range obsolete {
				if err := os.Remove(filepath.Join(dir, h.Hex())); err != nil && !os.IsNotExist(err) {
					report.Errors = append(report.Errors, fmt.Sprintf("shard file %s: remove: %v", h.Hex(), err))
				}
			}
		}
	}
	s.gcMu.Unlock()

	// --- sweep the xorbs: delete what no live file references ----------
	type victim struct {
		key  string
		hash merklehash.Hash
		size int64
	}
	var victims []victim
	now = time.Now()
	err := enumerator.Enumerate(ctx, func(key string, size int64, modTime time.Time) error {
		h, err := merklehash.FromHex(key)
		if err != nil {
			return nil // not a xorb this server wrote
		}
		if liveXorbs[h] {
			report.XorbsKept++
			report.XorbsKeptBytes += size
			return nil
		}
		s.xorbMu.RLock()
		last := s.xorbLastAccess[h]
		inFlight := s.xorbInFlight[h] > 0
		s.xorbMu.RUnlock()
		if inFlight || now.Sub(last) < opts.Grace || now.Sub(modTime) < opts.Grace {
			report.XorbsDeferred++
			report.XorbsDeferredBytes += size
			return nil
		}
		victims = append(victims, victim{key, h, size})
		return nil
	})
	if err != nil {
		return report, fmt.Errorf("casserver: gc: enumerate xorbs: %w", err)
	}
	for _, v := range victims {
		if !opts.DryRun {
			if err := deleter.Delete(ctx, v.key); err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("xorb %s: delete: %v", v.key, err))
				continue
			}
			s.ForgetKey(v.key)
		}
		report.XorbsDeleted++
		report.XorbsDeletedBytes += v.size
	}

	slog.Info("casserver: gc finished", "dryRun", opts.DryRun, "filesKept", report.FilesKept, "filesDropped", report.FilesDropped,
		"shardsRewritten", report.ShardsRewritten, "shardsDeleted", report.ShardsDeleted,
		"xorbsDeleted", report.XorbsDeleted, "bytesFreed", report.XorbsDeletedBytes,
		"xorbsDeferred", report.XorbsDeferred, "keepUnknown", report.KeepUnknown, "errors", len(report.Errors))
	return report, nil
}

// shardAge is the later of when this process indexed shard h and its
// file's modification time in the shard dir (the durable record, which
// also covers a shard restored from a snapshot). Zero if neither is
// known, which any grace treats as old. Caller holds chunkDedupMu.
func (s *Server) shardAge(h merklehash.Hash) time.Time {
	t := s.shardIndexedAt[h]
	if dir := s.ShardDir(); dir != "" {
		if info, err := os.Stat(filepath.Join(dir, h.Hex())); err == nil && info.ModTime().After(t) {
			t = info.ModTime()
		}
	}
	return t
}

// GCPath is where cmd/xetd mounts GCHandler on the CAS port. It is not
// registered by Server.routes on purpose: a Server embedded in
// xet-proxyd is a cache and must never get a route that deletes by keep
// set, so only the standalone binary wires it.
const GCPath = V1 + "/gc"

// GCRequest is POST GCPath's JSON body.
type GCRequest struct {
	Keep []string `json:"keep"`
	// GraceSeconds overrides the server's default grace when present
	// (0 is "no grace"; see the file comment).
	GraceSeconds *int64 `json:"grace_seconds,omitempty"`
	DryRun       bool   `json:"dry_run"`
}

// GCHandlerConfig wires the parts of a collection that live outside
// this package.
type GCHandlerConfig struct {
	// DefaultGrace applies when the request names none.
	DefaultGrace time.Duration
	// ExtraKeep, if set, returns OIDs to keep in every run regardless
	// of the request (the Hub shim's file registry).
	ExtraKeep func() []string
	// AfterCollect, if set, runs after a collection that was not a dry
	// run: the place to snapshot. Its error is reported in the
	// response but the collection stands.
	AfterCollect func() error
	// Disabled, if non-empty, makes every request fail with 409 and
	// this reason (synced-folder mode).
	Disabled string
}

// maxGCRequestBytes bounds the keep list: 64 MB is about a million OIDs.
const maxGCRequestBytes = 64 << 20

// GCHandler serves POST GCPath: Collect with the request's keep set
// plus cfg.ExtraKeep, answering with the GCReport as JSON. Requires
// auth.ScopeAdmin, which only the operator's shared secret carries (or
// every request, with no auth configured).
func (s *Server) GCHandler(cfg GCHandlerConfig) http.Handler {
	return s.requireScope(auth.ScopeAdmin, func(w http.ResponseWriter, r *http.Request) {
		if cfg.Disabled != "" {
			httpError(w, "garbage collection is disabled: "+cfg.Disabled, http.StatusConflict)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxGCRequestBytes)
		var req GCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		opts := GCOptions{Keep: req.Keep, Grace: cfg.DefaultGrace, DryRun: req.DryRun}
		if req.GraceSeconds != nil {
			opts.Grace = time.Duration(*req.GraceSeconds) * time.Second
		}
		if cfg.ExtraKeep != nil {
			opts.Keep = append(append([]string(nil), opts.Keep...), cfg.ExtraKeep()...)
		}
		if opts.Grace < ClientShardCacheLifetime {
			slog.Warn("casserver: gc grace is shorter than a xet client keeps its own shards; a client that dedups from its cache against a deleted xorb produces a file that cannot be downloaded",
				"grace", opts.Grace, "clientShardCacheLifetime", ClientShardCacheLifetime, "dryRun", opts.DryRun)
		}
		report, err := s.Collect(r.Context(), opts)
		if err != nil {
			if errors.Is(err, ErrGCUnsupported) {
				httpError(w, err.Error(), http.StatusNotImplemented)
				return
			}
			httpError(w, "gc failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if !opts.DryRun && cfg.AfterCollect != nil {
			if err := cfg.AfterCollect(); err != nil {
				report.Errors = append(report.Errors, "after gc: "+err.Error())
			}
		}
		writeJSON(w, report)
	})
}
