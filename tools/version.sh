#!/bin/sh
set -eu

# The build label every binary carries: the VERSION file, plus the commit (and -dirty) when git is available.
cd "$(dirname "$0")/.."
base="$(tr -d '[:space:]' < VERSION)"
if rev="$(git describe --always --dirty 2>/dev/null)"; then printf '%s+%s\n' "$base" "$rev"; else printf '%s\n' "$base"; fi
