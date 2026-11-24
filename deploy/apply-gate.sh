#!/usr/bin/env bash
# foxess gate apply — deploy/apply-gate.sh (install:
# /usr/local/lib/foxess-local/apply-gate.sh, root)
#
# Live since stage 1a (2026-11-15); boot-time drop and MAC scoping added
# 2026-11-17 (audit S2, S3). Mechanism: nftables; the TPROXY fwmark
# routing uses iproute2 (mark 1 / table 100).
#
# Usage: apply-gate.sh {early|up|down|show|configured}
#   early loads foxess_gate_boot: a drop of everything the inverter would
#         forward, from its IP (and MAC). Run by foxess-gate-early.service
#         before the network comes up (it needs no route), so the
#         inverter is never forwarded unfiltered while the real gate
#         waits for the network — and stays blocked if `up` fails.
#   up    idempotent: renders gate.nft (the @VARS@; lines naming the MAC
#         are dropped when INVERTER_MAC is unset) and loads it as one
#         atomic nft transaction that also deletes the previous gate and
#         the boot-time drop. Then ensures the fwmark routing: created
#         only if absent, and recorded in a marker file so `down` never
#         removes routing the prox deploy owns.
#   down  removes the gate's tables (and the boot-time drop) and — only
#         if this script created them — the fwmark routing. The
#         one-command rollback.
#   show  dumps the live gate state (rules, counters, fwmark routing).
#   configured  exits 0 when there is an inverter address, 1 (saying so)
#         when there is none yet (installer --no-inverter): the units'
#         ExecCondition, so they stay inactive rather than failed.
#
# Environment overrides: INVERTER_IP (no default: gate.env), INVERTER_MAC
# (optional: gate.env), GATEWAY_IP (default: auto-detected as the source
# we reach the inverter from), TPROXY_PORT (default 14431), GATE_NFT
# (default /etc/foxess-local/gate.nft), GATE_ENV (default
# /etc/foxess-local/gate.env).

set -euo pipefail

# Persistent gate knobs (installer-seeded /etc/foxess-local/gate.env).
# Sourced fill-only: a variable already set in the environment wins
# over the file (TPROXY_PORT=31280 apply-gate.sh up still overrides).
GATE_ENV="${GATE_ENV:-/etc/foxess-local/gate.env}"
if [ -f "$GATE_ENV" ]; then
    while IFS= read -r _line; do
        case "$_line" in '' | \#*) continue ;; esac
        _k="${_line%%=*}"
        _v="${_line#*=}"
        [ -n "${!_k:-}" ] || printf -v "$_k" '%s' "$_v"
    done < "$GATE_ENV"
fi

GATE_NFT="${GATE_NFT:-/etc/foxess-local/gate.nft}"
INVERTER_IP="${INVERTER_IP:-}"
if [ "${1:-}" = configured ]; then
    [ -z "$INVERTER_IP" ] || exit 0
    echo "apply-gate: no inverter address yet (INVERTER_IP is empty in $GATE_ENV): the gate stays off until one is set" >&2
    exit 1
fi
if [ -z "$INVERTER_IP" ]; then
    echo "apply-gate: INVERTER_IP is not set (add it to /etc/foxess-local/gate.env)" >&2
    exit 2
