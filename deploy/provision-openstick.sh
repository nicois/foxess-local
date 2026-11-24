#!/usr/bin/env bash
# Prepares an OpenStick (a UZ801-class MSM8916 4G USB stick running the
# OpenStick Debian image) to be the inverter's gateway, and optionally
# installs foxess-local on it. Idempotent: every step checks first and
# changes only what is missing, so re-running it is safe (and is how a
# changed kernel gets its modules rebuilt).
#
# Usage (as root, on the stick):
#   provision-openstick.sh [--inverter-ip IP | --appliance] [--forward-mode MODE]
#                          [--version vX.Y.Z] [--hostname NAME]
#                          [--from DIR [--allow-unsigned]]
#                          [--ssh-keys-only] [--check] [--force]
#   provision-openstick.sh --seal [--check]
#
#   --inverter-ip IP     also install foxess-local: download the release
#                        for this stick, verify its signature, run its
#                        installer (skipped when already installed: an
#                        installed copy updates itself)
#   --appliance          prepare an appliance stick for someone else
#                        (D-031): foxess-local without an inverter, the
#                        setup wizard, the stick's hotspot name and
#                        password, and its label (~/foxess-label/);
#                        ModemManager masked, the IMEI checked
#   --seal               last, after --appliance and a test: SSH keys
#                        only (refused without one), well-known account
#                        passwords replaced, the wizard's state cleared,
#                        and this Wi-Fi connection removed (run it over
#                        USB): the stick then starts its hotspot, and
#                        every boot is "unconfigured" until set up
#   --from DIR           take the release files from DIR instead of
#                        downloading them (still verified)
#   --allow-unsigned     with --from: accept files without a signature
#                        (a development build; never for a real stick)
#   --forward-mode MODE  off | telemetry_only | full, for the installer's
#                        first-install question (asked when omitted)
#   --version TAG        the release to install (default: the latest)
#   --hostname NAME      set the stick's host name
#   --ssh-keys-only      turn off SSH password logins (refused unless the
#                        account you came in on has an authorized key)
#   --check              report what would change; change nothing
#   --force              run on a device that is not an MSM8916 stick
#
# What it does:
#   1. apt: pins the suite names to this release's codename ("stable"
#      now means a newer Debian; installing from it would start a partial
#      upgrade of the whole system)
#   2. packages: nftables, iproute2, conntrack, procps, curl, minisign,
#      zram swap
#   3. zram swap (half the RAM, zstd): the stick has ~320 MB
#   4. journald capped at 48 MB on flash
#   5. Wi-Fi power saving off on the client connection: the inverter's
#      traffic arrives over it
#   6. IPv4 forwarding on (the inverter's NTP passes through the gate)
#   7. TPROXY: the stock kernel has no TPROXY support, which the gate
#      needs. The modules are built here, on the stick, from the
#      kernel.org source of the running kernel's version (checksum
#      pinned below) with the running kernel's own configuration, then
#      loaded and tested. UDP transparent proxying is stubbed out (the
#      kernel does not export the UDP socket lookups; the gate proxies
#      TCP only). Takes about 5 minutes and ~250 MB of downloads,
#      packages and scratch space; the source is deleted afterwards.
#   8. warns about the image's well-known default password
#   9. with --inverter-ip: installs foxess-local (signed release);
#      with --appliance: the appliance (see above)
set -euo pipefail

REPO="${FOXESS_REPO:-nicois/foxess-local}"
# the release signing key (deploy/release-key.pub; TestProvisionKey)
RELEASE_KEY="RWRREykm/jQ8VVPcIxLpOEKOVyXC6B4/XP6i9qdkHRbDVlOTlm8vcIb3"
MODSET="foxess-tproxy"  # /lib/modules/<release>/updates/<this>
MODSET_REV=1            # bump when the module build changes

say() { echo "provision: $*"; }
ok() { echo "  ok       $*"; }
changed() { echo "  changed  $*"; }
would() { echo "  would    $*"; }
warn() { echo "  WARNING  $*"; }
die() {
	echo "provision: $*" >&2
	exit 1
}

