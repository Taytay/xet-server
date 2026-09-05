# Protocol notes: where reality diverges from the spec

This document exists because getting wire compatibility with real
`hf_xet`/xet-core required more than reading the OpenAPI spec and source
comments — several behaviors only surfaced by capturing and replaying
actual bytes from a live `hf_xet` client session, and diverge from what the
documented format implies. If you're changing `casserver`, `hubserver`, or
any of the protocol packages, read this first: reverting one of these to
match the "obvious" spec reading will silently break real-client
compatibility again.

Every finding below has a regression test replaying the real captured
bytes (see `internal/*/testdata/`) — don't delete those fixtures.

## 1. Hashing: BLAKE3-keyed, not SHA-256, with a non-obvious hex encoding

xet-core's `DataHash` (`xet_core_structures::merklehash::data_hash`) is not
a plain SHA-256 or unkeyed BLAKE3 hash. Two distinct keys are used:

- **`DATA_KEY`** — for leaf/chunk hashes (`compute_data_hash`)
- **`INTERNAL_NODE_HASH`** — for interior Merkle-tree nodes
  (`compute_internal_node_hash`)

Both are fed through `blake3::keyed_hash`. `internal/merklehash/datahash.go`
ports both keys verbatim.

**The hex encoding is not a straight byte-to-hex conversion.** `DataHash`
stores its 32 bytes as four little-endian `u64` words in memory
(`[u64; 4]`), and `DataHash::hex()` prints each word as 16 hex digits in
normal (big-endian) digit order — which means each 8-byte group's byte
order is reversed relative to a naive hex-encode of the raw bytes. Every
hash that appears as a JSON string (a `file_id` in a URL, a `hash` field in
a reconstruction response) goes through this transform;
`internal/merklehash/datahash.go`'s `Hex()`/`FromHex()` implement it, and
`TestHex_ByteOrderReferenceVector` in `datahash_test.go` pins it against
xet-core's own test vector — a case chosen specifically because it is *not*
symmetric under a naive whole-buffer hex encode, so a byte-order regression
would fail that test immediately.

By contrast, **hashes stored in a binary format field are raw wire bytes,
no transform** — `write_hash`/`read_hash` in xet-core's
`serialization_utils.rs` just call `m.as_bytes()`. `merklehash.Hash.Bytes()`
/ `FromRawBytes()` are the untransformed accessors used by `xorbformat` and
`shardformat`; `Hex()`/`FromHex()` are only for the JSON/URL-facing side.

**The one field that breaks this rule**: `shardformat.FileMetadataExt.SHA256`
reuses the 32-byte `Hash` type purely for storage (it holds a plain SHA-256,
not a Merkle hash), but real `hf_xet` clients write its wire bytes through
the *same* word-reversal transform as `Hex()` anyway — confirmed by
capturing a real upload and comparing the field's raw bytes against the
file's actual SHA-256 as declared in the commit payload's `lfsFile.oid`:
`Hex()` of the raw bytes equals `lfsFile.oid` exactly; a raw hex encode
(`hex.EncodeToString(h.Bytes())`) does not. `casserver.handleUploadShard`'s
`sha256ToXet` index is therefore keyed by `h.Hex()`, not `h.Bytes()` — the
opposite of what the "raw bytes for binary-format fields" rule above would
suggest, and easy to get backwards (this project did, initially: the
lookup silently missed on every real download until traced back to this).

The Merkle-aggregation algorithm itself (branching factor 4, "natural cut"
when a node's low 64 bits are divisible by 4, salted HMAC for file hashes)
is ported in `internal/merklehash/aggregated_hashes.go` and verified against
reference vectors xet-core's own test suite publishes specifically "to
ensure that other implementations or ports of these functions produce the
correct hashes" (their words, in
`aggregated_hashes.rs::test_correctness`).

## 2. Xorbs are uploaded without a footer

`internal/xorbformat` implements the full `XorbObjectInfoV1` footer format
(chunk hashes, physical/logical boundary offsets, `XETBLOB`/`XBLBHSH`/
`XBLBBND` section idents) — this is the on-disk/cache format xet-core uses
locally. **It is not what gets POSTed to `/v1/xorbs/{prefix}/{hash}`.**

xet-core's `file_upload_session.rs` builds the upload with:

```rust
SerializedXorbObject::from_xorb(xorb, /* serialize_footer = */ false, ...)
```

with the comment directly above it:

> XORBs are sent without footer - the server/client reconstructs it from
> chunk data.

A captured real upload confirmed this exactly: a 500,000-byte file's xorb
upload body was **500,072 bytes total — precisely the sum of 9 chunk
headers + payloads, with zero bytes left over for a footer.** The original
implementation of `casserver.handleUploadXorb` assumed a footer would
follow the last chunk and rejected every real upload with "malformed xorb
footer: EOF."

