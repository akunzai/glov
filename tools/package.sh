#!/usr/bin/env bash
set -euo pipefail

# The macOS release archive: a release build of glov, glovd, glov-image and the Guest Services, in one folder that
# `glov host install` installs from, plus its SHA-256 file. Prints the archive's path.
cd "$(dirname "$0")/.."
tools/build.sh release >&2
bin_dir="$(swift build -c release --show-bin-path)"
version="$(tr -d '[:space:]' < VERSION)"
name="glov-$version-macos-arm64"
dist=".build/dist"
stage="$dist/$name"
rm -rf "$stage" "$dist/$name.tar.gz" "$dist/$name.tar.gz.sha256"
mkdir -p "$stage"
cp "$bin_dir/glov" "$bin_dir/glovd" "$bin_dir/glov-image" "$stage/"
cp -R "$bin_dir/guest-services" "$stage/"
cp LICENSE "$stage/"
python3 -B tools/third-party-notices.py -o "$stage/THIRD_PARTY_NOTICES.md" --swift --go guest:linux --go guest:windows --go diskimage:darwin
# arm64 macOS runs no unsigned code: glov and glov-image must carry the linker's ad hoc signature, since only glovd is
# signed again by `glov host install`.
for binary in glov glovd glov-image; do /usr/bin/codesign -v "$stage/$binary"; done
tar --no-xattrs --no-mac-metadata --uid 0 --gid 0 --uname root --gname wheel -C "$dist" -czf "$dist/$name.tar.gz" "$name"
(cd "$dist" && shasum -a 256 "$name.tar.gz" > "$name.tar.gz.sha256")
printf '%s\n' "$dist/$name.tar.gz"
