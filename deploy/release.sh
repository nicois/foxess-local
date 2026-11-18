#!/usr/bin/env bash
# Builds a foxess-local release into DIST (design D-023; RELEASING.md).
#
#   deploy/release.sh VERSION REPO [DIST]
#   e.g. deploy/release.sh v0.2.0 owner/foxess-local dist
#
# Static, reproducible builds (-trimpath, no VCS stamp, fixed flags):
# the same source and Go toolchain give bit-identical binaries, so anyone
# can check a release by rebuilding it. Signing is a separate, local
# step (make sign) — CI never holds the key.
set -euo pipefail

VERSION="${1:-}"
REPO="${2:-}"
DIST="${3:-dist}"

case "$VERSION" in
v[0-9]*.[0-9]*.[0-9]*) ;;
*)
	echo "release: VERSION must be a tag like v0.2.0" >&2
	exit 2
	;;
esac
if [ -z "$REPO" ]; then
	echo "release: REPO must be the GitHub owner/name releases are published to" >&2
	exit 2
fi
if grep -q '^replace .* => /' go.mod && [ -z "${ALLOW_LOCAL_REPLACE:-}" ]; then
	echo "release: go.mod replaces a module with a local path; a public release must build from public sources" >&2
	echo "         (ALLOW_LOCAL_REPLACE=1 for a dry run)" >&2
	exit 2
fi
if [ ! -s deploy/release-key.pub ]; then
	echo "release: WARNING deploy/release-key.pub is empty: these binaries cannot self-upgrade" >&2
fi

mkdir -p "$DIST"
ldflags="-s -w -X main.version=$VERSION -X main.releaseRepo=$REPO"
# ARCHES narrows the build (the release rehearsal builds amd64 only)
for arch in ${ARCHES:-amd64 arm64 armv7}; do
	goarch="$arch"
	goarm=""
	if [ "$arch" = armv7 ]; then
		goarch=arm
		goarm=7
	fi
	CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" GOARM="$goarm" \
		go build -trimpath -buildvcs=false -ldflags "$ldflags" \
		-o "$DIST/foxess-local_linux_$arch" ./cmd/foxess-local
done
bash deploy/gen-installer.sh >"$DIST/installer.sh"
chmod +x "$DIST/installer.sh"
(cd "$DIST" && sha256sum foxess-local_linux_* installer.sh >SHA256SUMS)
echo "release: $DIST/ ready ($VERSION for $REPO) — next: make sign, then make verify-release"
