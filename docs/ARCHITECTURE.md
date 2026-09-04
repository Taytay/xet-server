# Architecture

Xet Server is two cooperating HTTP servers — a **CAS (Content Addressable
Storage) server** and a **Hub API shim** — plus a set of protocol packages
that give both servers wire compatibility with Hugging Face's real Xet
ecosystem (`hf_xet`/xet-core), and a simpler standalone demo API kept
alongside them for quick manual testing.

## System overview

```mermaid
graph TB
    subgraph "Real client (unmodified)"
        HFCli["hf CLI / huggingface_hub / hf_xet\n━━━━━━━━━━━━━━━━━\nreal upload/download client code"]
    end

    subgraph "cmd/xetd"
        direction TB
        Hub["hubserver — Hub API shim\n━━━━━━━━━━━━━━━━━\nrepo create · preupload\nxet-{read,write}-token · commit\nresolve/HEAD metadata"]
        CAS["casserver — CAS HTTP API\n━━━━━━━━━━━━━━━━━\nxorb upload/fetch · shard upload\nreconstruction (Range-aware)\nchunk-dedup · telemetry stubs"]
        Demo["api — simple demo API\n━━━━━━━━━━━━━━━━━\nPOST /upload · GET /files/{id}"]
    end

    subgraph "Protocol packages (internal/)"
        direction LR
        Hash["merklehash\nBLAKE3-keyed Merkle hash"]
        Xorb["xorbformat\nchunk headers + V1 footer"]
        Shard["shardformat\nfile/xorb info + lookup tables"]
        LZ4["lz4\nblock + frame decoder"]
        BG4["bg4\nByteGrouping4 codec"]
    end

    subgraph "Storage backends"
        direction LR
        FS["fsstore\nfilesystem"]
        S3["s3store\nS3 / MinIO via sigv4"]
    end

    HFCli -->|"HF_ENDPOINT"| Hub
    Hub -->|"X-Xet-Cas-Url header"| CAS
    HFCli -.->|"or point hf_xet directly at"| CAS

    CAS --> Hash
    CAS --> Xorb
    CAS --> Shard
    Xorb --> LZ4
    Xorb --> BG4
    CAS --> FS
    CAS --> S3

    style HFCli fill:#e1f5ff,stroke:#0099cc
    style Hub fill:#fff3e0,stroke:#ff9900
    style CAS fill:#e1ffe1,stroke:#00cc66,stroke-width:3px
    style Demo fill:#f9f9f9,stroke:#999
```

`cmd/xetd` runs the CAS server (and, if `-hub-addr` is set, the Hub shim as
a second listener) plus the demo API — three separate `http.Handler`s
mounted on independent muxes/ports, matching how huggingface.co's real Hub
and CAS are actually separate services rather than one monolith.

## Upload flow

```mermaid
sequenceDiagram
    participant Client as hf_xet client
    participant Hub as hubserver
    participant CAS as casserver
    participant Store as storage.Store

    Client->>Hub: POST /api/repos/create
    Client->>Hub: POST /api/{type}s/{repo}/preupload/{rev}
    Hub-->>Client: uploadMode: "lfs" per file
    Client->>Hub: GET /api/{type}s/{repo}/xet-write-token/{rev}
    Hub-->>Client: X-Xet-Cas-Url, X-Xet-Access-Token (headers)

    Client->>CAS: POST /v1/xorbs/default/{hash}  (no footer)
    Note over CAS: scan chunk headers, decompress<br/>each (lz4/bg4 as needed), re-hash,<br/>verify against claimed hash
    CAS->>Store: Put(hash, raw bytes)
    CAS-->>Client: {was_inserted}

    Client->>CAS: POST /v1/shards  (no footer,<br/>verification+metadata_ext flags set)
    Note over CAS: index file to xorb/chunk-range entries,<br/>record sha256 to Xet-hash mapping
    CAS-->>Client: {result: 1}

    Client->>Hub: POST /api/{type}s/{repo}/commit/{rev}  (ndjson, lfsFile.oid = sha256)
    Hub-->>Client: {commitOid, commitUrl}
```

The two "no footer" notes are the load-bearing detail: real `hf_xet`
clients strip the footer/lookup-table sections from both xorb and shard
uploads before sending them, and expect the **server** to reconstruct that
metadata by scanning headers. See [PROTOCOL.md](PROTOCOL.md) for how this
was discovered and why it's correct per xet-core's own source.

## Download flow

