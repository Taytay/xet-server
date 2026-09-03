# Xet Server

A simplified, local re-implementation of the core idea behind Hugging Face's
[Xet storage](https://huggingface.co/docs/hub/en/xet/index): instead of
storing whole files (as Git LFS does), files are split into
**content-defined chunks**, each chunk is stored once by content hash, and a
per-file **manifest** records which chunks (in which order) reconstruct the
original bytes. Re-uploading a file that shares most of its content with one
already stored — e.g. a fine-tuned or pruned/sparse checkpoint — only needs
to store the *new* chunks.

This is not protocol-compatible with real Xet (no CAS/xorb/shard wire
format), but it captures the same mechanism: **content-defined chunking +
hash-based dedup + manifest-based reconstruction**, running entirely on your
machine with a small Go server and CLI.

## Features

- **Content-Defined Chunking**: Gear-hash rolling-hash chunker cuts chunk
  boundaries based on local content, not fixed offsets — an edit in the
  middle of a file only changes the chunk(s) around that edit
- **Content-Addressed Dedup**: Chunks are stored once by SHA-256; uploading
  a near-duplicate file only writes the chunks that don't already exist
- **Manifest-Based Reconstruction**: Each uploaded file gets a JSON manifest
  (ordered chunk refs) used to stream the exact original bytes back on
  download
- **HTTP API**: Simple `POST /upload` / `GET /files/{id}` / `GET
  /files/{id}/manifest` / `GET /stats` surface, easy to script or curl
- **CLI Client**: `xet push` / `xet pull` / `xet stats` for everyday use
- **Zero External Dependencies**: Pure Go standard library — no modules to
  fetch, no network access required to build
- **Comprehensive Testing**: Unit tests across all packages plus end-to-end
  integration tests that exercise a live server (push/pull round-trip,
  dedup behavior, error paths)

## Architecture

- `internal/chunk` — gear-hash rolling-hash chunker (~64 KiB average chunk
  size, bounded to [4 KiB, 256 KiB])
- `internal/store` — content-addressed filesystem store for chunk blobs
  (`<data-dir>/chunks/<hash prefix>/<hash>`)
- `internal/manifest` — per-file JSON manifest: full-file SHA-256, size, and
  ordered `{hash, offset, length}` chunk references
- `internal/api` + `cmd/xetd` — HTTP server exposing upload/download/stats
- `internal/client` + `cmd/xet` — CLI that talks to `xetd`

## Installation

### Prerequisites

- Go 1.21 or later
- GNU Make
- Bash (for integration tests)

### Build

```bash
make build
```

This produces `bin/xetd` (server) and `bin/xet` (CLI). No external Go
modules are required.

## Usage

### Run the server

```bash
make run
# or directly:
./bin/xetd -addr :8420 -data ./xet-data
```

`./xet-data/chunks/` holds deduplicated chunk blobs; `./xet-data/manifests/`
holds one JSON manifest per uploaded file.

### Use the CLI

```bash
# upload (chunks + dedups against everything already stored)
./bin/xet push /path/to/model.safetensors
# -> file_id:  2d53223aa33715f0eff757537ed9cf8f
#    size:     2000000 bytes
#    chunks:   28 total, 28 new
#    stored:   2000000 bytes (0.0% deduplicated)

# upload a near-duplicate (e.g. a fine-tuned checkpoint sharing most weights)
./bin/xet push /path/to/model-v2.safetensors
# -> chunks:   30 total, 4 new
#    stored:   436017 bytes (81.0% deduplicated)

# download by file_id, verify round-trip
./bin/xet pull -out ./restored.safetensors 2d53223aa33715f0eff757537ed9cf8f
cmp /path/to/model.safetensors ./restored.safetensors   # no output = identical

# see store-wide dedup stats
./bin/xet stats
```

All commands take `-server http://host:port` (defaults to
`http://localhost:8420`).

## HTTP API

- `POST /upload?name=<optional>` — body is the raw file; response is JSON
  with `file_id`, chunk counts, and dedup percentage.
- `GET /files/{id}` — streams the reconstructed file.
- `GET /files/{id}/manifest` — returns the manifest JSON.
- `GET /stats` — store-wide unique chunk count/bytes and file count.

### Manual testing with curl

```bash
curl -s -X POST --data-binary @model.safetensors "http://localhost:8420/upload?name=model.safetensors"
curl -s -o restored.safetensors "http://localhost:8420/files/<file_id>"
curl -s "http://localhost:8420/files/<file_id>/manifest" | jq .
curl -s "http://localhost:8420/stats" | jq .
```

## Testing

### Unit Tests

```bash
make test
```

Covers the chunker (determinism, boundary sizing, local-edit isolation), the
content-addressed store (dedup, sharded layout), the manifest (save/load
round-trip), and the HTTP API (upload/download round-trip, dedup across
uploads, error paths like unknown file IDs and path traversal attempts).

### Integration Tests

```bash
# Run all integration tests
make integration-test

# Run a single test script
make integration-test TEST=integration-tests/dedup_near_duplicate.sh
```

Each integration test is a standalone bash script under `integration-tests/`
run against a live `xetd` instance (started and torn down automatically by
`integrationTests.sh`). Tests cover:

- `push_pull_roundtrip.sh` — upload then download reproduces the original
  file exactly
- `dedup_near_duplicate.sh` — a near-duplicate upload dedups most chunks
  against a prior upload, and both versions still restore correctly
- `identical_reupload.sh` — re-uploading identical content reuses the same
  `file_id` with zero new chunks
- `pull_unknown_id_fails.sh` — pulling a nonexistent file ID fails cleanly
- `stats_reflect_uploads.sh` — `/stats` accounts for new uploads

## Examples

### Verify dedup on a modified model checkpoint

```bash
./bin/xet push checkpoint-epoch1.safetensors
./bin/xet push checkpoint-epoch2.safetensors   # shares most weights with epoch1
./bin/xet stats                                 # unique_chunk_bytes << sum of both file sizes
```

### Fetch just the manifest to inspect chunking

```bash
curl -s "http://localhost:8420/files/<file_id>/manifest" | jq '.chunks | length'
```

## Development

### Code Formatting
```bash
make format
```

### Clean Build
```bash
make clean
make build
```

### All Checks
```bash
make all
```

Runs formatting, build, unit tests, and integration tests in sequence.

## Where this diverges from real Xet

- No merkle/shard aggregation — manifests list raw chunk refs directly.
- No range-request/partial-file support on download.
- No compression of chunks at rest.
- Single-node, no auth — this is for local experimentation only.

## Troubleshooting

### Integration tests fail to start the server
The test runner picks port `18420` by default to avoid colliding with a
`make run` instance on `8420`. Override with `XETD_PORT=<port> make
integration-test` if that port is also taken.

### `make integration-test` reports "Operation not permitted" creating temp files
The runner uses `$TMPDIR` (or `/tmp` if unset) for all scratch state. Make
sure your environment allows writes there.

### Chunk counts differ between two very similar files more than expected
The chunker targets an average chunk size of 64 KiB; edits smaller than that
still land inside one chunk boundary, and byte-level insertions can shift
downstream boundaries until the rolling hash resynchronizes. This is
expected content-defined-chunking behavior, not a bug — dedup improves with
larger files and edits that don't disturb chunk boundaries.

## License

MIT License - See [LICENSE](LICENSE.md) for details.

## Contributing

1. Fork the repository
2. Create a feature branch
3. Add tests for new functionality
4. Ensure all tests pass (`make all`)
5. Submit a pull request

## Related Projects

- [Hugging Face Xet](https://huggingface.co/docs/hub/en/xet/index)
- [Git LFS](https://git-lfs.com/)
