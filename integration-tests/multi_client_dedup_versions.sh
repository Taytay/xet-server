#!/bin/bash
# XET_IT_TEST_TIMEOUT: 60
# Global dedup between two machines when the file has an edited history.
#
# alice uploads a 32 MB file, then re-uploads it with its first byte and
# a middle byte changed. Her second upload dedups against her own cache,
# so its shard lists only the new xorb: one or two chunks, including the
# file's new first chunk. bob, on an empty cache, uploads a copy with one
# more byte changed. xet-core asks the server about the first chunk of a
# file, then at most once per 256 chunks, so the answer to that first
# query decides how much of the file bob can skip. Answering with the
# shard that introduced the chunk gave him two chunks and he re-uploaded
# the other 32 MB; the answer must cover every xorb of the file.
#
# This is multi_client_dedup_stranger.sh with one extra version in the
# history, which is what every real repository has.
set -euo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/lib/multi_client.bash"
startXetd "${XET_IT_MC_VERSIONS_CAS_PORT:-18466}" "${XET_IT_MC_VERSIONS_HUB_PORT:-18467}"

REPO_ID="localtest/multi-client-versions-$$"

randomFile "$WORKDIR/v1.bin" "$MC_BIG_BYTES"
cp "$WORKDIR/v1.bin" "$WORKDIR/v2.bin"
printf 'A' | dd of="$WORKDIR/v2.bin" bs=1 seek=0 conv=notrunc 2>/dev/null
printf 'B' | dd of="$WORKDIR/v2.bin" bs=1 seek=$((MC_BIG_BYTES / 2)) conv=notrunc 2>/dev/null
cp "$WORKDIR/v2.bin" "$WORKDIR/v3.bin"
printf 'C' | dd of="$WORKDIR/v3.bin" bs=1 seek=$((MC_BIG_BYTES / 4)) conv=notrunc 2>/dev/null
cmp -s "$WORKDIR/v1.bin" "$WORKDIR/v2.bin" && { echo "v2 edit did not change the file"; exit 1; }
cmp -s "$WORKDIR/v2.bin" "$WORKDIR/v3.bin" && { echo "v3 edit did not change the file"; exit 1; }

echo "alice: hf upload $REPO_ID model.bin (32 MB), then the edited version"
runHf alice upload "$REPO_ID" "$WORKDIR/v1.bin" model.bin >/dev/null
runHf alice upload "$REPO_ID" "$WORKDIR/v2.bin" model.bin >/dev/null
bytesAfterAlice=$(xorbBytes)
hitsAfterAlice=$(serverLogCount 'request completed method=GET path=/v1/chunks/.*status=200')

echo "bob (fresh cache): hf upload $REPO_ID model-bob.bin (the edited version, one more byte changed)"
runHf bob upload "$REPO_ID" "$WORKDIR/v3.bin" model-bob.bin >/dev/null
growth=$(( $(xorbBytes) - bytesAfterAlice ))
dedupHits=$(( $(serverLogCount 'request completed method=GET path=/v1/chunks/.*status=200') - hitsAfterAlice ))
bobFound=$(clientLogCount bob '"result":"found"')
echo "dedup queries answered 200 during bob's upload: $dedupHits; bob parsed as found: $bobFound; xorb growth: $growth bytes"

[[ $dedupHits -ge 1 ]] || { echo "no dedup query was answered 200 during bob's upload"; exit 1; }
[[ $bobFound -ge 1 ]] || { echo "bob's client never logged a dedup answer as found"; exit 1; }
[[ $growth -gt 0 ]] || { echo "bob's edit added no xorb bytes; the edited chunk had to be stored"; exit 1; }
[[ $growth -lt $((1024 * 1024)) ]] || { echo "xorb store grew by $growth bytes: the dedup answer for the file's first chunk did not cover the file, so bob re-uploaded it"; exit 1; }
echo "bob deduped against a file with an edited history: $growth bytes stored for a 32 MB file"

echo "carol (fresh cache): hf download model-bob.bin"
mkdir -p "$WORKDIR/dl-carol"
runHf carol download "$REPO_ID" model-bob.bin --local-dir "$WORKDIR/dl-carol" >/dev/null
cmp "$WORKDIR/v3.bin" "$WORKDIR/dl-carol/model-bob.bin"
echo "third machine downloaded bob's file byte-identical"
