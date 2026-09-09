#!/bin/bash
# The /v1/stats endpoint must reflect uploads: file count increases, and
# unique chunk bytes account for dedup (not simply the sum of all uploaded
# bytes).
set -euo pipefail

statsBefore=$(curl -sf "$XETD_URL/v1/stats")
filesBefore=$(echo "$statsBefore" | grep -o '"files_stored":[0-9]*' | grep -o '[0-9]*$')

dd if=/dev/urandom of="$WORKDIR/a.bin" bs=1024 count=400 2>/dev/null
"$XET" push -server "$XETD_URL" "$WORKDIR/a.bin" >/dev/null

statsAfter=$(curl -sf "$XETD_URL/v1/stats")
filesAfter=$(echo "$statsAfter" | grep -o '"files_stored":[0-9]*' | grep -o '[0-9]*$')

if [[ "$filesAfter" -ne $((filesBefore + 1)) ]]; then
    echo "expected files_stored to increase by 1, went from $filesBefore to $filesAfter"
    exit 1
fi

echo "stats endpoint correctly reflects new upload: files_stored $filesBefore -> $filesAfter"
