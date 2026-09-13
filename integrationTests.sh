#!/bin/bash
# =============================================================================
# xet-server Integration Test Runner
# =============================================================================
# Starts a single xetd instance (both the CAS server and the Hub API shim)
# against a scratch data directory, then runs every *.sh script in
# integration-tests/ (or a single script if given) as an independent test
# case. Each test script gets:
#
#   $XET             - path to the built xet CLI binary
#   $XETD            - path to the built xetd server binary (for tests that
#                       need to start their OWN xetd instance with different
#                       flags, e.g. -auth-token, rather than using the
#                       shared no-auth instance at $XETD_URL/$HUB_URL)
#   $XET_PROXYD      - path to the built xet-proxyd binary (for tests that
#                       start their own xet-proxyd instance, e.g. the
#                       offline-handoff test - proves a plain xetd can
#                       read a proxy's cache directory directly)
#   $XETD_URL        - base URL of the running xetd CAS server
#   $HUB_URL         - base URL of the running xetd Hub API shim
#   $WORKDIR         - a fresh scratch directory, unique to this test
#   $PYTHON_VERSION  - Python version to request via `pipenv --python`
#                      (defaults to "3"; set by `make integration-test`
#                      from the Makefile's PYTHON_VERSION variable)
#
# A test passes if the script exits 0, and fails otherwise. Scripts should
# use `set -euo pipefail` and assert with plain shell (e.g. `[ "$a" = "$b" ]
# || { echo "mismatch"; exit 1; }`) or `cmp`/`diff` for file comparisons.
#
# Each test runs under a timeout (default 10s, override with
# XET_IT_TEST_TIMEOUT) so a single hanging test - e.g. a network client that
# doesn't respect NO_PROXY for localhost in a proxied environment - fails
# that one test instead of blocking the whole suite indefinitely.
# =============================================================================

set -u

# ---- locate binaries -------------------------------------------------------

XETD_BIN="${1:-bin/xetd}"
XET_BIN="${2:-bin/xet}"
XET_PROXYD_BIN="${3:-bin/xet-proxyd}"
TARGET="${4:-integration-tests}"

if [[ -z "$XETD_BIN" || -z "$XET_BIN" || -z "$XET_PROXYD_BIN" ]]; then
    echo "Usage: $0 <xetd-binary> <xet-binary> <xet-proxyd-binary> [test-dir-or-file]" >&2
    exit 1
fi

if [[ ! -x "$XETD_BIN" ]]; then
    echo "ERROR: xetd binary not found or not executable: $XETD_BIN" >&2
    exit 1
fi
if [[ ! -x "$XET_BIN" ]]; then
    echo "ERROR: xet binary not found or not executable: $XET_BIN" >&2
    exit 1
fi
if [[ ! -x "$XET_PROXYD_BIN" ]]; then
    echo "ERROR: xet-proxyd binary not found or not executable: $XET_PROXYD_BIN" >&2
    exit 1
fi

XETD_BIN="$(cd "$(dirname "$XETD_BIN")" && pwd)/$(basename "$XETD_BIN")"
XET_BIN="$(cd "$(dirname "$XET_BIN")" && pwd)/$(basename "$XET_BIN")"
XET_PROXYD_BIN="$(cd "$(dirname "$XET_PROXYD_BIN")" && pwd)/$(basename "$XET_PROXYD_BIN")"

# ---- colors -----------------------------------------------------------------

if [[ -t 1 ]] && [[ "${TERM:-}" != "dumb" ]] && [[ -z "${NO_COLOR:-}" ]]; then
    RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
else
    RED=""; GREEN=""; YELLOW=""; BLUE=""; NC=""
fi

logInfo()  { echo -e "${GREEN}INFO:${NC} $1"; }
logError() { echo -e "${RED}ERROR:${NC} $1"; }
logTest()  { echo -e "${BLUE}TEST:${NC} $1"; }

# ---- timeout helper ----------------------------------------------------------
#
# Every test must complete in a handful of seconds - this is a local
# integration suite, not an end-to-end network test bed. 10s is generous for
# any of these tests under normal conditions; a test that needs longer than
# that is either hung (see hf_cli_roundtrip.sh's proxy-hang note) or doing
# too much for this suite.

TEST_TIMEOUT="${XET_IT_TEST_TIMEOUT:-10}"

