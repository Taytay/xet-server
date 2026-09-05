# Changelog

All notable changes to Xet Server will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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
