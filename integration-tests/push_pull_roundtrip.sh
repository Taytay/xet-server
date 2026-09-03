#!/bin/bash
# Push a file and pull it back; the two must be byte-for-byte identical.
set -euo pipefail

dd if=/dev/urandom of="$WORKDIR/original.bin" bs=1024 count=500 2>/dev/null

pushOutput=$("$XET" push -server "$XETD_URL" "$WORKDIR/original.bin")
echo "$pushOutput"

fileId=$(echo "$pushOutput" | awk '/^file_id:/ {print $2}')
if [[ -z "$fileId" ]]; then
    echo "could not parse file_id from push output"
    exit 1
fi

"$XET" pull -server "$XETD_URL" -out "$WORKDIR/restored.bin" "$fileId"

cmp "$WORKDIR/original.bin" "$WORKDIR/restored.bin"
echo "round trip OK for file_id=$fileId"