# Prefer GNU coreutils' `timeout` (Linux, or `brew install coreutils` on
# macOS); fall back to `gtimeout`; if neither exists, run without a timeout
# and warn once, since a hang then blocks the whole suite.
TIMEOUT_CMD=""
if command -v timeout >/dev/null 2>&1; then
    TIMEOUT_CMD="timeout ${TEST_TIMEOUT}s"
elif command -v gtimeout >/dev/null 2>&1; then
    TIMEOUT_CMD="gtimeout ${TEST_TIMEOUT}s"
else
    echo -e "${YELLOW}WARN:${NC} no 'timeout' or 'gtimeout' found - tests cannot self-terminate if one hangs."
fi

# ---- auth environment isolation ---------------------------------------------
#
# Every server this suite starts (the shared xetd below, plus the per-test
# xetd/xet-proxyd instances) resolves its own shared secret via
# auth.ResolveToken, which falls back to the environment:
#   xetd       -> $XETD_AUTH_TOKEN, then $HF_TOKEN
#   xet-proxyd -> $XET_PROXYD_AUTH_TOKEN
#
# A developer with $HF_TOKEN exported for the real `hf` CLI (very common)
# would therefore start an xetd that DEMANDS that token on every route,
# while the tests issue unauthenticated requests - so the suite fails with
# `401 unauthenticated: auth: request is not authenticated` purely because
# of ambient environment, not because of any code change. Worse, it passes
# on a machine without $HF_TOKEN and fails on one with it.
#
# Clear all three here so the suite is hermetic and reproducible. Tests that
# specifically exercise auth (auth_gated_access) pass -auth-token explicitly
# to their own server instance, so they are unaffected.
unset HF_TOKEN XETD_AUTH_TOKEN XET_PROXYD_AUTH_TOKEN

# HF_ENDPOINT likewise: if the developer has it pointed at some other local
# server, the `hf` CLI tests would silently talk to THAT instead of the
# instance this suite just started. Each test exports its own value.
unset HF_ENDPOINT

# hf_xet writes a debug log to $HF_XET_LOG_PATH. The tests don't want it, but
# the discard path is platform-specific: the `hf` CLI is a NATIVE Windows
# program here, so handing it the literal string "/dev/null" makes it create
# a stray file at <drive>:\dev\null rather than discarding. Windows' null
# device is "NUL". Exported centrally (tests defer to an already-set value)
# so there's one place to get this right.
if command -v cygpath >/dev/null 2>&1; then
    export HF_XET_LOG_PATH="NUL"
else
    export HF_XET_LOG_PATH=/dev/null
fi

# ---- server lifecycle -------------------------------------------------------

RUN_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/xet-server-it.XXXXXX")"

# On Windows (MSYS2/Git Bash/Cygwin), mktemp hands back a POSIX path such as
# /tmp/xet-server-it.AbC123. Bash understands it, but the tests hand these
# paths to NATIVE Windows programs - the `hf` CLI and its bundled Python, via
# pipenv - which cannot resolve a POSIX path and fail with
# `FileNotFoundError: '/tmp/xet-server-it.AbC123/work/<test>/model.bin'`.
# `cygpath -m` rewrites it to mixed form (D:/tmp/...), which BOTH MSYS bash
# and native Windows programs accept, so every derived path below (WORKDIR,
# SERVER_DATA, HF_HOME, --local-dir, ...) is usable from either side without
# per-test conversion. No-op on Linux/macOS, where cygpath doesn't exist.
if command -v cygpath >/dev/null 2>&1; then
    RUN_ROOT="$(cygpath -m "$RUN_ROOT")"
fi

SERVER_DATA="$RUN_ROOT/server-data"
SERVER_LOG="$RUN_ROOT/xetd.log"
SERVER_PORT="${XETD_PORT:-18420}"
HUB_PORT="${XETD_HUB_PORT:-18421}"
XETD_URL="http://127.0.0.1:${SERVER_PORT}"
HUB_URL="http://127.0.0.1:${HUB_PORT}"

mkdir -p "$SERVER_DATA"

