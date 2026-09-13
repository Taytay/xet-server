#!/bin/bash
# Proxy write-path relay regression test (no pipenv/hf needed): the
# xet-proxyd must forward the Hub write requests - create-repo, preupload,
# commit - to its upstream Hub and relay the upstream's response. A
# decode-and-reencode would silently drop the preupload per-file `sample`
# field and the commit ndjson LFS-pointer fields, both of which the real
# Hub requires ("expected string, received undefined"); this test drives
# those exact bodies through the proxy against a local stand-in upstream
# xetd and asserts the relayed responses come back 200.
#
# Starts its own upstream xetd (the stand-in "real Hub") and its own
# xet-proxyd in front of it, so it is independent of the shared server.
set -euo pipefail

if [[ -z "${XET_PROXYD:-}" || ! -x "${XET_PROXYD:-}" ]]; then
    echo "xet-proxyd binary not found (build with: make build)."
    exit 77
fi

UPSTREAM_CAS_PORT="${XET_PROXYD_IT_UPSTREAM_CAS_PORT:-18460}"
UPSTREAM_HUB_PORT="${XET_PROXYD_IT_UPSTREAM_HUB_PORT:-18461}"
PROXY_CAS_PORT="${XET_PROXYD_IT_PROXY_CAS_PORT:-18462}"
PROXY_HUB_PORT="${XET_PROXYD_IT_PROXY_HUB_PORT:-18463}"
UPSTREAM_HUB_URL="http://127.0.0.1:${UPSTREAM_HUB_PORT}"
PROXY_HUB_URL="http://127.0.0.1:${PROXY_HUB_PORT}"
UPSTREAM_DATA="$WORKDIR/upstream-data"
PROXY_DATA="$WORKDIR/proxy-data"
UPSTREAM_LOG="$WORKDIR/upstream.log"
PROXY_LOG="$WORKDIR/proxy.log"
mkdir -p "$UPSTREAM_DATA" "$PROXY_DATA"

"$XETD" -addr ":${UPSTREAM_CAS_PORT}" -hub-addr ":${UPSTREAM_HUB_PORT}" -data "$UPSTREAM_DATA" >"$UPSTREAM_LOG" 2>&1 &
UPSTREAM_PID=$!
"$XET_PROXYD" -addr ":${PROXY_CAS_PORT}" -hub-addr ":${PROXY_HUB_PORT}" \
    -data "$PROXY_DATA" -upstream-hub-url "$UPSTREAM_HUB_URL" >"$PROXY_LOG" 2>&1 &
PROXY_PID=$!

cleanup() {
    for pid in "${UPSTREAM_PID:-}" "${PROXY_PID:-}"; do
        if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
            kill "$pid" 2>/dev/null || true
            wait "$pid" 2>/dev/null || true
        fi
    done
}
trap cleanup EXIT INT TERM

waitReady() {
    local url="$1" name="$2"
    for _ in $(seq 1 50); do
        code=$(curl -s -o /dev/null -w "%{http_code}" "$url")
        [[ "$code" != "000" ]] && return 0
        sleep 0.1
    done
    echo "$name did not become ready in time"
    return 1
}
waitReady "http://127.0.0.1:${UPSTREAM_CAS_PORT}/v1/stats" "upstream xetd" || { cat "$UPSTREAM_LOG" >&2; exit 1; }
waitReady "$PROXY_HUB_URL/api/models/x/y/revision/main" "xet-proxyd Hub port" || { cat "$PROXY_LOG" >&2; exit 1; }

REPO_NAME="proxy-write-relay-it-$$"
REPO_ID="localtest/$REPO_NAME"

# ---- create-repo relayed to upstream --------------------------------------

resp=$(curl -s -w '\n%{http_code}' -X POST -H "Content-Type: application/json" \
    -d "{\"name\":\"$REPO_NAME\",\"organization\":\"localtest\",\"type\":\"model\"}" \
    "$PROXY_HUB_URL/api/repos/create")
code=$(echo "$resp" | tail -1)
body=$(echo "$resp" | sed '$d')
if [[ "$code" != "200" ]]; then
    echo "create-repo via proxy: status=$code body=$body"
    exit 1
fi

# ---- preupload with `sample` field relayed verbatim ------------------------

resp=$(curl -s -w '\n%{http_code}' -X POST -H "Content-Type: application/json" \
    -d '{"files":[{"path":"model.bin","size":1024,"sample":"c2FtcGxlLXNhbXBsZQ=="}]}' \
    "$PROXY_HUB_URL/api/models/$REPO_ID/preupload/main")
code=$(echo "$resp" | tail -1)
if [[ "$code" != "200" ]]; then
    echo "preupload via proxy: status=$code body=$(echo "$resp" | sed '$d')"
    exit 1
fi

# ---- commit ndjson relayed verbatim ----------------------------------------

resp=$(curl -s -w '\n%{http_code}' -X POST -H "Content-Type: application/x-ndjson" \
    -d '{"key":"lfsFile","value":{"path":"model.bin","oid":"sha256:0000000000000000000000000000000000000000000000000000000000000000","size":1024}}' \
    "$PROXY_HUB_URL/api/models/$REPO_ID/commit/main")
code=$(echo "$resp" | tail -1)
body=$(echo "$resp" | sed '$d')
if [[ "$code" != "200" ]]; then
    echo "commit via proxy: status=$code body=$body"
    exit 1
fi

echo "proxy write-path relay OK (create-repo, preupload with sample, commit ndjson)"