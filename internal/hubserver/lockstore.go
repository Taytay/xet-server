package hubserver

// lockstore.go: where git-lfs file locks live. Two implementations:
//
//   - memoryLockStore, the default: a per-repo map on repoState, saved in
//     the hub snapshot. Right for one server that owns its data dir.
//   - folderLockStore: locks as write-once files in a directory, for a
//     data dir that is a synced folder shared by several replicas (see
//     casserver/folder.go). A lock is a claim file; an unlock is a
//     tombstone file next to it; nothing is ever modified or deleted, so
//     any sync tool can carry them without conflicts. Which claim holds a
//     path is DERIVED by an election every replica computes the same
//     way: among untombstoned claims on a path, the earliest locked_at
//     wins, ties broken by id.
//
// The election is what makes offline locking honest. Two people on
// different machines can both "take" a lock while their folders are not
// yet synced - each replica answers 201, as it must, having no way to
// know better. Once the claims meet, both replicas agree on one holder;
// the other person's claim is superseded (invisible to list/verify, so
// their next push is refused by lock verification exactly as if they
// had never held it) and becomes the holder again if the winner unlocks
// first. Same shape as git-remote-dfs's ref election: no compare-and-
// swap anywhere, and a conflict is a visible outcome rather than
// corruption.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// lockStore is the small contract lfslocks.go's handlers program against.
type lockStore interface {
	// Create records lock (its ID, Path, LockedAt and Owner already set)
	// unless the path is held; then it returns the holder as existing.
	Create(repoID string, lock lfsLock) (created, existing *lfsLock, err error)
	// List returns every current lock of the repo, sorted by
	// (LockedAt, ID), one per path.
	List(repoID string) ([]*lfsLock, error)
	// Lookup returns the lock with the given id, or nil if there is none
	// (never created, or already released). A superseded claim in the
	// folder store is still returned here so its owner can release it.
	Lookup(repoID, id string) (*lfsLock, error)
	// Remove releases the lock with the given id; a no-op if absent.
	Remove(repoID, id string) error
}

// memoryLockStore keeps locks on repoState (snapshot.go persists them).
type memoryLockStore struct{ s *Server }

func (m memoryLockStore) Create(repoID string, lock lfsLock) (*lfsLock, *lfsLock, error) {
	rs := m.s.getOrCreateRepo("model", repoID)
	rs.locksMu.Lock()
	defer rs.locksMu.Unlock()
	for _, existing := range rs.locks {
		if existing.Path == lock.Path {
			return nil, existing, nil
		}
	}
	l := lock
	rs.locks[l.ID] = &l
	return &l, nil, nil
}

func (m memoryLockStore) List(repoID string) ([]*lfsLock, error) {
	rs := m.s.getOrCreateRepo("model", repoID)
	rs.locksMu.Lock()
	defer rs.locksMu.Unlock()
	return rs.sortedLocks(), nil
}

func (m memoryLockStore) Lookup(repoID, id string) (*lfsLock, error) {
	rs := m.s.getOrCreateRepo("model", repoID)
	rs.locksMu.Lock()
	defer rs.locksMu.Unlock()
	return rs.locks[id], nil
}

func (m memoryLockStore) Remove(repoID, id string) error {
	rs := m.s.getOrCreateRepo("model", repoID)
	rs.locksMu.Lock()
	defer rs.locksMu.Unlock()
	delete(rs.locks, id)
	return nil
}

// sortedLocks returns rs's locks in a stable order (creation time, then
// id) so cursors - plain offsets into this order - stay meaningful
// between pages. Caller holds rs.locksMu.
func (rs *repoState) sortedLocks() []*lfsLock {
	out := make([]*lfsLock, 0, len(rs.locks))
	for _, l := range rs.locks {
		out = append(out, l)
	}
	sortLocks(out)
	return out
}

func sortLocks(locks []*lfsLock) {
	sort.Slice(locks, func(i, j int) bool {
		if locks[i].LockedAt != locks[j].LockedAt {
			return locks[i].LockedAt < locks[j].LockedAt
		}
		return locks[i].ID < locks[j].ID
	})
}

