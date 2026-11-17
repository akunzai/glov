#!/bin/bash
# Ad-hoc signs glovd with the virtualization entitlement, which VZ (Linux) VMs need.
# swift build relinks glovd without the entitlement, so run this after every build, then restart the daemon.
#   tools/linux-vm/sign-daemon.sh            # .build/debug/glovd
#   tools/linux-vm/sign-daemon.sh <binary>   # any other glovd (e.g. .build/release/glovd)
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
bin=${1:-$repo/.build/debug/glovd}
[ -x "$bin" ] || { echo "sign-daemon: $bin not found; run swift build first" >&2; exit 1; }
codesign -s - --entitlements "$here/glovd.entitlements" -f "$bin"
codesign -d --entitlements - "$bin" 2>/dev/null | grep -q com.apple.security.virtualization \
  || { echo "sign-daemon: entitlement missing after signing $bin" >&2; exit 1; }
echo "signed $bin with com.apple.security.virtualization"
