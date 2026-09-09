#!/bin/bash
# Real `hf` CLI end-to-end round-trip: drives the actual `hf upload` /
# `hf download` shell commands (via huggingface_hub + hf_xet, installed
# through pipenv — NOT this repo's own client) against the shared xetd
# instance's Hub API shim ($HUB_URL), which in turn talks to the CAS server
# ($XETD_URL). This is the strongest possible verification that this server
# is wire- and API-compatible with the real Hugging Face ecosystem: nothing
# here exercises this project's own client code, only the unmodified `hf`
# CLI. Covers both `hf download REPO_ID FILENAME` (single named file) and
# `hf download REPO_ID` (whole repo, no filename) — the latter exercises
# huggingface_hub's snapshot_download, which calls two endpoints
# (.../revision/{revision}, .../tree/{revision}) the single-file path never
# touches at all.
#
# Needs `pipenv` with huggingface_hub + hf_xet installed (see Pipfile). Set
# up once with:
#   make install
#
# If pipenv or its environment isn't set up, this test SKIPs (exit 77)
# rather than failing the whole suite — it's an optional dependency, not a
# required one. $PYTHON_VERSION (passed down from integrationTests.sh, which
# gets it from the Makefile's PYTHON_VERSION) is cross-checked against the
# pipenv-managed interpreter actually in use, so a stale/mismatched venv
# fails clearly instead of silently running under the wrong Python.
#
# Known environment limitation: some corporate/sandboxed networks proxy all
# outbound traffic, including localhost, and hf_xet's Rust HTTP client does
# not consistently honor NO_PROXY/no_proxy for localhost there. When that
# happens, `hf upload` hangs — not a xetd bug, since the same upload/download
# flow passes when driven directly against internal/casserver without going
# through a proxied hf_xet client. Both the runner's outer timeout
# (integrationTests.sh, XET_IT_TEST_TIMEOUT) and this script's own per-command
# timeout below turn that hang into a clear TIMEOUT/failure instead of
# blocking the suite.

set -euo pipefail

PYTHON_VERSION="${PYTHON_VERSION:-3}"
CMD_TIMEOUT="${XET_HF_CLI_CMD_TIMEOUT:-4}"

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

TIMEOUT_CMD=""
if command -v timeout >/dev/null 2>&1; then
    TIMEOUT_CMD="timeout ${CMD_TIMEOUT}s"
elif command -v gtimeout >/dev/null 2>&1; then
    TIMEOUT_CMD="gtimeout ${CMD_TIMEOUT}s"
fi

runHf() {
    pipenv run $TIMEOUT_CMD hf "$@"
}

WORKDIR=${WORKDIR:-$TMPDIR}
HUB_URL=${HUB_URL:-http://localhost:18421}

export HF_HOME="$WORKDIR/hf-home"
export HF_XET_CACHE="$WORKDIR/hf-xet-cache"
export HF_ENDPOINT="$HUB_URL"
export HF_TOKEN="local-test-token"
export HF_XET_LOG_PATH=/dev/null
export NO_PROXY="localhost,127.0.0.1,${NO_PROXY:-}"
export no_proxy="localhost,127.0.0.1,${no_proxy:-}"
mkdir -p "$HF_HOME" "$HF_XET_CACHE"

TEST_FILE="$WORKDIR/model.bin"
pipenv run python3 -c \
    "import os; open('$TEST_FILE', 'wb').write(os.urandom(300_000))"

REPO_ID="localtest/xet-server-hf-cli-it-$$"

echo "hf upload $REPO_ID model.bin (timeout: ${CMD_TIMEOUT}s)"
if ! runHf upload "$REPO_ID" "$TEST_FILE" model.bin; then
    status=$?
    [[ $status -eq 124 ]] && echo "TIMEOUT: 'hf upload' exceeded ${CMD_TIMEOUT}s (see script header re: proxy hangs)."
    exit 1
fi

echo "hf download $REPO_ID model.bin (timeout: ${CMD_TIMEOUT}s)"
DOWNLOAD_DIR="$WORKDIR/downloaded"
mkdir -p "$DOWNLOAD_DIR"
if ! runHf download "$REPO_ID" model.bin --local-dir "$DOWNLOAD_DIR"; then
    status=$?
    [[ $status -eq 124 ]] && echo "TIMEOUT: 'hf download' exceeded ${CMD_TIMEOUT}s (see script header re: proxy hangs)."
    exit 1
fi

cmp "$TEST_FILE" "$DOWNLOAD_DIR/model.bin"
echo "real hf CLI upload + download round-trip byte-identical"

# Whole-repo download (no filename argument) exercises a different code
# path than the single-named-file download above: huggingface_hub's
# snapshot_download first calls GET .../revision/{revision} then GET
# .../tree/{revision} to resolve and enumerate the repo before downloading
# anything, neither of which the single-file path above ever touches.
echo "hf download $REPO_ID (whole repo, timeout: ${CMD_TIMEOUT}s)"
WHOLE_REPO_DIR="$WORKDIR/downloaded-whole-repo"
mkdir -p "$WHOLE_REPO_DIR"
if ! runHf download "$REPO_ID" --local-dir "$WHOLE_REPO_DIR"; then
    status=$?
    [[ $status -eq 124 ]] && echo "TIMEOUT: 'hf download' (whole repo) exceeded ${CMD_TIMEOUT}s (see script header re: proxy hangs)."
    exit 1
fi

cmp "$TEST_FILE" "$WHOLE_REPO_DIR/model.bin"
echo "real hf CLI whole-repo download byte-identical"
