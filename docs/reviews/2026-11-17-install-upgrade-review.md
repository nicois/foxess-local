# foxesslocal — Install & Upgrade Review

Date: 2026-11-17 · Reviewed at commit `3312b1b` · Companion to `2026-11-17-project-audit.md`

## Summary

The upgrade machinery is the strongest part of this path and the first install is the weakest: a newcomer faces eleven steps, five of them with little or no documentation, and the secure route through them is the long one. The review found 7 High, 13 Medium and 4 Low findings across 24 items. No release has been made yet, so all of them can be fixed before anyone depends on the current behaviour.

The findings that matter most:

- **A fresh install forwards readings to the vendor cloud without asking (I1).** For a tool people adopt to get away from the cloud, the default must be an explicit choice.
- **First-install trust is circular and verification is optional (T1, T2).** The key comes from the same place as the files, and the installer runs as root whether or not anything was checked.
- **Rerouting the inverter is one sentence (I2).** It is the step everything depends on and the one most likely to defeat a newcomer.
- **The installer and the upgrader both declare success too early (I3, G2).** One prints "started" unchecked; the other confirms an upgrade after two minutes of uptime.
- **Only half the system updates itself (G1).** Fixes to the gate and the units never reach users unless they happen to re-run a new installer.

The main proposal is to give the gateway its own small network for the inverter. That removes the hardest step for users and makes the system fail closed by layout, which is rare: one change that improves usability and security together. On top of it sit a first-run wizard and, for newcomers, a signed appliance image.

## Scope and method

This review covers everything between "I heard about this project" and "it has been running and updating itself for a year": the release pipeline, `installer.sh`, the first-run experience, self-upgrade and rollback, and the container route. It was done by reading the code and documents at commit `3312b1b`; nothing was installed or upgraded.

One fact frames every finding: **no release has ever been made.** `deploy/release-key.pub` is empty, prox is unpublished, and the design note records that the image build and the signature verifier have not been exercised against real artefacts. The design is reviewed here as written, not as proven.

Read: `deploy/gen-installer.sh` and the scripts and units it embeds, `deploy/release.sh`, `internal/update`, `deploy/upgrade-guard.sh`, `deploy/container/`, the three workflows, `INSTALL.md`, `RELEASING.md`, the distribution design (D-023), and the 14 November installer critique. Not tested: a real install on Debian, Raspberry Pi OS, Ubuntu or Fedora; a real upgrade or rollback; the container image.

Severity is judged for the stated audience: people who adopt the tool for security and may be new to Linux. **High** = blocks a newcomer or weakens the security promise. **Medium** = causes confusion, support load or a recoverable risk. **Low** = polish.

## The journey today

A newcomer faces eleven steps, and the documentation covers six of them. The installer itself (step 6) is the smoothest part; the hard steps are the ones on either side of it.

| # | Step | Documented | Where a newcomer stalls |
| --- | --- | --- | --- |
| 1 | Get a Linux machine onto the network and log in to it | No | Flashing a Pi, enabling SSH and finding its address are assumed. |
| 2 | Pick and download four files for the right CPU | Yes | "arm64 or armv7?" has no way to check from the instructions. |
| 3 | Move the files to the Linux machine | No | No `scp` example and no direct download commands. |
| 4 | Verify the signature | Yes | Needs `minisign` installed and a public key copied from the same site as the files. Easy to skip, and skipping still works. |
| 5 | Find the inverter's address and make it fixed | Partly | "On a fixed IP address" is listed as a requirement with no guidance on finding or fixing it. |
| 6 | Run the installer as root | Yes | Missing packages are named but not installed. IPv4 forwarding is warned about, not fixed. |
| 7 | Save the token shown once in the terminal | Yes | A 67-character secret in SSH scrollback, to be retyped on a phone. |
| 8 | Change the inverter's gateway to the Linux machine | One sentence | The hardest step. Where the dongle's network settings are is not said, and most home routers cannot set a per-device gateway. |
| 9 | Wait, then open the web UI and paste the token | Yes | Nothing tells the user whether the inverter has arrived, or why not. |
| 10 | Decide what the cloud may see | Buried | The default is chosen silently (see I1). |
| 11 | Keep it updated | Yes | App updates are one click. Gate and unit updates need a new download and root, and nothing announces them. |

Steps 1, 3, 5 and 8 are where a person without Linux or networking experience will stop. None of them is the installer's fault, and all of them are the product's problem.

## Install findings

The installer is a sound piece of shell: idempotent, atomic file writes, no network access, config kept on re-runs, clean uninstall. Its weakness is that it stops at "files are in place" and leaves the user to discover whether the system works.