INVERTER_IP="" FORWARD_MODE="" VERSION="" NEW_HOSTNAME="" KEYS_ONLY="" CHECK="" FORCE=""
APPLIANCE="" SEAL="" FROM="" ALLOW_UNSIGNED=""
while [ $# -gt 0 ]; do
	case "$1" in
	--inverter-ip) INVERTER_IP="${2:-}"; shift 2 ;;
	--inverter-ip=*) INVERTER_IP="${1#*=}"; shift ;;
	--forward-mode) FORWARD_MODE="${2:-}"; shift 2 ;;
	--forward-mode=*) FORWARD_MODE="${1#*=}"; shift ;;
	--version) VERSION="${2:-}"; shift 2 ;;
	--version=*) VERSION="${1#*=}"; shift ;;
	--hostname) NEW_HOSTNAME="${2:-}"; shift 2 ;;
	--hostname=*) NEW_HOSTNAME="${1#*=}"; shift ;;
	--ssh-keys-only) KEYS_ONLY=1; shift ;;
	--appliance) APPLIANCE=1; shift ;;
	--seal) SEAL=1; shift ;;
	--from) FROM="${2:-}"; shift 2 ;;
	--from=*) FROM="${1#*=}"; shift ;;
	--allow-unsigned) ALLOW_UNSIGNED=1; shift ;;
	--check) CHECK=1; shift ;;
	--force) FORCE=1; shift ;;
	-h | --help) sed -n '2,64p' "$0"; exit 0 ;;
	*) die "unknown option $1 (see --help)" ;;
	esac
done
[ -z "$APPLIANCE" ] || [ -z "$INVERTER_IP" ] || die "--appliance installs without an inverter (the wizard sets it): drop --inverter-ip"
if [ -n "$SEAL" ] && [ -n "$APPLIANCE$INVERTER_IP$FROM" ]; then
	die "--seal is a separate, last run: first --appliance, then test the stick, then --seal alone"
fi
[ -z "$ALLOW_UNSIGNED" ] || [ -n "$FROM" ] || die "--allow-unsigned needs --from (it is for local development builds)"
[ -z "$FROM" ] || [ -d "$FROM" ] || die "--from: $FROM is not a directory"
case "$FORWARD_MODE" in "" | off | telemetry_only | full) ;; *) die "--forward-mode: off, telemetry_only or full" ;; esac
case "$VERSION" in "" | v[0-9]*.[0-9]*.[0-9]*) ;; *) die "--version: a release tag like v0.2.0" ;; esac
if [ -n "$NEW_HOSTNAME" ] && ! echo "$NEW_HOSTNAME" | grep -Eq '^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$'; then
	die "--hostname: letters, digits and hyphens (not $NEW_HOSTNAME)"
fi

# ---- preflight ----
[ "$(id -u)" = 0 ] || die "run as root (sudo bash $0 ...)"
[ "$(uname -m)" = aarch64 ] || die "this is for the arm64 OpenStick image (this machine is $(uname -m))"
if ! tr '\0' '\n' </proc/device-tree/compatible 2>/dev/null | grep -qx 'qcom,msm8916'; then
	[ -n "$FORCE" ] || die "not an MSM8916 device; for other machines follow INSTALL.md (or --force)"
	say "not an MSM8916 device; continuing (--force)"
fi
# shellcheck disable=SC1091
CODENAME="$(. /etc/os-release && echo "${VERSION_CODENAME:-}")"
[ -n "$CODENAME" ] || die "cannot tell the Debian release (/etc/os-release has no VERSION_CODENAME)"
KVER="$(uname -r)"
MODDIR="/lib/modules/$KVER/updates/$MODSET"
[ -z "$CHECK" ] || say "--check: reporting only; nothing is changed"
say "device: $(tr -d '\0' </proc/device-tree/model 2>/dev/null || echo unknown), Debian $CODENAME, kernel $KVER"