**The fix**: `handleUploadXorb` now scans chunk headers to EOF via
`xorbformat.ScanChunks`, and for each chunk, decompresses the payload
(per its declared scheme — see §4) and re-hashes the *decompressed* bytes
with `merklehash.ComputeDataHash`. The resulting chunk hash list is
aggregated with `merklehash.XorbHash` and checked against the hash the
client claimed in the URL. The footer the server needs internally
(`xorbformat.FooterV1`, held in `casserver.Server.xorbFooters`) is built
from this scan, never trusted from the wire.

## 3. Shards are uploaded without a footer either — and file headers carry extra flagged sections

Same pattern as xorbs, confirmed the same way: a captured real shard
upload for the same 500,000-byte file was exactly 816 bytes, and walking
the file-info + xorb-info sections by hand (header → 1 file entry → bookend
→ 1 xorb entry with 9 chunks → bookend) landed **exactly** at byte 816 —
zero bytes left for lookup tables or a footer.

The exact source, `xet-core`'s `shard_interface/native.rs`:

```rust
fn read_shard_to_bytes_remove_footer(si: &Arc<MDBShardFile>) -> Result<Bytes> {
    let split_off_index = si.shard.metadata.file_lookup_offset as usize;
    // Read only the portion of the shard file up to the file_lookup_offset,
    // which excludes the footer and lookup sections.
    ...
    header.footer_size = 0;
    ...
}
```

It truncates a normal (footer-having) shard at `file_lookup_offset` — i.e.
right after both content sections' bookend headers — and rewrites the
header's `footer_size` field to `0` before sending.

There's a second, independent gotcha in the file-info section itself: real
clients set **both** `MDB_FILE_FLAG_WITH_VERIFICATION` and
`MDB_FILE_FLAG_WITH_METADATA_EXT` on every file header
(`file_flags = 0xC0000000` in the captured bytes). A reader that ignores
these flag bits reads a fixed-size `FileDataSequenceEntry` array and then
immediately misparses everything after it — the next "header" it reads is
actually a `FileVerificationEntry`, and everything cascades from there.

`shardformat.ReadShard` (via `readShardWithoutFooter`/`readFileInfoSection`)
now:
1. Branches on `header.FooterSize == 0` to decide whether to read
   sequentially to EOF (real client) or seek to a footer at the end (a
   shard this package wrote itself via `WriteShard`, which still uses the
   full self-describing format for local round-trip testing).
2. Reads `FileVerificationEntry`/`FileMetadataExt` conditionally, per
   `FileDataSequenceHeader.ContainsVerification()`/`ContainsMetadataExt()`.

The `FileMetadataExt.SHA256` field is also how `casserver` bridges a
committed file's plain SHA-256 (from the Hub API's commit payload) to its
Xet/Merkle hash — see `casserver.handleUploadShard`'s population of
`sha256ToXet`, and the byte-order note in §1 about why that uses `.Hex()`
rather than `.Bytes()`.

## 4. Real chunk payloads are compressed — with LZ4 (frame format) and, optionally, byte-grouping

Not every chunk uses `CompressionScheme::None`. A capture of a
highly-compressible upload (repeated text, ~900 KB) produced 7 chunks all
tagged `CompressionScheme::LZ4`, compressing to 585 bytes each. Since
verifying a chunk hash requires the *uncompressed* bytes, the server needs
a real decoder — trusting the client's claimed hash for anything except
`CompressionScheme::None` would mean silently skipping verification for
the common case.

Two details matter for building that decoder correctly:

- xet-core uses the **LZ4 frame format** via the Rust `lz4_flex` crate's
  `frame` module — not raw LZ4 blocks. `internal/lz4/frame.go` implements
  frame parsing (magic number, frame descriptor, per-block size headers,
  end mark) on top of `internal/lz4/block.go`'s block decoder, both written
  directly from the public LZ4 format specs (block:
  `lz4_Block_format.md`, frame: `lz4_Frame_format.md`), not ported from any
  existing implementation. `TestDecompressFrame_RealHFXetCapture` replays
  the actual captured LZ4 frame from a real upload and checks the decoded
  text matches.
- `CompressionScheme::ByteGrouping4LZ4` applies a byte-regrouping transform
  *before* LZ4 (regroup bytes by position-mod-4, then compress — exposes
  more redundancy in structured binary data like same-width floats).
  `internal/bg4` implements the reverse transform, verified against a
  published test vector from an independent third-party reference
  implementation, [zig-xet](https://github.com/jedisct1/zig-xet)
  (`src/compression.zig`), rather than xet-core's own Rust source (which
  wasn't read for this specific transform — only its existence was
  confirmed there).

`casserver.decompressChunkPayload` dispatches on the chunk header's
declared scheme to the right decoder before re-hashing.

## 5. Reconstruction must honor Range and return 416 at EOF — not an empty 200

The `GET /v1/reconstructions/{file_id}` handler originally ignored the
`Range` header entirely and always returned the same full-file term list.
This passed every test that only requested a whole small file in one shot
— and then failed on a real 500 KB download with:

