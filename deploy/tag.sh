#!/usr/bin/env bash
# make tag (RELEASING.md): the next release's version from CHANGELOG.md's
# Unreleased entries; the entries dated under it; a "Release vX.Y.Z"
# commit and an annotated tag carrying that section. Nothing is pushed:
# it prints the push command, so the commit and tag can be looked at
# first.
#
#   deploy/tag.sh            # the bump the entries call for
#   BUMP=minor deploy/tag.sh # raise it (never lower it)
set -euo pipefail
cd "$(dirname "$0")/.."

die() { echo "tag: $*" >&2; exit 1; }

[ -z "$(git status --porcelain)" ] || die "the working tree has changes: commit or stash them first"
branch="$(git symbolic-ref --short HEAD 2>/dev/null || true)"
[ "$branch" = main ] || die "on '${branch:-a detached HEAD}', not main"

last="$(git tag -l 'v*.*.*' --sort=-v:refname | head -1)"
next="$(go run ./tools/changelog next -from "$last" ${BUMP:+-bump "$BUMP"})" || die "no version (see above)"
git rev-parse -q --verify "refs/tags/$next" >/dev/null && die "$next is already tagged"

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