# ---- 1. apt suites ----
# pin_suites FILE: "stable", "stable-updates", "stable-security" -> the
# codename, in one-line (.list) or deb822 (.sources) form. Prints the
# rewritten file on stdout.
pin_suites() {
	case "$1" in
	*.sources) sed -E "/^Suites:/ s/([[:space:]])stable(-[a-z]+)?([[:space:]]|\$)/\1${CODENAME}\2\3/g; /^Suites:/ s/([[:space:]])stable(-[a-z]+)?([[:space:]]|\$)/\1${CODENAME}\2\3/g" "$1" ;;
	*) sed -E "s/^(deb(-src)?[[:space:]]+(\[[^]]*\][[:space:]]+)?[^[:space:]]+[[:space:]]+)stable(-[a-z]+)?([[:space:]])/\1${CODENAME}\4\5/" "$1" ;;
	esac
}
step_apt_suites() {
	local f
	for f in /etc/apt/sources.list /etc/apt/sources.list.d/*.list /etc/apt/sources.list.d/*.sources; do
		[ -f "$f" ] || continue
		if pin_suites "$f" | cmp -s - "$f"; then
			ok "apt: $f names no moving suite"
		elif [ -n "$CHECK" ]; then
			would "apt: pin $f to $CODENAME (it names \"stable\")"
		else
			pin_suites "$f" >"$f.foxess.tmp" && mv "$f.foxess.tmp" "$f"
			APT_STALE=1
			changed "apt: $f pinned to $CODENAME"
		fi
	done
}

# ---- 2. packages ----
# refresh the package lists before the first install: a fresh image has
# none, and old ones name package versions the mirror no longer has
APT_STALE=1
installed() { dpkg-query -W -f='${Status}' "$1" 2>/dev/null | grep -q 'install ok installed'; }
# need_pkgs PKG...: install whichever are missing
need_pkgs() {
	local p missing=""
	for p in "$@"; do installed "$p" || missing="$missing $p"; done
	if [ -z "$missing" ]; then
		ok "packages:$(printf ' %s' "$@")"
		return
	fi
	if [ -n "$CHECK" ]; then
		would "install:$missing"
		return
	fi
	if [ -n "$APT_STALE" ]; then
		apt-get update -q
		APT_STALE=""
	fi
	# shellcheck disable=SC2086 # a word list
	DEBIAN_FRONTEND=noninteractive apt-get install -y -q --no-install-recommends $missing
	changed "installed:$missing"
}

# ---- 3. zram swap ----
ZRAM_CONF=/etc/systemd/zram-generator.conf
ZRAM_WANT='# provision-openstick.sh: compressed swap in RAM (the stick has ~320 MB)
[zram0]
zram-size = ram / 2
compression-algorithm = zstd'
step_zram() {
	if [ "$(cat "$ZRAM_CONF" 2>/dev/null)" = "$ZRAM_WANT" ]; then
		ok "zram swap configured"
	elif [ -n "$CHECK" ]; then
		would "configure zram swap ($ZRAM_CONF)"
		return
	else
		echo "$ZRAM_WANT" >"$ZRAM_CONF"
		systemctl daemon-reload
		changed "zram swap configured"
	fi
	if grep -q '^/dev/zram0 ' /proc/swaps; then
		ok "zram swap active"
	elif [ -n "$CHECK" ]; then
		would "start zram swap"
	else
		# the generator makes two units: the device, and the swap on it
		systemctl daemon-reload
		systemctl restart systemd-zram-setup@zram0.service
		systemctl start dev-zram0.swap
		changed "zram swap started ($(awk '/^\/dev\/zram0 /{print int($3/1024)" MB"}' /proc/swaps))"
	fi
}

# put_file DEST CONTENT DESCRIPTION: write when different; 0 = changed
put_file() {
	if [ "$(cat "$1" 2>/dev/null)" = "$2" ]; then
		ok "$3"
		return 1
	fi
	if [ -n "$CHECK" ]; then
		would "$3 ($1)"
		return 1
	fi
	mkdir -p "$(dirname "$1")"
	echo "$2" >"$1"
	changed "$3"
}

# ---- 4. journald ----
step_journald() {
	if put_file /etc/systemd/journald.conf.d/50-foxess-openstick.conf '# provision-openstick.sh: spare the flash
[Journal]
SystemMaxUse=48M
RuntimeMaxUse=16M' "journald capped at 48 MB"; then
		systemctl restart systemd-journald
	fi
}

# ---- 5. Wi-Fi ----
step_wifi() {
	command -v nmcli >/dev/null || { warn "no NetworkManager: check Wi-Fi power saving by hand"; return; }
	local uuid name ps method any=""
	while IFS=: read -r uuid name; do
		[ "$(nmcli -g 802-11-wireless.mode con show "$uuid" 2>/dev/null)" = infrastructure ] || continue
		any=1
		ps="$(nmcli -g 802-11-wireless.powersave con show "$uuid")"
		case "$ps" in
		2 | disable*) ok "Wi-Fi '$name': power saving off" ;;
		*)
			if [ -n "$CHECK" ]; then
				would "turn Wi-Fi power saving off on '$name'"
			else
				nmcli con modify "$uuid" 802-11-wireless.powersave 2
				changed "Wi-Fi '$name': power saving off (applies on its next connect)"
			fi
			;;
		esac
		method="$(nmcli -g ipv4.method con show "$uuid")"
		if [ "$method" = auto ]; then
			warn "Wi-Fi '$name' takes its address from DHCP: reserve this stick's address on your"
			warn "  router. The inverter's gateway points at it; if it changes, the inverter goes offline."
		fi
	done < <(nmcli -t -f UUID,NAME,TYPE con show | awk -F: '$3=="802-11-wireless"{print $1":"$2}')
	[ -n "$any" ] || warn "no Wi-Fi client connection: join your network first (nmtui)"
}

# ---- 6. forwarding ----
step_forwarding() {
	# the installer's own file (installer.sh uninstall --purge removes it)
	put_file /etc/sysctl.d/90-foxess.conf 'net.ipv4.ip_forward=1' "IPv4 forwarding persisted" || true
	if [ "$(cat /proc/sys/net/ipv4/ip_forward)" = 1 ]; then
		ok "IPv4 forwarding on"
	elif [ -n "$CHECK" ]; then
		would "turn IPv4 forwarding on"
	else
		sysctl -q -w net.ipv4.ip_forward=1
		changed "IPv4 forwarding on"
	fi
}

# ---- 7. TPROXY modules ----
# kernel.org source tarballs by kernel version (the checksums are from
# kernel.org's signed sha256sums.asc). Another version: add a line, or
# set KERNEL_SRC_SHA256 to the tarball's checksum.
src_sha256() {
	case "$1" in
	linux-6.6.tar.xz) echo d926a06c63dd8ac7df3f86ee1ffc2ce2a3b81a2d168484e76b5b389aba8e56d0 ;;
	*) echo "${KERNEL_SRC_SHA256:-}" ;;
	esac
}
build_info() { printf 'kernel=%s\nbuild=%s\nrev=%s\n' "$KVER" "$(uname -v)" "$MODSET_REV"; }

tproxy_state() { # prints: kernel | ours | stale | none
	local f
	f="$(modinfo -n nft_tproxy 2>/dev/null || true)"
	case "$f" in
	"$MODDIR"/*)
		if [ "$(cat "$MODDIR/BUILD-INFO" 2>/dev/null)" = "$(build_info)" ]; then echo ours; else echo stale; fi
		;;
	"") if grep -qw nft_tproxy /proc/modules 2>/dev/null; then echo kernel; else echo none; fi ;;
	*) echo kernel ;;
	esac
}

build_tproxy() {
	local base tarball sha work src mod n f sym jobs
	base="${KVER%%-*}"                    # 6.6.0
	case "$base" in
	*.*.0) tarball="linux-${base%.0}.tar.xz" ;;
	*.*.*) tarball="linux-$base.tar.xz" ;;
	*) die "cannot read the kernel version from $KVER" ;;
	esac
	sha="$(src_sha256 "$tarball")"
	[ -n "$sha" ] || die "no pinned checksum for $tarball (kernel $KVER): set KERNEL_SRC_SHA256 from kernel.org's sha256sums.asc"
	[ -r /proc/config.gz ] || die "the running kernel's configuration (/proc/config.gz) is not available"

	need_pkgs gcc make bc flex bison libc6-dev libelf-dev xz-utils kmod
	work="$(mktemp -d /var/tmp/foxess-kbuild.XXXXXX)"
	# shellcheck disable=SC2064 # expand now: $work is local
	trap "rm -rf '$work'" EXIT
	say "downloading $tarball (kernel.org, ~140 MB)"
	curl -fsSL --retry 3 -o "$work/$tarball" "https://cdn.kernel.org/pub/linux/kernel/v${base%%.*}.x/$tarball"
	echo "$sha  $work/$tarball" | sha256sum -c --quiet - || die "$tarball: checksum mismatch"
	say "extracting the parts the build needs"
	src="$work/${tarball%.tar.xz}"
	tar -xJf "$work/$tarball" -C "$work" --wildcards \
		'*Kconfig*' "${src##*/}/Makefile" "${src##*/}/Kbuild" "${src##*/}/scripts" "${src##*/}/include" \
		"${src##*/}/arch/arm64" "${src##*/}/arch/arm/vdso" "${src##*/}/arch/arm/tools" \
		"${src##*/}/arch/x86/entry/syscalls" "${src##*/}/lib/vdso" \
		"${src##*/}/kernel/bounds.c" "${src##*/}/kernel/time/timeconst.bc" \
		"${src##*/}/net/netfilter" "${src##*/}/net/ipv4/netfilter" "${src##*/}/net/ipv6/netfilter" \
		"${src##*/}/tools/include" "${src##*/}/tools/scripts"
	rm -f "$work/$tarball"

	say "configuring with the running kernel's configuration"
	zcat /proc/config.gz >"$src/.config"
	"$src/scripts/config" --file "$src/.config" --module NF_TPROXY_IPV4 --module NF_TPROXY_IPV6 --module NFT_TPROXY
	make -s -C "$src" olddefconfig
	[ "$(make -s -C "$src" kernelrelease)" = "$KVER" ] || die "the source builds $(make -s -C "$src" kernelrelease), not $KVER"
	jobs="$(nproc)"
	say "preparing the build tree (a few minutes)"
	make -s -C "$src" -j"$jobs" modules_prepare

	mod="$work/mod"
	mkdir "$mod"
	cp "$src/net/ipv4/netfilter/nf_tproxy_ipv4.c" "$src/net/ipv6/netfilter/nf_tproxy_ipv6.c" \
		"$src/net/netfilter/nft_tproxy.c" "$mod/"
	# the UDP socket lookups are exported only by kernels built with
	# TPROXY; without them the module cannot load. The gate proxies TCP
	# only: stub them (a UDP lookup finds no socket).
	for f in nf_tproxy_ipv4:udp4_lib_lookup nf_tproxy_ipv6:udp6_lib_lookup; do
		sym="${f#*:}"
		f="$mod/${f%%:*}.c"
		n="$(grep -n '^#include' "$f" | tail -1 | cut -d: -f1)"
		sed -i "${n}a #define $sym(...) ((struct sock *)NULL) /* provision-openstick.sh: not exported; UDP TPROXY unsupported */" "$f"
	done
	echo 'obj-m := nf_tproxy_ipv4.o nf_tproxy_ipv6.o nft_tproxy.o' >"$mod/Kbuild"
	say "building nf_tproxy_ipv4, nf_tproxy_ipv6, nft_tproxy"
	# no Module.symvers for a distributed kernel: modpost cannot see the
	# kernel's exports, so unresolved-symbol reports are warnings; the
	# load test below is the real check
	make -s -C "$src" M="$mod" KBUILD_MODPOST_WARN=1 modules 2>&1 | grep -v 'undefined!$\|Module.symvers\|Modules may not\|unresolved symbol\|KBUILD_MODPOST_WARN\|at your own risk' || true
	for f in nf_tproxy_ipv4 nf_tproxy_ipv6 nft_tproxy; do
		[ -f "$mod/$f.ko" ] || die "$f.ko was not built"
		[ "$(modinfo -F vermagic "$mod/$f.ko" | cut -d' ' -f1)" = "$KVER" ] || die "$f.ko does not match kernel $KVER"
	done

	rm -rf "$MODDIR"
	install -d "$MODDIR"
	install -m 0644 "$mod"/*.ko "$MODDIR/"
	build_info >"$MODDIR/BUILD-INFO"
	depmod -a "$KVER"
	rm -rf "$work"
	trap - EXIT
}

# tproxy_test: load the module and have nftables accept a TPROXY rule
# (in a throwaway table) — what the gate's rules need.
tproxy_test() {
	modprobe nft_tproxy || return 1
	nft -f - <<'EOF' || return 1
table ip foxess_provision_test {
	chain p {
		type filter hook prerouting priority mangle
		ip saddr 192.0.2.1 tcp dport 14431 tproxy ip to 127.0.0.1:14431 meta mark set 0x1
	}
}
EOF
	nft delete table ip foxess_provision_test
}

step_tproxy() {
	local state
	state="$(tproxy_state)"
	case "$state" in
	kernel) ok "TPROXY: provided by the kernel" ;;
	ours) ok "TPROXY: modules built for this kernel ($MODDIR)" ;;
	stale | none)
		if [ -n "$CHECK" ]; then
			would "build the TPROXY modules for kernel $KVER (about 5 minutes)"
			return
		fi
		[ "$state" = none ] || say "the TPROXY modules here were built for another kernel build: rebuilding"
		build_tproxy
		changed "TPROXY modules built and installed ($MODDIR)"
		;;
	esac
	[ "$state" = kernel ] || put_file /etc/modules-load.d/foxess-tproxy.conf 'nft_tproxy' "nft_tproxy loaded at boot" || true
	[ -n "$CHECK" ] && return
	if tproxy_test; then
		ok "TPROXY: an nftables tproxy rule loads"
	else
		[ "$state" = kernel ] || { rm -rf "$MODDIR"; depmod -a "$KVER"; }
		dmesg | tail -5 >&2
		die "TPROXY does not work on this kernel (see the kernel messages above); the gate cannot run here"
	fi
}

# ---- 8. accounts and SSH ----
# well-known passwords of OpenStick images and their guides
DEFAULT_PASSWORDS="1 user root openstick 123456 12345678 password"
step_accounts() {
	local u
	command -v perl >/dev/null || return 0 # perl-base: always there on Debian
	for u in $(default_password_accounts); do
		if [ -n "$APPLIANCE" ]; then
			warn "the account '$u' still has a well-known default password: --seal replaces it"
		else
			warn "the account '$u' still has a well-known default password: change it (passwd $u)"
		fi
	done
}

step_ssh() {
	local user home keys conf=/etc/ssh/sshd_config.d/10-foxess-keys-only.conf
	if [ -z "$KEYS_ONLY" ]; then
		if [ -f "$conf" ]; then ok "SSH: password logins off"; else
			[ "$(sshd -T 2>/dev/null | awk '$1=="passwordauthentication"{print $2}')" != yes ] ||
				warn "SSH accepts passwords: --ssh-keys-only turns that off once you log in with a key"
		fi
		return
	fi
	user="${SUDO_USER:-root}"
	home="$(getent passwd "$user" | cut -d: -f6)"
	keys="$home/.ssh/authorized_keys"
	if ! grep -Eq '^(ssh-|ecdsa-|sk-)' "$keys" 2>/dev/null; then
		die "--ssh-keys-only: $user has no authorized key ($keys); add one first, or you would be locked out"
	fi
	if put_file "$conf" '# provision-openstick.sh --ssh-keys-only
PasswordAuthentication no
KbdInteractiveAuthentication no' "SSH: password logins off"; then
		sshd -t || { rm -f "$conf"; die "sshd rejected the setting; left unchanged"; }
		systemctl reload ssh
	fi
}

# ---- 9. hostname ----
step_hostname() {
	[ -n "$NEW_HOSTNAME" ] || return 0
	if [ "$(hostname)" = "$NEW_HOSTNAME" ]; then
		ok "host name $NEW_HOSTNAME"
	elif [ -n "$CHECK" ]; then
		would "set the host name to $NEW_HOSTNAME"
	else
		local old
		old="$(hostname)"
		hostnamectl set-hostname "$NEW_HOSTNAME"
		if grep -q "^127\.0\.1\.1[[:space:]]" /etc/hosts; then
			sed -i "s/^127\.0\.1\.1[[:space:]].*/127.0.1.1\t$NEW_HOSTNAME/" /etc/hosts
		else
			printf '127.0.1.1\t%s\n' "$NEW_HOSTNAME" >>/etc/hosts
		fi
		changed "host name $old -> $NEW_HOSTNAME"
	fi
}

