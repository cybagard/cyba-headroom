#!/usr/bin/env bash
# Run the darwin test binaries in a clean macOS Tart VM.
# The VM is a clone of $TART_BASE (APFS clone, no extra disk), sized small,
# and stopped afterwards: it takes one of the two macOS VM slots while it runs.
set -euo pipefail

vm=${TART_VM:-headroom-mac}
base=${TART_BASE:-macos-xcode}
root=$(cd "$(dirname "$0")/.." && pwd)
share="/Volumes/My Shared Files/src"

if ! tart get "$vm" >/dev/null 2>&1; then
	tart clone "$base" "$vm"
	tart set "$vm" --cpu 4 --memory 4096
fi

started=0
if [[ $(tart get "$vm" --format json | sed -n 's/.*"State" *: *"\([a-z]*\)".*/\1/p') != running ]]; then
	tart run --no-graphics --dir="src:$root:ro" "$vm" >/dev/null 2>&1 &
	started=1
fi
stop() { [[ $started == 1 ]] && tart stop "$vm" >/dev/null 2>&1 || true; }
trap stop EXIT

for _ in $(seq 60); do
	tart exec "$vm" test -d "$share" >/dev/null 2>&1 && break
	sleep 2
done
tart exec "$vm" bash "$share/scripts/darwin-tests.sh" run "$share"
