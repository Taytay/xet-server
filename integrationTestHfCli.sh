#!/bin/bash
# =============================================================================
# Real `hf` CLI end-to-end integration test
# =============================================================================
# Drives the actual `hf upload` / `hf download` shell commands (via
# huggingface_hub + hf_xet, NOT a hand-rolled client) against a local xetd
# instance, through the Hub API shim (internal/hubserver) via HF_ENDPOINT.
# This is the strongest possible verification that this server is wire- and
# API-compatible with the real Hugging Face ecosystem: nothing here is
# testing this server's own client code, only the unmodified `hf` CLI.
#
# Requires:
#   - Python 3 with huggingface_hub + hf_xet installed (e.g. in a venv:
#       python3 -m venv .venv-hf && .venv-hf/bin/pip install huggingface_hub hf_xet)
#   - Network egress to localhost NOT intercepted by a proxy that blocks it
#     (some corporate/sandboxed environments proxy all outbound traffic,
#     including localhost, and this test will hang or fail with a 403
#     ProxyError in that case — this is an environment limitation, not a
#     bug in xetd; run this script outside such an environment, or with
#     NO_PROXY/no_proxy set to bypass the proxy for localhost/127.0.0.1)
#
# Usage:
#   ./integrationTestHfCli.sh <path-to-hf-venv> [xetd-binary] [xet-binary]
#
# Example:
#   make build
#   python3 -m venv .venv-hf && .venv-hf/bin/pip install huggingface_hub hf_xet
#   ./integrationTestHfCli.sh .venv-hf
# =============================================================================

set -euo pipefail

VENV_DIR="${1:?Usage: $0 <path-to-hf-venv> [xetd-binary] [xet-binary]}"
XETD_BIN="${2:-./bin/xetd}"

HF_BIN="$VENV_DIR/bin/hf"
PYTHON_BIN="$VENV_DIR/bin/python3"

if [[ ! -x "$HF_BIN" ]]; then
    echo "ERROR: hf CLI not found at $HF_BIN — install huggingface_hub and hf_xet into $VENV_DIR first" >&2
    exit 1
fi
if [[ ! -x "$XETD_BIN" ]]; then
    echo "ERROR: xetd binary not found at $XETD_BIN — run 'make build' first" >&2
    exit 1
fi

RUN_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/xet-server-hf-cli-it.XXXXXX")"
CAS_PORT="${XETD_CAS_PORT:-18440}"
HUB_PORT="${XETD_HUB_PORT:-18441}"

cleanup() {
    if [[ -n "${SERVER_PID:-}" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
        kill "$SERVER_PID" 2>/dev/null
        wait "$SERVER_PID" 2>/dev/null
    fi
    rm -rf "$RUN_ROOT"
}
trap cleanup EXIT INT TERM

echo "Starting xetd (CAS :$CAS_PORT, Hub shim :$HUB_PORT)..."
"$XETD_BIN" -addr ":$CAS_PORT" -hub-addr ":$HUB_PORT" -data "$RUN_ROOT/data" \
    > "$RUN_ROOT/xetd.log" 2>&1 &
SERVER_PID=$!

ready=false
for _ in $(seq 1 50); do
    if curl -s -o /dev/null -X POST -d '{}' "http://localhost:$CAS_PORT/v1/telemetry"; then
        ready=true
        break
    fi
    sleep 0.1
done
if [[ "$ready" != true ]]; then
    echo "ERROR: xetd did not become ready in time; log follows:" >&2
    cat "$RUN_ROOT/xetd.log" >&2
    exit 1
fi
echo "xetd is ready"

export HF_HOME="$RUN_ROOT/hf-home"
export HF_XET_CACHE="$RUN_ROOT/hf-xet-cache"
export HF_ENDPOINT="http://localhost:$HUB_PORT"
export HF_TOKEN="local-test-token"
export HF_XET_LOG_PATH=/dev/null
mkdir -p "$HF_HOME" "$HF_XET_CACHE"

echo "Generating test file..."
TEST_FILE="$RUN_ROOT/model.bin"
"$PYTHON_BIN" -c "import os; open('$TEST_FILE', 'wb').write(os.urandom(300_000))"

REPO_ID="localtest/xet-server-hf-cli-it-$$"

echo "Running: hf upload $REPO_ID model.bin"
"$HF_BIN" upload "$REPO_ID" "$TEST_FILE" model.bin

echo "Running: hf download $REPO_ID model.bin"
DOWNLOAD_DIR="$RUN_ROOT/downloaded"
mkdir -p "$DOWNLOAD_DIR"
"$HF_BIN" download "$REPO_ID" model.bin --local-dir "$DOWNLOAD_DIR"

echo "Comparing original and downloaded files..."
cmp "$TEST_FILE" "$DOWNLOAD_DIR/model.bin"

echo "SUCCESS: real hf CLI upload + download round-trip byte-identical"
