# Changelog

All notable changes to Xet Server will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.7.0] - 2026-09-08

A protocol-completeness release: every gap this project's own "Where this
diverges from real Xet" list called out has been closed except
authentication (explicitly deferred to a future version). This is also
the first release with restart-surviving state — previously, an `xetd`
restart lost all reconstruction/repo metadata even though bulk chunk
data was always durable in the storage backend.

### Added
- **Optional dedup-hit content verification** (`internal/storage`'s new
  `VerifyingStore`, enabled via `xetd -verify-dedup`). On a dedup hit
  (`Put` for a content hash that already exists), instead of trusting
  the hash alone, byte-compares the incoming upload against the stored
  blob concurrently in fixed-size chunks, bailing at the first mismatch
  rather than reading either side in full. A mismatch — a hash collision
  or undetected storage-layer corruption — returns a new
  `storage.ErrContentMismatch` sentinel and refuses the write (the
  original stored blob is never overwritten), logged at `Error` via
  `casserver` so it's distinguishable from routine request failures.
  Off by default: benchmarked at roughly 10x slower than the default
  trust-the-hash path on a dedup hit (a full extra read), and mismatch
  detection is confirmed to scale with where the mismatch actually is
  (a mismatch 1% into a 16 MiB object is caught ~16x faster than one at
  99%) rather than always paying full-object comparison cost. Required
  adding `merklehash.Hash.MarshalText`/`UnmarshalText` so `Hash` can be
  used as a JSON map key at all (a prerequisite for this release's
  persistence work too, not just this feature).
- **Real revisions/branches in the Hub API shim.** `hubserver.repoState`
  now holds a map of independent `revisionState`s (each with its own file
  set and commit history) instead of one implicit `main`; a `main`
  revision is still created automatically on repo creation (matching a
  real repo always having a default branch), and any other revision name
  is created on first commit to it, mirroring how pushing to a new branch
  name creates it on a real repo. Committing the same file path to two
  different revisions now correctly produces two independent Xet-hash/
  size mappings, confirmed by `TestRevisions_IndependentFileSetsPerRevision`.
