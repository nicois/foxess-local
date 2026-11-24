#!/usr/bin/env bash
# Checks a release directory before `make sign` signs it (RELEASING.md):
#
#   deploy/sign-check.sh VERSION [DIST]
#
# - VERSION is a release tag (vX.Y.Z or vX.Y.Z-rc.N);
# - every file SHA256SUMS lists is present and unchanged;
# - every binary carries VERSION as its stamped version (release.sh's
#   -X main.version), so a stale dist cannot be signed under the next
#   version's label.
#
# It reads the binaries and never runs them: the signing account must not
# execute what the build account wrote.
set -euo pipefail

VERSION="${1:-}"
DIST="${2:-dist}"
tag='v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?'

if ! [[ "$VERSION" =~ ^${tag}$ ]]; then
	echo "sign-check: VERSION \"$VERSION\" is not a release tag (vX.Y.Z or vX.Y.Z-rc.N)" >&2
	exit 2
fi
if ! (cd "$DIST" && sha256sum --check --quiet --strict SHA256SUMS) >&2; then
	echo "sign-check: $DIST/SHA256SUMS does not match the files in $DIST: run make release again" >&2
	exit 2
fi
bins=$(awk '$2 ~ /^foxess-(local|wizard)_linux_/ {print $2}' "$DIST/SHA256SUMS")
if [ -z "$bins" ]; then
	echo "sign-check: $DIST/SHA256SUMS lists no foxess-local binaries" >&2
	exit 2
fi
for b in $bins; do
	# every version-shaped string in the binary, whole: v0.4.1-rc.10 is
	# not v0.4.1-rc.1, nor v0.4.1-rc.1 v0.4.1
	if ! grep -aoE "$tag" "$DIST/$b" | grep -qxF -- "$VERSION"; then
		echo "sign-check: $DIST/$b does not carry $VERSION: run make release VERSION=$VERSION" >&2
		exit 2
	fi
done
echo "sign-check: $DIST/ is $VERSION"