# ---- 10. release files ----
# fetch_release FILE...: the release's FILEs into $REL (a temporary
# directory), verified: SHA256SUMS signed with the release key, and each
# FILE listed there and matching. --from DIR takes them from DIR;
# --allow-unsigned accepts a development build without a signature.
REL=""
fetch_release() {
	local base f
	REL="$(mktemp -d /var/tmp/foxess-release.XXXXXX)"
	# shellcheck disable=SC2064 # expand now
	trap "rm -rf '$REL'" EXIT
	if [ -n "$FROM" ]; then
		for f in SHA256SUMS SHA256SUMS.minisig; do [ ! -f "$FROM/$f" ] || cp "$FROM/$f" "$REL/"; done
		for f in "$@"; do cp "$FROM/$f" "$REL/" || die "--from: $FROM has no $f"; done
	else
		if [ -n "$VERSION" ]; then base="https://github.com/$REPO/releases/download/$VERSION"; else
			base="https://github.com/$REPO/releases/latest/download"
		fi
		say "downloading ${VERSION:-the latest release}:$(printf ' %s' "$@")"
		for f in SHA256SUMS SHA256SUMS.minisig "$@"; do
			curl -fsSL --retry 3 -o "$REL/$f" "$base/$f" || die "download failed: $base/$f"
		done
	fi
	if [ -f "$REL/SHA256SUMS.minisig" ]; then
		minisign -Vqm "$REL/SHA256SUMS" -P "$RELEASE_KEY" || die "SHA256SUMS: bad signature — do not install; report it"
	elif [ -n "$ALLOW_UNSIGNED" ]; then
		warn "no signature: unverified development files (--allow-unsigned); never for a real stick"
	else
		die "--from: $FROM has no SHA256SUMS.minisig (a development build needs --allow-unsigned)"
	fi
	if [ -f "$REL/SHA256SUMS" ]; then
		for f in "$@"; do
			grep -q "  $f\$" "$REL/SHA256SUMS" || die "SHA256SUMS does not list $f"
		done
		(cd "$REL" && sha256sum -c --quiet --ignore-missing SHA256SUMS) || die "a file does not match SHA256SUMS"
	else
		[ -n "$ALLOW_UNSIGNED" ] || die "no SHA256SUMS"
		warn "no SHA256SUMS: the files are not checked"
	fi
	for f in "$@"; do case "$f" in *_linux_*) chmod +x "$REL/$f" ;; esac; done
}
drop_release() {
	rm -rf "$REL"
	trap - EXIT
}

