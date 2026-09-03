#!/bin/bash
# Uploading byte-identical content twice must return the same file_id and
# dedup 100% of chunks on the second push.
set -euo pipefail

dd if=/dev/urandom of="$WORKDIR/data.bin" bs=1024 count=300 2>/dev/null

out1=$("$XET" push -server "$XETD_URL" "$WORKDIR/data.bin")
id1=$(echo "$out1" | awk '/^file_id:/ {print $2}')

out2=$("$XET" push -server "$XETD_URL" "$WORKDIR/data.bin")
id2=$(echo "$out2" | awk '/^file_id:/ {print $2}')

if [[ "$id1" != "$id2" ]]; then
    echo "expected identical content to produce the same file_id, got $id1 and $id2"
    exit 1
fi

chunksNew=$(echo "$out2" | awk -F'[ ,]+' '/^chunks:/ {print $4}')
if [[ "$chunksNew" != "0" ]]; then
    echo "expected re-upload of identical content to have 0 new chunks, got $chunksNew"
    exit 1
fi

echo "identical re-upload correctly reused file_id=$id1 with 0 new chunks"