| ID | Severity | Finding | Evidence | Recommendation |
| --- | --- | --- | --- | --- |
| I1 | High | A fresh install sends the inverter's data to the vendor cloud by default, without asking. The default mode is `telemetry_only`, which is also the one mode never tested on the live system. | `config.go` `validateForward`; `config.example.toml`; CLAUDE.md ("untested experiment") | Make the cloud posture an explicit question at install with no silent default. Until `telemetry_only` is proven live, default to `off`. |
| I2 | High | Pointing the inverter at the gateway is one sentence, and it is the step the whole product depends on. There is no guide to the dongle's settings, no fallback for routers without per-device gateway options, and no check that it worked. | INSTALL.md step 4 | See the proposed experience: give the gateway its own Wi-Fi network for the dongle, or serve DHCP to the inverter alone. Add a per-dongle guide with screenshots. |
| I3 | High | The installer reports success without checking anything. It prints "started" whether or not the service is running, the gate is applied, or the UI answers. A failed token mint is swallowed (`2>/dev/null ... \|\| true`), which leaves an app that refuses to start and no message saying why. | `gen-installer.sh` token and start blocks | End every install with a checked list: service active, gate rules loaded, UI reachable, token present, inverter seen or not yet. Fail loudly when the token cannot be minted. |
| I4 | Medium | Host firewalls are not detected. `ufw` and `firewalld` block port 18080 by default; Docker sets the forward policy to drop, which blocks the inverter's NTP; on Debian, restarting `nftables.service` flushes the gate until the 5-minute check restores it. | `check_requirements` tests for none of these | Detect each and either fix with consent or print the exact command. Ship the gate as a drop-in under `/etc/nftables.d` where the distribution supports it. |
| I5 | Medium | Missing prerequisites are listed, not resolved. IPv4 forwarding being off produces a warning and no remedy. | `check_requirements` | Offer to install `nftables` and `conntrack` through the system package manager, and to write a `sysctl.d` drop-in, each behind a yes/no prompt. |
| I6 | Medium | Starting the service is opt-in. Without `--enable` a newcomer is left with a `systemctl` command to copy. | `--enable` handling | Start by default; offer `--no-start` for the cautious. |
| I7 | Medium | The inverter address is taken on trust. `999.1.1.1` passes validation; nothing checks that the address is on the local network, is not this machine, or belongs to an inverter. | IPv4 regex in the installer | Offer discovery: list LAN devices and mark any that are dialling TCP 14431 or carry a known dongle MAC prefix. Validate octets and subnet. |
| I8 | Medium | The container route does not work as documented, by my reading. `--gate-only` seeds no `config.toml`, yet the compose file mounts `/etc/foxess-local` read-only and starts the app with that path, which fails on a missing file. The image name still contains `OWNER`. | `gen-installer.sh` (config seeded only when the app is installed); `compose.yaml`; `config.Load` | Seed the config in `--gate-only` too, or let the app run without a file. Exercise the container path once before documenting it. |
| I9 | Medium | The first credential is handed over awkwardly: a long token printed once in a terminal, to be carried to a browser. Losing it means stopping the service and using the command line. | Installer tail; INSTALL.md troubleshooting | Print a one-time setup link and QR code valid for 15 minutes from the LAN. The user then names and stores their own token in the browser. |
| I10 | Low | The uninstall message is inaccurate. It says the inverter now reaches the cloud directly, but the inverter still routes through this machine until its gateway is changed back, and with forwarding off it is simply cut off. | Uninstall block | Say what is true and what to do: "set the inverter's gateway back to your router". |
| I11 | Low | The 956-line generated installer runs as root and has no automated test. Only the upgrade guard and key format are tested. | `deploy/*_test.go` | Run `shellcheck` and an install, re-install, uninstall cycle in containers for Debian, Ubuntu and Fedora in CI. |

## Upgrade findings

The self-upgrade design is better than most commercial products manage: signature and checksum verified against a built-in key, a self-test before the swap, an atomic replace, and automatic rollback. The gaps are in what it does not cover and how it decides an upgrade succeeded.

