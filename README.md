# Xet Server

A Go server that is **wire-compatible** with Hugging Face's real [Xet
storage protocol](https://huggingface.co/docs/hub/en/xet/index) — the
content-defined-chunking, dedup-first storage layer that replaces Git LFS
for large model files on the Hub. Point the real `hf` CLI
(`huggingface_hub` + `hf_xet`) at this server and it works: `hf upload`,
`hf download`, byte-identical round-trips, real BLAKE3 hashing, real
xorb/shard binary formats, real LZ4/byte-grouping compression.

This started as a simplified, non-wire-compatible chunking/dedup demo
(kept in `internal/chunk`, `internal/manifest`, `internal/api`, `cmd/xet`
for quick manual testing) and grew into a from-scratch, spec-driven
reimplementation of the actual protocol — verified end-to-end against a
live `hf_xet` client, with several real wire-format quirks discovered only
by capturing and replaying genuine client traffic. See
[docs/PROTOCOL.md](docs/PROTOCOL.md) for that story.

---

# Table of Contents

- [Features](#features)
- [Architecture](#architecture)
- [Installation](#installation)
- [Usage](#usage)
  - [Run the wire-compatible server](#run-the-wire-compatible-server)
  - [Point the real `hf` CLI at it](#point-the-real-hf-cli-at-it)
  - [Simple demo API + CLI](#simple-demo-api--cli)
- [HTTP API](#http-api)
- [Storage backends](#storage-backends)
- [Testing](#testing)
- [Documentation](#documentation)
- [Where this diverges from real Xet](#where-this-diverges-from-real-xet)
- [Troubleshooting](#troubleshooting)
- [License](#license)
- [Contributing](#contributing)
- [Related Projects](#related-projects)

---

# Features

- **Wire-compatible CAS HTTP API** (`internal/casserver`): xorb
  upload/fetch, shard upload, file reconstruction with Range-based paging,
  matching xet-core's own `openapi/cas.openapi.yaml` — verified
  byte-identical against a real `hf_xet` client, for both compressible and
  incompressible content.
- **Hub API shim** (`internal/hubserver`): enough of huggingface.co's Hub
  REST API (repo create, preupload, `xet-{read,write}-token`, commit,
  resolve/HEAD) that the real `hf upload`/`hf download` shell commands work
  against this server via `HF_ENDPOINT`.
- **Real BLAKE3-keyed Merkle hashing** (`internal/merklehash`): a
  byte-for-byte port of xet-core's `DataHash`, verified against xet-core's
  own published reference vectors — not an approximation.
- **Real xorb and shard binary formats** (`internal/xorbformat`,
  `internal/shardformat`), each verified against real bytes captured from
  a live `hf_xet` upload, including the footer-less upload behavior real
  clients actually use (see [docs/PROTOCOL.md](docs/PROTOCOL.md)).
- **From-scratch LZ4 decoder and ByteGrouping4 codec** (`internal/lz4`,
  `internal/bg4`): written directly from the public LZ4 spec / verified
  against a third-party reference implementation, so chunk hashes can be
  independently re-verified regardless of compression scheme.
- **Pluggable, streaming storage** (`internal/storage`): a `Store`
  interface with filesystem (`fsstore`) and S3-compatible (`s3store`)
  backends, built on `io.Reader`/`io.ReadCloser` rather than `[]byte` —
  neither backend ever buffers a full object in memory, so a multi-GB
  upload/download costs a fixed amount of memory. The S3 backend uses a
  from-scratch AWS SigV4 signer (`internal/sigv4`), no AWS SDK dependency.
  Verified end-to-end with real GGUF model files up to 27.6 GB.
- **Zero required external dependencies to build the demo path**; the
  protocol path adds exactly one pure-Go module
  (`github.com/zeebo/blake3`), pinned in `go.sum`.
- **Real-client regression fixtures**: several packages carry
  `testdata/` captured directly from a live `hf_xet` session, replayed in
  unit tests — the strongest guard against silently regressing wire
  compatibility.

# Architecture

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the full system
diagram (upload/download sequence diagrams, package responsibility table,
and the key design decisions). Short version:

- `cmd/xetd` runs the CAS server, and — with `-hub-addr` — the Hub API shim
  as a second HTTP listener, matching how huggingface.co's real Hub and
  CAS are actually separate services.
- `cmd/xet` is a small CLI for the original simple demo API only (not the
  wire-compatible protocol — use the real `hf` CLI for that).

# Installation

## Prerequisites

- Go 1.21 or later
- GNU Make
- Bash (for integration tests)
- (Optional, for the real `hf` CLI round-trip test) `pipenv`, to install
  `huggingface_hub` + `hf_xet`
- (Optional) `merman-cli` or `mmdc` (mermaid-cli), to render Mermaid
  diagrams as SVG in `make docs`/`make docs-serve` — falls back to a
  source + mermaid.live link if neither is available

If `pipenv` can't reach `pypi.org` directly (a corporate proxy, an
air-gapped environment), copy `.env.example` to `.env` and set
`PIPENV_PYPI_MIRROR` to a reachable index mirror; `.env` is read by
`make install` and is gitignored.

## Build

```bash
make pre-check   # verify Go/bash are present
make build       # -> bin/xetd, bin/xet
```

# Usage

## Run the wire-compatible server

```bash
# CAS server only, on :8420
./bin/xetd -addr :8420 -data ./xet-data

# CAS server + Hub API shim (needed for the real hf CLI), on :8420 / :8421
./bin/xetd -addr :8420 -hub-addr :8421 -data ./xet-data
```

## Point the real `hf` CLI at it

```bash
export HF_ENDPOINT="http://localhost:8421"   # the Hub shim's address
export HF_TOKEN="anything"                    # auth isn't enforced

hf upload myuser/my-model ./model.safetensors model.safetensors
hf download myuser/my-model model.safetensors --local-dir ./downloaded
cmp ./model.safetensors ./downloaded/model.safetensors   # byte-identical
```

You can also drive `hf_xet`'s low-level `XetSession` Python API directly
against the CAS server's own address (`:8420` above) with no Hub API
involved at all — useful for isolating whether an issue is in the CAS
protocol or the Hub shim.

## Simple demo API + CLI

The original, non-wire-compatible gear-hash-CDC + JSON-manifest demo is
still available for quick manual testing, mounted on the same `xetd`
process at `/upload`, `/files`, `/stats`:

```bash
./bin/xet push /path/to/model.safetensors
# -> file_id:  2d53223aa33715f0eff757537ed9cf8f
#    chunks:   28 total, 28 new
#    stored:   2000000 bytes (0.0% deduplicated)

./bin/xet push /path/to/model-v2.safetensors   # a near-duplicate checkpoint
# -> chunks:   30 total, 4 new
#    stored:   436017 bytes (81.0% deduplicated)

./bin/xet pull -out ./restored.safetensors 2d53223aa33715f0eff757537ed9cf8f
./bin/xet stats
```

# HTTP API

## CAS protocol (wire-compatible, mounted at `/v1`, `/v2`)

- `POST /v1/xorbs/{prefix}/{hash}` — upload a serialized xorb (chunk
  headers + payloads, no footer — see PROTOCOL.md)
- `GET /v1/xorbs/{prefix}/{hash}` — fetch raw (possibly compressed) xorb
  bytes, honors `Range`
- `POST /v1/shards` — upload a serialized shard (file/xorb info sections,
  no footer)
- `GET /v1/reconstructions/{file_id}` — file → xorb/chunk-range map,
  honors `Range`, returns `416` at EOF
- `GET /v1/chunks/{prefix}/{hash}` — global chunk-dedup lookup (always
  `404`: no global dedup index is maintained)
- `GET /v2/reconstructions/{file_id}` — always `501` (signals clients to
  fall back to V1)
- `POST /v1/telemetry` — no-op ack

## Hub API shim (mounted on a separate port via `-hub-addr`)

- `POST /api/repos/create`
- `POST /api/{repo_type}s/{repo_id}/preupload/{revision}`
- `GET /api/{repo_type}s/{repo_id}/xet-{read,write}-token/{revision}`
- `POST /api/{repo_type}s/{repo_id}/commit/{revision}`
- `HEAD`/`GET /{repo_id}/resolve/{revision}/{filename}`

## Simple demo API (mounted at `/`)

- `POST /upload?name=<optional>` — chunks + dedups the uploaded file
- `GET /files/{id}` / `GET /files/{id}/manifest` / `GET /stats`

# Storage backends

`internal/storage.Store` is the abstraction both `casserver` and the demo
API store chunk/xorb bytes through:

- **`fsstore`** — content-addressed filesystem directory (the default)
- **`s3store`** — any S3-compatible endpoint (AWS S3, MinIO), signed with
  the from-scratch `internal/sigv4` signer. Set `XET_S3STORE_LIVE_TEST=1`
  plus `XET_TEST_S3_*` env vars to run its tests against a real MinIO
  instance.

# Testing

```bash
make test               # unit tests, all packages
make integration-test   # bash integration suite against a live server
```

`integrationTests.sh` starts one `xetd` instance (CAS + Hub shim) and runs
every script in `integration-tests/`, with a per-test timeout so a hang
doesn't block the suite. `integration-tests/hf_cli_roundtrip.sh` drives the
**real, unmodified `hf` CLI** through a full upload+download round-trip —
the strongest compatibility check available — and skips cleanly (not a
failure) if `pipenv`/its environment aren't set up:

```bash
make install             # pipenv --python 3.14 && pipenv install
make integration-test    # now includes hf_cli_roundtrip.sh
```

See [CONTRIBUTING.md](CONTRIBUTING.md#2-running-tests) for the full test
layer breakdown, including why several packages carry real-client
`testdata/` fixtures.

# Documentation

- **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)** — system diagram,
  upload/download sequence diagrams, package responsibility table, design
  decisions
- **[docs/PROTOCOL.md](docs/PROTOCOL.md)** — wire-compatibility deep dive:
  every place the real client's behavior diverges from the documented spec,
  how each was discovered, and why the fix is correct
- **[CONTRIBUTING.md](CONTRIBUTING.md)** — dev setup, test layers, doc-comment
  conventions, branching/PR conventions
- **[CHANGELOG.md](CHANGELOG.md)** — release history

Package-level godoc comments are the source of truth for implementation
details not covered above:

```bash
go doc ./internal/merklehash
go doc ./internal/casserver
```

`make docs` regenerates the committed `docs/godoc/*.md` package reference
(via `go doc -all`) and renders every Markdown doc in this repo to
browsable HTML in `docs/build/`; `make docs-serve` does the same and then
serves it locally. Both are implemented by `scripts/build_docs.go` (its
own Go module, so the main `xet-server` module keeps zero external
dependencies).

# Where this diverges from real Xet

- No revisions/branches in the Hub shim — every repo has one implicit
  `main`.
- No auth enforcement — any bearer token is accepted.
- `GET /v2/reconstructions` always signals fall-back to V1 rather than
  implementing the multi-range-optimized V2 response shape.
- No global chunk-dedup index (`GET /v1/chunks/...` is always `404`).
- Single-node, in-memory reconstruction/repo indices — bulk chunk data
  persists in the storage backend, but the file→chunk mapping does not
  survive a restart.

# Troubleshooting

### Debugging `xetd` itself
Run `xetd` with `DEBUG=1` to log every HTTP request it receives (method,
path, status, duration) plus commit/shard/resolve lookup details at debug
level via the standard library's `log/slog`:

```bash
DEBUG=1 ./bin/xetd -addr :8420 -hub-addr :8421 -data ./xet-data
```

This is the fastest way to see whether a request from `hf upload`/`hf
download` (or anything else) actually reached the server, and what it did
once it got there — see [docs/PROTOCOL.md](docs/PROTOCOL.md)'s "How these
were found" section for how this was used to track down real bugs.

### `hf upload`/`hf download` hangs or times out
If you're in a sandboxed/corporate network that proxies all outbound
traffic (including `localhost`), `hf_xet`'s Rust HTTP client may not
consistently honor `NO_PROXY`/`no_proxy` for localhost, and the upload call
hangs. This is an environment limitation, not a xetd bug — the identical
upload/download flow works when driven directly against the CAS server via
`hf_xet`'s low-level Python API. Try setting
`NO_PROXY=localhost,127.0.0.1` / `no_proxy=localhost,127.0.0.1`, or run
outside the proxied environment. `integration-tests/hf_cli_roundtrip.sh`
enforces a timeout so this fails visibly instead of hanging the test suite.

### Integration tests fail to start the server
The runner picks ports `18420`/`18421` by default to avoid colliding with a
`make run`-style instance on `8420`/`8421`. Override with
`XETD_PORT=<port> XETD_HUB_PORT=<port> make integration-test` if those are
also taken.

### `make integration-test` reports "Operation not permitted" creating temp files
The runner uses `$TMPDIR` (or `/tmp` if unset) for all scratch state. Make
sure your environment allows writes there.

### Chunk counts differ between two very similar files more than expected (demo API only)
The demo API's chunker targets an average chunk size of 64 KiB; edits
smaller than that still land inside one chunk boundary, and byte-level
insertions can shift downstream boundaries until the rolling hash
resynchronizes. Expected content-defined-chunking behavior, not a bug.

# License

MIT License — see [LICENSE.md](LICENSE.md) for details.

# Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the full guide. Short version:

1. Fork the repository, branch from `main`
2. `make all` before opening a PR (format, vet, build, unit tests,
   integration tests)
3. PR titles follow [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/)
4. If you touch protocol/wire-format behavior, read
   [docs/PROTOCOL.md](docs/PROTOCOL.md) first

# Related Projects

- [Hugging Face Xet](https://huggingface.co/docs/hub/en/xet/index)
- [xet-core](https://github.com/huggingface/xet-core) (the real Rust
  implementation this project is wire-compatible with)
- [zig-xet](https://github.com/jedisct1/zig-xet) (an independent
  third-party reference implementation, used to verify the ByteGrouping4
  codec)
- [Git LFS](https://git-lfs.com/)