# ---- 11. foxess-local ----
step_install() {
	[ -n "$INVERTER_IP" ] || return 0
	local bin=/var/lib/foxess-local/bin/foxess-local
	if [ -x "$bin" ]; then
		ok "foxess-local installed ($("$bin" version 2>/dev/null || echo "version unknown")); it updates itself"
		return
	fi
	if [ -n "$CHECK" ]; then
		would "install foxess-local ${VERSION:-(latest release)} for inverter $INVERTER_IP"
		return
	fi
	fetch_release installer.sh foxess-local_linux_arm64
	changed "verified $("$REL/foxess-local_linux_arm64" version) (key $RELEASE_KEY)"
	local args=(install --inverter-ip "$INVERTER_IP" --fix)
	[ -z "$FORWARD_MODE" ] || args+=(--forward-mode "$FORWARD_MODE")
	bash "$REL/installer.sh" "${args[@]}"
	drop_release
}

# ---- 12. the appliance (D-031) ----
WIZARD_DIR=/usr/local/lib/foxess-wizard
WIZARD="$WIZARD_DIR/foxess-wizard"
WIZARD_ID=/etc/foxess-wizard/hotspot.env
WIZARD_UNIT='# provision-openstick.sh --appliance: the OpenStick setup wizard (D-031)
[Unit]
Description=foxess OpenStick setup wizard
Wants=NetworkManager.service
After=NetworkManager.service

