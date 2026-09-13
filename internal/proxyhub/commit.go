package proxyhub

// commit.go: POST /api/{repo_type}s/{repo_id}/commit/{revision} (ndjson
// body) and POST /api/{repo_type}s/{repo_id}/preupload/{revision} -
// both relayed to the real Hub write-through, uncached - only the real
// Hub can accept a real commit or negotiate a real upload mode. The
// commit's effect is mirrored into Embedded (via IngestFile/
// IngestCommit) so a subsequent read of the same data is already a
// local hit.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	"github.com/guilt/xet-server/internal/auth"
	"github.com/guilt/xet-server/internal/hfclient"
	"github.com/guilt/xet-server/internal/merklehash"
)

// commitLine mirrors one line of the ndjson commit payload
// huggingface_hub sends - see internal/hubserver/commit.go's commitLine
// for the identical shape this duplicates.
type commitLine struct {
	Key   string `json:"key"`
	Value struct {
		Path string `json:"path"`
		OID  string `json:"oid"`
		Size int64  `json:"size"`
	} `json:"value"`
}

// handleCommit parses the caller's ndjson commit payload, relays it to
// the real Hub, and - on success - mirrors every lfsFile entry into
// Embedded using the REAL upstream commit OID (never a fabricated one;
// see hubserver.Server.IngestCommit's doc comment on why that matters).
// Files are ingested with a zero XetHash (not yet known - a fresh
// commit's Xet hash is only ever backfilled by a real shard upload,
// which this proxy relays but does not itself parse independent of a
// real client's own upload - see internal/proxycas's handleUploadShard
// for where that actually happens); a subsequent resolve of one of
// these files still works via Embedded's own CAS bridge once that
// shard's Xet hash becomes known, exactly as a real hubserver would.
func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request, repoType, repoID, revision string) {
	// Relay the caller's exact ndjson body verbatim: the LFS-pointer
	// lines carry fields a decode-and-reencode would drop, and the real
	// Hub parses them. The raw body is parsed separately only for the
	// Embedded bookkeeping below.
	raw, err := readHubWriteBody(w, r)
	if err != nil {
		return
	}
	path := "/api/" + repoType + "s/" + repoID + "/commit/" + url.PathEscape(revision)
	resp, err := s.Hub.RelayRaw(r.Context(), auth.CredentialFromRequest(r), http.MethodPost, path, "application/x-ndjson", bytes.NewReader(raw))
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		httpErrorJSON(w, "read upstream commit response: "+err.Error(), http.StatusBadGateway)
		return
	}

	if !s.NoCache {
		// Best-effort bookkeeping mirror of the commit's effect into
		// Embedded, so a subsequent read is already a local hit. Failures
		// are logged, never fatal - the caller's own commit already
		// succeeded upstream.
		scanner := bufio.NewScanner(bytes.NewReader(raw))
		scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var cl commitLine
			if err := json.Unmarshal(line, &cl); err != nil {
				slog.Debug("proxyhub: commit ingest: skip unparsable ndjson line", "error", err)
				continue
			}
			if cl.Key != "lfsFile" {
				continue
			}
			s.Embedded.IngestFile(repoType, repoID, revision, cl.Value.Path, cl.Value.OID, cl.Value.Size, merklehash.Hash{})
		}
		var result hfclient.CommitResult
		if err := json.Unmarshal(respBody, &result); err == nil && result.CommitOID != "" {
			s.Embedded.IngestCommit(repoType, repoID, revision, result.CommitOID)
		}
	}

	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

// handlePreupload relays the caller's file list to the real Hub's
// preupload negotiation, uncached - the negotiated upload mode can
// depend on upstream-side state this proxy has no way to reason about
// locally. The caller's exact request body is forwarded verbatim: real
// huggingface_hub includes a per-file `sample` field that a
// decode-and-reencode would silently drop, and the real Hub rejects the
// result ("expected string, received undefined at files[0].sample").
func (s *Server) handlePreupload(w http.ResponseWriter, r *http.Request, repoType, repoID, revision string) {
	body, err := readHubWriteBody(w, r)
	if err != nil {
		return
	}
	path := "/api/" + repoType + "s/" + repoID + "/preupload/" + url.PathEscape(revision)
	resp, err := s.Hub.RelayRaw(r.Context(), auth.CredentialFromRequest(r), http.MethodPost, path, "application/json", bytes.NewReader(body))
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	defer resp.Body.Close()
	relayHubResponse(w, resp)
}

// relayHubResponse copies an upstream response's status, headers, and
// body to w verbatim - the transparent-relay counterpart to
// writeUpstreamError, used by the write-path handlers that forward the
// real Hub's response unchanged (preupload's negotiated modes, commit's
// commit-URL/OID payload) so real clients see exactly what the real Hub
// returned.
// relayHubResponse copies an upstream response's status, headers, and
// body to w verbatim - the transparent-relay counterpart to
// writeUpstreamError, used by the write-path handlers that forward the
// real Hub's response unchanged (preupload's negotiated modes, commit's
// commit-URL/OID payload) so real clients see exactly what the real Hub
// returned.
func relayHubResponse(w http.ResponseWriter, resp *http.Response) {
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// maxHubWriteBodyBytes caps the size of a Hub write request body this
// proxy buffers before relaying (preupload, commit ndjson, Git LFS
// batch/object). These are metadata-sized (a commit's file list, an LFS
// batch negotiation); buffering them is fine, but an unbounded read is a
// memory-exhaustion vector, so an oversize body gets 413 instead - the
// same defense casserver/proxycas apply to xorb/shard uploads.
const maxHubWriteBodyBytes = 64 * 1024 * 1024

// readHubWriteBody reads r.Body, bounded to maxHubWriteBodyBytes via
// http.MaxBytesReader. Returns err (already written as a 413) if the body
// exceeds the cap; otherwise returns the raw bytes for relay.
func readHubWriteBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxHubWriteBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			httpErrorJSON(w, "request body exceeds the "+strconv.Itoa(int(maxHubWriteBodyBytes))+" byte limit", http.StatusRequestEntityTooLarge)
			return nil, err
		}
		return nil, err
	}
	return body, nil
}

// relayHubLive relays any request to the real Hub verbatim (method, body,
// content-type, query), returning the upstream response unchanged. Used
// for Hub protocol surfaces this proxy does not model - Git LFS batch and
// object transfer for small/non-Xet files - so a real client's LFS upload
// or download flows through this proxy to the real Hub transparently.
func (s *Server) relayHubLive(w http.ResponseWriter, r *http.Request) {
	body, err := readHubWriteBody(w, r)
	if err != nil {
		return
	}
	path := r.URL.Path
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	resp, err := s.Hub.RelayRaw(r.Context(), auth.CredentialFromRequest(r), r.Method, path, r.Header.Get("Content-Type"), bytes.NewReader(body))
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	defer resp.Body.Close()
	relayHubResponse(w, resp)
}
