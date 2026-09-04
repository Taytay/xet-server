#!/bin/bash
# Real `hf` CLI end-to-end round-trip: drives the actual `hf upload` /
# `hf download` shell commands (via huggingface_hub + hf_xet, NOT this
# repo's own client) against the shared xetd instance's Hub API shim
# ($HUB_URL), which in turn talks to the CAS server ($XETD_URL). This is the
# strongest possible verification that this server is wire- and
# API-compatible with the real Hugging Face ecosystem: nothing here
# exercises this project's own client code, only the unmodified `hf` CLI.
#
# Needs a Python venv with huggingface_hub + hf_xet installed. Set up once
# with:
#   python3 -m venv .venv-hf && .venv-hf/bin/pip install huggingface_hub hf_xet
#
# Override the venv location with XET_HF_VENV=/path/to/venv. If the venv
# isn't present, this test SKIPs (exit 77) rather than failing the whole
# suite — it's an optional dependency, not a required one.
#
# Known environment limitation: some corporate/sandboxed networks proxy all
# outbound traffic, including localhost, and hf_xet's Rust HTTP client does
# not consistently honor NO_PROXY/no_proxy for localhost there. When that
# happens, `hf upload` hangs — not a xetd bug, since the same upload/download
# flow passes when driven directly against internal/casserver without going
# through a proxied hf_xet client. The runner's own timeout (see
# integrationTests.sh) turns that hang into a clear TIMEOUT result instead of
# blocking the suite.

set -euo pipefail

VENV_DIR="${XET_HF_VENV:-.venv-hf}"
HF_BIN="$VENV_DIR/bin/hf"
PYTHON_BIN="$VENV_DIR/bin/python3"

if [[ ! -x "$HF_BIN" || ! -x "$PYTHON_BIN" ]]; then
    echo "hf CLI not found at $HF_BIN"
    echo "Set up with: python3 -m venv $VENV_DIR && $VENV_DIR/bin/pip install huggingface_hub hf_xet"
    exit 77
fi

export HF_HOME="$WORKDIR/hf-home"
export HF_XET_CACHE="$WORKDIR/hf-xet-cache"
export HF_ENDPOINT="$HUB_URL"
export HF_TOKEN="local-test-token"
export HF_XET_LOG_PATH=/dev/null
export NO_PROXY="localhost,127.0.0.1,${NO_PROXY:-}"
export no_proxy="localhost,127.0.0.1,${no_proxy:-}"
mkdir -p "$HF_HOME" "$HF_XET_CACHE"

TEST_FILE="$WORKDIR/model.bin"
"$PYTHON_BIN" -c "import os; open('$TEST_FILE', 'wb').write(os.urandom(300_000))"

REPO_ID="localtest/xet-server-hf-cli-it-$$"

echo "hf upload $REPO_ID model.bin"
"$HF_BIN" upload "$REPO_ID" "$TEST_FILE" model.bin

echo "hf download $REPO_ID model.bin"
DOWNLOAD_DIR="$WORKDIR/downloaded"
mkdir -p "$DOWNLOAD_DIR"
"$HF_BIN" download "$REPO_ID" model.bin --local-dir "$DOWNLOAD_DIR"

cmp "$TEST_FILE" "$DOWNLOAD_DIR/model.bin"
echo "real hf CLI upload + download round-trip byte-identical"