[Service]
ExecStart=/usr/local/lib/foxess-wizard/foxess-wizard run
Restart=always
RestartSec=5
RuntimeDirectory=foxess-wizard
RuntimeDirectoryMode=0700
StateDirectory=foxess-wizard
StateDirectoryMode=0700
NoNewPrivileges=yes
PrivateTmp=yes
ProtectHome=yes

[Install]
WantedBy=multi-user.target'

# label_dir: where the label goes — the home of the account that ran sudo
label_dir() {
	local user="${SUDO_USER:-root}"
	echo "$(getent passwd "$user" | cut -d: -f6)/foxess-label"
}

# step_modemmanager: ModemManager hands the IMEI to any local user over
# D-Bus, and the stick never uses the modem: masked.
step_modemmanager() {
	if ! systemctl cat ModemManager.service >/dev/null 2>&1; then
		ok "ModemManager: not installed"
	elif [ "$(systemctl is-enabled ModemManager.service 2>/dev/null)" = masked ]; then
		ok "ModemManager: masked"
	elif [ -n "$CHECK" ]; then
		would "stop and mask ModemManager (it shares the IMEI with every local user)"
	else
		systemctl disable --now ModemManager.service >/dev/null 2>&1 || true
		systemctl mask ModemManager.service >/dev/null
		changed "ModemManager stopped and masked"
	fi
}

