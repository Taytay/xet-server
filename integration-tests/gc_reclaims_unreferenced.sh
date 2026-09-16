#!/bin/bash
# XET_IT_TEST_TIMEOUT: 60
# Garbage collection on a standalone server, end to end through the real
# binaries: `xetd gc` against a running, auth-gated xetd.
#
# The store gets a xorb no shard references (a real hf_xet capture,
# uploaded raw - the state a crashed push leaves) and, when the pipenv
# environment is available, a file committed through the Hub shim with
# the real `hf` CLI. Then:
#   - a minted write token (what every client holds) is refused with 403:
#     deleting is an operator action, only the shared secret may do it;
#   - with the default grace the orphan is deferred, since it was just
#     uploaded and a client may still reference it;
#   - with -grace 0 a dry run reports it and deletes nothing, a real run
#     deletes it, and the hf-uploaded file survives without being listed
#     (the Hub registry is part of every keep set) and still downloads;
#   - a file pushed through the demo Xet Data API is untouched: GC only
#     ever walks the CAS's own xorb and shard directories.
set -euo pipefail

# Hardcoded integration-test fixture, not a real credential.
SECRET="it-fixture-token-not-a-real-secret"
CAS_PORT="${XET_IT_GC_TEST_PORT:-18440}"
HUB_PORT="${XET_IT_GC_TEST_HUB_PORT:-18441}"
CAS_URL="http://127.0.0.1:${CAS_PORT}"
HUB_URL="http://127.0.0.1:${HUB_PORT}"
DATA_DIR="$WORKDIR/server-data"
SERVER_LOG="$WORKDIR/xetd-gc.log"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FIXTURE="$REPO_ROOT/internal/casserver/testdata/real_upload_lz4.bin"
FIXTURE_HASH="93c0d0f39f510203216b20b0ad1c1279e49601d47ea887fd3cd473ce00ed5cdb"
export NO_PROXY="localhost,127.0.0.1,${NO_PROXY:-}"
export no_proxy="localhost,127.0.0.1,${no_proxy:-}"

mkdir -p "$DATA_DIR"
XETD_AUTH_TOKEN="$SECRET" "$XETD" -addr ":${CAS_PORT}" -hub-addr ":${HUB_PORT}" -data "$DATA_DIR" >"$SERVER_LOG" 2>&1 &
SERVER_PID=$!
cleanup() {
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
}
trap cleanup EXIT
for _ in $(seq 1 50); do
    curl -s -o /dev/null "$CAS_URL/v1/stats" && break
    sleep 0.1
done
curl -sf -o /dev/null "$CAS_URL/v1/stats" || { echo "xetd did not start"; cat "$SERVER_LOG"; exit 1; }

xorbFiles() { find "$DATA_DIR/xorbs" -type f ! -name '*.tmp-*' | wc -l | tr -d ' '; }
gc() { XETD_AUTH_TOKEN="$SECRET" HF_TOKEN= "$XETD" gc -server "$CAS_URL" "$@"; }

# --- a file through the demo Xet Data API (its own chunk store) ------------
dd if=/dev/urandom of="$WORKDIR/demo.bin" bs=1024 count=300 2>/dev/null
demoId=$("$XET" push -server "$CAS_URL" -auth-token "$SECRET" "$WORKDIR/demo.bin" | awk '/^file_id:/ {print $2}')
[[ -n "$demoId" ]] || { echo "demo push gave no file_id"; exit 1; }

# --- an orphan xorb: uploaded, never described by a shard ------------------
status=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $SECRET" \
    -X POST --data-binary "@$FIXTURE" "$CAS_URL/v1/xorbs/default/$FIXTURE_HASH")
[[ "$status" == 200 ]] || { echo "orphan xorb upload: status $status"; exit 1; }
[[ "$(xorbFiles)" == 1 ]] || { echo "expected 1 xorb file, found $(xorbFiles)"; exit 1; }

# --- a file through the real hf CLI, if the environment exists -------------
hfFile=""
if command -v pipenv >/dev/null 2>&1 && pipenv run hf --version >/dev/null 2>&1; then
    hfFile="$WORKDIR/kept.bin"
    dd if=/dev/urandom of="$hfFile" bs=1024 count=512 2>/dev/null
    export HF_ENDPOINT="$HUB_URL" HF_TOKEN="$SECRET" HF_HOME="$WORKDIR/hf-home" HF_XET_CACHE="$WORKDIR/hf-cache"
    unset HF_HUB_ENABLE_HF_TRANSFER
    if ! timeout 30s pipenv run hf upload it-user/gc-repo "$hfFile" kept.bin >"$WORKDIR/hf-upload.log" 2>&1; then
        echo "hf upload failed:"; cat "$WORKDIR/hf-upload.log"; exit 1
    fi
    [[ "$(xorbFiles)" == 2 ]] || { echo "expected 2 xorb files after hf upload, found $(xorbFiles)"; exit 1; }
    echo "hf upload done; the registry must keep it without a keep list entry"
