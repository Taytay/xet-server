# Changelog

All notable changes to Xet Server will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
