package hubserver

// lfslocks.go implements the git-lfs File Locking API
// (git-lfs/docs/api/locking.md): exclusive per-path locks so a team can
// serialize edits to unmergeable binaries. `git lfs lock <path>` creates
// one, `git lfs locks` lists them, `git lfs unlock` releases one, and
// with lfs.locksverify enabled every push first calls locks/verify and
// refuses to push a file someone else holds.
//
// Ownership is the authenticated Subject: the user name of a Basic
// credential (what git-lfs sends via git's credential helper), or the
// bound subject of a minted token. A deployment on the raw shared
// secret with no user name sees every lock owned by the same
// "shared-secret" subject, which makes "theirs" empty for everyone - so
// give each person their own Basic user name if locking matters.

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/guilt/xet-server/internal/auth"
)

// lfsLock is the wire shape of one lock; it is also what the snapshot
// persists, so the json tags double as the on-disk format.
type lfsLock struct {
	ID       string       `json:"id"`
	Path     string       `json:"path"`
	LockedAt string       `json:"locked_at"` // RFC 3339, second precision, UTC
	Owner    lfsLockOwner `json:"owner"`
}

type lfsLockOwner struct {
	Name string `json:"name"`
}

// Paging bounds for list and verify. git-lfs asks for 100 by default.
const (
	defaultLockPageSize = 100
	maxLockPageSize     = 1000
)

// handleLFSLocks dispatches POST (create) and GET (list) on /locks.
func (s *Server) handleLFSLocks(w http.ResponseWriter, r *http.Request, repoID string) {
	switch r.Method {
	case http.MethodPost:
		s.handleLFSLockCreate(w, r, repoID)
	case http.MethodGet:
		s.handleLFSLockList(w, r, repoID)
	default:
		lfsError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleLFSLockCreate(w http.ResponseWriter, r *http.Request, repoID string) {
	principal, ok := s.lfsAuthenticate(w, r, auth.ScopeWrite)
	if !ok {
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxLFSBatchBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		lfsError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Path == "" {
		lfsError(w, "path is required", http.StatusBadRequest)
		return
	}

	rs := s.getOrCreateRepo("model", repoID)
	rs.locksMu.Lock()
	defer rs.locksMu.Unlock()
	for _, existing := range rs.locks {
		if existing.Path == req.Path {
			writeLFSJSON(w, http.StatusConflict, struct {
				Lock    *lfsLock `json:"lock"`
				Message string   `json:"message"`
			}{existing, "already created lock"})
			return
		}
	}
	id, err := randomToken()
	if err != nil {
		lfsError(w, "generate lock id: "+err.Error(), http.StatusInternalServerError)
		return
	}
	lock := &lfsLock{
		ID:       id,
		Path:     req.Path,
		LockedAt: time.Now().UTC().Format(time.RFC3339),
		Owner:    lfsLockOwner{Name: principal.Subject()},
	}
	rs.locks[id] = lock
	writeLFSJSON(w, http.StatusCreated, struct {
		Lock *lfsLock `json:"lock"`
	}{lock})
}

// sortedLocks returns rs's locks in a stable order (creation time, then
// id) so cursors - plain offsets into this order - stay meaningful
// between pages. Caller holds rs.locksMu.
func (rs *repoState) sortedLocks() []*lfsLock {
	out := make([]*lfsLock, 0, len(rs.locks))
	for _, l := range rs.locks {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LockedAt != out[j].LockedAt {
			return out[i].LockedAt < out[j].LockedAt
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// page applies cursor/limit to locks and returns the page plus the next
// cursor ("" when this was the last page).
func page(locks []*lfsLock, cursor string, limit int) ([]*lfsLock, string) {
	offset := 0
	if cursor != "" {
		if n, err := strconv.Atoi(cursor); err == nil && n >= 0 {
			offset = n
		}
	}
	if limit <= 0 {
		limit = defaultLockPageSize
	}
	if limit > maxLockPageSize {
		limit = maxLockPageSize
	}
	if offset >= len(locks) {
		return []*lfsLock{}, ""
	}
	end := offset + limit
	next := ""
	if end < len(locks) {
		next = strconv.Itoa(end)
	} else {
		end = len(locks)
	}
	return locks[offset:end], next
}

func (s *Server) handleLFSLockList(w http.ResponseWriter, r *http.Request, repoID string) {
	if _, ok := s.lfsAuthenticate(w, r, auth.ScopeRead); !ok {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))

	rs := s.getOrCreateRepo("model", repoID)
	rs.locksMu.Lock()
	all := rs.sortedLocks()
	rs.locksMu.Unlock()

	filtered := make([]*lfsLock, 0, len(all))
	for _, l := range all {
		if p := q.Get("path"); p != "" && l.Path != p {
			continue
		}
		if id := q.Get("id"); id != "" && l.ID != id {
			continue
		}
		filtered = append(filtered, l)
	}
	locks, next := page(filtered, q.Get("cursor"), limit)
	writeLFSJSON(w, http.StatusOK, struct {
		Locks      []*lfsLock `json:"locks"`
		NextCursor string     `json:"next_cursor,omitempty"`
	}{locks, next})
}

func (s *Server) handleLFSLocksVerify(w http.ResponseWriter, r *http.Request, repoID string) {
	if r.Method != http.MethodPost {
		lfsError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	principal, ok := s.lfsAuthenticate(w, r, auth.ScopeWrite)
	if !ok {
		return
	}
	var req struct {
		Cursor string `json:"cursor"`
		Limit  int    `json:"limit"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxLFSBatchBytes)
	// An empty body is legal here (git-lfs sends {} or nothing).
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
		lfsError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	rs := s.getOrCreateRepo("model", repoID)
	rs.locksMu.Lock()
	all := rs.sortedLocks()
	rs.locksMu.Unlock()

	locks, next := page(all, req.Cursor, req.Limit)
	ours, theirs := []*lfsLock{}, []*lfsLock{}
	for _, l := range locks {
		if l.Owner.Name == principal.Subject() {
			ours = append(ours, l)
		} else {
			theirs = append(theirs, l)
		}
	}
	writeLFSJSON(w, http.StatusOK, struct {
		Ours       []*lfsLock `json:"ours"`
		Theirs     []*lfsLock `json:"theirs"`
		NextCursor string     `json:"next_cursor,omitempty"`
	}{ours, theirs, next})
}

func (s *Server) handleLFSUnlock(w http.ResponseWriter, r *http.Request, repoID, id string) {
	if r.Method != http.MethodPost {
		lfsError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	principal, ok := s.lfsAuthenticate(w, r, auth.ScopeWrite)
	if !ok {
		return
	}
	var req struct {
		Force bool `json:"force"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxLFSBatchBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
		lfsError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	rs := s.getOrCreateRepo("model", repoID)
	rs.locksMu.Lock()
	defer rs.locksMu.Unlock()
	lock, exists := rs.locks[id]
	if !exists {
		lfsError(w, "lock not found", http.StatusNotFound)
		return
	}
	if lock.Owner.Name != principal.Subject() && !req.Force {
		lfsError(w, "lock is owned by "+lock.Owner.Name+"; pass --force to remove another user's lock", http.StatusForbidden)
		return
	}
	delete(rs.locks, id)
	writeLFSJSON(w, http.StatusOK, struct {
		Lock *lfsLock `json:"lock"`
	}{lock})
}
