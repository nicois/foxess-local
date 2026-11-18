#!/usr/bin/env bash
# make tag (RELEASING.md): the next release's version from CHANGELOG.md's
# Unreleased entries; the entries dated under it; a "Release vX.Y.Z"
# commit and an annotated tag carrying that section. Nothing is pushed:
# it prints the push command, so the commit and tag can be looked at
# first.
#
#   deploy/tag.sh            # the bump the entries call for
#   BUMP=minor deploy/tag.sh # raise it (never lower it)
#   PRE=1 deploy/tag.sh      # a prerelease of that version: vX.Y.Z-rc.N
#
# A prerelease leaves CHANGELOG.md alone (no commit): its tag and its
# draft's notes are the Unreleased section as it stands. Installations
# see it only when an installer opted in to prereleases; the release
# that follows moves the entries under its version as usual.
set -euo pipefail
cd "$(dirname "$0")/.."

die() { echo "tag: $*" >&2; exit 1; }

[ -z "$(git status --porcelain)" ] || die "the working tree has changes: commit or stash them first"
branch="$(git symbolic-ref --short HEAD 2>/dev/null || true)"
[ "$branch" = main ] || die "on '${branch:-a detached HEAD}', not main"

# the last release (prereleases do not move the base version)
last="$(git tag -l 'v*.*.*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -1 || true)"
next="$(go run ./tools/changelog next -from "$last" ${BUMP:+-bump "$BUMP"})" || die "no version (see above)"
git rev-parse -q --verify "refs/tags/$next" >/dev/null && die "$next is already tagged"

if [ -n "${PRE:-}" ]; then
	n="$(git tag -l "$next-rc.*" | sed 's/.*-rc\.//' | grep -E '^[0-9]+$' | sort -n | tail -1 || true)"
	pre="$next-rc.$((${n:-0} + 1))"
	notes="$(go run ./tools/changelog notes -version Unreleased)"
	git tag -a --cleanup=verbatim "$pre" -m "foxess-local $pre (prerelease)" -m "$notes"
	echo "tag: $last -> $pre (prerelease of $next)"
	echo
	echo "$notes"
	echo
	echo "Push the tag (CI then builds a prerelease draft):"
	echo "  git push origin $pre"
	exit 0
fi

go run ./tools/changelog release -version "$next" -date "$(date -u +%F)"
notes="$(go run ./tools/changelog notes -version "$next")"
# verbatim: the notes' "### Added" headings are not comment lines
git commit -q --cleanup=verbatim -m "Release $next" -m "$notes" CHANGELOG.md
git tag -a --cleanup=verbatim "$next" -m "foxess-local $next" -m "$notes"

echo "tag: $last -> $next"
echo
echo "$notes"
echo
echo "Push the release commit and the tag (CI then builds the draft):"
echo "  git push origin main $next"
