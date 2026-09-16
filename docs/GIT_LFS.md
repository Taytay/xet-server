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
(default 1h). Clients never see the secret in a token.

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

Locks are persisted in the hub snapshot with everything else.

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
