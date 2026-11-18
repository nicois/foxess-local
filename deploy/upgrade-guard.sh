#!/bin/sh
# foxess-local upgrade guard — ExecStartPre of foxess-local.service, run
# as the foxess user before every start (design D-023).
#
# After a self-upgrade the app leaves bin/upgrade-pending (from=, to=,
# attempts=, and online=1 when the inverter was online at the upgrade).
# Each start is counted here; the app deletes the marker
# once the new binary has run healthily for a while. On the fourth
# counted start the new binary is presumed broken: the previous binary
# (bin/foxess-local.prev) is restored and the marker becomes
# bin/upgrade-failed, which the app shows to the owner.
#
# Root-owned and read-only to the app: a compromised binary cannot edit
# the rollback policy. Never fails the start (exit 0 always).
#
# A start the app ended for an environmental reason (exit=port_in_use:
# the inverter port still held by a previous session) is not counted
# (install review G4) — at most MAX_UNCOUNTED times, so a binary that
# always claims it cannot dodge the rollback.
set -u
BIN_DIR="${BIN_DIR:-/var/lib/foxess-local/bin}"
P="$BIN_DIR/upgrade-pending"
MAX_ATTEMPTS=3
MAX_UNCOUNTED=3

[ -f "$P" ] || exit 0

from=$(sed -n 's/^from=//p' "$P")
to=$(sed -n 's/^to=//p' "$P")
attempts=$(sed -n 's/^attempts=//p' "$P")
online=$(sed -n 's/^online=//p' "$P")
exitcause=$(sed -n 's/^exit=//p' "$P" | tail -n 1)
uncounted=$(sed -n 's/^uncounted=//p' "$P")
case "$attempts" in ''|*[!0-9]*) attempts=0 ;; esac
case "$uncounted" in ''|*[!0-9]*) uncounted=0 ;; esac
if [ "$exitcause" = port_in_use ] && [ "$uncounted" -lt "$MAX_UNCOUNTED" ]; then
	uncounted=$((uncounted + 1))
	echo "upgrade-guard: the last start of $to ended with the inverter port still in use — not counted ($uncounted of $MAX_UNCOUNTED)"
else
	attempts=$((attempts + 1))
fi

if [ "$attempts" -gt "$MAX_ATTEMPTS" ]; then
	if [ -f "$BIN_DIR/foxess-local.prev" ]; then
		mv -f "$BIN_DIR/foxess-local.prev" "$BIN_DIR/foxess-local"
	fi
	mv -f "$P" "$BIN_DIR/upgrade-failed"
	echo "upgrade-guard: $to failed to start $MAX_ATTEMPTS times; restored $from"
	exit 0
fi

{
	printf 'from=%s\nto=%s\nattempts=%d\nuncounted=%d\n' "$from" "$to" "$attempts" "$uncounted"
	# the app's flag: the inverter was online at the upgrade (keep it)
	[ "$online" != 1 ] || printf 'online=1\n'
} > "$P.tmp" && mv -f "$P.tmp" "$P" ||
	echo "upgrade-guard: ERROR cannot record the start in $P: a failing $to will NOT be rolled back (permissions or SELinux labels of $BIN_DIR?)"
echo "upgrade-guard: starting $to (attempt $attempts of $MAX_ATTEMPTS)"
exit 0
