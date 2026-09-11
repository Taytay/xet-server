#!/bin/bash
# End-to-end proof of xet-proxyd's whole reason to exist: a caching proxy
# in front of a Hub, whose cache directory a PLAIN xetd can later read
# directly — the "move away from huggingface.co when it shuts down" path.
#
# 1. Start a plain xetd (its own data dir) as the "real Hub/CAS" for this
#    test — a wire-compatible stand-in for the actual huggingface.co, so
#    this test needs no network access.
# 2. Start xet-proxyd (a SEPARATE data dir), with -upstream-hub-url
#    pointed at that xetd's Hub port.
# 3. Drive the real `hf` CLI (via pipenv — not this repo's own client)
#    through the proxy: `hf upload` then `hf download`, proving the
#    proxy's write-through + fetch-and-cache paths both work against a
#    real, unmodified Hub client.
# 4. Shut the proxy down (a final snapshot is taken on graceful
#    shutdown — see cmd/xet-proxyd/main.go's <-ctx.Done() block).
# 5. Start a FRESH plain xetd pointed at the PROXY's data dir (not the
#    upstream xetd's) — no upstream at all, no network, nothing but what
#    the proxy cached.
# 6. `hf download` the same file again through this second xetd: it must
#    succeed with no extra setup, and the content must be byte-identical
#    — proving the proxy's on-disk state is a real, self-sufficient xetd
#    data directory, not merely usable while the proxy process is alive.
#
# Needs `pipenv` with huggingface_hub + hf_xet installed (see Pipfile). Set
# up once with `make install`; SKIPs (exit 77) if unavailable, same
# convention as hf_cli_roundtrip.sh.
set -euo pipefail

PYTHON_VERSION="${PYTHON_VERSION:-3}"
CMD_TIMEOUT="${XET_PROXYD_IT_CMD_TIMEOUT:-4}"

if ! command -v pipenv >/dev/null 2>&1; then
    echo "pipenv not found. Set up with: make install"
    exit 77
fi
if ! pipenv run hf --version >/dev/null 2>&1; then
    echo "pipenv environment not set up (or hf/huggingface_hub/hf_xet missing)."
    echo "Set up with: make install"
    exit 77
fi
if [[ "$PYTHON_VERSION" != "3" ]]; then
    actualVersion="$(pipenv run python3 -c 'import sys; print(f"{sys.version_info.major}.{sys.version_info.minor}")' 2>/dev/null || echo unknown)"
    case "$actualVersion" in
        "$PYTHON_VERSION"|"$PYTHON_VERSION".*) ;;
        *)
            echo "pipenv's active interpreter is Python $actualVersion, but PYTHON_VERSION=$PYTHON_VERSION was requested."
            echo "Run 'pipenv --python $PYTHON_VERSION && make install' to recreate the environment, or 'make clean' first."
            exit 77
            ;;
    esac
fi

if [[ -z "${XET_PROXYD:-}" || ! -x "${XET_PROXYD:-}" ]]; then
    echo "xet-proxyd binary not found (build with: make build). Set up with: make build"
    exit 77
fi

TIMEOUT_CMD=""
if command -v timeout >/dev/null 2>&1; then
    TIMEOUT_CMD="timeout ${CMD_TIMEOUT}s"
elif command -v gtimeout >/dev/null 2>&1; then
    TIMEOUT_CMD="gtimeout ${CMD_TIMEOUT}s"
fi

runHf() {
    pipenv run $TIMEOUT_CMD hf "$@"
}

# ---- start the "real Hub" (a second, independent xetd) --------------------

UPSTREAM_CAS_PORT="${XET_PROXYD_IT_UPSTREAM_CAS_PORT:-18450}"
UPSTREAM_HUB_PORT="${XET_PROXYD_IT_UPSTREAM_HUB_PORT:-18451}"
UPSTREAM_HUB_URL="http://127.0.0.1:${UPSTREAM_HUB_PORT}"
UPSTREAM_DATA="$WORKDIR/upstream-xetd-data"
UPSTREAM_LOG="$WORKDIR/upstream-xetd.log"
mkdir -p "$UPSTREAM_DATA"

