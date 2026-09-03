#!/bin/bash
# Pulling an unknown file_id must fail (non-zero exit), not silently write
# an empty/garbage file.
set -uo pipefail

if "$XET" pull -server "$XETD_URL" -out "$WORKDIR/should-not-exist.bin" "totally-bogus-file-id-000"; then
    echo "expected pull of an unknown file_id to fail, but it succeeded"
    exit 1
fi

if [[ -s "$WORKDIR/should-not-exist.bin" ]]; then
    echo "pull failed as expected but still left a non-empty output file"
    exit 1
fi

echo "pull of unknown file_id correctly failed"
