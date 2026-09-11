package hubserver

// tree.go implements GET /api/{repo_type}s/{repo_id}/tree/{revision}[/{path_in_repo}]:
// huggingface_hub's list_repo_tree, which snapshot_download (used by
// `hf download` for a whole-repo download) calls with recursive=True to
// enumerate every file before downloading them. Paired with
// handleRepoInfo (repo.go) — together these are the two endpoints a
// whole-repo download needs beyond what single-named-file downloads
// already used (xet-token issuance, resolve/HEAD).

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
)

// treeEntry mirrors one element of list_repo_tree's response array, as
// deserialized by RepoFile.__init__ (hf_api.py): path, size, and oid are
// required; xetHash is optional but is exactly what lets huggingface_hub
// skip a redundant per-file HEAD request during whole-repo download (the
// resolve path's response headers carry the same information for a
// single-file request, but the tree listing is the only chance to hand it
// over up front for every file in one round trip). "type": "file" is the
// discriminator list_repo_tree uses to build a RepoFile instead of a
// RepoFolder — this shim has no folder concept, so every entry is "file".
type treeEntry struct {
	Type    string `json:"type"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	OID     string `json:"oid"`
	XetHash string `json:"xetHash,omitempty"`
}

// handleListTree implements the tree-listing endpoint described above.
// pathInRepo filters to files whose path is under that prefix (a real
// Hub tree listing scopes to one folder unless the caller recurses from
// the root) — empty means the whole revision. Matches handleRepoInfo's
// read-path convention exactly, including the X-Error-Code header on a
// missing revision (see handleRepoInfo's doc comment for why that header
// is required, not cosmetic) — the repo is implicitly touched, but a
// nonexistent revision 404s rather than being silently created.
func (s *Server) handleListTree(w http.ResponseWriter, r *http.Request, repoType, repoID, revision, pathInRepo string) {
	rs := s.getOrCreateRepo(repoType, repoID)
	vs, ok := rs.getRevision(revision)
	if !ok {
		w.Header().Set("X-Error-Code", "RevisionNotFound")
		http.NotFound(w, r)
		return
	}

	vs.mu.RLock()
	entries := make([]treeEntry, 0, len(vs.files))
	for path, ref := range vs.files {
		if pathInRepo != "" && !isUnderPath(path, pathInRepo) {
			continue
		}
		entry := treeEntry{Type: "file", Path: path, Size: ref.Size, OID: ref.SHA256Hex}
		if !ref.XetHash.IsZero() {
			entry.XetHash = ref.XetHash.Hex()
		}
		entries = append(entries, entry)
	}
	vs.mu.RUnlock()

	// Deterministic order for test/debugging friendliness — the real Hub
	// API makes no ordering guarantee, and huggingface_hub's own
	// paginate() consumes results as a plain iterable, so any order is
	// spec-valid; this just avoids flaky-looking diffs between calls.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })

	// list_repo_tree's paginate() helper does `yield from r.json()`, i.e.
	// it expects a bare JSON array at the top level, not an object
	// wrapping one — unlike every other response this shim returns via
	// writeJSON.
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(entries); err != nil {
		slog.Error("hubserver: encode response", "error", err)
	}
}

// isUnderPath reports whether path is exactly pathInRepo or nested under
// it ("pathInRepo/..."), matching how a real Hub tree listing scopes to a
// folder and its descendants.
func isUnderPath(path, pathInRepo string) bool {
	if path == pathInRepo {
		return true
	}
	return len(path) > len(pathInRepo) && path[:len(pathInRepo)] == pathInRepo && path[len(pathInRepo)] == '/'
}