"$XETD" -addr ":${UPSTREAM_CAS_PORT}" -hub-addr ":${UPSTREAM_HUB_PORT}" -data "$UPSTREAM_DATA" >"$UPSTREAM_LOG" 2>&1 &
UPSTREAM_PID=$!

# ---- start xet-proxyd in front of it ---------------------------------------

PROXY_CAS_PORT="${XET_PROXYD_IT_PROXY_CAS_PORT:-18452}"
PROXY_HUB_PORT="${XET_PROXYD_IT_PROXY_HUB_PORT:-18453}"
PROXY_HUB_URL="http://127.0.0.1:${PROXY_HUB_PORT}"
PROXY_DATA="$WORKDIR/proxy-data"
PROXY_LOG="$WORKDIR/xet-proxyd.log"
mkdir -p "$PROXY_DATA"

"$XET_PROXYD" -addr ":${PROXY_CAS_PORT}" -hub-addr ":${PROXY_HUB_PORT}" \
    -data "$PROXY_DATA" -upstream-hub-url "$UPSTREAM_HUB_URL" \
    -snapshot-interval 0 >"$PROXY_LOG" 2>&1 &
PROXY_PID=$!

cleanup() {
    for pid in "${PROXY_PID:-}" "${SECOND_XETD_PID:-}" "${UPSTREAM_PID:-}"; do
        if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
            kill "$pid" 2>/dev/null
            wait "$pid" 2>/dev/null
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
# proxycas has no /v1/stats route of its own (that's cmd/xetd's demo API,
# not part of the wire-compatible CAS protocol) — probe a real proxycas
# route instead, so "ready" means the CAS-facing proxy is actually
# listening and routing, not just that some TCP listener exists. Any HTTP
# response at all (even a 400/404) proves that; waitReady only checks the
# connection succeeded, not the status code.
PROXY_PROBE_HASH="$(printf '0%.0s' {1..64})"
waitReady "http://127.0.0.1:${PROXY_CAS_PORT}/v1/xorbs/default/${PROXY_PROBE_HASH}" "xet-proxyd CAS-facing port" || { cat "$PROXY_LOG" >&2; exit 1; }

echo "upstream xetd ready ($UPSTREAM_HUB_URL), xet-proxyd ready (Hub: $PROXY_HUB_URL, relaying to upstream)"

# ---- drive the real `hf` CLI through the proxy -----------------------------

export HF_HOME="$WORKDIR/hf-home"
export HF_XET_CACHE="$WORKDIR/hf-xet-cache"
export HF_ENDPOINT="$PROXY_HUB_URL"
export HF_TOKEN="local-test-token"
export HF_XET_LOG_PATH=/dev/null
export NO_PROXY="localhost,127.0.0.1,${NO_PROXY:-}"
export no_proxy="localhost,127.0.0.1,${no_proxy:-}"
mkdir -p "$HF_HOME" "$HF_XET_CACHE"

TEST_FILE="$WORKDIR/model.bin"
pipenv run python3 -c \
    "import os; open('$TEST_FILE', 'wb').write(os.urandom(300_000))"

REPO_ID="localtest/xet-proxyd-offline-handoff-it-$$"

echo "hf upload $REPO_ID model.bin, through xet-proxyd (timeout: ${CMD_TIMEOUT}s)"
if ! runHf upload "$REPO_ID" "$TEST_FILE" model.bin; then
    status=$?
    [[ $status -eq 124 ]] && echo "TIMEOUT: 'hf upload' through the proxy exceeded ${CMD_TIMEOUT}s"
    exit 1
fi

echo "hf download $REPO_ID model.bin, through xet-proxyd (timeout: ${CMD_TIMEOUT}s)"
DOWNLOAD_DIR="$WORKDIR/downloaded-via-proxy"
mkdir -p "$DOWNLOAD_DIR"
if ! runHf download "$REPO_ID" model.bin --local-dir "$DOWNLOAD_DIR"; then
    status=$?
    [[ $status -eq 124 ]] && echo "TIMEOUT: 'hf download' through the proxy exceeded ${CMD_TIMEOUT}s"
    exit 1
fi
cmp "$TEST_FILE" "$DOWNLOAD_DIR/model.bin"
echo "upload + download through xet-proxyd byte-identical"

# ---- shut the proxy down (final snapshot on graceful shutdown) ------------

echo "stopping xet-proxyd (final snapshot on graceful shutdown)"
kill "$PROXY_PID"
wait "$PROXY_PID" 2>/dev/null
PROXY_PID=""

if [[ ! -f "$PROXY_DATA/casserver-snapshot.json" ]]; then
    echo "expected $PROXY_DATA/casserver-snapshot.json after graceful shutdown, not found"
    cat "$PROXY_LOG" >&2
    exit 1
fi
if [[ ! -f "$PROXY_DATA/hubserver-snapshot.json" ]]; then
    echo "expected $PROXY_DATA/hubserver-snapshot.json after graceful shutdown, not found"
    cat "$PROXY_LOG" >&2
    exit 1
fi
echo "both snapshot files present after shutdown, using the SAME filenames cmd/xetd itself writes/reads — proving this data dir is a real, drop-in xetd data dir"

# ---- start a FRESH plain xetd pointed at the PROXY's data dir -------------
#
# No -upstream-hub-url, no network: whatever the proxy cached is either
# already there or this second server has nothing to fall back on at all.

SECOND_CAS_PORT="${XET_PROXYD_IT_SECOND_CAS_PORT:-18454}"
SECOND_HUB_PORT="${XET_PROXYD_IT_SECOND_HUB_PORT:-18455}"
SECOND_HUB_URL="http://127.0.0.1:${SECOND_HUB_PORT}"
SECOND_LOG="$WORKDIR/second-xetd.log"

# Deliberately unset HF_TOKEN (still exported into this script's own
# environment from the earlier `hf` CLI run — see below) for THIS
# server process specifically: cmd/xetd.resolveAuthToken falls back to
# $HF_TOKEN for its own required local-access token (unlike
# xet-proxyd, which deliberately excludes that fallback — see its own
# resolveAuthToken's doc comment, since $HF_TOKEN there means something
# else: the caller's upstream credential, forwarded through unchanged).
# Leaving it set here would make this xetd wrongly require auth against
# a token nothing downstream will ever present.
HF_TOKEN= "$XETD" -addr ":${SECOND_CAS_PORT}" -hub-addr ":${SECOND_HUB_PORT}" -data "$PROXY_DATA" >"$SECOND_LOG" 2>&1 &
SECOND_XETD_PID=$!

if ! waitReady "http://127.0.0.1:${SECOND_CAS_PORT}/v1/stats" "second xetd (reading the proxy's data dir)"; then
    cat "$SECOND_LOG" >&2
    exit 1
fi
echo "second xetd is serving the proxy's own data dir directly, with no proxy process running at all"

# Tear down the upstream too, so a bug that silently falls back to it
# instead of truly serving from the handed-off data dir would be caught by
# a connection failure rather than quietly "working" for the wrong reason.
echo "stopping the upstream xetd — the second xetd must serve entirely from the handed-off data dir now"
kill "$UPSTREAM_PID"
wait "$UPSTREAM_PID" 2>/dev/null
UPSTREAM_PID=""

export HF_ENDPOINT="$SECOND_HUB_URL"
DOWNLOAD_DIR_2="$WORKDIR/downloaded-via-second-xetd"
mkdir -p "$DOWNLOAD_DIR_2"

echo "hf download $REPO_ID model.bin, via the second (upstream-less) xetd (timeout: ${CMD_TIMEOUT}s)"
if ! runHf download "$REPO_ID" model.bin --local-dir "$DOWNLOAD_DIR_2"; then
    status=$?
    [[ $status -eq 124 ]] && echo "TIMEOUT: 'hf download' via the handed-off data dir exceeded ${CMD_TIMEOUT}s"
    cat "$SECOND_LOG" >&2
    exit 1
fi

cmp "$TEST_FILE" "$DOWNLOAD_DIR_2/model.bin"
echo "download via the proxy's handed-off data dir (served by a plain xetd, no upstream, no proxy running) byte-identical — this is the whole point of xet-proxyd"
