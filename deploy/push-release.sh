#!/usr/bin/env bash
# deploy/push-release.sh: push a release tag to GitHub over https.
#
# The origin remote is SSH and this machine has no git push key, and the
# gh CLI's default token cannot write to the repo — so the plain
# `git push origin <tag>` from RELEASING.md fails. The writer token comes
# from the environment: GH_FOXESS_LOCAL (this repo's token) first, then
# GH_TOKEN / GITHUB_TOKEN (the deploy/publish.sh convention).
#
#   deploy/push-release.sh <tag>            # prereleases: the tag only
#   deploy/push-release.sh <tag> --main     # final releases: main + tag
set -euo pipefail
cd "$(dirname "$0")/.."

die() { echo "push-release: $*" >&2; exit 1; }

[ "$#" -ge 1 ] || die "usage: $0 <tag> [--main]"
tag="$1"
push_main="${2:-}"

REPO="$(git remote get-url origin | sed -E 's#^(git@|https://)github\.com[:/]##; s#\.git$##')"
[ -n "$REPO" ] || die "cannot derive owner/repo from the origin remote"

command -v gh >/dev/null || die "gh is required"

# gh and its git credential helper use GH_TOKEN when it is set
if [ -n "${GH_FOXESS_LOCAL:-}" ]; then
	GH_TOKEN="$GH_FOXESS_LOCAL"
elif [ -z "${GH_TOKEN:-}" ]; then
	GH_TOKEN="${GITHUB_TOKEN:-}"
fi
[ -n "${GH_TOKEN:-}" ] ||
	die "no writer token in the environment (GH_FOXESS_LOCAL, GH_TOKEN or GITHUB_TOKEN)"
export GH_TOKEN

# fail early: a token without write permission is the usual breakage
pushperm="$(gh api "repos/$REPO" --jq '.permissions.push // false' 2>/dev/null || echo false)"
[ "$pushperm" = "true" ] || die "the token cannot write to $REPO"

git rev-parse -q --verify "refs/tags/$tag" >/dev/null ||
	die "tag $tag not found locally: run make tag (PRE=1) first"
[ -z "$(git status --porcelain)" ] ||
	die "the working tree has changes: commit or stash them first"

gh auth setup-git
if [ "$push_main" = "--main" ]; then
	git push "https://github.com/$REPO.git" main "$tag"
else
	git push "https://github.com/$REPO.git" "$tag"
fi
echo "push-release: pushed $tag to $REPO (CI now builds the draft)"
