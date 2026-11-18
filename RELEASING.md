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
1. Tag and push: `git tag v0.2.0 && git push origin v0.2.0`. CI runs the
   tests, builds `dist/` and opens a **draft** release.
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
4. Verify with the new binary's own verifier (this also cross-checks the
   app's minisign implementation against the real tool):
   `make verify-release`.
5. Upload `dist/SHA256SUMS.minisig` to the draft and publish it. The
   image workflow then builds the container image from the published,
   verified assets.

Installed copies see the release within a day (or at once with
"check now") and offer it in More → Settings → Updates.