| ID | Severity | Finding | Evidence | Recommendation |
| --- | --- | --- | --- | --- |
| G1 | High | Only the app updates itself. The gate rules, systemd units, upgrade guard and scripts change only when the user downloads a new installer and runs it as root, and nothing records which version of them is installed or says when a re-run is needed. A security fix to the gate would never reach most users. | INSTALL.md ("re-run it"); no version stamp in the installed files | Stamp the installed host components with a version. Let the app compare it with what it expects and show "host components out of date" with the exact command. Longer term, a root-owned helper that installs a signed host bundle on request. |
| G2 | High | An upgrade is confirmed after two minutes of the process staying up. A version that runs but cannot hold the inverter session, or cannot write to the store, is declared healthy and the previous binary loses its protection. | `service.go` `Run`: `healthyAfter`, then `ConfirmHealthy` | Confirm only when the inverter session is up and fresh data has been stored, or after a longer window if the inverter was already offline before the upgrade. |
| G3 | Medium | The database is not protected across an upgrade. Migrations run on first start with no snapshot beforehand, each step and its version record are separate statements, and an older binary does not check for a newer schema. After a rollback, old code runs against a migrated database. | `store.go` `migrate` | Snapshot before migrating when space allows. Wrap each migration in a transaction. Refuse to open a schema newer than the binary knows, with a clear message. |
| G4 | Medium | The rollback guard counts every failed start, whatever the cause. A busy port or an unrelated config error during the pending window triggers a rollback of a good release. | `upgrade-guard.sh`; the listener exits non-zero after 120 s of `EADDRINUSE` | Have the app record why it exited, and count only failures that occur before the listener binds. |
| G5 | Medium | Updates are easy to miss and hard to judge. Availability appears on a settings page, with no dashboard banner or notification, and the confirmation shows a version number with no description of what changed or whether it is a security fix. | `app.js` settings panel; `Status` carries a URL only | Show a banner to installer-role sessions. Carry a short signed summary and a security flag in the release metadata. Offer opt-in automatic patch updates in a chosen time window. |
| G6 | Medium | There is no offline upgrade and no version choice. The app installs "latest" from GitHub or nothing. A user who blocks the gateway's internet access, a likely choice for this audience, has no verified path. | `Updater.latest`; `upgrade` command flags | Add `foxess-local upgrade --from <dir>` that verifies a downloaded release with the built-in key, and allow skipping a version. |
| G7 | Low | Re-running the installer overwrites the unit files, so local edits vanish. | `write_file` on units | Document systemd drop-ins as the supported way to customise. |

## Trust chain findings

Once a genuine copy is installed, the chain is strong: an offline key that CI never holds, reproducible builds, and an app that accepts nothing unsigned. The weak link is the first download, which is exactly where a newcomer has the least ability to judge.

| ID | Severity | Finding | Evidence | Recommendation |
| --- | --- | --- | --- | --- |
| T1 | High | First-install trust is circular. The public key is obtained from the same GitHub repository that serves the files, and the documented fallback asks the downloaded binary to verify itself. Anyone who can alter a release can alter both. | INSTALL.md step 2 | Publish the key fingerprint in at least two places not controlled by the same account. Add a second, independent proof such as build provenance attestations. Show the trusted fingerprint in the installer output and in the UI so users can compare. |
| T2 | High | Verification is a separate, optional step, and the root installer runs whatever binary sits beside it. The secure path is the longer path, so most newcomers will not take it. | `gen-installer.sh` binary selection; INSTALL.md | Make verification part of installing, not a step before it. Fewer artefacts help: one file to check instead of four. Best for newcomers is a signed package repository, where the operating system verifies every install and update automatically. |
| T3 | Medium | The release process has never been run. The key file is empty, the verifier has not been checked against the real signing tool, the image build was not exercised, and action pinning is pending. | `release-key.pub` (0 bytes); D-023 "not done / open" | Run a full rehearsal in a private repository: release, install on a Pi, upgrade, and a forced rollback. |
| T4 | Medium | Container users get a weaker guarantee than native users. The image is built and pushed by CI, is not signed, and is referenced as `latest`. They are trusting the two parties the offline key was designed to exclude. | `image.yml`; `compose.yaml` | Sign the image and publish its digest in the signed release metadata. Document pinning by digest. |
| T5 | Medium | There is one signing key with no rotation or revocation path. A lost key strands every install; a leaked one requires each user to verify a new release by hand. | RELEASING.md | Build in two keys, one active and one held offline as a successor, so a rotation is itself signed. Add a signed release date so an old release cannot be replayed as new. |
| T6 | Low | Reproducible builds are claimed and valuable, but only the maintainer rebuilds. | D-023 | Add a rebuild-and-compare job in a separate environment and publish the result with each release. |

The daily update check tells GitHub the gateway's address and app version. It is disclosed and can be turned off; that is the right treatment and should stay prominent.

## Proposed experience

The goal is three steps for a newcomer (flash, plug in, answer five questions in a browser) with a stronger security position than today's eleven-step path. The idea that makes both possible is to stop asking the user to reroute the inverter through their router.

### The key change: the gateway owns the inverter's network

Today the inverter stays on the home network and is asked to use a different gateway. That request is the hardest step for users (I2) and the reason the gate can fail open: the inverter always has a working path to the router.

Instead, let the gateway machine offer a small dedicated Wi-Fi network, and join the dongle to that. Joining the dongle to a Wi-Fi network is the vendor's own standard procedure, done once at every installation.

- **Simpler:** no router settings, no fixed addresses, no address to type. The only device on that network is the inverter, so discovery is automatic.
- **Safer:** the inverter has no route to the internet except through the gateway. If the gate rules are missing or the app is down, traffic stops. The fail-open findings in the audit (S2, S3) are closed by the network layout, not by a detector.

