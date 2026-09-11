# Mirroring / self-hosting your own models with Xet Server

A practical, copy-pasteable guide to running your own Hugging Face
Hub-compatible model host, so `hf download` and `hf upload` work against
your own server instead of huggingface.co — for private model mirrors, air-gapped
environments, or just keeping a large set of checkpoints somewhere you
control. Sections 1–8 cover `xetd`, where you deliberately upload the
content you want hosted; section 9 covers `xet-proxyd`, which instead
transparently caches whatever's requested through it from the real
huggingface.co — read that section if you want a mirror with no manual
upload step, or one that stays useful even after huggingface.co itself
becomes unreachable.

This guide only uses the real `hf` CLI (from `huggingface_hub`) — nothing
in `cmd/xet`/`internal/api` (the "Xet Data API") is needed for this. If
you've never used this project before, skim
[Architecture](../docs/ARCHITECTURE.md) first; this guide assumes you
already have `xetd`/`xet-proxyd` built (`make build` from the repo root).

## 1. What you're actually running

Real Hugging Face splits "the Hub" (repo/commit/metadata API) and "Xet CAS"
(the actual chunked, deduplicated blob storage) into two separate
services. `xetd` mirrors that split as two listeners from one binary:

- **CAS port** (`-addr`, default `:8420`) — the actual chunk/xorb storage.
- **Hub shim port** (`-hub-addr`, e.g. `:8421`) — the repo API `hf` talks to
  directly; it hands back a token + the CAS address so `hf`'s `hf_xet`
  layer can talk to CAS itself for the actual bytes.

Both need to be running for `hf upload`/`hf download` to work — the Hub
shim on its own can't move any bytes; the CAS server on its own doesn't
know what a repo is.

## 2. Start the server

```bash
./bin/xetd -addr :8420 -hub-addr :8421 -data ./xet-data
```