- **A real global chunk-dedup index.** `GET /v1/chunks/{prefix}/{hash}`
  previously always returned `404`. The real wire contract (confirmed
  against xet-core's actual client source — see docs/PROTOCOL.md §9) is
  to return the raw bytes of whichever previously-uploaded shard
  referenced the queried chunk hash, which the client parses itself to
  discover every dedup-eligible chunk in that shard, not just the one it
  asked about. `casserver.handleUploadShard` now indexes every chunk hash
  referenced by an uploaded shard's xorb-info section against that
  shard's raw bytes; a query for a known chunk hash returns those bytes
  verbatim.
- **Real V2 (multi-range) reconstruction.** `GET /v2/reconstructions/{file_id}`
  previously always returned `501`. It now returns xet-core's actual
  `QueryReconstructionResponseV2` shape (confirmed against xet-core's own
  struct definitions and its `update_260316_v2_reconstruction_multirange.md`
  changelog): the same underlying terms and physical byte ranges as V1,
  grouped by xorb hash into one `XorbMultiRangeFetch` entry (one URL,
  multiple chunk/byte ranges) instead of V1's one `fetch_info` entry per
  term. `TestReconstructionV2_MatchesV1Data` cross-checks V1 and V2
  responses for the same file and asserts identical underlying data. See
  docs/PROTOCOL.md §10 for why this is a response-shape optimization, not
  new reconstruction logic.
- **Restart-surviving persistence for in-memory metadata.**
  `casserver.Server` and `hubserver.Server` both gained
  `Snapshot(path)`/`LoadSnapshot(path)`: a periodic (default 1 minute,
  `-snapshot-interval`) and shutdown-time (`SIGINT`/`SIGTERM`, handled via
  `signal.NotifyContext` in `cmd/xetd/main.go`) atomic JSON checkpoint of
  every in-memory index, using the same stage-to-temp-then-rename pattern
  `storage/fsstore.Store.Put` already relies on so a reader never
  observes a half-written snapshot. Deliberately a periodic checkpoint,
  not a write-ahead log — writes between checkpoints are lost on an
  *un*graceful process termination (a crash, `kill -9`, power loss), not
  just preserved-until-clean-exit; see docs/PROTOCOL.md §11 for the full
  tradeoff writeup and why this was chosen over a WAL for this project.
  Verified with a real end-to-end round-trip: `hf upload` against a live
  server, a process restart against the same `-data` directory, then a
  real `hf download` of the same file from the fresh process producing a
  byte-identical file with zero re-upload.

### Changed
- README's "Where this diverges from real Xet" section now lists only
  authentication — every other previously-tracked gap (revisions,
  V2 reconstruction, global chunk-dedup, restart persistence) is closed
  as of this release.


storage layer, driven by "how do we know this is actually safe against a
hostile or merely broken client" rather than a new feature — specifically
including whether the dedup fast-path itself could be cheaply starved or
bypassed. Found and fixed four real bugs, two of them genuine
remotely-triggerable DoS vectors, plus added the test infrastructure
(native Go fuzzers, adversarial HTTP payload tests, chaos/concurrency
tests, benchmarks) to keep catching this class of issue going forward.

### Fixed
- **LZ4 decompression-amplification denial-of-service — the more severe
  of the two DoS findings, and the direct answer to "can dedup itself be
  attacked":** `decompressBlockUnknownSize` regrew its output buffer by
  doubling with no ceiling whenever decompression exceeded the current
  buffer. LZ4's block format lets a single match-length extension sequence
  (a run of `0xFF` bytes, each worth +255 to the match length) expand to
  hundreds of times its compressed size. **Empirically confirmed**: a
  ~16 MiB compressed chunk — well within a single chunk's 24-bit
  `CompressedLength` field, and far under `casserver`'s 128 MiB
  whole-upload cap — decompressed to **3.8 GB and took ~8 seconds** on
  ordinary hardware. Critically, this cost is paid on *every* upload
  attempt of the same malicious xorb: a chunk's hash can't be verified
  (and therefore can't be deduplicated against) without first
  decompressing it, so **the dedup fast-path provides no mitigation for
  this attack shape** — investigated specifically in response to the
  question of whether dedup itself opens a cheaper DoS path. Fixed by
  treating the LZ4 frame descriptor's declared max-block-size code as a
  hard decompression ceiling rather than merely an initial sizing guess —
  a real, spec-compliant encoder never produces a block exceeding it, so
  rejecting one that does can only ever reject a malformed or hostile
  frame, never a legitimate one. See `docs/PROTOCOL.md` §7 and
  `internal/lz4/dos_test.go`.
- **Allocation-size denial-of-service in shard/xorb parsing.**
  `shardformat.ReadFileLookupTable`/`ReadXorbLookupTable`/
  `ReadChunkLookupTable` and `readFileInfoSection`/`readXorbInfoSection`
  allocated `make([]T, numEntries)` directly from an attacker-controlled
  wire field (`FileDataSequenceHeader.NumEntries`,
  `XorbChunkSequenceHeader.NumEntries`, and the footer's lookup-table entry
  counts), with no bound against how many bytes were actually available to
  read. **Empirically confirmed**: a 96-byte malicious shard body claiming
  `NumEntries = 0xFFFFFFFF`, posted to `POST /v1/shards` (no auth
  required, well under the existing 16 MiB `maxShardBytes` cap), forced a
  single allocation request of **206,158,505,008 bytes (~192 GiB)** — an
  instant crash on any real machine. `xorbformat.ParseFooterV1`'s three
  `numChunks`-sized allocations had the identical shape (not currently
  reachable via any HTTP path, since real clients upload xorbs without a
  footer — see PROTOCOL.md §2 — but fixed anyway since it parses
  untrusted-shaped data). Fixed by switching every site to incremental
  `append`-based growth capped at a small preallocation ceiling
  (`maxLookupEntryPreallocate`/`maxSectionEntryPreallocate`/
  `maxFooterEntryPreallocate`, 4096 entries): a claim of billions of
  entries now fails fast on the first genuinely-missing byte instead of
  attempting the allocation upfront, while a legitimately large, honest
  shard/footer still parses correctly via `append`'s normal growth.
  Regression tests in `internal/shardformat/dos_test.go` reproduce the
  exact malicious payload and assert it fails within 5 seconds without
  attempting more than 64 MiB of heap growth.
- **Concurrent uploads of the same content could spuriously fail.**
  `fsstore.Put`'s staging temp file was named `<path>.tmp-<pid>` — every
  concurrent `Put` call for the *same key* within one process (e.g. several
  clients uploading an identical xorb at once) collided on that exact
  path, and `O_EXCL` correctly rejected the collision with `EEXIST`,
  surfacing as a `500` to every request but one. This is a normal,
  expected race for a content-addressed store (concurrent duplicate
  uploads should all succeed via dedup, not serialize on a filename
  accident) — found by
  `TestAdversarial_ConcurrentUploadsOfSameXorb` firing 20 concurrent
  uploads of one xorb and observing only 1 succeed. Fixed by using
  `os.CreateTemp` (a unique random suffix per call) instead of a
  PID-based name, plus treating a losing `os.Rename` race as a successful
  dedup (not a failure) as long as the target path exists afterward.
- **`fsstore.TotalBytes` (backing the eviction sweep's storage-budget
  check) could abort entirely on an ordinary concurrent race.** Found by
  `TestChaos_UploadFetchEvictInterleaved` running uploads, fetches, and an
  eviction sweep concurrently: `filepath.WalkDir` visiting a path that a
  concurrent `Put`'s rename or a concurrent `Delete` had just removed
  returned `os.ErrNotExist`, which the walk callback treated as a fatal
  error, aborting the *entire* measurement rather than just skipping that
  one now-vanished entry. Fixed to treat a disappearing file/directory as
  "not there anymore, don't count it" rather than a walk failure.
  Separately, in-flight upload staging files (`.tmp-*`) were being counted
  toward the budget at all — inflating it with uploads that hadn't
  committed and might never complete — and a staging file orphaned by a
  crashed process (never reaching its own cleanup) would then be silently
  excluded *forever*, a permanent disk-space leak hidden from the count.
  Fixed: a temp file younger than 30 minutes is excluded (presumed
  legitimately in-flight); one older than that is treated as orphaned and
  actually removed, so disk space is reclaimed rather than just hidden.

### Added
- **Native Go fuzz tests** (`go test -fuzz`) for every binary/wire parser
  reachable with attacker-controlled bytes: `merklehash.FromHex`/
  `FromRawBytes`, `lz4.DecompressFrame`/`DecompressBlock`, `bg4.Reverse`,
  `xorbformat.ReadChunkHeader`/`ScanChunks`/`ParseFooterV1`,
  `shardformat.ReadShard`, and `casserver.parseByteRange`. Seeded with
  valid round-tripped encodings (including the real `hf_xet`-captured
  fixtures already in `testdata/`) plus hand-picked edge cases. Each ran
  clean (zero crashes/hangs/OOMs) across tens of millions of executions
  combined during this pass — see `*/fuzz_test.go`.
- **Adversarial HTTP payload tests** (`*/adversarial_test.go`): malformed
  xorb/shard bodies, hostile hash/path/filename segments (path traversal,
  SQL-injection shapes, null bytes, oversized ndjson lines), malformed
  `Range` headers, `Content-Length` lies, and a concurrent-duplicate-upload
  stress test — each asserting both a clean error response *and* that the
  server keeps serving correctly afterward, not just "didn't crash on this
  one request."
- **Chaos/reliability tests** (`internal/casserver/chaos_test.go`): an
  upload interrupted mid-body followed by a clean retry (proving
  `fsstore.Put`'s atomic-rename staging leaves no corruption for the retry
  to inherit), sustained concurrent traffic mixing valid and malformed
  requests (proving malformed traffic never corrupts or drops unrelated
  valid state), and upload/fetch/eviction-sweep interleaving under a tight
  storage budget (proving no deadlock and no corruption of anything the
  sweep didn't evict).
- **Benchmarks** (`*/benchmark_test.go`, `go test -bench`): chunking
  throughput (`internal/chunk`), BLAKE3-keyed hashing and Merkle
  aggregation (`internal/merklehash`), LZ4 frame decompression
  (`internal/lz4`, including against the real captured fixture),
  ByteGrouping4 reverse transform (`internal/bg4`), and a dedup-speed
  benchmark (`internal/api`) quantifying the actual wall-clock advantage of
  deduplication: a fully-duplicate 16 MB upload runs at **~250 MB/s**
  versus **~74 MB/s** for entirely unique content of the same size — a
  **~3.4x** throughput difference from skipping storage I/O on the dedup
  fast path (chunking/hashing cost is paid either way).

## [0.5.0] - 2026-09-07

Defense-in-depth follow-up to 0.4.0's performance work: bounded, observable
storage eviction and per-client upload rate limiting — both scoped as
measurable, single-node policies rather than open-ended hardening.

### Added
- **Storage auto-pruning** (`internal/eviction`): an optional background
  sweep (`xetd -max-storage-bytes N -eviction-interval 5m`) that evicts
  least-recently-accessed xorbs once total storage exceeds the configured
  budget. Never evicts a xorb with a fetch currently in progress
  (`casserver.Server` tracks per-xorb in-flight fetch counts); every
  eviction is logged at `Info` with key, reason, and bytes freed. Disabled
  by default (`-max-storage-bytes 0`).
  - `storage.Deleter` and `storage.Sizer` are new optional capability
    interfaces (matching the existing `storage.URLPresigner` pattern),
    implemented by both `fsstore` (directory walk / `os.Remove`) and
    `s3store` (paginated `ListObjectsV2` / `DELETE`).
  - `GET /v1/storage-stats` (operator-facing, not part of the real Xet CAS
    API) reports whether eviction is enabled and, if so, the configured
    budget plus cumulative evictions/bytes freed — so the policy's effect
    is directly observable on a running server, not just inferable from
    logs.
- **Per-source-IP upload rate limiting** (`internal/ratelimit`): a
  hand-rolled token-bucket limiter (no new dependency) gating the xorb and
  shard upload endpoints specifically — the expensive paths (chunk
  decompression, hashing) a client hammering the server would otherwise
  cost the most. Configurable via `xetd -rate-limit-rps N
  -rate-limit-burst N`; disabled by default. Exceeding the limit returns
  `429 Too Many Requests` with a `Retry-After` header, logged at `Debug`
  (a retrying client backing off is expected behavior, not a fault).
  Fetch/reconstruction/HEAD endpoints are deliberately not rate-limited.

### Notes
- Both features are single-node, in-memory policies with no cross-restart
  persistence (eviction's LRU state resets on restart; rate-limit buckets
  are per-process) — consistent with the rest of this server's existing
  persistence model (see README's "Where this diverges from real Xet").
  Auth-based (rather than IP-based) rate limiting is out of scope until
  this server has an auth model at all.

## [0.4.0] - 2026-09-05

### Added
- Sentinel storage errors `storage.ErrNotFound` and `storage.ErrSizeMismatch`
  (`errors.Is`-checkable), returned consistently by both `fsstore` and
  `s3store` instead of backend-specific ad hoc errors — a caller no longer
  needs to know which backend it's talking to to detect "not found" vs. a
  real fault.
- `casserver`'s xorb and shard upload handlers now cap request body size
  (`http.MaxBytesReader`, 128 MiB for xorbs — well above real xet-core's
  ~64 MiB per-xorb target, 16 MiB for shards — metadata bounded by chunk
  count, not file size) and return `413 Request Entity Too Large` instead
  of allowing an unbounded read.
- `log/slog` is now used consistently for all runtime logging across
  `casserver`, `hubserver`, and `cmd/xetd` (previously a mix of `log.Printf`
  and `slog.Debug`). 4xx responses (client protocol/input errors — a bad
  hash, a truncated upload) log at `Debug`; 5xx responses (server-side
  faults) log at `Warn`; startup/lifecycle events log at `Info`. `log.Fatal`
  remains for unrecoverable startup errors in `cmd/xetd`, since `slog` has
  no equivalent terminate-and-exit call.

### Changed
- **`storage.Store` is now streaming.** `Put(ctx, key, data []byte)` became
  `Put(ctx, key, r io.Reader, size int64)`; `Get`/`GetRange` now return
  `io.ReadCloser` instead of `[]byte`. Neither `fsstore` nor `s3store` ever
  buffers a full xorb in memory anymore — a multi-gigabyte upload/download
  now costs a fixed, small amount of memory regardless of file size,
  verified end-to-end with real GGUF model files up to 27.6 GB
  (`~/.ollama/models/blobs`) round-tripped byte-identical through the
  actual `hf` CLI, and unit-benchmarked at ~500-575 MB/s sustained write
  throughput to a local filesystem store.
  - `fsstore.Put` stages each write to a per-attempt temp file and only
    renames it into place once the full declared size has been copied —
    a failed, canceled, or short read leaves no partial blob visible under
    the key, and the caller can simply retry with a fresh reader.
  - `casserver.handleUploadXorb` streams the request body to a temp file
    (`xorbformat.ScanChunks` needs `io.Seeker`, which an `http.Request.Body`
    doesn't support) and only hands it to the storage backend once the
    whole body is received and its claimed hash verified — a client that
    disconnects mid-upload never leaves a partial xorb stored.
  - `s3store.Put` signs with `sigv4.UnsignedPayload` instead of a
    precomputed SHA-256 content hash, since computing that hash would
    require buffering the whole body up front, defeating the point of
    streaming a multi-GB xorb.
- **`casserver.Server`'s single global `sync.RWMutex` is now three
  independent locks** (`fileReconMu`, `xorbMu`, `sha256Mu`), one per index
  map. No code path ever needed a consistent snapshot across more than one
  map, so the shared lock only serialized unrelated concurrent
  uploads/downloads without buying any real consistency guarantee — a large
  xorb upload (touching only `xorbFooters`/`xorbRawLength`) can now proceed
  concurrently with an unrelated reconstruction lookup (touching only
  `fileRecon`).
- **`hubserver.Server`'s single global mutex now only guards the top-level
  `repos` map**; each `repoState` has its own mutex for its files and commit
  metadata, so a commit or resolve request against one repo no longer
  blocks on unrelated activity in a different repo.

### Fixed
- `handleUploadXorb` seeks its staging temp file back to the start before
  scanning chunk headers — a regression introduced while switching from
  `bytes.NewReader` (which was implicitly at the correct offset) to a
  temp file (left at EOF after `io.Copy` from the request body), caught by
  the existing xorb-upload regression tests before it shipped.
- `hubserver.resolve.go`'s `commitOIDOrPlaceholder` read `repoState.commitOID`
  /`commitSeen` without holding `repoState`'s mutex — a pre-existing data
  race, now fixed as part of introducing per-repo locking. Confirmed the
  full test suite (unit + integration) passes clean under `go test -race`.

## [0.3.0] - 2026-09-04

### Added
- `Pipfile`/`Pipfile.lock` and `make install`/`make install-deps` to manage
  the optional `huggingface_hub`/`hf_xet` dependencies for
  `hf_cli_roundtrip.sh` via `pipenv`, instead of a hand-rolled venv.
- `scripts/build_docs.go`: a self-contained Go program (own module,
  `scripts/go.mod`) that regenerates `docs/godoc/*.md` via `go doc -all`
  and renders all Markdown docs to browsable HTML in `docs/build/`, using
  `goldmark` (GFM/tables/TOC anchors), `chroma` (pygments-equivalent
  syntax highlighting), and `goldmark-mermaid` (Mermaid diagram
  rendering via `merman-cli` or `mmdc`, whichever is available and
  functional, with a styled source + mermaid.live-link fallback
  otherwise). `make docs`/`make docs-serve` now just run this program;
  `make pre-check` reports which Mermaid renderer (if any) is available.
- `.env`/`.env.example` support in the `Makefile` (`PIPENV_PYPI_MIRROR`,
  for networks that can't reach `pypi.org` directly) — read by `make
  install` via `include .env` + `export`, gitignored, never committed.
- `DEBUG=1` environment variable for `cmd/xetd`: enables `log/slog` debug
  logging of every HTTP request (method, path, status, duration) plus
  commit/shard/resolve lookup details in `hubserver`/`casserver`. This is
  what surfaced both real bugs fixed below.

### Changed
- Integration test default timeout lowered from 60s to 10s
  (`XET_IT_TEST_TIMEOUT`); `hf_cli_roundtrip.sh` now also wraps each `hf`
  invocation in its own tighter internal timeout (`XET_HF_CLI_CMD_TIMEOUT`,
  default 4s) so a proxy-hang fails fast instead of consuming the outer
  budget. The full suite (5 fast tests + the hf CLI test failing/skipping)
  now completes in well under 10 seconds.

### Removed
- `scripts/build_docs.py` and `scripts/gen-docs.sh` (Python/pipenv +
  bash), replaced by `scripts/build_docs.go`. `markdown`/`pygments`
  dropped from `Pipfile`'s dev-packages — pipenv is now only needed for
  the optional `hf` CLI integration test, not docs.

### Fixed
- `GET /api/{repo_type}s/{repo_id}/xet-{read,write}-token/{revision}`
  (the Hub API shim) returned the CAS URL/access token only as
  `X-Xet-*` response headers with an empty body. Real `hf_xet` clients
  decode this response as a **JSON body**
  (`DirectRefreshRouteTokenRefresher::get_cas_jwt` in xet-core, deserializing
  into `CasJWTInfo{casUrl, exp, accessToken}`) — an empty body fails that
  decode, which `hf_xet` treats as a transient error and retries
  indefinitely instead of failing fast, so `hf upload` just hung past any
  timeout. Now returns the JSON body in addition to the headers (the
  latter still used by `huggingface_hub`'s resolve/download metadata
  path). See [docs/PROTOCOL.md](docs/PROTOCOL.md) §6.
- `casserver.handleUploadShard`'s `sha256ToXet` index (bridging a
  committed file's plain SHA-256 to its Xet/Merkle hash for downloads)
  was keyed by a raw hex encode of `FileMetadataExt.SHA256`'s wire
  bytes. Real `hf_xet` clients write that field through the same
  word-reversal byte-order transform as a genuine Merkle hash's `Hex()`
  — confirmed by capturing a real upload and comparing the raw bytes
  against the file's actual SHA-256 in the commit payload's
  `lfsFile.oid`. The raw-byte encoding never matched, so every
  `hf download` 404'd once uploads stopped hanging (previous bug). Fixed
  to use `.Hex()`. See [docs/PROTOCOL.md](docs/PROTOCOL.md) §1/§3.
- The Mermaid diagram fallback link (shown when no working Mermaid
  renderer is available) pointed to a `mermaid.live/edit#pako:` fragment
  built from a plain URL-encoded diagram source; mermaid.live actually
  expects that fragment to be a zlib-deflated, base64url-encoded JSON
  envelope (`{"code": ..., "mermaid": {...}}`), so the link never
  decoded. Fixed to build the correct payload.
- `docs/ARCHITECTURE.md`'s CAS-upload sequence diagram used `→` and a
  stray `;` inside a `Note over` line, which some Mermaid parsers
  (including `merman-cli`) reject — replaced with plain ASCII.
- `make help` (and any target output listing `$(MAKEFILE_LIST)`) printed
  `Makefile` as every target's name instead of the real target, once
  `.env` was added to `MAKEFILE_LIST` via `include` — `grep -E` prefixes
  matches with the source filename when searching more than one file,
  which shifted `awk`'s field split. Fixed with `grep -hE`.

### Planned
- ByteGrouping4LZ4 verification against a real captured chunk (currently only
  the codec itself is verified against zig-xet's reference vector; no real
  hf_xet capture using this scheme has been exercised end-to-end yet)
- V2 (`/v2/reconstructions`) multi-range fetch support — currently always
  signals a 501 fall-back to V1
- Global chunk-deduplication index (`GET /v1/chunks/{prefix}/{hash}`) —
  currently always 404
- Revision/branch support in the Hub API shim — every repo currently has a
  single implicit `main` revision

## [0.2.0] - 2026-09-04

### Added
- **Hub API shim** (`internal/hubserver`): a minimal implementation of
  huggingface.co's Hub REST API (distinct from the CAS API) — repo creation,
  preupload mode negotiation, `xet-{read,write}-token` issuance via response
  headers, ndjson commit parsing, and resolve/HEAD metadata with
  `X-Xet-Hash` — enough for the real `hf upload` / `hf download` CLI to
  target this server via `HF_ENDPOINT`. `cmd/xetd` gained a `-hub-addr` flag
  to run it alongside the CAS server.
- **Wire-compatible CAS HTTP API** (`internal/casserver`): the real Xet CAS
  protocol per xet-core's own `openapi/cas.openapi.yaml` — xorb upload/fetch,
  shard upload, file reconstruction with Range-based paging, chunk-dedup and
  telemetry stubs. Verified end-to-end against a live `hf_xet` Python
  client: byte-identical upload/download round-trips for both compressible
  and incompressible content.
- **BLAKE3-keyed Merkle hashing** (`internal/merklehash`): a byte-for-byte
  port of xet-core's `DataHash` type and Merkle-aggregation algorithm,
  verified against xet-core's own published reference vectors.
- **Xorb binary format** (`internal/xorbformat`) and **shard binary format**
  (`internal/shardformat`): ports of xet-core's on-wire chunk/xorb/shard
  layouts, each verified against real bytes captured from a live `hf_xet`
  upload.
- **From-scratch LZ4 decoder** (`internal/lz4`): an LZ4 block + frame
  decoder written directly from the public LZ4 format specifications (not
  ported from any existing implementation), needed to independently
  re-verify chunk hashes regardless of compression scheme.
- **ByteGrouping4 codec** (`internal/bg4`): the reverse transform for Xet's
  `ByteGrouping4LZ4` chunk compression scheme, cross-checked against
  [zig-xet](https://github.com/jedisct1/zig-xet)'s published test vector.
- **Pluggable storage backend** (`internal/storage`): a `Store` interface
  with filesystem (`fsstore`) and S3-compatible (`s3store`) implementations.
  The S3 backend uses a from-scratch AWS SigV4 request signer
  (`internal/sigv4`, verified against independently-computed HMAC chains and
  a live MinIO instance) instead of a third-party SDK.
- `integration-tests/hf_cli_roundtrip.sh`: drives the actual `hf` CLI
  (not this project's own client) through a full upload+download round-trip
  against the shared test server, wired into `integrationTests.sh` with a
  per-test timeout and SKIP semantics for environments without the optional
  `huggingface_hub`/`hf_xet` Python dependencies.
- `make pre-check` / `.DEFAULT_GOAL := help` / `make vet` targets.

### Changed
- Project renamed from `xet-lite` to `xet-server` (Go module path, folder
  name) to reflect the shift from a from-scratch dedup demo to a
  wire-compatible protocol server.
- `internal/api`'s original filesystem-backed chunk store was extracted into
  `internal/storage/fsstore` behind the new `storage.Store` interface.

### Fixed
- Real `hf_xet` clients upload xorbs and shards *without* a footer/lookup
  tables — the server now reconstructs chunk hashes, boundaries, and file
  indices itself by scanning headers, rather than requiring (and rejecting
  the absence of) a client-supplied footer.
- Shard file-info headers real clients set `MDB_FILE_FLAG_WITH_VERIFICATION`
  and `MDB_FILE_FLAG_WITH_METADATA_EXT` unconditionally; the shard reader now
  parses the corresponding verification/metadata_ext trailing entries
  instead of misreading everything after the first file header.
- `GET /v1/reconstructions/{file_id}` now honors the HTTP `Range` header and
  returns 416 once the requested range starts at or past EOF, matching how
  `hf_xet` pages through large-file downloads (previously always returned
  the same full-file term, desyncing the client's sequential writer on the
  second page).

## [0.1.0] - 2026-09-03

### Added
- Initial release as `xet-lite`: a simplified, non-wire-compatible
  re-implementation of the core idea behind Hugging Face's Xet storage —
  content-defined chunking (gear-hash rolling hash), content-addressed
  chunk store, JSON manifests for file reconstruction.
- `cmd/xetd` HTTP server (`POST /upload`, `GET /files/{id}`,
  `GET /files/{id}/manifest`, `GET /stats`) and `cmd/xet` CLI
  (`push`/`pull`/`stats`).
- Unit tests for all packages and a bash-based integration test harness
  (`integrationTests.sh` + `integration-tests/*.sh`) exercising a live
  server end-to-end.
- `LICENSE.md` (MIT), `.gitignore`, `Makefile` (`format`/`build`/`test`/
  `integration-test`/`clean`/`all`), and initial `README.md`.
