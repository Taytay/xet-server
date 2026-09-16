# Git LFS on xetd: a Xet-backed LFS server

`xetd -hub-addr` serves the [Git LFS API](https://github.com/git-lfs/git-lfs/tree/main/docs/api)
under `/{owner}/{name}.git/info/lfs`. Point a repo's `lfs.url` there and the
stock `git-lfs` client stores its objects in this server's Xet CAS, with
chunk-level deduplication on upload, while the git objects go to any plain
git host (a bare repo over SSH or `git://`, Forgejo, GitHub). No custom git
server is involved.

```
 developer machine                          server
 ┌──────────────────────┐                   ┌─────────────────────────────┐
 │ git push ────────────┼── git objects ───►│ any git host                │
 │   └─ git-lfs         │                   │                             │
 │        ├─ batch ─────┼── POST batch ────►│ xetd hub port  (-hub-addr)  │
 │        │             │◄─ transfer:xet ───│   /{repo}.git/info/lfs/...  │
 │        └─ git-xet ───┼── xorbs/shards ──►│ xetd CAS port  (-addr)      │
 │ git pull             │                   │   /v1/xorbs /shards ...     │
 │   └─ git-lfs basic ──┼── GET objects/oid►│ xetd: reconstruct by oid    │
 └──────────────────────┘                   └─────────────────────────────┘
```

## Client requirements

- `git-lfs` (any 3.x).
- `git-xet` 0.2.x from [huggingface/xet-core](https://github.com/huggingface/xet-core/releases)
  for **pushing**. It registers itself as the `xet` custom transfer agent
  (`git xet install`, or `--local` for one repo). Pulling needs only git-lfs.
- A credential for the hub port: the server's shared secret, sent
  - by git-lfs as HTTP Basic (`<any user name>:<secret>`) through git's
    credential helper (the user name becomes the owner of file locks), and
  - by git-xet as a Bearer token for its token-refresh route, resolved from
    (in order) credentials embedded in the git remote URL, `$HF_TOKEN`,
    `~/.netrc` for the remote's host, `git-lfs-authenticate` over SSH, or
    git's credential helper for the **git remote's** host. On a LAN setup
    where the git remote and the LFS server are different hosts, `HF_TOKEN`
    is the reliable choice.

## Server

```bash
XETD_AUTH_TOKEN=<secret> xetd -addr :8420 -hub-addr :8421 \
    -cas-url http://server:8420 -data /srv/xet
```

`-cas-url` must be the CAS address as clients reach it: it is handed to
git-xet in every batch response. With a secret set, both ports authenticate
with `auth.SignedTokenAuth`: the secret itself (Bearer or Basic password),
and short-lived HMAC-signed tokens the hub port mints for the CAS - scoped
read or write, bound to the caller's user name, expiring after `-token-ttl`
(default 1h). Clients never see the secret in a token. The xorb URLs in a
reconstruction response carry such a read token in their query string,
because xet-core fetches them with no Authorization header (on the real
Hub they are presigned CDN URLs); only the xorb GET/HEAD routes accept a
token from a URL.

## Repo setup

```bash
git init game && cd game
git lfs install --local
git xet install --local
git remote add origin git@nas:srv/git/team/game.git         # any git host
git config -f .lfsconfig lfs.url http://server:8421/team/game.git/info/lfs
git config lfs.locksverify true
export HF_TOKEN=<secret>
printf '*.psd filter=lfs diff=lfs merge=lfs -text lockable\n' > .gitattributes
git add .lfsconfig .gitattributes && git commit -m "LFS via Xet"
```

The LFS URL must end in `{owner}/{name}.git/info/lfs`: the two segments
before `.git` are the repo id (an optional type prefix such as
`datasets/` is accepted and ignored, as on resolve URLs).

## What each endpoint does

| Method | Path under `…/info/lfs` | Scope | Behavior |
|---|---|---|---|
| POST | `objects/batch` (upload) | write | If the client offers `xet`: transfer `xet`; each object not yet stored gets an `upload` action whose `href` is the `xet-write-token` refresh route and whose headers carry `X-Xet-Cas-Url`, `X-Xet-Access-Token`, `X-Xet-Token-Expiration`, `X-Xet-Session-Id`. Stored objects get no action (git-lfs skips them). Without `xet`: transfer `basic` and a per-object error telling the user to install git-xet - this server stores nothing but xorbs. |
| POST | `objects/batch` (download) | read | Transfer `basic`; each stored object gets a `download` action to `objects/{oid}` with a minted read token in its header. Unknown oids get a per-object 404. |
| GET/HEAD | `objects/{oid}` | read | The file's plain bytes, reconstructed from xorbs (`casserver.ReconstructFile`), with `Content-Length`, `Accept-Ranges`, single-range `Range` support (206), and a SHA-256 check of the streamed whole file. |
| POST/GET | `locks` | write/read | Create (201; 409 with the existing lock if the path is taken) and list (`path`, `id`, `cursor`, `limit` filters). |
| POST | `locks/verify` | write | `ours`/`theirs` split by the caller's user name. |
| POST | `locks/{id}/unlock` | write | 403 unless owner or `force`. |

Locks are persisted in the hub snapshot with everything else (or as
claim files, in synced-folder mode below).

## Synced-folder mode: no server, one Dropbox

`-sync-folder` lets the data directory be a folder a sync tool carries
between machines - Dropbox, Google Drive, Syncthing, a NAS mount - with
every machine running its own xetd against its own copy:

```
 laptop A                            laptop B
 ┌────────────────────────┐          ┌────────────────────────┐
 │ git-lfs/git-xet        │          │ git-lfs/git-xet        │
 │   └─► xetd (localhost) │          │   └─► xetd (localhost) │
 │         └─► ~/Dropbox/team/xet ◄──sync──► ~/Dropbox/team/xet │
 └────────────────────────┘          └────────────────────────┘
```

```bash
XETD_AUTH_TOKEN=<secret> xetd -sync-folder -data ~/Dropbox/team/xet \
    -addr 127.0.0.1:8420 -hub-addr 127.0.0.1:8421
```

Every repo's `.lfsconfig` then points at `http://127.0.0.1:8421/...` and is
identical on every machine; the git side can be any host, or a
[git-remote-dfs](https://github.com/Taytay/taytays_stuff/tree/main/experiments/git-remote-dfs)
store in the same folder, so history and content ride one dumb folder
with no server anywhere.

What changes under the flag, and why it is safe on a folder two machines
write to at once:

- **Every authoritative file is write-once and named by its content.**
  Xorbs already were (`xorbs/<hash>`). Shard bodies are now persisted the
  same way (`shards/<hash>`, staged through a temp file and renamed), and
  no snapshot is written or read. Two machines writing "the same" file
  write identical bytes, so the sync tool never has anything to merge and
  a "conflicted copy" cannot arise.
- **The index is derived, not stored.** At startup the shard directory is
  scanned and every shard re-indexed; a xorb's footer is derived from its
  bytes the first time it is needed. A file is trusted only if its content
  hashes to its name, so a half-delivered file is invisible until the
  sync completes. The directory is rescanned every `-rescan-interval`
  (default 10s) and, at once, whenever a lookup misses and the directory's
  mtime has moved - so another machine's push is visible the first time a
  client here asks for it, and its chunks take part in dedup on the next
  push here.
- **A file whose xorbs have not arrived yet is refused, not truncated.**
  The batch answers a per-object 503 ("content not yet available on this
  replica, N xorb(s) still syncing") and `objects/{oid}` answers 503 with
  `Retry-After`; `git lfs pull` reports it and succeeds when run again
  after the folder has synced.
- **Locks are claims with an election** (`locks/<repo>/<id>.lock.json`,
  released by a tombstone `<id>.unlock`; nothing is modified or deleted).
  Two people can lock the same path while their folders are apart - each
  replica must grant it, having no way to know better. Once the claims
  meet, every replica computes the same holder: the earliest `locked_at`,
  ties broken by id. The other claim is superseded (not listed, so that
  person's next push is refused by lock verification exactly as if they
  had never held it) and becomes the holder if the winner unlocks first.
  This is the git-remote-dfs ref election applied to locks: no
  compare-and-swap anywhere, and a race is a visible outcome rather than
  corruption.

Consequences to know about:

- Dedup across machines is only as fresh as the sync. A push on B before
  A's shard has arrived re-uploads chunks A already has; both copies are
  kept (different xorbs, same bytes inside) and both are correct.
- `-max-storage-bytes` is refused with `-sync-folder`: eviction deletes
  files other replicas still reference.
- The hub's repo/revision registry (`hf upload`/`hf download`, not
  git-lfs) is in memory only in this mode.
- Temp files (`*.tmp-*`) appear briefly next to the final files; a sync
  tool with an ignore list can exclude that pattern. Dropbox and Syncthing
  deliver files by staging and renaming, which is what the content-hash
  check assumes; a tool that writes in place would show partial files,
  which the check also catches.
- Nothing here is Linux-specific: the binary cross-compiles for Windows
  and macOS, and the on-disk operations are create-temp, rename, stat and
  readdir. Windows has not been exercised end to end.

## Known limits

- Uploads require git-xet. A `basic`-only client cannot push (its objects
  get a clear per-object error). Server-side chunking for basic uploads
  would remove that requirement; it is not implemented.
- Downloads are whole-file over the basic transfer, so a pull is not
  chunk-deduplicated against the client's local cache the way Hub
  downloads through hf_xet are. Fine for a LAN.
- Owner identity is the Basic user name (or the token's bound subject). A
  team on the raw shared secret with no user names sees every lock as
  owned by `shared-secret`; give people distinct user names in their
  credential helper if locking matters.
- The LFS path does not register files under a repo revision, so a file
  pushed with git is not listed by `hf download <repo>`; the reverse works
  (a file from `hf upload` is downloadable through the LFS bridge by its
  SHA-256, since the shard carries it).
- `-max-storage-bytes` eviction treats the store as a cache. For an LFS
  source of truth leave it off.