`./xet-data` is a plain directory — everything (chunks, xorbs, shard
metadata, and the Hub shim's repo/commit index) lives under it, and
survives a restart via periodic snapshotting (see
[Storage backends](../README.md#storage-backends) in the main README). For
a real, long-running mirror, point `-data` at a disk with enough room for
every model you plan to host, plus growth — see
[storage auto-pruning](../README.md#storage-auto-pruning-upload-rate-limiting-dedup-verification-and-persistence)
if you want to cap it instead.

Opening `http://<host>:8420/` or `http://<host>:8421/` in a browser shows a
landing page confirming the server is up and listing what each port
serves — useful for a first sanity check before touching the `hf` CLI at
all.

## 3. Secure it before anyone else can reach it

If this server will be reachable by anyone other than you (even just other
users on the same machine), turn on authentication before going further —
skipping this step means **anyone who can reach the port can upload or
download anything**:

```bash
export XETD_AUTH_TOKEN="$(openssl rand -hex 32)"   # generate once, keep it secret
./bin/xetd -addr :8420 -hub-addr :8421 -data ./xet-data
```

Write that token down now — you'll pass the exact same value as `HF_TOKEN`
in the next step. See [Authentication](../README.md#authentication) in the
main README for the full flag/env-var precedence and what read vs. write
scope means here. If this server is genuinely only ever reachable by you
(e.g. `-addr 127.0.0.1:8420` on your own laptop), you can skip this and
`HF_TOKEN` can be any placeholder string — the server just won't check it.

## 4. Point `hf` at it

```bash
export HF_ENDPOINT="http://<host>:8421"     # the Hub shim port, not the CAS port
export HF_TOKEN="<the token from step 3, or any string if auth is off>"
```

`HF_ENDPOINT` is `huggingface_hub`'s own variable — every `hf` command
(and every `huggingface_hub`/`hf_xet` Python call) respects it
automatically, so nothing else needs to change to point an existing
workflow at your server instead of huggingface.co.

## 5. Upload (host) a model

```bash
hf upload myuser/my-model ./local-model-dir --repo-type model
```

- `myuser/my-model` is created automatically on first upload — there's no
  separate "create repo" step to run by hand.
- Every file gets chunked and deduplicated against everything already
  stored on this server (not just within one repo) — re-uploading a
  near-identical checkpoint after a small fine-tune only transfers and
  stores what actually changed.
- Pushing to a named revision that doesn't exist yet creates it, exactly
  like a real Hub repo's branch:

  ```bash
  hf upload myuser/my-model ./local-model-dir --revision experiment-1
  ```

  `main` always exists once the repo does (created implicitly, matching a
  real repo always having a default branch); any other revision name is
  created the first time something is committed to it. Revisions are
  fully independent — the same file path can hold different content on
  `main` vs. `experiment-1`.

## 6. Download (mirror) a model

```bash
hf download myuser/my-model --local-dir ./downloaded
```

```bash
# a specific revision:
hf download myuser/my-model --revision experiment-1 --local-dir ./downloaded

# a single file, rather than the whole repo:
hf download myuser/my-model config.json --local-dir ./downloaded
```

This is the actual mirroring operation: point `HF_ENDPOINT` at your
server, run `hf download`, and every file streams from your own CAS
storage — no request to huggingface.co is made at all once `HF_ENDPOINT`
is set.

## 7. Verify it end-to-end

```bash
hf upload myuser/verify-mirror ./some-file.bin
hf download myuser/verify-mirror some-file.bin --local-dir ./verify-out
cmp ./some-file.bin ./verify-out/some-file.bin && echo "byte-identical"
```

If that round-trip is byte-identical, your mirror is working correctly.
This exact round-trip is also what `integration-tests/hf_cli_roundtrip.sh`
checks automatically as part of this project's own test suite
(`make integration-test`) — worth running once yourself after any config
change to this server.

## 8. Common problems

**`hf` hangs instead of failing or succeeding.** Almost always a corporate
or sandboxed network proxying `localhost`/your server's address, which
`hf_xet`'s Rust HTTP client doesn't always respect `NO_PROXY` for. Set
`NO_PROXY`/`no_proxy` to include your server's host, or run `hf` from a
network path that doesn't proxy it. See
[Troubleshooting](../README.md#troubleshooting) in the main README for
more detail — this is a client-side networking issue, not a bug in this
server.

**`401 Unauthorized` on every `hf` command.** `xetd` has `-auth-token`/
`$XETD_AUTH_TOKEN` set but `$HF_TOKEN` on the client side is missing,
empty, or doesn't match. Confirm both sides have the exact same value.

**Uploaded a file, but `hf download` can't find it on a different
machine.** Confirm `HF_ENDPOINT` is set to the same value on both
machines, and that the upload actually succeeded (check the command's own
output, or `hf download myuser/my-model --local-dir /tmp/verify` against
your `HF_ENDPOINT` to confirm the file is really there) — a
silently-failed upload with no local error is the most common cause of
"it's not there on the other end."

**Disk filling up over time.** Every uploaded chunk is retained forever by
default. See
[storage auto-pruning](../README.md#storage-auto-pruning-upload-rate-limiting-dedup-verification-and-persistence)
(`-max-storage-bytes`) if you want old, unused xorbs evicted automatically
once total storage crosses a budget.

## 9. A different kind of mirror: `xet-proxyd` (transparent caching, not manual re-hosting)

Everything above assumes you're deliberately `hf upload`-ing content you
already have onto your own `xetd`. If instead you want a mirror that
**transparently caches whatever real models get requested through it** —
no manual upload step, no deciding in advance what to host — use
`xet-proxyd` instead:

```bash
./bin/xet-proxyd -addr :8420 -hub-addr :8421 -data ./xet-proxy-data \
  -upstream-hub-url https://huggingface.co
```

Point `HF_ENDPOINT` at `:8421` exactly like you would for `xetd` — every
`hf download` (and `hf upload`, relayed straight through to the real Hub)
just works, with no repo-by-repo setup. The first time any model is
requested it's fetched from the real huggingface.co and cached; every
request after that (from you or anyone else pointed at this proxy) is
served locally, and **keeps being servable even if huggingface.co itself
becomes unreachable** — that's the entire reason this exists, not a side
effect. See the main README's
[Caching pull-through proxy](../README.md#caching-pull-through-proxy-xet-proxyd)
section for the full flag reference (rate limiting, cache TTL, the
`-no-cache` pure-relay escape hatch), and its
[Offline handoff](../README.md#offline-handoff-proving-the-point)
subsection for the step-by-step "huggingface.co is really gone now" story:
stop the proxy, point a plain `xetd` at the exact same `-data` directory,
and every model that was ever requested through the proxy keeps working,
byte-identical, forever — no proxy process required.

This is the practical answer to "what do I do the day huggingface.co
shuts down or I lose access to it": run `xet-proxyd` starting today, let
it accumulate a cache of whatever your team actually uses, and that cache
is already a complete, working, disconnected model store the moment you
need one.

## What this is not

This is this project's own reimplementation of the Xet CAS + Hub
protocols (see [Where this diverges from real Xet](../README.md#where-this-diverges-from-real-xet)
for the exact, current list of gaps) — not huggingface.co. A plain
`xetd` mirror (sections 1–8 above) has no connection to huggingface.co at
all: nothing you upload is visible there, and nothing there is visible
here, unless you separately mirror it yourself (e.g. `hf download` from
the real Hub, then `hf upload` the same files to your own server with
`HF_ENDPOINT` pointed at each in turn). `xet-proxyd` (section 9) is the
one deliberate exception — by design, it DOES talk to the real
huggingface.co on a cache miss; anything it caches came from a real,
successful request to the real Hub/CAS, not fabricated or copied any
other way.