cleanup() {
    if [[ -n "${SERVER_PID:-}" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
        # `|| true` so a non-zero status from a force-terminated server (the
        # normal case on Windows, where MSYS kill uses TerminateProcess
        # rather than delivering a signal) never becomes this script's own
        # exit status via the EXIT trap, masking the real test results.
        kill "$SERVER_PID" 2>/dev/null || true
        wait "$SERVER_PID" 2>/dev/null || true
    fi
    rm -rf "$RUN_ROOT"
}
trap cleanup EXIT INT TERM

logInfo "Starting xetd on $XETD_URL (Hub shim: $HUB_URL, data: $SERVER_DATA)"
"$XETD_BIN" -addr ":${SERVER_PORT}" -hub-addr ":${HUB_PORT}" -data "$SERVER_DATA" >"$SERVER_LOG" 2>&1 &
SERVER_PID=$!

# Wait for the server to accept connections (up to ~5s).
ready=false
for _ in $(seq 1 50); do
    if curl -s -o /dev/null "$XETD_URL/v1/stats"; then
        ready=true
        break
    fi
    sleep 0.1
done
if [[ "$ready" != true ]]; then
    logError "xetd did not become ready in time; log follows:"
    cat "$SERVER_LOG" >&2
    exit 1
fi
logInfo "xetd is ready (pid $SERVER_PID)"

export XET="$XET_BIN"
export XETD="$XETD_BIN"
export XET_PROXYD="$XET_PROXYD_BIN"
export XETD_URL
export HUB_URL
export PYTHON_VERSION="${PYTHON_VERSION:-3}"

# ---- discover tests ----------------------------------------------------------

declare -a testFiles=()
if [[ -f "$TARGET" ]]; then
    testFiles=("$TARGET")
elif [[ -d "$TARGET" ]]; then
    while IFS= read -r -d '' f; do
        testFiles+=("$f")
    done < <(find "$TARGET" -name "*.sh" -print0 2>/dev/null | sort -z)
else
    logError "Argument must be a directory or a .sh file: $TARGET"
    exit 1
fi

if [[ ${#testFiles[@]} -eq 0 ]]; then
    logError "No integration test scripts found in $TARGET"
    exit 1
fi
logInfo "Found ${#testFiles[@]} integration test(s)"

# ---- run tests ----------------------------------------------------------------

passed=0
failed=0
skipped=0
declare -a failedNames=()

for testFile in "${testFiles[@]}"; do
    testName="$(basename "$testFile" .sh)"
    logTest "Running $testName"

    WORKDIR="$RUN_ROOT/work/$testName"
    mkdir -p "$WORKDIR"
    export WORKDIR

    startTime=$(date +%s%N 2>/dev/null || echo 0)
    output=$($TIMEOUT_CMD bash "$testFile" 2>&1)
    exitCode=$?
    endTime=$(date +%s%N 2>/dev/null || echo 0)
    durationMs=$(( (endTime - startTime) / 1000000 ))

    # Exit code 77 is this suite's "SKIP" convention (a test that can't run
    # in the current environment, e.g. a missing optional dependency) -
    # counted separately from pass/fail so an environment gap doesn't look
    # like a regression.
    if [[ $exitCode -eq 77 ]]; then
        echo -e "  Result: ${YELLOW}SKIP${NC}"
        echo "$output" | sed 's/^/  | /'
        ((skipped++))
    elif [[ $exitCode -eq 0 ]]; then
        echo -e "  Result: ${GREEN}PASS${NC}"
        ((passed++))
    else
        if [[ $exitCode -eq 124 ]]; then
            echo -e "  Result: ${RED}TIMEOUT${NC} (exceeded ${TEST_TIMEOUT}s)"
        else
            echo -e "  Result: ${RED}FAIL${NC}"
        fi
        echo "$output" | sed 's/^/  | /'
        failedNames+=("$testName")
        ((failed++))
    fi
    echo "  Duration: ${durationMs}ms"
    echo
done

echo "========================================"
echo "INTEGRATION TEST SUMMARY"
echo "========================================"
echo "Total: $((passed + failed + skipped))"
echo -e "Passed: ${GREEN}${passed}${NC}"
echo -e "Skipped: ${YELLOW}${skipped}${NC}"
echo -e "Failed: ${RED}${failed}${NC}"

if [[ $failed -gt 0 ]]; then
    echo
    echo -e "${RED}FAILED TESTS:${NC}"
    for name in "${failedNames[@]}"; do
        echo "  - $name"
    done
fi

[[ $failed -gt 0 ]] && exit 1 || exit 0
