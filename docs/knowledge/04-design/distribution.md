---
project: foxesslocal
level: 4
feature: distribution
last_verified: 2026-11-17
traces_up: [01-vision.md, 02-constraints.md, 03-architecture.md]
traces_down: [05-coverage.md, 06-tests.md]
---
# Design: build, install and upgrade

Status: **built 2026-11-17** (owner decisions: public GitHub releases,
signed; notify + one action to upgrade; amd64 / arm64 / armv7 + a
container image). Prerequisite before the first public release: prox
published, the local `replace` removed, the signing key created
(`RELEASING.md`).

## D-023: Signed releases, no-root self-upgrade with rollback, a general installer

**Decision**:

- **Releases**: `deploy/release.sh` (`make release`) builds static,
  reproducible binaries for linux amd64 / arm64 / armv7 (`-trimpath`,
  no VCS stamp, fixed ldflags; verified bit-identical across clean
  builds), the installer and `SHA256SUMS`. CI (`release.yml`) builds an
  unsigned draft on a tag; the owner checks it by rebuilding, signs
  `SHA256SUMS` offline with minisign, and publishes. CI never holds the
  key. The container image (`image.yml`) is built only from a published
  release after the release binary verified it.
- **Trust anchor**: the minisign public key is embedded in the binary
  (`deploy/release-key.pub`). An installed binary accepts only releases
  signed by that key. Empty key (a source build): self-upgrade refused.
- **Self-upgrade** (`internal/update`): a daily check of GitHub's latest
  release (opt-out `[update] check`; one request carrying the version
  only). Applying needs an installer (UI button or `foxess-local
  upgrade`):
  1. verify the `SHA256SUMS` signature (legacy and BLAKE2b-prehashed),
     then the binary's hash;
  2. self-test it (`version` must print the tag);
  3. hard-link the installed binary to `.prev`, rename the new one over
     it, and write `upgrade-pending`;
  4. exit — systemd (`Restart=always`) starts the new binary.

  The app confirms the upgrade once it is healthy — the inverter
  session back with fresh data stored and no store write error — checked
  from two minutes of uptime on (2026-11-17, install review G2; it was
  uptime alone). An upgrade made while the inverter was online
  (`online=1` in the marker) that is not healthy within 15 minutes fails
  the process; one made while it was offline is confirmed after 10
  minutes. The root-owned `upgrade-guard.sh` (`ExecStartPre`) counts
  starts while it is pending (keeping `online=`) and restores `.prev` on
  the fourth, leaving `upgrade-failed` for the UI. No root at any step.
- **Installer**: subcommands `install` / `check` / `uninstall`; picks
  the binary for the machine's architecture; `--inverter-ip` is required
  on the first install (no IP is hard-coded anywhere any more); it mints
  the first installer token; `--enable` starts everything;
  `--gate-only` / `--app-only` split the components (container hosts).
  Requirements are checked up front: root, systemd, nftables + TPROXY,
  iproute2, conntrack, IPv4 forwarding.
- **Container**: distroless static image, host networking, rootful with
  all capabilities dropped but `NET_ADMIN` (TPROXY needs a real one in
  the host namespace), read-only root, no new privileges. The gate stays
  on the host. The image never updates itself (`FOXESS_CONTAINER`).

**Context**: the project goes public. Its users are more
security-conscious than average, run it on a Pi or a small box, and
should not need SSH and root for every release.

**Rationale**:

- Offline signing plus reproducible builds means a compromised CI or
  GitHub account cannot ship a binary that installed copies accept, and
  anyone can check a release by rebuilding it.
- The app already owned its binary directory (drop-in upgrades). The
  signature check is what makes self-replacement safe; the root-owned
  guard bounds the cost of a bad release to three failed starts.

**Trades against**: a compromised running app could already rewrite its
own binary (unchanged by this design; the guard and the gate remain
root-owned). Accepted with the owner's no-root goal; the residual is
named in INSTALL.md's security table.

**Not done / open**:

- The image build was not exercised here: this machine's rootless podman
  lacks subuid mappings.
- The minisign verifier is tested against the format built from its
  specification, not yet against the reference tool's output;
  `make verify-release` at the first real release closes that.
- Action SHA pinning; DEF-006 (socket activation to drop the interim
  capability) remains blocked on prox.
