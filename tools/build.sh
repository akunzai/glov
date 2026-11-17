#!/usr/bin/env bash
set -euo pipefail

# Keep host and guest artifacts together so CLI, daemon and installed copies use the same build.
cd "$(dirname "$0")/.."
configuration="${1:-release}"
case "$configuration" in debug|release) ;; *) echo 'usage: tools/build.sh [debug|release]' >&2; exit 2 ;; esac
swift build -c "$configuration"
bin_dir="$(swift build -c "$configuration" --show-bin-path)"
agent_version="$(tools/version.sh)"
for guest_os in windows linux; do
    target_dir="$bin_dir/guest-services/$guest_os-arm64"
    mkdir -p "$target_dir"
    binary="$target_dir/glov-guest"
    if [[ "$guest_os" == windows ]]; then binary="$binary.exe"; fi
    (cd guest && GOOS="$guest_os" GOARCH=arm64 go build -ldflags "-X main.version=$agent_version" -o "$binary" ./cmd/glov-guest)
    python3 - "$binary" "$agent_version" "$target_dir/manifest.json" <<'PY'
import hashlib
import json
import pathlib
import sys
binary, version, manifest = sys.argv[1:]
pathlib.Path(manifest).write_text(json.dumps({"version": version, "sha256": hashlib.sha256(pathlib.Path(binary).read_bytes()).hexdigest()}) + "\n")
PY
done
# The host's own disk image helper, installed beside glovd: Linux guests then need nothing from Homebrew.
(cd diskimage && GOOS=darwin GOARCH=arm64 go build -ldflags "-X main.version=$agent_version" -o "$bin_dir/glov-image" ./cmd/glov-image)
# Both replacement bundles are complete; discard the obsolete generated layout.
rm -rf "$bin_dir/agents"
printf 'Built host, glov-image and Guest Services in %s\n' "$bin_dir"
