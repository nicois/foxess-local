#!/usr/bin/env bash
# Release rehearsal (RELEASING.md): install and self-upgrade end to end
# on a throwaway Debian 13 or Fedora 43 VM, exactly as a user would, without GitHub
# and without the real signing key.
#
#   test/release-e2e/rehearse.sh                  # Debian 13; ~20 min; needs KVM
#   DISTRO=fedora43 test/release-e2e/rehearse.sh  # Fedora 43 (dnf, SELinux enforcing)
#   KEEP=1 test/release-e2e/rehearse.sh           # leave the VM running after
#
# Needs: qemu-system-x86_64 with /dev/kvm, qemu-img, genisoimage,
# minisign, ssh, curl, and network access (the cloud image, the package manager in
# the VM). Nothing runs as root on this machine.
#
# What it checks:
#   1. release.sh builds v0.0.1, v0.0.2 and v0.0.3 (deliberately broken:
#      `run` exits at once) signed with a throwaway key built into them;
#      each binary verifies its own release (verify-release);
#   2. installer.sh installs v0.0.1 on a fresh VM (--fix installs the
#      missing packages): gate and app active, the token works;
#   3. the app upgrades itself to v0.0.2 from a fake GitHub API
#      (FOXESS_UPDATE_API) — check, apply, restart, the upgrade marker
#      committed after the healthy-run wait (the VM has no inverter);
#   4. v0.0.3 fails to start: the upgrade guard restores v0.0.2 and the
#      API reports the rollback;
#   5. an older download's installer refuses to downgrade; re-running the
#      installer keeps the config and the token;
#   6. uninstall --purge removes everything.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CACHE="$ROOT/.cache/e2e"
DISTRO="${DISTRO:-debian13}"
case "$DISTRO" in
debian13)
	IMG_URL="https://cloud.debian.org/images/cloud/trixie/latest"
	IMG="debian-13-genericcloud-amd64.qcow2"
	;;
fedora43)
	IMG_URL="https://download.fedoraproject.org/pub/fedora/linux/releases/43/Cloud/x86_64/images"
	IMG="Fedora-Cloud-Base-Generic-43-1.6.x86_64.qcow2"
	;;
*)
	echo "DISTRO must be debian13 or fedora43" >&2
	exit 2
	;;
esac
REPO="e2e/foxess-local"
SSH_PORT="${SSH_PORT:-2222}"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/foxess-e2e.XXXXXX")"
PIDFILE="$WORK/qemu.pid"

log() { printf '\n== %s\n' "$*"; }
fail() { printf '\nFAIL: %s\n' "$*" >&2; exit 1; }
cleanup() {
	if [ -z "${KEEP:-}" ] && [ -f "$PIDFILE" ]; then
		kill "$(cat "$PIDFILE")" 2>/dev/null || true
	fi
	[ -n "${KEEP:-}" ] && echo "VM kept: ssh -p $SSH_PORT -i $WORK/id root@127.0.0.1 (work dir $WORK)" && return
	rm -rf "$WORK"
}
trap cleanup EXIT

for t in qemu-system-x86_64 qemu-img genisoimage minisign ssh curl go; do
	command -v "$t" >/dev/null || fail "missing tool: $t"
done
[ -w /dev/kvm ] || fail "no access to /dev/kvm"

