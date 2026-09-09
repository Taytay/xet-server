#!/bin/bash
# The shared xetd instance used by every other integration test has no auth
# enforced (that's the pre-v0.8.0 baseline these tests must keep passing).
# This test starts its OWN xetd with -auth-token set, to prove the actual
# auth feature works end-to-end through the real xet/xetd binaries: pushing
# without a token fails, pushing with the wrong token fails, pushing with
# the right token (whether passed as -auth-token or via $XET_AUTH_TOKEN)
# succeeds, and the round-tripped file is byte-identical.
set -euo pipefail

# "it-fixture-token-not-a-real-secret": a hardcoded integration-test
# fixture, not a real credential of any kind.
AUTH_TOKEN="it-fixture-token-not-a-real-secret"
PORT="${XET_IT_AUTH_TEST_PORT:-18430}"
SERVER_URL="http://127.0.0.1:${PORT}"
DATA_DIR="$WORKDIR/server-data"
SERVER_LOG="$WORKDIR/xetd-auth.log"

mkdir -p "$DATA_DIR"

# XETD_AUTH_TOKEN is deliberately set (not -auth-token) for this server, to
# exercise the environment-variable path end-to-end, not just the flag.
XETD_AUTH_TOKEN="$AUTH_TOKEN" "$XETD" -addr ":${PORT}" -data "$DATA_DIR" >"$SERVER_LOG" 2>&1 &
SERVER_PID=$!
cleanup() {
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
}
trap cleanup EXIT

ready=false
for _ in $(seq 1 50); do
    if curl -s -o /dev/null "$SERVER_URL/v1/stats"; then
        ready=true
        break
    fi
    sleep 0.1
done
if [[ "$ready" != true ]]; then
    echo "auth-enabled xetd did not become ready in time; log follows:"
    cat "$SERVER_LOG"
    exit 1
fi

dd if=/dev/urandom of="$WORKDIR/secret.bin" bs=1024 count=200 2>/dev/null

# No token at all: push must fail. $XET_AUTH_TOKEN/$HF_TOKEN are explicitly
# cleared for every "must fail without a token" step below, so this test is
# hermetic even if the runner's own shell happens to export either (e.g. a
# developer who already has HF_TOKEN set for everyday `hf` CLI use).
if XET_AUTH_TOKEN= HF_TOKEN= "$XET" push -server "$SERVER_URL" "$WORKDIR/secret.bin" >"$WORKDIR/no-token-push.log" 2>&1; then
    echo "expected push with no token to fail against an auth-enabled server, but it succeeded"
    cat "$WORKDIR/no-token-push.log"
    exit 1
fi
echo "push with no token correctly failed"

# Wrong token: push must fail.
if XET_AUTH_TOKEN= HF_TOKEN= "$XET" push -server "$SERVER_URL" -auth-token "wrong-fixture-token" "$WORKDIR/secret.bin" >"$WORKDIR/wrong-token-push.log" 2>&1; then
    echo "expected push with the wrong token to fail, but it succeeded"
    cat "$WORKDIR/wrong-token-push.log"
    exit 1
fi
echo "push with the wrong token correctly failed"

# Correct token via -auth-token flag: push must succeed, and the pulled
# file must be byte-identical.
pushOutput=$(XET_AUTH_TOKEN= HF_TOKEN= "$XET" push -server "$SERVER_URL" -auth-token "$AUTH_TOKEN" "$WORKDIR/secret.bin")
fileId=$(echo "$pushOutput" | awk '/^file_id:/ {print $2}')
if [[ -z "$fileId" ]]; then
    echo "could not parse file_id from push output"
    echo "$pushOutput"
    exit 1
fi

# Pulling with no token must fail even though the file exists.
if XET_AUTH_TOKEN= HF_TOKEN= "$XET" pull -server "$SERVER_URL" -out "$WORKDIR/should-not-exist.bin" "$fileId" >"$WORKDIR/no-token-pull.log" 2>&1; then
    echo "expected pull with no token to fail against an auth-enabled server, but it succeeded"
    cat "$WORKDIR/no-token-pull.log"
    exit 1
fi
echo "pull with no token correctly failed"

# Correct token via $XET_AUTH_TOKEN (not the flag) must succeed — proves the
# client-side environment-variable fallback works end-to-end, not just the
# flag path already exercised by the push above.
XET_AUTH_TOKEN="$AUTH_TOKEN" HF_TOKEN= "$XET" pull -server "$SERVER_URL" -out "$WORKDIR/restored.bin" "$fileId"

cmp "$WORKDIR/secret.bin" "$WORKDIR/restored.bin"
echo "auth-gated round trip OK for file_id=$fileId (server via \$XETD_AUTH_TOKEN, client via -auth-token then \$XET_AUTH_TOKEN)"
