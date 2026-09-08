# `xet-server/internal/casserver`

```
package casserver // import "xet-server/internal/casserver"

Package casserver implements the wire-compatible CAS (Content Addressable
Storage) HTTP API real Xet clients (hf_xet/xet-core, and by extension the `hf`
CLI) speak, per xet-core's own openapi/cas.openapi.yaml:

  - POST /v1/xorbs/{prefix}/{hash} — upload a serialized xorb
  - POST /v1/shards — upload a serialized shard
  - GET /v1/reconstructions/{file_id} — file → xorb/chunk-range map
  - GET /v1/xorbs/{prefix}/{hash} — fetch raw (compressed) xorb bytes
    (byte-serving endpoint for the URLs handed out in fetch_info; not part of
    the public CAS API surface, but needed since this server plays both the
    CAS metadata role and the byte-transfer role real Xet splits across two
    services)
  - GET /v1/chunks/{prefix}/{hash} — global chunk dedup lookup (always 404:
    no global dedup index is maintained)
  - GET /v2/reconstructions/{file_id} — always 404/501, signaling clients to
    fall back to V1
  - POST /v1/telemetry — no-op ack
  - GET /v1/storage-stats — eviction policy stats (operator-facing; not part of
    the real Xet CAS API)

This server never decompresses chunk payloads — like real CAS, it stores
and serves xorb bytes as opaque blobs, and integrity is checked via the xorb
footer's own hash tree rather than by re-verifying chunk contents.

TYPES

type Server struct {
	// Has unexported fields.
}
    Server implements the CAS HTTP API against a storage.Store backend for xorb
    bytes. File-reconstruction and xorb-footer indexes are held in memory:
    they are metadata derived from uploaded shards/xorbs, cheap to rebuild,
    and small relative to the bulk chunk data in Store.

    Each index has its own mutex rather than one shared lock: none of the four
    maps are ever read or written together under one critical section (confirmed
    — no code path needs a consistent snapshot across more than one of them), so
    a single global lock only serialized unrelated concurrent uploads/downloads
    without buying any actual consistency guarantee. Splitting them lets a
    large xorb upload (which only touches xorbFooters/xorbRawLength) proceed
    concurrently with an unrelated reconstruction lookup (which only touches
    fileRecon).

    xorbLastAccess/xorbInFlight exist purely to support eviction.Sweeper (see
    EvictionCandidates/ForgetKey below): the last time each xorb was uploaded
    or fetched, and how many fetches are in progress right now, so a sweep never
    deletes a blob a client might be mid-download of.

func New(xorbs storage.Store) *Server

func (s *Server) EvictionCandidates() []eviction.Candidate
    EvictionCandidates implements eviction.Registry: every xorb this server
    knows about, excluding any with a fetch currently in progress. Called by
    eviction.Sweeper on its own poll interval, not a request hot path.

func (s *Server) FileSize(fileHash merklehash.Hash) (int64, bool)
    FileSize returns the total unpacked size of a file known to this server's
    reconstruction index, or false if fileHash is unknown.

func (s *Server) ForgetKey(key string)
    ForgetKey implements eviction.Registry: drops key from every in-memory index
    once eviction.Sweeper has already deleted the underlying blob from storage.
    A subsequent fetch of this xorb 404s, exactly as if it had never been
    uploaded — a client that still needs it must re-upload (real Xet clients
    already handle a missing xorb by re-deriving it from the source file,
    since CAS storage is explicitly not guaranteed permanent).

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request)

func (s *Server) SetEvictionStats(statsFunc func() eviction.Stats)
    SetEvictionStats wires an eviction.Sweeper's Stats method into GET
    /v1/storage-stats, so the eviction policy's effect is observable via the
    running server rather than only inferable from logs. Call once at startup if
    an eviction.Sweeper was created for this server's store.

func (s *Server) SetUploadRateLimiter(limiter *ratelimit.Limiter)
    SetUploadRateLimiter wires a per-source-IP token-bucket limiter in front
    of the xorb and shard upload endpoints. Must be called before serving any
    traffic — it rebuilds the route table (http.ServeMux panics on duplicate
    pattern registration, so routes are re-registered from scratch on a fresh
    mux rather than layered on top of the existing one).

func (s *Server) XetHashForSHA256(sha256Hex string) (merklehash.Hash, bool)
    XetHashForSHA256 returns the Xet/Merkle file hash for a file previously
    uploaded via a shard whose FileMetadataExt declared this SHA-256, or false
    if no such file is known. Used by hubserver to bridge the Hub commit
    API's plain-SHA-256 file identity to the Xet hash the CAS layer indexes
    reconstructions under.
```