```mermaid
sequenceDiagram
    participant Client as hf_xet client
    participant Hub as hubserver
    participant CAS as casserver

    Client->>Hub: HEAD /{repo}/resolve/{rev}/{filename}
    Hub-->>Client: X-Xet-Hash, X-Xet-Refresh-Route, X-Linked-Size (headers)
    Client->>Hub: GET xet-read-token (via refresh route)
    Hub-->>Client: X-Xet-Cas-Url, X-Xet-Access-Token

    loop paged by byte range
        Client->>CAS: GET /v1/reconstructions/{file_hash}<br/>Range: bytes=start-end
        alt range within file
            CAS-->>Client: {terms, fetch_info} (200)
            Client->>CAS: GET /v1/xorbs/default/{hash}<br/>Range: bytes=start-end
            CAS-->>Client: raw (possibly compressed) chunk bytes
        else range at/past EOF
            CAS-->>Client: 416 Range Not Satisfiable
        end
    end
```

The `416`-at-EOF behavior is required, not optional: `hf_xet` pages through
large files by re-requesting `/v1/reconstructions` with successively higher
`Range` windows, and treats `416` as "no more data, stop" — an empty `200`
instead leaves its sequential writer waiting forever for a term that will
never arrive (this was the second real bug found in this project; see
PROTOCOL.md).

## Package responsibilities

| Package | Responsibility |
|---|---|
| `internal/merklehash` | Byte-for-byte port of xet-core's `DataHash`: BLAKE3-keyed leaf/interior-node hashing, the Merkle-aggregation algorithm (branching factor 4, natural-cut rule), and the non-obvious hex-encoding byte-order transform. Verified against xet-core's own published reference vectors. |
| `internal/xorbformat` | Xorb chunk header (version/compressed-length/scheme/uncompressed-length) and the V1 footer format (chunk hashes + boundary offsets). A CAS server almost never needs the footer on the wire — see PROTOCOL.md — but this package can both parse and produce it for local round-trip testing. |
| `internal/shardformat` | Shard header/footer, file-info and xorb-info content sections (including the verification/metadata_ext trailing entries real clients always send), and the three lookup tables. Supports reading both a full shard (with footer) and a footer-less real-client upload. |
| `internal/lz4` | LZ4 block + frame decompression, written from the public LZ4 format specs. Needed because a CAS server must decompress and re-hash chunk payloads to verify a client's claimed hash, regardless of which compression scheme was used. |
| `internal/bg4` | The `ByteGrouping4` reverse transform xet-core applies before LZ4 for the `ByteGrouping4LZ4` scheme. |
| `internal/sigv4` | AWS Signature Version 4 request signing (header-based and query-string presigning), written from the public AWS spec — used by `s3store`, not tied to any AWS SDK. |
| `internal/storage` | The `Store` interface (`Put`/`Get`/`GetRange`/`Has`) plus the optional `URLPresigner` capability interface, with `fsstore` (filesystem) and `s3store` (S3/MinIO via `sigv4`) implementations. |
| `internal/casserver` | The real Xet CAS HTTP API: xorb upload/fetch, shard upload, reconstruction (Range-aware), chunk-dedup and telemetry stubs. This is where the protocol packages above are wired together into an HTTP surface. |
| `internal/hubserver` | A minimal shim of huggingface.co's Hub REST API (repo create, preupload, xet-token issuance, commit, resolve/HEAD) — separate from the CAS API, since real Hub and CAS are separate services. |
| `internal/chunk`, `internal/manifest`, `internal/api`, `internal/client` | The original, simpler, non-wire-compatible chunk/dedup demo (gear-hash CDC + JSON manifests) this project started as. Kept for quick manual testing via `cmd/xet`/`internal/api`; unrelated to the CAS/Hub protocol work. |

## Design decisions worth knowing

- **No decompression on the write path unless verifying.** The CAS server
  stores xorb bytes as an opaque blob (matching real production CAS's
  role) and only decompresses transiently, during upload, to compute the
  chunk hashes needed to verify the client's claimed xorb hash. It never
  needs to decompress again after that.
- **Presigned URLs are optional, not required.** `fetch_info` URLs point at
  a presigned S3/MinIO URL when the storage backend implements
  `storage.URLPresigner`, or fall back to the CAS server's own
  `/v1/xorbs/{prefix}/{hash}` byte-serving endpoint otherwise. Both are
  spec-valid; production Xet always uses the presigned-URL path.
- **In-memory indices, not a persisted database.** `casserver.Server` and
  `hubserver.Server` hold their reconstruction/repo state in memory,
  protected by a `sync.RWMutex`. This is a deliberate simplification for a
  local/single-node server — bulk chunk data lives in the pluggable
  `storage.Store`, but the metadata that maps files to chunks does not
  survive a restart.
