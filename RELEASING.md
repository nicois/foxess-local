# Releasing foxess-local

Design: `docs/knowledge/04-design/distribution.md` (D-023). The short
version: CI builds an unsigned draft; you check it by rebuilding (the
builds are reproducible), sign it offline, and publish. Installed copies
only accept binaries signed by the key built into them.

## Prerequisites (once)

1. ~~Publish prox and remove the local `replace` from `go.mod`~~ — done
   2026-11-17 (`make release` still refuses a local replace: a public
   release must build from public sources). Tagging prox (e.g. `v0.1.0`)
   and requiring the tag instead of the pseudo-version is optional.
2. **Create the signing key** on a machine you trust (ideally offline) —
   done 2026-11-18 (key ID `553C34FE26291351`):

   ```bash
   minisign -G -p foxess-local.pub -s foxess-local.key
   ```

   Keep `foxess-local.key` (and its password) offline and backed up.
   Copy the public key into the repo and commit it:

   ```bash
   cp foxess-local.pub deploy/release-key.pub
   git commit -m "release signing key" deploy/release-key.pub
   ```

   Every binary built afterwards carries it. **Losing the secret key**
   means users must reinstall manually; **leaking it** means rotating
   (commit a new key, update INSTALL.md step 2 — `TestInstallDocKey`
   fails until both agree — release, and tell users to verify that
   release by hand) — guard it.

   Optionally publish the key ID and key somewhere you control outside
   GitHub too (a personal site, a signed post): the key in INSTALL.md
   and the release notes both come from GitHub, so a second source is
   what protects a first install against a compromised GitHub account.
3. ~~Pin the GitHub Actions in `.github/workflows/*.yml` to commit SHAs~~
   — done 2026-11-18.

4. **The repository must be public** before the first release users
   install: installed copies check `api.github.com/repos/…/releases/latest`
   without credentials, and a private repository answers 404 (no update
   is ever offered).

## Each release

0. Rehearse, on both families: `test/release-e2e/rehearse.sh` (Debian 13,
   apt) and `DISTRO=fedora43 test/release-e2e/rehearse.sh` (dnf, SELinux
   enforcing) — about 20 minutes each; KVM, no root. On a throwaway VM it
   builds three releases
   signed with a throwaway key, installs the first with `installer.sh`,
   self-upgrades to the second from a fake GitHub API, checks that a
   deliberately broken third is rolled back by the upgrade guard, re-runs
   the installer and uninstalls. Nothing touches GitHub or the real key.
1. Tag: `make tag` (on a clean `main`). It picks the version from
   `CHANGELOG.md`'s Unreleased entries:
   - Breaking or Removed → major (minor before 1.0);
   - Added → minor;
   - anything else → patch.

   `BUMP=minor` or `BUMP=major` raises the bump; nothing lowers it. It
   dates the entries under the new version, commits "Release vX.Y.Z" and
   creates an annotated tag carrying that section. Nothing is pushed:
   check the commit and tag, then push both (`git push origin main
   vX.Y.Z`, the command it prints). CI runs the tests, builds `dist/` and
   opens a **draft** release whose notes are that changelog section,
   followed by the verification commands. A tag without a section fails
   before any draft.

   Between releases, add an entry under `## [Unreleased]` with each
   notable change. `make tag` refuses an empty Unreleased section.

   **A prerelease** first, to try a release on your own installation:
   `make tag PRE=1` tags `vX.Y.Z-rc.N` (the next version, the next free
   rc number) without touching `CHANGELOG.md`; its notes are the
   Unreleased section as it stands. Push the tag alone
   (`git push origin vX.Y.Z-rc.N` — or `deploy/push-release.sh
   vX.Y.Z-rc.N`, which pushes over https with the repo's writer token
   from the environment, `GH_FOXESS_LOCAL` first, then `GH_TOKEN`/
   `GITHUB_TOKEN`). CI builds the draft and marks it a
   prerelease. Check, sign and publish it as below. Only installations
   that opted in to prereleases (System → Updates → Update
   choices) are offered it, and its container image is never tagged
   `latest`. When it holds up, `make tag` makes the release.

      **One-shot (`make publish`)**: with a write-token `gh` in the
      environment (`GH_FOXESS_LOCAL`, or `GH_TOKEN`/`GITHUB_TOKEN`),
      `make publish VERSION=v0.5.0-rc.1
      REPO=<owner>/<repo>` does steps 2-5 in one go: it builds `dist/`,
      pushes the tag, waits for the CI draft, then prints a prompt and
      polls for `dist/SHA256SUMS.minisig` while `make sign` runs on the
      machine with the key; it verifies, uploads and publishes.
2. Check the draft by rebuilding locally from the same tag:

   ```bash
   make release VERSION=v0.2.0 REPO=<owner>/<repo>
   gh release download v0.2.0 --dir /tmp/draft --pattern SHA256SUMS
   diff dist/SHA256SUMS /tmp/draft/SHA256SUMS   # identical: reproducible
   ```

3. Sign, as the account that holds the key (`SIGN_KEY`, default
   `/home/nfarrell/foxess-key/foxess-local.key`):
   `make -C <this checkout> sign VERSION=v0.2.0` (minisign asks for the
   key password). `make release` leaves `dist/SHA256SUMS.minisig` as an
   empty file that account may write; the build account reads the result.
   `make sign` first runs `deploy/sign-check.sh`, which refuses a
   `VERSION` that is not a release tag (`v0.4.1.rc.1`), a `SHA256SUMS`
   that does not match the files, and binaries that do not carry
   `VERSION` (a stale `dist/`). It reads the binaries without running them.
4. Verify with the new binary's own verifier (this also cross-checks the
   app's minisign implementation against the real tool):
   `make verify-release`.
5. Upload `dist/SHA256SUMS.minisig` to the draft and publish it. The
   image workflow then builds the container image from the published,
   verified assets.

Installed copies see the release within a day (or at once with
"check now") and offer it in System → Updates.
