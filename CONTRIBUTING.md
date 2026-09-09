# Contributing to Xet Server

Welcome! This guide covers how to set up your environment, run the test
suite, and the conventions this project follows for docs, branches, and
pull requests.

## 1. Setting Up the Development Environment

Clone the repository and verify your toolchain:

```bash
git clone <this-repo-url> Xet-Server
cd Xet-Server
make pre-check
```

`make pre-check` verifies Go and Bash are present and prints their versions.
This project has **zero external Go dependencies** beyond the standard
library plus `github.com/zeebo/blake3` (a pure-Go BLAKE3 implementation
required for wire compatibility with real Xet hashes) — no proxy config,
no network access needed to build.

Build both binaries:

```bash
make build   # -> bin/xetd (server), bin/xet (CLI)
```

## 2. Running Tests

Three layers of tests exist; run all of them before opening a PR.

### Unit tests

```bash
make test
```

Every `internal/*` package has its own `_test.go` files. Several packages
carry **regression fixtures captured from a real `hf_xet` client session**
(`internal/casserver/testdata/`, `internal/lz4/testdata/`,
`internal/shardformat/testdata/`) — these exist because the real client's
wire format differs from the documented spec in a few places (see
[docs/PROTOCOL.md](docs/PROTOCOL.md)), and replaying the exact bytes that
once broke this server is the strongest regression guard against
re-introducing those bugs. Don't delete or "clean up" these fixtures.

To run a single package's tests:

```bash
go test -v ./internal/merklehash/...
```

### Integration tests (bash, against a live server)

```bash
make integration-test
# or a single script:
make integration-test TEST=integration-tests/push_pull_roundtrip.sh
```

`integrationTests.sh` starts one `xetd` instance (both the CAS server and
the Hub API shim) and runs every `integration-tests/*.sh` script against
it, with a per-test timeout (`XET_IT_TEST_TIMEOUT`, default 10s) so a
single hanging test doesn't block the whole suite. Scripts receive `$XET`,
`$XETD_URL`, `$HUB_URL`, `$WORKDIR`, and `$PYTHON_VERSION` — see the header
comment in `integrationTests.sh` for the full contract.

A test script can `exit 77` to **SKIP** instead of fail, for cases that
depend on an optional external dependency not being present (see
`integration-tests/hf_cli_roundtrip.sh` for the pattern).

### Real `hf` CLI round-trip

`integration-tests/hf_cli_roundtrip.sh` drives the actual `hf upload`/
`hf download` shell commands (via `huggingface_hub` + `hf_xet`, installed
through `pipenv` — not this project's own client) against the shared test
server. This is the strongest possible compatibility check, since it never
touches this repo's own code on the client side. It's optional and skips
cleanly if `pipenv`/its environment aren't set up:

```bash
make install             # pipenv --python 3.14 && pipenv install
make integration-test    # will now include and run hf_cli_roundtrip.sh
```

If it hangs or times out in a sandboxed/proxied network environment, that's
a known environment limitation (`hf_xet`'s Rust HTTP client doesn't always
honor `NO_PROXY` for localhost), not a protocol bug — see the script's
header comment and [docs/PROTOCOL.md](docs/PROTOCOL.md) for details.

### Fuzz tests

Every binary/wire-format parser that touches attacker-controlled bytes has
a native Go fuzz test (`go test -fuzz`, no external fuzzing framework)
in a `fuzz_test.go` alongside its package: `internal/merklehash`,
`internal/lz4`, `internal/bg4`, `internal/xorbformat`,
`internal/shardformat`, `internal/casserver` (just `parseByteRange`).
These found a real, confirmed allocation-size DoS during this project's
first fuzzing pass — see [docs/PROTOCOL.md](docs/PROTOCOL.md)'s §7 for the
full story and the general rule it generalizes to.

```bash
go test ./internal/shardformat/... -run '^$' -fuzz '^FuzzReadShard$' -fuzztime 60s
```

`-run '^$'` skips running the package's regular tests first (fuzzing
implicitly runs the seed corpus as regular test cases anyway); drop it if
you want both. If a fuzzer finds a crash, it writes the failing input to
`testdata/fuzz/<FuzzName>/<hash>` in that package — commit that file as a
permanent regression seed once you've fixed the bug it found, the same
way `internal/casserver/testdata/`'s real-capture fixtures are committed.

If you're on a machine where the default Go build cache directory isn't
writable (e.g. a locked-down sandbox), set `GOCACHE` to somewhere it is:
`GOCACHE=$(mktemp -d) go test ... -fuzz ...`.

### Adversarial and chaos tests

`*/adversarial_test.go` (in `internal/casserver`, `internal/hubserver`)
post deliberately malformed/hostile payloads at the real HTTP handlers —
truncated bodies, hostile path segments, malformed `Range` headers,
`Content-Length` lies — and assert both a clean error response *and* that
the server keeps working correctly afterward. `internal/casserver/chaos_test.go`
goes further: sustained concurrent mixed valid/invalid traffic, an
upload interrupted mid-body followed by a clean retry, and
upload/fetch/eviction-sweep interleaving under a tight storage budget —
each checked against actual data integrity (byte-identical round-trips),
not just "didn't crash." Run these as part of `make test` like any other
Go test; they're intentionally fast enough not to need a separate target.

### Benchmarks