fi
INVERTER_MAC="${INVERTER_MAC:-}"
if [ -n "$INVERTER_MAC" ] && ! [[ "$INVERTER_MAC" =~ ^([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}$ ]]; then
    echo "apply-gate: INVERTER_MAC '$INVERTER_MAC' is not a MAC address" >&2
    exit 2
fi
TPROXY_PORT="${TPROXY_PORT:-14431}"
MARK=1
ROUTE_TABLE=100
RULE_PRIORITY=3000
TABLE="foxess_gate"
TABLE6="foxess_gate6"
BOOT_TABLE="foxess_gate_boot"
STATE_DIR="${STATE_DIR:-/var/lib/foxess-gate}"
ROUTING_MARKER="$STATE_DIR/routing-owned"

log() { logger -t foxess-gate "$*" || true; }

# present FAMILY NAME: the nft table exists.
present() { nft list table "$1 $2" >/dev/null 2>&1; }

# deletions: the delete lines for whichever gate tables exist (an atomic
# file cannot delete a missing table — the whole file would abort).
deletions() {
    present ip "$TABLE" && echo "delete table ip $TABLE"
    present ip6 "$TABLE6" && echo "delete table ip6 $TABLE6"
    present ip "$BOOT_TABLE" && echo "delete table ip $BOOT_TABLE"
    true
}

gateway_ip() {
    if [[ -n "${GATEWAY_IP:-}" ]]; then
        echo "${GATEWAY_IP}"
        return
    fi
    # the source we use to reach the inverter = this box's LAN IP
    ip -4 route get "$INVERTER_IP" 2>/dev/null | grep -oP 'src \K[\d.]+' | tail -1
}

fw_rule_present() {
    # iproute2 renders the fwmark in hex (mark 1 -> "fwmark 0x1")
    ip rule show | grep -q "^${RULE_PRIORITY}:.*from all fwmark 0x${MARK} lookup ${ROUTE_TABLE}"
}

local_route_present() {
    # iproute2 renders 0.0.0.0/0 as "default"
    ip route show table "$ROUTE_TABLE" 2>/dev/null | grep -q '^local default dev lo'
}

# ensure_routing brings the fwmark routing to the "exists" state and
# records ownership (marker) only when this script created it — the
# prox deploy's identical rule (if present) is left alone and unowned.
ensure_routing() {
    local created=0
    if ! fw_rule_present; then
        ip rule add priority "$RULE_PRIORITY" fwmark "$MARK" table "$ROUTE_TABLE"
        created=1
    fi
    if ! local_route_present; then
        ip route replace local 0.0.0.0/0 dev lo table "$ROUTE_TABLE"
        created=1
    fi
    if [[ "$created" -eq 1 ]]; then
        mkdir -p "$STATE_DIR"
        touch "$ROUTING_MARKER"
        log "up: created fwmark routing (rule priority ${RULE_PRIORITY}, table ${ROUTE_TABLE})"
    fi
}

cmd_up() {
    local gw tmp
    gw="$(gateway_ip)"
    if [[ -z "$gw" ]]; then
        log "up: cannot auto-detect GATEWAY_IP; set it explicitly"
        exit 1
    fi
    tmp="$(mktemp)"
    {
        deletions
        if [ -n "$INVERTER_MAC" ]; then
            sed -e "s/@INVERTER_MAC@/${INVERTER_MAC}/g" "$GATE_NFT"
        else
            sed -e "/@INVERTER_MAC@/d" "$GATE_NFT"
        fi | sed -e "s/@INVERTER_IP@/${INVERTER_IP}/g" \
            -e "s/@GATEWAY_IP@/${gw}/g" \
            -e "s/@TPROXY_PORT@/${TPROXY_PORT}/g"
    } > "$tmp"
    nft -f "$tmp"
    rm -f "$tmp"
    ensure_routing
    log "up: gate applied (inverter ${INVERTER_IP}${INVERTER_MAC:+ / $INVERTER_MAC}, tproxy -> ${gw}:${TPROXY_PORT})"
}

# cmd_early loads the boot-time drop (no route needed: it runs before
# the network). An existing gate is left alone — it is stricter.
cmd_early() {
    if present ip "$TABLE"; then
        log "early: the gate is already live; boot-time drop not needed"
        return
    fi
    local tmp
    tmp="$(mktemp)"
    {
        present ip "$BOOT_TABLE" && echo "delete table ip $BOOT_TABLE"
        echo "table ip $BOOT_TABLE {"
        echo "  chain forward_deny {"
        echo "    type filter hook forward priority filter"
        echo "    ip saddr ${INVERTER_IP} counter drop"
        [ -n "$INVERTER_MAC" ] && echo "    ether saddr ${INVERTER_MAC} counter drop"
        echo "  }"
        echo "}"
        if [ -n "$INVERTER_MAC" ]; then
            echo "table ip6 $TABLE6 {"
            echo "  chain forward_deny {"
            echo "    type filter hook forward priority filter"
            echo "    ether saddr ${INVERTER_MAC} counter drop"
            echo "  }"
            echo "}"
        fi
    } > "$tmp"
    if [ -n "$INVERTER_MAC" ] && present ip6 "$TABLE6"; then
        sed -i "1i delete table ip6 $TABLE6" "$tmp"
    fi
    nft -f "$tmp"
    rm -f "$tmp"
    log "early: boot-time drop loaded (inverter ${INVERTER_IP}${INVERTER_MAC:+ / $INVERTER_MAC})"
}

cmd_down() {
    local f t
    for f in "ip $TABLE" "ip6 $TABLE6" "ip $BOOT_TABLE"; do
        if nft list table "$f" >/dev/null 2>&1; then
            nft delete table "$f"
            t="${f#* }"
            log "down: removed table ${f% *} ${t}"
        fi
    done
    if [[ -f "$ROUTING_MARKER" ]]; then
        if fw_rule_present; then
            ip rule del priority "$RULE_PRIORITY"
        fi
        if local_route_present; then
            ip route del local 0.0.0.0/0 table "$ROUTE_TABLE"
        fi
        rm -f "$ROUTING_MARKER"
        log "down: removed the gate-created fwmark routing"
    fi
}

cmd_show() {
    if ! nft list table "ip $TABLE" >/dev/null 2>&1; then
        echo "table ip ${TABLE}: not present"
        exit 1
    fi
    echo "--- fwmark routing"
    ip rule show | grep "fwmark 0x${MARK}" || echo "(no fwmark rule)"
    ip route show table "$ROUTE_TABLE"
}

# one change at a time: the installer starts the gate and the rule-loss
# check together, and the check re-applies a gate it finds missing. Two
# concurrent `up`s both computed "no table yet" (doubling every rule)
# and raced on `ip rule add` (the loser failed: "File exists").
case "${1:-up}" in
    early | up | down)
        mkdir -p "$STATE_DIR"
        exec 9>"$STATE_DIR/lock"
        flock 9
        ;;
esac

case "${1:-up}" in
    early) cmd_early ;;
    up) cmd_up ;;
    down) cmd_down ;;
    show) cmd_show ;;
    *) echo "usage: apply-gate.sh {early|up|down|show|configured}" >&2; exit 2 ;;
esac
