package hubserver

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/guilt/xet-server/internal/auth"
)

// lfsBatchRequest is the git-lfs batch API request body huggingface_hub
// POSTs to /{repo_id}.git/info/lfs/objects/batch when uploading files.
// Only the fields this server actually uses are modeled; the client also
// sends operation/transfers/hash_algo/ref, which are ignored here.
type lfsBatchRequest struct {
	Operation string `json:"operation"`
	Objects   []struct {
		OID  string `json:"oid"`
		Size int64  `json:"size"`
	} `json:"objects"`
}

type lfsBatchObject struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

type lfsBatchResponse struct {
	Transfer string           `json:"transfer"`
	Objects  []lfsBatchObject `json:"objects"`
}

// maxLFSBatchBytes caps a batch request body. A batch is pure metadata -
// an oid/size pair per file - so even a maximally-sized legitimate request
// is a few tens of KB; this bound is generous by orders of magnitude while
// still refusing an unbounded body. Without it, json.Decode on r.Body will
// happily stream and buffer a multi-GB request.
const maxLFSBatchBytes = 4 * 1024 * 1024

// maxLFSBatchObjects caps how many objects one batch may carry. Real
// clients chunk uploads at 256 per batch (huggingface_hub's
// UPLOAD_BATCH_MAX_NUM_FILES), so 4096 leaves a 16x margin for a client
// that batches more aggressively. The cap matters because the response
// ECHOES every object back: without it a compact request listing millions
// of one-byte oids costs little to send but forces a proportional
// allocation here plus a far larger response - a cheap amplification
// primitive. Rejecting is correct rather than truncating, since silently
// dropping objects would make the client believe files were negotiated
// that never were.
const maxLFSBatchObjects = 4096

// handleLFSBatch implements POST /{repo_id}.git/info/lfs/objects/batch:
// the git-lfs batch endpoint through which huggingface_hub negotiates the
// upload transfer for every large file. This server only ever stores files
// as Xet xorbs, so it answers every upload batch with transfer "xet" and
// echoes the caller's oid/size pairs back unchanged - exactly the shape
// the real Hub returns for a Xet-enabled repo (see _commit_api.py's
// _upload_files: chosen_transfer == "xet" routes the client into
// _upload_xet_files, which fetches a xet-write-token and uploads the file
// to the paired CAS via hf_xet). Echoing oids means per-object actions are
// absent, which _validate_batch_actions tolerates (oid+size only) and the
// xet path ignores entirely.
func (s *Server) handleLFSBatch(w http.ResponseWriter, r *http.Request, repoID string) {
	if r.Method != http.MethodPost {
		httpErrorJSON(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireScope(w, r, auth.ScopeWrite) {
		return
	}

	// Bound the body before decoding it (same pattern as casserver's xorb
	// and shard uploads). MaxBytesReader also makes the failure explicit
	// via *http.MaxBytesError rather than a generic decode error, so an
	// oversize body reports 413 instead of a misleading 400 "invalid JSON".
	r.Body = http.MaxBytesReader(w, r.Body, maxLFSBatchBytes)

	var req lfsBatchRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			httpErrorJSON(w, "batch request body exceeds maximum size", http.StatusRequestEntityTooLarge)
			return
		}
		httpErrorJSON(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Decode stops at the end of the first JSON value and silently ignores
	// whatever follows, so `{"operation":"upload"}<anything>` would be
	// accepted as a well-formed batch. Require the body to be exactly one
	// JSON value: a second Decode must hit EOF (trailing whitespace is
	// skipped by the decoder, so a trailing newline still qualifies).
	// Lenient trailing-data handling is how two intermediaries end up
	// disagreeing about what a request said. (Found by FuzzLFSBatchHandler.)
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		httpErrorJSON(w, "unexpected trailing data after JSON body", http.StatusBadRequest)
		return
	}
	if req.Operation != "upload" {
		httpErrorJSON(w, "unsupported operation: "+req.Operation, http.StatusBadRequest)
		return
	}
	if len(req.Objects) > maxLFSBatchObjects {
		httpErrorJSON(w, "too many objects in one batch", http.StatusRequestEntityTooLarge)
		return
	}
	// repoID comes from extractRepoIDBefore, which (like parseResolvePath)
	// just takes the last two path segments - so a malformed path can yield
	// a repo ID with an empty namespace or name. Reject rather than create
	// state for an identity no real request can address.
	if ns, nm, found := strings.Cut(repoID, "/"); !found || ns == "" || nm == "" {
		httpErrorJSON(w, "malformed repo id", http.StatusBadRequest)
		return
	}

	// Only touch repo state once the request is known to be well-formed.
	// getOrCreateRepo allocates a repoState (plus a revisionState) the
	// first time any repo ID is seen, so doing it before validation would
	// let a flood of malformed requests bearing random repo IDs grow the
	// repos map without ever submitting a valid batch.
	s.getOrCreateRepo("model", repoID)

	resp := lfsBatchResponse{
		Transfer: "xet",
		// Exact capacity: len is already bounded above, so this cannot be
		// used to force an oversized allocation.
		Objects: make([]lfsBatchObject, 0, len(req.Objects)),
	}
	for _, obj := range req.Objects {
		resp.Objects = append(resp.Objects, lfsBatchObject{OID: obj.OID, Size: obj.Size})
	}
	writeJSON(w, resp)
}