// folderLockStore is the synced-folder implementation. Layout:
//
//	<dir>/<repo id, path-escaped>/<lock id>.lock.json   the claim
//	<dir>/<repo id, path-escaped>/<lock id>.unlock      its tombstone
//
// Every call lists the repo's directory; lock traffic is rare and a
// listing is the only way to see what other replicas have synced in.
// mu serializes this process's own create-after-check so two local
// requests cannot both claim a free path (other replicas are handled
// by the election, not by locking).
type folderLockStore struct {
	dir string
	mu  sync.Mutex
}

const (
	lockClaimSuffix     = ".lock.json"
	lockTombstoneSuffix = ".unlock"
)

func newFolderLockStore(dir string) (*folderLockStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("hubserver: create lock dir: %w", err)
	}
	return &folderLockStore{dir: dir}, nil
}

func (f *folderLockStore) repoDir(repoID string) string {
	return filepath.Join(f.dir, url.PathEscape(repoID))
}

// claims reads every claim of the repo, tombstoned ones excluded. A
// claim file that does not parse (half-synced) is skipped this time.
func (f *folderLockStore) claims(repoID string) ([]*lfsLock, error) {
	entries, err := os.ReadDir(f.repoDir(repoID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("hubserver: list lock dir: %w", err)
	}
	tombstoned := make(map[string]bool)
	var claimNames []string
	for _, e := range entries {
		name := e.Name()
		switch {
		case strings.HasSuffix(name, lockTombstoneSuffix):
			tombstoned[strings.TrimSuffix(name, lockTombstoneSuffix)] = true
		case strings.HasSuffix(name, lockClaimSuffix):
			claimNames = append(claimNames, strings.TrimSuffix(name, lockClaimSuffix))
		}
	}
	var out []*lfsLock
	for _, id := range claimNames {
		if tombstoned[id] {
			continue
		}
		data, err := os.ReadFile(filepath.Join(f.repoDir(repoID), id+lockClaimSuffix))
		if err != nil {
			continue
		}
		var l lfsLock
		if err := json.Unmarshal(data, &l); err != nil || l.ID != id {
			continue
		}
		out = append(out, &l)
	}
	sortLocks(out)
	return out, nil
}

// elect reduces claims (sorted) to one holder per path: the first claim
// on each path in (LockedAt, ID) order.
func elect(claims []*lfsLock) []*lfsLock {
	held := make(map[string]bool)
	var out []*lfsLock
	for _, c := range claims {
		if held[c.Path] {
			continue
		}
		held[c.Path] = true
		out = append(out, c)
	}
	return out
}

func (f *folderLockStore) Create(repoID string, lock lfsLock) (*lfsLock, *lfsLock, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	claims, err := f.claims(repoID)
	if err != nil {
		return nil, nil, err
	}
	for _, holder := range elect(claims) {
		if holder.Path == lock.Path {
			return nil, holder, nil
		}
	}
	if err := writeOnce(filepath.Join(f.repoDir(repoID), lock.ID+lockClaimSuffix), lock); err != nil {
		return nil, nil, err
	}
	l := lock
	return &l, nil, nil
}

func (f *folderLockStore) List(repoID string) ([]*lfsLock, error) {
	claims, err := f.claims(repoID)
	if err != nil {
		return nil, err
	}
	return elect(claims), nil
}

func (f *folderLockStore) Lookup(repoID, id string) (*lfsLock, error) {
	claims, err := f.claims(repoID)
	if err != nil {
		return nil, err
	}
	for _, c := range claims {
		if c.ID == id {
			return c, nil
		}
	}
	return nil, nil
}

func (f *folderLockStore) Remove(repoID, id string) error {
	tomb := struct {
		ReleasedAt string `json:"released_at"`
	}{time.Now().UTC().Format(time.RFC3339)}
	return writeOnce(filepath.Join(f.repoDir(repoID), id+lockTombstoneSuffix), tomb)
}

// writeOnce writes v as JSON to path unless path exists, staging through
// a temp file so a sync tool never ships a partial file under its final
// name.
func writeOnce(path string, v any) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		if _, statErr := os.Stat(path); statErr == nil {
			return nil
		}
		return err
	}
	return nil
}