step_appliance() {
	local need="" lbl
	[ -x /var/lib/foxess-local/bin/foxess-local ] || need="installer.sh foxess-local_linux_arm64"
	[ -x "$WIZARD" ] || need="$need foxess-wizard_linux_arm64"
	if [ -n "$CHECK" ]; then
		[ -z "$need" ] || would "install from ${FROM:-${VERSION:-the latest release}}:$need"
	elif [ -n "$need" ]; then
		# shellcheck disable=SC2086 # a word list
		fetch_release $need
	fi
	# foxess-local, waiting for an inverter address (the wizard sets it)
	if [ -x /var/lib/foxess-local/bin/foxess-local ]; then
		ok "foxess-local installed ($(/var/lib/foxess-local/bin/foxess-local version 2>/dev/null || echo "version unknown"))"
	elif [ -z "$CHECK" ]; then
		local args=(install --no-inverter --fix)
		[ -z "$FORWARD_MODE" ] || args+=(--forward-mode "$FORWARD_MODE")
		bash "$REL/installer.sh" "${args[@]}"
		changed "foxess-local installed, without an inverter"
	fi
	# the wizard: root-owned, it replaces itself when it updates
	if [ -x "$WIZARD" ]; then
		ok "wizard installed ($("$WIZARD" version 2>/dev/null || echo "version unknown"))"
	elif [ -z "$CHECK" ]; then
		install -d -m 0755 -o root -g root "$WIZARD_DIR"
		install -m 0755 -o root -g root "$REL/foxess-wizard_linux_arm64" "$WIZARD"
		changed "wizard $("$WIZARD" version) installed ($WIZARD)"
	fi
	[ -z "$REL" ] || drop_release
	if put_file /etc/systemd/system/foxess-wizard.service "$WIZARD_UNIT" "wizard service unit"; then
		systemctl daemon-reload
	fi
	# the stick's hotspot name and password: made once, kept (the label names them)
	if [ -f "$WIZARD_ID" ]; then
		ok "hotspot name and password ($WIZARD_ID)"
	elif [ -n "$CHECK" ]; then
		would "make the stick's hotspot name and password ($WIZARD_ID)"
	else
		"$WIZARD" init -config "$WIZARD_ID" >/dev/null
		changed "hotspot name and password made ($WIZARD_ID)"
	fi
	step_modemmanager
	[ -z "$CHECK" ] || return 0
	if "$WIZARD" imei-check; then
		ok "the wizard reads the IMEI: compare its last four digits with the one under the cover"
	else
		warn "the wizard cannot read a valid IMEI: the operations that ask for it fall back to the"
		warn "  stick's password alone. Fine for a test stick; check the modem before giving it away."
	fi
	lbl="$(label_dir)"
	"$WIZARD" label -config "$WIZARD_ID" -out "$lbl" | sed 's/^/    /'
	[ -z "${SUDO_USER:-}" ] || chown -R "$SUDO_USER:" "$lbl"
	if systemctl is-active -q foxess-wizard.service; then
		systemctl restart foxess-wizard.service
		ok "wizard running (restarted)"
	else
		systemctl enable --now foxess-wizard.service
		changed "wizard enabled and started"
	fi
}

