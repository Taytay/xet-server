# FAQ

## Why not just put model files in S3 (or any plain HTTP file host) instead of building all this?

Because a model repo isn't one file that changes rarely - it's a set of
large binary files (often tens of GB combined) that get **re-uploaded
with small changes** over and over: a fine-tune, a quantization pass, a
checkpoint saved every N steps. Plain object storage (S3, a static HTTP
host, Git LFS) has no concept of "this new 20GB file is 95% identical to
the one already stored" - every push re-uploads and re-stores the whole
object, every pull re-downloads it whole. At the scale Hugging Face
operates at (millions of repos, files up to hundreds of GB, constant
churn from the whole ML community), that's not just slow - it's the
dominant cost, both in transfer time and in storage.

Xet's answer is **content-defined chunking (CDC) + a global,
cross-repo, cross-user dedup index**: every file is split into
variable-length chunks by a rolling hash (so a small edit only shifts the
chunk boundaries immediately around it, not the whole file), each chunk
is content-addressed by its hash, and chunks already stored **anywhere on
the whole system** - not just in your repo, not just from your account -
are never re-uploaded or re-stored. A near-duplicate 20GB checkpoint often
transfers only the few chunks that actually changed. Git LFS, by
contrast, treats each file version as one opaque blob; S3/HTTP hosting
has no chunking or dedup concept at all. This project (`internal/chunk`,
`internal/casserver`) reimplements exactly that model, wire-compatible
with the real thing - see
[Where this diverges from real Xet](../README.md#where-this-diverges-from-real-xet).

## OK, but why does the *protocol* need to be this complicated - CAS server, separate Hub API, xorbs, shards, reconstructions?

Because the actual data-movement concern (chunk storage/retrieval - "CAS",
Content Addressable Storage) and the repo/commit/metadata concern ("the
Hub") are genuinely different services with different scaling needs, and
real Hugging Face splits them that way - this project mirrors that split
(`internal/casserver` vs. `internal/hubserver`) rather than inventing a
simpler shape that would then not be wire-compatible. The specific
terms:

- **Chunk** - the smallest unit, produced by content-defined chunking.
- **Xorb** - a bundle of chunks uploaded together (real clients batch many
  small chunks into one xorb rather than one HTTP call per chunk).
- **Shard** - the metadata describing which chunks (from which xorbs, at
  which byte ranges) make up a given file, plus a reverse index so a
  chunk can be looked up back to the shard(s) referencing it (global
  dedup).
- **Reconstruction** - the response a CAS server gives for "how do I
  reassemble file X": an ordered list of xorb/chunk-range terms, plus a
  fetch URL per xorb.

See [docs/ARCHITECTURE.md](ARCHITECTURE.md) for the full data-flow
diagrams and [docs/PROTOCOL.md](PROTOCOL.md) for exactly how each of
these terms round-trips over the wire, verified against real client
traffic.

## Why does this project exist - isn't Xet already open source?

xet-core (the real Rust implementation, linked below) is open source, but
it's a *client* library plus the pieces Hugging Face needs to run their
own CAS/Hub servers - there was no publicly available, from-scratch,
independently-verified **server** implementation you could run yourself,
audit line-by-line, or extend, before this project. This one exists to
be exactly that: read every byte on the wire, verified against a live
`hf_xet` client (not just the published spec, which turned out to diverge
from it in several places - see [docs/PROTOCOL.md](PROTOCOL.md)'s "How
these were found" section), so you can self-host, mirror, or simply
understand the real protocol without needing access to Hugging Face's own
server code.

## Why does `xet-proxyd` exist - isn't a mirror just `xetd` with the files uploaded to it?

That works if you already have the files and know in advance which
repos you want to host (see [docs/MIRRORING.md](MIRRORING.md) sections
1-8). `xet-proxyd` solves a different, narrower problem: **you don't want
to manually curate a mirror, you want your existing `hf download`
traffic to transparently become resilient to huggingface.co disappearing
- without changing how you use it.** It's a caching pull-through proxy:
point `HF_ENDPOINT` at it like any other Hub endpoint, and whatever gets
requested through it gets cached automatically. The distinguishing
design point (see [docs/ARCHITECTURE.md](ARCHITECTURE.md#caching-pull-through-proxy-cmdxet-proxyd))
is that it's built by *embedding* the exact same `casserver.Server`/
`hubserver.Server` engines `xetd` runs, not a separate cache
implementation - so the moment you don't need the real huggingface.co
anymore (it's down, rate-limiting you, or genuinely gone), the proxy's
own `-data` directory is already a real, complete `xetd` data directory
you can hand to a plain `xetd` with zero conversion step. See
[docs/MIRRORING.md](MIRRORING.md#9-a-different-kind-of-mirror-xet-proxyd-transparent-caching-not-manual-re-hosting)
for the walkthrough.

## What happens to data I've cached in `xet-proxyd` if huggingface.co goes down mid-request?

Every read path falls back to whatever's already cached, **regardless of
how old it is**, on any upstream failure - network error, timeout, 5xx.
This is deliberate, not a bug: a proxy that errored instead of serving
slightly-stale-but-known-good cached data would be strictly worse for the
"offline resilience" use case this program exists for. Writes (uploads,
commits) are never served from cache - only the real Hub can accept a
real commit, so a write during an outage genuinely fails; there's nothing
to fall back to. See [docs/ARCHITECTURE.md](ARCHITECTURE.md)'s design
decisions section for the full reasoning, including why no
retry-with-backoff logic was added on top of this (it wouldn't reach a
better outcome faster than the existing fallback already does).

## Why BLAKE3 instead of SHA-256 for content hashing?

Because that's what xet-core actually uses on the wire - this project
isn't free to choose; it has to match the real protocol exactly, or a
real `hf_xet` client's hash checks fail. BLAKE3 is also genuinely faster
than SHA-256 for this workload (parallelizable via its Merkle-tree
internal structure), which is presumably *why* xet-core chose it, but
that's not something this project decided - see
[docs/PROTOCOL.md #1](PROTOCOL.md#1-hashing-blake3-keyed-not-sha-256-with-a-non-obvious-hex-encoding)
for the exact keying scheme and a genuinely surprising hex-encoding
byte-order quirk that took real client-traffic capture to discover.

## Why is `github.com/zeebo/blake3` the one external dependency, when everything else (LZ4, SigV4, etc.) is from-scratch?

BLAKE3 is a cryptographic hash function - writing your own is exactly the
kind of thing you should never do, since a subtle bug there breaks the
one thing (content-addressing integrity) this whole system depends on to
detect corruption. LZ4 decoding and AWS SigV4 signing are comparatively
simple, well-specified, low-risk-if-slightly-wrong algorithms verified by
independent test suites - writing them from scratch keeps this project
free of a general-purpose SDK dependency without taking on cryptographic
risk. See [Acknowledgments & Related Projects](../README.md#acknowledgments--related-projects)
for where `zeebo/blake3`'s own verification comes from.

## Does this project connect to the real huggingface.co at all?

Only `xet-proxyd` does, and only by design - see
[docs/MIRRORING.md #9](MIRRORING.md#9-a-different-kind-of-mirror-xet-proxyd-transparent-caching-not-manual-re-hosting).
Plain `xetd` has no network calls to huggingface.co anywhere in its code;
it's a completely standalone server that merely speaks the same wire
protocol. Nothing you upload to a plain `xetd` is ever visible on the
real Hub, and nothing on the real Hub is visible to a plain `xetd` unless
you separately mirror it yourself.

## Is any of this affiliated with or endorsed by Hugging Face?

No. This is an independent, unofficial reimplementation built by reading
the public OpenAPI spec and observing real client traffic - see
[docs/PROTOCOL.md](PROTOCOL.md) for exactly how. It is not affiliated
with, endorsed by, or connected to Hugging Face Inc. in any way.
