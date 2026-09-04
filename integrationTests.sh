#!/bin/bash
# =============================================================================
# xet-server Integration Test Runner
# =============================================================================
# Starts a single xetd instance (both the CAS server and the Hub API shim)
# against a scratch data directory, then runs every *.sh script in
# integration-tests/ (or a single script if given) as an independent test
# case. Each test script gets:
#
#   $XET        - path to the built xet CLI binary
#   $XETD_URL   - base URL of the running xetd CAS server
#   $HUB_URL    - base URL of the running xetd Hub API shim
#   $WORKDIR    - a fresh scratch directory, unique to this test
#
# A test passes if the script exits 0, and fails otherwise. Scripts should
# use `set -euo pipefail` and assert with plain shell (e.g. `[ "$a" = "$b" ]
# || { echo "mismatch"; exit 1; }`) or `cmp`/`diff` for file comparisons.
#
# Each test runs under a timeout (default 60s, override with
# XET_IT_TEST_TIMEOUT) so a single hanging test — e.g. a network client that
# doesn't respect NO_PROXY for localhost in a proxied environment — fails
# that one test instead of blocking the whole suite indefinitely.
# =============================================================================

set -u

# ---- locate binaries -------------------------------------------------------

XETD_BIN="$1"
XET_BIN="$2"
TARGET="${3:-integration-tests}"

if [[ -z "$XETD_BIN" || -z "$XET_BIN" ]]; then
    echo "Usage: $0 <xetd-binary> <xet-binary> [test-dir-or-file]" >&2
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

XETD_BIN="$(cd "$(dirname "$XETD_BIN")" && pwd)/$(basename "$XETD_BIN")"
XET_BIN="$(cd "$(dirname "$XET_BIN")" && pwd)/$(basename "$XET_BIN")"

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

TEST_TIMEOUT="${XET_IT_TEST_TIMEOUT:-60}"

# Prefer GNU coreutils' `timeout` (Linux, or `brew install coreutils` on
# macOS); fall back to `gtimeout`; if neither exists, run without a timeout
# and warn once, since a hang then blocks the whole suite.
TIMEOUT_CMD=""
if command -v timeout >/dev/null 2>&1; then
    TIMEOUT_CMD="timeout ${TEST_TIMEOUT}s"
elif command -v gtimeout >/dev/null 2>&1; then
    TIMEOUT_CMD="gtimeout ${TEST_TIMEOUT}s"
else
    echo -e "${YELLOW}WARN:${NC} no 'timeout' or 'gtimeout' found — tests cannot self-terminate if one hangs."
fi

# ---- server lifecycle -------------------------------------------------------

RUN_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/xet-server-it.XXXXXX")"
SERVER_DATA="$RUN_ROOT/server-data"
SERVER_LOG="$RUN_ROOT/xetd.log"
SERVER_PORT="${XETD_PORT:-18420}"
HUB_PORT="${XETD_HUB_PORT:-18421}"
XETD_URL="http://127.0.0.1:${SERVER_PORT}"
HUB_URL="http://127.0.0.1:${HUB_PORT}"

mkdir -p "$SERVER_DATA"

cleanup() {
    if [[ -n "${SERVER_PID:-}" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
        kill "$SERVER_PID" 2>/dev/null
        wait "$SERVER_PID" 2>/dev/null
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
    if curl -s -o /dev/null "$XETD_URL/stats"; then
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
export XETD_URL
export HUB_URL

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
    # in the current environment, e.g. a missing optional dependency) —
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