# ---- --seal ----
# client_wifi: the UUIDs of the Wi-Fi client connections (the
# preparer's network), not the hotspots.
client_wifi() {
	local uuid type
	while IFS=: read -r uuid type; do
		[ "$type" = 802-11-wireless ] || continue
		[ "$(nmcli -g 802-11-wireless.mode con show "$uuid" 2>/dev/null)" = infrastructure ] || continue
		echo "$uuid"
	done < <(nmcli -t -f UUID,TYPE con show)
}

# random_password: 20 characters of base32 (96 bits).
random_password() {
	local p
	p="$(head -c 12 /dev/urandom | base32)"
	echo "${p//=/}"
}

# default_password_accounts: the accounts whose password is a
# well-known one.
default_password_accounts() {
	local u hash p
	while IFS=: read -r u hash _; do
		case "$hash" in \$*) ;; *) continue ;; esac
		for p in $DEFAULT_PASSWORDS; do
			# shellcheck disable=SC2016 # perl's own variables
			if [ "$(perl -e 'print crypt($ARGV[0], $ARGV[1]) eq $ARGV[1] ? 1 : 0' "$p" "$hash")" = 1 ]; then
				echo "$u"
				break
			fi
		done
	done </etc/shadow
}

seal() {
	local u newpw="" uuids
	[ -x "$WIZARD" ] && [ -f "$WIZARD_ID" ] || die "--seal: this stick is not an appliance yet (run --appliance first)"
	say "sealing (D-031)"
	KEYS_ONLY=1
	step_ssh
	for u in $(default_password_accounts); do
		if [ -n "$CHECK" ]; then
			would "replace the well-known password of '$u'"
		elif [ "$u" = "${SUDO_USER:-root}" ]; then
			newpw="$(random_password)"
			echo "$u:$newpw" | chpasswd
			changed "the password of '$u' replaced (shown below, once)"
		else
			passwd -q -l "$u"
			changed "'$u' locked (it had a well-known password)"
		fi
	done
	uuids="$(client_wifi | tr '\n' ' ')"
	if [ -n "$CHECK" ]; then
		would "clear the wizard's state, remove the Wi-Fi client connections (${uuids:-none}), start the wizard"
		return
	fi
	systemctl stop foxess-wizard.service
	find /var/lib/foxess-wizard -mindepth 1 -delete 2>/dev/null || true
	changed "the wizard's state cleared: the next start is unconfigured"
	# after this script has finished: over Wi-Fi, the session ends here
	systemd-run --quiet --collect --unit foxess-seal --on-active=5 \
		/bin/sh -c "for u in $uuids; do nmcli con delete \$u; done; systemctl start foxess-wizard.service"
	echo
	say "sealed. In 5 seconds the stick forgets this Wi-Fi network (an SSH session over Wi-Fi ends)"
	say "  and starts its hotspot $(sed -n 's/^SSID=//p' "$WIZARD_ID"). Unplug it whenever you like."
	if [ -n "$newpw" ]; then
		say "  the password of '${SUDO_USER:-root}' (sudo needs it; shown once, keep it):"
		echo "    $newpw"
	fi
}

if [ -n "$SEAL" ]; then
	seal
	exit 0
fi

say "1/9 apt sources"
step_apt_suites
say "2/9 packages"
need_pkgs nftables iproute2 conntrack procps curl ca-certificates minisign systemd-zram-generator
say "3/9 memory"
step_zram
say "4/9 logs"
step_journald
say "5/9 Wi-Fi"
step_wifi
say "6/9 forwarding"
step_forwarding
say "7/9 TPROXY"
step_tproxy
say "8/9 accounts"
step_accounts
step_ssh
step_hostname
if [ -n "$APPLIANCE" ]; then
	say "9/9 appliance"
	step_appliance
else
	say "9/9 foxess-local"
	if [ -n "$INVERTER_IP" ]; then step_install; else ok "not installing (no --inverter-ip)"; fi
fi

addr="$(ip -4 -o route get 1.1.1.1 2>/dev/null | grep -oE 'src [0-9.]+' | cut -d' ' -f2 || true)"
echo
if [ -n "$CHECK" ]; then
	say "check done"
elif [ -n "$APPLIANCE" ]; then
	say "done: the appliance is prepared. The label is in $(label_dir)/."
	say "  Check it on the home network: $(sed -n 's/^HOST=//p' "$WIZARD_ID" 2>/dev/null).local:18081 (the password is on the label)"
	say "  Then seal it, over USB (it removes this Wi-Fi connection): sudo bash $0 --seal"
elif [ -x /var/lib/foxess-local/bin/foxess-local ]; then
	say "done. Web UI: http://${addr:-<this stick>}:18080/"
	say "next: set the inverter's gateway to ${addr:-the address of this stick} (INSTALL.md, step 4)"
else
	say "done: ready for foxess-local. Install it with"
	say "  sudo bash $0 --inverter-ip <inverter address>"
fi