`*/benchmark_test.go` covers chunking (`internal/chunk`), hashing
(`internal/merklehash`), LZ4/ByteGrouping4 decompression (`internal/lz4`,
`internal/bg4`), and dedup speed (`internal/api` — fully-duplicate vs.
always-unique upload throughput at the same size, quantifying what dedup
actually buys in wall-clock terms):

```bash
go test ./internal/merklehash/... -bench . -benchtime 1s -benchmem -run '^$'
```

## 3. Working with Documentation

This project uses **godoc comments as the source of truth** for
implementation details, with a small Go program
(`scripts/build_docs.go`, its own module so the main `xet-server` module
keeps zero external dependencies) to make them — and every other
Markdown doc in this repo — browsable:

1. **Every package has a `// Package foo ...` comment** directly above its
   `package foo` declaration, on exactly one file in that package (Go
   convention — don't duplicate it across files). These comments are
   substantial: they explain *why* the package exists and what real-world
   spec/source it's porting or implementing, not just what the code does
   mechanically. Read a few in `internal/merklehash/datahash.go` or
   `internal/casserver/casserver.go` for the expected depth.
2. **Exported types and functions get a doc comment** starting with the
   identifier's name (`// XorbHash computes ...`), per standard Go
   convention, so `go doc` and godoc.org-style tooling render correctly.
3. **Non-obvious "why" comments belong inline**, not in a separate design
   doc — e.g. a citation to the exact xet-core source file/behavior a piece
   of code mirrors, or the reason a workaround exists. See
   [docs/PROTOCOL.md](docs/PROTOCOL.md) for the cases substantial enough to
   warrant a standalone writeup instead.

View a package's rendered docs locally:

```bash
go doc ./internal/merklehash
go doc ./internal/merklehash Hash.Hex
```

Or regenerate the committed `docs/godoc/*.md` package reference and render
every doc (README, CHANGELOG, CONTRIBUTING, `docs/*.md`, `docs/godoc/*.md`)
to browsable HTML:

```bash
make docs          # -> docs/godoc/*.md (commit these) + docs/build/*.html
make docs-serve    # same, then serves docs/build/ locally
```

`docs/godoc/*.md` is committed source — it's plain Markdown, so it renders
natively on GitHub with no build step. `docs/build/` is a disposable,
gitignored build artifact; never commit it.

If you add a new package, give it a `// Package foo ...` comment before
opening a PR — `go vet` won't catch a missing one, but reviewers will.

## 4. Branching and Pull Requests

- **`main`** contains the latest stable code; branch from it.
- **Feature branches**: `feature/short-description`. **Fixes**:
  `fix/short-description`.
- **PR titles** should follow [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/)
  (`feat: ...`, `fix: ...`, `docs: ...`) so the history stays scannable.
- Run `make all` before opening a PR (format, vet, build, unit tests,
  integration tests). Fill out the PR template checklist.

## 5. Directory Structure

```
Xet-Server/
├── cmd/
│   ├── xetd/                 # server binary: CAS API + Xet Data API + optional Hub API shim
│   └── xet/                  # CLI client for the Xet Data API
├── internal/
│   ├── merklehash/           # BLAKE3-keyed Merkle hashing (xet-core DataHash port)
│   ├── xorbformat/           # xorb binary format (chunk headers, V1 footer)
│   ├── shardformat/          # shard binary format (file/xorb info, lookup tables)
│   ├── lz4/                  # from-scratch LZ4 block+frame decoder
│   ├── bg4/                  # ByteGrouping4 codec
│   ├── sigv4/                # from-scratch AWS SigV4 request signer
│   ├── storage/              # Store interface + fsstore/s3store backends
│   ├── eviction/              # optional storage-budget auto-pruning sweep
│   ├── ratelimit/             # optional per-source-IP upload rate limiter
│   ├── auth/                  # pluggable AuthN/AuthZ (Authenticator/CredentialHelper)
│   ├── routing/                # declarative route-table helpers (Mount/Apply)
│   ├── apidocs/                # embedded OpenAPI spec + Swagger UI (served at /api-docs)
│   ├── landingpage/             # HTML landing pages for xetd's ports
│   ├── casserver/            # wire-compatible CAS HTTP API
│   ├── hubserver/            # Hub REST API shim (repo/commit/resolve)
│   ├── chunk/, manifest/, api/, client/  # original simple chunk/dedup Xet Data API
│   └── ...
├── third_party/
│   └── swagger-ui-dist/       # vendored Swagger UI static assets (see VENDORED.md)
├── scripts/                  # build_docs.go — own go.mod, keeps the main
│                              # module dependency-free (goldmark/chroma/
│                              # goldmark-mermaid live only here)
├── integration-tests/        # bash scripts run by integrationTests.sh
├── docs/
│   ├── ARCHITECTURE.md       # system diagram + package responsibilities
│   ├── PROTOCOL.md           # wire-compatibility deep dive
│   ├── godoc/                # generated package reference (commit these; `make docs`)
│   └── build/                # rendered HTML (gitignored; `make docs-serve`)
└── Makefile
```

## 6. Where to Look for Protocol Details

If you're changing anything in `casserver`, `hubserver`, `xorbformat`,
`shardformat`, `merklehash`, `lz4`, or `bg4`, read
[docs/PROTOCOL.md](docs/PROTOCOL.md) first — it documents several places
where the real `hf_xet`/xet-core wire format differs from what the public
OpenAPI spec or source comments imply, discovered only by capturing and
replaying real client traffic. Silently "fixing" one of these to match the
spec instead of the real client will break compatibility again.

---

Thank you for contributing!
