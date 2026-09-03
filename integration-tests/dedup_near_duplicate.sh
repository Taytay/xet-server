#!/bin/bash
# Pushing a near-duplicate file (same content plus an appended tail) must
# dedup most chunks against the first upload, and both must still restore
# correctly.
set -euo pipefail

dd if=/dev/urandom of="$WORKDIR/v1.bin" bs=1024 count=800 2>/dev/null
cat "$WORKDIR/v1.bin" > "$WORKDIR/v2.bin"
dd if=/dev/urandom bs=1024 count=200 2>/dev/null >> "$WORKDIR/v2.bin"

out1=$("$XET" push -server "$XETD_URL" "$WORKDIR/v1.bin")
echo "--- v1 ---"
echo "$out1"
id1=$(echo "$out1" | awk '/^file_id:/ {print $2}')

out2=$("$XET" push -server "$XETD_URL" "$WORKDIR/v2.bin")
echo "--- v2 ---"
echo "$out2"
id2=$(echo "$out2" | awk '/^file_id:/ {print $2}')

chunksTotal=$(echo "$out2" | awk -F'[ ,]+' '/^chunks:/ {print $2}')
chunksNew=$(echo "$out2" | awk -F'[ ,]+' '/^chunks:/ {print $4}')

if [[ -z "$chunksNew" || -z "$chunksTotal" ]]; then
    echo "could not parse chunk counts from push output"
    exit 1
fi

if [[ "$chunksNew" -ge "$chunksTotal" ]]; then
    echo "expected v2 to dedup against v1's chunks, but chunksNew ($chunksNew) >= chunksTotal ($chunksTotal)"
    exit 1
fi
echo "dedup confirmed: $chunksNew new out of $chunksTotal total chunks"

"$XET" pull -server "$XETD_URL" -out "$WORKDIR/v1.restored.bin" "$id1"
"$XET" pull -server "$XETD_URL" -out "$WORKDIR/v2.restored.bin" "$id2"

cmp "$WORKDIR/v1.bin" "$WORKDIR/v1.restored.bin"
cmp "$WORKDIR/v2.bin" "$WORKDIR/v2.restored.bin"
echo "both versions restored correctly"
