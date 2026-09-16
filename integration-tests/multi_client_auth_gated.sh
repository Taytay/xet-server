#!/bin/bash
# XET_IT_TEST_TIMEOUT: 60
# Two machines with authentication on, real hf CLI on both sides.
#
# The other multi_client tests run against a server with no auth. With
# -auth-token set, every request needs a credential the CAS accepts. The
# Hub shim mints a scoped token for hf_xet's uploads and reconstruction
# queries, but the xorb URLs in a reconstruction response are fetched by
# xet-core with no Authorization header at all - on the real Hub they are
# presigned CDN URLs - so the server has to put the credential into the
# URL itself. Before it did, every `hf download` against an auth-gated
# server failed with 401 on GET /v1/xorbs/... while uploads, and git-lfs
# downloads (whose batch actions carry a header), worked.
#
# alice uploads a 32 MB file; bob (empty cache) dedups an edited copy
# against it, so the dedup path is covered with auth on too; carol
# (empty cache) downloads bob's file.
set -euo pipefail

# "it-fixture-token-not-a-real-secret": a hardcoded integration-test
# fixture, not a real credential of any kind (auth_gated_access.sh uses
# the same string).
MC_HF_TOKEN="it-fixture-token-not-a-real-secret"
source "$(dirname "${BASH_SOURCE[0]}")/lib/multi_client.bash"
startXetd "${XET_IT_MC_AUTH_CAS_PORT:-18468}" "${XET_IT_MC_AUTH_HUB_PORT:-18469}" -auth-token "$MC_HF_TOKEN"

REPO_ID="localtest/multi-client-auth-$$"
randomFile "$WORKDIR/alice.bin" "$MC_BIG_BYTES"
cp "$WORKDIR/alice.bin" "$WORKDIR/bob.bin"
printf 'XY' | dd of="$WORKDIR/bob.bin" bs=1 seek=$((MC_BIG_BYTES / 2)) conv=notrunc 2>/dev/null

echo "alice: hf upload $REPO_ID model.bin (32 MB) with auth on"
runHf alice upload "$REPO_ID" "$WORKDIR/alice.bin" model.bin >/dev/null
bytesAfterAlice=$(xorbBytes)
hitsAfterAlice=$(serverLogCount 'request completed method=GET path=/v1/chunks/.*status=200')

echo "bob (fresh cache): hf upload $REPO_ID model-edited.bin"
runHf bob upload "$REPO_ID" "$WORKDIR/bob.bin" model-edited.bin >/dev/null
growth=$(( $(xorbBytes) - bytesAfterAlice ))
dedupHits=$(( $(serverLogCount 'request completed method=GET path=/v1/chunks/.*status=200') - hitsAfterAlice ))
bobFound=$(clientLogCount bob '"result":"found"')
echo "dedup 200s during bob's upload: $dedupHits; bob parsed as found: $bobFound; xorb growth: $growth bytes"
[[ $dedupHits -ge 1 && $bobFound -ge 1 ]] || { echo "bob did not get and use a dedup answer with auth on"; exit 1; }
[[ $growth -gt 0 && $growth -lt $((1024 * 1024)) ]] || { echo "xorb growth $growth for a two-byte edit"; exit 1; }

echo "carol (fresh cache): hf download $REPO_ID model-edited.bin"
mkdir -p "$WORKDIR/dl-carol"
if ! runHf carol download "$REPO_ID" model-edited.bin --local-dir "$WORKDIR/dl-carol" >"$WORKDIR/carol.out" 2>&1; then
    grep -oE "HTTP status client error \([^)]*\)[^,]*" "$WORKDIR/carol.out" | head -2
    echo "xorb GETs answered 401: $(serverLogCount 'request completed method=GET path=/v1/xorbs/.*status=401')"
    echo "carol's download failed: the xorb URLs in the reconstruction response carry no credential"
    exit 1
fi
cmp "$WORKDIR/bob.bin" "$WORKDIR/dl-carol/model-edited.bin"
xorbGets=$(serverLogCount 'request completed method=GET path=/v1/xorbs/.*status=2[0-9][0-9]')
unauth=$(serverLogCount 'request completed method=GET path=/v1/xorbs/.*status=401')
[[ $xorbGets -ge 1 && $unauth -eq 0 ]] || { echo "xorb GETs: $xorbGets ok, $unauth unauthorized"; exit 1; }
echo "third machine downloaded byte-identical through signed xorb urls ($xorbGets xorb fetches, none rejected)"

# A signed url is not a skeleton key: stripped of its token it is refused.
signedURL=$(grep -oE 'path=/v1/xorbs/[^ ]+' "$MC_SERVER_LOG" | head -1 | sed 's/path=//')
status=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${XET_IT_MC_AUTH_CAS_PORT:-18468}${signedURL}")
[[ $status = 401 ]] || { echo "GET $signedURL without a token: $status, want 401"; exit 1; }
echo "xorb url without its token is refused (401)"