else
    echo "pipenv/hf not available; skipping the Hub-registry half (the orphan half still runs)"
fi

# --- a minted token cannot collect ------------------------------------------
minted=$(curl -sf -H "Authorization: Bearer $SECRET" "$HUB_URL/api/models/it-user/gc-repo/xet-write-token/main" \
    | grep -o '"accessToken":"[^"]*"' | cut -d'"' -f4)
[[ -n "$minted" ]] || { echo "could not mint a write token from the Hub shim"; exit 1; }
status=$(curl -s -o "$WORKDIR/minted-gc.out" -w '%{http_code}' -H "Authorization: Bearer $minted" \
    -H 'Content-Type: application/json' -d '{"keep":[],"grace_seconds":0}' "$CAS_URL/v1/gc")
[[ "$status" == 403 ]] || { echo "gc with a minted write token: status $status, want 403"; cat "$WORKDIR/minted-gc.out"; exit 1; }
status=$(curl -s -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' -d '{"keep":[]}' "$CAS_URL/v1/gc")
[[ "$status" == 401 ]] || { echo "gc with no credential: status $status, want 401"; exit 1; }
[[ "$(xorbFiles)" -ge 1 ]] || { echo "a refused gc deleted something"; exit 1; }
echo "minted token refused (403), no credential refused (401)"

# --- default grace defers the fresh upload ----------------------------------
gc -keep /dev/null -dry-run >"$WORKDIR/gc-default.out"
cat "$WORKDIR/gc-default.out"
grep -q '1 deferred within the 528h0m0s grace' "$WORKDIR/gc-default.out" || { echo "default grace did not defer the fresh orphan"; exit 1; }

# --- grace 0: dry run reports, real run deletes ------------------------------
gc -keep /dev/null -grace 0 -dry-run >"$WORKDIR/gc-dry.out"
cat "$WORKDIR/gc-dry.out"
grep -q ' 1 would delete (' "$WORKDIR/gc-dry.out" || { echo "dry run did not report the orphan"; exit 1; }
before=$(xorbFiles)
[[ "$before" == "$( [[ -n "$hfFile" ]] && echo 2 || echo 1 )" ]] || { echo "dry run changed the store: $before xorb files"; exit 1; }

gc -keep /dev/null -grace 0 -json >"$WORKDIR/gc-real.json"
cat "$WORKDIR/gc-real.json"
grep -q '"xorbs_deleted": 1' "$WORKDIR/gc-real.json" || { echo "real run did not delete exactly the orphan"; exit 1; }
grep -q '"dry_run": false' "$WORKDIR/gc-real.json" || { echo "real run reported as dry"; exit 1; }
status=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $SECRET" "$CAS_URL/v1/xorbs/default/$FIXTURE_HASH")
[[ "$status" == 404 ]] || { echo "orphan still served after gc: status $status"; exit 1; }
[[ -f "$DATA_DIR/casserver-snapshot.json" ]] || { echo "no snapshot written after gc"; exit 1; }

if [[ -n "$hfFile" ]]; then
    grep -q '"files_kept": 1' "$WORKDIR/gc-real.json" || { echo "the Hub-registered file was not kept"; exit 1; }
    [[ "$(xorbFiles)" == 1 ]] || { echo "expected the hf file's xorb to remain, found $(xorbFiles)"; exit 1; }
    mkdir -p "$WORKDIR/dl"
    if ! timeout 30s pipenv run hf download it-user/gc-repo kept.bin --local-dir "$WORKDIR/dl" >"$WORKDIR/hf-download.log" 2>&1; then
        echo "hf download after gc failed:"; cat "$WORKDIR/hf-download.log"; exit 1
    fi
    cmp "$hfFile" "$WORKDIR/dl/kept.bin"
    echo "Hub-registered file kept and downloads byte-identical after gc"
else
    [[ "$(xorbFiles)" == 0 ]] || { echo "expected an empty xorb store, found $(xorbFiles)"; exit 1; }
fi

# --- the demo API's own store was never touched -----------------------------
"$XET" pull -server "$CAS_URL" -auth-token "$SECRET" -out "$WORKDIR/demo-restored.bin" "$demoId"
cmp "$WORKDIR/demo.bin" "$WORKDIR/demo-restored.bin"
echo "gc reclaimed the orphan xorb and left everything referenced in place"
