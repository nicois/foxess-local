#!/usr/bin/env bash
# foxess cutover rollback — deploy/rollback.sh (install:
# /usr/local/lib/foxess-local/rollback.sh, root)
#
# One-command abort of the cutover: the gate is gone and no
# interception of the inverter's cloud channel remains. The gate's
# tproxy rule was the only 14431 rule in the ruleset, and the pre-1a
# redirect was already removed at 1a (verified 2026-11-15:
# `iptables-save | grep 14431` empty; `nft list ruleset | grep 14431`
# shows only the gate's tproxy rule). The inverter's next re-dial
# lands on the real cloud. prox.service is left up (old listener on
# :31280) but no rule delivers inverter traffic to it.
#
# Usage: rollback.sh
#   1. stop foxess-gate-check.timer  — the detector RE-APPLIES the gate
#      on rule loss; it must not fight the rollback within 5 minutes
#   2. start prox.service            — the old service stays up (old
#      listener on :31280) but idle: with the pre-1a redirect gone
#      (verified 2026-11-15), no rule delivers inverter traffic to it.
#      Only retargeting the tproxy to :31280 (keeping the gate)
#      re-intercepts.
#   3. stop foxess-local.service     — the app's 14431 listener goes
#   4. apply-gate.sh down            — gate table + gate-owned routing
#   5. verify: gate table gone, prox active + listening, and dump the
#      surviving ruleset/iptables state to the journal (expect NO
#      inverter-scoped 14431 rule — that is the post-abort status quo)
#
# After the script: the inverter's next re-dial (<= ~2 h cloud
# read-timeout, or force with a power cycle) lands on the real cloud
# directly; HA (cloud-fed foxess-control) picks the values back up on
# its own.
#
# NOTE (reboots): the gate's boot-time re-application unit (1a.2) runs
# on every boot. If you reboot while the app is stopped AND expect
# status quo, run this script once after boot (the gate will be live,
# the app dead). With foxess-local.service enabled, a reboot instead
# lands back in the deployed state — that is why the app unit ships
# enabled.
#
# Environment overrides: APPLY (default /usr/local/lib/foxess-local/
# apply-gate.sh), PROX_PORT (default 31280 — the current prox listener
# port).

set -euo pipefail

# INVERTER_IP: the environment, else /etc/foxess-local/gate.env (the
# installer seeds it from --inverter-ip). There is no default.
gate_env_value() {
    local f="${GATE_ENV:-/etc/foxess-local/gate.env}"
    [ -f "$f" ] && sed -n "s/^$1=//p" "$f" | tail -1
}
INVERTER_IP="${INVERTER_IP:-$(gate_env_value INVERTER_IP)}"
if [ -z "$INVERTER_IP" ]; then
    echo "$(basename "$0"): INVERTER_IP is not set (add it to /etc/foxess-local/gate.env)" >&2
    exit 2
fi

APPLY="${APPLY:-/usr/local/lib/foxess-local/apply-gate.sh}"
PROX_PORT="${PROX_PORT:-31280}"
GATE_TIMER="foxess-gate-check.timer"
GATE_TABLE="foxess_gate"

log() { logger -t foxess-rollback "$*"; echo "rollback: $*"; }

log "stopping $GATE_TIMER (it re-applies the gate on rule loss)"
systemctl stop "$GATE_TIMER" 2>/dev/null \
    || log "note: $GATE_TIMER not installed/running; continuing"

# prox is the recon proxy some installations ran before foxess-local
HAVE_PROX=""
if systemctl cat prox.service >/dev/null 2>&1; then
    HAVE_PROX=1
    log "starting prox.service (old listener back on :$PROX_PORT)"
    systemctl start prox.service
fi

log "stopping foxess-local.service"
systemctl stop foxess-local.service

log "removing the gate (stop the unit so its state tracks the rules)"
# A bare 'down' leaves the RemainAfterExit oneshot 'active (exited)';
# the re-attempt's 'enable --now' would then be a no-op start and the
# gate would come back only via the 5-min check timer.
if systemctl is-active foxess-gate.service >/dev/null 2>&1; then
    systemctl stop foxess-gate.service
else
    "$APPLY" down
fi

# --- verify -----------------------------------------------------------------
if nft list table "ip $GATE_TABLE" >/dev/null 2>&1; then
    log "ERROR: table ip $GATE_TABLE still present after down"
    exit 1
fi
if [ -n "$HAVE_PROX" ] && [[ "$(systemctl is-active prox.service 2>/dev/null)" != "active" ]]; then
    log "ERROR: prox.service is not active"
    exit 1
fi
if [ -n "$HAVE_PROX" ] && ! ss -ltn 2>/dev/null | grep -q "[:.]${PROX_PORT}\b"; then
    log "ERROR: nothing listening on :$PROX_PORT"
    exit 1
fi

log "surviving ruleset state (expect no inverter-scoped 14431 rule):"
nft list ruleset 2>/dev/null | logger -t foxess-rollback --
iptables-save 2>/dev/null | logger -t foxess-rollback -- || true
ip rule show | logger -t foxess-rollback --
ip route show table 100 2>/dev/null | logger -t foxess-rollback -- || true

ruleset="$(nft list ruleset 2>/dev/null || true)"
inv_rules="$(grep -F "$INVERTER_IP" <<<"$ruleset" || true)"
if [ -n "$inv_rules" ] && grep -q '14431' <<<"$inv_rules"; then
    log "WARN: an inverter-scoped 14431 rule still exists (journal dump above) — the inverter may not reach the real cloud"
else
    log "no inverter-scoped 14431 rule remains — the inverter reaches the real cloud directly"
fi
log "cutover aborted: gate gone, inverter re-dial (<= ~2 h, or power-cycle) lands on the real cloud"