```
RuntimeError: Task error: File reconstruction error: Internal Writer Error:
Byte range not sequential: expected start at 500000, got 256000000
```

The cause, traced through `xet_data::file_reconstruction`: `hf_xet` pages
through a file by calling `next_file_terms()` repeatedly, each time
re-requesting reconstruction for a **shifted byte-range window**
(`ReconstructionTermManager::prefetch_block`, sized dynamically based on
throughput). Since the server ignored `Range` and always returned the same
`[0, file_size)` term, the second prefetch call got back byte range
`[0, 500000)` again — but the client's `SequentialWriter` was already
expecting the *next* window to start where the previous one left off, and
saw a wildly different (miscomputed, from the mismatched term) start
offset instead.

The client's own documented contract for "no more data" is a plain HTTP
status, confirmed in `remote_client.rs`'s `get_reconstruction_impl`:

```rust
.with_expected_416()
...
Err(ClientError::ReqwestError(ref e, _))
    if e.status() == Some(StatusCode::RANGE_NOT_SATISFIABLE) => Ok(None),
```

**The fix**: `handleReconstructionV1` now parses the `Range` header (via the
same `parseByteRange` helper the xorb-fetch endpoint uses), clips terms to
the requested window, computes `offset_into_first_range` correctly for a
window that starts mid-term, and returns `416` when `rangeStart >= fileSize`
— exactly the signal real `hf_xet` is waiting for to stop paging.

## 6. The Hub API's xet-token response is a JSON body, not headers-only

`GET /api/{repo_type}s/{repo_id}/xet-{read,write}-token/{revision}` looked
like a pure headers-in/headers-out exchange from `huggingface_hub`'s Python
side: `parse_xet_connection_info_from_headers` reads `X-Xet-Cas-Url`,
`X-Xet-Access-Token`, and `X-Xet-Token-Expiration` straight off the
response headers, and the original `hubserver.handleXetToken` set exactly
those three headers and returned an empty `200` body.

That's the wrong contract for the code path a real `hf upload`/`hf
download` actually exercises. `hf_xet`'s Rust client fetches this same
token via `DirectRefreshRouteTokenRefresher::get_cas_jwt`
(`xet_client::cas_client::auth`), which decodes the response **body** as
JSON into a `CasJWTInfo` struct
(`xet_client::hub_client::types`):

```rust
#[derive(Deserialize, Debug)]
#[serde(rename_all = "camelCase")]
pub struct CasJWTInfo {
    pub cas_url: String,
    pub exp: u64,
    pub access_token: String,
}
```

An empty body fails that JSON decode. `retry_wrapper.rs`'s
`run_and_extract_json` classifies a decode error as **transient**
(`e.is_decode()` → `RetryableReqwestError::RetryableError`) and retries
with backoff — so instead of failing fast, `hf upload` just hung,
re-requesting the same token every couple of seconds until the caller's
own timeout eventually killed it. `parse_xet_connection_info_from_headers`
is real code, just for a different caller (the resolve/download metadata
path, which pairs `X-Xet-Hash` with a `Link: rel="xet-auth"` or
`X-Xet-Refresh-Route` header) — not the write/read-token refresh endpoint
`hf_xet`'s upload/download session itself calls.

**The fix**: `handleXetToken` now returns `{"casUrl", "exp",
"accessToken"}` as a JSON body (matching `CasJWTInfo`'s wire format
exactly, camelCase field names included) *in addition to* the `X-Xet-*`
headers, so both call sites are satisfied.

## How these were found: capture, don't guess

Every fix above came from the same loop, not from re-reading the spec more
carefully:

1. Point `hf_xet`'s low-level `XetSession` Python API directly at a local
   `casserver` instance (`endpoint=` kwarg — no Hub API needed for this
   step).
2. Run a real upload/download and read the actual error. Run `xetd` with
   `DEBUG=1` alongside this to log every request it receives (method,
   path, status, duration) plus commit/shard/resolve lookup details — the
   §3 and §6 findings above were both traced this way: a request that
   *should* have hit the CAS server (e.g. `POST /v1/xorbs/...`) never
   showed up in the debug log at all, which pointed straight at the Hub
   API step just before it instead of anywhere in the CAS path.
3. When the error was opaque (e.g. a byte-count mismatch), insert a
   capturing HTTP proxy between the client and the server to record the
   exact raw request bytes.
4. Hand-parse the captured bytes against the expected struct layout, byte
   offset by byte offset, until the actual layout became obvious.
5. Cross-reference the real xet-core source once the shape was known, to
   confirm *why* (not just *that*) the real format differs, and to find the
   exact comment/function explaining it.
6. Fix the Go implementation, then commit the captured bytes as a
   `testdata/` regression fixture so the bug can't silently return.

If you hit a new wire-compatibility gap, follow the same loop — the
`internal/casserver/testdata/`, `internal/lz4/testdata/`, and
`internal/shardformat/testdata/` fixtures already checked in were all
produced this way, and are a good reference for how to capture and encode
a new one.
