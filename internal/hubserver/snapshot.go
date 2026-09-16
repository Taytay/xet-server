// Package hubserver's snapshot.go implements Server's persistence,
// mirroring casserver's snapshot.go: an atomic (stage-then-rename) JSON
// dump of every repo/revision/file this server knows about, and a load of
// that file on startup. See casserver/snapshot.go's package doc comment
// for the full tradeoff writeup (periodic checkpoint, not a WAL - writes
// between snapshots are lost on a hard crash).
//
// Server's live state (repoState/revisionState, each carrying its own
// sync.RWMutex) isn't itself JSON-marshalable, so this file defines flat,
// mutex-free snapshot structs and converts to/from them.
package hubserver

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/guilt/xet-server/internal/merklehash"
)

// fileRefSnapshot mirrors fileRef with no unexported-field restrictions
// (fileRef's fields are already all exported, but kept as a separate type
// so a future field added to fileRef for live-only purposes doesn't leak
// into the snapshot format without a deliberate decision).
type fileRefSnapshot struct {
	Path      string `json:"path"`
	SHA256Hex string `json:"sha256_hex"`
	XetHash   string `json:"xet_hash"` // Hex(); empty string if never backfilled
	Size      int64  `json:"size"`
}

type revisionSnapshot struct {
	Files      map[string]fileRefSnapshot `json:"files"`
	CommitOID  string                     `json:"commit_oid"`
	CommitSeen bool                       `json:"commit_seen"`
}

type repoSnapshot struct {
	RepoType  string                      `json:"repo_type"`
	RepoID    string                      `json:"repo_id"`
	Revisions map[string]revisionSnapshot `json:"revisions"`
	// Locks is the repo's git-lfs file locks (lfslocks.go). Additive:
	// a snapshot from before locks existed simply has none, and this
	// build reads it fine, so snapshotVersion stays at 1.
	Locks []lfsLock `json:"locks,omitempty"`
}

type snapshot struct {
	Version int            `json:"version"`
	Repos   []repoSnapshot `json:"repos"`
}

const snapshotVersion = 1

// Snapshot serializes s's repos/revisions/files to path via the same
// stage-to-temp-then-rename pattern casserver.Server.Snapshot and
// storage/fsstore.Store.Put both use, for the same reason: a reader must
// never observe a partially-written file.
//
// As with casserver's Snapshot, this briefly RLocks each repo/revision in
// turn rather than holding one lock across the whole operation - this
// server's own locking design (see the package doc comment) is already
// per-repo/per-revision specifically so unrelated repos don't contend, and
// a snapshot is not a stronger consistency boundary than any other
// multi-repo read already provides.
func (s *Server) Snapshot(path string) error {
	s.mu.RLock()
	repoKeys := make([]repoKey, 0, len(s.repos))
	repoStates := make([]*repoState, 0, len(s.repos))
	for k, rs := range s.repos {
		repoKeys = append(repoKeys, k)
		repoStates = append(repoStates, rs)
	}
	s.mu.RUnlock()

	snap := snapshot{Version: snapshotVersion}
	for i, key := range repoKeys {
		rs := repoStates[i]
		rs.mu.RLock()
		revisionNames := make([]string, 0, len(rs.revisions))
		revisionStates := make([]*revisionState, 0, len(rs.revisions))
		for name, vs := range rs.revisions {
			revisionNames = append(revisionNames, name)
			revisionStates = append(revisionStates, vs)
		}
		rs.mu.RUnlock()

		repoSnap := repoSnapshot{
			RepoType:  key.repoType,
			RepoID:    key.repoID,
			Revisions: make(map[string]revisionSnapshot, len(revisionNames)),
		}
		for j, name := range revisionNames {
			vs := revisionStates[j]
			vs.mu.RLock()
			files := make(map[string]fileRefSnapshot, len(vs.files))
			for path, ref := range vs.files {
				xetHashHex := ""
				if !ref.XetHash.IsZero() {
					xetHashHex = ref.XetHash.Hex()
				}
				files[path] = fileRefSnapshot{
					Path:      ref.Path,
					SHA256Hex: ref.SHA256Hex,
					XetHash:   xetHashHex,
					Size:      ref.Size,
				}
			}
			repoSnap.Revisions[name] = revisionSnapshot{
				Files:      files,
				CommitOID:  vs.commitOID,
				CommitSeen: vs.commitSeen,
			}
			vs.mu.RUnlock()
		}
		rs.locksMu.Lock()
		for _, l := range rs.sortedLocks() {
			repoSnap.Locks = append(repoSnap.Locks, *l)
		}
		rs.locksMu.Unlock()
		snap.Repos = append(snap.Repos, repoSnap)
	}

	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("hubserver: marshal snapshot: %w", err)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("hubserver: create snapshot temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("hubserver: write snapshot temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("hubserver: close snapshot temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("hubserver: rename snapshot into place: %w", err)
	}
	return nil
}

// LoadSnapshot reads a snapshot previously written by Snapshot and
// restores s's repos/revisions/files from it. Intended to be called once,
// before s starts serving any traffic. A missing file at path is not an
// error (a fresh server with no prior snapshot).
func (s *Server) LoadSnapshot(path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("hubserver: read snapshot: %w", err)
	}

	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("hubserver: parse snapshot: %w", err)
	}
	if snap.Version != snapshotVersion {
		return fmt.Errorf("hubserver: snapshot version %d, this build supports %d", snap.Version, snapshotVersion)
	}

	for _, repoSnap := range snap.Repos {
		key := repoKey{repoType: repoSnap.RepoType, repoID: repoSnap.RepoID}
		rs := &repoState{
			revisions: make(map[string]*revisionState, len(repoSnap.Revisions)),
			locks:     make(map[string]*lfsLock, len(repoSnap.Locks)),
		}
		for _, l := range repoSnap.Locks {
			lock := l
			rs.locks[lock.ID] = &lock
		}
		for name, revSnap := range repoSnap.Revisions {
			vs := &revisionState{
				files:      make(map[string]*fileRef, len(revSnap.Files)),
				commitOID:  revSnap.CommitOID,
				commitSeen: revSnap.CommitSeen,
			}
			for path, fileSnap := range revSnap.Files {
				ref := &fileRef{
					Path:      fileSnap.Path,
					SHA256Hex: fileSnap.SHA256Hex,
					Size:      fileSnap.Size,
				}
				if fileSnap.XetHash != "" {
					if h, err := merklehash.FromHex(fileSnap.XetHash); err == nil {
						ref.XetHash = h
					}
				}
				vs.files[path] = ref
			}
			rs.revisions[name] = vs
		}
		// Ensure the implicit default revision exists even if the
		// snapshot predates a code version that always created it -
		// matches getOrCreateRepo's own invariant that every repo has at
		// least "main" present.
		if _, ok := rs.revisions[defaultRevision]; !ok {
			rs.revisions[defaultRevision] = newRevisionState()
		}
		s.repos[key] = rs
	}
	return nil
}
