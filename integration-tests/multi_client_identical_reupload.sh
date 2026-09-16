#!/bin/bash
# XET_IT_TEST_TIMEOUT: 45
# The same bytes uploaded from two machines are stored once.
#
# alice uploads a 32 MB file; bob, from an empty cache, uploads a
# byte-identical file under another name in the same repo. bob's client
# has no idea alice exists: it chunks the file, asks the server about a
# chunk, gets alice's shard back, and must conclude that it has nothing
# to upload. The server's own xorb-hash dedup would also catch a re-POST
# of the identical xorb, so the test asserts the dedup answer was used
# (bob's client logged "found", no xorb was POSTed by bob) and not just
# that the store did not grow. A third fresh client then downloads the
# whole repo - huggingface_hub's snapshot_download path - and gets both
# files, byte-identical, though bob's was never stored as such.
set -euo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/lib/multi_client.bash"
startXetd "${XET_IT_MC_REUPLOAD_CAS_PORT:-18462}" "${XET_IT_MC_REUPLOAD_HUB_PORT:-18463}"

REPO_ID="localtest/multi-client-reupload-$$"
randomFile "$WORKDIR/data.bin" "$MC_BIG_BYTES"

echo "alice: hf upload $REPO_ID original.bin (32 MB)"
runHf alice upload "$REPO_ID" "$WORKDIR/data.bin" original.bin >/dev/null
bytesAfterAlice=$(xorbBytes)
xorbPostsAfterAlice=$(serverLogCount 'request completed method=POST path=/v1/xorbs/')
hitsAfterAlice=$(serverLogCount 'request completed method=GET path=/v1/chunks/.*status=200')

echo "bob (fresh cache): hf upload $REPO_ID copy.bin (identical bytes)"
runHf bob upload "$REPO_ID" "$WORKDIR/data.bin" copy.bin >/dev/null

growth=$(( $(xorbBytes) - bytesAfterAlice ))
bobXorbPosts=$(( $(serverLogCount 'request completed method=POST path=/v1/xorbs/') - xorbPostsAfterAlice ))
dedupHits=$(( $(serverLogCount 'request completed method=GET path=/v1/chunks/.*status=200') - hitsAfterAlice ))
bobFound=$(clientLogCount bob '"result":"found"')
echo "xorb growth: $growth bytes; xorbs POSTed by bob: $bobXorbPosts; dedup 200s: $dedupHits; bob parsed as found: $bobFound"

[[ $dedupHits -ge 1 && $bobFound -ge 1 ]] || { echo "bob's client did not get and use a dedup answer; it would have re-uploaded everything"; exit 1; }
[[ $bobXorbPosts -eq 0 ]] || { echo "bob POSTed $bobXorbPosts xorb(s) for bytes the server already had"; exit 1; }
[[ $growth -eq 0 ]] || { echo "xorb store grew by $growth bytes for an identical re-upload"; exit 1; }
echo "identical re-upload from a second machine stored nothing new"

echo "carol (fresh cache): hf download $REPO_ID (whole repo)"
mkdir -p "$WORKDIR/dl-carol"
runHf carol download "$REPO_ID" --local-dir "$WORKDIR/dl-carol" >/dev/null
cmp "$WORKDIR/data.bin" "$WORKDIR/dl-carol/original.bin"
cmp "$WORKDIR/data.bin" "$WORKDIR/dl-carol/copy.bin"
recon=$(serverLogCount 'request completed method=GET path=/v[12]/reconstructions/.*status=200')
[[ $recon -ge 2 ]] || { echo "expected reconstructions for both files, saw $recon"; exit 1; }
echo "third machine downloaded both files byte-identical"
