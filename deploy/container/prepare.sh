#!/usr/bin/env bash
# Lays out a container build context from a release directory:
#   deploy/container/prepare.sh DIST CTX
# CTX gets the Containerfile and bin/{amd64,arm64,arm} (the GOARCH names
# buildx passes as TARGETARCH; arm = the armv7 build).
set -euo pipefail
DIST="${1:?release directory}"
CTX="${2:?context directory}"
mkdir -p "$CTX/bin"
cp "$DIST/foxess-local_linux_amd64" "$CTX/bin/amd64"
cp "$DIST/foxess-local_linux_arm64" "$CTX/bin/arm64"
cp "$DIST/foxess-local_linux_armv7" "$CTX/bin/arm"
cp "$(dirname "$0")/Containerfile" "$CTX/Containerfile"
echo "prepare: $CTX ready"
