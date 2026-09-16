#!/bin/bash
# XET_IT_TEST_TIMEOUT: 45
# Downloads by machines that never uploaded.
#
# hf_cli_roundtrip.sh downloads with the same HF_HOME that uploaded, so
# the client's own caches (the uploaded shard, the chunk cache filled by
# the download) sit between it and the server. Here alice uploads two
# files and every download runs from an empty cache: bob fetches one file
# by name, carol fetches the whole repo (snapshot_download, which walks
# the revision and tree routes first), and each byte must come from the
# server's reconstruction of alice's xorbs.
set -euo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/lib/multi_client.bash"
startXetd "${XET_IT_MC_DOWNLOAD_CAS_PORT:-18464}" "${XET_IT_MC_DOWNLOAD_HUB_PORT:-18465}"

REPO_ID="localtest/multi-client-download-$$"
randomFile "$WORKDIR/small.bin" $((700 * 1024))
randomFile "$WORKDIR/large.bin" $((5 * 1024 * 1024))

echo "alice: hf upload $REPO_ID small.bin, large.bin"
runHf alice upload "$REPO_ID" "$WORKDIR/small.bin" small.bin >/dev/null
runHf alice upload "$REPO_ID" "$WORKDIR/large.bin" large.bin >/dev/null
reconAfterUpload=$(serverLogCount 'request completed method=GET path=/v[12]/reconstructions/.*status=200')

for who in bob carol; do
    [[ -e "$WORKDIR/clients/$who" ]] && { echo "$who's cache exists before the first download; the test is not what it claims"; exit 1; }
done

echo "bob (fresh cache): hf download $REPO_ID large.bin"
mkdir -p "$WORKDIR/dl-bob"
runHf bob download "$REPO_ID" large.bin --local-dir "$WORKDIR/dl-bob" >/dev/null
cmp "$WORKDIR/large.bin" "$WORKDIR/dl-bob/large.bin"

echo "carol (fresh cache): hf download $REPO_ID (whole repo)"
mkdir -p "$WORKDIR/dl-carol"
runHf carol download "$REPO_ID" --local-dir "$WORKDIR/dl-carol" >/dev/null
cmp "$WORKDIR/small.bin" "$WORKDIR/dl-carol/small.bin"
cmp "$WORKDIR/large.bin" "$WORKDIR/dl-carol/large.bin"

recon=$(( $(serverLogCount 'request completed method=GET path=/v[12]/reconstructions/.*status=200') - reconAfterUpload ))
[[ $recon -ge 3 ]] || { echo "expected 3 reconstructions served to the downloading machines, saw $recon"; exit 1; }
echo "two fresh machines downloaded alice's files byte-identical ($recon reconstructions served)"