# ---- 1. the releases ----
log "building three signed releases with a throwaway key"
SRC="$WORK/src"
mkdir -p "$SRC"
(cd "$ROOT" && git ls-files -co --exclude-standard -z | xargs -0 cp --parents -t "$SRC")
minisign -G -W -p "$WORK/key.pub" -s "$WORK/key.sec" >/dev/null
cp "$WORK/key.pub" "$SRC/deploy/release-key.pub"
build() { # build TAG
	(cd "$SRC" && ARCHES=amd64 deploy/release.sh "$1" "$REPO" "$WORK/$1" >/dev/null)
	minisign -S -s "$WORK/key.sec" -m "$WORK/$1/SHA256SUMS" -x "$WORK/$1/SHA256SUMS.minisig" -t "foxess-local $1" >/dev/null
	minisign -V -p "$WORK/key.pub" -m "$WORK/$1/SHA256SUMS" >/dev/null || fail "$1: minisign rejects the signature"
	"$WORK/$1/foxess-local_linux_amd64" verify-release "$WORK/$1" >/dev/null || fail "$1: verify-release"
	[ "$("$WORK/$1/foxess-local_linux_amd64" version)" = "$1" ] || fail "$1: version stamp"
}
build v0.0.1
build v0.0.2
# v0.0.3: passes the self-test (`version`) but cannot run — the case the
# upgrade guard exists for
cat >"$SRC/cmd/foxess-local/zz_e2e_broken.go" <<'EOF'
package main

import (
	"fmt"
	"os"
)

func init() {
	if len(os.Args) > 1 && os.Args[1] == "run" {
		fmt.Fprintln(os.Stderr, "e2e: this release is deliberately broken")
		os.Exit(1)
	}
}
EOF
build v0.0.3
rm "$SRC/cmd/foxess-local/zz_e2e_broken.go"

# the fake GitHub API: /repos/<repo>/releases/latest + the assets
for v in v0.0.2 v0.0.3; do
	mkdir -p "$WORK/www/dl/$v"
	cp "$WORK/$v/foxess-local_linux_amd64" "$WORK/$v/SHA256SUMS" "$WORK/$v/SHA256SUMS.minisig" "$WORK/www/dl/$v/"
	size=$(stat -c %s "$WORK/$v/foxess-local_linux_amd64")
	cat >"$WORK/latest-$v.json" <<EOF
{"tag_name":"$v","html_url":"http://127.0.0.1:8099/$v","draft":false,"prerelease":false,"assets":[
 {"name":"foxess-local_linux_amd64","browser_download_url":"http://127.0.0.1:8099/dl/$v/foxess-local_linux_amd64","size":$size},
 {"name":"SHA256SUMS","browser_download_url":"http://127.0.0.1:8099/dl/$v/SHA256SUMS"},
 {"name":"SHA256SUMS.minisig","browser_download_url":"http://127.0.0.1:8099/dl/$v/SHA256SUMS.minisig"}]}
EOF
done

# ---- the VM ----
log "booting a fresh $DISTRO VM"
mkdir -p "$CACHE"
if [ ! -f "$CACHE/$IMG" ]; then
	curl -sSfL -o "$CACHE/$IMG.part" "$IMG_URL/$IMG"
	case "$DISTRO" in
	debian13)
		want=$(curl -sSfL "$IMG_URL/SHA512SUMS" | awk -v f="$IMG" '$2 == f {print $1}')
		got=$(sha512sum "$CACHE/$IMG.part" | cut -d' ' -f1)
		;;
	fedora43) # "SHA256 (<file>) = <hash>"
		want=$(curl -sSfL "$IMG_URL/Fedora-Cloud-43-1.6-x86_64-CHECKSUM" | sed -n "s/^SHA256 ($IMG) = //p")
		got=$(sha256sum "$CACHE/$IMG.part" | cut -d' ' -f1)
		;;
	esac
	[ -n "$want" ] && [ "$got" = "$want" ] || fail "cloud image checksum"
	mv "$CACHE/$IMG.part" "$CACHE/$IMG"
fi
qemu-img create -q -f qcow2 -F qcow2 -b "$CACHE/$IMG" "$WORK/disk.qcow2" 8G
ssh-keygen -q -t ed25519 -N "" -f "$WORK/id"
mkdir -p "$WORK/seed"
cat >"$WORK/seed/user-data" <<EOF
#cloud-config
disable_root: false
users:
  - name: root
    ssh_authorized_keys: ["$(cat "$WORK/id.pub")"]
