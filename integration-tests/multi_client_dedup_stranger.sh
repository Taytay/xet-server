#!/bin/bash
# XET_IT_TEST_TIMEOUT: 45
# Global dedup between two machines, with the real hf CLI on both sides.
#
# alice uploads a 32 MB file. bob, on a machine that has never seen it
# (empty shard cache), uploads a copy with two bytes changed. bob's client
# asks the server whether a chunk it is about to upload exists anywhere
# (GET /v1/chunks/{prefix}/{hash}); the answer is the whole shard alice
# uploaded, which bob's client must load and use to skip alice's chunks.
# Only a second machine ever parses that answer - alice's own re-uploads
# dedup against her cache - which is how the server got away with
# rejecting the query's prefix (400) and, once that was fixed, with
# returning bytes no client could load, until git-xet was tried from a
# second machine. See casserver's dedup_prefix_test.go and
# dedup_response_test.go for the unit-level version of each.
#
# Asserts, so the test cannot pass by uploading everything twice:
#   - the server answered at least one dedup query 200 (DEBUG log)
#   - bob's client logged that query as "found" (it parsed the shard)
#   - the xorb store grew by well under 1 MB for bob's 32 MB upload
# then checks reconstruction across the two machines' xorbs: bob's file
# is alice's xorb plus his own small one, and each of alice, bob and a
# third fresh client downloads what the others uploaded, byte-identical.
set -euo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/lib/multi_client.bash"
startXetd "${XET_IT_MC_DEDUP_CAS_PORT:-18460}" "${XET_IT_MC_DEDUP_HUB_PORT:-18461}"

REPO_ID="localtest/multi-client-dedup-$$"

randomFile "$WORKDIR/alice.bin" "$MC_BIG_BYTES"
cp "$WORKDIR/alice.bin" "$WORKDIR/bob.bin"
# Two bytes changed in the middle: one chunk of bob's file is new, the
# rest are alice's.
printf 'XY' | dd of="$WORKDIR/bob.bin" bs=1 seek=$((MC_BIG_BYTES / 2)) conv=notrunc 2>/dev/null
cmp -s "$WORKDIR/alice.bin" "$WORKDIR/bob.bin" && { echo "edit did not change the file"; exit 1; }

echo "alice: hf upload $REPO_ID model.bin (32 MB)"
runHf alice upload "$REPO_ID" "$WORKDIR/alice.bin" model.bin >/dev/null
bytesAfterAlice=$(xorbBytes)
[[ $bytesAfterAlice -ge $MC_BIG_BYTES ]] || { echo "alice's upload stored only $bytesAfterAlice bytes of xorbs"; exit 1; }
foundBefore=$(serverLogCount 'request completed method=GET path=/v1/chunks/.*status=200')

echo "bob (fresh cache): hf upload $REPO_ID model-edited.bin (same bytes, two changed)"
runHf bob upload "$REPO_ID" "$WORKDIR/bob.bin" model-edited.bin >/dev/null
growth=$(( $(xorbBytes) - bytesAfterAlice ))

dedupHits=$(( $(serverLogCount 'request completed method=GET path=/v1/chunks/.*status=200') - foundBefore ))
dedupRejected=$(serverLogCount 'request completed method=GET path=/v1/chunks/.*status=400')
bobFound=$(clientLogCount bob '"result":"found"')
echo "dedup queries answered 200 during bob's upload: $dedupHits (400s overall: $dedupRejected); bob parsed as found: $bobFound; xorb growth: $growth bytes"

[[ $dedupRejected -eq 0 ]] || { echo "server rejected dedup queries with 400: it does not accept the prefix hf_xet sends"; exit 1; }
[[ $dedupHits -ge 1 ]] || { echo "no dedup query was answered 200 during bob's upload: nothing cross-client was exercised"; exit 1; }
[[ $bobFound -ge 1 ]] || { echo "bob's client never logged a dedup query as found: it could not use the server's answer"; exit 1; }
[[ $growth -gt 0 ]] || { echo "bob's edited file added no xorb bytes; the edited chunk had to be stored"; exit 1; }
[[ $growth -lt $((1024 * 1024)) ]] || { echo "xorb store grew by $growth bytes for a two-byte edit; bob re-uploaded instead of deduping"; exit 1; }
echo "bob deduped against alice's upload: $growth bytes stored for a 32 MB file"

# Reconstruction across the two machines' xorbs. bob's file is served
# from alice's xorb plus his own; nobody but the server knows that.
mkdir -p "$WORKDIR/dl-alice" "$WORKDIR/dl-bob" "$WORKDIR/dl-carol"
echo "alice: hf download model-edited.bin (bob's file)"
runHf alice download "$REPO_ID" model-edited.bin --local-dir "$WORKDIR/dl-alice" >/dev/null
cmp "$WORKDIR/bob.bin" "$WORKDIR/dl-alice/model-edited.bin"
echo "bob: hf download model.bin (alice's file)"
runHf bob download "$REPO_ID" model.bin --local-dir "$WORKDIR/dl-bob" >/dev/null
cmp "$WORKDIR/alice.bin" "$WORKDIR/dl-bob/model.bin"
echo "carol (fresh cache): hf download model-edited.bin"
runHf carol download "$REPO_ID" model-edited.bin --local-dir "$WORKDIR/dl-carol" >/dev/null
cmp "$WORKDIR/bob.bin" "$WORKDIR/dl-carol/model-edited.bin"

recon=$(serverLogCount 'request completed method=GET path=/v[12]/reconstructions/.*status=200')
[[ $recon -ge 3 ]] || { echo "expected at least 3 reconstructions served, saw $recon: the downloads did not go through the server"; exit 1; }
echo "three machines read each other's uploads byte-identical ($recon reconstructions served)"