A wired dongle gets the same result from a second network port or a USB Ethernet adapter. Users who prefer the current layout keep it as an advanced option.

### Three ways in

| Route | For whom | What they do |
| --- | --- | --- |
| Appliance image | Newcomers with a Raspberry Pi | Flash a signed image with the standard imager, plug it in, open `http://foxess.local`, follow the wizard. |
| Guided setup | People with a Linux machine | Download one file, check it, run `sudo ./foxess-local setup`. It asks questions, fixes what it can with consent, and verifies the result. |
| Package or container | Experienced users | A signed package repository, where the system's own updater verifies everything, or the container with a pinned, signed digest. |

### The first-run wizard

One page, served only to the local network and only until the device is claimed:

1. **Claim the device** with the one-time code from the setup output or the device's label. The browser creates and stores the installer token.
2. **Choose what the cloud may see:** nothing, readings only, or full access. Each option says plainly what the vendor can then do. There is no default.
3. **Connect the inverter:** shows the dedicated network's name and password, then a live indicator that turns green when the inverter appears.
4. **Confirm the device:** model, firmware and battery read from the inverter, with capacity filled in from what it reports.
5. **Updates and alerts:** notify only, or install security fixes automatically in a chosen window; where to send alerts.

### Today and proposed, side by side

| Stage | Today | Proposed |
| --- | --- | --- |
| Getting the software | Four files, architecture chosen by hand | One image, or one file that detects the architecture |
| Verifying it | Optional, separate tool, key from the same site | Built into setup; fingerprint shown for comparison; a second independent proof |
| Prerequisites | Listed for the user to fix | Fixed with consent, or already present in the image |
| Finding the inverter | User supplies a fixed address | Automatic: it is the only device on its network |
| Rerouting the inverter | Router or dongle gateway setting | Join the dongle to the gateway's Wi-Fi |
| Cloud posture | Silent default that forwards readings | Explicit choice, no default |
| First sign-in | Paste a 67-character token | One-time code or QR, then a token the browser keeps |
| Knowing it works | Open the UI and wait | Checked list at the end of setup; live indicator in the wizard |
| App updates | One click on a settings page | Banner with a signed summary; optional automatic security fixes; offline bundle |
| Host component updates | New download, run as root, unannounced | Version tracked; the app says when and how; later, one signed bundle |
| After an upgrade | Confirmed after two minutes of uptime | Confirmed when the inverter is back and data is flowing; database snapshot kept |

### What must not be traded away

- The signing key stays offline and out of CI.
- No `curl | sh` install. Convenience comes from fewer artefacts and built-in checks, not from skipping them.
- Files that enforce policy (gate, units, rollback guard) stay root-owned and unwritable by the app.
- Nothing is forwarded to the cloud without an explicit choice.
- Automatic updates are opt-in, and still pass signature, self-test and health checks.
- The setup page is reachable only from the local network and closes once claimed.
- The appliance image must be reproducible and signed like the binaries, and must keep its operating system patched without user action. An image that goes stale is worse than no image.

## Prioritised actions

**First: before any public release (small changes, mostly in the installer):**

1. Remove the silent cloud default: ask at install, default to `off` (I1).
2. End the installer with a checked result list and fail loudly on a token error (I3).
3. Start by default; offer to install prerequisites and enable forwarding; detect `ufw`, `firewalld`, Docker and `nftables.service` (I4, I5, I6).
4. Validate the inverter address properly (I7).
5. Fix the container route and the uninstall message (I8, I10).
6. Gate upgrade confirmation on the inverter session and stored data (G2).
7. Create the signing key, publish its fingerprint in two independent places, and run a full release rehearsal including a forced rollback (T1, T3).

**Next: the first public release:**

1. Make verification part of installing, and reduce the download to one file with a `setup` command (T2).
2. One-time setup code and QR in place of the printed token (I9).
3. Version-stamp the host components and announce when they are out of date (G1).
4. Database snapshot, transactional migrations and a newer-schema guard (G3).
5. Update banner with a signed summary and security flag; offline upgrade from a directory (G5, G6).
6. Installer tests in CI across three distributions (I11).
7. Sign the container image and pin it by digest (T4).
8. A dongle guide with screenshots for the current gateway method (I2).

**Later: the newcomer path:**

1. Dedicated inverter network on the gateway, as the recommended layout (I2, and audit S2, S3).
2. First-run wizard in the web UI.
3. Signed package repository.
4. Appliance image for Raspberry Pi, once the dedicated network and wizard exist.
5. Second signing key and signed release dates; independent rebuild check (T5, T6).
6. Root-owned helper for signed host-component updates (G1).
