#!/usr/bin/env bash
# foxess gate rule-loss check — deploy/check-gate.sh (install:
# /usr/local/lib/foxess-local/check-gate.sh, run by
# foxess-gate-check.timer every 5 min, root) — DEF-003
#
# Live since stage 1a (2026-11-15; the 1a.0 inventory is done). The IPv6
# table is checked too when gate.env sets INVERTER_MAC (2026-11-17).
#
# The named risk: the gateway host reboots, the rules do not re-apply, the
# inverter's egress silently reverts to default-allow — it reaches the
# real cloud unfiltered with no signal to the local side.
#
# Two checks; a failure logs an ALERT-level
# "foxess-gate: RULE-LOSS <detail>" journal line (the DEF-001 hook) and
# re-applies the gate (apply-gate.sh up):
#   (a) the ruleset is live — the table exists and the fwmark routing
#       (rule + local route) exists;
#   (b) no inverter flow to a disallowed destination — conntrack scan:
#       dport 14431 (tproxied; conntrack shows the original tuple), 123,
#       and 53 -> the local resolver are allowed, anything else is rule
#       loss or a new channel — either way, alert. (New channels that
#       get dropped never reach conntrack; they surface in the drop log,
#       reviewed from the journal.)
#
# Each run leaves its result in $STATUS_DIR/status (default
# /run/foxess-gate/status, world-readable: ts=<unix>, result=ok|rule-loss,
# detail=<the first problem>) — the app reads it to notify the owner and
# for its readiness check (F2).
#
# Requires: nftables. Optional: the conntrack tool (conntrack /
# conntrack-tools package) — without it (b) is skipped and only (a)
# runs; a healthy gate still passes.
#
# (b) parses conntrack's one-line-per-flow rendering — lowercase
# `src=`/`dst=`/`dport=`, the original tuple first, then the reply tuple
# (verified on the gateway host 2026-11-17; TestGateCheckConntrackTuples carries
# the captured line). A different rendering would match zero flows and
# pass silently — re-check it on a new distribution.

set -uo pipefail

# INVERTER_IP: the environment, else /etc/foxess-local/gate.env (the
# installer seeds it from --inverter-ip). There is no default.
gate_env_value() {
    local f="${GATE_ENV:-/etc/foxess-local/gate.env}"
    [ -f "$f" ] && sed -n "s/^$1=//p" "$f" | tail -1
}
INVERTER_IP="${INVERTER_IP:-$(gate_env_value INVERTER_IP)}"
INVERTER_MAC="${INVERTER_MAC:-$(gate_env_value INVERTER_MAC)}"
if [ -z "$INVERTER_IP" ]; then
    echo "$(basename "$0"): INVERTER_IP is not set (add it to /etc/foxess-local/gate.env)" >&2
    exit 2
fi
TPROXY_PORT="${TPROXY_PORT:-14431}"
MARK=1
ROUTE_TABLE=100
RULE_PRIORITY=3000
TABLE="foxess_gate"
APPLY="${APPLY:-/usr/local/lib/foxess-local/apply-gate.sh}"
CONNTRACK="${CONNTRACK:-conntrack}"
GATEWAY_IP="${GATEWAY_IP:-$(ip -4 route get "$INVERTER_IP" 2>/dev/null | grep -oP 'src \K[\d.]+' | tail -1)}"

fail=0
first_problem=""
STATUS_DIR="${STATUS_DIR:-/run/foxess-gate}"

# write_status leaves this run's result for the app (atomically: a
# temporary file renamed over the old one). A failure to write it never
# fails the check itself.
# shellcheck disable=SC2329 # invoked by the EXIT trap
write_status() {
    local result=ok tmp
    [ "$fail" -eq 0 ] || result=rule-loss
    mkdir -p "$STATUS_DIR" 2>/dev/null && chmod 0755 "$STATUS_DIR" 2>/dev/null || return 0
    tmp="$(mktemp "$STATUS_DIR/.status.XXXXXX" 2>/dev/null)" || return 0
    if printf 'ts=%s\nresult=%s\ndetail=%s\n' "$(date +%s)" "$result" "${first_problem//$'\n'/ }" >"$tmp" &&
        chmod 0644 "$tmp" && mv -f "$tmp" "$STATUS_DIR/status"; then
        return 0
    fi
    rm -f "$tmp"
}
trap write_status EXIT

alert() {
    logger -p daemon.alert -t foxess-gate "RULE-LOSS: $*"
    fail=1
    [ -n "$first_problem" ] || first_problem="$*"
    if ! "$APPLY" up; then
        logger -p daemon.err -t foxess-gate "RULE-LOSS: re-apply failed: $*"
    fi
}

# (a) ruleset live
if ! nft list table "ip $TABLE" >/dev/null 2>&1; then
    alert "table ip ${TABLE} missing"
    exit 1 # the rest is uncheckable until the re-apply lands
fi
if [ -n "$INVERTER_MAC" ] && ! nft list table ip6 foxess_gate6 >/dev/null 2>&1; then
	alert "table ip6 foxess_gate6 missing (the IPv6 drop for ${INVERTER_MAC})"
fi
if ! ip rule show | grep -q "^${RULE_PRIORITY}:.*from all fwmark 0x${MARK} lookup ${ROUTE_TABLE}"; then
    alert "fwmark routing rule missing (fwmark ${MARK} table ${ROUTE_TABLE})"
fi
if ! ip route show table "$ROUTE_TABLE" 2>/dev/null | grep -q '^local default dev lo'; then
    alert "local route missing in table ${ROUTE_TABLE}"
fi

# (b) no off-allowlist inverter flow
if ! command -v "$CONNTRACK" >/dev/null 2>&1; then
    # optional: the rules were checked above; only the flow scan needs it
    exit "$fail"
fi
# conntrack -L lines: "tcp 6 111 ESTABLISHED src=.. dst=.. sport=.. dport=..
# src=.. dst=.. sport=.. dport=.. [ASSURED] ..." — the original tuple,
# then the reply tuple; only the first names the inverter's destination
while IFS= read -r line; do
    dport="$(grep -oP 'dport=\K[0-9]+' <<<"$line" | head -n 1)"
    dst="$(grep -oP 'dst=\K[\d.]+' <<<"$line" | head -n 1)"
    case "$dport" in
    "$TPROXY_PORT" | 123)
        continue
        ;;
    53)
        if [[ "$dst" == "$GATEWAY_IP" ]]; then
            continue
        fi
        alert "off-allowlist inverter flow: $line"
        ;;
    *)
        alert "off-allowlist inverter flow: $line"
        ;;
    esac
done < <("$CONNTRACK" -L 2>/dev/null | grep "src=${INVERTER_IP}" || true)

exit "$fail"
