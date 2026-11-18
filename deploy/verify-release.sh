#!/usr/bin/env bash
# Runs the freshly built release binary for this machine against its own
# release directory: SHA256SUMS signature (with the key built into that
# binary) and every file's hash. Run before publishing a release.
set -euo pipefail
DIST="${1:-dist}"
arch="$(go env GOARCH)"
[ "$arch" = arm ] && arch=armv7
exec "$DIST/foxess-local_linux_$arch" verify-release "$DIST"