EOF
printf 'instance-id: foxess-e2e\nlocal-hostname: foxess-e2e\n' >"$WORK/seed/meta-data"
# Fedora's sshd refuses root by password only; key login is allowed, but
# cloud-init must not lock root (disable_root above)
genisoimage -quiet -output "$WORK/seed.iso" -volid cidata -joliet -rock "$WORK/seed/user-data" "$WORK/seed/meta-data"
qemu-system-x86_64 -enable-kvm -cpu host -m 1024 -smp 2 -display none -daemonize -pidfile "$PIDFILE" \
	-drive "file=$WORK/disk.qcow2,if=virtio" -drive "file=$WORK/seed.iso,if=virtio,media=cdrom" \
	-nic "user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:$SSH_PORT-:22"
SSH=(ssh -q -p "$SSH_PORT" -i "$WORK/id" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 root@127.0.0.1)
vm() { "${SSH[@]}" "$@"; }
put() { tar -C "$1" -cf - . | vm "mkdir -p $2 && tar -C $2 -xf -"; }
for _ in $(seq 60); do vm true 2>/dev/null && break; sleep 5; done
vm true || fail "the VM never answered on ssh"
vm cloud-init status --wait >/dev/null || true

# ---- 2. install ----
log "installing v0.0.1 with installer.sh"
put "$WORK/v0.0.1" /root/v0.0.1
vm "cd /root/v0.0.1 && bash installer.sh install --inverter-ip 192.0.2.10 --fix --forward-mode off" | tee "$WORK/install.log"
TOKEN=$(grep -Eo 'fl_[0-9a-f]+' "$WORK/install.log" | head -1)
[ -n "$TOKEN" ] || fail "the installer printed no token"
vm systemctl is-active foxess-gate foxess-local >/dev/null || fail "gate or app not active after install"
api() { vm curl -sS -H "'Authorization: Bearer $TOKEN'" "$@"; }
update_field() { api http://127.0.0.1:18080/api/v1/update | python3 -c "import json,sys; print(json.load(sys.stdin).get('$1') or '')"; }
[ "$(update_field current)" = v0.0.1 ] || fail "the app does not report v0.0.1"

# point the release check at the fake API (a drop-in, as a mirror would)
put "$WORK/www" /root/www
vm "mkdir -p /etc/systemd/system/foxess-local.service.d && printf '[Service]\nEnvironment=FOXESS_UPDATE_API=http://127.0.0.1:8099\n' >/etc/systemd/system/foxess-local.service.d/e2e.conf && systemctl daemon-reload && systemctl restart foxess-local"
vm "systemd-run --unit e2e-www python3 -m http.server 8099 --bind 127.0.0.1 --directory /root/www" >/dev/null
publish() { # publish TAG: it becomes "latest"
	vm "mkdir -p /root/www/repos/$REPO/releases" && "${SSH[@]}" "cat >/root/www/repos/$REPO/releases/latest" <"$WORK/latest-$1.json"
}
wait_current() { # wait_current TAG SECONDS
	for _ in $(seq "$(($2 / 5))"); do
		[ "$(update_field current 2>/dev/null)" = "$1" ] && return 0
		sleep 5
	done
	fail "the app did not come back as $1 within $2 s"
}
wait_current v0.0.1 60

# ---- 3. upgrade ----
log "self-upgrade to v0.0.2"
publish v0.0.2
api -X POST http://127.0.0.1:18080/api/v1/update/check >/dev/null
[ "$(update_field available)" = True ] || fail "v0.0.2 not offered: $(api http://127.0.0.1:18080/api/v1/update)"
api -X POST http://127.0.0.1:18080/api/v1/update/apply >/dev/null
wait_current v0.0.2 120
vm test -f /var/lib/foxess-local/bin/upgrade-pending || fail "no upgrade-pending marker after the upgrade"
vm "journalctl -u foxess-local --no-pager -o cat | grep -q 'starting v0.0.2'" || fail "the guard never saw v0.0.2 start"
vm test -f /var/lib/foxess-local/bin/foxess-local.prev || fail "the previous binary was not kept"
log "waiting for the upgrade to be committed (no inverter: 10 min of healthy running)"
for _ in $(seq 150); do
	vm test -f /var/lib/foxess-local/bin/upgrade-pending || break
	sleep 5
done
vm test -f /var/lib/foxess-local/bin/upgrade-pending && fail "the upgrade was never committed"

# ---- 4. rollback ----
log "a broken v0.0.3 is rolled back"
publish v0.0.3
api -X POST http://127.0.0.1:18080/api/v1/update/check >/dev/null
api -X POST http://127.0.0.1:18080/api/v1/update/apply >/dev/null
# the old process answers until it restarts: wait for the guard's verdict
# (three failed starts, 5 s apart), not for the version
for _ in $(seq 36); do
	vm test -f /var/lib/foxess-local/bin/upgrade-failed && break
	sleep 5
done
vm test -f /var/lib/foxess-local/bin/upgrade-failed || fail "no upgrade-failed marker after the rollback"
wait_current v0.0.2 60
api http://127.0.0.1:18080/api/v1/update | grep -q '"rolled_back"' || fail "the API does not report the rollback"
vm journalctl -u foxess-local --no-pager | grep -q "restored v0.0.2" || fail "the guard did not log the restore"

# ---- 5. re-run the installer ----
log "an older download's installer refuses to downgrade"
if vm "cd /root/v0.0.1 && bash installer.sh install --inverter-ip 192.0.2.10 --fix --forward-mode off" >"$WORK/downgrade.log" 2>&1; then
	fail "the v0.0.1 installer downgraded v0.0.2"
fi
grep -q "older than the installed v0.0.2" "$WORK/downgrade.log" || fail "no downgrade refusal: $(tail -3 "$WORK/downgrade.log")"
wait_current v0.0.2 30
log "re-running the installer keeps config and token"
vm "cp /etc/foxess-local/config.toml /root/config.before"
vm "cd /root/v0.0.1 && bash installer.sh install --inverter-ip 192.0.2.10 --fix --forward-mode off --binary /var/lib/foxess-local/bin/foxess-local" >"$WORK/reinstall.log"
vm "cmp /etc/foxess-local/config.toml /root/config.before" || fail "the re-install changed the config"
grep -Eq 'fl_[0-9a-f]+' "$WORK/reinstall.log" && fail "the re-install minted a second token"
wait_current v0.0.2 60

# ---- 6. uninstall ----
log "uninstall --purge"
vm "cd /root/v0.0.1 && bash installer.sh uninstall --purge" >/dev/null
vm "! systemctl cat foxess-local foxess-gate >/dev/null 2>&1" || fail "units left after uninstall"
vm "! test -e /var/lib/foxess-local && ! test -e /etc/foxess-local" || fail "files left after uninstall --purge"
vm "! nft list tables | grep -q foxess" || fail "nftables rules left after uninstall"
vm "! ip rule | grep -q 'fwmark'" || fail "policy-routing rules left after uninstall"
vm "! id foxess >/dev/null 2>&1" || fail "the foxess user is left after uninstall --purge"
vm "! test -e /etc/sysctl.d/90-foxess.conf" || fail "the forwarding sysctl file is left after uninstall --purge"

if [ "$DISTRO" = fedora43 ]; then
	vm "test \"\$(getenforce)\" = Enforcing" || fail "SELinux was not enforcing: the run proves nothing about it"
	if vm "journalctl --no-pager -o cat | grep -E 'avc: +denied' | grep -Ei 'foxess|upgrade-guard|gate'" >"$WORK/avc.log"; then
		fail "SELinux denials:
$(cat "$WORK/avc.log")"
	fi
fi
log "PASS ($DISTRO): build, sign, verify, install, upgrade, commit, rollback, re-install, uninstall"
