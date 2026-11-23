#!/usr/bin/env bash
# make publish (RELEASING.md): the asset path of a release in one go —
# build dist/ from the tag, push the tag, wait for CI's unsigned draft,
# then prompt for and wait for the signature (the key holder's
# interactive step), verify it, upload it, and publish the release.
#
#   deploy/publish.sh VERSION [REPO] [DIST]
# e.g. deploy/publish.sh v0.5.0-rc.1 nicois/foxess-local dist
#
# The tag must exist locally first (make tag / make tag PRE=1). Needs a
# gh token that can write to the repo (GH_TOKEN or GITHUB_TOKEN in the
# environment); the tag push uses the normal git credentials. Neither
# CI nor this script holds the signing key: `make sign` is run on the
# machine with the key, and this script polls for
# dist/SHA256SUMS.minisig until it appears.
#
# POLL: seconds between checks (default 15). CI_TIMEOUT: seconds for the
# CI draft (default 1800). The signature wait has no timeout.
# CLOBBER=1 replaces a published SHA256SUMS.minisig that differs.
set -euo pipefail
cd "$(dirname "$0")/.."

die() { echo "publish: $*" >&2; exit 1; }

VERSION="${1:-}"
REPO="${2:-}"
DIST="${3:-dist}"
POLL="${POLL:-15}"
CI_TIMEOUT="${CI_TIMEOUT:-1800}"

case "$VERSION" in
v[0-9]*.[0-9]*.[0-9]*) ;;
*) die "VERSION must be a tag like v0.2.0" ;;
esac
# REPO is optional: derive it from the origin remote (owner/name).
if [ -z "$REPO" ]; then
	REPO="$(git remote get-url origin 2>/dev/null | sed -E 's#^git@github.com:##; s#^https://github.com/##; s#\.git$##')"
fi
[ -n "$REPO" ] || die "REPO (owner/repo) is required, or set the origin remote"
command -v gh >/dev/null || die "gh is required (with a write token: GH_TOKEN or GITHUB_TOKEN)"
git rev-parse -q --verify "refs/tags/$VERSION" >/dev/null ||
	die "tag $VERSION not found locally: run make tag (or make tag PRE=1) first"

# The signed assets must be the tag's bytes: a clean tree, HEAD at the tag.
[ -z "$(git status --porcelain)" ] || die "the working tree has changes: commit or stash them first"
head="$(git rev-parse HEAD)"
tagcommit="$(git rev-parse "$VERSION^{commit}")"
[ "$head" = "$tagcommit" ] || die "HEAD is not the tag's commit: check out $VERSION first"

# The writer token is needed for the upload and publish; fail early.
pushperm="$(gh api "repos/$REPO" --jq '.permissions.push // false' 2>/dev/null || echo false)"
[ "$pushperm" = "true" ] ||
	die "the gh token cannot write to $REPO: set GH_TOKEN/GITHUB_TOKEN to a writer token"

# 1) the local assets (release.sh also refuses a local go.mod replace).
echo "==> building $DIST/ for $VERSION"
bash deploy/release.sh "$VERSION" "$REPO" "$DIST"
if [ -s "$DIST/SHA256SUMS.minisig" ]; then
	echo "    keeping the existing $DIST/SHA256SUMS.minisig"
else
	# the signer is another account: an existing file it may write is all
	# minisign needs (it truncates in place)
	install -m 0666 /dev/null "$DIST/SHA256SUMS.minisig"
fi

# 2) the draft: push the tag and wait for the release workflow, unless a
#    release (draft or published) already exists.
if gh release view "$VERSION" >/dev/null 2>&1; then
	echo "==> a $VERSION release already exists on GitHub: skipping the push and CI"
else
	echo "==> pushing the tag"
	git push origin "$VERSION"
	echo "==> waiting for the CI draft (workflow 'release', tag $VERSION; ${CI_TIMEOUT}s budget)"
	deadline=$(( $(date +%s) + CI_TIMEOUT ))
	while :; do
		line="$(gh run list --workflow release.yml --limit 30 \
			--template '{{range .}}{{.DatabaseID}}|{{.HeadBranch}}|{{.Status}}|{{.Conclusion}}
{{end}}' | grep -F "|$VERSION|" | head -1 || true)"
		if [ -n "$line" ]; then
			IFS='|' read -r id _ status conclusion <<<"$line"
			if [ "$status" = "completed" ]; then
				if [ "$conclusion" = "success" ]; then
					echo "    CI run $id succeeded"
					break
				fi
				die "the release workflow ended '$conclusion' (run $id): gh run view $id"
			fi
		fi
		if [ "$(date +%s)" -ge "$deadline" ]; then
			die "the release workflow did not finish within ${CI_TIMEOUT}s: gh run list --workflow release.yml"
		fi
		sleep "$POLL"
	done
fi

# 3) the signature: a human step on the machine that holds the key.
if [ ! -s "$DIST/SHA256SUMS.minisig" ]; then
	echo
	echo "==> TIME TO SIGN: on the machine that holds the key (SIGN_KEY), run"
	echo
	echo "        make sign VERSION=$VERSION"
	echo
	echo "    and put the result here as $DIST/SHA256SUMS.minisig (e.g. by scp)."
	echo "    Waiting for it (polling every ${POLL}s; Ctrl-C to abort) …"
	n=0
	while [ ! -s "$DIST/SHA256SUMS.minisig" ]; do
		sleep "$POLL"
		n=$((n + 1))
		if [ $((n % 4)) -eq 0 ]; then
			echo "    still waiting for $DIST/SHA256SUMS.minisig …"
		fi
	done
fi

# 4) verify with the release's own verifier before it goes live.
echo "==> verifying"
bash deploy/verify-release.sh "$DIST"

# 5) upload the signature (skip when the same one is already there).
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
echo "==> uploading the signature"
if gh release download "$VERSION" --pattern SHA256SUMS.minisig --dir "$tmp" >/dev/null 2>&1 \
	&& [ -s "$tmp/SHA256SUMS.minisig" ]; then
	if cmp -s "$tmp/SHA256SUMS.minisig" "$DIST/SHA256SUMS.minisig"; then
		echo "    already uploaded and identical: skipping"
	elif [ "${CLOBBER:-0}" = "1" ]; then
		gh release upload "$VERSION" "$DIST/SHA256SUMS.minisig" --clobber
	else
		die "the published SHA256SUMS.minisig differs from the local one; re-run with CLOBBER=1 to replace it"
	fi
else
	gh release upload "$VERSION" "$DIST/SHA256SUMS.minisig"
fi

# 6) publish (a no-op when it already is).
if [ "$(gh release view "$VERSION" --json draft --jq .draft)" = "true" ]; then
	echo "==> publishing"
	gh release edit "$VERSION" --draft=false
fi
echo
echo "published: https://github.com/$REPO/releases/tag/$VERSION"
case "$VERSION" in
*-*) echo "prerelease: offered only to opted-in installations; the image is never tagged latest" ;;
esac
echo "the image workflow now builds the container image from the published assets"
if git rev-parse -q --verify refs/remotes/origin/main >/dev/null 2>&1 \
	&& ! git merge-base --is-ancestor "$tagcommit" refs/remotes/origin/main; then
	echo "note: the release commit is not on origin/main yet: git push origin main"
fi
