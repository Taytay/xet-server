# Shared setup for the multi_client_*.sh integration tests. Sourced, not
# run: it has no .sh suffix on purpose, because integrationTests.sh runs
# every *.sh under integration-tests/ as a test.
#
# Why these tests exist. A xet client keeps every shard it has uploaded or
# fetched in a local cache (hf_xet and git-xet: $HF_XET_CACHE, default
# $HF_HOME/xet) and dedups a new upload against that cache before it asks
# the server anything. Two uploads from one cache therefore never exercise
# the server's answer to a global-dedup query: the second upload finds
# every chunk locally. That is how "GET /v1/chunks/{prefix}/{hash} returns
# bytes no client can load" survived every single-client test - the only
# client that ever parsed the answer was one on another machine.
#
# So every test here runs each hf invocation as a named client with its
# own HF_HOME, HF_XET_CACHE and token, sharing nothing with the other
# clients but the server, and asserts from the server's DEBUG request log
# and the client's own log that the cross-client path it claims to cover
# really ran (a dedup query answered 200 and parsed as "found", a
# reconstruction served to a client that never uploaded, ...). A test
# that passes without those lines is not testing what its name says.
#
# Each test starts its own xetd (with DEBUG=1, on ports it picks) so it
# owns the data directory it measures and the log it greps.
#
# Requires pipenv with huggingface_hub + hf_xet (`make install`); SKIPs
# (exit 77) otherwise, like hf_cli_roundtrip.sh.

PYTHON_VERSION="${PYTHON_VERSION:-3}"

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

# Per-hf-invocation timeout; see hf_cli_roundtrip.sh for why Windows gets
# more. The files here are 32 MB (see MC_BIG_BYTES) so the bound is wider
# than that test's 4s, but a hang still fails fast.
case "$(uname -s)" in
    MINGW*|MSYS*|CYGWIN*) DEFAULT_CMD_TIMEOUT=40 ;;
    *)                    DEFAULT_CMD_TIMEOUT=15 ;;
esac
CMD_TIMEOUT="${XET_HF_CLI_CMD_TIMEOUT:-$DEFAULT_CMD_TIMEOUT}"
TIMEOUT_CMD=""
if command -v timeout >/dev/null 2>&1; then
    TIMEOUT_CMD="timeout ${CMD_TIMEOUT}s"
elif command -v gtimeout >/dev/null 2>&1; then
    TIMEOUT_CMD="gtimeout ${CMD_TIMEOUT}s"
fi

# hf_xet asks the server about a chunk at most once per 256 chunks
# (min_spacing_between_global_dedup_queries, ~16 MB at the default ~64 KB
# chunk), and only at chunks whose hash makes them eligible. 32 MB is the
# smallest size that reliably produces a query on a fresh client; a test
# that needs a dedup hit uses this and asserts the hit, so a silent miss
# cannot pass.
MC_BIG_BYTES=$((32 * 1024 * 1024))

WORKDIR=${WORKDIR:-$TMPDIR}
# Ambient huggingface_hub settings must not leak into the clients: each
# client's environment is exactly what asClient sets. (HF_HUB_ENABLE_HF_TRANSFER
# only draws a deprecation warning, but a developer's HF_XET_* tuning could
# change how often dedup queries fire.)
unset HF_HUB_ENABLE_HF_TRANSFER HF_XET_HIGH_PERFORMANCE
for v in $(env | grep -oE '^HF_XET_[A-Z_]+' | grep -vE '^HF_XET_LOG_PATH$'); do
    unset "$v"
done
export NO_PROXY="localhost,127.0.0.1,${NO_PROXY:-}"
export no_proxy="localhost,127.0.0.1,${no_proxy:-}"

MC_SERVER_DATA="$WORKDIR/server-data"
MC_SERVER_LOG="$WORKDIR/xetd.log"

# startXetd <cas-port> <hub-port>: start this test's private xetd with
# DEBUG=1 and wait for it. Sets MC_HUB_URL. Killed on exit.
startXetd() {
    local casPort=$1 hubPort=$2
    MC_HUB_URL="http://127.0.0.1:${hubPort}"
    mkdir -p "$MC_SERVER_DATA"
    DEBUG=1 "$XETD" -addr ":${casPort}" -hub-addr ":${hubPort}" -data "$MC_SERVER_DATA" >"$MC_SERVER_LOG" 2>&1 &
    MC_SERVER_PID=$!
    trap 'kill "$MC_SERVER_PID" 2>/dev/null || true; wait "$MC_SERVER_PID" 2>/dev/null || true' EXIT
    local ready=false
    for _ in $(seq 1 50); do
        if curl -s -o /dev/null "http://127.0.0.1:${casPort}/v1/stats"; then
            ready=true
            break
        fi
        sleep 0.1
    done
    if [[ "$ready" != true ]]; then
        echo "xetd did not become ready in time; log follows:"
        cat "$MC_SERVER_LOG"
        exit 1
    fi
}

# asClient <name> <hf args...>: run the hf CLI as the named client. The
# first call for a name creates its empty HF_HOME and HF_XET_CACHE; every
# later call reuses them, so one name is one machine across the test.
asClient() {
    local name=$1; shift
    local home="$WORKDIR/clients/$name"
    mkdir -p "$home/hf" "$home/xet"
    HF_HOME="$home/hf" HF_XET_CACHE="$home/xet" HF_TOKEN="fixture-token-$name" \
    HF_ENDPOINT="$MC_HUB_URL" HF_XET_LOG_PATH="${HF_XET_LOG_PATH:-/dev/null}" \
        pipenv run $TIMEOUT_CMD hf "$@"
}

# clientLogCount <name> <fixed-string>: how many lines of the named
# client's hf_xet log contain the string. hf_xet 1.6 writes JSON lines to
# $HF_XET_CACHE/logs/ (it ignores HF_XET_LOG_PATH); a dedup query that the
# client parsed and used ends with "result":"found".
clientLogCount() {
    local name=$1 needle=$2
    cat "$WORKDIR/clients/$name/xet/logs/"*.log 2>/dev/null | grep -cF -- "$needle" || true
}

# serverLogCount <regex>: how many lines of the server's DEBUG log match.
serverLogCount() {
    grep -cE -- "$1" "$MC_SERVER_LOG" || true
}

# xorbBytes: bytes of xorb data the server has on disk.
xorbBytes() {
    if [[ -d "$MC_SERVER_DATA/xorbs" ]]; then
        du -sb "$MC_SERVER_DATA/xorbs" | cut -f1
    else
        echo 0
    fi
}

# randomFile <path> <bytes>
randomFile() {
    head -c "$2" /dev/urandom >"$1"
}

# runHf <name> <hf args...>: asClient with the timeout diagnosed, so a
# proxied-localhost hang reads as such instead of as a generic failure.
runHf() {
    local name=$1; shift
    local status=0
    asClient "$name" "$@" || status=$?
    if [[ $status -ne 0 ]]; then
        [[ $status -eq 124 ]] && echo "TIMEOUT: 'hf ${1:-}' as $name exceeded ${CMD_TIMEOUT}s (see hf_cli_roundtrip.sh re: proxy hangs)."
        echo "hf $* as $name failed with status $status"
        return 1
    fi
}
